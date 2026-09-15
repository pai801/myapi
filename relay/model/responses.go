package model

import (
	"encoding/json"
	"fmt"
)

// ============== Responses API Type Definitions ==============

// ResponsesRequest is the request structure for Responses API
// 顶层字段对齐 docs/responses-protocol.md §2：采样/布尔字段用指针保留显式零值与 false
// （temperature/top_p/store/parallel_tool_calls/service_tier），否则 omitempty 会吞掉
// 客户端显式意图；stop/frequency_penalty/presence_penalty 不属于 Responses 请求 schema，
// 非缺省值必须在转换边界显式拒绝而非序列化丢失。
type ResponsesRequest struct {
	Model              string   `json:"model"`
	Instructions       string   `json:"instructions,omitempty"` // system prompt
	Input              any      `json:"input"`                  // string or []ResponsesItem
	PreviousResponseID string   `json:"previous_response_id,omitempty"`
	Store              *bool    `json:"store,omitempty"`               // default true
	MaxTokens          int      `json:"max_output_tokens,omitempty"`   // max output tokens
	Temperature        *float64 `json:"temperature,omitempty"`         // temperature
	TopP               *float64 `json:"top_p,omitempty"`               // top_p
	Stream             bool     `json:"stream,omitempty"`              // stream output
	StreamOptions      any      `json:"stream_options,omitempty"`      // stream options
	RawTools           []any    `json:"tools,omitempty"`               // §9 扁平工具定义
	ToolChoice         any      `json:"tool_choice,omitempty"`         // string or object
	ParallelToolCalls  *bool    `json:"parallel_tool_calls,omitempty"` // parallel tool calls

	Reasoning   any      `json:"reasoning,omitempty"`    // 推理配置（§2 effort 等）
	Text        any      `json:"text,omitempty"`         // 输出文本配置（§2 format/verbosity）
	Modalities  []string `json:"modalities,omitempty"`   // 输出模态
	Metadata    any      `json:"metadata,omitempty"`     // 附加元数据
	ServiceTier *string  `json:"service_tier,omitempty"` // 服务层级
	User        string   `json:"user,omitempty"`         // user identifier

	// TransformerMetadata is used to preserve original format info during request transformation
	// This field is not serialized to JSON, only valid within the same request processing chain
	TransformerMetadata map[string]any `json:"-"`
}

// ResponsesItem is a message item in Responses API
type ResponsesItem struct {
	ID        string          `json:"id,omitempty"`
	Type      string          `json:"type"`           // message, text, function_call, function_call_output
	Role      string          `json:"role,omitempty"` // user, assistant (for type=message)
	Status    string          `json:"status,omitempty"`
	Content   interface{}     `json:"content,omitempty"` // string or []ContentBlock
	Summary   interface{}     `json:"summary,omitempty"`
	ToolUse   *ToolUse        `json:"tool_use,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

// decodeJSONStringField 将 Responses wire 上的字符串字段解码为实际字符串（报告二 3/4）。
// Responses §3.2/§3.5：arguments/input/output 是 JSON 字符串，直接 string(RawMessage)
// 会带外层引号与转义（二次编码）；必须先按 JSON string 解码。
// 空/“null” 返回 ""（缺失由调用方按必填性判定）；非 string 的合法 JSON（兼容对象形态）
// 原样返回序列化文本；非法 JSON 返回 error，禁止静默回退。
func decodeJSONStringField(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("raw value is not valid JSON: %s", string(raw))
	}
	return string(raw), nil
}

// InputString 返回 custom_tool_call input 的实际字符串（Responses §3.5）。
func (r ResponsesItem) InputString() (string, error) {
	return decodeJSONStringField(r.Input)
}

// ArgumentsString 返回 function_call arguments 的实际 JSON 字符串（Responses §3.2）。
func (r ResponsesItem) ArgumentsString() (string, error) {
	return decodeJSONStringField(r.Arguments)
}

// OutputString 返回 output 类 item output 字段的实际字符串（Responses §3.3）。
func (r ResponsesItem) OutputString() (string, error) {
	return decodeJSONStringField(r.Output)
}

// ContentBlock is a content block for nested content arrays
type ContentBlock struct {
	Type string `json:"type"` // input_text, output_text
	Text string `json:"text"`
}

// ToolUse defines a tool usage
type ToolUse struct {
	ID    string      `json:"id"`
	Name  string      `json:"name"`
	Input interface{} `json:"input"`
}

// ResponsesResponse is the response structure for Responses API
// wire key 严格对齐 docs/responses-protocol.md §4：时间为 created_at、前序引用为 previous_response_id，
// 不得再输出 created/previous_id。Go 字段名保持 Created/PreviousID 不变，避免下游转换函数被连带改名。
// codex 直通日志 capture 快照经本结构体序列化，缺字段会导致 response.incomplete 终态的截断原因丢失。
type ResponsesResponse struct {
	ID                string                     `json:"id"`
	Object            string                     `json:"object"`
	Model             string                     `json:"model"`
	Output            []ResponsesItem            `json:"output"`
	Status            string                     `json:"status"` // completed, failed
	PreviousID        *string                    `json:"previous_response_id"`
	Usage             ResponsesUsage             `json:"usage"`
	Created           int64                      `json:"created_at"`
	Error             *ResponseError             `json:"error"`
	IncompleteDetails *ResponseIncompleteDetails `json:"incomplete_details"`

	// 以下为 §4 的可回显请求字段，capture 重序列化时必须原名保留
	Instructions      any            `json:"instructions"`
	MaxOutputTokens   *int           `json:"max_output_tokens"`
	ParallelToolCalls bool           `json:"parallel_tool_calls"`
	Reasoning         any            `json:"reasoning"`
	ServiceTier       string         `json:"service_tier"`
	Temperature       *float64       `json:"temperature"`
	ToolChoice        any            `json:"tool_choice"`
	Tools             []interface{}  `json:"tools"`
	TopP              *float64       `json:"top_p"`
	Truncated         bool           `json:"truncated"`
	User              any            `json:"user"`
	Metadata          map[string]any `json:"metadata"`
	Store             *bool          `json:"store"`
	Text              any            `json:"text"`
	Modalities        []string       `json:"modalities"`
	OutputText        string         `json:"output_text,omitempty"`
}

// ResponseError represents an error in a failed Responses API response
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

// ResponseIncompleteDetails represents the incomplete reason in an incomplete Responses API response
// Reason 枚举见 docs/responses-protocol.md §4：max_output_tokens、content_filter、steered 等
type ResponseIncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponsesStreamFrame preserves one SSE frame as emitted by the upstream stream.
type ResponsesStreamFrame struct {
	Event string          `json:"event,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Done  bool            `json:"done,omitempty"`
}

// ResponsesStreamCapture captures the full SSE sequence and the final completed response.
type ResponsesStreamCapture struct {
	Frames   []ResponsesStreamFrame `json:"frames"`
	Response *ResponsesResponse     `json:"response,omitempty"`
	Usage    *ResponsesUsage        `json:"-"`
}

// ResponsesUsage is the usage statistics for Responses API
// Supports detailed usage fields for both OpenAI Responses API and Claude API
// 五个顶层字段与两个 details 对象按 §6 全必填：details 指针不带 omitempty，
// 使生成端始终输出该对象（nil 输出 null），同时解析上游缺失时仍可为 nil 以识别畸形来源。
type ResponsesUsage struct {
	InputTokens         int                  `json:"input_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details"`
	OutputTokens        int                  `json:"output_tokens"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details"`
	TotalTokens         int                  `json:"total_tokens"`

	// Claude extension fields for cache creation statistics
	CacheCreationInputTokens   int    `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation5mInputTokens int    `json:"cache_creation_5m_input_tokens,omitempty"` // 5min TTL
	CacheCreation1hInputTokens int    `json:"cache_creation_1h_input_tokens,omitempty"` // 1hour TTL
	CacheReadInputTokens       int    `json:"cache_read_input_tokens,omitempty"`
	CacheTTL                   string `json:"cache_ttl,omitempty"` // "5m" | "1h" | "mixed"
}

// InputTokensDetails contains detailed input token statistics（Responses §6）
// cache_write_tokens 是 §6 必填的缓存写入明细，缺失会导致 capture 重序列化丢字段。
type InputTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// OutputTokensDetails contains detailed output token statistics（Responses §6）
type OutputTokensDetails struct {
	ReasoningTokens          int `json:"reasoning_tokens"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens,omitempty"`
	AudioTokens              int `json:"audio_tokens,omitempty"`
	TextTokens               int `json:"text_tokens,omitempty"`
}

// ResponsesStreamEvent is a streaming event for Responses API
type ResponsesStreamEvent struct {
	ID         string             `json:"id,omitempty"`
	ItemID     string             `json:"item_id,omitempty"`
	Item       *ResponsesItem     `json:"item,omitempty"`
	Model      string             `json:"model,omitempty"`
	Output     []ResponsesItem    `json:"output,omitempty"`
	Status     string             `json:"status,omitempty"`
	PreviousID string             `json:"previous_response_id,omitempty"`
	Usage      *ResponsesUsage    `json:"usage,omitempty"`
	Type       string             `json:"type,omitempty"` // delta, done
	Delta      interface{}        `json:"delta,omitempty"`
	Response   *ResponsesResponse `json:"response,omitempty"`
}

// ResponseStreamErrorEvent represents a streaming error event
type ResponseStreamErrorEvent struct {
	Type           string `json:"type"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	SequenceNumber int    `json:"sequence_number,omitempty"`
}

// ResponsesDelta is the streaming delta data
type ResponsesDelta struct {
	Type    string      `json:"type,omitempty"`
	Content interface{} `json:"content,omitempty"`
}
