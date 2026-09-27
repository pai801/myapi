package adaptor

import (
	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"io"
	"net/http"
)

type Adaptor interface {
	Init(meta *meta.Meta)
	GetRequestURL(meta *meta.Meta) (string, error)
	SetupRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) error
	ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error)
	ConvertImageRequest(request *model.ImageRequest) (any, error)
	DoRequest(c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error)
	DoResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (usage *model.Usage, err *model.ErrorWithStatusCode)
	// GetModelList 返回该渠道的默认模型清单，**仅用于界面展示与候选**。
	//
	// 该清单 MUST NOT 被当作出站请求的模型准入白名单：出站路径不得以「模型不在清单内」
	// 为由在本地拒绝合法模型，模型合法性由上游裁决。清单陈旧只影响展示，不得成为拒绝
	// 合法模型的理由。
	GetModelList() []string
	GetChannelName() string
}
