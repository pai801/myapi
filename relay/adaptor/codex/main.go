package codex

import (
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/client"
	"github.com/songquanpeng/one-api/relay/adaptor"
	"github.com/songquanpeng/one-api/relay/meta"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/relaymode"
)

var _ adaptor.Adaptor = (*Adaptor)(nil)

type Adaptor struct {
	OpenAiImpl adaptor.Adaptor
	meta       *meta.Meta
}

func (a *Adaptor) Init(meta *meta.Meta) {
	a.meta = meta
}

func (a *Adaptor) GetRequestURL(meta *meta.Meta) (string, error) {
	baseURL := strings.TrimSuffix(meta.BaseURL, "/")
	if strings.HasPrefix(meta.RequestURLPath, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	return baseURL + meta.RequestURLPath, nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	return nil
}

func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	return request, nil
}

func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	return request, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error) {
	url, _ := a.GetRequestURL(meta)
	req, err := http.NewRequest("POST", url, requestBody)
	if err != nil {
		return nil, err
	}
	a.SetupRequestHeader(c, req, meta)

	client := client.HTTPClient
	return client.Do(req)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	// Chat Completions / Completions 模式：上游返回的是 chat 格式 SSE，用 openai 的 handler 解析
	if meta.Mode == relaymode.ChatCompletions || meta.Mode == relaymode.Completions {
		return a.OpenAiImpl.DoResponse(c, resp, meta)
	}
	// Responses API 模式：上游返回的是 Responses 格式
	if meta.IsStream {
		err, _, usage := StreamResponsesHandler(c, resp)
		return usage, err
	}
	return DoResponsesResponse(c, resp, meta)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return "codex"
}
