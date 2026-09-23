package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/model"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// legacyDashboardResponse 对应 GET /api/user/dashboard 的响应信封。
type legacyDashboardResponse struct {
	Success bool                  `json:"success"`
	Message string                `json:"message"`
	Data    []*model.LogStatistic `json:"data"`
}

// initLegacyDashboardTestDB 用内存 SQLite 独立初始化日志库与元数据库，
// 并返回恢复函数，避免污染包内其他测试。
func initLegacyDashboardTestDB(t *testing.T) func() {
	t.Helper()

	origDB := model.DB
	origLogDB := model.LOG_DB
	origUsingSQLite := common.UsingSQLite
	origRedisEnabled := common.RedisEnabled

	common.UsingSQLite = true
	common.RedisEnabled = false

	var err error
	model.LOG_DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory log db: %v", err)
	}
	if err := model.LOG_DB.AutoMigrate(&model.Log{}); err != nil {
		t.Fatalf("failed to migrate logs: %v", err)
	}

	// 元数据走全局 DB，空表使 alias 为空，锁定"仅小写化"的基线
	dbPath := filepath.Join(t.TempDir(), "legacy_dashboard_test.db")
	model.DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open metadata db: %v", err)
	}
	if err := model.DB.AutoMigrate(&model.ModelMetadata{}); err != nil {
		t.Fatalf("failed to migrate model metadata: %v", err)
	}

	return func() {
		model.DB = origDB
		model.LOG_DB = origLogDB
		common.UsingSQLite = origUsingSQLite
		common.RedisEnabled = origRedisEnabled
	}
}

// aggregateDashboardResponse 对应 GET /api/user/dashboard/aggregate 的响应信封。
type aggregateDashboardResponse struct {
	Success bool                        `json:"success"`
	Message string                      `json:"message"`
	Data    []*model.DashboardAggregate `json:"data"`
}

// callDashboardAggregate 以普通用户身份请求 aggregate handler，返回原始 recorder 与解析后的信封。
func callDashboardAggregate(query string) (*httptest.ResponseRecorder, aggregateDashboardResponse) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/user/dashboard/aggregate"+query, nil)
	c.Set(ctxkey.Role, model.RoleCommonUser)
	c.Set(ctxkey.Id, 1)

	GetUserDashboardAggregate(c)

	var resp aggregateDashboardResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// callDashboardAggregateAs 以指定角色与认证用户 ID 请求 aggregate handler，
// 返回原始 recorder 与解析后的信封，用于权限 scope 场景。
func callDashboardAggregateAs(role, id int, query string) (*httptest.ResponseRecorder, aggregateDashboardResponse) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/user/dashboard/aggregate"+query, nil)
	c.Set(ctxkey.Role, role)
	c.Set(ctxkey.Id, id)

	GetUserDashboardAggregate(c)

	var resp aggregateDashboardResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// summaryDashboardResponse 对应 GET /api/user/dashboard/summary 的响应信封。
type summaryDashboardResponse struct {
	Success bool                    `json:"success"`
	Message string                  `json:"message"`
	Data    *model.DashboardSummary `json:"data"`
}

// callDashboardSummaryAs 以指定角色与认证用户 ID 请求 summary handler，
// 返回原始 recorder 与解析后的信封，用于权限 scope 场景。
func callDashboardSummaryAs(role, id int, query string) (*httptest.ResponseRecorder, summaryDashboardResponse) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/user/dashboard/summary"+query, nil)
	c.Set(ctxkey.Role, role)
	c.Set(ctxkey.Id, id)

	GetUserDashboardSummary(c)

	var resp summaryDashboardResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestGetUserDashboardSummaryScope 锁定 summary handler 的权限 scope 边界：
// 普通用户一律强制限定为认证用户 ID（请求中的 username 被忽略），仅汇总本人数据；
// 管理员 userID=0，username 非空时精确过滤该用户、为空时覆盖全部用户；
// 非法参数必须在触库前返回 HTTP 400（测试期间把 LOG_DB 置 nil，一旦触库即 panic）。
func TestGetUserDashboardSummaryScope(t *testing.T) {
	restore := initLegacyDashboardTestDB(t)
	defer restore()

	day := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()

	seed := []*model.Log{
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o", PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5, Quota: 10, CreatedAt: day},
		{UserId: 2, Username: "u2", Type: model.LogTypeConsume, ModelName: "gpt-4o-mini", PromptTokens: 30, CompletionTokens: 40, CachedTokens: 15, Quota: 20, CreatedAt: day},
	}
	for _, l := range seed {
		if err := model.LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}

	start := strconv.FormatInt(day-3600, 10)
	end := strconv.FormatInt(day+3600, 10)

	Convey("Invalid parameters are HTTP 400 before any query", t, func() {
		// 触库即 panic：用于证明非法输入未调用 model 查询
		origLogDB := model.LOG_DB
		model.LOG_DB = nil
		defer func() { model.LOG_DB = origLogDB }()

		for _, query := range []string{
			"?granularity=minute&start_timestamp=" + start + "&end_timestamp=" + end,
			"?start_timestamp=" + start + "&end_timestamp=" + end,
			"?granularity=day&start_timestamp=abc&end_timestamp=" + end,
			"?granularity=day&start_timestamp=" + end + "&end_timestamp=" + start,
		} {
			w, resp := callDashboardSummaryAs(model.RoleCommonUser, 1, query)
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			So(resp.Success, ShouldBeFalse)
		}
	})

	Convey("Common user scope is forced to the authenticated user ID", t, func() {
		// username=u2 被忽略：仍只汇总本人（u1）的数据
		query := "?granularity=day&username=u2&start_timestamp=" + start + "&end_timestamp=" + end
		w, resp := callDashboardSummaryAs(model.RoleCommonUser, 1, query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		So(resp.Message, ShouldEqual, "")
		So(resp.Data, ShouldNotBeNil)
		So(resp.Data.TotalRequests, ShouldEqual, 1)
		So(resp.Data.TotalQuota, ShouldEqual, 10)
	})

	Convey("Administrator scope follows the exact-username/all-users contract", t, func() {
		Convey("empty username covers all users", func() {
			query := "?granularity=day&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardSummaryAs(model.RoleAdminUser, 2, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			So(resp.Data, ShouldNotBeNil)
			So(resp.Data.TotalRequests, ShouldEqual, 2)
			So(resp.Data.TotalQuota, ShouldEqual, 30)
		})

		Convey("non-empty username filters exactly that user", func() {
			query := "?granularity=day&username=u2&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardSummaryAs(model.RoleAdminUser, 1, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			So(resp.Data, ShouldNotBeNil)
			So(resp.Data.TotalRequests, ShouldEqual, 1)
			So(resp.Data.TotalQuota, ShouldEqual, 20)
		})
	})
}

// TestGetUserDashboardSummaryNullLatency 锁定 summary HTTP 响应中延迟字段的「无样本」语义：
// 区间内消费日志存在但无正延迟样本时，avg_ttft/avg_elapsed 必须是 JSON null（前端据此渲染
// "--"），而 total_requests/total_quota/total_tokens/cached_tokens 的数值零仍是有效值，
// 必须继续为 0 而非 null；有正样本时延迟返回正确均值。success=true 且信封不变。
func TestGetUserDashboardSummaryNullLatency(t *testing.T) {
	restore := initLegacyDashboardTestDB(t)
	defer restore()

	day := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC).Unix()

	seed := []*model.Log{
		// 无正延迟样本：first_token_time/elapsed_time 均为 0
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o",
			PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5, Quota: 10, CreatedAt: day},
		// 有正延迟样本
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o",
			PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5, Quota: 10, CreatedAt: day2,
			FirstTokenTime: 200, ElapsedTime: 1000},
	}
	for _, l := range seed {
		if err := model.LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}

	Convey("Summary latency is JSON null when no positive samples exist", t, func() {
		// 只覆盖 day（无正样本），day2 的正样本不落入区间
		start := strconv.FormatInt(day-3600, 10)
		end := strconv.FormatInt(day+3600, 10)
		query := "?granularity=day&start_timestamp=" + start + "&end_timestamp=" + end

		w, resp := callDashboardSummaryAs(model.RoleCommonUser, 1, query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		So(resp.Message, ShouldEqual, "")
		So(resp.Data, ShouldNotBeNil)
		So(resp.Data.AvgTTFT, ShouldBeNil)
		So(resp.Data.AvgElapsed, ShouldBeNil)

		// 原始 JSON 必须是 null，前端 isDisplayable 才会渲染 "--"
		body := w.Body.String()
		So(strings.Contains(body, `"avg_ttft":null`), ShouldBeTrue)
		So(strings.Contains(body, `"avg_elapsed":null`), ShouldBeTrue)

		// total_* 的数值零仍是有效值，必须继续为 0
		So(resp.Data.TotalRequests, ShouldEqual, 1)
		So(resp.Data.TotalQuota, ShouldEqual, 10)
		So(resp.Data.TotalTokens, ShouldEqual, 30)
		So(resp.Data.CachedTokens, ShouldEqual, 5)
	})

	Convey("Summary latency stays numeric when positive samples exist", t, func() {
		start := strconv.FormatInt(day2-3600, 10)
		end := strconv.FormatInt(day2+3600, 10)
		query := "?granularity=day&start_timestamp=" + start + "&end_timestamp=" + end

		w, resp := callDashboardSummaryAs(model.RoleCommonUser, 1, query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		So(resp.Data, ShouldNotBeNil)
		So(resp.Data.AvgTTFT, ShouldNotBeNil)
		So(*resp.Data.AvgTTFT, ShouldEqual, 200)
		So(resp.Data.AvgElapsed, ShouldNotBeNil)
		So(*resp.Data.AvgElapsed, ShouldEqual, 1000)
	})

	Convey("Empty range yields null latency with numeric zero totals", t, func() {
		emptyDay := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		start := strconv.FormatInt(emptyDay, 10)
		end := strconv.FormatInt(emptyDay+3599, 10)
		query := "?granularity=day&start_timestamp=" + start + "&end_timestamp=" + end

		w, resp := callDashboardSummaryAs(model.RoleCommonUser, 1, query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		So(resp.Data, ShouldNotBeNil)
		So(resp.Data.AvgTTFT, ShouldBeNil)
		So(resp.Data.AvgElapsed, ShouldBeNil)
		// 数值零仍是有效样本：total_* 保持 0
		So(resp.Data.TotalRequests, ShouldEqual, 0)
		So(resp.Data.TotalTokens, ShouldEqual, 0)
		So(strings.Contains(w.Body.String(), `"total_requests":0`), ShouldBeTrue)
	})
}

// TestGetUserDashboardAggregatePermissions 锁定 aggregate handler 的权限 scope 边界：
// 普通用户请求 channel/user 维度必须在触库前返回 HTTP 403（测试期间把 LOG_DB 置 nil，
// 一旦触库即 panic，以此证明 403 先于 model 查询）；普通用户请求 model/token 维度一律
// 强制限定为认证用户 ID，请求中的 username 参数被忽略；管理员 userID=0，username 非空
// 时精确过滤、为空时覆盖全部用户。
func TestGetUserDashboardAggregatePermissions(t *testing.T) {
	restore := initLegacyDashboardTestDB(t)
	defer restore()

	day := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()

	// u1 与 u2 使用不同 model/token 名，便于按行归属判断 scope 是否生效
	seed := []*model.Log{
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o", TokenName: "tok-a", CreatedAt: day, Quota: 10},
		{UserId: 2, Username: "u2", Type: model.LogTypeConsume, ModelName: "gpt-4o-mini", TokenName: "tok-b", CreatedAt: day, Quota: 20},
	}
	for _, l := range seed {
		if err := model.LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}

	start := strconv.FormatInt(day-3600, 10)
	end := strconv.FormatInt(day+3600, 10)

	Convey("Restricted dimensions from a common user are HTTP 403 before any query", t, func() {
		// 触库即 panic：用于证明受限维度未调用 model 查询
		origLogDB := model.LOG_DB
		model.LOG_DB = nil
		defer func() { model.LOG_DB = origLogDB }()

		for _, dimension := range []string{"channel", "user"} {
			query := "?dimension=" + dimension + "&granularity=day&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardAggregateAs(model.RoleCommonUser, 1, query)
			So(w.Code, ShouldEqual, http.StatusForbidden)
			So(resp.Success, ShouldBeFalse)
		}
	})

	Convey("Common user scope is forced to the authenticated user ID", t, func() {
		Convey("model dimension returns only the authenticated user's rows", func() {
			query := "?dimension=model&granularity=day&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardAggregateAs(model.RoleCommonUser, 1, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			So(len(resp.Data), ShouldEqual, 1)
			So(resp.Data[0].Key, ShouldEqual, "gpt-4o")
			So(resp.Data[0].Requests, ShouldEqual, 1)
		})

		Convey("a supplied username never widens the common user's scope", func() {
			query := "?dimension=model&granularity=day&username=u2&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardAggregateAs(model.RoleCommonUser, 1, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			// username=u2 被忽略：仍只返回本人（u1）的 gpt-4o，绝不返回 u2 的 gpt-4o-mini
			So(len(resp.Data), ShouldEqual, 1)
			So(resp.Data[0].Key, ShouldEqual, "gpt-4o")
		})

		Convey("token dimension is also forced to the authenticated user", func() {
			query := "?dimension=token&granularity=day&username=u2&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardAggregateAs(model.RoleCommonUser, 1, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			So(len(resp.Data), ShouldEqual, 1)
			So(resp.Data[0].Key, ShouldEqual, "tok-a")
		})
	})

	Convey("Administrator scope follows the exact-username/all-users contract", t, func() {
		Convey("empty username covers all users", func() {
			query := "?dimension=model&granularity=day&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardAggregateAs(model.RoleAdminUser, 2, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			// userID=0 覆盖全部用户：两个不同 model 各自成行
			keys := []string{}
			for _, row := range resp.Data {
				keys = append(keys, row.Key)
			}
			So(len(resp.Data), ShouldEqual, 2)
			So(keys, ShouldContain, "gpt-4o")
			So(keys, ShouldContain, "gpt-4o-mini")
		})

		Convey("non-empty username filters exactly that user", func() {
			query := "?dimension=model&granularity=day&username=u2&start_timestamp=" + start + "&end_timestamp=" + end
			w, resp := callDashboardAggregateAs(model.RoleAdminUser, 1, query)

			So(w.Code, ShouldEqual, http.StatusOK)
			So(resp.Success, ShouldBeTrue)
			So(len(resp.Data), ShouldEqual, 1)
			So(resp.Data[0].Key, ShouldEqual, "gpt-4o-mini")
		})
	})
}

// TestGetUserDashboardAggregateValidation 锁定 aggregate handler 的参数校验边界：
// 非法 dimension/granularity、非法或倒置的时间范围一律在调用 model 查询前返回 HTTP 400
// （测试期间把 LOG_DB 置 nil，一旦触库即 panic，以此证明校验先于查询）；
// 合法但空结果区间返回 HTTP 200 与 {"success":true,"message":"","data":[]}。
func TestGetUserDashboardAggregateValidation(t *testing.T) {
	restore := initLegacyDashboardTestDB(t)
	defer restore()

	day := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()

	Convey("GetUserDashboardAggregate rejects invalid input before querying", t, func() {
		// 触库即 panic：用于证明非法输入未调用 model 查询
		origLogDB := model.LOG_DB
		model.LOG_DB = nil
		defer func() { model.LOG_DB = origLogDB }()

		Convey("Invalid dimension is HTTP 400", func() {
			w, resp := callDashboardAggregate("?dimension=models&granularity=day&start_timestamp=1&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			So(resp.Success, ShouldBeFalse)
		})

		Convey("Invalid granularity is HTTP 400", func() {
			w, resp := callDashboardAggregate("?dimension=model&granularity=minute&start_timestamp=1&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			So(resp.Success, ShouldBeFalse)
		})

		Convey("Malformed timestamps are HTTP 400", func() {
			w, resp := callDashboardAggregate("?dimension=model&granularity=day&start_timestamp=abc&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			So(resp.Success, ShouldBeFalse)

			w, resp = callDashboardAggregate("?dimension=model&granularity=day&start_timestamp=1&end_timestamp=abc")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			So(resp.Success, ShouldBeFalse)

			// 非正整数（0 / 负数）同样非法
			w, _ = callDashboardAggregate("?dimension=model&granularity=day&start_timestamp=0&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			w, _ = callDashboardAggregate("?dimension=model&granularity=day&start_timestamp=-5&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
		})

		Convey("start >= end is HTTP 400", func() {
			w, _ := callDashboardAggregate("?dimension=model&granularity=day&start_timestamp=100&end_timestamp=100")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			w, _ = callDashboardAggregate("?dimension=model&granularity=day&start_timestamp=200&end_timestamp=100")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
		})

		Convey("Missing required parameters are HTTP 400", func() {
			w, _ := callDashboardAggregate("?granularity=day&start_timestamp=1&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
			w, _ = callDashboardAggregate("?dimension=model&start_timestamp=1&end_timestamp=2")
			So(w.Code, ShouldEqual, http.StatusBadRequest)
		})
	})

	Convey("GetUserDashboardAggregate returns HTTP 200 with an empty array for an empty range", t, func() {
		// 空区间：无任何日志命中，仍须是成功的空数组而非错误
		emptyStart := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		emptyEnd := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC).Unix()

		query := "?dimension=model&granularity=day&start_timestamp=" +
			strconv.FormatInt(emptyStart, 10) + "&end_timestamp=" + strconv.FormatInt(emptyEnd, 10)
		w, resp := callDashboardAggregate(query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		So(resp.Message, ShouldEqual, "")
		So(resp.Data, ShouldNotBeNil)
		So(len(resp.Data), ShouldEqual, 0)

		// 信封必须显式为 []，而非 null
		So(strings.Contains(w.Body.String(), `"data":[]`), ShouldBeTrue)
	})

	Convey("GetUserDashboardAggregate returns rows for a populated range", t, func() {
		if err := model.LOG_DB.Create(&model.Log{
			UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day, Quota: 10,
		}).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}

		query := "?dimension=model&granularity=day&start_timestamp=" +
			strconv.FormatInt(day-3600, 10) + "&end_timestamp=" + strconv.FormatInt(day+3600, 10)
		w, resp := callDashboardAggregate(query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		So(len(resp.Data), ShouldEqual, 1)
		So(resp.Data[0].Key, ShouldEqual, "gpt-4o")
	})
}

// TestLegacyDashboardCompatibility 锁定旧 dashboard 接口的响应信封与行内容基线：
// GET /api/user/dashboard 必须保持 {"success":true,"message":"","data":[...]} 结构，
// 且 day/model 行字段与取值不变。生产 handler 与旧路由一律不得改动，本用例是回归闸门。
func TestLegacyDashboardCompatibility(t *testing.T) {
	restore := initLegacyDashboardTestDB(t)
	defer restore()

	day1 := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC).Unix()

	seed := []*model.Log{
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "DeepSeek-v4-Pro", Quota: 100, PromptTokens: 10, CompletionTokens: 20, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "deepseek-v4-pro", Quota: 150, PromptTokens: 11, CompletionTokens: 21, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o", Quota: 50, PromptTokens: 5, CompletionTokens: 6, CreatedAt: day2},
		{UserId: 1, Username: "u1", Type: model.LogTypeManage, ModelName: "DeepSeek-v4-Pro", CreatedAt: day1},
	}
	for _, l := range seed {
		if err := model.LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}

	// callDashboard 以普通用户身份请求旧接口，返回解析后的响应信封。
	callDashboard := func(start, end int64) legacyDashboardResponse {
		query := "?start_timestamp=" + strconv.FormatInt(start, 10) + "&end_timestamp=" + strconv.FormatInt(end, 10)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/user/dashboard"+query, nil)
		c.Set(ctxkey.Role, model.RoleCommonUser)
		c.Set(ctxkey.Id, 1)

		GetUserDashboard(c)

		So(w.Code, ShouldEqual, http.StatusOK)
		var resp legacyDashboardResponse
		So(json.Unmarshal(w.Body.Bytes(), &resp), ShouldBeNil)
		return resp
	}

	Convey("GetUserDashboard keeps the legacy response envelope and rows", t, func() {
		Convey("Common user gets success envelope with normalized day/model rows", func() {
			resp := callDashboard(day1-86400, day2+86400)

			So(resp.Success, ShouldBeTrue)
			So(resp.Message, ShouldEqual, "")
			So(len(resp.Data), ShouldEqual, 2)

			So(resp.Data[0].Day, ShouldEqual, "2024-07-03")
			So(resp.Data[0].ModelName, ShouldEqual, "deepseek-v4-pro")
			So(resp.Data[0].RequestCount, ShouldEqual, 2)
			So(resp.Data[0].Quota, ShouldEqual, 250)
			So(resp.Data[0].PromptTokens, ShouldEqual, 21)
			So(resp.Data[0].CompletionTokens, ShouldEqual, 41)

			So(resp.Data[1].Day, ShouldEqual, "2024-07-04")
			So(resp.Data[1].ModelName, ShouldEqual, "gpt-4o")
			So(resp.Data[1].RequestCount, ShouldEqual, 1)
			So(resp.Data[1].Quota, ShouldEqual, 50)
		})

		Convey("Empty range still returns a successful empty envelope", func() {
			resp := callDashboard(day2+100000, day2+200000)

			So(resp.Success, ShouldBeTrue)
			So(resp.Message, ShouldEqual, "")
			So(len(resp.Data), ShouldEqual, 0)
		})
	})
}

// TestGetUserDashboardAggregateModelNormalization 是 aggregate 链路的端到端回归闸门：
// 锁定 dimension=model 时 handler 返回的行已经过模型名归一化（厂商前缀剥离 + 小写合并），
// 同一模型的 openai/GPT-4o 与 gpt-4o 两个变体必须合并为单行且求和指标正确——这正是
// "normalizeDashboardAggregates 仅有单测、链路未调用" 盲区所暴露的真实缺陷 D1。
// 同时断言 dimension=token 下大小写不同的 token 仍保持独立行，防止过度归一化。
func TestGetUserDashboardAggregateModelNormalization(t *testing.T) {
	restore := initLegacyDashboardTestDB(t)
	defer restore()

	day := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()

	// 同一模型的两个大小写/前缀变体，各自贡献一条消费日志
	seed := []*model.Log{
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "openai/GPT-4o", TokenName: "Token-A",
			CreatedAt: day, Quota: 100, PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5},
		{UserId: 1, Username: "u1", Type: model.LogTypeConsume, ModelName: "gpt-4o", TokenName: "token-a",
			CreatedAt: day, Quota: 200, PromptTokens: 20, CompletionTokens: 40, CachedTokens: 15},
	}
	for _, l := range seed {
		if err := model.LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}

	start := strconv.FormatInt(day-3600, 10)
	end := strconv.FormatInt(day+3600, 10)

	Convey("dimension=model merges vendor-prefix and case variants end to end", t, func() {
		query := "?dimension=model&granularity=day&start_timestamp=" + start + "&end_timestamp=" + end
		w, resp := callDashboardAggregateAs(model.RoleCommonUser, 1, query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		// 必须合并为单行：绝不返回 openai/GPT-4o 与 gpt-4o 两行
		So(len(resp.Data), ShouldEqual, 1)
		So(resp.Data[0].Key, ShouldEqual, "gpt-4o")
		So(resp.Data[0].Requests, ShouldEqual, 2)
		So(resp.Data[0].Quota, ShouldEqual, 300)
		So(resp.Data[0].PromptTokens, ShouldEqual, 30)
		So(resp.Data[0].CompletionTokens, ShouldEqual, 60)
		So(resp.Data[0].CachedTokens, ShouldEqual, 20)
	})

	Convey("dimension=token keeps case variants as distinct rows", t, func() {
		query := "?dimension=token&granularity=day&start_timestamp=" + start + "&end_timestamp=" + end
		w, resp := callDashboardAggregateAs(model.RoleCommonUser, 1, query)

		So(w.Code, ShouldEqual, http.StatusOK)
		So(resp.Success, ShouldBeTrue)
		// 防过度归一化：token 维度逐字保留，大小写不同即两行独立
		So(len(resp.Data), ShouldEqual, 2)
		keys := []string{}
		for _, row := range resp.Data {
			keys = append(keys, row.Key)
		}
		So(keys, ShouldContain, "Token-A")
		So(keys, ShouldContain, "token-a")
	})
}
