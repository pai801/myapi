package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/middleware"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/monitor"
	"github.com/songquanpeng/one-api/relay/controller"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/relaymode"
)

// https://platform.openai.com/docs/api-reference/chat

func relayHelper(c *gin.Context, relayMode int) *model.ErrorWithStatusCode {
	var err *model.ErrorWithStatusCode
	switch relayMode {
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
		logger.Debugf(ctx, "request body: %s", string(requestBody))
	}
	channelId := c.GetInt(ctxkey.ChannelId)
	userId := c.GetInt(ctxkey.Id)
	requestModel := c.GetString(ctxkey.RequestModel)
	if requestModel == "" {
		requestModel = "auto"
	}
	bizErr := relayHelper(c, relayMode)
	middleware.AffinityGlobal.Set(userId, requestModel, channelId)
	if bizErr == nil {
		monitor.Emit(channelId, true)
		return
	}
	lastFailedChannelId := channelId
	channelName := c.GetString(ctxkey.ChannelName)
	group := c.GetString(ctxkey.Group)
	go processChannelRelayError(ctx, userId, channelId, channelName, *bizErr)
	requestId := c.GetString(helper.RequestIdKey)
	retryTimes := config.RetryTimes
	if !shouldRetry(c, bizErr.StatusCode) {
		logger.Errorf(ctx, "relay error happen, status code is %d, won't retry in this case", bizErr.StatusCode)
		retryTimes = 0
	}
	// Use the original request model (could be "auto" or specific model)
	// to maintain proper distribution behavior during retries.
	for i := retryTimes; i > 0; i-- {
		channel, suggestedModel, err := middleware.SelectChannel(group, requestModel, lastFailedChannelId, userId)
		if err != nil {
			logger.Errorf(ctx, "DistributeForRetry failed: %+v", err)
			break
		}
		logger.Infof(ctx, "using channel #%d model:%v to retry (remain times %d)", channel.Id, suggestedModel, i)
		middleware.SetupContextForSelectedChannel(c, channel, suggestedModel)
		requestBody, err := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		bizErr = relayHelper(c, relayMode)
		middleware.AffinityGlobal.Set(userId, requestModel, channel.Id)
		if bizErr == nil {
			return
		}
		channelId = c.GetInt(ctxkey.ChannelId)
		lastFailedChannelId = channelId
		channelName = c.GetString(ctxkey.ChannelName)
		go processChannelRelayError(ctx, userId, channelId, channelName, *bizErr)
	}
	if bizErr != nil {
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

func shouldRetry(c *gin.Context, statusCode int) bool {
	if _, ok := c.Get(ctxkey.SpecificChannelId); ok {
		return false
	}
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode/100 == 5 {
		return true
	}
	if statusCode == http.StatusBadRequest {
		return false
	}
	if statusCode/100 == 2 {
		return false
	}
	return true
}

func buildFailureLog(c *gin.Context, bizErr *model.ErrorWithStatusCode, channelName string) *dbmodel.Log {
	respBody, _ := json.Marshal(bizErr.Error) // safe: Error struct has only simple types
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
		RequestHeader: fmt.Sprintf("%v", c.Request.Header),
	}
}

func recordFailureLog(c *gin.Context, bizErr *model.ErrorWithStatusCode, channelName string) {
	log := buildFailureLog(c, bizErr, channelName)
	dbmodel.RecordConsumeLog(c.Request.Context(), log)
}

func processChannelRelayError(ctx context.Context, userId int, channelId int, channelName string, err model.ErrorWithStatusCode) {
	logger.Errorf(ctx, "relay error (channel id %d, user id: %d): %s", channelId, userId, err.Message)
	// https://platform.openai.com/docs/guides/error-codes/api-errors
	if monitor.ShouldDisableChannel(&err.Error, err.StatusCode) {
		monitor.DisableChannel(channelId, channelName, err.Message)
	} else {
		monitor.Emit(channelId, false)
		middleware.CooldownGlobal.Put(channelId)
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := model.Error{
		Message: "API not implemented",
		Type:    "one_api_error",
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
