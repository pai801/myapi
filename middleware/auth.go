package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/blacklist"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/network"
	"github.com/songquanpeng/one-api/model"
)

func authHelper(c *gin.Context, minRole int) {
	var username interface{}
	var roleVal interface{}
	var idVal interface{}
	var statusVal interface{}
	authenticated := false
	fromSession := false

	// 1. Try JWT from cookie (stateless)
	if jwtCookie, err := c.Cookie("session"); err == nil && jwtCookie != "" {
		claims, parseErr := common.ParseJWT(jwtCookie)
		if parseErr == nil {
			username = claims.Username
			roleVal = claims.Role
			idVal = claims.UserId
			statusVal = claims.Status
			authenticated = true
		}
	}

	// 2. Try JWT from Authorization header
	if !authenticated {
		authHeader := c.Request.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			claims, parseErr := common.ParseJWT(tokenStr)
			if parseErr == nil {
				username = claims.Username
				roleVal = claims.Role
				idVal = claims.UserId
				statusVal = claims.Status
				authenticated = true
			}
		}
	}

	// 3. Fallback to session (backward compatibility)
	if !authenticated {
		sess := sessions.Default(c)
		username = sess.Get("username")
		roleVal = sess.Get("role")
		idVal = sess.Get("id")
		statusVal = sess.Get("status")
		if username != nil {
			authenticated = true
			fromSession = true
		}
	}

	// 4. Fallback to access token
	if !authenticated {
		accessToken := c.Request.Header.Get("Authorization")
		if accessToken != "" {
			user := model.ValidateAccessToken(accessToken)
			if user != nil && user.Username != "" {
				username = user.Username
				roleVal = user.Role
				idVal = user.Id
				statusVal = user.Status
				authenticated = true
			}
		}
	}

	if !authenticated {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "无权进行此操作，未登录且未提供有效凭证",
		})
		c.Abort()
		return
	}

	statusInt, statusOk := statusVal.(int)
	idInt, idOk := idVal.(int)
	if !statusOk || !idOk || statusInt == model.UserStatusDisabled || blacklist.IsUserBanned(idInt) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "用户已被封禁",
		})
		if fromSession {
			sess := sessions.Default(c)
			sess.Clear()
			_ = sess.Save()
		}
		c.Abort()
		return
	}

	roleInt, roleOk := roleVal.(int)
	if !roleOk || roleInt < minRole {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权进行此操作，权限不足",
		})
		c.Abort()
		return
	}

	c.Set("username", username)
	c.Set("role", roleVal)
	c.Set("id", idVal)
	c.Next()
}

func UserAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, model.RoleCommonUser)
	}
}

func AdminAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, model.RoleAdminUser)
	}
}

func RootAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		authHelper(c, model.RoleRootUser)
	}
}

func TokenAuth() func(c *gin.Context) {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		key := c.Request.Header.Get("Authorization")
		key = strings.TrimPrefix(key, "Bearer ")
		key = strings.TrimPrefix(key, "sk-")
		parts := strings.Split(key, "-")
		key = parts[0]
		token, err := model.ValidateUserToken(key)
		if err != nil {
			abortWithMessage(c, http.StatusUnauthorized, err.Error())
			return
		}
		if token.Subnet != nil && *token.Subnet != "" {
			if !network.IsIpInSubnets(ctx, c.ClientIP(), *token.Subnet) {
				abortWithMessage(c, http.StatusForbidden, fmt.Sprintf("该令牌只能在指定网段使用：%s，当前 ip：%s", *token.Subnet, c.ClientIP()))
				return
			}
		}
		userEnabled, err := model.CacheIsUserEnabled(token.UserId)
		if err != nil {
			abortWithMessage(c, http.StatusInternalServerError, err.Error())
			return
		}
		if !userEnabled || blacklist.IsUserBanned(token.UserId) {
			abortWithMessage(c, http.StatusForbidden, "用户已被封禁")
			return
		}
		requestModel, err := getRequestModel(c)
		if err != nil && shouldCheckModel(c) {
			abortWithMessage(c, http.StatusBadRequest, err.Error())
			return
		}
		c.Set(ctxkey.RequestModel, requestModel)
		if token.Models != nil && *token.Models != "" {
			c.Set(ctxkey.AvailableModels, *token.Models)
			if requestModel != "" && requestModel != "auto" && !isModelInList(requestModel, *token.Models) {
				abortWithMessage(c, http.StatusForbidden, fmt.Sprintf("该令牌无权使用模型：%s", requestModel))
				return
			}
		}
		c.Set(ctxkey.Id, token.UserId)
		c.Set(ctxkey.TokenId, token.Id)
		c.Set(ctxkey.TokenName, token.Name)
		if len(parts) > 1 {
			if model.IsAdmin(token.UserId) {
				c.Set(ctxkey.SpecificChannelId, parts[1])
			} else {
				abortWithMessage(c, http.StatusForbidden, "普通用户不支持指定渠道")
				return
			}
		}

		// set channel id for proxy relay
		if channelId := c.Param("channelid"); channelId != "" {
			c.Set(ctxkey.SpecificChannelId, channelId)
		}

		c.Next()
	}
}

func shouldCheckModel(c *gin.Context) bool {
	if strings.HasPrefix(c.Request.URL.Path, "/v1/completions") {
		return true
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/chat/completions") {
		return true
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/images") {
		return true
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/audio") {
		return true
	}
	return false
}
