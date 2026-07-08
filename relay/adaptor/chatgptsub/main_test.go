package chatgptsub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupGin() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

func testMeta() *meta.Meta {
	return &meta.Meta{
		ChannelType: 53,
		ChannelId:   1,
		BaseURL:     "https://chatgpt.com",
		APIKey:      "test-session-token",
		Mode:        relaymode.ChatCompletions,
		APIType:     20,
	}
}

func boolPtr(b bool) *bool { return &b }

func ptrFloat(v float64) *float64 { return &v }

// ==================== ConvertRequest — Chat Completions → Responses ====================

func TestConvertRequest_ChatToResponses(t *testing.T) {
	req := &model.GeneralOpenAIRequest{
		Model: "gpt-4o",
		Messages: []model.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello!"},
			{
				Role:             "assistant",
				Content:          "Hi there!",
				ReasoningContent: "thinking step by step...",
			},
			{Role: "tool", Content: "weather result", ToolCallId: "call_tool_1"},
			{
				Role:    "assistant",
				Content: "",
				ToolCalls: []model.Tool{
					{
						Id:   "call_func_1",
						Type: "function",
						Function: model.Function{
							Name:      "get_weather",
							Arguments: `{"location":"NYC"}`,
						},
					},
				},
			},
		},
		MaxTokens: 1000,
		Tools: []model.Tool{
			{
				Type: "function",
				Function: model.Function{
					Name:        "get_weather",
					Description: "Get current weather",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
		},
		Temperature:      ptrFloat(0.7),
		TopP:             ptrFloat(1.0),
		Stream:           false,
		ToolChoice:       "auto",
		User:             "user_abc",
		ParallelTooCalls: boolPtr(true),
	}

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	adpt := &Adaptor{}
	result, err := adpt.ConvertRequest(c, relaymode.ChatCompletions, req)
	require.NoError(t, err)
	require.NotNil(t, result)

	respReq, ok := result.(*model.ResponsesRequest)
	require.True(t, ok, "expected *model.ResponsesRequest")

	// 顶层字段验证
	assert.Equal(t, "gpt-4o", respReq.Model)
	assert.Equal(t, 1000, respReq.MaxTokens)
	assert.Equal(t, 0.7, respReq.Temperature)
	assert.Equal(t, 1.0, respReq.TopP)
	assert.False(t, respReq.Stream)
	assert.Equal(t, "auto", respReq.ToolChoice)
	assert.Equal(t, "user_abc", respReq.User)
	require.NotNil(t, respReq.ParallelToolCalls)
	assert.True(t, *respReq.ParallelToolCalls)

	// input 数组验证
	// 5 条消息 → 6 个 item: system, user, assistant-reasoning, assistant-message, tool, assistant-function_call
	input, ok := respReq.Input.([]interface{})
	require.True(t, ok, "input should be []interface{}")
	require.Len(t, input, 6, "expected 6 input items")

	// item[0]: system → developer
	item0 := input[0].(map[string]interface{})
	assert.Equal(t, "message", item0["type"])
	assert.Equal(t, "developer", item0["role"])
	assert.Equal(t, "You are a helpful assistant.", item0["content"])

	// item[1]: user
	item1 := input[1].(map[string]interface{})
	assert.Equal(t, "message", item1["type"])
	assert.Equal(t, "user", item1["role"])
	assert.Equal(t, "Hello!", item1["content"])

	// item[2]: assistant reasoning (reasoning_content 在 content 前添加)
	item2 := input[2].(map[string]interface{})
	assert.Equal(t, "reasoning", item2["type"])
	summary, ok := item2["summary"].([]interface{})
	require.True(t, ok)
	require.Len(t, summary, 1)
	sumBlock := summary[0].(map[string]interface{})
	assert.Equal(t, "summary_text", sumBlock["type"])
	assert.Equal(t, "thinking step by step...", sumBlock["text"])

	// item[3]: assistant content (message)
	item3 := input[3].(map[string]interface{})
	assert.Equal(t, "message", item3["type"])
	assert.Equal(t, "assistant", item3["role"])
	contentArr, ok := item3["content"].([]interface{})
	require.True(t, ok)
	require.Len(t, contentArr, 1)
	block := contentArr[0].(map[string]interface{})
	assert.Equal(t, "output_text", block["type"])
	assert.Equal(t, "Hi there!", block["text"])

	// item[4]: tool (function_call_output)
	item4 := input[4].(map[string]interface{})
	assert.Equal(t, "function_call_output", item4["type"])
	assert.Equal(t, "call_tool_1", item4["call_id"])
	assert.Equal(t, "weather result", item4["output"])

	// item[5]: assistant function_call
	item5 := input[5].(map[string]interface{})
	assert.Equal(t, "function_call", item5["type"])
	assert.Equal(t, "call_func_1", item5["call_id"])
	assert.Equal(t, "get_weather", item5["name"])
	assert.Equal(t, `{"location":"NYC"}`, item5["arguments"])

	// tools 验证
	require.Len(t, respReq.RawTools, 1)
	tool0 := respReq.RawTools[0].(map[string]interface{})
	assert.Equal(t, "function", tool0["type"])
	func0, ok := tool0["function"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "get_weather", func0["name"])
	assert.Equal(t, "Get current weather", func0["description"])
}

// ==================== ConvertRequest — Responses 透传 ====================

func TestConvertRequest_ResponsesPassthrough(t *testing.T) {
	rawBody := `{"model":"gpt-4o","input":"hello world","max_output_tokens":100}`

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(rawBody))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.KeyRequestBody, []byte(rawBody))

	adpt := &Adaptor{}
	result, err := adpt.ConvertRequest(c, relaymode.Responses, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	bodyMap, ok := result.(map[string]interface{})
	require.True(t, ok, "expected map[string]interface{} for Responses passthrough")
	assert.Equal(t, "gpt-4o", bodyMap["model"])
	assert.Equal(t, "hello world", bodyMap["input"])
	assert.Equal(t, float64(100), bodyMap["max_output_tokens"])
}

// ==================== SetupRequestHeader 白名单过滤 ====================

func TestSetupRequestHeader_Whitelist(t *testing.T) {
	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamReq := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	// 白名单头（注意 conversation_id/session_id 使用下划线，与 allowedHeaders map 匹配）
	upstreamReq.Header.Set("Accept-Language", "en-US")
	upstreamReq.Header.Set("User-Agent", "test-agent")
	upstreamReq.Header.Set("Conversation_id", "conv-123")
	upstreamReq.Header.Set("Session_id", "sess-456")
	upstreamReq.Header.Set("X-Codex-Turn-State", "state-value")
	upstreamReq.Header.Set("X-Codex-Turn-Metadata", "meta-value")
	upstreamReq.Header.Set("Originator", "test-origin")
	// 非白名单头
	upstreamReq.Header.Set("X-Custom-Header", "should-be-filtered")
	upstreamReq.Header.Set("Sensitive-Header", "should-be-filtered")

	metaVal := testMeta()

	adpt := &Adaptor{}
	err := adpt.SetupRequestHeader(c, upstreamReq, metaVal)
	require.NoError(t, err)

	// 白名单头应保留
	assert.Equal(t, "en-US", upstreamReq.Header.Get("Accept-Language"))
	assert.Equal(t, "test-agent", upstreamReq.Header.Get("User-Agent"))
	assert.Equal(t, "conv-123", upstreamReq.Header.Get("Conversation_id"))
	assert.Equal(t, "sess-456", upstreamReq.Header.Get("Session_id"))
	assert.Equal(t, "state-value", upstreamReq.Header.Get("X-Codex-Turn-State"))
	assert.Equal(t, "meta-value", upstreamReq.Header.Get("X-Codex-Turn-Metadata"))
	assert.Equal(t, "test-origin", upstreamReq.Header.Get("Originator"))

	// 非白名单头应被过滤
	assert.Equal(t, "", upstreamReq.Header.Get("X-Custom-Header"))
	assert.Equal(t, "", upstreamReq.Header.Get("Sensitive-Header"))

	// 固定头被自动设置
	assert.Equal(t, "Bearer test-session-token", upstreamReq.Header.Get("Authorization"))
	assert.Equal(t, "application/json", upstreamReq.Header.Get("Content-Type"))
}

// ==================== 非流式 Chat Completions 响应 ====================

func TestNonStreamDoResponse_Chat(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	responsesBody := `{
		"id": "resp_test_123",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hello, world!"}]
			}
		],
		"status": "completed",
		"usage": {"input_tokens": 10, "output_tokens": 20, "total_tokens": 30},
		"created": 1234567890
	}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responsesBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode, "expected no error for successful response")
	require.NotNil(t, usage, "expected usage from completed response")

	// 验证 usage
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	assert.Equal(t, 30, usage.TotalTokens)

	// 验证响应体是 Chat Completions 格式
	var chatResp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &chatResp)
	require.NoError(t, err)

	assert.Equal(t, "chat.completion", chatResp["object"])
	assert.Equal(t, "gpt-4o", chatResp["model"])
	assert.Contains(t, chatResp["id"].(string), "chatcmpl-")

	choices, ok := chatResp["choices"].([]interface{})
	require.True(t, ok)
	require.Len(t, choices, 1)

	choice := choices[0].(map[string]interface{})
	assert.Equal(t, "stop", choice["finish_reason"])

	msg, ok := choice["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "Hello, world!", msg["content"])

	usageMap, ok := chatResp["usage"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, float64(10), usageMap["prompt_tokens"])
	assert.Equal(t, float64(20), usageMap["completion_tokens"])
	assert.Equal(t, float64(30), usageMap["total_tokens"])
}

func TestNonStreamDoResponse_ChatWithReasoningAndToolCalls(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	responsesBody := `{
		"id": "resp_reasoning_tool",
		"model": "gpt-4o",
		"output": [
			{
				"type": "reasoning",
				"summary": [{"type": "summary_text", "text": "I need to think about this..."}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Let me check the weather."}]
			},
			{
				"type": "function_call",
				"call_id": "call_abc",
				"name": "get_weather",
				"arguments": {"location":"NYC"}
			}
		],
		"status": "completed",
		"usage": {"input_tokens": 5, "output_tokens": 15, "total_tokens": 20},
		"created": 1234567890
	}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responsesBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	assert.Equal(t, 5, usage.PromptTokens)
	assert.Equal(t, 15, usage.CompletionTokens)
	assert.Equal(t, 20, usage.TotalTokens)

	var chatResp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &chatResp)
	require.NoError(t, err)

	choices := chatResp["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})

	assert.Equal(t, "Let me check the weather.", msg["content"])
	assert.Equal(t, "I need to think about this...", msg["reasoning_content"])

	toolCalls, ok := msg["tool_calls"].([]interface{})
	require.True(t, ok)
	require.Len(t, toolCalls, 1)

	tc := toolCalls[0].(map[string]interface{})
	assert.Equal(t, "call_abc", tc["id"])
	assert.Equal(t, "function", tc["type"])
	tcFunc := tc["function"].(map[string]interface{})
	assert.Equal(t, "get_weather", tcFunc["name"])
	assert.Equal(t, `{"location":"NYC"}`, tcFunc["arguments"])
}

func TestNonStreamDoResponse_ChatWithCachedTokens(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	responsesBody := `{
		"id": "resp_cached",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Cached response"}]
			}
		],
		"status": "completed",
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"total_tokens": 15,
			"input_tokens_details": {"cached_tokens": 8}
		},
		"created": 1234567890
	}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responsesBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 5, usage.CompletionTokens)
	assert.Equal(t, 15, usage.TotalTokens)

	require.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)

	var chatResp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &chatResp)
	require.NoError(t, err)

	usageMap := chatResp["usage"].(map[string]interface{})
	details := usageMap["prompt_tokens_details"].(map[string]interface{})
	assert.Equal(t, float64(8), details["cached_tokens"])
}

func TestNonStreamDoResponse_ChatErrorMapping(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	errBody := `{"error":{"code":"rate_limit_exceeded","message":"Rate limit hit","type":"rate_limit_error"}}`

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(errBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, usage)
	require.Nil(t, errWithCode)

	var errResp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)

	errObj, ok := errResp["error"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "Rate limit hit", errObj["message"])
	assert.Equal(t, "rate_limit_exceeded", errObj["code"])
	assert.Equal(t, "rate_limit_error", errObj["type"])
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// ==================== toolCallArgumentsString 单元测试 ====================

func TestToolCallArgumentsString(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{"nil", nil, "{}"},
		{"string", `{"a":1}`, `{"a":1}`},
		{"bytes", []byte(`{"b":2}`), `{"b":2}`},
		{"map", map[string]interface{}{"c": 3}, `{"c":3}`},
		{"int", 42, `42`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolCallArgumentsString(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ==================== extractUsageFromResponses 单元测试 ====================

func TestExtractUsageFromResponses(t *testing.T) {
	body := `{
		"id":"resp_usage_test",
		"model":"gpt-4o",
		"output":[],
		"status":"completed",
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}
	}`
	usage := extractUsageFromResponses([]byte(body))
	require.NotNil(t, usage)
	assert.Equal(t, 1, usage.PromptTokens)
	assert.Equal(t, 2, usage.CompletionTokens)
	assert.Equal(t, 3, usage.TotalTokens)
	assert.Nil(t, usage.PromptTokensDetails)
}

func TestExtractUsageFromResponses_WithCached(t *testing.T) {
	body := `{
		"id":"resp_cached_usage",
		"model":"gpt-4o",
		"output":[],
		"status":"completed",
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":7}}
	}`
	usage := extractUsageFromResponses([]byte(body))
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 5, usage.CompletionTokens)
	assert.Equal(t, 15, usage.TotalTokens)
	require.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 7, usage.PromptTokensDetails.CachedTokens)
}

func TestExtractUsageFromResponses_InvalidJSON(t *testing.T) {
	usage := extractUsageFromResponses([]byte("not json"))
	assert.Nil(t, usage)
}

// ==================== float64OrZero 单元测试 ====================

func TestFloat64OrZero(t *testing.T) {
	v := 3.14
	assert.Equal(t, 3.14, float64OrZero(&v))
	assert.Equal(t, 0.0, float64OrZero(nil))
}

// ==================== mapFinishReason 单元测试 ====================

func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		status   string
		expected string
	}{
		{"completed", "stop"},
		{"failed", "stop"},
		{"in_progress", ""},
		{"unknown", "stop"},
		{"", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := mapFinishReason(tt.status)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ==================== extractSessionHash 单元测试 ====================

func TestExtractSessionHash(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("conversation_id优先", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set("conversation_id", "conv-001")
		c.Request.Header.Set("session_id", "sess-001")
		assert.Equal(t, "conv-001", extractSessionHash(c))
	})

	t.Run("仅session_id", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set("session_id", "sess-002")
		assert.Equal(t, "sess-002", extractSessionHash(c))
	})

	t.Run("无任何标识", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		assert.Equal(t, "", extractSessionHash(c))
	})
}

// ==================== mapResponsesErrorToChatFormat 单元测试 ====================

func TestMapResponsesErrorToChatFormat(t *testing.T) {
	errBody := `{"error":{"code":"server_error","message":"Internal error","type":"server_error"}}`
	mapped := mapResponsesErrorToChatFormat([]byte(errBody))
	require.NotNil(t, mapped)

	var errResp map[string]interface{}
	err := json.Unmarshal(mapped, &errResp)
	require.NoError(t, err)

	errObj := errResp["error"].(map[string]interface{})
	assert.Equal(t, "Internal error", errObj["message"])
	assert.Equal(t, "server_error", errObj["code"])
	assert.Equal(t, "server_error", errObj["type"])
}

func TestMapResponsesErrorToChatFormat_NoError(t *testing.T) {
	normalBody := `{"id":"resp_123","status":"completed"}`
	mapped := mapResponsesErrorToChatFormat([]byte(normalBody))
	assert.Equal(t, normalBody, string(mapped), "non-error body should be returned as-is")
}

func TestMapResponsesErrorToChatFormat_EmptyType(t *testing.T) {
	// 注意：现有代码中 chatErr["type"] 检查的是顶层 map 的 type key，
	// 而 type 实际在 chatErr["error"] 嵌套 map 中，因此空 type 不会被默认填充。
	// 此测试验证当前的实际行为。
	errBody := `{"error":{"code":"rate_limit","message":"too many requests"}}`
	mapped := mapResponsesErrorToChatFormat([]byte(errBody))
	require.NotNil(t, mapped)

	// 验证空 type 不会被自动填充为 invalid_request_error（现有代码行为）
	var errResp map[string]interface{}
	err := json.Unmarshal(mapped, &errResp)
	require.NoError(t, err)
	// 修复后，空 type 应被填充为 invalid_request_error
	assert.Equal(t, "invalid_request_error", errResp["error"].(map[string]interface{})["type"])
}

// ==================== convertResponsesToChat 集成验证 ====================

func TestConvertResponsesToChat_Basic(t *testing.T) {
	responsesBody := `{
		"id": "resp_conv_test",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hello!"}]
			}
		],
		"status": "completed",
		"usage": {"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
		"created": 999999
	}`

	chatBytes := convertResponsesToChat([]byte(responsesBody), "gpt-4o")
	require.NotNil(t, chatBytes)

	var chatResp map[string]interface{}
	err := json.Unmarshal(chatBytes, &chatResp)
	require.NoError(t, err)

	assert.Equal(t, "chatcmpl-resp_conv_test", chatResp["id"])
	assert.Equal(t, "chat.completion", chatResp["object"])
	assert.Equal(t, "gpt-4o", chatResp["model"])
	assert.Equal(t, float64(999999), chatResp["created"])
}

// ==================== SSE 流式 Chat Completions ====================

func TestSSEStream_ChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sseData := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_stream_1","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello "}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"World!"}`,
		``,
		`event: response.reasoning.delta`,
		`data: {"type":"response.reasoning.delta","delta":"thinking..."}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_stream_1","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}}`,
		``,
		`data: [DONE]`,
		``,
	}

	streamStr := strings.Join(sseData, "\n")

	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(streamStr)),
	}

	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode, "expected no error for streaming response")
	require.NotNil(t, usage, "expected usage from streaming completed response")

	assert.Equal(t, 5, usage.PromptTokens)
	assert.Equal(t, 10, usage.CompletionTokens)
	assert.Equal(t, 15, usage.TotalTokens)

	output := w.Body.String()
	t.Logf("SSE output:\n%s", output)

	assert.Contains(t, output, `"content":"Hello "`)
	assert.Contains(t, output, `"content":"World!"`)
	assert.Contains(t, output, `"reasoning_content":"thinking..."`)
	assert.Contains(t, output, `"finish_reason":"stop"`)
	assert.Contains(t, output, `"prompt_tokens":5`)
	assert.Contains(t, output, `"completion_tokens":10`)
	assert.Contains(t, output, `"total_tokens":15`)
	assert.Contains(t, output, "[DONE]")
}

func TestSSEStream_ChatCompletions_WithToolCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sseData := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_tool_stream","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_fc1","name":"get_weather","arguments":"{}","status":"in_progress"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"location\":"}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"NYC\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_fc1","name":"get_weather","arguments":"{\"location\":\"NYC\"}","status":"completed"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_tool_stream","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		``,
		`data: [DONE]`,
		``,
	}

	streamStr := strings.Join(sseData, "\n")

	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(streamStr)),
	}

	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode, "expected no error for streaming tool call")
	require.NotNil(t, usage, "expected usage")

	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 15, usage.TotalTokens)

	output := w.Body.String()
	t.Logf("SSE tool call output:\n%s", output)

	assert.Contains(t, output, `"tool_calls"`)
	assert.Contains(t, output, `"call_fc1"`)
	assert.Contains(t, output, `"get_weather"`)
	assert.Contains(t, output, `"arguments"`)
	assert.Contains(t, output, `"finish_reason":"stop"`)
	assert.Contains(t, output, "[DONE]")
}

func TestSSEStream_ChatCompletions_ErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sseData := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_err","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
		``,
		`event: error`,
		`data: {"type":"error","code":"internal_error","message":"Something went wrong"}`,
		``,
		`data: [DONE]`,
		``,
	}

	streamStr := strings.Join(sseData, "\n")

	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(streamStr)),
	}

	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	// 错误事件本身不返回 ErrorWithStatusCode（被写入 SSE 流）
	require.Nil(t, errWithCode)
	require.Nil(t, usage)

	output := w.Body.String()
	t.Logf("SSE error output:\n%s", output)

	assert.Contains(t, output, `"error"`)
	assert.Contains(t, output, `"Something went wrong"`)
	assert.Contains(t, output, `"internal_error"`)
	assert.Contains(t, output, "[DONE]")
}
