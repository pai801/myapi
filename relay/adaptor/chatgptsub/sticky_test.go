package chatgptsub

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestStickyManager(ttl time.Duration) *StickySessionManager {
	return &StickySessionManager{
		entries: make(map[string]stickyEntry),
		ttl:     ttl,
	}
}

func TestStickyGetSet(t *testing.T) {
	m := newTestStickyManager(time.Hour)
	m.Set("group1", "session-a", 42)

	chID, ok := m.Get("group1", "session-a")
	require.True(t, ok, "Get should return true for a Set entry")
	require.Equal(t, 42, chID, "Get should return the correct channelID")
}

func TestStickyGetNotExist(t *testing.T) {
	m := newTestStickyManager(time.Hour)

	_, ok := m.Get("nonexistent", "key")
	require.False(t, ok, "Get should return false for a non-existent key")
}

func TestStickyExpiry(t *testing.T) {
	m := newTestStickyManager(10 * time.Millisecond)
	m.Set("group1", "session-exp", 99)

	// 立即读取应在过期前
	chID, ok := m.Get("group1", "session-exp")
	require.True(t, ok, "entry should exist before expiry")
	require.Equal(t, 99, chID)

	// 等待 TTL 过期
	time.Sleep(50 * time.Millisecond)

	_, ok = m.Get("group1", "session-exp")
	require.False(t, ok, "entry should be gone after TTL")
}

func TestStickyOverride(t *testing.T) {
	m := newTestStickyManager(time.Hour)

	m.Set("group1", "session-ovr", 1)
	chID, ok := m.Get("group1", "session-ovr")
	require.True(t, ok)
	require.Equal(t, 1, chID)

	// 覆盖为新的 channelID
	m.Set("group1", "session-ovr", 2)
	chID, ok = m.Get("group1", "session-ovr")
	require.True(t, ok)
	require.Equal(t, 2, chID, "after override, Get should return the new channelID")
}

// TestStickyEvictClearsExpiredBeforeActive 断言：容量触顶触发 evictLocked 时，
// 第一步「先全量清过期」会把所有过期条目清掉，且不误伤仍然活跃的条目。
// 这是替代「后台 goroutine 周期性清理」的容量治理核心路径之一
// （另一路径为 Get 惰性删除，见 TestStickyLazyDeleteOnGet），故逐键验证 map 状态。
func TestStickyEvictClearsExpiredBeforeActive(t *testing.T) {
	const limit = 100
	const half = limit / 2
	m := &StickySessionManager{
		entries:    make(map[string]stickyEntry),
		ttl:        time.Hour,
		maxEntries: limit,
	}

	// 先写入 half 个活跃条目（ttl=1h，不会过期）。
	for i := 0; i < half; i++ {
		m.Set("g", fmt.Sprintf("active-%d", i), i)
	}

	// 再写入 half 个条目，并统一置为已过期。
	for i := 0; i < half; i++ {
		m.Set("g", fmt.Sprintf("expired-%d", i), 1000+i)
	}
	m.mu.Lock()
	for i := 0; i < half; i++ {
		key := stickyKey("g", fmt.Sprintf("expired-%d", i))
		entry := m.entries[key]
		entry.expiresAt = time.Now().Add(-time.Second)
		m.entries[key] = entry
	}
	m.mu.Unlock()

	// 此时 len == limit；写入新键触发 evictLocked。
	m.Set("g", "trigger", 9999)

	m.mu.RLock()
	defer m.mu.RUnlock()

	// 过期条目应被「先全量清过期」这一步全部清除。
	for i := 0; i < half; i++ {
		_, ok := m.entries[stickyKey("g", fmt.Sprintf("expired-%d", i))]
		require.False(t, ok, "expired entry expired-%d should be cleared by evictLocked", i)
	}
	// 活跃条目不得被误伤。
	for i := 0; i < half; i++ {
		_, ok := m.entries[stickyKey("g", fmt.Sprintf("active-%d", i))]
		require.True(t, ok, "active entry active-%d must survive eviction", i)
	}
	// 触发写入的新键应存在。
	_, ok := m.entries[stickyKey("g", "trigger")]
	require.True(t, ok, "trigger key should be present after eviction")
}

func TestStickyConcurrent(t *testing.T) {
	m := newTestStickyManager(time.Hour)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := fmt.Sprintf("session-%d", j)
				m.Set("g", key, n)
				m.Get("g", key)
			}
		}(i)
	}

	wg.Wait()
	// 如果 -race 通过则无数据竞争
}

// TestStickyLazyDeleteOnGet 断言：Get 命中过期条目时返回未命中，并从 map 中惰性删除。
// 这是替代「后台 goroutine 周期性清理」的核心机制，必须在 map 级别（而非仅返回值）验证。
func TestStickyLazyDeleteOnGet(t *testing.T) {
	m := newTestStickyManager(time.Hour)
	m.Set("g", "s-expired", 7)

	// 手动将该条目置为已过期
	m.mu.Lock()
	key := stickyKey("g", "s-expired")
	entry := m.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	m.entries[key] = entry
	m.mu.Unlock()

	_, ok := m.Get("g", "s-expired")
	require.False(t, ok, "expired entry should miss on Get")

	m.mu.RLock()
	_, stillThere := m.entries[key]
	m.mu.RUnlock()
	require.False(t, stillThere, "expired entry should be lazily deleted from the map")
}

// TestStickyCapacityEviction 断言：容量触顶后条目数不超过上限，且读写仍正常。
func TestStickyCapacityEviction(t *testing.T) {
	const limit = 100
	m := &StickySessionManager{
		entries:    make(map[string]stickyEntry),
		ttl:        time.Hour,
		maxEntries: limit,
	}

	// 写入远超上限的条目
	for i := 0; i < limit*3; i++ {
		m.Set("g", fmt.Sprintf("s-%d", i), i)
	}

	m.mu.RLock()
	size := len(m.entries)
	m.mu.RUnlock()
	require.LessOrEqual(t, size, limit, "entries must not exceed maxEntries")

	// 触顶后仍可正常读写
	m.Set("g", "after-evict", 12345)
	chID, ok := m.Get("g", "after-evict")
	require.True(t, ok, "Set/Get should still work after eviction")
	require.Equal(t, 12345, chID)

	m.mu.RLock()
	size = len(m.entries)
	m.mu.RUnlock()
	require.LessOrEqual(t, size, limit, "entries must still not exceed maxEntries after write")
}

// TestStickyTTLIsOneHour 断言 sticky 的 TTL 语义未变（仍为 1 小时）。
func TestStickyTTLIsOneHour(t *testing.T) {
	require.Equal(t, time.Hour, DefaultStickyManager.ttl, "sticky TTL must remain 1 hour")
}

// TestStickyNoAutoStartedGoroutine 断言 sticky.go 不再自动拉起 goroutine
// （原先 init() 里的 `go DefaultStickyManager.Cleanup(...)` 已移除）。
// 采用源码扫描这一确定性方法：若包内出现任何 `go` 启动语句或 init()，用例即失败。
func TestStickyNoAutoStartedGoroutine(t *testing.T) {
	src, err := os.ReadFile("sticky.go")
	require.NoError(t, err)

	require.NotRegexp(t, `(?m)^[ \t]*go[ \t]`, string(src),
		"sticky.go must not launch any goroutine (no auto-started Cleanup)")
	require.NotContains(t, string(src), "func init()",
		"sticky.go must not define init() that auto-starts a goroutine")
}
