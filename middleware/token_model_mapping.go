package middleware

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
)

// TokenModelMapping 根据 token 的 ModelMapping 配置，把 ctxkey.RequestModel 映射为目标模型名
// 只修改 ctxkey.RequestModel，不修改原始请求体
// token 的 ModelMapping 字符串由 TokenAuth 中间件预先解析并放入 context
// 后续的 Distribute 中间件会基于映射后的 RequestModel 选择渠道
// 后续渠道级的模型映射依然按原有逻辑执行
func TokenModelMapping() func(c *gin.Context) {
	return func(c *gin.Context) {
		modelMappingStr, exists := c.Get(ctxkey.TokenModelMapping)
		if !exists {
			c.Next()
			return
		}
		mappingStr, ok := modelMappingStr.(string)
		if !ok || mappingStr == "" || mappingStr == "{}" {
			c.Next()
			return
		}
		modelMapping := make(map[string]string)
		err := json.Unmarshal([]byte(mappingStr), &modelMapping)
		if err != nil {
			logger.Log.Errorf("failed to unmarshal token model mapping: %s", err.Error())
			c.Next()
			return
		}
		requestModel := c.GetString(ctxkey.RequestModel)
		if requestModel == "" || requestModel == "auto" {
			c.Next()
			return
		}
		if mapped, ok := modelMapping[requestModel]; ok && mapped != "" {
			c.Set(ctxkey.RequestModel, mapped)
		}
		c.Next()
	}
}
