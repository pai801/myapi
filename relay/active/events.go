package active

import "sync"

// LogRecordData DB 写入成功后推送给前端的完整日志记录，用于 SSE complete 事件。
type LogRecordData struct {
	Id               int    `json:"id"`
	UserId           int    `json:"user_id"`
	CreatedAt        int64  `json:"created_at"`
	Content          string `json:"content"`
	Username         string `json:"username"`
	TokenName        string `json:"token_name"`
	ModelName        string `json:"model_name"`
	Quota            int    `json:"quota"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	ChannelId        int    `json:"channel"`
	RequestId        string `json:"request_id"`
	ElapsedTime      int64  `json:"elapsed_time"`
	IsStream         bool   `json:"is_stream"`
	ChannelName      string `json:"channel_name"`
	HasRequestBody   bool   `json:"has_request_body"`
	HasResponseBody  bool   `json:"has_response_body"`
	HasRequestHeader bool   `json:"has_request_header"`
}

// RequestEvent is a broadcast event for SSE subscribers
type RequestEvent struct {
	Type      string         `json:"type"`                // "start", "update", "end", "complete"
	RequestID string         `json:"request_id,omitempty"` // 仅 end 事件需要
	Data      *ActiveRequest `json:"data,omitempty"`       // start/update 携带（指针才能 omitempty）
	Log       *LogRecordData `json:"log,omitempty"`        // 仅 complete 事件携带
}

type eventBus struct {
	mu          sync.RWMutex
	subscribers map[chan RequestEvent]struct{}
}

var bus = &eventBus{
	subscribers: make(map[chan RequestEvent]struct{}),
}

// Subscribe registers a new subscriber channel.
// Buffer size 64 to tolerate brief subscriber slowness; EventSource reconnect
// will naturally resync if events are still dropped.
func Subscribe() chan RequestEvent {
	ch := make(chan RequestEvent, 64)
	bus.mu.Lock()
	bus.subscribers[ch] = struct{}{}
	bus.mu.Unlock()
	return ch
}

// Unsubscribe removes and closes a subscriber channel
func Unsubscribe(ch chan RequestEvent) {
	bus.mu.Lock()
	delete(bus.subscribers, ch)
	close(ch)
	bus.mu.Unlock()
}

// BroadcastComplete 广播一条 complete 事件，携带完整 DB 日志记录。
// 应在 RecordConsumeLog 成功写入后调用。
func BroadcastComplete(log *LogRecordData) {
	bus.broadcast(RequestEvent{
		Type: "complete",
		Log:  log,
	})
}

func (b *eventBus) broadcast(evt RequestEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- evt:
		default:
			// subscriber too slow, drop event
		}
	}
}
