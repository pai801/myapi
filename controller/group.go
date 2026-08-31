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

// escapeLike 对 LIKE 模式中的 \ % _ 前置 \ 转义，配合 ESCAPE '\' 子句使用，
// 避免组名含通配符时被 SQL LIKE 展开；必须先转义 \ 自身，否则后续追加的 \ 会被二次转义
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
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
		// 拒绝式重命名校验（参照 DeleteGroup）：token/channel 按名称引用分组，
		// 重命名会导致路由查不到该组、计费倍率回退 1.0；级联更新需同步 abilities 表风险大，
		// 故存在引用时直接拒绝（无 channel 引用即无该组 abilities，token 引用不产生 abilities）
		oldName := existing.Name
		newName := strings.TrimSpace(req.Name)
		groupCol := "`group`"
		if common.UsingPostgreSQL {
			groupCol = `"group"`
		}
		// 仅去空白（newName == oldName）不是真实重命名，不触发引用校验，保持原有放行行为
		if newName != oldName {
			// 引用检查 fail-closed：查询出错时拒绝重命名，而非放行导致引用悬空
			var tokenCount int64
			if err := model.DB.Model(&model.Token{}).Where(groupCol+" = ?", oldName).Count(&tokenCount).Error; err != nil {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": err.Error(),
				})
				return
			}
			if tokenCount > 0 {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": "该分组仍被 Token 引用，无法重命名",
				})
				return
			}
			var channels []*model.Channel
			// LIKE 模式中的 oldName 须转义并以 ESCAPE '\' 声明转义符，防止 % _ 被当通配符多查候选渠道
			escapedName := escapeLike(oldName)
			if err := model.DB.Where(
				groupCol+" = ? OR "+groupCol+" LIKE ? ESCAPE '\\' OR "+groupCol+" LIKE ? ESCAPE '\\' OR "+groupCol+" LIKE ? ESCAPE '\\'",
				oldName, escapedName+",%", "%,"+escapedName+",%", "%,"+escapedName,
			).Find(&channels).Error; err != nil {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": err.Error(),
				})
				return
			}
			for _, ch := range channels {
				for _, g := range strings.Split(ch.Group, ",") {
					if strings.TrimSpace(g) == oldName {
						c.JSON(http.StatusOK, gin.H{
							"success": false,
							"message": "该分组仍被 Channel 引用，无法重命名",
						})
						return
					}
				}
			}
		}
		existing.Name = newName
	}
	if req.ModelRatio != nil {
		existing.ModelRatio = *req.ModelRatio
	}
	// TOCTOU 说明：上面的引用检查与本处的 UpdateGroup 落库之间存在检查-使用间隙，
	// 并发新建的 token/channel 仍可引用旧组名，重命名后形成悬空引用
	// （该组路由查不到、计费倍率回退 1.0）。接受理由：分组重命名是管理端低频人工操作，
	// 引入事务化级联更新（token+channel+abilities）的复杂度与回归风险远大于该窗口的残余风险。
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
	// LIKE 模式中的 name 须转义并以 ESCAPE '\' 声明转义符，防止 % _ 被当通配符多查候选渠道
	escapedName := escapeLike(name)
	model.DB.Where(
		groupCol+" = ? OR "+groupCol+" LIKE ? ESCAPE '\\' OR "+groupCol+" LIKE ? ESCAPE '\\' OR "+groupCol+" LIKE ? ESCAPE '\\'",
		name, escapedName+",%", "%,"+escapedName+",%", "%,"+escapedName,
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
