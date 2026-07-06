package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/apitype"
	billingratio "github.com/pai801/myapi/relay/billing/ratio"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
)

func RelayTextHelper(c *gin.Context) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	meta := meta.GetByContext(c)
	// get & validate textRequest
	textRequest, err := getAndValidateTextRequest(c, meta.Mode)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "invalid_text_request", err)
		return openai.ErrorWrapper(err, "invalid_text_request", http.StatusBadRequest)
	}
	meta.IsStream = textRequest.Stream

	// map model name
	meta.OriginModelName = textRequest.Model
	textRequest.Model, _ = getMappedModelName(meta.ActualModelName, meta.ModelMapping)
	// set system prompt if not empty
	systemPromptReset := setSystemPrompt(ctx, textRequest, meta.ForcedSystemPrompt)

	// Store request body and headers in context for logging
	bodyJSON, _ := json.Marshal(textRequest)
	const maxBodySize = 256 * 1024 // 256KB
	if len(bodyJSON) <= maxBodySize {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(bodyJSON))
	} else {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, fmt.Sprintf("[body too large: %d bytes]", len(bodyJSON)))
	}
	ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))

	// get model ratio
	modelRatio := billingratio.GetModelRatio(textRequest.Model, meta.ChannelType)
	ratio := modelRatio
	// balance check
	promptTokens := getPromptTokens(textRequest, meta.Mode)
	meta.PromptTokens = promptTokens
	estimatedQuota := int64(float64(500+promptTokens) * ratio)
	if textRequest.MaxTokens != 0 {
		estimatedQuota += int64(float64(textRequest.MaxTokens) * ratio)
	}
	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota < estimatedQuota {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// Pre-consume to close race window between check and actual consumption
	if err := dbmodel.DecreaseUserQuota(meta.UserId, estimatedQuota); err != nil {
		logger.Log.Errorf("pre-consume quota failed for user %d: %v", meta.UserId, err)
		return openai.ErrorWrapper(err, "pre_consume_quota_failed", http.StatusInternalServerError)
	} else {
		ctx = context.WithValue(ctx, CtxKeyPreConsumedQuota, estimatedQuota)
	}

	adaptor := relay.GetAdaptor(meta.APIType)
	if adaptor == nil {
		logger.Log.Errorf("[%s] %+v", "invalid_api_type", fmt.Errorf("invalid api type: %d", meta.APIType))
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}
	adaptor.Init(meta)

	// get request body
	requestBody, err := getRequestBody(c, meta, textRequest, adaptor)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "convert_request_failed", err)
		return openai.ErrorWrapper(err, "convert_request_failed", http.StatusInternalServerError)
	}

	// do request
	resp, err := adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		// Rollback pre-consumed quota
		if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
			_ = dbmodel.IncreaseUserQuota(meta.UserId, preConsumed)
		}
		logger.Log.Errorf("[%s] %+v", "do_request_failed", err)
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	if isErrorHappened(meta, resp) {
		// Rollback pre-consumed quota
		if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
			_ = dbmodel.IncreaseUserQuota(meta.UserId, preConsumed)
		}
		return RelayErrorHandler(resp)
	}

	// do response
	usage, respErr := adaptor.DoResponse(c, resp, meta)
	if respBody := c.GetString(ctxkey.ResponseBody); respBody != "" {
		ctx = context.WithValue(ctx, CtxKeyResponseBody, respBody)
	}
	if respErr != nil {
		// Rollback pre-consumed quota
		if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
			_ = dbmodel.IncreaseUserQuota(meta.UserId, preConsumed)
		}
		logger.Log.Errorf("respErr is not nil: %+v", respErr)
		return respErr
	}
	// post-consume quota
	go postConsumeQuota(ctx, usage, meta, textRequest, ratio, modelRatio, systemPromptReset)
	return nil
}

func getRequestBody(c *gin.Context, meta *meta.Meta, textRequest *model.GeneralOpenAIRequest, adaptor adaptor.Adaptor) (io.Reader, error) {
	if !config.EnforceIncludeUsage &&
		meta.APIType == apitype.OpenAI &&
		meta.OriginModelName == meta.ActualModelName &&
		meta.ChannelType != channeltype.Baichuan &&
		meta.ForcedSystemPrompt == "" {
		// no need to convert request for openai
		return c.Request.Body, nil
	}

	// get request body
	var requestBody io.Reader
	convertedRequest, err := adaptor.ConvertRequest(c, meta.Mode, textRequest)
	if err != nil {
		logger.Log.Debugf("converted request failed: %s\n", err.Error())
		return nil, err
	}
	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		logger.Log.Debugf("converted request json_marshal_failed: %s\n", err.Error())
		return nil, err
	}
	logger.Log.Debugf("converted request: \n%s", string(jsonData))
	requestBody = bytes.NewBuffer(jsonData)
	return requestBody, nil
}
