// Package routeregistry 承载「渠道路由」的注册与装配（PRD §5.11 / §7.3 决策 D1 路径 A）。
//
// 与 relay/chanregistry 同属「能力注册表」家族。独立成包（而非塞进 relay/adaptor）是为了让
// adaptor 接口包保持纯净：本包会 import gin，不应把 gin 依赖带进 relay/adaptor。
//
// 两阶段装配（§5.11）：注册方（本仓 router 包，或外部扩展方）只在注册期登记自己，
// 真正的 gin.Engine / RouterGroup 挂载发生在 router 启动期遍历注册表时完成——注册期拿不到
// engine / group 实例，故接口只暴露两个「接收 group」的方法。
//
// 中间件不由注册方决定：挂到哪个组就继承哪个组的中间件（公开组仅 gzip + 全局限流；鉴权组额外
// 挂 AdminAuth），注册方碰不到中间件，安全边界清晰。
//
// 无注册行为：注册表无条目 → 无路由注册 → 请求返回 404（而非 500 / 编译失败）。
package routeregistry

import (
	"fmt"
	"sync"

	"github.com/gin-gonic/gin"
)

// RouteRegistrar 是渠道可注册的「路由装配能力」（PRD §5.11）。
//
// 契约：实现必须是「纯路由声明、无副作用」。挂载前 router 包会先在临时 engine 上
// 试探性调用这两个方法一次（读回声明的 method+path 做冲突检测），随后再在真实分组上
// 调用一次完成挂载——即每个方法会被调用两次。实现不得依赖调用次数、不得在方法内
// 修改外部状态或执行一次性副作用（见 DeclaredRoutes）。
type RouteRegistrar interface {
	// RegisterPublicRoutes 把无需登录态的路由挂到公开组（如第三方回调 /{ext}/callback）。
	RegisterPublicRoutes(g *gin.RouterGroup)
	// RegisterAuthRoutes 把需要鉴权的路由挂到鉴权组（如 /{ext}/login/{start,poll}）。
	RegisterAuthRoutes(g *gin.RouterGroup)
}

// Registry 是路由注册方的注册表。
//
// 单独做成结构体（而非纯包级状态）是为了让测试能构造一个**空注册表**，
// 覆盖「无任何渠道注册」的真实情形。
type Registry struct {
	mu         sync.RWMutex
	registrars map[string]RouteRegistrar
	// order 记录注册顺序，供 Registered 保序返回（与 chanregistry / adaptor registry 风格一致）。
	order []string
}

// New 返回一个空的注册表。
func New() *Registry {
	return &Registry{registrars: make(map[string]RouteRegistrar)}
}

// Register 以「内置权威」语义登记一个路由注册方，id 为注册身份。
// 同一 id 重复注册属于启动期的代码错误（内置注册只会发生一次），
// 因此直接 panic 并在信息中带上冲突的 id（与 chanregistry.Register / adaptor.Register 语义一致）。
func (r *Registry) Register(id string, reg RouteRegistrar) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.registrars[id]; ok {
		panic(fmt.Sprintf("routeregistry: duplicate registration for id %q", id))
	}
	r.registrars[id] = reg
	r.order = append(r.order, id)
}

// Registered 按注册顺序返回全部注册方；空注册表返回 nil（据此不挂任何路由）。
func (r *Registry) Registered() []RouteRegistrar {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil
	}
	out := make([]RouteRegistrar, 0, len(r.order))
	for _, id := range r.order {
		if reg := r.registrars[id]; reg != nil {
			out = append(out, reg)
		}
	}
	return out
}

// RegisteredIDs 按注册顺序返回已登记的 id（保序，便于测试与诊断）。
func (r *Registry) RegisteredIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// defaultRegistry 是进程内默认注册表。渠道在 init() 里通过包级 Register 注册进来。
var defaultRegistry = New()

// Register 在默认注册表上登记一个路由注册方（供 router/channel_routes.go 的集中注册调用）。
func Register(id string, reg RouteRegistrar) { defaultRegistry.Register(id, reg) }

// Registered 返回默认注册表上已登记的注册方（保序）；无注册时返回 nil。
func Registered() []RouteRegistrar { return defaultRegistry.Registered() }

// RegisteredIDs 返回默认注册表上已登记的 id（保序）。
func RegisteredIDs() []string { return defaultRegistry.RegisteredIDs() }
