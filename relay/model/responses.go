package model

import "encoding/json"

// ============== Responses API Type Definitions ==============

// ResponsesRequest is the request structure for Responses API
type ResponsesRequest struct {
	Model              string                   `json:"model"`
	Instructions       string                   `json:"instructions,omitempty"` // system prompt
	Input              interface{}              `json:"input"`                  // string or []ResponsesItem
	PreviousResponseID string                   `json:"previous_response_id,omitempty"`
	Store              *bool                    `json:"store,omitempty"`               // default true
	MaxTokens          int                      `json:"max_output_tokens,omitempty"`   // max output tokens
	Temperature        float64                  `json:"temperature,omitempty"`         // temperature
	TopP               float64                  `json:"top_p,omitempty"`               // top_p
	FrequencyPenalty   float64                  `json:"frequency_penalty,omitempty"`   // frequency penalty
	PresencePenalty    float64                  `json:"presence_penalty,omitempty"`    // presence penalty
	Stream             bool                     `json:"stream,omitempty"`              // stream output
	Stop               interface{}              `json:"stop,omitempty"`                // stop sequences
	User               string                   `json:"user,omitempty"`                // user identifier
	StreamOptions      interface{}              `json:"stream_options,omitempty"`      // stream options
	Tools              []map[string]interface{} `json:"-"`                             // function tools
	RawTools           []interface{}            `json:"tools,omitempty"`               // raw tools definition
	ToolChoice         interface{}              `json:"tool_choice,omitempty"`         // string or object
	ParallelToolCalls  *bool                    `json:"parallel_tool_calls,omitempty"` // parallel tool calls

	// TransformerMetadata is used to preserve original format info during request transformation
	// This field is not serialized to JSON, only valid within the same request processing chain
	TransformerMetadata map[string]interface{} `json:"-"`
}

// ResponsesItem is a message item in Responses API
type ResponsesItem struct {
	ID        string      `json:"id,omitempty"`
	Type      string      `json:"type"`           // message, text, function_call, function_call_output
	Role      string      `json:"role,omitempty"` // user, assistant (for type=message)
	Status    string      `json:"status,omitempty"`
	Content   interface{} `json:"content,omitempty"` // string or []ContentBlock
	Summary   interface{} `json:"summary,omitempty"`
	ToolUse   *ToolUse    `json:"tool_use,omitempty"`
	CallID    string      `json:"call_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Namespace string      `json:"namespace,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

func (r ResponsesItem) InputString() string {
	return string(r.Input)
}

func (r ResponsesItem) ArgumentsString() string {
	return string(r.Arguments)
}

func (r ResponsesItem) OutputString() string {
	return string(r.Output)
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
type ResponsesResponse struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Output     []ResponsesItem `json:"output"`
	Status     string          `json:"status"` // completed, failed
	PreviousID string          `json:"previous_id,omitempty"`
	Usage      ResponsesUsage  `json:"usage"`
	Created    int64           `json:"created,omitempty"`
	Error      *ResponseError  `json:"error,omitempty"`
}

// ResponseError represents an error in a failed Responses API response
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
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
type ResponsesUsage struct {
	InputTokens         int                  `json:"input_tokens"`
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                  `json:"output_tokens"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                  `json:"total_tokens"`

	// Claude extension fields for cache creation statistics
	CacheCreationInputTokens   int    `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation5mInputTokens int    `json:"cache_creation_5m_input_tokens,omitempty"` // 5min TTL
	CacheCreation1hInputTokens int    `json:"cache_creation_1h_input_tokens,omitempty"` // 1hour TTL
	CacheReadInputTokens       int    `json:"cache_read_input_tokens,omitempty"`
	CacheTTL                   string `json:"cache_ttl,omitempty"` // "5m" | "1h" | "mixed"
}

// InputTokensDetails contains detailed input token statistics
type InputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputTokensDetails contains detailed output token statistics
type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesStreamEvent is a streaming event for Responses API
type ResponsesStreamEvent struct {
	ID         string             `json:"id,omitempty"`
	Item       *ResponsesItem     `json:"item,omitempty"`
	Model      string             `json:"model,omitempty"`
	Output     []ResponsesItem    `json:"output,omitempty"`
	Status     string             `json:"status,omitempty"`
	PreviousID string             `json:"previous_id,omitempty"`
	Usage      *ResponsesUsage    `json:"usage,omitempty"`
	Type       string             `json:"type,omitempty"` // delta, done
	Delta      interface{}        `json:"delta,omitempty"`
	Response   *ResponsesResponse `json:"response,omitempty"`
}

// ResponsesDelta is the streaming delta data
type ResponsesDelta struct {
	Type    string      `json:"type,omitempty"`
	Content interface{} `json:"content,omitempty"`
}
