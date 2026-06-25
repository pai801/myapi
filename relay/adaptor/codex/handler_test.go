package codex

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/ctxkey"
)

func TestStreamResponsesHandler_CapturesStructuredFramesAndCollapsesOutputTextDelta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hel"}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		"",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"ignore me"}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}
	if usage == nil {
		t.Fatalf("expected usage from completed stream response")
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body to be stored in context")
	}

	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}

	frames, ok := capture["frames"].([]interface{})
	if !ok {
		t.Fatalf("expected frames array, got %#v", capture["frames"])
	}
	if len(frames) != 4 {
		t.Fatalf("expected capture to keep 4 frames without pure delta noise, got %d: %#v", len(frames), frames)
	}

	first, ok := frames[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected first frame object, got %#v", frames[0])
	}
	if first["event"] != "response.created" {
		t.Fatalf("expected first frame event preserved, got %#v", first["event"])
	}
	if _, ok := first["data"].(map[string]interface{}); !ok {
		t.Fatalf("expected first frame data to stay structured JSON object, got %#v", first["data"])
	}

	deltaFound := false
	outputItemFound := false
	for _, frame := range frames {
		fm := frame.(map[string]interface{})
		if fm["event"] == "response.output_item.done" {
			outputItemFound = true
		}
		if fm["event"] == "response.output_text.delta" {
			deltaFound = true
			data := fm["data"].(map[string]interface{})
			if data["delta"] != "Hello" {
				t.Fatalf("expected delta fragments to be aggregated into Hello, got %#v", data["delta"])
			}
		}
		if fm["event"] == "response.reasoning_summary_text.delta" {
			t.Fatalf("did not expect reasoning summary delta frame to be preserved: %#v", fm)
		}
	}
	if !deltaFound {
		t.Fatalf("expected one aggregated output_text.delta frame in capture")
	}
	if !outputItemFound {
		t.Fatalf("expected output_item.done frame to be preserved for fallback")
	}

	respJSON, ok := capture["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected completed response in capture, got %#v", capture["response"])
	}
	if _, ok := capture["output_items"]; ok {
		t.Fatalf("did not expect separate output_items array in serialized capture")
	}
	if respJSON["id"] != "resp_1" {
		t.Fatalf("expected completed response id preserved, got %#v", respJSON["id"])
	}
	if respJSON["status"] != "completed" {
		t.Fatalf("expected completed status preserved, got %#v", respJSON["status"])
	}
	if respJSON["usage"].(map[string]interface{})["total_tokens"] != float64(3) {
		t.Fatalf("expected usage preserved, got %#v", respJSON["usage"])
	}
}

func TestStreamResponsesHandler_PreservesToolSearchCallWithObjectArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ts_1","type":"tool_search_call","status":"in_progress","call_id":"call_1","name":"search_docs","arguments":{"query":"codex","top_k":3}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"ts_1","type":"tool_search_call","status":"completed","call_id":"call_1","name":"search_docs","arguments":{"query":"codex","top_k":3}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_tool_search","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":2,"output_tokens":4,"total_tokens":6}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}
	if usage == nil || usage.TotalTokens != 6 {
		t.Fatalf("expected usage to be preserved, got %#v", usage)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body to be stored in context")
	}

	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}

	respJSON := capture["response"].(map[string]interface{})
	output := respJSON["output"].([]interface{})
	if len(output) != 1 {
		t.Fatalf("expected tool_search_call to be preserved in output, got %#v", output)
	}
	item := output[0].(map[string]interface{})
	if item["type"] != "tool_search_call" {
		t.Fatalf("expected preserved output item type tool_search_call, got %#v", item["type"])
	}
	if item["name"] != "search_docs" {
		t.Fatalf("expected preserved tool name, got %#v", item["name"])
	}
}

func TestStreamResponsesHandler_SkipsUnknownToolItemWithoutBreakingCompletedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"unk_1","type":"mystery_tool_call","status":"in_progress","call_id":"call_x","name":"mystery","arguments":{"foo":"bar"}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_unknown","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("expected usage to be preserved, got %#v", usage)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body to be stored in context")
	}

	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}

	respJSON := capture["response"].(map[string]interface{})
	output := respJSON["output"].([]interface{})
	if len(output) != 0 {
		t.Fatalf("expected unknown item to be skipped from output, got %#v", output)
	}
}

func TestStreamResponsesHandler_MixedToolsSurviveBadItem(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_fc","name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_fc","name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ctc_1","type":"custom_tool_call","status":"in_progress","call_id":"call_ctc","name":"apply_patch","input":"patch text"}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_ctc","name":"apply_patch","input":"patch text"}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"ts_1","type":"tool_search_call","status":"in_progress","call_id":"call_ts","name":"search_docs","arguments":{"query":"codex","top_k":3}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"ts_1","type":"tool_search_call","status":"completed","call_id":"call_ts","name":"search_docs","arguments":{"query":"codex","top_k":3}}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"bad_1","type":"unknown_tool_call","status":"in_progress","call_id":"call_bad","name":"broken","arguments":{"x":1}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_mixed","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected usage to be preserved, got %#v", usage)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body to be stored in context")
	}

	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}

	respJSON := capture["response"].(map[string]interface{})
	output := respJSON["output"].([]interface{})
	if len(output) != 3 {
		t.Fatalf("expected 3 preserved output items, got %#v", output)
	}
	if output[0].(map[string]interface{})["type"] != "function_call" {
		t.Fatalf("expected first item function_call, got %#v", output[0])
	}
	if output[1].(map[string]interface{})["type"] != "custom_tool_call" {
		t.Fatalf("expected second item custom_tool_call, got %#v", output[1])
	}
	if output[2].(map[string]interface{})["type"] != "tool_search_call" {
		t.Fatalf("expected third item tool_search_call, got %#v", output[2])
	}
}

func TestStreamResponsesHandler_DetectsResponseFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// rate_limit_exceeded -> 429 应该触发重试
	stream := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_fail_1","model":"gpt-4o","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded for user, please retry later"}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from response.failed event, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from failed response, got %#v", usage)
	}
	if err.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429 for rate_limit error, got %d", err.StatusCode)
	}
	if err.Message != "Concurrency limit exceeded for user, please retry later" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
	if err.Code != "rate_limit_exceeded" {
		t.Fatalf("expected error code rate_limit_exceeded, got %v", err.Code)
	}
	// 确认没有任何数据被写入 response body（因为 header 还没发就返回了）
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected empty response body for failed first frame, got %d bytes", recorder.Body.Len())
	}
}

func TestStreamResponsesHandler_ResponseFailedServerErrorMapsTo5xx(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_fail_2","model":"gpt-4o","status":"failed","output":[],"error":{"code":"server_error","message":"internal server error"}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from response.failed event, got nil")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for server_error, got %d", err.StatusCode)
	}
}

func TestStreamResponsesHandler_ResponseFailedInvalidRequestMapsTo4xx(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_fail_3","model":"gpt-4o","status":"failed","output":[],"error":{"code":"invalid_request_error","message":"invalid parameter"}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from response.failed event, got nil")
	}
	if err.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status 400 for invalid_request, got %d", err.StatusCode)
	}
}

func TestMapFailedErrorToStatusCode(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		errType  string
		message  string
		expected int
	}{
		{"rate_limit_exceeded code", "rate_limit_exceeded", "", "", http.StatusTooManyRequests},
		{"rate limit in message", "", "", "Rate limit exceeded", http.StatusTooManyRequests},
		{"concurrency limit", "", "", "Concurrency limit exceeded", http.StatusTooManyRequests},
		{"too many requests", "", "", "too many requests", http.StatusTooManyRequests},
		{"server_error code", "server_error", "", "", http.StatusBadGateway},
		{"server_error type", "", "server_error", "", http.StatusBadGateway},
		{"internal server error msg", "", "", "internal server error", http.StatusBadGateway},
		{"request timeout msg", "", "", "request timeout", http.StatusBadGateway},
		{"timed out msg", "", "", "timed out", http.StatusBadGateway},
		{"deadline exceeded msg", "", "", "deadline exceeded", http.StatusBadGateway},
		{"connection timeout msg", "", "", "connection timeout", http.StatusBadGateway},
		{"unavailable type", "", "unavailable", "", http.StatusBadGateway},
		{"service unavailable msg", "", "", "service unavailable", http.StatusBadGateway},
		{"bad gateway msg", "", "", "bad gateway", http.StatusBadGateway},
		{"internal_error type", "", "internal_error", "", http.StatusBadGateway},
		// "timeout" 单独出现不是服务端错误，不应误匹配 502
		{"bare timeout word no match", "", "", "timeout parameter is invalid", http.StatusBadRequest},
		{"invalid_request", "invalid_request_error", "", "", http.StatusBadRequest},
		{"unknown error", "unknown_code", "unknown_type", "some message", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapFailedErrorToStatusCode(tt.code, tt.errType, tt.message)
			if result != tt.expected {
				t.Fatalf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}
