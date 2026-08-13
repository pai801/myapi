package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/active"
	"github.com/pai801/myapi/relay/adaptor/openai"
	billingratio "github.com/pai801/myapi/relay/billing/ratio"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/controller/validator"
	"github.com/pai801/myapi/relay/constant/role"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

type contextKey int

const (
	CtxKeyRequestBody contextKey = iota
	CtxKeyResponseBody
	CtxKeyRequestHeader
	CtxKeyFirstTokenTime
)

var CtxKeyPreConsumedQuota = "pre_consumed_quota"

// ttftWriter 包装 gin.ResponseWriter：流式请求首次写出响应体时记录首字耗时（TTFT，ms）。
// 非流式请求不记录（FirstTokenTime 保持 0）。首次写出通常即 SSE 首帧/首个 data 行。
// 持有 meta 指针而非重新 GetByContext：StartTime 基准与 ElapsedTime 一致，
// IsStream 在包装后可能被入口逻辑更新，写时读取实时值。
// 必须同时覆盖 Write 与 WriteString：内嵌接口的 WriteString 会直接转发到底层 writer，
// 绕过本包装的 Write 埋点（如透传/转换流式路径用 c.Writer.WriteString 写 SSE）。
type ttftWriter struct {
	gin.ResponseWriter
	c    *gin.Context
	meta *meta.Meta
	set  bool
}

func (w *ttftWriter) Write(data []byte) (int, error) {
	w.markFirstToken()
	return w.ResponseWriter.Write(data)
}

func (w *ttftWriter) WriteString(s string) (int, error) {
	w.markFirstToken()
	return w.ResponseWriter.Write([]byte(s))
}

// markFirstToken 首次写出时记录首字耗时，并同步到活跃请求（供前端实时 SSE 展示）。
func (w *ttftWriter) markFirstToken() {
	if w.set {
		return
	}
	w.set = true
	if w.meta == nil || !w.meta.IsStream {
		return
	}
	ms := time.Since(w.meta.StartTime).Milliseconds()
	w.c.Set(ctxkey.FirstTokenTime, ms)
	if rid := w.c.GetString(helper.RequestIdKey); rid != "" {
		active.Global.Update(rid, func(req *active.ActiveRequest) {
			req.FirstTokenMs = ms
		})
	}
}

// wrapTTFTWriter 在 relay 入口包装 ResponseWriter，用于记录流式首字耗时。
func wrapTTFTWriter(c *gin.Context, m *meta.Meta) {
	c.Writer = &ttftWriter{ResponseWriter: c.Writer, meta: m, c: c}
}

// getFirstTokenTime 从 context 读取流式首字耗时（ms），非流式/未记录时为 0。
func getFirstTokenTime(ctx context.Context) int64 {
	if v := ctx.Value(CtxKeyFirstTokenTime); v != nil {
		return v.(int64)
	}
	return 0
}

func getAndValidateTextRequest(c *gin.Context, relayMode int) (*relaymodel.GeneralOpenAIRequest, error) {
	textRequest := &relaymodel.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err != nil {
		return nil, err
	}
	if relayMode == relaymode.Moderations && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	if relayMode == relaymode.Embeddings && textRequest.Model == "" {
		textRequest.Model = c.Param("model")
	}
	err = validator.ValidateTextRequest(textRequest, relayMode)
	if err != nil {
		return nil, err
	}
	return textRequest, nil
}

func getPromptTokens(textRequest *relaymodel.GeneralOpenAIRequest, relayMode int) int {
	switch relayMode {
	case relaymode.ChatCompletions:
		return openai.CountTokenMessages(textRequest.Messages, textRequest.Model)
	case relaymode.Completions:
		return openai.CountTokenInput(textRequest.Prompt, textRequest.Model)
	case relaymode.Moderations:
		return openai.CountTokenInput(textRequest.Input, textRequest.Model)
	}
	return 0
}

func postConsumeQuota(ctx context.Context, usage *relaymodel.Usage, meta *meta.Meta, textRequest *relaymodel.GeneralOpenAIRequest, ratio float64, modelRatio float64, systemPromptReset bool) {
	if usage == nil {
		logger.Log.Errorf("usage is nil, which is unexpected")
		return
	}
	var quota int64
	completionRatio := billingratio.GetCompletionRatio(textRequest.Model, meta.ChannelType)
	groupRatio := model.GetGroupModelRatio(meta.Group)
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
		logger.Log.Warnf("totalTokens is 0 for user %d, model %s, rolling back pre-consumed quota", meta.UserId, textRequest.Model)
		if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
			if err := model.IncreaseUserQuota(meta.UserId, preConsumed); err != nil {
				logger.Log.Errorf("error rolling back pre-consumed quota: " + err.Error())
			}
			model.PostConsumeResetUserQuotaCache(ctx, meta.UserId, preConsumed)
		}
		return
	}

	// Check pre-consumed quota and adjust by the delta
	var err error
	preConsumedQuota := int64(0)
	if v := ctx.Value(CtxKeyPreConsumedQuota); v != nil {
		preConsumedQuota = v.(int64)
	}

	if preConsumedQuota > 0 {
		diff := quota - preConsumedQuota
		if diff > 0 {
			err = model.DecreaseUserQuota(meta.UserId, diff)
		} else if diff < 0 {
			err = model.IncreaseUserQuota(meta.UserId, -diff)
		}
		// diff == 0: exactly right, no adjustment needed
	} else {
		err = model.DecreaseUserQuota(meta.UserId, quota)
	}
	if err != nil {
		logger.Log.Errorf("error decrease user quota: " + err.Error())
	}
	// DB quota has already been updated above; refresh Redis cache from DB.
	model.PostConsumeResetUserQuotaCache(ctx, meta.UserId, quota)

	logContent := fmt.Sprintf("倍率：%.2f × %.2f × 分组%.2f", modelRatio, completionRatio, groupRatio)

	var requestBody string
	if v := ctx.Value(CtxKeyRequestBody); v != nil {
		requestBody = v.(string)
	}
	var responseBody string
	if v := ctx.Value(CtxKeyResponseBody); v != nil {
		responseBody = v.(string)
	}
	var requestHeader string
	if v := ctx.Value(CtxKeyRequestHeader); v != nil {
		requestHeader = v.(string)
	}

	logRecord := &model.Log{
		UserId:            meta.UserId,
		ChannelId:         meta.ChannelId,
		PromptTokens:      promptTokens,
		CompletionTokens:  completionTokens,
		CachedTokens:      cachedTokens,
		ModelName:         textRequest.Model,
		TokenName:         meta.TokenName,
		Quota:             int(quota),
		Content:           logContent,
		IsStream:          meta.IsStream,
		ElapsedTime:       helper.CalcElapsedTime(meta.StartTime),
		FirstTokenTime:    getFirstTokenTime(ctx),
		SystemPromptReset: systemPromptReset,
		ChannelName:       meta.ChannelName,
		RequestBody:       requestBody,
		ResponseBody:      responseBody,
		RequestHeader:     requestHeader,
	}
	model.RecordConsumeLog(ctx, logRecord)

	// DB 写入成功后广播 complete 事件，推送完整日志到前端
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
	model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
	model.UpdateChannelUsedQuota(meta.ChannelId, quota)
}

func getMappedModelName(modelName string, mapping map[string]string) (string, bool) {
	if mapping == nil {
		return modelName, false
	}
	mappedModelName := mapping[modelName]
	if mappedModelName != "" {
		return mappedModelName, true
	}
	return modelName, false
}

func isErrorHappened(meta *meta.Meta, resp *http.Response) bool {
	if resp == nil {
		if meta.ChannelType == channeltype.AwsClaude {
			return false
		}
		return true
	}
	if resp.StatusCode != http.StatusOK &&
		// replicate return 201 to create a task
		resp.StatusCode != http.StatusCreated {
		return true
	}
	if meta.ChannelType == channeltype.DeepL {
		// skip stream check for deepl
		return false
	}

	if meta.IsStream && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") &&
		// Even if stream mode is enabled, replicate will first return a task info in JSON format,
		// requiring the client to request the stream endpoint in the task info
		meta.ChannelType != channeltype.Replicate {
		return true
	}
	return false
}

func setSystemPrompt(ctx context.Context, request *relaymodel.GeneralOpenAIRequest, prompt string) (reset bool) {
	if prompt == "" {
		return false
	}
	if len(request.Messages) == 0 {
		return false
	}
	if request.Messages[0].Role == role.System {
		request.Messages[0].Content = prompt
		logger.Log.Infof("rewrite system prompt")
		return true
	}
	request.Messages = append([]relaymodel.Message{{
		Role:    role.System,
		Content: prompt,
	}}, request.Messages...)
	logger.Log.Infof("add system prompt")
	return true
}
