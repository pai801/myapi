package active

import (
	"sync"
	"time"
)

// ActiveRequest 表示一个正在转发中的请求
type ActiveRequest struct {
	RequestID        string `json:"request_id"`
	UserID           int    `json:"user_id"`
	TokenName        string `json:"token_name"`
	ModelName        string `json:"model_name"`
	ChannelID        int    `json:"channel"`
	ChannelName      string `json:"channel_name"`
	Group            string `json:"group"`
	IsStream         bool   `json:"is_stream"`
	StartedAt        int64  `json:"started_at"`
	ElapsedMs        int64  `json:"elapsed_ms"`
	RelayMode        int    `json:"relay_mode"`
	RequestBody      string `json:"request_body"`
	RequestHeader    string `json:"request_header"`
	HasRequestBody   bool   `json:"has_request_body"`
	HasRequestHeader bool   `json:"has_request_header"`
	Username         string `json:"username,omitempty"`
}

// ActiveStore 并发安全的活跃请求存储
type ActiveStore struct {
	mu    sync.RWMutex
	items map[string]*ActiveRequest // key: request_id
}

// Global 全局活跃请求存储实例
var Global = &ActiveStore{items: make(map[string]*ActiveRequest)}

// Add 添加一个活跃请求。广播在锁内完成以保证 start 事件严格在 Remove 之前送达。
func (s *ActiveStore) Add(req *ActiveRequest) {
	s.mu.Lock()
	s.items[req.RequestID] = req
	copyReq := *req
	bus.broadcast(RequestEvent{Type: "start", Data: &copyReq})
	s.mu.Unlock()
}

// Get 获取指定请求 ID 的活跃请求
func (s *ActiveStore) Get(requestID string) *ActiveRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[requestID]
}

// Update 更新指定请求 ID 的活跃请求信息。广播在锁内完成以保证顺序。
func (s *ActiveStore) Update(requestID string, upd func(*ActiveRequest)) {
	s.mu.Lock()
	req, ok := s.items[requestID]
	var copyReq ActiveRequest
	if ok {
		upd(req)
		copyReq = *req
	}
	if ok {
		bus.broadcast(RequestEvent{Type: "update", Data: &copyReq})
	}
	s.mu.Unlock()
}

// Remove 移除指定请求 ID 的活跃请求。end 事件仅携带 request_id 以缩减 SSE 传输量。
func (s *ActiveStore) Remove(requestID string) {
	s.mu.Lock()
	_, ok := s.items[requestID]
	delete(s.items, requestID)
	s.mu.Unlock()
	if ok {
		bus.broadcast(RequestEvent{Type: "end", RequestID: requestID})
	}
}

// List 返回所有活跃请求的副本列表，动态计算实时耗时。
// 返回的是值副本（非指针），调用方可安全地并发修改/序列化。
func (s *ActiveStore) List() []ActiveRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UnixMilli()
	result := make([]ActiveRequest, 0, len(s.items))
	for _, req := range s.items {
		req.ElapsedMs = now - req.StartedAt
		result = append(result, *req)
	}
	return result
}

// CleanupStale 移除超过 maxAge 毫秒的孤儿条目，并广播 end 事件通知 SSE 订阅者
func (s *ActiveStore) CleanupStale(maxAge int64) int {
	s.mu.Lock()
	now := time.Now().UnixMilli()
	var removedIDs []string
	for id, req := range s.items {
		if now-req.StartedAt > maxAge {
			removedIDs = append(removedIDs, id)
			delete(s.items, id)
		}
	}
	s.mu.Unlock()
	for _, id := range removedIDs {
		bus.broadcast(RequestEvent{Type: "end", RequestID: id})
	}
	return len(removedIDs)
}

// StartCleanupLoop 启动后台协程，定期清理超时的孤儿条目
func StartCleanupLoop(interval time.Duration, maxAge time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			Global.CleanupStale(maxAge.Milliseconds())
		}
	}()
}
