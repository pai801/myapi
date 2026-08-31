package client

import (
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	"net/http"
	"net/url"
	"time"
)

var HTTPClient *http.Client
var ImpatientHTTPClient *http.Client
var UserContentRequestHTTPClient *http.Client

// 与 config 中 USER_CONTENT_REQUEST_TIMEOUT 的 env 默认值 30 保持一致
const defaultUserContentRequestTimeout = 30 * time.Second

func Init() {
	// net/http 中 Timeout<=0 表示永不超过：显式配 0 会复现无限等待，负值会立即超时，
	// 故非正值统一回退默认 30s 并告警
	userContentTimeout := time.Second * time.Duration(config.UserContentRequestTimeout)
	if config.UserContentRequestTimeout <= 0 {
		logger.Log.Warnf("USER_CONTENT_REQUEST_TIMEOUT=%d is not positive, fallback to default %s", config.UserContentRequestTimeout, defaultUserContentRequestTimeout)
		userContentTimeout = defaultUserContentRequestTimeout
	}
	if config.UserContentRequestProxy != "" {
		logger.Log.Infof("using %s as proxy to fetch user content", config.UserContentRequestProxy)
		proxyURL, err := url.Parse(config.UserContentRequestProxy)
		if err != nil {
			logger.Log.Fatalf("USER_CONTENT_REQUEST_PROXY set but invalid: %s", config.UserContentRequestProxy)
		}
		transport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		UserContentRequestHTTPClient = &http.Client{
			Transport: transport,
			Timeout:   userContentTimeout,
		}
	} else {
		UserContentRequestHTTPClient = &http.Client{
			// 与 proxy 分支对齐：无代理时同样施加超时，避免缺省 http.Client 无限等待
			Timeout: userContentTimeout,
		}
	}
	var transport http.RoundTripper
	if config.RelayProxy != "" {
		logger.Log.Infof("using %s as api relay proxy", config.RelayProxy)
		proxyURL, err := url.Parse(config.RelayProxy)
		if err != nil {
			logger.Log.Fatalf("USER_CONTENT_REQUEST_PROXY set but invalid: %s", config.UserContentRequestProxy)
		}
		transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
	}

	if config.RelayTimeout == 0 {
		HTTPClient = &http.Client{
			Transport: transport,
		}
	} else {
		HTTPClient = &http.Client{
			Timeout:   time.Duration(config.RelayTimeout) * time.Second,
			Transport: transport,
		}
	}

	ImpatientHTTPClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
}
