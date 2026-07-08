package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
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
	err := model.DB.Model(&model.Group{}).Distinct("name").Pluck("name", &groupNames).Error
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

func GetGroupList(c *gin.Context) {
	p, _ := strconv.Atoi(c.Query("p"))
	if p < 0 {
		p = 0
	}
	pageSize := config.ItemsPerPage
	groups, total, err := model.GetGroupList(p, pageSize)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"items": groups,
			"total": total,
		},
	})
}

func GetGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的分组 ID",
		})
		return
	}
	group, err := model.GetGroupById(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    group,
	})
}

type GroupRequest struct {
	Name       string   `json:"name"`
	ModelRatio *float64 `json:"model_ratio"`
}

func AddGroup(c *gin.Context) {
	var req GroupRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "请求体解析失败",
		})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "分组名称不能为空",
		})
		return
	}
	if req.Name == "default" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "default 分组已存在，不可创建",
		})
		return
	}
	existing, err := model.GetGroupByName(req.Name)
	if err == nil && existing != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "分组名称已存在",
		})
		return
	}
	ratio := 1.0
	if req.ModelRatio != nil {
		ratio = *req.ModelRatio
	}
	group := &model.Group{
		Name:       req.Name,
		ModelRatio: ratio,
	}
	if err := model.AddGroup(group); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    group,
	})
}

func UpdateGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的分组 ID",
		})
		return
	}
	existing, err := model.GetGroupById(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	var req GroupRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "请求体解析失败",
		})
		return
	}
	if strings.TrimSpace(req.Name) != "" && req.Name != existing.Name {
		dup, err := model.GetGroupByName(strings.TrimSpace(req.Name))
		if err == nil && dup != nil && dup.Id != id {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "分组名称已存在",
			})
			return
		}
		existing.Name = strings.TrimSpace(req.Name)
	}
	if req.ModelRatio != nil {
		existing.ModelRatio = *req.ModelRatio
	}
	if err := model.UpdateGroup(existing); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    existing,
	})
}

func DeleteGroup(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的分组 ID",
		})
		return
	}
	existing, err := model.GetGroupById(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if existing.Name == "default" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "禁止删除 default 分组",
		})
		return
	}
	// 检查是否仍有 Token 引用
	tokenGroupCol := "`group`"
	if common.UsingPostgreSQL {
		tokenGroupCol = `"group"`
	}
	var tokenCount int64
	model.DB.Model(&model.Token{}).Where(tokenGroupCol+" = ?", existing.Name).Count(&tokenCount)
	if tokenCount > 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "该分组下仍有 Token 引用，无法删除",
		})
		return
	}
	// 检查是否仍有 Channel 引用（Channel.Group 为逗号分隔的白名单）
	groupCol := "`group`"
	if common.UsingPostgreSQL {
		groupCol = `"group"`
	}
	name := existing.Name
	var channels []*model.Channel
	model.DB.Where(
		groupCol+" = ? OR "+groupCol+" LIKE ? OR "+groupCol+" LIKE ? OR "+groupCol+" LIKE ?",
		name, name+",%", "%,"+name+",%", "%,"+name,
	).Find(&channels)
	for _, ch := range channels {
		for _, g := range strings.Split(ch.Group, ",") {
			if strings.TrimSpace(g) == name {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": "该分组下仍有 Channel 引用，无法删除",
				})
				return
			}
		}
	}
	if err := model.DeleteGroup(id); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}
