package chatgptsub

import (
	"fmt"
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

func TestStickyCleanup(t *testing.T) {
	m := newTestStickyManager(10 * time.Minute)
	m.Set("g", "s1", 1) // 将被手动过期
	m.Set("g", "s2", 2) // 将保留

	// 将 s1 的过期时间设为过去
	m.mu.Lock()
	key := stickyKey("g", "s1")
	if entry, ok := m.entries[key]; ok {
		entry.expiresAt = time.Now().Add(-1 * time.Second)
		m.entries[key] = entry
	}
	m.mu.Unlock()

	// 启动 Cleanup goroutine，等待它运行
	go m.Cleanup(10 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	_, ok1 := m.Get("g", "s1")
	require.False(t, ok1, "expired entry should be cleaned up")

	_, ok2 := m.Get("g", "s2")
	require.True(t, ok2, "non-expired entry should remain")
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
