package middleware

import (
	"sort"
	"sync"
	"time"

	"github.com/pai801/myapi/common/config"
)

var AffinityGlobal = NewAffinityManager(
	config.AffinityExpireSeconds,
	config.AffinityTurnExpireSeconds,
	config.AffinitySessionExpireSeconds,
	config.AffinityMaxEntries,
)

type affinityEntry struct {
	channelId int
	expiresAt time.Time
}

// AffinityManager 保存「亲和键 → 渠道」的内存态绑定。
// key 为 AffinityKey.Value；过期仅做惰性删除，容量触顶时做淘汰，不起后台 goroutine。
type AffinityManager struct {
	mu         sync.Mutex
	entries    map[string]*affinityEntry
	turnDur    time.Duration
	sessionDur time.Duration
	userDur    time.Duration
	maxEntries int
}

// NewAffinityManager 构造管理器；user/turn/session 为各层 TTL（秒），
// maxEntries 为容量上限；<=0 时兜底为 config.DefaultAffinityMaxEntries（不再表示不限）。
func NewAffinityManager(userSeconds, turnSeconds, sessionSeconds, maxEntries int) *AffinityManager {
	// maxEntries<=0 必须兜底为默认容量：否则 Set 内 am.maxEntries > 0 的容量守卫会静默失效，
	// 亲和表退化为无界增长（正是本次分层改造要防的内存泄漏）。默认值取自 config 的单一真源，
	// 不在此复制魔法数字。
	if maxEntries <= 0 {
		maxEntries = config.DefaultAffinityMaxEntries
	}
	return &AffinityManager{
		entries:    make(map[string]*affinityEntry),
		turnDur:    time.Duration(turnSeconds) * time.Second,
		sessionDur: time.Duration(sessionSeconds) * time.Second,
		userDur:    time.Duration(userSeconds) * time.Second,
		maxEntries: maxEntries,
	}
}

// durationFor 返回给定层级对应的 TTL。
func (am *AffinityManager) durationFor(level AffinityLevel) time.Duration {
	switch level {
	case AffinityLevelTurn:
		return am.turnDur
	case AffinityLevelSession:
		return am.sessionDur
	default:
		return am.userDur
	}
}

// Get 按传入顺序（细 → 粗）依次查，返回首个未过期的有效命中及其层级。
// 命中时若发现过期则惰性删除该条目（保持既有惰性删除语义）。
func (am *AffinityManager) Get(keys []AffinityKey) (channelId int, hitLevel AffinityLevel, ok bool) {
	am.mu.Lock()
	defer am.mu.Unlock()

	now := time.Now()
	for _, key := range keys {
		entry, exists := am.entries[key.Value]
		if !exists {
			continue
		}
		if now.After(entry.expiresAt) {
			delete(am.entries, key.Value)
			continue
		}
		return entry.channelId, key.Level, true
	}
	return 0, AffinityLevelUser, false
}

// Set 写入单个键，TTL 按 key.Level 取。调用方（生产路径经 KeysToSet）会对同一次成功转发
// 逐个调用 Set，写入全部可用层（turn + session，有 session 且开关开启时不含 user；无 session
// 的 turn-only 客户端则含 user）；本方法本身不感知层级组合。
func (am *AffinityManager) Set(key AffinityKey, channelId int) {
	am.mu.Lock()
	defer am.mu.Unlock()

	// 仅当是新键且已达容量上限时才治理，避免覆盖已有键时误淘汰。
	if _, exists := am.entries[key.Value]; !exists && am.maxEntries > 0 && len(am.entries) >= am.maxEntries {
		am.evictLocked()
	}
	am.entries[key.Value] = &affinityEntry{
		channelId: channelId,
		expiresAt: time.Now().Add(am.durationFor(key.Level)),
	}
}

// Remove 删除指定键。
func (am *AffinityManager) Remove(key AffinityKey) {
	am.mu.Lock()
	defer am.mu.Unlock()

	delete(am.entries, key.Value)
}

// evictLocked 在容量触顶时治理：先全量清除过期条目，仍超限则按 expiresAt 最早淘汰约 5%。
// 调用方必须已持有 am.mu。
func (am *AffinityManager) evictLocked() {
	now := time.Now()
	for k, v := range am.entries {
		if now.After(v.expiresAt) {
			delete(am.entries, k)
		}
	}
	if am.maxEntries <= 0 || len(am.entries) < am.maxEntries {
		return
	}

	evictCount := am.maxEntries / 20 // 约 5%
	if evictCount < 1 {
		evictCount = 1
	}
	type kv struct {
		key       string
		expiresAt time.Time
	}
	all := make([]kv, 0, len(am.entries))
	for k, v := range am.entries {
		all = append(all, kv{key: k, expiresAt: v.expiresAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].expiresAt.Before(all[j].expiresAt) })
	for i := 0; i < evictCount && i < len(all); i++ {
		delete(am.entries, all[i].key)
	}
}
