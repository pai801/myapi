package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/relay/chandesc"
)

// GetChannelDescriptors 下发渠道能力清单（PRD §5.12 层1 / 决策 D7）。
//
// 前端启动 / 进入渠道页时拉取，据此渲染渠道类型下拉、列表标签与通用面板，
// 从而不在前端硬编码任何渠道类型（描述符驱动 + 通用渲染器）。
//
// 本仓不注册任何渠道 → 注册表为空 → 返回空列表；扩展渠道的清单由扩展方在其 init()
// 中注入。data 恒为数组（空时为 []），便于前端统一处理。
func GetChannelDescriptors(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    chandesc.All(),
	})
}
