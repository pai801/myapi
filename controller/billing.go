package controller

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
)

func GetSubscription(c *gin.Context) {
	userId := c.GetInt(ctxkey.Id)
	userQuota, err := model.GetUserQuota(userId)
	if err != nil {
		logger.Log.Errorf("GetUserQuota failed for user %d: %v", userId, err)
		userQuota = 0
	}
	// Convert: original ratio was 500,000 quota units per 1 USD
	softLimitUSD := float64(userQuota) / 500000.0

	subscription := OpenAISubscriptionResponse{
		Object:             "billing_subscription",
		HasPaymentMethod:   true,
		SoftLimitUSD:       softLimitUSD,
		HardLimitUSD:       softLimitUSD,
		SystemHardLimitUSD: softLimitUSD,
		AccessUntil:        time.Now().Unix() + 86400*365, // 1 year from now
	}
	c.JSON(200, subscription)
}

func GetUsage(c *gin.Context) {
	userId := c.GetInt(ctxkey.Id)
	usedQuota, err := model.GetUserUsedQuota(userId)
	if err != nil {
		logger.Log.Errorf("GetUserUsedQuota failed for user %d: %v", userId, err)
		usedQuota = 0
	}
	usage := OpenAIUsageResponse{
		Object:     "list",
		TotalUsage: float64(usedQuota) / 5000.0,
	}
	c.JSON(200, usage)
}
