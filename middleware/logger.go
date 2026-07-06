package middleware

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/env"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
)

func SetUpLogger(server *gin.Engine) {
	skipPaths := getSkipPaths()
	server.Use(func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		c.Next()

		for _, p := range skipPaths {
			if strings.HasPrefix(path, p) {
				return
			}
		}

		latency := time.Since(start)
		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()

		var requestID string
		if c.Keys != nil {
			if rid, ok := c.Keys[helper.RequestIdKey]; ok {
				requestID, _ = rid.(string)
			}
		}

		if raw != "" {
			path = path + "?" + raw
		}

		logger.Log.Infow(path,
			"status", statusCode,
			"latency", latency,
			"client_ip", clientIP,
			"method", method,
			"request_id", requestID,
		)
	})
}

func getSkipPaths() []string {
	raw := env.String("LOG_SKIP_PATHS", "/api/status")
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
