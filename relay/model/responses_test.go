package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// responsesCanonicalResponseSample 依据 docs/responses-protocol.md §4/§6 构造的严格合法 Response 样本：
// 时间字段为 created_at、前序引用为 previous_response_id，usage 五个顶层字段与两个 details 对象齐备。
const responsesCanonicalResponseSample = `{
  "id": "resp_canon_1",
  "object": "response",
  "created_at": 1700000000,
  "status": "completed",
  "error": null,
  "incomplete_details": null,
  "instructions": "be concise",
  "max_output_tokens": 4096,
  "model": "gpt-5-codex",
  "output": [],
  "parallel_tool_calls": true,
  "previous_response_id": "resp_prev_1",
  "reasoning": {"effort": "medium"},
  "service_tier": "default",
  "temperature": 1,
  "tool_choice": "auto",
  "tools": [],
  "top_p": 1,
  "truncated": false,
  "usage": {
    "input_tokens": 100,
    "input_tokens_details": {"cached_tokens": 40, "cache_write_tokens": 12},
    "output_tokens": 50,
    "output_tokens_details": {"reasoning_tokens": 10, "accepted_prediction_tokens": 2, "rejected_prediction_tokens": 1, "audio_tokens": 3, "text_tokens": 34},
    "total_tokens": 150
  },
  "user": null,
  "metadata": {},
  "store": true,
  "text": {"format": {"type": "text"}},
  "modalities": ["text"],
  "output_text": "hi"
}`

// TestResponsesResponseCanonicalWireRoundTrip 锁定共享 Response wire model 的协议键名（报告一 P1-11）：
// 合法 §4/§6 样本必须能被严格解码，并经 Unmarshal→Marshal 往返不改名、不丢字段。
func TestResponsesResponseCanonicalWireRoundTrip(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(responsesCanonicalResponseSample))
	dec.DisallowUnknownFields()
	var resp ResponsesResponse
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("canonical §4/§6 sample must decode strictly into ResponsesResponse, got %v", err)
	}

	data, err := json.Marshal(&resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var want map[string]any
	if err := json.Unmarshal([]byte(responsesCanonicalResponseSample), &want); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal re-serialized response: %v (json=%s)", err, data)
	}

	if !reflect.DeepEqual(want, got) {
		t.Fatalf("canonical response must round-trip unchanged\nwant %#v\n got %#v\njson=%s", want, got, data)
	}

	for _, illegalKey := range []string{"created", "previous_id"} {
		if _, ok := got[illegalKey]; ok {
			t.Fatalf("protocol-illegal wire key %q must not be emitted, got %s", illegalKey, data)
		}
	}

	if got["created_at"] != float64(1700000000) {
		t.Fatalf("expected created_at bound to protocol key, got %#v", got["created_at"])
	}
	if got["previous_response_id"] != "resp_prev_1" {
		t.Fatalf("expected previous_response_id bound to protocol key, got %#v", got["previous_response_id"])
	}

	// usage 的两个 details 对象在生成 Responses 输出时必须始终存在（§6 全必填）
	usageJSON, err := json.Marshal(&ResponsesUsage{})
	if err != nil {
		t.Fatalf("marshal empty usage: %v", err)
	}
	var usage map[string]any
	if err := json.Unmarshal(usageJSON, &usage); err != nil {
		t.Fatalf("unmarshal empty usage: %v", err)
	}
	for _, key := range []string{"input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details", "total_tokens"} {
		if _, ok := usage[key]; !ok {
			t.Fatalf("usage field %q must always be serialized per §6, got %s", key, usageJSON)
		}
	}
}

func TestResponsesStreamCaptureMarshal(t *testing.T) {
	capture := ResponsesStreamCapture{
		Frames: []ResponsesStreamFrame{{
			Event: "response.created",
			Data:  json.RawMessage(`{"id":"evt_1","type":"response.created"}`),
		}, {
			Event: "response.completed",
			Data:  json.RawMessage(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`),
		}},
		Response: &ResponsesResponse{
			ID:     "resp_1",
			Model:  "gpt-4o",
			Status: "completed",
			Output: []ResponsesItem{{Type: "message"}},
			Usage: ResponsesUsage{
				InputTokens:  10,
				OutputTokens: 20,
				TotalTokens:  30,
			},
		},
	}

	data, err := json.Marshal(capture)
	if err != nil {
		t.Fatalf("marshal capture: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}

	if _, ok := got["frames"]; !ok {
		t.Fatalf("expected frames in serialized capture")
	}
	frames, ok := got["frames"].([]interface{})
	if !ok || len(frames) != 2 {
		t.Fatalf("expected 2 frames in serialized capture, got %#v", got["frames"])
	}

	first := frames[0].(map[string]interface{})
	if first["event"] != "response.created" {
		t.Fatalf("expected first frame event preserved, got %#v", first["event"])
	}
	firstData, ok := first["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected first frame data to be structured JSON object, got %#v", first["data"])
	}
	if firstData["id"] != "evt_1" || firstData["type"] != "response.created" {
		t.Fatalf("expected first frame object preserved, got %#v", firstData)
	}

	second := frames[1].(map[string]interface{})
	if second["event"] != "response.completed" {
		t.Fatalf("expected completed frame event preserved, got %#v", second["event"])
	}
	secondData, ok := second["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected completed frame data to be structured JSON object, got %#v", second["data"])
	}
	if secondData["type"] != "response.completed" {
		t.Fatalf("expected completed type preserved, got %#v", secondData["type"])
	}

	resp, ok := got["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected response in serialized capture")
	}
	if resp["id"] != "resp_1" {
		t.Fatalf("expected completed response preserved, got %#v", resp["id"])
	}
	output, ok := resp["output"].([]interface{})
	if !ok || len(output) != 1 {
		t.Fatalf("expected response.output to remain present, got %#v", resp["output"])
	}
	if _, ok := got["text"]; ok {
		t.Fatalf("did not expect redundant text field in serialized capture")
	}
}

func TestResponsesResponseIncompleteDetailsRoundTrip(t *testing.T) {
	// 终态事件 payload 经 ResponsesStreamEvent 反序列化进 ResponsesResponse，缺字段会在 capture 快照中丢失截断原因
	payload := `{"type":"response.incomplete","response":{"id":"resp_inc","model":"gpt-4o","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"incomplete_details":{"reason":"max_output_tokens"}}}`

	var event ResponsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatalf("unmarshal stream event: %v", err)
	}
	if event.Response == nil || event.Response.IncompleteDetails == nil {
		t.Fatalf("expected incomplete_details parsed into ResponsesResponse, got %#v", event.Response)
	}
	if event.Response.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("expected truncation reason preserved, got %q", event.Response.IncompleteDetails.Reason)
	}

	data, err := json.Marshal(event.Response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(data), `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("expected incomplete_details re-serialized, got %s", data)
	}

	// §4 规定 error/incomplete_details 为 Response 对象必填键：completed 无截断原因时输出 null，
	// 而不是随 omitempty 消失（旧断言违反协议，按协议名重写）。
	completed, err := json.Marshal(&ResponsesResponse{ID: "resp_ok", Status: "completed"})
	if err != nil {
		t.Fatalf("marshal completed response: %v", err)
	}
	var completedMap map[string]json.RawMessage
	if err := json.Unmarshal(completed, &completedMap); err != nil {
		t.Fatalf("unmarshal completed response: %v", err)
	}
	raw, ok := completedMap["incomplete_details"]
	if !ok {
		t.Fatalf("expected incomplete_details key present as protocol-required field, got %s", completed)
	}
	if string(raw) != "null" {
		t.Fatalf("expected completed response to carry incomplete_details null, got %s", completed)
	}
	if _, ok := completedMap["error"]; !ok {
		t.Fatalf("expected error key present as protocol-required field, got %s", completed)
	}
}
