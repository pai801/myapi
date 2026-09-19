// Package chanregistry 承载「渠道能力」的注册与查找（PRD §5.11 / §7.3）。
//
// 与 relay/adaptor 的 adaptor registry 分成独立包，是为了让 adaptor 接口包保持纯净：
// 后续能力接口（如路由注册）会引入 gin 等依赖，不应污染 adaptor 包。
//
// 本阶段只落地「凭证压缩能力」（CredentialCompressor）。扩展渠道在 init() 里注册自己的
// 压缩器；无注册时 controller 查不到即回退到原有的「按 '\n' 拆分」路径。
package chanregistry

import (
	"fmt"
	"sync"

	"github.com/pai801/myapi/common/logger"
)

// CredentialCompressor 是渠道可注册的「凭证压缩能力」（PRD §5.11）。
//
// 语义：某些渠道（如反代型渠道）的渠道 Key 是**整段凭证 JSON**（可含多行）。
// 这类 Key 落库前必须压缩成紧凑单行，且必须作为**单条**渠道录入，不能按 '\n' 拆分成多条。
type CredentialCompressor interface {
	// Supports 报告本压缩器是否负责给定渠道类型。
	Supports(channelType int) bool
	// CompactKey 把整段凭证 Key 归一化成紧凑单行。无法识别为凭证时按实现约定原样返回。
	CompactKey(raw string) (string, error)
}

// Registry 是凭证压缩器的注册表。
//
// 单独做成结构体（而非纯包级状态）是为了让测试能构造一个**空注册表**，
// 覆盖「无任何渠道注册」的真实情形。
type Registry struct {
	mu          sync.RWMutex
	compressors map[string]CredentialCompressor
	// order 记录注册顺序，供 GetCompressor 保序遍历（与 adaptor registry 风格一致）。
	order []string
}

// New 返回一个空的注册表。
func New() *Registry {
	return &Registry{compressors: make(map[string]CredentialCompressor)}
}

// Register 以「内置权威」语义登记一个压缩器，id 为注册身份。
// 同一 id 重复注册属于启动期的代码错误（内置注册只会发生一次），
// 因此直接 panic 并在信息中带上冲突的 id（与 adaptor.Register 语义一致）。
func (r *Registry) Register(id string, c CredentialCompressor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.compressors[id]; ok {
		panic(fmt.Sprintf("chanregistry: duplicate registration for id %q", id))
	}
	r.compressors[id] = c
	r.order = append(r.order, id)
}

// GetCompressor 按注册顺序返回第一个 Supports(channelType) 为真的压缩器；
// 无匹配（含空注册表）时返回 nil —— 调用方据此回退到原有路径。
//
// Supports 调用被 recover 包裹（PRD §5.10 故障隔离）：扩展压缩器实现 panic 时视为
// 「不支持该渠道」并记录日志，继续尝试下一个压缩器，绝不让单个渠道带崩主进程。
func (r *Registry) GetCompressor(channelType int) CredentialCompressor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, id := range r.order {
		c := r.compressors[id]
		if c == nil {
			continue
		}
		if !safeSupports(id, c, channelType) {
			continue
		}
		return c
	}
	return nil
}

// safeSupports 包裹扩展压缩器的 Supports 调用。panic 时返回 false（降级为「不匹配」），
// 与未注册压缩器的语义一致，使调用方回退到原有「按 '\n' 拆分」路径。
func safeSupports(id string, c CredentialCompressor, channelType int) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Log.Errorf("chanregistry: compressor %q Supports(%d) panicked: %v", id, channelType, rec)
			ok = false
		}
	}()
	return c.Supports(channelType)
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

// Register 在默认注册表上登记一个压缩器（供 relay/register.go 的集中注册调用）。
func Register(id string, c CredentialCompressor) { defaultRegistry.Register(id, c) }

// GetCompressor 在默认注册表上查找压缩器；未命中（含无任何注册）返回 nil。
func GetCompressor(channelType int) CredentialCompressor {
	return defaultRegistry.GetCompressor(channelType)
}

// RegisteredIDs 返回默认注册表上已登记的 id（保序）。
func RegisteredIDs() []string { return defaultRegistry.RegisteredIDs() }
