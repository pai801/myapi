package controller

import (
	"net/http"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"

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
			"quota_per_unit":      config.QuotaPerUnit,
			"display_in_currency": config.DisplayInCurrencyEnabled,
		},
	})
	return
}

func GetAbout(c *gin.Context) {
	config.OptionMapRWMutex.RLock()
	defer config.OptionMapRWMutex.RUnlock()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.OptionMap["About"],
	})
	return
}

func GetHomePageContent(c *gin.Context) {
	config.OptionMapRWMutex.RLock()
	defer config.OptionMapRWMutex.RUnlock()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    config.OptionMap["HomePageContent"],
	})
	return
}
