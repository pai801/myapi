package middleware

import (
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
)

func RelayPanicRecover() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logPanicRecover(c, err)
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{
						"message": fmt.Sprintf("Panic detected, error: %v. Please submit an issue with the related log here: https://github.com/pai801/myapi", err),
						"type":    "myapi_panic",
					},
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}

// logPanicRecover 输出 panic 恢复日志。
//
// 该函数运行在 recover 分支内部，**必须自身绝不 panic**：任何一处取值失败都会让恢复逻辑
// 二次崩溃、进而拖垮整个进程。故对可能为 nil 的字段逐一显式判断，不做「先读后判」的乐观取值，
// 也不依赖 common.GetRequestBody（其内部 io.ReadAll(c.Request.Body) 无 nil 保护）。
func logPanicRecover(c *gin.Context, err any) {
	logger.Log.Errorf("panic detected: %v", err)
	logger.Log.Errorf("stacktrace from panic: %s", string(debug.Stack()))
	logger.Log.Errorf("request: %s", requestLineForLog(c))
	logger.Log.Errorf("request body: %s", requestBodyForLog(c))
}

// requestLineForLog 安全地格式化 "METHOD /path"，任一字段不可用时返回占位文本而非 panic。
func requestLineForLog(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return "<unavailable>"
	}
	if c.Request.URL == nil {
		return c.Request.Method + " <unavailable>"
	}
	return fmt.Sprintf("%s %s", c.Request.Method, c.Request.URL.Path)
}

// requestBodyForLog 读取并格式化请求体用于 panic 日志，任何异常都降级为可读占位文本，
// 绝不返回错误、绝不 panic —— 恢复路径的日志不得因请求体问题而失败。
func requestBodyForLog(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return "<unavailable>"
	}
	// 优先复用已缓存的 body：relay 请求的 body 常已被上游读取并缓存，此时 c.Request.Body
	// 可能已被消费，直接读会拿到空内容，与原先经 common.GetRequestBody 的行为保持一致。
	if cached, exists := c.Get(ctxkey.KeyRequestBody); exists {
		if body, ok := cached.([]byte); ok {
			return formatRequestBodyForLog(body)
		}
	}
	if c.Request.Body == nil {
		return "<unavailable>"
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return fmt.Sprintf("<unavailable: read error: %v>", err)
	}
	return formatRequestBodyForLog(body)
}

// formatRequestBodyForLog 将请求体格式化为单行日志文本。抽成独立函数以便单测直接断言，
// 无需为「日志内容」搭建整套中间件。
//
// 语义（受 config.PanicLogBodyMaxBytes 控制）：
//   - <= 0：完全不输出 body 内容，仅输出长度 —— 应急关闭，防止用户源码/提示词/凭据落盘；
//   - 长度 <= 上限：原样（经安全化）输出；
//   - 长度 >  上限：截断到上限字节并标注原始总长度，明确告知已截断。
//
// 处理顺序为「先按原始字节截断 → 再合法化 UTF-8 → 最后转义 CR/LF」，理由：
//   - 上限 N 的语义是限制「暴露的原始 body 字节数」，故必须对原始字节计数；若先转义再截断，
//     转义会把单个 \n 撑成 2 字节，导致实际暴露的原始内容少于 N、截断位置失真；
//   - 截断可能切坏多字节字符，故随后做 UTF-8 合法化，避免日志出现非法字节乱码；
//   - 最后转义 CR/LF，防止请求体里的换行伪造日志行（CWE-117 日志注入）。
func formatRequestBodyForLog(body []byte) string {
	maxBytes := config.PanicLogBodyMaxBytes
	if maxBytes <= 0 {
		return fmt.Sprintf("<omitted>, length=%d", len(body))
	}
	if len(body) <= maxBytes {
		return sanitizeForLog(string(body))
	}
	return fmt.Sprintf("%s...(truncated, total %d bytes)", sanitizeForLog(string(body[:maxBytes])), len(body))
}

// sanitizeForLog 让任意原始 body 片段可安全写入单行日志：
//  1. strings.ToValidUTF8 清理按字节截断可能切坏的多字节字符（替换为空），保证日志是合法 UTF-8；
//  2. 转义 CR/LF 为字面 \r / \n，防止攻击者用请求体里的换行伪造日志行（CWE-117）。
func sanitizeForLog(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
