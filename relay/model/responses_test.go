package model

import (
	"encoding/json"
	"strings"
	"testing"
)

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
				OutputTokens:  20,
				TotalTokens:   30,
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

	// completed 响应无 incomplete_details 时应随 omitempty 省略
	completed, err := json.Marshal(&ResponsesResponse{ID: "resp_ok", Status: "completed"})
	if err != nil {
		t.Fatalf("marshal completed response: %v", err)
	}
	if strings.Contains(string(completed), "incomplete_details") {
		t.Fatalf("did not expect incomplete_details on completed response, got %s", completed)
	}
}
