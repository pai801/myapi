package middleware

import (
	"sync"
	"time"

	"github.com/pai801/myapi/common/config"
)

var CooldownGlobal = NewCooldownManager(config.ChannelCooldownSeconds)

type CooldownManager struct {
	mu          sync.Mutex
	entries     map[int]time.Time // channelId -> cooldown expire timestamp
	cooldownDur time.Duration
}

func NewCooldownManager(seconds int) *CooldownManager {
	return &CooldownManager{
		entries:     make(map[int]time.Time),
		cooldownDur: time.Duration(seconds) * time.Second,
	}
}

func (cm *CooldownManager) Put(channelId int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.entries[channelId] = time.Now().Add(cm.cooldownDur)
}

func (cm *CooldownManager) IsCoolingDown(channelId int) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	expiresAt, exists := cm.entries[channelId]
	if !exists {
		return false
	}

	if time.Now().After(expiresAt) {
		delete(cm.entries, channelId)
		return false
	}

	return true
}

func (cm *CooldownManager) Reset(channelId int) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.entries, channelId)
}
