package middleware

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/env"
	"github.com/songquanpeng/one-api/common/helper"
)

func SetUpLogger(server *gin.Engine) {
	skipPaths := getSkipPaths()
	server.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		Formatter: func(param gin.LogFormatterParams) string {
			var requestID string
			if param.Keys != nil {
				requestID = param.Keys[helper.RequestIdKey].(string)
			}
			return fmt.Sprintf("[GIN] %s | %s | %3d | %13v | %15s | %7s %s\n",
				param.TimeStamp.Format("2006/01/02 - 15:04:05"),
				requestID,
				param.StatusCode,
				param.Latency,
				param.ClientIP,
				param.Method,
				param.Path,
			)
		},
		SkipPaths: skipPaths,
	}))
}

func getSkipPaths() []string {
	raw := env.String("LOG_SKIP_PATHS", "/api/status")
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
