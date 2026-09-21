package chatgptsub

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// defaultStickyMaxEntries 是 sticky 绑定表的容量上限。
//
// 取值理由：sticky 绑定是「会话 → 渠道」的短生命周期映射，TTL 仅 1 小时
// （见 DefaultStickyManager）。20000 条足以覆盖单实例一小时内的高峰活跃会话
// （每条约几十字节，占用约 1~2MB），同时在异常流量（大量伪造 session 打满表）
// 下提供兜底，避免条目无界增长。需要调整时直接改此常量即可；不引入环境变量，
// 以免配置面膨胀（与 affinity 侧由 config 集中管理不同，这里保持包内自洽）。
const defaultStickyMaxEntries = 20000

type stickyEntry struct {
	channelID int
	expiresAt time.Time
}

type StickySessionManager struct {
	mu      sync.RWMutex
	entries map[string]stickyEntry
	ttl     time.Duration
	// maxEntries 为容量上限；<=0 时兜底为 defaultStickyMaxEntries（见 effectiveMaxEntries）。
	maxEntries int
}

// stickyKey 由 group + sessionHash 组成
func stickyKey(group, sessionHash string) string {
	return fmt.Sprintf("%s:%s", group, sessionHash)
}

// effectiveMaxEntries 返回生效的容量上限；maxEntries<=0 时兜底为默认值，
// 避免容量守卫静默失效导致 map 无界增长。
func (m *StickySessionManager) effectiveMaxEntries() int {
	if m.maxEntries > 0 {
		return m.maxEntries
	}
	return defaultStickyMaxEntries
}

// Get 返回 sticky binding 的 channelID（如果存在且未过期）。
// 命中但已过期时惰性删除该条目并返回未命中，避免过期条目长期驻留 map
// （替代原先由后台 goroutine 周期性清理的做法）。
func (m *StickySessionManager) Get(group, sessionHash string) (int, bool) {
	key := stickyKey(group, sessionHash)
	// 过期命中需要删除，故走写锁。
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		return 0, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(m.entries, key)
		return 0, false
	}
	return entry.channelID, true
}

// Set 记录这次的 channelID 与 session 的绑定。
// 写入新键且已达容量上限时先做一次容量治理（见 evictLocked），
// 与 Get 的惰性删除一起保证 map 不会无界增长。
func (m *StickySessionManager) Set(group, sessionHash string, channelID int) {
	key := stickyKey(group, sessionHash)
	m.mu.Lock()
	defer m.mu.Unlock()

	// 仅当写入新键时才治理，避免覆盖已有键时误触发淘汰。
	if _, exists := m.entries[key]; !exists && len(m.entries) >= m.effectiveMaxEntries() {
		m.evictLocked()
	}
	m.entries[key] = stickyEntry{
		channelID: channelID,
		expiresAt: time.Now().Add(m.ttl),
	}
}

// evictLocked 在容量触顶时治理：先全量清除过期条目，仍超限则按 expiresAt 最早淘汰约 5%。
// 调用方必须已持有 m.mu；本函数自身不再加锁，避免重入/死锁。
func (m *StickySessionManager) evictLocked() {
	limit := m.effectiveMaxEntries()
	now := time.Now()
	for k, v := range m.entries {
		if now.After(v.expiresAt) {
			delete(m.entries, k)
		}
	}
	if len(m.entries) < limit {
		return
	}

	evictCount := limit / 20 // 约 5%
	if evictCount < 1 {
		evictCount = 1
	}
	type kv struct {
		key       string
		expiresAt time.Time
	}
	all := make([]kv, 0, len(m.entries))
	for k, v := range m.entries {
		all = append(all, kv{key: k, expiresAt: v.expiresAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].expiresAt.Before(all[j].expiresAt) })
	for i := 0; i < evictCount && i < len(all); i++ {
		delete(m.entries, all[i].key)
	}
}

var DefaultStickyManager = &StickySessionManager{
	entries:    make(map[string]stickyEntry),
	ttl:        1 * time.Hour,
	maxEntries: defaultStickyMaxEntries,
}
