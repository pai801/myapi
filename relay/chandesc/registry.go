package chandesc

import (
	"fmt"
	"sync"
)

// Registry 是渠道能力清单的注册表。
//
// 单独做成结构体（而非纯包级状态）是为了让测试能构造一个**空注册表**，
// 覆盖「无任何渠道注册」的真实情形。
type Registry struct {
	mu sync.RWMutex
	// byID / byType 双索引：byID 用于重复注册检测与保序遍历，byType 供前端按渠道类型查找。
	byID   map[string]Descriptor
	byType map[int]string
	// order 记录注册顺序，供 All / RegisteredIDs 保序遍历（与其它 registry 风格一致）。
	order []string
}

// New 返回一个空的注册表。
func New() *Registry {
	return &Registry{byID: make(map[string]Descriptor), byType: make(map[int]string)}
}

// Register 登记一个渠道能力清单。
//
// 重复的 ID 或重复的 ChannelType 都属于启动期的代码错误（注册各只会发生一次），
// 因此直接 panic 并在信息中带上冲突键（与 adaptor.Register / chanregistry.Register 语义一致）。
func (r *Registry) Register(d Descriptor) {
	if d.ID == "" {
		panic("chandesc: empty descriptor ID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[d.ID]; ok {
		panic(fmt.Sprintf("chandesc: duplicate registration for id %q", d.ID))
	}
	if existing, ok := r.byType[d.ChannelType]; ok {
		panic(fmt.Sprintf("chandesc: duplicate channel type %d (already registered as %q)", d.ChannelType, existing))
	}
	r.byID[d.ID] = d
	r.byType[d.ChannelType] = d.ID
	r.order = append(r.order, d.ID)
}

// Get 返回 ChannelType 对应的清单；未命中（含空注册表）返回 nil。
// 返回的是**深拷贝**，调用方无法借它（含嵌套 map）改写注册表内部状态。
func (r *Registry) Get(channelType int) *Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byType[channelType]
	if !ok {
		return nil
	}
	d := r.byID[id].Clone()
	return &d
}

// All 按注册顺序返回全部清单（深拷贝）；空注册表返回空切片（保证 JSON 序列化为 [] 而非 null）。
func (r *Registry) All() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id].Clone())
	}
	return out
}

// RegisteredIDs 按注册顺序返回已登记的 ID（保序，便于测试与诊断）。
func (r *Registry) RegisteredIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// defaultRegistry 是进程内默认注册表。渠道在 init() 里通过包级 Register 注册进来。
var defaultRegistry = New()

// Register 在默认注册表上登记一个清单（供扩展方的 init() 调用）。
func Register(d Descriptor) { defaultRegistry.Register(d) }

// Get 在默认注册表上按渠道类型查找清单；未命中（含空注册表）返回 nil。
func Get(channelType int) *Descriptor { return defaultRegistry.Get(channelType) }

// All 返回默认注册表上的全部清单（保序）；空注册表返回空切片。
func All() []Descriptor { return defaultRegistry.All() }

// RegisteredIDs 返回默认注册表上已登记的 ID（保序）。
func RegisteredIDs() []string { return defaultRegistry.RegisteredIDs() }
