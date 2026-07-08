package chatgptsub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/render"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

var (
	_             adaptor.Adaptor = (*Adaptor)(nil)
	stickyManager                 = DefaultStickyManager
	statsManager                  = NewStatsManager()
	channelProber                 = NewChannelProber()
)

type Adaptor struct {
	OpenAiImpl adaptor.Adaptor
	meta       *meta.Meta
}

// extractSessionHash 从请求头中提取会话标识，用于粘性绑定。
// 优先使用 conversation_id，其次 session_id。
func extractSessionHash(c *gin.Context) string {
	if v := c.GetHeader("conversation_id"); v != "" {
		return v
	}
	if v := c.GetHeader("session_id"); v != "" {
		return v
	}
	return ""
}

func (a *Adaptor) Init(meta *meta.Meta) {
	a.meta = meta
}

func (a *Adaptor) GetRequestURL(meta *meta.Meta) (string, error) {
	baseURL := strings.TrimSuffix(meta.BaseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	switch meta.Mode {
	case relaymode.ChatCompletions, relaymode.Responses:
		return baseURL + "/backend-api/codex/responses", nil
	default:
		return baseURL + meta.RequestURLPath, nil
	}
}

// allowedHeaders 是 ChatGPT Web 请求中允许透传到上游的头白名单
var allowedHeaders = map[string]bool{
	"accept-language":       true,
	"content-type":          true,
	"conversation_id":       true,
	"user-agent":            true,
	"originator":            true,
	"session_id":            true,
	"x-codex-turn-state":    true,
	"x-codex-turn-metadata": true,
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) error {
	// 粘性会话检查：如果这个会话有绑定的 channel，且绑定的不是当前 channel，尝试切换
	sessionHash := extractSessionHash(c)
	if sessionHash != "" {
		if boundID, ok := stickyManager.Get(meta.Group, sessionHash); ok && boundID != meta.ChannelId {
			if boundChannel, err := dbmodel.GetChannelById(boundID, true); err == nil && boundChannel != nil && boundChannel.Status == dbmodel.ChannelStatusEnabled {
				meta.ChannelId = boundChannel.Id
				meta.APIKey = boundChannel.Key
				meta.BaseURL = boundChannel.GetBaseURL()
				// 同步更新请求头
				req.Header.Set("Authorization", "Bearer "+meta.APIKey)
			}
		}
	}

	adaptor.SetupCommonRequestHeader(c, req, meta)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if meta.IsStream {
		req.Header.Set("Accept", "text/event-stream")
	}
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)

	// 头白名单过滤：只保留白名单内的头 + 固定头
	for key := range req.Header {
		lowerKey := strings.ToLower(key)
		// 永远保留这些头
		if lowerKey == "authorization" || lowerKey == "content-type" || lowerKey == "accept" {
			continue
		}
		if !allowedHeaders[lowerKey] {
			req.Header.Del(key)
		}
	}
	return nil
}

func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	switch relayMode {
	case relaymode.ChatCompletions:
		return convertChatToResponsesRequest(request), nil
	case relaymode.Responses:
		// 透传原始 Responses 请求体
		rawBody, err := common.GetRequestBody(c)
		if err != nil {
			return nil, err
		}
		var bodyMap map[string]interface{}
		if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
			return nil, err
		}
		return bodyMap, nil
	default:
		return request, nil
	}
}

func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	return request, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error) {
	return adaptor.DoRequestHelper(a, c, meta, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (usage *model.Usage, respErr *model.ErrorWithStatusCode) {
	// 使用 defer 在返回前统一上报健康统计 + 粘性绑定
	defer func() {
		success := respErr == nil && resp != nil && resp.StatusCode < 400
		statsManager.ReportResult(meta.ChannelId, success, nil)

		if success {
			sessionHash := extractSessionHash(c)
			if sessionHash != "" {
				stickyManager.Set(meta.Group, sessionHash, meta.ChannelId)
			}
		}
	}()

	if meta.IsStream {
		switch meta.Mode {
		case relaymode.ChatCompletions:
			// Responses SSE 流 → Chat Completions SSE 流
			return streamChatFromResponses(c, resp, meta)
		case relaymode.Responses:
			// Responses SSE 流直接透传
			err, _, usage := codex.StreamResponsesHandler(c, resp)
			return usage, err
		default:
			return a.OpenAiImpl.DoResponse(c, resp, meta)
		}
	}

	switch meta.Mode {
	case relaymode.ChatCompletions:
		return handleChatCompletionsResponse(c, resp, meta)
	case relaymode.Responses:
		return codex.DoResponsesResponse(c, resp, meta)
	default:
		return a.OpenAiImpl.DoResponse(c, resp, meta)
	}
}

func (a *Adaptor) GetModelList() []string {
	return []string{
		"gpt-5.5",
		"gpt-5.4-mini",
		"gpt-5.4",
	}
}

func (a *Adaptor) GetChannelName() string {
	return "chatgpt-sub"
}

// ==================== Chat Completions Streaming ====================

// toolCallState 记录流式 tool call 的累积状态
type toolCallState struct {
	index      int
	id         string
	name       string
	argsBuffer strings.Builder
}

// streamChatFromResponses 读取上游 Responses SSE 流，转换为 Chat Completions SSE 格式写回客户端。
func streamChatFromResponses(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	reader := bufio.NewReaderSize(resp.Body, constant.ScannerBufferInitial)

	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	var contentBuffer strings.Builder
	var reasoningBuffer strings.Builder
	responseID := ""
	sawFirstToken := false
	sawCompleted := false
	sawFailed := false
	sawDone := false
	var streamErr error
	var outUsage *model.Usage
	modelName := meta.ActualModelName

	// Tool call 跟踪：item_id → tool call info
	toolCallStates := make(map[string]*toolCallState)
	var completedToolCallIDs []string
	var toolCallIdxSeq int

	eventCount := 0
loop:
	for {
		event, err := codex.ReadSSEEvent(reader, constant.ScannerBufferMax)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, context.Canceled) {
				break
			}
			streamErr = err
			break
		}
		eventCount++

		if event.Done {
			// [DONE] 事件 - 写 Chat 的 [DONE]
			render.Done(c)
			sawDone = true
			break
		}

		payload := event.Data
		eventType := event.Event

		// 从 payload 中探测 type 字段
		if payload != "" {
			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(payload), &probe); err == nil && probe.Type != "" {
				eventType = probe.Type
			}
		}

		if payload == "" || eventType == "" {
			continue
		}

		var streamResp model.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &streamResp); err != nil {
			// 解析失败时直接跳过
			continue
		}

		switch eventType {
		case "error":
			// 先解析错误信息
			var errEvent model.ResponseStreamErrorEvent
			errCode := ""
			errMsg := "unknown error"
			if err := json.Unmarshal([]byte(payload), &errEvent); err == nil {
				errCode = errEvent.Code
				errMsg = errEvent.Message
			}
			// 写带错误信息的 chat chunk，让客户端感知错误
			writeChatErrorDelta(c, errMsg, errCode, responseID, modelName)
			render.Done(c)
			sawDone = true
			break loop

		case "response.failed":
			sawFailed = true
			// 获取错误信息
			errMsg := "unknown error"
			errCode := ""
			if streamResp.Response != nil && streamResp.Response.Error != nil {
				errMsg = streamResp.Response.Error.Message
				errCode = streamResp.Response.Error.Code
			}
			// 写带错误信息的 chat chunk，让客户端感知错误
			writeChatErrorDelta(c, errMsg, errCode, responseID, modelName)
			render.Done(c)
			sawDone = true
			break loop

		case "response.created", "response.in_progress":
			responseID = streamResp.ID
			if streamResp.Model != "" {
				modelName = streamResp.Model
			}

		case "response.output_text.delta":
			if streamResp.Delta == nil {
				continue
			}
			deltaStr, ok := streamResp.Delta.(string)
			if !ok || deltaStr == "" {
				continue
			}
			contentBuffer.WriteString(deltaStr)
			writeChatContentDelta(c, deltaStr, responseID, modelName)
			sawFirstToken = true

		case "response.reasoning.delta":
			if streamResp.Delta == nil {
				continue
			}
			deltaStr, ok := streamResp.Delta.(string)
			if !ok || deltaStr == "" {
				continue
			}
			reasoningBuffer.WriteString(deltaStr)
			writeChatReasoningDelta(c, deltaStr, responseID, modelName)
			sawFirstToken = true

		case "response.function_call_arguments.delta":
			if streamResp.Delta == nil {
				continue
			}
			deltaStr, ok := streamResp.Delta.(string)
			if !ok || deltaStr == "" {
				continue
			}
			itemID := streamItemID(&streamResp)
			if itemID == "" {
				continue
			}
			state, exists := toolCallStates[itemID]
			if !exists {
				// 可能是 function_call 的 arguments，但未收到 output_item.added
				// 防御性创建
				state = &toolCallState{index: toolCallIdxSeq}
				toolCallIdxSeq++
				toolCallStates[itemID] = state
			}
			state.argsBuffer.WriteString(deltaStr)
			// 写 tool call delta（需要 id 和 name 已就位）
			if state.id != "" && state.name != "" {
				writeChatToolCallDelta(c, state.index, state.id, state.name, deltaStr, responseID, modelName)
			}

		case "response.output_item.added":
			if streamResp.Item == nil {
				continue
			}
			itemID := streamResp.Item.ID
			if itemID == "" {
				continue
			}
			if streamResp.Item.Type == "function_call" {
				state, exists := toolCallStates[itemID]
				if !exists {
					state = &toolCallState{
						index: toolCallIdxSeq,
						id:    streamResp.Item.CallID,
						name:  streamResp.Item.Name,
					}
					toolCallIdxSeq++
				} else {
					// 已存在（function_call_arguments.delta 先到达），更新 id 和 name
					state.id = streamResp.Item.CallID
					state.name = streamResp.Item.Name
				}
				toolCallStates[itemID] = state

				// 如果已有初始 arguments，写第一个 delta
				initArgs := streamResp.Item.ArgumentsString()
				if initArgs != "" && initArgs != "{}" && initArgs != "null" {
					state.argsBuffer.WriteString(initArgs)
				}
				// 如果 argsBuffer 已有累积内容且 id/name 刚就位，补发自累积以来的第一个 delta
				if state.argsBuffer.Len() > 0 && state.id != "" && state.name != "" {
					writeChatToolCallDelta(c, state.index, state.id, state.name, state.argsBuffer.String(), responseID, modelName)
				}
			}

		case "response.output_item.done":
			if streamResp.Item == nil {
				continue
			}
			itemID := streamResp.Item.ID
			if itemID == "" {
				continue
			}
			completedToolCallIDs = append(completedToolCallIDs, itemID)

		case "response.completed":
			sawCompleted = true
			if streamResp.Response != nil {
				_ = responseID // 已记录
				if streamResp.Response.Usage.InputTokens > 0 || streamResp.Response.Usage.OutputTokens > 0 {
					outUsage = &model.Usage{
						PromptTokens:     streamResp.Response.Usage.InputTokens,
						CompletionTokens: streamResp.Response.Usage.OutputTokens,
						TotalTokens:      streamResp.Response.Usage.TotalTokens,
					}
					if streamResp.Response.Usage.InputTokensDetails != nil && streamResp.Response.Usage.InputTokensDetails.CachedTokens > 0 {
						outUsage.PromptTokensDetails = &model.PromptTokensDetails{
							CachedTokens: streamResp.Response.Usage.InputTokensDetails.CachedTokens,
						}
					}
				}
				if streamResp.Response.Error != nil {
					sawFailed = true
				}
			}
			// 写最终的 finish choice（包含 usage）
			writeChatStreamFinish(c, contentBuffer.String(), reasoningBuffer.String(), toolCallStates, sawFirstToken, outUsage, responseID, modelName)

		default:
			// 其他事件（response.output_item.done 等）暂不处理
		}
	}

	// 流提前结束：补写 finish 事件
	if !sawDone && !sawCompleted && !sawFailed && (sawFirstToken || contentBuffer.Len() > 0) {
		writeChatStreamFinish(c, contentBuffer.String(), reasoningBuffer.String(), toolCallStates, sawFirstToken, outUsage, responseID, modelName)
		render.Done(c)
	}

	_ = resp.Body.Close()

	if streamErr != nil {
		return outUsage, codex.ErrorWrapper(streamErr, "stream_read_error", http.StatusInternalServerError)
	}
	return outUsage, nil
}

// writeChatContentDelta 写一条包含 content 的 Chat delta SSE
func writeChatContentDelta(c *gin.Context, content, responseID, modelName string) {
	choice := map[string]interface{}{
		"index": 0,
		"delta": map[string]interface{}{
			"content": content,
		},
	}
	writeChatDeltaSSE(c, responseID, modelName, []interface{}{choice})
}

// writeChatReasoningDelta 写一条包含 reasoning_content 的 Chat delta SSE
func writeChatReasoningDelta(c *gin.Context, reasoning, responseID, modelName string) {
	choice := map[string]interface{}{
		"index": 0,
		"delta": map[string]interface{}{
			"reasoning_content": reasoning,
		},
	}
	writeChatDeltaSSE(c, responseID, modelName, []interface{}{choice})
}

// writeChatToolCallDelta 写一条包含 tool_calls delta 的 Chat SSE
func writeChatToolCallDelta(c *gin.Context, index int, id, name, argsDelta, responseID, modelName string) {
	toolCall := map[string]interface{}{
		"index": index,
		"id":    id,
		"type":  "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": argsDelta,
		},
	}
	choice := map[string]interface{}{
		"index": 0,
		"delta": map[string]interface{}{
			"tool_calls": []interface{}{toolCall},
		},
	}
	writeChatDeltaSSE(c, responseID, modelName, []interface{}{choice})
}

// writeChatStreamFinish 写最终的 finish delta（不含 content，含 finish_reason 和可选的 usage）
func writeChatStreamFinish(c *gin.Context, content, reasoning string, toolCallStates map[string]*toolCallState, sawFirstToken bool, usage *model.Usage, responseID, modelName string) {
	delta := map[string]interface{}{}
	// 非空 content/reasoning 已在之前的 delta 中发送，此处不再重复
	choice := map[string]interface{}{
		"index":         0,
		"delta":         delta,
		"finish_reason": "stop",
	}
	payload := map[string]interface{}{
		"choices": []interface{}{choice},
	}
	if responseID != "" {
		payload["id"] = fmt.Sprintf("chatcmpl-%s", responseID)
	}
	if modelName != "" {
		payload["model"] = modelName
	}
	payload["object"] = "chat.completion.chunk"
	payload["created"] = 0 // 可选择从上游提取

	if usage != nil {
		payload["usage"] = map[string]interface{}{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	render.StringData(c, string(data))
}

// writeChatDeltaSSE 写一条 Chat Delta SSE 事件
func writeChatDeltaSSE(c *gin.Context, responseID, modelName string, choices []interface{}) {
	payload := map[string]interface{}{
		"choices": choices,
	}
	if responseID != "" {
		payload["id"] = fmt.Sprintf("chatcmpl-%s", responseID)
	}
	if modelName != "" {
		payload["model"] = modelName
	}
	payload["object"] = "chat.completion.chunk"
	payload["created"] = 0

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	render.StringData(c, string(data))
}

// writeChatErrorDelta 写一条包含 error 信息的 Chat delta SSE（在 [DONE] 之前发送，让客户端感知错误）
func writeChatErrorDelta(c *gin.Context, errMsg, errCode, responseID, modelName string) {
	choice := map[string]interface{}{
		"index":         0,
		"delta":         map[string]interface{}{},
		"finish_reason": nil,
	}
	payload := map[string]interface{}{
		"choices": []interface{}{choice},
		"error": map[string]interface{}{
			"message": errMsg,
			"code":    errCode,
		},
	}
	if responseID != "" {
		payload["id"] = fmt.Sprintf("chatcmpl-%s", responseID)
	}
	if modelName != "" {
		payload["model"] = modelName
	}
	payload["object"] = "chat.completion.chunk"
	payload["created"] = 0

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	render.StringData(c, string(data))
}

// streamItemID 从 ResponsesStreamEvent 中提取 item_id
func streamItemID(event *model.ResponsesStreamEvent) string {
	if event.ItemID != "" {
		return event.ItemID
	}
	if event.Item != nil && event.Item.ID != "" {
		return event.Item.ID
	}
	return ""
}

// ==================== Request Conversion ====================

// convertChatToResponsesRequest 把 Chat Completions 请求转为 Responses 请求
func convertChatToResponsesRequest(request *model.GeneralOpenAIRequest) *model.ResponsesRequest {
	respReq := &model.ResponsesRequest{
		Model:       request.Model,
		MaxTokens:   request.MaxTokens,
		Temperature: float64OrZero(request.Temperature),
		TopP:        float64OrZero(request.TopP),
		Stream:      request.Stream,
		User:        request.User,
		ToolChoice:  request.ToolChoice,
	}

	if request.ParallelTooCalls != nil {
		respReq.ParallelToolCalls = request.ParallelTooCalls
	}

	// 构建 input items
	if len(request.Messages) > 0 {
		inputItems := buildInputItems(request.Messages)
		if len(inputItems) > 0 {
			respReq.Input = inputItems
		}
	}

	// 转换 tools
	if len(request.Tools) > 0 {
		rawTools := make([]interface{}, 0, len(request.Tools))
		for _, t := range request.Tools {
			rawTools = append(rawTools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Function.Name,
					"description": t.Function.Description,
					"parameters":  t.Function.Parameters,
				},
			})
		}
		respReq.RawTools = rawTools
	}

	return respReq
}

// buildInputItems 把 chat messages 转为 Responses input 数组
func buildInputItems(messages []model.Message) []interface{} {
	var items []interface{}

	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			content := msg.StringContent()
			if content == "" {
				continue
			}
			items = append(items, map[string]interface{}{
				"type":    "message",
				"role":    "developer",
				"content": content,
			})

		case "user":
			item := map[string]interface{}{
				"type": "message",
				"role": "user",
			}
			if msg.IsStringContent() {
				item["content"] = msg.StringContent()
			} else if contentList := msg.ParseContent(); len(contentList) > 0 {
				var blocks []map[string]interface{}
				for _, c := range contentList {
					if c.Type == model.ContentTypeImageURL && c.ImageURL != nil {
						blocks = append(blocks, map[string]interface{}{
							"type":      "input_image",
							"image_url": c.ImageURL.Url,
						})
					} else {
						block := map[string]interface{}{
							"type": c.Type,
						}
						if c.Type == model.ContentTypeText {
							block["text"] = c.Text
						}
						blocks = append(blocks, block)
					}
				}
				item["content"] = blocks
			}
			items = append(items, item)

		case "assistant":
			// reasoning_content
			if msg.ReasoningContent != nil {
				if rc, ok := msg.ReasoningContent.(string); ok && rc != "" {
					items = append(items, map[string]interface{}{
						"type":    "reasoning",
						"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": rc}},
					})
				}
			}

			// content
			content := msg.StringContent()
			if content != "" {
				items = append(items, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": []interface{}{map[string]interface{}{"type": "output_text", "text": content}},
				})
			}

			// tool_calls
			for _, tc := range msg.ToolCalls {
				args := toolCallArgumentsString(tc.Function.Arguments)
				items = append(items, map[string]interface{}{
					"type":      "function_call",
					"call_id":   tc.Id,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}

		case "tool":
			content := msg.StringContent()
			items = append(items, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": msg.ToolCallId,
				"output":  content,
			})
		}
	}

	return items
}

// toolCallArgumentsString 将 tool call arguments 转为字符串
func toolCallArgumentsString(args interface{}) string {
	if args == nil {
		return "{}"
	}
	switch v := args.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return "{}"
	}
}

// extractToolCallArgs 从 ResponsesItem 中提取 tool_call arguments 字符串
func extractToolCallArgs(item model.ResponsesItem) string {
	args := string(item.Arguments)
	if args == "" || args == "null" {
		args = string(item.Input)
	}
	if args == "" || args == "null" {
		args = "{}"
	}
	return args
}

// ==================== Non-Streaming Response ====================

// handleChatCompletionsResponse 处理 Chat Completions 的非流式响应
func handleChatCompletionsResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, codex.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	resp.Body.Close()

	// 检查上游是否返回了错误
	{
		var errResp struct {
			Error *model.Error `json:"error"`
		}
		if err := json.Unmarshal(responseBody, &errResp); err == nil && errResp.Error != nil && errResp.Error.Message != "" {
			// 尝试把 Responses 错误格式映射为 Chat 格式
			mappedBody := mapResponsesErrorToChatFormat(responseBody)
			resp.Body = io.NopCloser(bytes.NewBuffer(mappedBody))
			for k, v := range resp.Header {
				for _, vv := range v {
					c.Writer.Header().Add(k, vv)
				}
			}
			c.Writer.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(c.Writer, resp.Body)
			resp.Body.Close()
			return nil, nil
		}
	}

	// 把上游 Responses 格式转为 Chat 格式
	chatResp := convertResponsesToChat(responseBody, meta.ActualModelName)

	// 提取 usage
	usage := extractUsageFromResponses(responseBody)

	// 写回客户端
	for k, v := range resp.Header {
		for _, vv := range v {
			c.Writer.Header().Add(k, vv)
		}
	}
	if _, err := c.Writer.Write(chatResp); err != nil {
		return nil, codex.ErrorWrapper(err, "write_response_body_failed", http.StatusInternalServerError)
	}

	c.Set(ctxkey.ResponseBody, string(chatResp))
	return usage, nil
}

// convertResponsesToChat 把 Responses API JSON 响应转为 Chat Completions 格式
func convertResponsesToChat(respBody []byte, modelName string) []byte {
	var responsesResp model.ResponsesResponse
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		return respBody
	}

	chatResp := make(map[string]interface{})
	chatResp["id"] = fmt.Sprintf("chatcmpl-%s", responsesResp.ID)
	chatResp["object"] = "chat.completion"
	chatResp["created"] = responsesResp.Created
	chatResp["model"] = modelName

	msg := make(map[string]interface{})
	msg["role"] = "assistant"

	var reasoningContent string
	var toolCalls []interface{}

	for _, item := range responsesResp.Output {
		switch item.Type {
		case "message":
			var contentStr string
			if content, ok := item.Content.(string); ok {
				contentStr = content
			} else if contentArr, ok := item.Content.([]interface{}); ok {
				for _, block := range contentArr {
					if blockMap, ok := block.(map[string]interface{}); ok {
						if blockType, _ := blockMap["type"].(string); blockType == "output_text" || blockType == "text" {
							if text, ok := blockMap["text"].(string); ok {
								contentStr += text
							}
						}
					}
				}
			}
			msg["content"] = contentStr

		case "reasoning":
			if summary, ok := item.Summary.([]interface{}); ok {
				for _, s := range summary {
					if sm, ok := s.(map[string]interface{}); ok {
						if text, ok := sm["text"].(string); ok {
							reasoningContent += text
						}
					}
				}
			}

		case "function_call", "custom_tool_call":
			args := extractToolCallArgs(item)
			tc := map[string]interface{}{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      item.Name,
					"arguments": args,
				},
			}
			toolCalls = append(toolCalls, tc)
		}
	}

	if reasoningContent != "" {
		msg["reasoning_content"] = reasoningContent
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	// 确保 content 字段存在（即使为空）
	if msg["content"] == nil {
		msg["content"] = ""
	}

	choice := map[string]interface{}{
		"index":   0,
		"message": msg,
	}
	if reason := mapFinishReason(responsesResp.Status); reason != "" {
		choice["finish_reason"] = reason
	}
	chatResp["choices"] = []interface{}{choice}

	// 转换 usage
	usageMap := map[string]interface{}{
		"prompt_tokens":     responsesResp.Usage.InputTokens,
		"completion_tokens": responsesResp.Usage.OutputTokens,
		"total_tokens":      responsesResp.Usage.TotalTokens,
	}
	if responsesResp.Usage.InputTokensDetails != nil && responsesResp.Usage.InputTokensDetails.CachedTokens > 0 {
		usageMap["prompt_tokens_details"] = map[string]interface{}{
			"cached_tokens": responsesResp.Usage.InputTokensDetails.CachedTokens,
		}
	}
	chatResp["usage"] = usageMap

	result, err := json.Marshal(chatResp)
	if err != nil {
		return respBody
	}
	return result
}

// mapResponsesErrorToChatFormat 尝试把 Responses 格式的 error 响应映射为 OpenAI Chat 通用错误格式
func mapResponsesErrorToChatFormat(respBody []byte) []byte {
	var responsesResp struct {
		Error *model.ResponseError `json:"error"`
	}
	if err := json.Unmarshal(respBody, &responsesResp); err != nil || responsesResp.Error == nil {
		return respBody
	}
	chatErr := map[string]interface{}{
		"error": map[string]interface{}{
			"message": responsesResp.Error.Message,
			"type":    responsesResp.Error.Type,
			"code":    responsesResp.Error.Code,
		},
	}
	if errMap, ok := chatErr["error"].(map[string]interface{}); ok {
		if typeVal, exists := errMap["type"]; !exists || typeVal == nil || typeVal == "" {
			errMap["type"] = "invalid_request_error"
		}
	}
	if b, err := json.Marshal(chatErr); err == nil {
		return b
	}
	return respBody
}

// extractUsageFromResponses 从 Responses 响应体中提取 model.Usage
func extractUsageFromResponses(respBody []byte) *model.Usage {
	var responsesResp model.ResponsesResponse
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		return nil
	}
	usage := &model.Usage{
		PromptTokens:     responsesResp.Usage.InputTokens,
		CompletionTokens: responsesResp.Usage.OutputTokens,
		TotalTokens:      responsesResp.Usage.TotalTokens,
	}
	if responsesResp.Usage.InputTokensDetails != nil && responsesResp.Usage.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens: responsesResp.Usage.InputTokensDetails.CachedTokens,
		}
	}
	return usage
}

// mapFinishReason 把 Responses status 映射为 Chat 的 finish_reason
func mapFinishReason(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "failed":
		return "stop"
	case "in_progress":
		return ""
	default:
		return "stop"
	}
}

// float64OrZero 解指针 float64，nil 时返回 0
func float64OrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
