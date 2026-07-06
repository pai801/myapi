package controller

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"gorm.io/gorm/clause"
)

var (
	groupCache     []string
	groupCacheTime time.Time
	groupCacheMu   sync.RWMutex
)

func GetGroups(c *gin.Context) {
	groupCacheMu.RLock()
	if time.Since(groupCacheTime) < 60*time.Second && groupCache != nil {
		cached := groupCache
		groupCacheMu.RUnlock()
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data":    cached,
		})
		return
	}
	groupCacheMu.RUnlock()

	groupCacheMu.Lock()
	defer groupCacheMu.Unlock()
	// Double-check after acquiring write lock
	if time.Since(groupCacheTime) < 60*time.Second && groupCache != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data":    groupCache,
		})
		return
	}

	groupNames := make([]string, 0)
	err := model.DB.Model(&model.Channel{}).Distinct(clause.Column{Name: "group"}).Select(clause.Column{Name: "group"}).Find(&groupNames).Error
	if err != nil {
		logger.Log.Errorf("Pluck group failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "Failed to get groups",
		})
		return
	}
	groupCache = groupNames
	groupCacheTime = time.Now()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    groupNames,
	})
}
