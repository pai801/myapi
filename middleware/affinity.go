package middleware

import (
	"fmt"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common/config"
)

var AffinityGlobal = NewAffinityManager(config.AffinityExpireSeconds)

type affinityEntry struct {
	channelId int
	expiresAt time.Time
}

type AffinityManager struct {
	mu        sync.Mutex
	entries   map[string]*affinityEntry // key: "userId:model"
	expireDur time.Duration
}

func NewAffinityManager(seconds int) *AffinityManager {
	return &AffinityManager{
		entries:   make(map[string]*affinityEntry),
		expireDur: time.Duration(seconds) * time.Second,
	}
}

func buildKey(userId int, model string) string {
	return fmt.Sprintf("%d:%s", userId, model)
}

func (am *AffinityManager) Get(userId int, model string) (channelId int, ok bool) {
	am.mu.Lock()
	defer am.mu.Unlock()

	key := buildKey(userId, model)
	entry, exists := am.entries[key]
	if !exists {
		return 0, false
	}

	if time.Now().After(entry.expiresAt) {
		delete(am.entries, key)
		return 0, false
	}

	return entry.channelId, true
}

func (am *AffinityManager) Set(userId int, model string, channelId int) {
	am.mu.Lock()
	defer am.mu.Unlock()

	key := buildKey(userId, model)
	am.entries[key] = &affinityEntry{
		channelId: channelId,
		expiresAt: time.Now().Add(am.expireDur),
	}
}

func (am *AffinityManager) Remove(userId int, model string) {
	am.mu.Lock()
	defer am.mu.Unlock()

	key := buildKey(userId, model)
	delete(am.entries, key)
}
