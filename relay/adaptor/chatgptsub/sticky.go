package chatgptsub

import (
	"fmt"
	"sync"
	"time"
)

type stickyEntry struct {
	channelID int
	expiresAt time.Time
}

type StickySessionManager struct {
	mu      sync.RWMutex
	entries map[string]stickyEntry
	ttl     time.Duration
}

// stickyKey 由 group + sessionHash 组成
func stickyKey(group, sessionHash string) string {
	return fmt.Sprintf("%s:%s", group, sessionHash)
}

// Get 返回 sticky binding 的 channelID（如果存在且未过期）
func (m *StickySessionManager) Get(group, sessionHash string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[stickyKey(group, sessionHash)]
	if !ok || time.Now().After(entry.expiresAt) {
		return 0, false
	}
	return entry.channelID, true
}

// Set 记录这次的 channelID 与 session 的绑定
func (m *StickySessionManager) Set(group, sessionHash string, channelID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[stickyKey(group, sessionHash)] = stickyEntry{
		channelID: channelID,
		expiresAt: time.Now().Add(m.ttl),
	}
}

// Cleanup 定期清理过期条目（goroutine 中调用）
func (m *StickySessionManager) Cleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for k, v := range m.entries {
			if now.After(v.expiresAt) {
				delete(m.entries, k)
			}
		}
		m.mu.Unlock()
	}
}

var DefaultStickyManager = &StickySessionManager{
	entries: make(map[string]stickyEntry),
	ttl:     1 * time.Hour,
}

func init() {
	go DefaultStickyManager.Cleanup(10 * time.Minute)
}
