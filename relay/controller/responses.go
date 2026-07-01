package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	relay2 "github.com/songquanpeng/one-api/relay"
	"github.com/songquanpeng/one-api/relay/adaptor/codex"
	"github.com/songquanpeng/one-api/relay/adaptor/openai"
	"github.com/songquanpeng/one-api/relay/apitype"
	"github.com/songquanpeng/one-api/relay/billing"
	billingratio "github.com/songquanpeng/one-api/relay/billing/ratio"
	"github.com/songquanpeng/one-api/relay/constant"
	metaPkg "github.com/songquanpeng/one-api/relay/meta"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/relaymode"
)

func RelayResponsesHelper(c *gin.Context) *model.ErrorWithStatusCode {
	ctxMeta := metaPkg.GetByContext(c)

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
	if ctxMeta.APIType == apitype.Codex {
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
			if ctxMeta.ActualModelName == "" {
				if mapped, ok := getMappedModelName(modelName, ctxMeta.ModelMapping); ok {
					ctxMeta.ActualModelName = mapped
				}
			}
		}
		if stream, ok := req["stream"].(bool); ok {
			ctxMeta.IsStream = stream
		}
	}

	// 存储请求体和请求头到 context 中
	if config.LogConsumeEnabled {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(requestBody))
		ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))
	}

	// 获取模型比率和分组比率
	modelRatio := billingratio.GetModelRatio(ctxMeta.ActualModelName, ctxMeta.ChannelType)
	groupRatio := billingratio.GetGroupRatio(ctxMeta.Group)
	ratio := modelRatio * groupRatio

	// 估算 prompt tokens 并预扣费
	promptTokens := estimateResponsesPromptTokens(req)
	preConsumedQuota, bizErr := preConsumeQuotaForResponses(ctx, promptTokens, ratio, ctxMeta)
	if bizErr != nil {
		logger.Log.Warnf("preConsumeQuota failed: %+v", *bizErr)
		return bizErr
	}

	resp, err := relayAdaptor.DoRequest(c, ctxMeta, bytes.NewBuffer(requestBody))
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "do request failed", err)
		return openai.ErrorWrapper(err, "do request failed", http.StatusInternalServerError)
	}

	if isErrorResp(resp) {
		billing.ReturnPreConsumedQuota(ctx, preConsumedQuota, ctxMeta.TokenId)
		return relayErrorHandler(resp)
	}

	usage, relayErr := relayAdaptor.DoResponse(c, resp, ctxMeta)
	if config.LogConsumeEnabled {
		if respBody := c.GetString(ctxkey.ResponseBody); respBody != "" {
			ctx = context.WithValue(ctx, CtxKeyResponseBody, respBody)
		}
	}
	if relayErr != nil {
		logger.Log.Errorf("DoResponse failed: %+v", relayErr)
		billing.ReturnPreConsumedQuota(ctx, preConsumedQuota, ctxMeta.TokenId)
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
	go postConsumeQuotaForResponses(ctx, usage, ctxMeta, ratio, preConsumedQuota, modelRatio, groupRatio, reqBody, respBody, reqHeader)

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
	if config.LogConsumeEnabled {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(chatRequest))
		ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))
	}

	// 获取模型比率和分组比率
	modelRatio := billingratio.GetModelRatio(modelName, ctxMeta.ChannelType)
	groupRatio := billingratio.GetGroupRatio(ctxMeta.Group)
	ratio := modelRatio * groupRatio

	// 估算 prompt tokens 并预扣费
	promptTokens := estimateResponsesPromptTokens(req)
	preConsumedQuota, bizErr := preConsumeQuotaForResponses(ctx, promptTokens, ratio, ctxMeta)
	if bizErr != nil {
		logger.Log.Warnf("preConsumeQuota failed: %+v", *bizErr)
		return bizErr
	}

	relayAdaptor.Init(chatMeta)

	resp, err := relayAdaptor.DoRequest(c, chatMeta, chatRequestReader)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "do request failed", err)
		return openai.ErrorWrapper(err, "do request failed", http.StatusInternalServerError)
	}

	if isErrorResp(resp) {
		billing.ReturnPreConsumedQuota(ctx, preConsumedQuota, ctxMeta.TokenId)
		return relayErrorHandler(resp)
	}

	finalUsage := &model.Usage{}

	if stream {
		// 流式响应处理
		common.SetEventStreamHeaders(c)
		c.Writer.WriteHeader(http.StatusOK)
		var converterState any
		streamResult, _ := forwardChatResponsesStream(c, resp.Body, requestBody, &converterState, fallbackReasoning)
		if streamResult.FailureError != nil {
			logger.Log.Errorf("[%s] %+v", "scan response failed", streamResult.FailureError)
		}
		if streamResult.FailedTerminal {
			billing.ReturnPreConsumedQuota(ctx, preConsumedQuota, ctxMeta.TokenId)
			logger.Log.Warnf("responses stream failed after SSE headers committed")
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

			if config.LogConsumeEnabled && !streamResult.StreamErrored {
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
			logger.Log.Errorf("[%s] %+v", "read response body failed", err)
			billing.ReturnPreConsumedQuota(ctx, preConsumedQuota, ctxMeta.TokenId)
			return openai.ErrorWrapper(err, "read response body failed", http.StatusInternalServerError)
		}
		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body: %v", err)
		}

		if config.LogConsumeEnabled {
			ctx = context.WithValue(ctx, CtxKeyResponseBody, string(respBody))
		}

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
	go postConsumeQuotaForResponses(ctx, finalUsage, ctxMeta, ratio, preConsumedQuota, modelRatio, groupRatio, reqBody, respBody, reqHeader)

	return nil
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

func preConsumeQuotaForResponses(ctx context.Context, promptTokens int, ratio float64, meta *metaPkg.Meta) (int64, *model.ErrorWithStatusCode) {
	preConsumedQuota := int64(0)

	// 对于流式请求，预扣费可以设置一个较小的值
	if meta.IsStream {
		preConsumedQuota = 500
	} else {
		// 非流式请求：基于 prompt tokens 计算预扣费
		preConsumedTokens := 500 + int64(promptTokens)
		preConsumedQuota = int64(math.Ceil(float64(preConsumedTokens) * ratio))
	}

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "get_user_quota_failed", err)
		return preConsumedQuota, openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}

	if userQuota-preConsumedQuota < 0 {
		logger.Log.Errorf("[%s] %+v", "insufficient_user_quota", nil)
		return preConsumedQuota, openai.ErrorWrapper(nil, "insufficient_user_quota", http.StatusForbidden)
	}

	// 判断是否信任用户：配额充足时不预扣
	if userQuota > 100*preConsumedQuota {
		preConsumedQuota = 0
		//		logger.Log.Infof("user %d has enough quota %d, trusted and no need to pre-consume", meta.UserId, userQuota)
	}

	if preConsumedQuota > 0 {
		err = dbmodel.CacheDecreaseUserQuota(meta.UserId, preConsumedQuota)
		if err != nil {
			logger.Log.Errorf("[%s] %+v", "decrease_user_quota_failed", err)
			return preConsumedQuota, openai.ErrorWrapper(err, "decrease_user_quota_failed", http.StatusInternalServerError)
		}

		err = dbmodel.PreConsumeTokenQuota(meta.TokenId, preConsumedQuota)
		if err != nil {
			logger.Log.Errorf("[%s] %+v", "pre_consume_token_quota_failed", err)
			return preConsumedQuota, openai.ErrorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
	}

	return preConsumedQuota, nil
}

func postConsumeQuotaForResponses(ctx context.Context, usage *model.Usage, meta *metaPkg.Meta, ratio float64, preConsumedQuota int64, modelRatio float64, groupRatio float64, reqBody string, respBody string, reqHeader string) {
	if usage == nil {
		logger.Log.Errorf("usage is nil, which is unexpected")
		usage = &model.Usage{
			CompletionTokensDetails: &model.CompletionTokensDetails{},
		}
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
	if totalTokens == 0 {
		quota = 0
	}

	quotaDelta := quota - preConsumedQuota
	err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quotaDelta)
	if err != nil {
		logger.Log.Errorf("error consuming token remain quota: " + err.Error())
	}

	err = dbmodel.CacheUpdateUserQuota(ctx, meta.UserId)
	if err != nil {
		logger.Log.Errorf("error update user quota cache: " + err.Error())
	}

	logContent := fmt.Sprintf("Responses API - 倍率：%.2f × %.2f × %.2f", modelRatio, groupRatio, completionRatio)

	dbmodel.RecordConsumeLog(ctx, &dbmodel.Log{
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
		SystemPromptReset: false,
		ChannelName:       meta.ChannelName,
		RequestBody:       reqBody,
		ResponseBody:      respBody,
		RequestHeader:     reqHeader,
	})

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
