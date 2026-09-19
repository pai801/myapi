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

// defaultSkipPaths 是未显式配置 LOG_SKIP_PATHS 时的默认「跳过访问日志」路径前缀。
//
// 基础项 /api/status 为默认。扩展渠道可在 init() 期经 RegisterDefaultSkipPath 追加
// 自己的回调路径——例如某 OAuth 回调的 query 携带 refreshToken/userJwt，日志中间件在
// c.Next() 前已快照 RawQuery，handler 侧脱敏无效，只能整条跳过；扩展路径不再硬编码在
// 本仓源码里（PRD §6.6）。
var defaultSkipPaths = []string{"/api/status"}

// RegisterDefaultSkipPath 向默认跳过名单追加一条路径前缀。
//
// 仅供扩展方在 init() 期调用；须在 SetUpLogger 之前完成注册。
// 用户显式设置 LOG_SKIP_PATHS 时仍以显式值为准，本默认名单不生效。
func RegisterDefaultSkipPath(prefix string) {
	defaultSkipPaths = append(defaultSkipPaths, prefix)
}

// DefaultSkipPaths 返回当前默认跳过名单的副本（供诊断与验收测试使用）。
func DefaultSkipPaths() []string {
	out := make([]string, len(defaultSkipPaths))
	copy(out, defaultSkipPaths)
	return out
}

func getSkipPaths() []string {
	raw := env.String("LOG_SKIP_PATHS", strings.Join(defaultSkipPaths, ","))
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
