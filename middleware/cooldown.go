package middleware

import (
	"strings"
	"sync"
	"time"

	"github.com/pai801/myapi/common/config"
)

var CooldownGlobal = NewCooldownManager(config.ChannelCooldownSeconds)

// cooldownKey 以 (channelId, model) 二元组定位冷却条目，将冷却粒度细化到渠道+模型
type cooldownKey struct {
	channelId int
	model     string
}

type CooldownManager struct {
	mu          sync.Mutex
	entries     map[cooldownKey]time.Time // (channelId, model) -> cooldown expire timestamp
	cooldownDur time.Duration
}

func NewCooldownManager(seconds int) *CooldownManager {
	return &CooldownManager{
		entries:     make(map[cooldownKey]time.Time),
		cooldownDur: time.Duration(seconds) * time.Second,
	}
}

// Put 记录一次冷却；model 为空字符串时退化为渠道级冷却（兼容未携带模型信息的调用方）
func (cm *CooldownManager) Put(channelId int, model string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.entries[cooldownKey{channelId: channelId, model: normalizeCooldownModel(model)}] = time.Now().Add(cm.cooldownDur)
}

func (cm *CooldownManager) IsCoolingDown(channelId int, model string) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	key := cooldownKey{channelId: channelId, model: normalizeCooldownModel(model)}
	expiresAt, exists := cm.entries[key]
	if !exists {
		return false
	}

	if time.Now().After(expiresAt) {
		delete(cm.entries, key)
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
