package monitor

import (
	"net/http"
	"strings"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/model"
)

func ShouldDisableChannel(err *model.Error, statusCode int) bool {
	if !config.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if statusCode == http.StatusUnauthorized {
		logger.Log.Debugf("ShouldDisableChannel: status=%d matches rule: unauthorized", statusCode)
		return true
	}
	switch err.Type {
	case "insufficient_quota", "authentication_error", "permission_error", "forbidden":
		logger.Log.Debugf("ShouldDisableChannel: type=%q matches rule: error_type", err.Type)
		return true
	}
	if err.Code == "invalid_api_key" || err.Code == "account_deactivated" {
		logger.Log.Debugf("ShouldDisableChannel: code=%v matches rule: error_code", err.Code)
		return true
	}

	lowerMessage := strings.ToLower(err.Message)
	if strings.Contains(lowerMessage, "your access was terminated") ||
		strings.Contains(lowerMessage, "violation of our policies") ||
		strings.Contains(lowerMessage, "your credit balance is too low") ||
		strings.Contains(lowerMessage, "organization has been disabled") ||
		strings.Contains(lowerMessage, "credit") ||
		strings.Contains(lowerMessage, "balance") ||
		strings.Contains(lowerMessage, "permission denied") ||
		strings.Contains(lowerMessage, "organization has been restricted") || // groq
		strings.Contains(lowerMessage, "api key not valid") || // gemini
		strings.Contains(lowerMessage, "api key expired") || // gemini
		strings.Contains(lowerMessage, "已欠费") {
		logger.Log.Debugf("ShouldDisableChannel: message contains disable keyword: matches rule: message_keyword")
		return true
	}
	return false
}

func ShouldEnableChannel(err error, openAIErr *model.Error) bool {
	if !config.AutomaticEnableChannelEnabled {
		return false
	}
	if err != nil {
		return false
	}
	if openAIErr != nil {
		return false
	}
	return true
}
