// Package controller is a package for handling the relay controller
package controller

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
)

// RelayProxyHelper is a helper function to proxy the request to the upstream service
func RelayProxyHelper(c *gin.Context, relayMode int) *relaymodel.ErrorWithStatusCode {
	meta := meta.GetByContext(c)

	adaptor := relay.GetAdaptor(meta.APIType)
	if adaptor == nil {
		logger.Log.Errorf("[%s] %+v", "invalid_api_type", fmt.Errorf("invalid api type: %d", meta.APIType))
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}
	adaptor.Init(meta)

	resp, err := adaptor.DoRequest(c, meta, c.Request.Body)
	// sticky 在 DoRequest 内部改写 meta.ChannelId；返回后记录实际服务渠道供归因使用
	recordActualChannel(c, meta)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "do_request_failed", err)
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}

	// do response
	_, respErr := adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		logger.Log.Errorf("respErr is not nil: %+v", respErr)
		return respErr
	}

	return nil
}
