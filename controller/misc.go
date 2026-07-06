package controller

import (
	"net/http"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"

	"github.com/gin-gonic/gin"
)

func GetStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"version":             common.Version,
			"start_time":          common.StartTime,
			"system_name":         config.SystemName,
			"logo":                config.Logo,
			"footer_html":         config.Footer,
			"server_address":      config.ServerAddress,
			"turnstile_check":     config.TurnstileCheckEnabled,
			"turnstile_site_key":  config.TurnstileSiteKey,
		},
	})
  return
}
