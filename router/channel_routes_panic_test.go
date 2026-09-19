package router

import (
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/relay/routeregistry"
)

// panicRegistrar 的两个装配方法都故意 panic，用于验证 PRD §5.10 的故障隔离：
// 扩展渠道的路由注册方 panic 时不得带崩主进程启动，只跳过它自己。
type panicRegistrar struct{}

func (panicRegistrar) RegisterPublicRoutes(*gin.RouterGroup) { panic("injected public routes panic") }

func (panicRegistrar) RegisterAuthRoutes(*gin.RouterGroup) { panic("injected auth routes panic") }

// TestMountChannelRoutesIsolatesPanickingRegistrar 锁定：坏注册方 panic 时被跳过，
// 好注册方仍完成两阶段装配（其路由挂载成功）。
func TestMountChannelRoutesIsolatesPanickingRegistrar(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	good := &fakeRegistrar{}
	// 坏的在前、好的在后：坏的不应阻止好的装配（也不应 panic 冒泡）。
	mountChannelRoutes(apiRouter, authGroup, []routeregistry.RouteRegistrar{panicRegistrar{}, good})

	got := routeSet(r)
	for _, w := range []string{"GET /api/plugin/public", "POST /api/channel/plugin/auth"} {
		if !got[w] {
			t.Errorf("good registrar route %q missing after a panicking registrar", w)
		}
	}
	if !good.publicCalled || !good.authCalled {
		t.Errorf("good registrar not fully invoked: public=%v auth=%v", good.publicCalled, good.authCalled)
	}
}
