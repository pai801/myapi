package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/i18n"
	"github.com/pai801/myapi/common/random"
	"github.com/pai801/myapi/model"
)

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func Login(c *gin.Context) {
	if !config.PasswordLoginEnabled {
		c.JSON(http.StatusOK, gin.H{
			"message": "管理员关闭了密码登录",
			"success": false,
		})
		return
	}
	var loginRequest LoginRequest
	err := json.NewDecoder(c.Request.Body).Decode(&loginRequest)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"message": i18n.Translate(c, "invalid_parameter"),
			"success": false,
		})
		return
	}
	username := loginRequest.Username
	password := loginRequest.Password
	if username == "" || password == "" {
		c.JSON(http.StatusOK, gin.H{
			"message": i18n.Translate(c, "invalid_parameter"),
			"success": false,
		})
		return
	}
	user := model.User{
		Username: username,
		Password: password,
	}
	err = user.ValidateAndFill()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}
	SetupLogin(&user, c)
}

// setup session & cookies and then return user info
func SetupLogin(user *model.User, c *gin.Context) {
	session := sessions.Default(c)
	session.Set("id", user.Id)
	session.Set("username", user.Username)
	session.Set("role", user.Role)
	session.Set("status", user.Status)
	err := session.Save()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"message": "无法保存会话信息，请重试",
			"success": false,
		})
		return
	}

	token, err := common.GenerateJWT(user.Id, user.Username, user.Role, user.Status)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"message": "无法生成令牌，请重试",
			"success": false,
		})
		return
	}

	c.SetCookie("session", token, config.JWTExpiresIn, "/", "", false, true)

	cleanUser := model.User{
		Id:          user.Id,
		Username:    user.Username,
		DisplayName: user.DisplayName,
		Role:        user.Role,
		Status:      user.Status,
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "",
		"success": true,
		"data":    cleanUser,
		"token":   token,
	})
}

func Logout(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	err := session.Save()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"message": err.Error(),
			"success": false,
		})
		return
	}
	c.SetCookie("session", "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{
		"message": "",
		"success": true,
	})
}

func GetAllUsers(c *gin.Context) {
	p, _ := strconv.Atoi(c.Query("p"))
	if p < 0 {
		p = 0
	}
	pageSize := config.ItemsPerPage
	startIdx := p * pageSize

	users, err := model.GetAllUsers(startIdx, pageSize)

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
		"data":    users,
	})
}

func SearchUsers(c *gin.Context) {
	keyword := c.Query("keyword")
	users, err := model.SearchUsers(keyword)
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
		"data":    users,
	})
	return
}

func GetUser(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	user, err := model.GetUserById(id, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	myRole := c.GetInt(ctxkey.Role)
	if myRole <= user.Role && myRole != model.RoleRootUser {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权获取同级或更高等级用户的信息",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    user,
	})
	return
}

func GetUserDashboard(c *gin.Context) {
	role := c.GetInt(ctxkey.Role)
	id := c.GetInt(ctxkey.Id)
	now := time.Now()

	// 解析时间范围参数，支持自定义查询区间
	startTimestamp, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	endTimestamp, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	if startTimestamp == 0 {
		startTimestamp = now.Truncate(24*time.Hour).AddDate(0, 0, -6).Unix()
	}
	if endTimestamp == 0 {
		endTimestamp = now.Truncate(24*time.Hour).Add(24*time.Hour - time.Second).Unix()
	}

	targetUsername := c.Query("username")

	// 管理员：可传 username 参数查指定用户或全部（不传=全部）
	if role >= model.RoleAdminUser {
		if targetUsername != "" {
			id = 0 // 用 username 过滤而非 userId
		} else {
			id = 0 // 查询所有用户
		}
	}

	dashboards, err := model.SearchLogsByDayAndModel(id, int(startTimestamp), int(endTimestamp), targetUsername)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无法获取统计信息",
			"data":    nil,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    dashboards,
	})
	return
}

func GenerateAccessToken(c *gin.Context) {
	id := c.GetInt(ctxkey.Id)
	user, err := model.GetUserById(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	user.AccessToken = random.GetUUID()

	if model.DB.Where("access_token = ?", user.AccessToken).First(user).RowsAffected != 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "请重试，系统生成的 UUID 竟然重复了！",
		})
		return
	}

	// 仅更新 access_token，避免把查出的旧值整行回写（并发下会覆盖 quota 等字段）
	if err := user.Update(false, "access_token"); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    user.AccessToken,
	})
	return
}

func GetSelf(c *gin.Context) {
	id := c.GetInt(ctxkey.Id)
	user, err := model.GetUserById(id, false)
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
		"data":    user,
	})
	return
}

// updateUserRequest 更新用户请求体。可变字段用指针承载：JSON 未携带该字段时为 nil，
// 该列不进入更新白名单也不做枚举校验（第三方 API 按字段缺省提交部分字段时，
// 不会被整体拒绝或缺省清空旧值），显式传值（含空串/0）才是合法的更新/清空操作。
// 外层同名字段优先于内嵌 User 同名字段参与解码（encoding/json 浅层优先），
// 命中后由控制器回填 User 对应字段。
type updateUserRequest struct {
	model.User
	Quota       *int64  `json:"quota"`
	Role        *int    `json:"role"`
	Status      *int    `json:"status"`
	DisplayName *string `json:"display_name"`
}

func UpdateUser(c *gin.Context) {
	ctx := c.Request.Context()
	var req updateUserRequest
	err := json.NewDecoder(c.Request.Body).Decode(&req)
	if err != nil || req.Id == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": i18n.Translate(c, "invalid_parameter"),
		})
		return
	}
	updatedUser := req.User
	if updatedUser.Password == "" {
		updatedUser.Password = "$I_LOVE_U" // make Validator happy :)
	}
	if err := common.Validate.Struct(&updatedUser); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": i18n.Translate(c, "invalid_input"),
		})
		return
	}
	// 显式列更新后零值会落库，负数额度必须拒绝（原先靠零值被忽略兜住）；未携带 quota 时跳过
	if req.Quota != nil && *req.Quota < 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "quota 不能为负数！",
		})
		return
	}
	// 显式列更新后非法枚举也会落库，故仅在请求显式携带 role/status 时校验枚举；
	// 未携带（nil）表示不更新该字段，跳过校验与写库（部分字段更新语义）
	if req.Status != nil {
		if *req.Status != model.UserStatusEnabled && *req.Status != model.UserStatusDisabled {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "status 非法，仅允许 1（启用）或 2（禁用）",
			})
			return
		}
		updatedUser.Status = *req.Status
	}
	if req.Role != nil {
		if *req.Role != model.RoleCommonUser && *req.Role != model.RoleAdminUser && *req.Role != model.RoleRootUser {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "role 非法，仅允许 1（普通）、10（管理）、100（超管）",
			})
			return
		}
		updatedUser.Role = *req.Role
	}
	originUser, err := model.GetUserById(updatedUser.Id, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	myRole := c.GetInt(ctxkey.Role)
	if myRole <= originUser.Role && myRole != model.RoleRootUser {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权更新同权限等级或更高权限等级的用户信息",
		})
		return
	}
	if myRole <= updatedUser.Role && myRole != model.RoleRootUser {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权将其他用户权限等级提升到大于等于自己的权限等级",
		})
		return
	}
	if updatedUser.Password == "$I_LOVE_U" {
		updatedUser.Password = "" // rollback to what it should be
	}
	updatePassword := updatedUser.Password != ""
	// 管理员合法可改字段全集（前端编辑表单回显全量提交）；email/access_token/统计列不开放。
	// 各指针字段仅在请求显式携带时进白名单，防止 JSON 缺字段缺省清空旧值
	// （显式传空串/0 仍会落库，是合法的清空/清零操作）
	columns := []string{"username"}
	if req.DisplayName != nil {
		// 指针遮蔽了 model.User.DisplayName 的 validate:"max=20"（外层指针无 tag，
		// 内嵌副本恒空串使 Struct 校验恒通过），长度须在此显式校验，与 validator 的 rune 计数一致
		if utf8.RuneCountInString(*req.DisplayName) > 20 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "display_name 非法，长度不能超过 20 个字符",
			})
			return
		}
		updatedUser.DisplayName = *req.DisplayName
		columns = append(columns, "display_name")
	}
	if req.Status != nil {
		columns = append(columns, "status")
	}
	if req.Role != nil {
		columns = append(columns, "role")
	}
	if req.Quota != nil {
		updatedUser.Quota = *req.Quota
		columns = append(columns, "quota")
	}
	if err := updatedUser.Update(updatePassword, columns...); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if req.Quota != nil && originUser.Quota != updatedUser.Quota {
		model.RecordLog(ctx, originUser.Id, model.LogTypeManage, fmt.Sprintf("管理员将用户额度从 %s修改为 %s", common.LogQuota(originUser.Quota), common.LogQuota(updatedUser.Quota)))
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}

// updateSelfRequest 自身信息更新请求体。display_name 用指针承载：JSON 未携带时为 nil，
// 该列不进更新白名单（部分字段提交不清空旧昵称），显式传空串才是合法清空；
// omitempty 让 nil 缺省跳过校验（validator 对 nil 指针不会自动跳过 max），
// max=20 补齐指针遮蔽 model.User 同名 validate tag 的缺口
type updateSelfRequest struct {
	model.User
	DisplayName *string `json:"display_name" validate:"omitempty,max=20"`
}

func UpdateSelf(c *gin.Context) {
	var req updateSelfRequest
	err := json.NewDecoder(c.Request.Body).Decode(&req)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": i18n.Translate(c, "invalid_parameter"),
		})
		return
	}
	if req.Password == "" {
		req.Password = "$I_LOVE_U" // make Validator happy :)
	}
	if err := common.Validate.Struct(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "输入不合法 " + err.Error(),
		})
		return
	}

	cleanUser := model.User{
		Id:       c.GetInt(ctxkey.Id),
		Username: req.Username,
		Password: req.Password,
	}
	if req.Password == "$I_LOVE_U" {
		req.Password = "" // rollback to what it should be
		cleanUser.Password = ""
	}
	updatePassword := req.Password != ""
	// 白名单刻意不含 role/quota/status 等敏感列：UpdateSelf 不得借本路径改动它们；
	// display_name 仅在请求显式携带时进入白名单，缺省不清空旧值
	columns := []string{"username"}
	if req.DisplayName != nil {
		cleanUser.DisplayName = *req.DisplayName
		columns = append(columns, "display_name")
	}
	if err := cleanUser.Update(updatePassword, columns...); err != nil {
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
	return
}

func DeleteUser(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	originUser, err := model.GetUserById(id, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	myRole := c.GetInt("role")
	if myRole <= originUser.Role {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权删除同权限等级或更高权限等级的用户",
		})
		return
	}
	err = model.DeleteUserById(id)
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
	})
	return
}

func CreateUser(c *gin.Context) {
	ctx := c.Request.Context()
	var user model.User
	err := json.NewDecoder(c.Request.Body).Decode(&user)
	if err != nil || user.Username == "" || user.Password == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": i18n.Translate(c, "invalid_parameter"),
		})
		return
	}
	if err := common.Validate.Struct(&user); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": i18n.Translate(c, "invalid_input"),
		})
		return
	}
	if user.DisplayName == "" {
		user.DisplayName = user.Username
	}
	myRole := c.GetInt("role")
	if user.Role >= myRole {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无法创建权限大于等于自己的用户",
		})
		return
	}
	// Even for admin users, we cannot fully trust them!
	cleanUser := model.User{
		Username:    user.Username,
		Password:    user.Password,
		DisplayName: user.DisplayName,
	}
	if err := cleanUser.Insert(ctx); err != nil {
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
	return
}

type ManageRequest struct {
	Username string `json:"username"`
	Action   string `json:"action"`
}

// ManageUser Only admin user can do this
func ManageUser(c *gin.Context) {
	var req ManageRequest
	err := json.NewDecoder(c.Request.Body).Decode(&req)

	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": i18n.Translate(c, "invalid_parameter"),
		})
		return
	}
	user := model.User{
		Username: req.Username,
	}
	// Fill attributes
	model.DB.Where(&user).First(&user)
	if user.Id == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "用户不存在",
		})
		return
	}
	myRole := c.GetInt("role")
	if myRole <= user.Role && myRole != model.RoleRootUser {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无权更新同权限等级或更高权限等级的用户信息",
		})
		return
	}
	switch req.Action {
	case "disable":
		user.Status = model.UserStatusDisabled
		if user.Role == model.RoleRootUser {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法禁用超级管理员用户",
			})
			return
		}
	case "enable":
		user.Status = model.UserStatusEnabled
	}

	if err := user.Update(false, "status"); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	clearUser := model.User{
		Role:   user.Role,
		Status: user.Status,
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    clearUser,
	})
	return
}

