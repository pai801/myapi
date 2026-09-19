package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestChannelRoutesAbsentWithEmptyRegistry 锁定默认行为（PRD §5.11）：主仓不注册任何渠道专属
// 路由（注册表为空），故这些路由均不存在 → 请求返回 404。
func TestChannelRoutesAbsentWithEmptyRegistry(t *testing.T) {
	r := newTestEngine()
	SetApiRouter(r)

	got := routeSet(r)
	for _, w := range []string{
		"GET /api/ext/callback",
		"POST /api/channel/ext/login/start",
		"GET /api/channel/ext/login/poll",
	} {
		if got[w] {
			t.Errorf("route %q must NOT be registered with an empty registry", w)
		}
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/ext/callback"},
		{http.MethodPost, "/api/channel/ext/login/start"},
		{http.MethodGet, "/api/channel/ext/login/poll"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s code=%d, want 404 (empty registry, no channel routes)", tc.method, tc.path, w.Code)
		}
	}
}
