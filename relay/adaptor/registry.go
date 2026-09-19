package adaptor

import (
	"fmt"
	"sync"

	"github.com/pai801/myapi/common/logger"
)

// ProviderAPIVersion 是本 registry 暴露的 provider 契约版本（PRD §5.9）。
// 主仓 registry 接口（Register/MustRegister/Get/RegisteredAPITypes 的签名与语义）
// 发生不兼容变更时必须递增此值；扩展方在 init() 注册时应断言与本值一致，
// 不一致则启动期即报错。本阶段只定义常量与语义，断言方随扩展方接入落地。
const ProviderAPIVersion = 1

// builtinPriority 是经 Register 登记的内置渠道的冲突裁决优先级。
// 预留字段：当前阶段仅被写入、未参与裁决；扩展方接入后，按 PRD §5.8
// 实现「内置 > 扩展」优先级裁决（内置渠道优先于扩展/插件渠道，扩展方注册时使用更低的值）时启用。
const builtinPriority = 100

// entry 是注册表中的一个条目。
type entry struct {
	factory func() Adaptor
	// priority 与 builtin 为预留字段：当前阶段仅被写入、未被读取，未参与任何冲突裁决。
	// 扩展方接入后，按 PRD §5.8 实现「内置 > 扩展」优先级裁决时启用。
	priority int
	builtin  bool
}

var (
	registryMu sync.RWMutex
	registry   = make(map[int]*entry)
	// order 记录注册顺序，供 RegisteredAPITypes 保序遍历（PRD §5.8 要求保序）。
	order []int
)

// Register 以内置权威语义登记一个 adaptor 工厂。
// 同一 apiType 重复注册属于启动期的代码错误（内置注册只会发生一次），
// 因此直接 panic 并在信息中带上冲突的 apiType 数值，让错误尽早暴露。
func Register(apiType int, factory func() Adaptor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[apiType]; ok {
		panic(fmt.Sprintf("adaptor: duplicate registration for apiType %d", apiType))
	}
	registry[apiType] = &entry{factory: factory, priority: builtinPriority, builtin: true}
	order = append(order, apiType)
}

// MustRegister 允许覆盖已登记的 apiType，绕过 Register 的重复注册保护。
// 仅供测试显式覆盖使用；生产代码必须使用 Register。
func MustRegister(apiType int, factory func() Adaptor) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if e, ok := registry[apiType]; ok {
		e.factory = factory
		return
	}
	registry[apiType] = &entry{factory: factory, priority: builtinPriority, builtin: false}
	order = append(order, apiType)
}

// Get 返回 apiType 对应的 adaptor 实例；未命中时返回 nil
// （与改造前 switch 落空隐式返回 nil 的行为完全一致）。
// factory 调用被 recover 包裹：单个渠道 panic 不会带崩进程，转为返回 nil 并记录错误日志（PRD §5.10）。
func Get(apiType int) Adaptor {
	registryMu.RLock()
	e, ok := registry[apiType]
	var factory func() Adaptor
	if ok {
		factory = e.factory
	}
	registryMu.RUnlock()
	if !ok {
		return nil
	}
	return safeFactory(apiType, factory)
}

func safeFactory(apiType int, factory func() Adaptor) (a Adaptor) {
	defer func() {
		if r := recover(); r != nil {
			logger.Log.Errorf("adaptor: factory for apiType %d panicked: %v", apiType, r)
			a = nil
		}
	}()
	return factory()
}

// RegisteredAPITypes 按注册顺序返回已登记的 apiType（PRD §5.8 保序）。
func RegisteredAPITypes() []int {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]int, len(order))
	copy(out, order)
	return out
}

// Unregister 从注册表中移除指定 apiType（对称于 Register / MustRegister，同样加写锁）。
//
// 供本仓测试使用：验证「扩展渠道被正确枚举」之类的行为需要临时注册一个 apiType，
// 测试收尾必须能把它清理干净，否则会污染同一进程内其它测试（例如断言注册表项数的
// registry_test.go）。这与 channeltype.UnregisterAPIType 提供的清理能力保持一致。
//
// 对未登记的 apiType 调用本函数为空操作（不 panic）；apiType 不在 order 列表中的
// 异常情形同样安全（仅删 map 项，不触碰 order）。
func Unregister(apiType int) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, ok := registry[apiType]; !ok {
		return
	}
	delete(registry, apiType)
	for i, at := range order {
		if at == apiType {
			order = append(order[:i], order[i+1:]...)
			break
		}
	}
}
