package adaptor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/relay/meta"
)

// excludedRequestHeaders 是全量透传时需要排除的请求头（小写）。
// 这些头不应转发到上游，原因包括：hop-by-hop 协议头、IP/网络信息泄露、
// 认证相关（由各 adaptor 接管）、浏览器特定头、CORS 头等。
var excludedRequestHeaders = map[string]bool{
	// === Hop-by-hop (RFC 2616 13.5.1) ===
	"host":              true,
	"content-length":    true,
	"accept-encoding":   true,
	"connection":        true,
	"transfer-encoding": true,
	"proxy-connection":  true,
	"keep-alive":        true,
	"upgrade":           true,
	"trailer":           true,
	"te":                true,

	// === IP / network disclosure ===
	"x-forwarded-for":    true,
	"x-forwarded-host":   true,
	"x-forwarded-proto":  true,
	"x-real-ip":          true,
	"x-client-ip":        true,
	"true-client-ip":     true,
	"cf-connecting-ip":   true,
	"cf-ray":             true,
	"forwarded":          true,
	"x-forwarded-server": true,
	"via":                true,
	"x-cache":            true,

	// === Authentication (adaptor handles) ===
	"authorization": true,
	"cookie":        true,

	// === HTTP/1.0 legacy ===
	"pragma":    true,
	"expect":    true,
	"dnt":       true,
	"from":      true,
	"save-data": true,

	// === Conditional / Range ===
	"if-modified-since":   true,
	"if-none-match":       true,
	"if-match":            true,
	"if-range":            true,
	"if-unmodified-since": true,
	"range":               true,

	// === Browser-specific (Sec-*) ===
	"sec-fetch-site":    true,
	"sec-fetch-mode":    true,
	"sec-fetch-dest":    true,
	"sec-fetch-user":    true,
	"sec-ch-ua":              true,
	"sec-ch-ua-mobile":       true,
	"sec-ch-ua-platform":     true,
	"sec-websocket-key":        true,
	"sec-websocket-version":    true,
	"sec-websocket-extensions": true,

	// === CORS / Referrer ===
	"origin":                        true,
	"referer":                       true,
	"access-control-request-headers":  true,
	"access-control-request-method":   true,

	// === Internal / proxy ===
	"x-http-method-override": true,
}

// IsExcludedRequestHeader 检查指定头是否应被排除透传。
// 对外暴露，供其他 package（如 proxy adaptor）使用。
func IsExcludedRequestHeader(key string) bool {
	return excludedRequestHeaders[strings.ToLower(key)]
}

// SetupCommonRequestHeader 全量透传客户端请求头到上游请求，
// 排除不应转发的头（hop-by-hop、IP 泄露、认证等）。
//
// 各 adaptor 应在此函数之后设置各自认证头，覆盖排除后的值。
func SetupCommonRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) {
	for k, v := range c.Request.Header {
		if !IsExcludedRequestHeader(k) {
			req.Header.Set(k, v[0])
		}
	}

	// 流模式下确保 Accept 正确
	if meta.IsStream && c.Request.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/event-stream")
	}
}

func DoRequestHelper(a Adaptor, c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error) {
	fullRequestURL, err := a.GetRequestURL(meta)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	req, err := http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	err = a.SetupRequestHeader(c, req, meta)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	resp, err := DoRequest(c, req)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}

func DoRequest(c *gin.Context, req *http.Request) (*http.Response, error) {
	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("resp is nil")
	}
	_ = req.Body.Close()
	_ = c.Request.Body.Close()
	return resp, nil
}
