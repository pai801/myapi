package routeregistry

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// RouteKey 唯一标识一条已注册路由：HTTP 方法 + 含前缀的完整路径。
// 用于挂载前的 method+path 冲突检测（PRD §5.11）。
type RouteKey struct {
	Method string
	Path   string
}

// DeclaredRoutes 在临时 engine 上「试探性」调用注册方的两个 Register 方法，
// 读回它声明的全部 method+path（含前缀的完整路径），供挂载前做冲突检测。
//
// 注册方只有在拿到 *gin.RouterGroup 之后才能声明路由，注册期无法预知其路由，
// 故此处用一个 gin.New() 的临时 engine、按与真实分组一致的前缀建两个组，让注册方
// 先声明一遍，再从 engine.Routes() 收集结果。试探不触碰真实 engine / group，对真实
// 路由无副作用。
//
// 注册方在试探期 panic 时被 recover 兜住并转成 error 返回，不让 panic 冒出去。
//
// 契约：本函数会对 reg 的两个 Register 方法各调用一次，随后真实挂载阶段还会再调用
// 一次，因此 RouteRegistrar 实现必须是「纯路由声明、无副作用」（见 RouteRegistrar 注释）。
func DeclaredRoutes(reg RouteRegistrar, publicPrefix, authPrefix string) ([]RouteKey, error) {
	e := gin.New()
	tmpPublic := e.Group(publicPrefix)
	tmpAuth := e.Group(authPrefix)

	if err := declareRoutes(reg, tmpPublic, tmpAuth); err != nil {
		return nil, err
	}

	routes := e.Routes()
	out := make([]RouteKey, 0, len(routes))
	for _, ri := range routes {
		out = append(out, RouteKey{Method: ri.Method, Path: ri.Path})
	}
	return out, nil
}

// declareRoutes 调用注册方的两个声明方法，panic 时转成 error 返回。
func declareRoutes(reg RouteRegistrar, public, auth *gin.RouterGroup) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("routeregistry: registrar %T panicked while declaring routes: %v", reg, rec)
		}
	}()
	reg.RegisterPublicRoutes(public)
	reg.RegisterAuthRoutes(auth)
	return nil
}
