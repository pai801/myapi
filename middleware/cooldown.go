package middleware

import (
	"strings"
	"sync"
	"time"

	"github.com/pai801/myapi/common/config"
)

// CooldownGlobal 全局冷却管理器；阈值/窗口采用"运行时读取 config"，
// 数据库更新 OptionMap 后即可立即生效，不会让配置形同虚设。
var CooldownGlobal = NewCooldownManager(
	config.ChannelCooldownSeconds,
	func() int { return config.ChannelCooldownErrorThreshold },
	func() int { return config.ChannelCooldownErrorWindowSeconds },
)

// cooldownKey 以 (channelId, model) 二元组定位冷却条目，将冷却粒度细化到渠道+模型
type cooldownKey struct {
	channelId int
	model     string
}

// cooldownEntry 维护某 (channelId, model) 的连续失败累计及冷却到期时间
type cooldownEntry struct {
	failCount  int
	expireAt   time.Time
	lastFailAt time.Time
}

type CooldownManager struct {
	mu              sync.Mutex
	entries         map[cooldownKey]*cooldownEntry
	cooldownDur     time.Duration
	thresholdSource func() int // 运行时阈值来源（避免数据库更新配置后不生效）
	windowSource   func() int // 运行时窗口来源
	now             func() time.Time
}

// NewCooldownManager 构造冷却管理器。
// thresholdSource/windowSource 为 nil 时 resolveThreshold/resolveWindow 返回 0：
// ReportFailure 中 threshold > 0 条件恒为 false，因此只累计权重、永不触发冷却（仅计数不冷却）。
// （注：Go 不支持重载，旧版本曾规划的单参签名 NewCooldownManager(seconds) 不存在。）
func NewCooldownManager(seconds int, thresholdSource func() int, windowSource func() int) *CooldownManager {
	return &CooldownManager{
		entries:         make(map[cooldownKey]*cooldownEntry),
		cooldownDur:     time.Duration(seconds) * time.Second,
		thresholdSource: thresholdSource,
		windowSource:    windowSource,
		now:             time.Now,
	}
}

// Put 强制立即写入冷却到期时间（兼容语义，供手动/测试场景使用，不计入累计）
func (cm *CooldownManager) Put(channelId int, model string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	key := cooldownKey{channelId: channelId, model: normalizeCooldownModel(model)}
	entry, ok := cm.entries[key]
	if !ok {
		entry = &cooldownEntry{}
		cm.entries[key] = entry
	}
	entry.expireAt = cm.now().Add(cm.cooldownDur)
	entry.failCount = 0
	entry.lastFailAt = time.Time{}
}

// ReportFailure 上报一次失败；weight<=0 直接忽略；累计权重达到阈值才写入冷却到期时间，
// 触发后 failCount 清零（防止冷却解除后立即再次触发需重新累计）。
func (cm *CooldownManager) ReportFailure(channelId int, model string, weight int) {
	if weight <= 0 {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()

	key := cooldownKey{channelId: channelId, model: normalizeCooldownModel(model)}
	entry, ok := cm.entries[key]
	if !ok {
		entry = &cooldownEntry{}
		cm.entries[key] = entry
	}

	now := cm.now()
	threshold := cm.resolveThreshold()
	window := cm.resolveWindow()

	// 已处于冷却中：仅刷新 failCount 为 0，等待冷却自然到期，不重复叠加权重
	if !entry.expireAt.IsZero() && now.Before(entry.expireAt) {
		entry.failCount = 0
		entry.lastFailAt = now
		return
	}

	// 超出窗口则先清零再累加（避免很久前的一次失败仍计入新窗口）
	if !entry.lastFailAt.IsZero() && window > 0 && now.Sub(entry.lastFailAt) > time.Duration(window)*time.Second {
		entry.failCount = 0
	}

	entry.failCount += weight
	entry.lastFailAt = now

	if threshold > 0 && entry.failCount >= threshold {
		entry.expireAt = now.Add(cm.cooldownDur)
		entry.failCount = 0
	}
}

// ResetSuccess 成功时清零该 key 的失败计数；已冷却中的维持原状（成功不提前解除冷却）。
func (cm *CooldownManager) ResetSuccess(channelId int, model string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	key := cooldownKey{channelId: channelId, model: normalizeCooldownModel(model)}
	entry, ok := cm.entries[key]
	if !ok {
		return
	}
	entry.failCount = 0
	entry.lastFailAt = time.Time{}
}

func (cm *CooldownManager) IsCoolingDown(channelId int, model string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	key := cooldownKey{channelId: channelId, model: normalizeCooldownModel(model)}
	entry, exists := cm.entries[key]
	if !exists || entry.expireAt.IsZero() {
		return false
	}

	now := cm.now()
	if now.After(entry.expireAt) {
		entry.expireAt = time.Time{}
		entry.failCount = 0
		return false
	}

	return true
}

// ResetChannel 清除该渠道下所有模型的冷却条目（供手动重置渠道使用）
func (cm *CooldownManager) ResetChannel(channelId int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for key := range cm.entries {
		if key.channelId == channelId {
			delete(cm.entries, key)
		}
	}
}

// 渠道 Models/ModelsAlias 按逗号分割后可能残留空格，触发冷却与查询冷却两侧的模型名需归一化才能命中同一 key
func normalizeCooldownModel(model string) string {
	return strings.TrimSpace(model)
}

// resolveThreshold 运行时读取阈值；thresholdSource 为 nil 时返回 0。
// 注：threshold=0 时 ReportFailure 仍会累加 failCount，只是 threshold > 0 条件恒为 false，永不触发冷却。
func (cm *CooldownManager) resolveThreshold() int {
	if cm.thresholdSource == nil {
		return 0
	}
	return cm.thresholdSource()
}

// resolveWindow 运行时读取窗口；为 0 表示禁用窗口过期清零逻辑。
func (cm *CooldownManager) resolveWindow() int {
	if cm.windowSource == nil {
		return 0
	}
	return cm.windowSource()
}