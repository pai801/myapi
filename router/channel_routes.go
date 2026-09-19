package router

import (
	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/routeregistry"
)

// 本文件保留「渠道专属路由」的两阶段装配函数（PRD §5.11 / §7.3 决策 D3）。
//
// 渠道专属路由（各扩展渠道的登录与回调）由扩展方提供：注册方在其 init() 里经
// routeregistry.Register 登记自己（id 为渠道标识），本仓不注册任何渠道 → 注册表为空
// → 这些路由不存在 → 请求返回 404。
//
// 为何装配放在 router 包而不是 relay/register.go：注册方的 RouteRegistrar 实现会
// import controller（依赖其 handler），而 controller 已 import relay，若 relay 反向
// import controller 会形成循环依赖；router 包则可自由 import controller。

// mountChannelRoutes 遍历注册表，把各注册方的公开 / 鉴权路由分别挂到两个组上
// ——即两阶段装配的第二步（第一步是注册方 init() 期的登记）。
//
// 抽成独立函数（而非内联进 SetApiRouter）是为了让测试能以「空注册表」直接驱动，
// 覆盖「无路由注册 → 404」的行为。
//
// 保留原签名：内部委托 mountChannelRoutesWithEngine(nil, ...)，即「无基线 engine」的
// 兼容路径（不比对内置路由，仅做注册方之间的 method+path 冲突检测）。
func mountChannelRoutes(publicGroup, authGroup *gin.RouterGroup, registrars []routeregistry.RouteRegistrar) {
	mountChannelRoutesWithEngine(nil, publicGroup, authGroup, registrars)
}

// mountChannelRoutesWithEngine 在两阶段装配的第二步之外，增加一层显式的 method+path
// 冲突检测（PRD §5.11）：撞了内置路由或前一个注册方的路由时，记明确日志并跳过该注册方，
// 不挂载、不触发 gin 自身的重复注册 panic。
//
// engine 非 nil 时，基线 = engine.Routes() 的全部 method+path（内置路由）；engine 为 nil
// 时基线为空。accepted 集合随处理推进累积「基线 + 已接受注册方的路由」，作为后续注册方的
// 比对对象。无冲突时行为与改造前完全一致：按 registrars 顺序、经 safeMountRegistrar 挂载。
func mountChannelRoutesWithEngine(engine *gin.Engine, publicGroup, authGroup *gin.RouterGroup, registrars []routeregistry.RouteRegistrar) {
	builtin := make(map[routeregistry.RouteKey]struct{})
	if engine != nil {
		for _, ri := range engine.Routes() {
			builtin[routeregistry.RouteKey{Method: ri.Method, Path: ri.Path}] = struct{}{}
		}
	}
	accepted := make(map[routeregistry.RouteKey]struct{}, len(builtin))
	for k := range builtin {
		accepted[k] = struct{}{}
	}

	for _, reg := range registrars {
		// 试探期前缀直接取自真实分组（而非硬编码常量）：常量若与 api.go 的分组前缀
		// 各写各的、将来改一处忘另一处，从临时 engine 读回的路径就会与真实路由对不上，
		// 冲突检测静默失真。BasePath() 返回分组的完整前缀（"/api"、"/api/channel"），
		// 天然与真实分组绑定，杜绝这种物理分离的隐患。
		declared, err := routeregistry.DeclaredRoutes(reg, publicGroup.BasePath(), authGroup.BasePath())
		if err != nil {
			logger.Log.Errorf("router: channel route registrar %T failed to declare routes, skipping: %v", reg, err)
			continue
		}
		if key, fromBuiltin, ok := findConflict(declared, accepted, builtin); ok {
			source := "another registrar"
			if fromBuiltin {
				source = "a built-in route"
			}
			logger.Log.Errorf("router: channel route registrar %T declares route %s %s conflicting with %s, skipping registrar", reg, key.Method, key.Path, source)
			continue
		}
		for _, k := range declared {
			accepted[k] = struct{}{}
		}
		safeMountRegistrar(reg, publicGroup, authGroup)
	}
}

// findConflict 返回 declared 中第一个与 accepted 冲突的 key，并标识该冲突来自内置路由
// （fromBuiltin=true）还是另一个注册方（fromBuiltin=false）。无冲突时 ok=false。
func findConflict(declared []routeregistry.RouteKey, accepted, builtin map[routeregistry.RouteKey]struct{}) (key routeregistry.RouteKey, fromBuiltin, ok bool) {
	for _, k := range declared {
		if _, hit := accepted[k]; !hit {
			continue
		}
		_, isBuiltin := builtin[k]
		return k, isBuiltin, true
	}
	return routeregistry.RouteKey{}, false, false
}

// safeMountRegistrar 包裹单个注册方的路由装配，panic 时记录日志并跳过。
func safeMountRegistrar(reg routeregistry.RouteRegistrar, publicGroup, authGroup *gin.RouterGroup) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Log.Errorf("router: channel route registrar %T panicked during mount: %v", reg, rec)
		}
	}()
	reg.RegisterPublicRoutes(publicGroup)
	reg.RegisterAuthRoutes(authGroup)
}
