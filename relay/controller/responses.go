package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	dbmodel "github.com/pai801/myapi/model"
	relay2 "github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/active"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/apitype"
	billingratio "github.com/pai801/myapi/relay/billing/ratio"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/constant"
	metaPkg "github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

func RelayResponsesHelper(c *gin.Context) *model.ErrorWithStatusCode {
	ctxMeta := metaPkg.GetByContext(c)
	// 包装 ResponseWriter 记录流式首字耗时（TTFT）
	wrapTTFTWriter(c, ctxMeta)

	// 对于 /v1/responses/compact 接口，只允许 Codex 渠道，否则返回错误
	if ctxMeta.Mode == relaymode.ResponsesCompact {
		if ctxMeta.APIType != apitype.Codex {
			return &model.ErrorWithStatusCode{
				Error: model.Error{
					Message: "unsupported endpoint \"/v1/responses/compact\", only Codex channels are supported",
					Type:    "invalid_request_error",
					Code:    "invalid_request",
				},
				StatusCode: http.StatusBadRequest,
			}
		}
		// Codex 渠道直接转发
		return relayResponsesDirect(c, ctxMeta)
	}

	// 普通 /v1/responses 接口的原有处理逻辑
	// DeepSeek 官方已原生支持 Responses 协议（V4-Flash 起），直接透传避免转换层损失（如 effort=max 被误映射为 auto 导致 400）
	if ctxMeta.APIType == apitype.Codex || ctxMeta.APIType == apitype.ChatGPTSub || ctxMeta.ChannelType == channeltype.DeepSeek {
		return relayResponsesDirect(c, ctxMeta)
	}

	return relayResponsesConverted(c, ctxMeta)
}

func relayResponsesDirect(c *gin.Context, ctxMeta *metaPkg.Meta) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	relayAdaptor := relay2.GetAdaptor(ctxMeta.APIType)
	if relayAdaptor == nil {
		logger.Log.Errorf("[%s] %+v", "invalid api type", nil)
		return openai.ErrorWrapper(nil, "invalid api type", http.StatusBadRequest)
	}
	relayAdaptor.Init(ctxMeta)

	requestBody, err := common.GetRequestBody(c)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "get request body failed", err)
		return openai.ErrorWrapper(err, "get request body failed", http.StatusInternalServerError)
	}

	// 解析请求体以获取模型名称和流式标记
	var req map[string]interface{}
	if err := json.Unmarshal(requestBody, &req); err != nil {
		logger.Log.Warnf("[responses] failed to parse request body: %v", err)
	} else {
		if modelName, ok := req["model"].(string); ok {
			ctxMeta.OriginModelName = modelName
		}
		if ctxMeta.ActualModelName == "" {
			if modelName, ok := req["model"].(string); ok {
				if mapped, ok := getMappedModelName(modelName, ctxMeta.ModelMapping); ok {
					ctxMeta.ActualModelName = mapped
				} else {
					ctxMeta.ActualModelName = modelName
				}
			}
		}
		if stream, ok := req["stream"].(bool); ok {
			ctxMeta.IsStream = stream
		}
	}

	// 替换请求体中的 model 字段为映射后的实际模型名
	upstreamBody := requestBody
	if ctxMeta.OriginModelName != ctxMeta.ActualModelName && ctxMeta.ActualModelName != "" {
		req["model"] = ctxMeta.ActualModelName
		if modifiedBody, err := json.Marshal(req); err == nil {
			upstreamBody = modifiedBody
			logger.Log.Debugf("[responsesDirect] model mapped: %s -> %s", ctxMeta.OriginModelName, ctxMeta.ActualModelName)
		} else {
			logger.Log.Warnf("[responsesDirect] failed to marshal modified request body: %v", err)
		}
	}

	// 存储请求体和请求头到 context 中
	if len(upstreamBody) <= config.MaxLoggedBodySize {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(upstreamBody))
	} else {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, fmt.Sprintf("[body too large: %d bytes]", len(upstreamBody)))
	}
	ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))

	// 获取模型比率和分组比率
	modelRatio := billingratio.GetModelRatio(ctxMeta.ActualModelName, ctxMeta.ChannelType)
	groupRatio := dbmodel.GetGroupModelRatio(ctxMeta.Group)
	ratio := modelRatio * groupRatio

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, ctxMeta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	estimatedQuota := int64(float64(500+estimateResponsesPromptTokens(req)) * ratio)
	// 预扣估算需纳入输出上限 max_output_tokens，否则低余额用户可用大输出参数把余额打成大负数
	if maxOutputTokens := estimateResponsesMaxOutputTokens(req); maxOutputTokens > 0 {
		estimatedQuota += int64(float64(maxOutputTokens) * ratio)
	}
	if userQuota < estimatedQuota {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// Pre-consume to close race window between check and actual consumption
	if err := dbmodel.DecreaseUserQuota(ctxMeta.UserId, estimatedQuota); err != nil {
		logger.Log.Errorf("pre-consume quota failed for user %d: %v", ctxMeta.UserId, err)
		return openai.ErrorWrapper(err, "pre_consume_quota_failed", http.StatusInternalServerError)
	} else {
		ctx = context.WithValue(ctx, CtxKeyPreConsumedQuota, estimatedQuota)
	}

	resp, err := relayAdaptor.DoRequest(c, ctxMeta, bytes.NewBuffer(upstreamBody))
	if err != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("[%s] %+v", "do request failed", err)
		return openai.ErrorWrapper(err, "do request failed", http.StatusInternalServerError)
	}

	if isErrorResp(resp) {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		return relayErrorHandler(resp)
	}

	usage, relayErr := handleResponsesDirect(c, resp, ctxMeta, relayAdaptor)
	if respBody := c.GetString(ctxkey.ResponseBody); respBody != "" {
		ctx = context.WithValue(ctx, CtxKeyResponseBody, respBody)
	}
	if ttft := c.GetInt64(ctxkey.FirstTokenTime); ttft > 0 {
		ctx = context.WithValue(ctx, CtxKeyFirstTokenTime, ttft)
	}
	if relayErr != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("DoResponse failed: %+v", relayErr)
		return relayErr
	}

	// 后消费逻辑 - 在 goroutine 外提取需要从 ctx 读取的值
	reqBody := ""
	respBody := ""
	reqHeader := ""
	if v := ctx.Value(CtxKeyRequestBody); v != nil {
		reqBody = v.(string)
	}
	if v := ctx.Value(CtxKeyResponseBody); v != nil {
		respBody = v.(string)
	}
	if v := ctx.Value(CtxKeyRequestHeader); v != nil {
		reqHeader = v.(string)
	}
	go postConsumeQuotaForResponses(ctx, usage, ctxMeta, ratio, modelRatio, reqBody, respBody, reqHeader)

	return nil
}

func relayResponsesConverted(c *gin.Context, ctxMeta *metaPkg.Meta) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	relayAdaptor := relay2.GetAdaptor(ctxMeta.APIType)
	if relayAdaptor == nil {
		logger.Log.Errorf("[%s] %+v", "failed to get openai adaptor", nil)
		return openai.ErrorWrapper(nil, "failed to get openai adaptor", http.StatusInternalServerError)
	}
	relayAdaptor.Init(ctxMeta)

	requestBody, err := common.GetRequestBody(c)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "get request body failed", err)
		return openai.ErrorWrapper(err, "get request body failed", http.StatusInternalServerError)
	}

	var req map[string]interface{}
	if err := json.Unmarshal(requestBody, &req); err != nil {
		logger.Log.Errorf("[%s] %+v", "invalid request body", err)
		return openai.ErrorWrapper(err, "invalid request body", http.StatusBadRequest)
	}

	modelName := ctxMeta.ActualModelName
	if modelName == "" {
		if m, ok := req["model"].(string); ok {
			modelName = m
		}
	}
	modelName, _ = getMappedModelName(modelName, ctxMeta.ModelMapping)

	stream := false
	if s, ok := req["stream"].(bool); ok {
		stream = s
	}
	ctxMeta.IsStream = stream

	// 决定是否对仅 reasoning 无 content 的响应兜底生成 message 事件
	fallbackReasoning := false
	if strings.Contains(strings.ToLower(modelName), "deepseek") {
		fallbackReasoning = true
	}

	chatRequest := codex.ConvertResponsesToChatRequest(modelName, requestBody, stream)

	chatRequestReader := bytes.NewBuffer(chatRequest)

	chatMeta := &metaPkg.Meta{
		Mode:               relaymode.ChatCompletions,
		ChannelType:        ctxMeta.ChannelType,
		ChannelId:          ctxMeta.ChannelId,
		TokenId:            ctxMeta.TokenId,
		TokenName:          ctxMeta.TokenName,
		UserId:             ctxMeta.UserId,
		Group:              ctxMeta.Group,
		ModelMapping:       ctxMeta.ModelMapping,
		OriginModelName:    modelName,
		ActualModelName:    modelName,
		BaseURL:            ctxMeta.BaseURL,
		APIKey:             ctxMeta.APIKey,
		APIType:            apitype.OpenAI,
		Config:             ctxMeta.Config,
		IsStream:           stream,
		RequestURLPath:     "/v1/chat/completions",
		ForcedSystemPrompt: ctxMeta.ForcedSystemPrompt,
		StartTime:          ctxMeta.StartTime,
		ChannelName:        ctxMeta.ChannelName,
	}

	// 存储请求体和请求头到 context 中
	if len(chatRequest) <= config.MaxLoggedBodySize {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(chatRequest))
	} else {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, fmt.Sprintf("[body too large: %d bytes]", len(chatRequest)))
	}
	ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))

	// 获取模型比率和分组比率
	modelRatio := billingratio.GetModelRatio(modelName, ctxMeta.ChannelType)
	groupRatio := dbmodel.GetGroupModelRatio(ctxMeta.Group)
	ratio := modelRatio * groupRatio

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, ctxMeta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	estimatedQuota := int64(float64(500+estimateResponsesPromptTokens(req)) * ratio)
	// 预扣估算需纳入输出上限 max_output_tokens，否则低余额用户可用大输出参数把余额打成大负数
	if maxOutputTokens := estimateResponsesMaxOutputTokens(req); maxOutputTokens > 0 {
		estimatedQuota += int64(float64(maxOutputTokens) * ratio)
	}
	if userQuota < estimatedQuota {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// Pre-consume to close race window between check and actual consumption
	if err := dbmodel.DecreaseUserQuota(ctxMeta.UserId, estimatedQuota); err != nil {
		logger.Log.Errorf("pre-consume quota failed for user %d: %v", ctxMeta.UserId, err)
		return openai.ErrorWrapper(err, "pre_consume_quota_failed", http.StatusInternalServerError)
	} else {
		ctx = context.WithValue(ctx, CtxKeyPreConsumedQuota, estimatedQuota)
	}

	relayAdaptor.Init(chatMeta)

	resp, err := relayAdaptor.DoRequest(c, chatMeta, chatRequestReader)
	if err != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("[%s] %+v", "do request failed", err)
		return openai.ErrorWrapper(err, "do request failed", http.StatusInternalServerError)
	}

	if isErrorResp(resp) {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		return relayErrorHandler(resp)
	}

	finalUsage := &model.Usage{}

	if stream {
		// 流式响应处理
		common.SetEventStreamHeaders(c)
		c.Writer.WriteHeader(http.StatusOK)
		var converterState any
		streamResult, _ := forwardChatResponsesStream(c, resp.Body, requestBody, &converterState, fallbackReasoning)
		if streamResult.FailureError != nil || streamResult.FailedTerminal {
			rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
			if streamResult.FailureError != nil {
				logger.Log.Errorf("[%s] %+v", "scan response failed", streamResult.FailureError)
			}
			if streamResult.FailedTerminal {
				logger.Log.Warnf("responses stream failed after SSE headers committed")
			}
			if err := resp.Body.Close(); err != nil {
				logger.Log.Warnf("failed to close response body: %v", err)
			}
			return nil
		}

		// 从流状态中提取 usage 和完整的响应体用于日志记录
		if converterState != nil {
			pt, ct, tt, cachedT := codex.GetStreamUsage(converterState)
			finalUsage.PromptTokens = pt
			finalUsage.CompletionTokens = ct
			finalUsage.TotalTokens = tt
			// 如果有缓存命中的token，设置到 PromptTokensDetails 中
			if cachedT > 0 {
				finalUsage.PromptTokensDetails = &model.PromptTokensDetails{
					CachedTokens: cachedT,
				}
			}

			if !streamResult.StreamErrored {
				completedBody := codex.GetStreamCompletedBody(converterState, requestBody)
				if completedBody != nil {
					ctx = context.WithValue(ctx, CtxKeyResponseBody, string(completedBody))
				}
			}
		}

		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body: %v", err)
		}
	} else {
		// 非流式响应处理
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
			logger.Log.Errorf("[%s] %+v", "read response body failed", err)
			return openai.ErrorWrapper(err, "read response body failed", http.StatusInternalServerError)
		}
		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body: %v", err)
		}

		ctx = context.WithValue(ctx, CtxKeyResponseBody, string(respBody))

		responsesResponse := codex.ConvertChatResponseToResponsesWithContext(respBody, modelName, fallbackReasoning, requestBody)
		c.JSON(http.StatusOK, json.RawMessage(responsesResponse))

		// 解析 usage
		var chatResponse map[string]interface{}
		if err := json.Unmarshal(respBody, &chatResponse); err == nil {
			if usage, ok := chatResponse["usage"].(map[string]interface{}); ok {
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					finalUsage.PromptTokens = int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					finalUsage.CompletionTokens = int(ct)
				}
				if tt, ok := usage["total_tokens"].(float64); ok {
					finalUsage.TotalTokens = int(tt)
				}
				// 解析 prompt_tokens_details.cached_tokens
				if promptTokensDetails, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
					if cachedTokens, ok := promptTokensDetails["cached_tokens"].(float64); ok && int(cachedTokens) > 0 {
						finalUsage.PromptTokensDetails = &model.PromptTokensDetails{
							CachedTokens: int(cachedTokens),
						}
					}
				}
			}
		}
	}

	// 后消费逻辑 - 在 goroutine 外提取需要从 ctx 读取的值
	if ttft := c.GetInt64(ctxkey.FirstTokenTime); ttft > 0 {
		ctx = context.WithValue(ctx, CtxKeyFirstTokenTime, ttft)
	}
	reqBody := ""
	respBody := ""
	reqHeader := ""
	if v := ctx.Value(CtxKeyRequestBody); v != nil {
		reqBody = v.(string)
	}
	if v := ctx.Value(CtxKeyResponseBody); v != nil {
		respBody = v.(string)
	}
	if v := ctx.Value(CtxKeyRequestHeader); v != nil {
		reqHeader = v.(string)
	}
	go postConsumeQuotaForResponses(ctx, finalUsage, ctxMeta, ratio, modelRatio, reqBody, respBody, reqHeader)

	return nil
}

// responsesUsage 解析 responses 协议的 usage 字段（DeepSeek 原生透传用）。
// responses 协议 token 字段为 input_tokens/output_tokens，与 chat 协议（prompt_tokens/completion_tokens）不同。
type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// toModelUsage 把 responses usage 映射到网关内部 Usage（用于扣费与日志）。
func (u *responsesUsage) toModelUsage() *model.Usage {
	if u == nil {
		return nil
	}
	usage := &model.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &model.PromptTokensDetails{CachedTokens: u.InputTokensDetails.CachedTokens}
	}
	return usage
}

// handleResponsesDirect 处理 responses 原生透传的响应。
// 上游为 DeepSeek 等原生支持 Responses 协议的 OpenAI 兼容渠道时使用：
// openai adaptor 的 DoResponse 只认 chat 格式（choices/usage.prompt_tokens），
// responses 格式（顶层 usage.input_tokens）会提取失真，流式下甚至不转发任何 SSE 数据，
// 因此这里直接原样透传响应，并单独按 responses 格式提取 usage。
func handleResponsesDirect(c *gin.Context, resp *http.Response, meta *metaPkg.Meta, relayAdaptor adaptor.Adaptor) (*model.Usage, *model.ErrorWithStatusCode) {
	if meta.ChannelType == channeltype.DeepSeek {
		if meta.IsStream {
			return handleResponsesDirectStream(c, resp)
		}
		return handleResponsesDirectNonStream(c, resp)
	}
	// Codex / ChatGPTSub 等渠道走各自适配器（已支持 responses 格式）
	return relayAdaptor.DoResponse(c, resp, meta)
}

func handleResponsesDirectNonStream(c *gin.Context, resp *http.Response) (*model.Usage, *model.ErrorWithStatusCode) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	_ = resp.Body.Close()

	// 兜底：上游 200 但 body 为错误 JSON（如 effort 非法）时直接返回错误，不转发
	var payload struct {
		Error *model.Error    `json:"error"`
		Usage *responsesUsage `json:"usage"`
	}
	_ = json.Unmarshal(responseBody, &payload)
	if payload.Error != nil && payload.Error.Message != "" {
		return nil, &model.ErrorWithStatusCode{Error: *payload.Error, StatusCode: resp.StatusCode}
	}

	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(responseBody)

	c.Set(ctxkey.ResponseBody, string(responseBody))
	return payload.Usage.toModelUsage(), nil
}

func handleResponsesDirectStream(c *gin.Context, resp *http.Response) (*model.Usage, *model.ErrorWithStatusCode) {
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	// 逐行原样转发 SSE 事件（response.created → ... → response.completed，无 [DONE]），
	// 同时从 response.completed 事件中提取 usage 用于扣费。
	// 响应体记录：SSE 流无整体 body，通过 responsesStreamAccumulator 把各事件合并成
	// 等价于非流式响应的完整 JSON（快照整体吸收、delta 增量拼接），存入 ctxkey.ResponseBody 供日志展示。
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, constant.ScannerBufferInitial), constant.ScannerBufferMax)
	var usage *model.Usage
	acc := newResponsesStreamAccumulator()
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := c.Writer.WriteString(line + "\n"); err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			acc.addPayload([]byte(payload))
			var evt struct {
				Type     string `json:"type"`
				Response *struct {
					Usage *responsesUsage `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				if evt.Type == "response.completed" && evt.Response != nil {
					usage = evt.Response.Usage.toModelUsage()
				}
			}
		}
	}
	c.Writer.Flush()
	_ = resp.Body.Close()

	if body := acc.buildResponseBody(); body != "" {
		c.Set(ctxkey.ResponseBody, body)
	}

	if usage == nil {
		usage = &model.Usage{}
	}
	return usage, nil
}

// responsesStreamAccumulator 把 responses 协议 SSE 事件合并为完整 response JSON（日志记录用）。
// 对齐 openai adaptor 的 chatStreamAccumulator 模式：快照类事件（response.created/output_item.done/
// response.completed）整体吸收字段，增量类事件（output_text.delta/function_call_arguments.delta）
// 拼接文本，最终 buildResponseBody 输出等价于非流式响应体的完整 JSON。
type responsesStreamAccumulator struct {
	resp    map[string]any // 顶层 response 对象（含 output 数组）
	output  []any          // 累积的 output items
	curItem map[string]any // 当前正在累积文本/参数的 output item
}

func newResponsesStreamAccumulator() *responsesStreamAccumulator {
	return &responsesStreamAccumulator{
		resp: map[string]any{
			"object": "response",
			"output": []any{},
		},
	}
}

func (a *responsesStreamAccumulator) addPayload(payload []byte) {
	var evt map[string]any
	if err := json.Unmarshal(payload, &evt); err != nil {
		return
	}
	evtType, _ := evt["type"].(string)
	switch evtType {
	case "response.created", "response.in_progress", "response.completed", "response.failed":
		if resp, ok := evt["response"].(map[string]any); ok {
			a.absorbResponse(resp)
		}
	case "response.output_item.added":
		if item, ok := evt["item"].(map[string]any); ok {
			a.absorbItem(item)
		}
	case "response.content_part.added":
		if part, ok := evt["part"].(map[string]any); ok {
			a.appendContentPart(part)
		}
	case "response.output_text.delta":
		if delta, ok := evt["delta"].(string); ok {
			a.appendOutputText(delta)
		}
	case "response.output_text.done":
		if text, ok := evt["text"].(string); ok {
			a.setOutputText(text)
		}
	case "response.function_call_arguments.delta":
		if delta, ok := evt["delta"].(string); ok {
			a.appendFunctionArgs(delta)
		}
	case "response.function_call_arguments.done":
		if args, ok := evt["arguments"].(string); ok {
			a.setFunctionArgs(args)
		}
	case "response.output_item.done":
		if item, ok := evt["item"].(map[string]any); ok {
			a.replaceItem(item)
		}
	}
}

// absorbResponse 吸收 response 快照字段；快照带完整 output 时整体替换，否则保留已累积的 output。
func (a *responsesStreamAccumulator) absorbResponse(resp map[string]any) {
	if output, ok := resp["output"].([]any); ok && len(output) > 0 {
		a.output = output
		a.resp["output"] = output
	}
	for k, v := range resp {
		if k == "output" {
			continue
		}
		a.resp[k] = v
	}
}

// absorbItem 追加新的 output item（output_item.added）。
func (a *responsesStreamAccumulator) absorbItem(item map[string]any) {
	a.curItem = item
	a.output = append(a.output, item)
	a.resp["output"] = a.output
}

// appendContentPart 追加 content part；若当前 item 已有同类型占位 part 则跳过（避免重复）。
func (a *responsesStreamAccumulator) appendContentPart(part map[string]any) {
	if a.curItem == nil {
		return
	}
	content, _ := a.curItem["content"].([]any)
	partType, _ := part["type"].(string)
	for _, cv := range content {
		if cm, ok := cv.(map[string]any); ok {
			if t, _ := cm["type"].(string); t == partType && t == "output_text" {
				return
			}
		}
	}
	a.curItem["content"] = append(content, part)
}

// lastOutputTextPart 返回当前 item 的最后一个 output_text part（delta 文本的落点）。
func (a *responsesStreamAccumulator) lastOutputTextPart() map[string]any {
	if a.curItem == nil {
		return nil
	}
	content, _ := a.curItem["content"].([]any)
	for i := len(content) - 1; i >= 0; i-- {
		if cm, ok := content[i].(map[string]any); ok {
			if t, _ := cm["type"].(string); t == "output_text" {
				return cm
			}
		}
	}
	return nil
}

// appendOutputText 把 output_text.delta 拼接到当前文本后。
func (a *responsesStreamAccumulator) appendOutputText(delta string) {
	part := a.lastOutputTextPart()
	if part == nil {
		return
	}
	text, _ := part["text"].(string)
	part["text"] = text + delta
}

// setOutputText 用 output_text.done 的完整文本覆盖。
func (a *responsesStreamAccumulator) setOutputText(text string) {
	if part := a.lastOutputTextPart(); part != nil {
		part["text"] = text
	}
}

// appendFunctionArgs 把 function_call_arguments.delta 拼接到当前 item 的 arguments 后。
func (a *responsesStreamAccumulator) appendFunctionArgs(delta string) {
	if a.curItem == nil {
		return
	}
	args, _ := a.curItem["arguments"].(string)
	a.curItem["arguments"] = args + delta
}

// setFunctionArgs 用 function_call_arguments.done 的完整 arguments 覆盖。
func (a *responsesStreamAccumulator) setFunctionArgs(args string) {
	if a.curItem != nil {
		a.curItem["arguments"] = args
	}
}

// replaceItem 用 output_item.done 的完整 item 快照替换同 id 的累积 item（未匹配则追加）。
func (a *responsesStreamAccumulator) replaceItem(item map[string]any) {
	id, _ := item["id"].(string)
	for i, v := range a.output {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if mid, _ := m["id"].(string); mid == id {
			a.output[i] = item
			if a.curItem != nil {
				if curID, _ := a.curItem["id"].(string); curID == id {
					a.curItem = item
				}
			}
			a.resp["output"] = a.output
			return
		}
	}
	a.absorbItem(item)
}

// buildResponseBody 序列化合并后的完整 response JSON。
func (a *responsesStreamAccumulator) buildResponseBody() string {
	body, err := json.Marshal(a.resp)
	if err != nil {
		logger.Log.Errorf("buildResponseBody marshal failed: " + err.Error())
		return ""
	}
	return string(body)
}

type chatResponsesStreamResult struct {
	StreamErrored   bool
	FailedTerminal  bool
	SuccessTerminal bool
	TerminalSeen    bool
	FailureError    *model.Error
}

type convertedEventMeta struct {
	EventName string
	Failed    bool
	Completed bool
	StreamErr *model.Error
}

func parseConvertedEventMeta(converted string) convertedEventMeta {
	meta := convertedEventMeta{}
	lines := strings.Split(converted, "\n")
	var payloadLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "event: ") {
			meta.EventName = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			payloadLines = append(payloadLines, strings.TrimPrefix(line, "data: "))
		}
	}
	payload := strings.Join(payloadLines, "\n")
	if meta.EventName == "response.completed" {
		meta.Completed = true
		return meta
	}
	if meta.EventName == "response.failed" {
		meta.Failed = true
		if payload != "" {
			var evt struct {
				Response *struct {
					Error *model.Error `json:"error"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err == nil && evt.Response != nil && evt.Response.Error != nil {
				meta.StreamErr = evt.Response.Error
			}
		}
		return meta
	}
	if meta.EventName == "error" {
		meta.Failed = true
		if payload != "" {
			var evt model.ResponseStreamErrorEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				e := model.Error{Message: evt.Message, Type: "upstream_error", Code: evt.Code}
				if e.Message == "" {
					e.Message = "upstream stream error"
				}
				if e.Code == nil || e.Code == "" {
					e.Code = "server_error"
				}
				meta.StreamErr = &e
			}
		}
	}
	return meta
}

func inspectConvertedResponsesEvents(c *gin.Context, convertedEvents []string) chatResponsesStreamResult {
	result := chatResponsesStreamResult{}
	for _, converted := range convertedEvents {
		_, _ = c.Writer.WriteString(converted)
		meta := parseConvertedEventMeta(converted)
		if meta.Completed {
			result.SuccessTerminal = true
			result.TerminalSeen = true
		}
		if meta.Failed {
			result.StreamErrored = true
			result.FailedTerminal = true
			result.TerminalSeen = true
			if meta.StreamErr != nil {
				result.FailureError = meta.StreamErr
			}
		}
	}
	return result
}

func forwardChatResponsesStream(c *gin.Context, body io.Reader, requestBody []byte, converterState *any, fallbackReasoning bool) (chatResponsesStreamResult, error) {
	reader := bufio.NewReaderSize(body, constant.ScannerBufferInitial)
	result := chatResponsesStreamResult{}
	for {
		event, err := codex.ReadSSEEvent(reader, constant.ScannerBufferMax*2)
		if err != nil {
			if err == io.EOF {
				// 如果转换器已累积状态但未生成终态事件，合成 [DONE] 触发 response.completed
				if !result.SuccessTerminal && !result.FailedTerminal && converterState != nil && *converterState != nil {
					synthEvents := codex.ConvertOpenAIChatToResponsesWithContext(
						requestBody, nil, []byte("data: [DONE]"), converterState, fallbackReasoning)
					eventResult := inspectConvertedResponsesEvents(c, synthEvents)
					if eventResult.SuccessTerminal {
						result.SuccessTerminal = true
					}
					c.Writer.Flush()
				}
				return result, nil
			}
			codex.RenderTerminalStreamReadErrorEvent(c, err)
			c.Writer.Flush()
			result.StreamErrored = true
			result.FailedTerminal = true
			if result.FailureError == nil {
				result.FailureError = &model.Error{Message: err.Error(), Type: "stream_read_error", Code: "bad_response"}
			}
			return result, err
		}
		if event.Event == "" && event.Data == "" {
			continue
		}

		rawLine := "data: " + event.Data
		convertedEvents := codex.ConvertOpenAIChatToResponsesWithContext(requestBody, nil, []byte(rawLine), converterState, fallbackReasoning)
		eventResult := inspectConvertedResponsesEvents(c, convertedEvents)
		if eventResult.SuccessTerminal {
			result.SuccessTerminal = true
		}
		if eventResult.FailedTerminal {
			result.StreamErrored = true
			result.FailedTerminal = true
			result.FailureError = eventResult.FailureError
		}
		c.Writer.Flush()
		if result.FailedTerminal || result.SuccessTerminal {
			// 消费剩余 body 以确保连接可复用
			_, _ = io.Copy(io.Discard, body)
			return result, nil
		}
	}
}

func formatSSEEvent(event codex.SSEEvent) string {
	var b strings.Builder
	if event.Event != "" {
		b.WriteString("event: ")
		b.WriteString(event.Event)
		b.WriteByte('\n')
	}
	if event.Data != "" {
		for i, line := range strings.Split(event.Data, "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("data: ")
			b.WriteString(line)
		}
	}
	return b.String()
}

// maxOutputTokensCap 单请求输出上限，用于 clamp 预扣估算输入，防止极大值导致额度计算溢出
const maxOutputTokensCap = 1_000_000

// estimateResponsesMaxOutputTokens 提取 responses 请求的输出上限（max_output_tokens）。
// 返回 0 表示未指定或非法，调用方保持默认兜底估算。
// req 是 json.Unmarshal 产物（数字为 float64），1e300 级极大值直接 int() 转换会溢出为负数，
// 使预扣额度变负；故转换前 clamp 到 maxOutputTokensCap
func estimateResponsesMaxOutputTokens(req map[string]interface{}) int {
	if v, ok := req["max_output_tokens"].(float64); ok && v > 0 {
		if v > maxOutputTokensCap {
			return maxOutputTokensCap
		}
		return int(v)
	}
	return 0
}

func estimateResponsesPromptTokens(req map[string]interface{}) int {
	if req == nil {
		return 0
	}
	promptTokens := 0

	// 估算 instructions 的 token 数
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		promptTokens += openai.CountTokenInput(instructions, "")
	}

	// 估算 input 的 token 数
	if input, ok := req["input"]; ok {
		switch v := input.(type) {
		case string:
			promptTokens += openai.CountTokenInput(v, "")
		case []interface{}:
			// 简单估算：每个消息大约 100 个 token
			promptTokens += len(v) * 100
		}
	}

	// 估算 tools 的 token 数
	if tools, ok := req["tools"].([]interface{}); ok {
		// 每个 tool 大约 200 个 token
		promptTokens += len(tools) * 200
	}

	// 确保至少有一些 token 数
	if promptTokens < 10 {
		promptTokens = 10
	}

	return promptTokens
}

func rollbackResponsesPreConsumedQuota(ctx context.Context, userId int) {
	if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
		if err := dbmodel.IncreaseUserQuota(userId, preConsumed); err != nil {
			logger.Log.Errorf("error rolling back pre-consumed quota: %v", err)
			return
		}
		dbmodel.PostConsumeResetUserQuotaCache(ctx, userId, preConsumed)
	}
}

func postConsumeQuotaForResponses(ctx context.Context, usage *model.Usage, meta *metaPkg.Meta, ratio float64, modelRatio float64, reqBody string, respBody string, reqHeader string) {
	if usage == nil {
		logger.Log.Errorf("usage is nil, which is unexpected")
		return
	}

	var quota int64
	completionRatio := billingratio.GetCompletionRatio(meta.ActualModelName, meta.ChannelType)
	promptTokens := usage.PromptTokens
	completionTokens := usage.CompletionTokens
	// 从 usage 中提取缓存命中的token数
	cachedTokens := 0
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	quota = int64(math.Ceil((float64(promptTokens) + float64(completionTokens)*completionRatio) * ratio))

	if ratio != 0 && quota <= 0 {
		quota = 1
	}

	totalTokens := promptTokens + completionTokens

	// Check pre-consumed quota early so we can rollback if totalTokens == 0
	preConsumedQuota := int64(0)
	if v := ctx.Value(CtxKeyPreConsumedQuota); v != nil {
		preConsumedQuota = v.(int64)
	}

	if totalTokens == 0 {
		if preConsumedQuota > 0 {
			logger.Log.Warnf("totalTokens is 0 for user %d, model %s, rolling back pre-consumed quota", meta.UserId, meta.ActualModelName)
			if err := dbmodel.IncreaseUserQuota(meta.UserId, preConsumedQuota); err != nil {
				logger.Log.Errorf("error rolling back pre-consumed quota: " + err.Error())
			}
			dbmodel.PostConsumeResetUserQuotaCache(ctx, meta.UserId, preConsumedQuota)
		}
		return
	}

	var err error
	if preConsumedQuota > 0 {
		diff := quota - preConsumedQuota
		if diff > 0 {
			err = dbmodel.DecreaseUserQuota(meta.UserId, diff)
		} else if diff < 0 {
			err = dbmodel.IncreaseUserQuota(meta.UserId, -diff)
		}
		// diff == 0: exactly right, no adjustment needed
	} else {
		err = dbmodel.DecreaseUserQuota(meta.UserId, quota)
	}
	if err != nil {
		logger.Log.Errorf("error decrease user quota: " + err.Error())
	}
	// DB quota has already been updated above; refresh Redis cache from DB.
	dbmodel.PostConsumeResetUserQuotaCache(ctx, meta.UserId, quota)

	groupRatio := dbmodel.GetGroupModelRatio(meta.Group)
	logContent := fmt.Sprintf("Responses API - 倍率：%.2f × %.2f × 分组%.2f", modelRatio, completionRatio, groupRatio)

	logRecord := &dbmodel.Log{
		UserId:            meta.UserId,
		ChannelId:         meta.ChannelId,
		PromptTokens:      promptTokens,
		CompletionTokens:  completionTokens,
		CachedTokens:      cachedTokens,
		ModelName:         meta.ActualModelName,
		TokenName:         meta.TokenName,
		Quota:             int(quota),
		Content:           logContent,
		IsStream:          meta.IsStream,
		ElapsedTime:       helper.CalcElapsedTime(meta.StartTime),
		FirstTokenTime:    getFirstTokenTime(ctx),
		SystemPromptReset: false,
		ChannelName:       meta.ChannelName,
		RequestBody:       reqBody,
		ResponseBody:      respBody,
		RequestHeader:     reqHeader,
	}
	dbmodel.RecordConsumeLog(ctx, logRecord)

	if logRecord.Id > 0 {
		active.BroadcastComplete(&active.LogRecordData{
			Id:               logRecord.Id,
			UserId:           logRecord.UserId,
			CreatedAt:        logRecord.CreatedAt,
			Content:          logRecord.Content,
			Username:         logRecord.Username,
			TokenName:        logRecord.TokenName,
			ModelName:        logRecord.ModelName,
			Quota:            logRecord.Quota,
			PromptTokens:     logRecord.PromptTokens,
			CompletionTokens: logRecord.CompletionTokens,
			CachedTokens:     logRecord.CachedTokens,
			ChannelId:        logRecord.ChannelId,
			RequestId:        logRecord.RequestId,
			ElapsedTime:      logRecord.ElapsedTime,
			FirstTokenTime:   logRecord.FirstTokenTime,
			IsStream:         logRecord.IsStream,
			ChannelName:      logRecord.ChannelName,
			HasRequestBody:   logRecord.RequestBody != "",
			HasResponseBody:  logRecord.ResponseBody != "",
			HasRequestHeader: logRecord.RequestHeader != "",
		})
	}

	dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
	dbmodel.UpdateChannelUsedQuota(meta.ChannelId, quota)
}

func isErrorResp(resp *http.Response) bool {
	return resp.StatusCode != http.StatusOK
}

func relayErrorHandler(resp *http.Response) *model.ErrorWithStatusCode {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "read response body failed", err)
		return openai.ErrorWrapper(err, "read response body failed", http.StatusInternalServerError)
	}
	err = resp.Body.Close()
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "close response body failed", err)
		return openai.ErrorWrapper(err, "close response body failed", http.StatusInternalServerError)
	}
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	var openaiErr model.Error
	err = json.Unmarshal(respBody, &openaiErr)
	if err != nil {
		logger.Log.Errorf("[%s] raw response: %s, err: %+v", "unmarshal response body failed", string(respBody), err)
		openaiErr = model.Error{
			Message: string(respBody),
			Type:    "server_error",
			Code:    "response_parse_error",
		}
	}
	if openaiErr.Message == "" {
		openaiErr.Message = string(respBody)
	}
	return &model.ErrorWithStatusCode{
		Error:      openaiErr,
		StatusCode: resp.StatusCode,
	}
}
