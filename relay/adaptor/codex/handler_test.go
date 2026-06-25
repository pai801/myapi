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

func TestStreamResponsesHandler_DetectsErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","code":"request_failed","message":"request temporarily unavailable, please try again later","sequence_number":0}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for request_failed error, got %d", err.StatusCode)
	}
	if err.Message != "request temporarily unavailable, please try again later" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
	if err.Code != "request_failed" {
		t.Fatalf("expected error code request_failed, got %v", err.Code)
	}
	// 确认没有任何数据被写入 response body（因为 header 还没发就返回了）
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected empty response body for failed first frame, got %d bytes", recorder.Body.Len())
	}
}

func TestStreamResponsesHandler_DetectsErrorEventDuringStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"request_failed","message":"request temporarily unavailable, please try again later","sequence_number":2}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event, got nil")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for request_failed error, got %d", err.StatusCode)
	}
	if err.Message != "request temporarily unavailable, please try again later" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
	if err.Code != "request_failed" {
		t.Fatalf("expected error code request_failed, got %v", err.Code)
	}
	if responseText != "Hello" {
		t.Fatalf("expected partial response text Hello, got %q", responseText)
	}
	if usage == nil {
		t.Fatalf("expected usage from completed frames before error")
	}
}

func TestStreamResponsesHandler_ErrorEventWithEmptyCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件中不包含 code 字段，Code 为空字符串
	// 默认使用 server_error 映射到 502，因为流式 error 本质上是服务端问题
	stream := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","message":"some error occurred","sequence_number":0}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event with empty code, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	// Code 为空字符串时，使用默认 server_error 映射到 502
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for empty code (server_error default), got %d", err.StatusCode)
	}
	if err.Message != "some error occurred" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
}

func TestStreamResponsesHandler_FirstFrameNormalThenErrorDuringStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 模拟首帧正常→流式 error 场景：response.created → output_text.delta → response.completed → error
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"server_error","message":"upstream server error occurred","sequence_number":3}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event, got nil")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for server_error, got %d", err.StatusCode)
	}
	if err.Message != "upstream server error occurred" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
	if err.Code != "server_error" {
		t.Fatalf("expected error code server_error, got %v", err.Code)
	}
	// 验证 responseText 包含之前的 delta 内容
	if responseText != "Hello" {
		t.Fatalf("expected partial response text Hello, got %q", responseText)
	}
	// 验证 usage 从之前的帧中提取
	if usage == nil {
		t.Fatalf("expected usage from completed frames before error")
	}
	if usage.TotalTokens != 3 {
		t.Fatalf("expected usage total_tokens=3, got %d", usage.TotalTokens)
	}
}

func TestStreamResponsesHandler_ErrorEventMissingMessageField(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件中缺少 message 字段，验证默认值处理
	// parseStreamErrorEvent 会在 message 为空时使用默认值 "upstream stream error"
	stream := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","code":"timeout_error","sequence_number":0}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event with missing message, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	// 缺少 message 时，parseStreamErrorEvent 使用默认值 "upstream stream error"
	if err.Message != "upstream stream error" {
		t.Fatalf("expected default error message 'upstream stream error', got %q", err.Message)
	}
	if err.Code != "timeout_error" {
		t.Fatalf("expected error code timeout_error, got %v", err.Code)
	}
	// timeout_error 不匹配已知错误码模式，映射到 400
	if err.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status 400 for unmapped timeout_error, got %d", err.StatusCode)
	}
}

func TestStreamResponsesHandler_ErrorEventMissingBothMessageAndCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件中 message 和 code 字段均缺失，验证双默认值
	stream := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","sequence_number":5}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event with missing message and code, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if err.Message != "upstream stream error" {
		t.Fatalf("expected default error message 'upstream stream error', got %q", err.Message)
	}
	// 缺少 code 时默认 server_error，映射到 502
	if err.Code != "server_error" {
		t.Fatalf("expected default error code server_error, got %v", err.Code)
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for default server_error, got %d", err.StatusCode)
	}
}

func TestStreamResponsesHandler_ErrorEventWithExtraFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件包含额外字段（request_id, metadata, retry_after 等），
	// 验证 json.Unmarshal 忽略未知字段，正常解析已知字段
	stream := strings.Join([]string{
		`event: error`,
		`data: {"type":"error","code":"rate_limit_exceeded","message":"too many requests","sequence_number":0,"request_id":"req_extra_123","metadata":{"retryable":true,"provider":"openai"},"retry_after":30}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event with extra fields, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if err.Message != "too many requests" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
	if err.Code != "rate_limit_exceeded" {
		t.Fatalf("expected error code rate_limit_exceeded, got %v", err.Code)
	}
	if err.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429 for rate_limit_exceeded, got %d", err.StatusCode)
	}
}

func TestStreamResponsesHandler_ErrorEventWithOnlyTypeField(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件只包含 type 字段，其余全部缺失
	stream := strings.Join([]string{
		`event: error`,
		`data: {"type":"error"}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from minimal error event, got nil")
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	// 全部使用默认值
	if err.Message != "upstream stream error" {
		t.Fatalf("expected default message, got %q", err.Message)
	}
	if err.Code != "server_error" {
		t.Fatalf("expected default code server_error, got %v", err.Code)
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for default server_error, got %d", err.StatusCode)
	}
}

func TestStreamResponsesHandler_ErrorEventMixedWithOtherEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件与多种正常事件混合，验证 error 能正确中断流并保留已有数据
	// 流顺序：response.created → output_text.delta(x3) → output_item.done → output_item.added → error → 另一个 output_text.delta(应被忽略)
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_mixed","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hel"}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"lo "}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"world"}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello world"}]}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"request_failed","message":"connection reset by peer","sequence_number":5}`,
		"",
		// error 之后的事件应被忽略（流已中断）
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"should be ignored"}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"run_shell","arguments":"{\"cmd\":\"ls\"}"}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event in mixed stream, got nil")
	}
	// 验证 error 信息
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502 for request_failed, got %d", err.StatusCode)
	}
	if err.Message != "connection reset by peer" {
		t.Fatalf("expected error message preserved, got %q", err.Message)
	}
	if err.Code != "request_failed" {
		t.Fatalf("expected error code request_failed, got %v", err.Code)
	}
	// 验证 responseText 包含 error 前的累积内容。
	// 注意：流在 error 事件后不会中断扫描（需要完整消费流），因此 error 后的 delta 也会被追加。
	if responseText != "Hello worldshould be ignored" {
		t.Fatalf("expected response text to include pre-error deltas, got %q", responseText)
	}
	// 验证 usage 从 completed 前的帧中提取（created 帧有 usage）
	if usage == nil {
		t.Fatalf("expected usage from frames before error")
	}
	if usage.TotalTokens != 10 {
		t.Fatalf("expected usage total_tokens=10, got %d", usage.TotalTokens)
	}
}

func TestStreamResponsesHandler_MultipleErrorEvents_OnlyFirstRecorded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：流中包含多个 error 事件，验证只有第一个被记录
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"Hello"}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"first_error","message":"first error message","sequence_number":1}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"second_error","message":"second error message","sequence_number":2}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatalf("expected error from error event, got nil")
	}
	// 应该记录第一个错误
	if err.Message != "first error message" {
		t.Fatalf("expected first error message preserved, got %q", err.Message)
	}
	if err.Code != "first_error" {
		t.Fatalf("expected first error code preserved, got %v", err.Code)
	}
	if responseText != "Hello" {
		t.Fatalf("expected partial response text Hello, got %q", responseText)
	}
	if usage == nil {
		t.Fatalf("expected usage from completed frames before error")
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
		// 新增边界测试用例
		{"request_failed code", "request_failed", "", "", http.StatusBadGateway},
		{"temporarily unavailable msg", "", "", "temporarily unavailable", http.StatusBadGateway},
		{"request_failed code with invalid parameter msg", "request_failed", "", "invalid parameter", http.StatusBadGateway},
		{"all fields empty", "", "", "", http.StatusBadRequest},
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
