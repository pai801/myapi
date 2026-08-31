package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/model"
	. "github.com/smartystreets/goconvey/convey"
)

// initUpdateUserTestDB 走生产 InitDB 路径初始化临时 SQLite 库；禁用 Redis/批量更新，
// 使 User.Update 的额度写直落 DB、缓存函数退化为直查。
func initUpdateUserTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	common.RedisEnabled = false
	common.UsingSQLite = true
	common.SQLitePath = filepath.Join(t.TempDir(), "myapi-test.db")
	model.InitDB()
	model.InitLogDB()
}

type updateUserResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func TestUpdateUserQuotaPointerSemantics(t *testing.T) {
	Convey("UpdateUser 的 quota 指针语义：缺字段不动额度，显式传值（含 0）才落库", t, func() {
		initUpdateUserTestDB(t)

		// Insert 强制 quota=0，用独立的增量路径播种非零额度作为基线
		target := &model.User{Username: "targetuser", DisplayName: "dn", Status: model.UserStatusEnabled}
		So(target.Insert(context.Background()), ShouldBeNil)
		So(model.IncreaseUserQuota(target.Id, 5000), ShouldBeNil)

		getQuota := func() int64 {
			u, err := model.GetUserById(target.Id, false)
			So(err, ShouldBeNil)
			return u.Quota
		}
		// quotaPtr 为 nil 时借助指针 omitempty 产出真正缺失 quota 字段的 JSON
		doUpdate := func(quotaPtr *int) updateUserResponse {
			body := struct {
				Id          int    `json:"id"`
				Username    string `json:"username"`
				DisplayName string `json:"display_name"`
				Role        int    `json:"role"`
				Status      int    `json:"status"`
				Quota       *int   `json:"quota,omitempty"`
			}{Id: target.Id, Username: "targetuser", DisplayName: "dn", Role: model.RoleCommonUser, Status: model.UserStatusEnabled, Quota: quotaPtr}
			raw, err := json.Marshal(body)
			So(err, ShouldBeNil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/", bytes.NewReader(raw))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(ctxkey.Role, model.RoleRootUser)

			UpdateUser(c)

			So(w.Code, ShouldEqual, http.StatusOK)
			var resp updateUserResponse
			So(json.Unmarshal(w.Body.Bytes(), &resp), ShouldBeNil)
			return resp
		}

		Convey("显式传 quota=0 落库清零", func() {
			zero := 0
			So(doUpdate(&zero).Success, ShouldBeTrue)
			So(getQuota(), ShouldEqual, 0)
		})

		Convey("JSON 缺失 quota 字段不更新额度", func() {
			So(doUpdate(nil).Success, ShouldBeTrue)
			So(getQuota(), ShouldEqual, 5000)
		})

		Convey("显式传负数 quota 被拒绝且额度不变", func() {
			negative := -1
			resp := doUpdate(&negative)
			So(resp.Success, ShouldBeFalse)
			So(getQuota(), ShouldEqual, 5000)
		})
	})
}

// TestUpdateUserPartialUpdateSemantics 验证 M2 修复：API 直连客户端只提交部分字段时
// 请求不被整体拒绝、未提交字段不清空（指针缺省语义），显式传非法枚举仍被拒绝
func TestUpdateUserPartialUpdateSemantics(t *testing.T) {
	Convey("UpdateUser 部分字段更新：缺省字段跳过校验与写库，显式传值才更新", t, func() {
		initUpdateUserTestDB(t)

		target := &model.User{Username: "partialuser", DisplayName: "keep-dn", Status: model.UserStatusEnabled}
		So(target.Insert(context.Background()), ShouldBeNil)
		So(model.IncreaseUserQuota(target.Id, 5000), ShouldBeNil)

		// 指针字段 + omitempty 产出真正缺失字段的 JSON
		doPartialUpdate := func(body map[string]interface{}) updateUserResponse {
			raw, err := json.Marshal(body)
			So(err, ShouldBeNil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/", bytes.NewReader(raw))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(ctxkey.Role, model.RoleRootUser)

			UpdateUser(c)

			So(w.Code, ShouldEqual, http.StatusOK)
			var resp updateUserResponse
			So(json.Unmarshal(w.Body.Bytes(), &resp), ShouldBeNil)
			return resp
		}
		getUser := func() *model.User {
			u, err := model.GetUserById(target.Id, false)
			So(err, ShouldBeNil)
			return u
		}

		Convey("仅提交 id+quota：其余字段不被清空、不被校验拒绝", func() {
			So(doPartialUpdate(map[string]interface{}{"id": target.Id, "quota": 100}).Success, ShouldBeTrue)

			u := getUser()
			So(u.Quota, ShouldEqual, 100)
			So(u.DisplayName, ShouldEqual, "keep-dn")
			// User.Role 列有 gorm default:1，新建用户基线即普通用户
			So(u.Role, ShouldEqual, model.RoleCommonUser)
			So(u.Status, ShouldEqual, model.UserStatusEnabled)
		})

		Convey("仅提交 id+display_name：额度与其他字段保持不变", func() {
			So(doPartialUpdate(map[string]interface{}{"id": target.Id, "display_name": "new-dn"}).Success, ShouldBeTrue)

			u := getUser()
			So(u.DisplayName, ShouldEqual, "new-dn")
			So(u.Quota, ShouldEqual, 5000)
			So(u.Status, ShouldEqual, model.UserStatusEnabled)
		})

		Convey("显式传非法枚举仍被整体拒绝且字段不变", func() {
			So(doPartialUpdate(map[string]interface{}{"id": target.Id, "role": 5}).Success, ShouldBeFalse)

			u := getUser()
			So(u.Role, ShouldEqual, model.RoleCommonUser)
			So(u.Quota, ShouldEqual, 5000)
		})

		Convey("显式传空串 display_name 是合法清空", func() {
			So(doPartialUpdate(map[string]interface{}{"id": target.Id, "display_name": ""}).Success, ShouldBeTrue)

			So(getUser().DisplayName, ShouldEqual, "")
		})

		Convey("显式传超长 display_name（>20 rune）被拒绝且原值不变", func() {
			// 指针遮蔽 model.User.DisplayName 的 validate:"max=20"，控制器须显式校验
			So(doPartialUpdate(map[string]interface{}{"id": target.Id, "display_name": strings.Repeat("字", 21)}).Success, ShouldBeFalse)

			So(getUser().DisplayName, ShouldEqual, "keep-dn")
		})

		Convey("恰好 20 rune 的 display_name 合法落库", func() {
			boundary := strings.Repeat("字", 20)
			So(doPartialUpdate(map[string]interface{}{"id": target.Id, "display_name": boundary}).Success, ShouldBeTrue)

			So(getUser().DisplayName, ShouldEqual, boundary)
		})
	})
}

// TestUpdateSelfPartialDisplayNameSemantics 验证 UpdateSelf 的 display_name 指针语义：
// JSON 缺省不清空旧昵称（A4）、显式传值才更新、超长经 validate:"max=20" 拒绝
func TestUpdateSelfPartialDisplayNameSemantics(t *testing.T) {
	Convey("UpdateSelf 的 display_name 指针语义", t, func() {
		initUpdateUserTestDB(t)

		target := &model.User{Username: "selfuser", DisplayName: "old-self-dn", Status: model.UserStatusEnabled}
		So(target.Insert(context.Background()), ShouldBeNil)

		doUpdateSelf := func(body map[string]interface{}) updateUserResponse {
			raw, err := json.Marshal(body)
			So(err, ShouldBeNil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/self", bytes.NewReader(raw))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(ctxkey.Id, target.Id)

			UpdateSelf(c)

			So(w.Code, ShouldEqual, http.StatusOK)
			var resp updateUserResponse
			So(json.Unmarshal(w.Body.Bytes(), &resp), ShouldBeNil)
			return resp
		}
		getUser := func() *model.User {
			u, err := model.GetUserById(target.Id, false)
			So(err, ShouldBeNil)
			return u
		}

		Convey("JSON 缺省 display_name 不清空旧值", func() {
			So(doUpdateSelf(map[string]interface{}{"username": "selfuser"}).Success, ShouldBeTrue)

			So(getUser().DisplayName, ShouldEqual, "old-self-dn")
		})

		Convey("显式传 display_name 更新昵称", func() {
			So(doUpdateSelf(map[string]interface{}{"username": "selfuser", "display_name": "new-self-dn"}).Success, ShouldBeTrue)

			So(getUser().DisplayName, ShouldEqual, "new-self-dn")
		})

		Convey("超长 display_name（>20 rune）被拒绝且原值不变", func() {
			So(doUpdateSelf(map[string]interface{}{"username": "selfuser", "display_name": strings.Repeat("字", 21)}).Success, ShouldBeFalse)

			So(getUser().DisplayName, ShouldEqual, "old-self-dn")
		})
	})
}
