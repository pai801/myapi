package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/relay/routeregistry"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	// common.RedisEnabled 默认 true，但测试未初始化 RDB；置 false 让限流走内存实现，
	// 否则 GlobalAPIRateLimit 会解引用 nil 的 common.RDB 而 panic。
	common.RedisEnabled = false
	m.Run()
}

// newTestEngine 复刻 main.go 的最小中间件装配。
// sessions 必装：鉴权组的 AdminAuth 会调用 sessions.Default（MustGet），缺失会 panic。
func newTestEngine() *gin.Engine {
	r := gin.New()
	store := cookie.NewStore([]byte("test-session-secret"))
	r.Use(sessions.Sessions("session", store))
	return r
}

// routeSet 收集 engine 的 "METHOD PATH" 集合，便于断言路由是否注册。
func routeSet(r *gin.Engine) map[string]bool {
	set := make(map[string]bool)
	for _, ri := range r.Routes() {
		set[ri.Method+" "+ri.Path] = true
	}
	return set
}

// routeHandler 返回指定 "METHOD PATH" 上最后一个处理器（即 controller handler）的全限定名，
// 未注册时返回空串。中间件不在 Routes() 中暴露，故仅用于断言 handler 绑定。
func routeHandler(r *gin.Engine, method, path string) string {
	for _, ri := range r.Routes() {
		if ri.Method == method && ri.Path == path {
			return ri.Handler
		}
	}
	return ""
}

// TestDashboardStatisticsRoutesRegistered 锁定 dashboard 统计扩展的路由契约（Task 2.4）：
// 两条新 GET 路由注册在既有 UserAuth 保护的 selfRoute 分组下，且既有
// GET /api/user/dashboard → controller.GetUserDashboard 绑定保持不变。
// 同时验证 /dashboard 与 /dashboard/{aggregate,summary} 在 gin 路由树中共存（SetApiRouter 不 panic）。
func TestDashboardStatisticsRoutesRegistered(t *testing.T) {
	r := newTestEngine()
	SetApiRouter(r)

	got := routeSet(r)

	// 1. 两条新路由已注册，且绑定到预期 handler。
	for _, tc := range []struct {
		method, path, handlerSuffix string
	}{
		{http.MethodGet, "/api/user/dashboard/aggregate", "controller.GetUserDashboardAggregate"},
		{http.MethodGet, "/api/user/dashboard/summary", "controller.GetUserDashboardSummary"},
	} {
		key := tc.method + " " + tc.path
		if !got[key] {
			t.Errorf("route %q not registered", key)
			continue
		}
		if h := routeHandler(r, tc.method, tc.path); !strings.HasSuffix(h, tc.handlerSuffix) {
			t.Errorf("route %q handler=%q, want suffix %q", key, h, tc.handlerSuffix)
		}
	}

	// 2. 既有 legacy 路由绑定保持不变。
	const legacyKey = "GET /api/user/dashboard"
	if !got[legacyKey] {
		t.Errorf("legacy route %q not registered", legacyKey)
	} else if h := routeHandler(r, http.MethodGet, "/api/user/dashboard"); !strings.HasSuffix(h, "controller.GetUserDashboard") {
		t.Errorf("legacy route handler=%q, want suffix %q", h, "controller.GetUserDashboard")
	}

	// 3. 两条新路由处于认证用户分组：无任何凭证时 UserAuth 拒绝并返回 401。
	for _, path := range []string{"/api/user/dashboard/aggregate", "/api/user/dashboard/summary"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s code=%d, want 401 (must be behind UserAuth)", path, w.Code)
		}
	}
}

// TestChannelDescriptorsRouteRegistered 断言渠道能力清单端点已注册（它是主仓路由，非渠道专属；
// 返回空清单由 controller 保证，见 controller/channel_descriptor_test.go）。此处同时验证新增
// 静态路由 /api/channel/descriptors 与既有 /api/channel/:id 不冲突（SetApiRouter 不 panic）。
func TestChannelDescriptorsRouteRegistered(t *testing.T) {
	r := newTestEngine()
	SetApiRouter(r)
	if got := routeSet(r); !got["GET /api/channel/descriptors"] {
		t.Errorf("GET /api/channel/descriptors not registered")
	}
}

// TestEmptyRegistryYields404 模拟空注册表：不挂任何渠道路由，
// 目标路径必须返回 404 且不 panic。
func TestEmptyRegistryYields404(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	mountChannelRoutes(apiRouter, authGroup, routeregistry.New().Registered())

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/ext/callback"},
		{http.MethodPost, "/api/channel/ext/login/start"},
		{http.MethodGet, "/api/channel/ext/login/poll"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s code=%d, want 404 (empty registry)", tc.method, tc.path, w.Code)
		}
	}
}

// TestMountChannelRoutesTwoPhase 用假注册方验证两阶段装配：公开 / 鉴权分别落到两个组，
// 且注册方只描述路由，不接触 engine。
func TestMountChannelRoutesTwoPhase(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	fake := &fakeRegistrar{}
	mountChannelRoutes(apiRouter, authGroup, []routeregistry.RouteRegistrar{fake})

	got := routeSet(r)
	for _, w := range []string{"GET /api/plugin/public", "POST /api/channel/plugin/auth"} {
		if !got[w] {
			t.Errorf("missing route %q after mount", w)
		}
	}
	if !fake.publicCalled || !fake.authCalled {
		t.Errorf("registrar methods not both invoked: public=%v auth=%v", fake.publicCalled, fake.authCalled)
	}
}

type fakeRegistrar struct {
	publicCalled bool
	authCalled   bool
}

func (f *fakeRegistrar) RegisterPublicRoutes(g *gin.RouterGroup) {
	f.publicCalled = true
	g.GET("/plugin/public", func(c *gin.Context) { c.Status(http.StatusOK) })
}

func (f *fakeRegistrar) RegisterAuthRoutes(g *gin.RouterGroup) {
	f.authCalled = true
	g.POST("/plugin/auth", func(c *gin.Context) { c.Status(http.StatusOK) })
}
