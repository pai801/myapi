package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/relay/constant/role"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor/openai"
	billingratio "github.com/pai801/myapi/relay/billing/ratio"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/controller/validator"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

type contextKey int

const (
	CtxKeyRequestBody contextKey = iota
	CtxKeyResponseBody
	CtxKeyRequestHeader
)

var CtxKeyPreConsumedQuota = "pre_consumed_quota"

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

	model.RecordConsumeLog(ctx, &model.Log{
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
		SystemPromptReset: systemPromptReset,
		ChannelName:       meta.ChannelName,
		RequestBody:       requestBody,
		ResponseBody:      responseBody,
		RequestHeader:     requestHeader,
	})
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
