package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/middleware"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/monitor"
	"github.com/pai801/myapi/relay/controller"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

// https://platform.openai.com/docs/api-reference/chat

func relayHelper(c *gin.Context, relayMode int) *model.ErrorWithStatusCode {
	var err *model.ErrorWithStatusCode
	switch relayMode {
	case relaymode.Responses:
		fallthrough
	case relaymode.ResponsesCompact:
		err = controller.RelayResponsesHelper(c)
	case relaymode.ImagesGenerations:
		err = controller.RelayImageHelper(c, relayMode)
	case relaymode.AudioSpeech:
		fallthrough
	case relaymode.AudioTranslation:
		fallthrough
	case relaymode.AudioTranscription:
		err = controller.RelayAudioHelper(c, relayMode)
	case relaymode.Proxy:
		err = controller.RelayProxyHelper(c, relayMode)
	default:
		err = controller.RelayTextHelper(c)
	}
	return err
}

func Relay(c *gin.Context) {
	ctx := c.Request.Context()
	relayMode := relaymode.GetByPath(c.Request.URL.Path)
	if config.DebugEnabled {
		requestBody, _ := common.GetRequestBody(c)
		logger.Log.Debugf("request body: %s", string(requestBody))
	}
	channelId := c.GetInt(ctxkey.ChannelId)
	userId := c.GetInt(ctxkey.Id)
	requestModel := c.GetString(ctxkey.RequestModel)
	if requestModel == "" {
		requestModel = "auto"
	}
	bizErr := relayHelper(c, relayMode)
	if bizErr == nil {
		middleware.AffinityGlobal.Set(userId, requestModel, channelId)
		monitor.Emit(channelId, true)
		return
	}
	lastFailedChannelId := channelId
	channelName := c.GetString(ctxkey.ChannelName)
	group := c.GetString(ctxkey.Group)
	go processChannelRelayError(ctx, userId, channelId, channelName, *bizErr)
	requestId := c.GetString(helper.RequestIdKey)
	retryTimes := config.RetryTimes
	if !shouldRetry(c, bizErr) {
		logger.Log.Infof("shouldRetry=false statusCode=%d requestId=%s lastFailedChannel=%d", bizErr.StatusCode, requestId, lastFailedChannelId)
		retryTimes = 0
	} else {
		logger.Log.Debugf("shouldRetry=true statusCode=%d retryTimes=%d requestId=%s", bizErr.StatusCode, retryTimes, requestId)
	}
	// Use the original request model (could be "auto" or specific model)
	// to maintain proper distribution behavior during retries.
	for i := retryTimes; i > 0; i-- {
		channel, suggestedModel, err := middleware.SelectChannel(ctx, group, requestModel, lastFailedChannelId, userId)
		if err != nil {
			logger.Log.Errorf("DistributeForRetry failed: %+v", err)
			break
		}
		logger.Log.Infof("retry attempt=%d remaining=%d failedChannel=%d selectedChannel=%d model=%s requestId=%s",
			retryTimes-i+1, i, lastFailedChannelId, channel.Id, suggestedModel, requestId)
		middleware.SetupContextForSelectedChannel(c, channel, suggestedModel)
		requestBody, err := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		bizErr = relayHelper(c, relayMode)
		if bizErr == nil {
			logger.Log.Infof("retry succeeded on channel #%d requestId=%s", channel.Id, requestId)
			middleware.AffinityGlobal.Set(userId, requestModel, channel.Id)
			return
		}
		channelId = c.GetInt(ctxkey.ChannelId)
		lastFailedChannelId = channelId
		channelName = c.GetString(ctxkey.ChannelName)
		logger.Log.Debugf("retry failed channel #%d status=%d requestId=%s", channelId, bizErr.StatusCode, requestId)
		go processChannelRelayError(ctx, userId, channelId, channelName, *bizErr)
	}
	if bizErr != nil {
		logger.Log.Infof("all retries exhausted lastFailedChannel=%d status=%d requestId=%s model=%s group=%s",
			lastFailedChannelId, bizErr.StatusCode, requestId, requestModel, group)
		if bizErr.StatusCode == http.StatusTooManyRequests {
			bizErr.Error.Message = "当前分组上游负载已饱和，请稍后再试"
		}

		// BUG: bizErr is in race condition
		bizErr.Error.Message = helper.MessageWithRequestId(bizErr.Error.Message, requestId)
		c.JSON(bizErr.StatusCode, gin.H{
			"error": bizErr.Error,
		})
		recordFailureLog(c, bizErr, channelName)
	}
}

func shouldRetry(c *gin.Context, bizErr *model.ErrorWithStatusCode) bool {
	if _, ok := c.Get(ctxkey.SpecificChannelId); ok {
		return false
	}
	if bizErr == nil {
		return false
	}
	statusCode := bizErr.StatusCode
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode/100 == 5 {
		return true
	}
	if isExplicitAdapterFailure(bizErr) {
		return true
	}
	if statusCode/100 == 2 {
		return false
	}
	if statusCode == http.StatusBadRequest {
		return isProviderCompatibilityError(bizErr)
	}
	if isClientSideStatus(statusCode) {
		return isProviderCompatibilityError(bizErr)
	}
	if looksLikeRequestShapeFailure(bizErr) {
		return false
	}
	return true
}

func isClientSideStatus(statusCode int) bool {
	return statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError
}

func isExplicitAdapterFailure(bizErr *model.ErrorWithStatusCode) bool {
	if bizErr == nil {
		return false
	}
	text := errorSemanticText(bizErr)
	if strings.Contains(text, "bad_response") {
		return true
	}
	transportMarkers := []string{
		"failed to parse",
		"parse upstream response",
		"malformed response",
		"invalid response",
		"bad response",
		"empty response",
		"resp is nil",
		"transport error",
		"response handling failure",
	}
	for _, marker := range transportMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isProviderCompatibilityError(bizErr *model.ErrorWithStatusCode) bool {
	if bizErr == nil {
		return false
	}
	text := errorSemanticText(bizErr)
	compatibilityMarkers := []string{
		"unsupported_model",
		"model_not_supported",
		"model is not supported",
		"unsupported by this provider",
		"unsupported by this channel",
		"not compatible with this provider",
		"does not support this model",
	}
	for _, marker := range compatibilityMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func looksLikeRequestShapeFailure(bizErr *model.ErrorWithStatusCode) bool {
	if bizErr == nil {
		return false
	}
	text := errorSemanticText(bizErr)
	requestShapeMarkers := []string{
		"invalid_request_error",
		"invalid request",
		"invalid input",
		"invalid parameter",
		"invalid schema",
		"invalid format",
		"malformed",
		"unsupported_request",
		"request body",
		"request schema",
	}
	for _, marker := range requestShapeMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func errorSemanticText(bizErr *model.ErrorWithStatusCode) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(bizErr.Type)),
		strings.ToLower(strings.TrimSpace(fmt.Sprint(bizErr.Code))),
		strings.ToLower(strings.TrimSpace(bizErr.Message)),
	}
	return strings.Join(parts, " | ")
}

func buildFailureLog(c *gin.Context, bizErr *model.ErrorWithStatusCode, channelName string) *dbmodel.Log {
	respBody, _ := json.Marshal(bizErr.Error)
	requestBody, _ := common.GetRequestBody(c)
	return &dbmodel.Log{
		UserId:        c.GetInt(ctxkey.Id),
		ChannelId:     c.GetInt(ctxkey.ChannelId),
		Quota:         0,
		Content:       fmt.Sprintf("HTTP status: %d, error: %s", bizErr.StatusCode, bizErr.Error.Message),
		ChannelName:   channelName,
		TokenName:     c.GetString(ctxkey.TokenName),
		ModelName:     c.GetString(ctxkey.RequestModel),
		ResponseBody:  string(respBody),
		RequestBody:   string(requestBody),
		RequestHeader: controller.MaskAuthorizationHeader(c.Request.Header),
	}
}

func recordFailureLog(c *gin.Context, bizErr *model.ErrorWithStatusCode, channelName string) {
	log := buildFailureLog(c, bizErr, channelName)
	dbmodel.RecordConsumeLog(c.Request.Context(), log)
}

func processChannelRelayError(ctx context.Context, userId int, channelId int, channelName string, err model.ErrorWithStatusCode) {
	logger.Log.Errorf("relay error (channel id %d, user id: %d): %s", channelId, userId, err.Message)
	// https://platform.openai.com/docs/guides/error-codes/api-errors
	if monitor.ShouldDisableChannel(&err.Error, err.StatusCode) {
		logger.Log.Infof("processChannelRelayError: disabling channel #%d (%s) reason=%q statusCode=%d", channelId, channelName, err.Message, err.StatusCode)
		monitor.DisableChannel(channelId, channelName, err.Message)
	} else {
		logger.Log.Infof("processChannelRelayError: cooling down channel #%d (%s) reason=%q statusCode=%d", channelId, channelName, err.Message, err.StatusCode)
		monitor.Emit(channelId, false)
		middleware.CooldownGlobal.Put(channelId)
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := model.Error{
		Message: "API not implemented",
		Type:    "myapi_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := model.Error{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}
