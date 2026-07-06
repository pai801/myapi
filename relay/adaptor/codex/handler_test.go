package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (r *flushRecorder) Flush() {
	r.flushCount++
	r.ResponseRecorder.Flush()
}

type blockingReadCloser struct {
	started chan struct{}
	release chan struct{}
	data    string
	read    bool
}

func (r *blockingReadCloser) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		close(r.started)
		<-r.release
		return copy(p, r.data), io.EOF
	}
	return 0, io.EOF
}

func (r *blockingReadCloser) Close() error { return nil }

type errReader struct {
	chunks []string
	idx    int
	err    error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func (r *errReader) Close() error {
	return nil
}

func parseResponsesEventData(events []string, eventName string) []map[string]interface{} {
	parsed := make([]map[string]interface{}, 0)
	for _, evt := range events {
		if !strings.Contains(evt, "event: "+eventName) {
			continue
		}
		idx := strings.Index(evt, "data: ")
		if idx < 0 {
			continue
		}
		payload := strings.TrimSpace(evt[idx+len("data: "):])
		var envelope map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			continue
		}
		parsed = append(parsed, envelope)
	}
	return parsed
}

func TestReadSSEEvent_AcceptsFieldWithoutSpace_Done(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("data:[DONE]\n\n"))

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent returned error: %v", err)
	}
	if event.Data != "[DONE]" {
		t.Fatalf("expected done payload, got %q", event.Data)
	}
	if !event.Done {
		t.Fatalf("expected done flag true")
	}
}

func TestReadSSEEvent_AcceptsFieldWithoutSpace_CompletedPayload(t *testing.T) {
	payload := `{"type":"response.completed","response":{"id":"resp_no_space","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	r := bufio.NewReader(strings.NewReader("data:" + payload + "\n\n"))

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent returned error: %v", err)
	}
	if event.Data != payload {
		t.Fatalf("expected completed payload preserved, got %q", event.Data)
	}
	if event.Done {
		t.Fatalf("expected non-done payload")
	}
}

func TestReadSSEEvent_AcceptsEventWithoutSpace(t *testing.T) {
	payload := `{"type":"response.completed"}`
	r := bufio.NewReader(strings.NewReader("event:response.completed\ndata:" + payload + "\n\n"))

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent returned error: %v", err)
	}
	if event.Event != "response.completed" {
		t.Fatalf("expected event name preserved, got %q", event.Event)
	}
	if event.Data != payload {
		t.Fatalf("expected payload preserved, got %q", event.Data)
	}
}

func TestReadSSEEvent_ReturnsFinalEventOnEOFWithoutTrailingBlankLine(t *testing.T) {
	payload := `{"type":"response.completed","response":{"id":"resp_eof","status":"completed"}}`
	r := bufio.NewReader(strings.NewReader("event: response.completed\ndata: " + payload))

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("expected final event returned before eof, got %v", err)
	}
	if event.Event != "response.completed" {
		t.Fatalf("expected completed event, got %q", event.Event)
	}
	if event.Data != payload {
		t.Fatalf("expected payload preserved, got %q", event.Data)
	}
	if event.Done {
		t.Fatalf("expected completed payload not marked as done")
	}

	_, err = readSSEEvent(r, maxSSEEventBytes)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected eof after final event consumed, got %v", err)
	}
}

func TestReadSSEEvent_PreservesEventField(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("event: error\ndata: {\"message\":\"boom\"}\n\n"))

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent returned error: %v", err)
	}
	if event.Event != "error" {
		t.Fatalf("expected event field preserved, got %q", event.Event)
	}
}

func TestReadSSEEvent_ReturnsEOFAfterCommentTerminatedEvent(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(": comment\n\n"))

	_, err := readSSEEvent(r, maxSSEEventBytes)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected eof for comment-only stream, got %v", err)
	}
}

func TestReadSSEEvent_ReturnsDoneOnEOFWithoutTrailingBlankLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("data: [DONE]"))

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("expected done event returned before eof, got %v", err)
	}
	if event.Data != "[DONE]" || !event.Done {
		t.Fatalf("expected done event preserved, got %#v", event)
	}
}

func TestStreamResponsesHandler_FlushesHeadersBeforeFirstEventRead(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	body := &blockingReadCloser{
		started: make(chan struct{}),
		release: make(chan struct{}),
		data:    "data: [DONE]\n\n",
	}
	resp := &http.Response{StatusCode: http.StatusOK, Body: body}

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		_, _, _ = StreamResponsesHandler(c, resp)
	}()

	<-body.started
	if recorder.flushCount == 0 {
		t.Fatalf("expected headers to flush before first frame read completes")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status written before first frame read completes, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content type before first frame read completes, got %q", got)
	}

	close(body.release)
	<-doneCh
}

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

func TestStreamResponsesHandler_CompletedPayloadRemainsCanonicalForCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_fallback","role":"assistant","content":[{"type":"output_text","text":"fallback text"}]}}`,
		"",
		`event: response.completed`,
		`data: {"response":{"id":"resp_canonical","model":"gpt-4o","output":[{"type":"message","id":"msg_final","role":"assistant","content":[{"type":"output_text","text":"canonical text"}]}],"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
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
		t.Fatalf("expected usage from canonical completed response")
	}
	if usage.TotalTokens != 0 {
		t.Fatalf("expected zero-token usage preserved from canonical completed response, got %#v", usage)
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
	if respJSON["id"] != "resp_canonical" {
		t.Fatalf("expected canonical completed response id, got %#v", respJSON["id"])
	}
	output := respJSON["output"].([]interface{})
	if len(output) != 1 {
		t.Fatalf("expected canonical completed output preserved, got %#v", output)
	}
	item := output[0].(map[string]interface{})
	if item["id"] != "msg_final" {
		t.Fatalf("expected canonical completed output item, got %#v", item)
	}
	usageJSON := respJSON["usage"].(map[string]interface{})
	if usageJSON["total_tokens"] != float64(0) {
		t.Fatalf("expected zero-token usage preserved in capture, got %#v", usageJSON)
	}
	if !strings.Contains(recorder.Body.String(), `data: {"response":{"id":"resp_canonical"`) {
		t.Fatalf("expected canonical completed payload to be forwarded unchanged, got %q", recorder.Body.String())
	}
}

func TestStreamResponsesHandler_SuccessTerminalOrdering(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_terminal","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_terminal","role":"assistant","content":[{"type":"output_text","text":"terminal text"}]}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_terminal","model":"gpt-4o","output":[{"type":"message","id":"msg_terminal_final","role":"assistant","content":[{"type":"output_text","text":"canonical terminal text"}]}],"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}
	if responseText != "" {
		t.Fatalf("expected no direct response text on canonical completed path, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected completed usage preserved, got %#v", usage)
	}

	body := recorder.Body.String()
	completedMarker := `"type":"response.completed"`
	completedMarkerIndex := strings.Index(body, completedMarker)
	if completedMarkerIndex < 0 {
		t.Fatalf("expected forwarded body to contain response.completed payload, got %q", body)
	}
	if strings.Count(body, completedMarker) != 1 {
		t.Fatalf("expected exactly one forwarded response.completed payload, got body %q", body)
	}
	doneMarkerIndex := strings.Index(body, `data: [DONE]`)
	if doneMarkerIndex < 0 {
		t.Fatalf("expected forwarded body to contain done marker, got %q", body)
	}
	if completedMarkerIndex >= doneMarkerIndex {
		t.Fatalf("expected response.completed payload before [DONE], got %q", body)
	}

	completedPayload := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, completedMarker) {
			completedPayload = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if completedPayload == "" {
		t.Fatalf("expected to extract response.completed data payload from forwarded body, got %q", body)
	}

	var completedEnvelope struct {
		Type     string                   `json:"type"`
		Response *model.ResponsesResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(completedPayload), &completedEnvelope); err != nil {
		t.Fatalf("expected completed payload to stay parseable, got error: %v; payload=%s", err, completedPayload)
	}
	if completedEnvelope.Type != "response.completed" {
		t.Fatalf("expected completed envelope type preserved, got %q", completedEnvelope.Type)
	}
	if completedEnvelope.Response == nil {
		t.Fatalf("expected completed payload to include final response object")
	}
	if completedEnvelope.Response.ID != "resp_terminal" {
		t.Fatalf("expected final response id preserved, got %q", completedEnvelope.Response.ID)
	}
	if completedEnvelope.Response.Status != "completed" {
		t.Fatalf("expected final response status completed, got %q", completedEnvelope.Response.Status)
	}
	if len(completedEnvelope.Response.Output) != 1 {
		t.Fatalf("expected final response output preserved, got %#v", completedEnvelope.Response.Output)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body to be stored in context")
	}

	var capture struct {
		Response *model.ResponsesResponse `json:"response"`
		Frames   []struct {
			Event string `json:"event"`
			Done  bool   `json:"done"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	if capture.Response == nil {
		t.Fatalf("expected final response capture to remain available")
	}
	if capture.Response.ID != completedEnvelope.Response.ID {
		t.Fatalf("expected capture response id %q to match completed payload, got %q", completedEnvelope.Response.ID, capture.Response.ID)
	}
	if capture.Response.Status != "completed" {
		t.Fatalf("expected capture response status completed, got %q", capture.Response.Status)
	}
	if len(capture.Response.Output) != 1 {
		t.Fatalf("expected captured final response output preserved, got %#v", capture.Response.Output)
	}
	completedCount := 0
	completedIndex := -1
	doneIndex := -1
	outputDoneIndex := -1
	for i, frame := range capture.Frames {
		if frame.Event == "response.output_item.done" {
			outputDoneIndex = i
		}
		if frame.Event == "response.completed" {
			completedCount++
			completedIndex = i
		}
		if frame.Done {
			doneIndex = i
		}
	}
	if completedCount != 1 {
		t.Fatalf("expected exactly one response.completed frame, got %#v", capture.Frames)
	}
	if completedIndex < 0 || doneIndex < 0 || completedIndex >= doneIndex {
		t.Fatalf("expected response.completed frame before done frame, got %#v", capture.Frames)
	}
	if outputDoneIndex < 0 || outputDoneIndex >= completedIndex {
		t.Fatalf("expected output item completion before response.completed, got %#v", capture.Frames)
	}
	if capture.Response.Usage.TotalTokens != 3 {
		t.Fatalf("expected captured final response usage preserved, got %#v", capture.Response.Usage)
	}
	messageItem := capture.Response.Output[0]
	if messageItem.Type != "message" {
		t.Fatalf("expected captured final response output item type message, got %#v", messageItem)
	}
	content, ok := messageItem.Content.([]interface{})
	if !ok {
		t.Fatalf("expected captured final response content array, got %#v", messageItem.Content)
	}
	if len(content) != 1 {
		t.Fatalf("expected one content block in captured final response, got %#v", content)
	}
	contentBlock, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected captured content block object, got %#v", content[0])
	}
	if contentBlock["text"] != "canonical terminal text" {
		t.Fatalf("expected captured final response to preserve canonical output text, got %#v", contentBlock)
	}
}

func TestStreamResponsesHandler_InterleavedOutputToolStableTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_interleaved","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"fc_interleaved","type":"function_call","status":"in_progress","call_id":"call_interleaved","name":"read_file","arguments":"{\"path\":\"terminal.txt\"}"}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_interleaved","role":"assistant","content":[{"type":"output_text","text":"assistant first"}]}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_interleaved","type":"function_call","status":"completed","call_id":"call_interleaved","name":"read_file","arguments":"{\"path\":\"terminal.txt\"}"}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_interleaved","model":"gpt-4o","output":[{"type":"message","id":"msg_interleaved_final","role":"assistant","content":[{"type":"output_text","text":"stable terminal text"}]},{"id":"fc_interleaved","type":"function_call","status":"completed","call_id":"call_interleaved","name":"read_file","arguments":"{\"path\":\"terminal.txt\"}"}],"status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}
	if responseText != "" {
		t.Fatalf("expected no direct response text on canonical completed path, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected completed usage preserved, got %#v", usage)
	}

	body := recorder.Body.String()
	completedMarker := `"type":"response.completed"`
	if strings.Count(body, completedMarker) != 1 {
		t.Fatalf("expected exactly one forwarded response.completed payload, got body %q", body)
	}
	messageDoneMarker := `"id":"msg_interleaved"`
	toolDoneMarker := `"id":"fc_interleaved","type":"function_call","status":"completed"`
	messageDoneIndex := strings.Index(body, messageDoneMarker)
	toolDoneIndex := strings.Index(body, toolDoneMarker)
	completedIndex := strings.Index(body, completedMarker)
	doneIndex := strings.Index(body, `data: [DONE]`)
	if messageDoneIndex < 0 || toolDoneIndex < 0 || completedIndex < 0 || doneIndex < 0 {
		t.Fatalf("expected interleaved output, tool, completed, and done markers in body, got %q", body)
	}
	if messageDoneIndex >= completedIndex {
		t.Fatalf("expected assistant output completion before response.completed, got %q", body)
	}
	if toolDoneIndex >= completedIndex {
		t.Fatalf("expected tool completion before response.completed, got %q", body)
	}
	if completedIndex >= doneIndex {
		t.Fatalf("expected exactly one response.completed before [DONE], got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body to be stored in context")
	}

	var capture struct {
		Response *model.ResponsesResponse `json:"response"`
		Frames   []struct {
			Event string      `json:"event"`
			Done  bool        `json:"done"`
			Data  interface{} `json:"data"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	if capture.Response == nil {
		t.Fatalf("expected final response capture to remain available")
	}
	if capture.Response.ID != "resp_interleaved" {
		t.Fatalf("expected captured final response id preserved, got %q", capture.Response.ID)
	}
	if capture.Response.Status != "completed" {
		t.Fatalf("expected captured final response status completed, got %q", capture.Response.Status)
	}
	if capture.Response.Usage.TotalTokens != 5 {
		t.Fatalf("expected captured final response usage preserved, got %#v", capture.Response.Usage)
	}
	if len(capture.Response.Output) != 2 {
		t.Fatalf("expected captured final response output preserved, got %#v", capture.Response.Output)
	}

	messageDoneFrameIndex := -1
	toolDoneFrameIndex := -1
	completedFrameIndex := -1
	doneFrameIndex := -1
	completedCount := 0
	for i, frame := range capture.Frames {
		if frame.Event == "response.output_item.done" {
			data, _ := frame.Data.(map[string]interface{})
			item, _ := data["item"].(map[string]interface{})
			if item["id"] == "msg_interleaved" {
				messageDoneFrameIndex = i
			}
			if item["id"] == "fc_interleaved" && item["type"] == "function_call" && item["status"] == "completed" {
				toolDoneFrameIndex = i
			}
		}
		if frame.Event == "response.completed" {
			completedCount++
			completedFrameIndex = i
		}
		if frame.Done {
			doneFrameIndex = i
		}
	}
	if completedCount != 1 {
		t.Fatalf("expected exactly one response.completed frame, got %#v", capture.Frames)
	}
	if messageDoneFrameIndex < 0 || messageDoneFrameIndex >= completedFrameIndex {
		t.Fatalf("expected assistant output completion frame before response.completed, got %#v", capture.Frames)
	}
	if toolDoneFrameIndex < 0 || toolDoneFrameIndex >= completedFrameIndex {
		t.Fatalf("expected tool completion frame before response.completed, got %#v", capture.Frames)
	}
	if completedFrameIndex < 0 || doneFrameIndex < 0 || completedFrameIndex >= doneFrameIndex {
		t.Fatalf("expected response.completed frame before done frame, got %#v", capture.Frames)
	}
}

func TestConvertOpenAIChatToResponses_InterleavedToolTerminalOrdering(t *testing.T) {
	chunks := []string{
		`data: {"id":"resp_interleaved_conv","choices":[{"index":0,"delta":{"role":"assistant","content":"assistant first"},"finish_reason":null}]}`,
		`data: {"id":"resp_interleaved_conv","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_interleaved_conv","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"terminal.txt\"}"}}]},"finish_reason":null}]}`,
		`data: {"id":"resp_interleaved_conv","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}

	var param any
	var allEvents []string
	reqBody := []byte(`{
		"model": "codex-test",
		"tools": [
			{"type": "function", "name": "read_file", "description": "read file", "parameters": {"type": "object"}}
		]
	}`)
	for _, chunk := range chunks {
		allEvents = append(allEvents, ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)...)
	}

	messageDoneIndex := -1
	toolDoneIndex := -1
	completedIndex := -1
	for i, evt := range allEvents {
		if strings.Contains(evt, "event: response.output_item.done") {
			payloads := parseResponsesEventData([]string{evt}, "response.output_item.done")
			if len(payloads) == 0 {
				continue
			}
			item, _ := payloads[0]["item"].(map[string]interface{})
			if item["type"] == "message" && item["id"] == "msg_resp_interleaved_conv_0" {
				messageDoneIndex = i
			}
			if item["type"] == "function_call" && item["id"] == "fc_call_interleaved_conv" {
				toolDoneIndex = i
			}
		}
		if strings.Contains(evt, "event: response.completed") {
			completedIndex = i
		}
	}

	outputDonePayloads := parseResponsesEventData(allEvents, "response.output_item.done")
	if len(outputDonePayloads) != 2 {
		t.Fatalf("expected 2 output_item.done events, got %#v", outputDonePayloads)
	}
	firstItem, _ := outputDonePayloads[0]["item"].(map[string]interface{})
	secondItem, _ := outputDonePayloads[1]["item"].(map[string]interface{})
	if firstItem["type"] != "message" || firstItem["id"] != "msg_resp_interleaved_conv_0" {
		t.Fatalf("expected first done item to be canonical message, got %#v", firstItem)
	}
	if secondItem["type"] != "function_call" || secondItem["id"] != "fc_call_interleaved_conv" || secondItem["status"] != "completed" {
		t.Fatalf("expected second done item to be completed function call, got %#v", secondItem)
	}
	if messageDoneIndex < 0 {
		t.Fatalf("expected message completion event in generated SSE, got %#v", allEvents)
	}
	if toolDoneIndex <= messageDoneIndex {
		t.Fatalf("expected tool completion after message completion, got %#v", allEvents)
	}
	if completedIndex <= toolDoneIndex {
		t.Fatalf("expected response.completed after tool completion, got %#v", allEvents)
	}

	completedPayloads := parseResponsesEventData(allEvents, "response.completed")
	if len(completedPayloads) != 1 {
		t.Fatalf("expected exactly one response.completed event, got %#v", completedPayloads)
	}
	response, _ := completedPayloads[0]["response"].(map[string]interface{})
	output, _ := response["output"].([]interface{})
	if len(output) != 2 {
		t.Fatalf("expected completed response output to preserve message and function call, got %#v", response)
	}
	outputMessage, _ := output[0].(map[string]interface{})
	outputTool, _ := output[1].(map[string]interface{})
	if outputMessage["type"] != "message" || outputMessage["id"] != "msg_resp_interleaved_conv_0" {
		t.Fatalf("expected completed output message preserved, got %#v", outputMessage)
	}
	if outputTool["type"] != "function_call" || outputTool["id"] != "fc_call_interleaved_conv" {
		t.Fatalf("expected completed output function call preserved, got %#v", outputTool)
	}
	if outputTool["call_id"] != "call_interleaved_conv" {
		t.Fatalf("expected completed output call_id preserved, got %#v", outputTool)
	}
	if outputTool["arguments"] != `{"path":"terminal.txt"}` {
		t.Fatalf("expected completed output arguments preserved, got %#v", outputTool)
	}
}

func TestStreamResponsesHandler_FailedTerminalShortCircuitsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_failed_terminal","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial text"}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed_terminal","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4},"error":{"code":"server_error","message":"terminal failure"}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_failed_terminal","model":"gpt-4o","status":"completed","output":[{"type":"message","id":"msg_late","role":"assistant","content":[{"type":"output_text","text":"must be ignored"}]}],"usage":{"input_tokens":3,"output_tokens":99,"total_tokens":102}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected failed terminal after SSE start not to bubble JSON error, got %+v", err)
	}
	if responseText != "partial text" {
		t.Fatalf("expected partial delta text preserved before failure, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("expected failed terminal to adopt failed payload usage without late success regression, got %#v", usage)
	}

	body := recorder.Body.String()
	failedMarker := `"type":"response.failed"`
	completedMarker := `"type":"response.completed"`
	if strings.Count(body, failedMarker) != 1 {
		t.Fatalf("expected exactly one forwarded response.failed payload, got %q", body)
	}
	if strings.Contains(body, completedMarker) {
		t.Fatalf("expected late response.completed to be dropped after failure, got %q", body)
	}
	if strings.Count(body, `data: [DONE]`) != 1 {
		t.Fatalf("expected done marker to remain forwarded once, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected failed terminal snapshot to remain observable")
	}
}

func TestStreamResponsesHandler_FailedTerminalDropsDuplicateFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_failed_duplicate","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed_duplicate","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4},"error":{"code":"server_error","message":"first terminal failure"}}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed_duplicate","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":3,"output_tokens":99,"total_tokens":102},"error":{"code":"server_error","message":"duplicate failed should be dropped"}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected duplicate failed terminal after SSE start not to bubble JSON error, got %+v", err)
	}
	if responseText != "" {
		t.Fatalf("expected no response text for failed-only stream, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("expected duplicate failed to preserve first failed usage snapshot, got %#v", usage)
	}

	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.failed"`) != 1 {
		t.Fatalf("expected duplicate response.failed to be dropped, got %q", body)
	}
	if strings.Contains(body, `duplicate failed should be dropped`) {
		t.Fatalf("expected duplicate failed payload not to be forwarded, got %q", body)
	}
	if strings.Count(body, `data: [DONE]`) != 1 {
		t.Fatalf("expected done marker to remain forwarded once, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected duplicate failed terminal snapshot to remain observable")
	}
}

func TestStreamResponsesHandler_FailedTerminalUsagePrefersNonZeroNestedButPreservesRicherTopLevel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name               string
		failedPayload      string
		expectPrompt       int
		expectCompletion   int
		expectTotal        int
		expectCachedTokens int
	}{
		{
			name:               "non-zero nested usage overrides top-level snapshot",
			failedPayload:      `{"type":"response.failed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":7}},"response":{"id":"resp_failed_nested_wins","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4,"input_tokens_details":{"cached_tokens":2}},"error":{"code":"server_error","message":"terminal failure"}}}`,
			expectPrompt:       3,
			expectCompletion:   1,
			expectTotal:        4,
			expectCachedTokens: 2,
		},
		{
			name:               "zero nested usage does not erase richer top-level usage",
			failedPayload:      `{"type":"response.failed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":7}},"response":{"id":"resp_failed_top_level_kept","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"error":{"code":"server_error","message":"terminal failure"}}}`,
			expectPrompt:       9,
			expectCompletion:   4,
			expectTotal:        13,
			expectCachedTokens: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_failed_usage","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
				"",
				`event: response.failed`,
				`data: ` + tt.failedPayload,
				"",
				`data: [DONE]`,
				"",
			}, "\n")

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

			err, _, usage := StreamResponsesHandler(c, resp)
			if err != nil {
				t.Fatalf("expected failed terminal to stay on SSE path, got %+v", err)
			}
			if usage == nil {
				t.Fatalf("expected failed terminal usage to be captured")
			}
			if usage.PromptTokens != tt.expectPrompt || usage.CompletionTokens != tt.expectCompletion || usage.TotalTokens != tt.expectTotal {
				t.Fatalf("expected usage p=%d c=%d t=%d, got %#v", tt.expectPrompt, tt.expectCompletion, tt.expectTotal, usage)
			}
			if tt.expectCachedTokens > 0 {
				if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != tt.expectCachedTokens {
					t.Fatalf("expected cached_tokens=%d, got %#v", tt.expectCachedTokens, usage.PromptTokensDetails)
				}
			}

			rawBody := c.GetString(ctxkey.ResponseBody)
			if rawBody == "" {
				t.Fatalf("expected failed terminal snapshot to remain observable")
			}
			var capture struct {
				Response *model.ResponsesResponse `json:"response"`
			}
			if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
				t.Fatalf("unmarshal capture json: %v", err)
			}
			if capture.Response == nil {
				t.Fatalf("expected failed response snapshot in capture")
			}
			if capture.Response.Usage.TotalTokens != tt.expectTotal {
				t.Fatalf("expected capture usage total_tokens=%d, got %#v", tt.expectTotal, capture.Response.Usage)
			}
		})
	}
}

func TestStreamResponsesHandler_CompletedUsageDoesNotOverwriteWithZeroValueResponseUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13},"response":{"id":"resp_usage_keep","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13},"response":{"id":"resp_usage_keep","model":"gpt-4o","output":[{"type":"message","id":"msg_usage_keep","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
		"",
		`data: [DONE]`,
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
		t.Fatalf("expected usage to be preserved")
	}
	if usage.PromptTokens != 9 || usage.CompletionTokens != 4 || usage.TotalTokens != 13 {
		t.Fatalf("expected top-level non-zero usage preserved, got %#v", usage)
	}

	var capture struct {
		Response *model.ResponsesResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(c.GetString(ctxkey.ResponseBody)), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	if capture.Response == nil || capture.Response.Usage.TotalTokens != 13 {
		t.Fatalf("expected capture response usage preserved, got %#v", capture.Response)
	}
}

func TestStreamResponsesHandler_CompletedTerminalIgnoresLateFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_completed_terminal","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_completed_terminal","model":"gpt-4o","status":"completed","output":[{"type":"message","id":"msg_completed_terminal","role":"assistant","content":[{"type":"output_text","text":"stable success"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_completed_terminal","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":2,"output_tokens":99,"total_tokens":101},"error":{"code":"server_error","message":"late failure must be ignored"}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected completed terminal to ignore late failure, got %+v", err)
	}
	if responseText != "" {
		t.Fatalf("expected canonical completed path to keep empty responseText, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected completed terminal usage to win, got %#v", usage)
	}

	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 1 {
		t.Fatalf("expected exactly one forwarded response.completed payload, got %q", body)
	}
	if strings.Contains(body, `"type":"response.failed"`) {
		t.Fatalf("expected late response.failed to be dropped after completion, got %q", body)
	}
	if strings.Count(body, `data: [DONE]`) != 1 {
		t.Fatalf("expected done marker to remain forwarded once, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected completed capture body to be stored")
	}

	var capture struct {
		Response *model.ResponsesResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	if capture.Response == nil {
		t.Fatalf("expected completed capture response to remain available")
	}
	if capture.Response.Status != "completed" {
		t.Fatalf("expected capture response status completed, got %q", capture.Response.Status)
	}
	if capture.Response.Usage.TotalTokens != 5 {
		t.Fatalf("expected capture usage from completed terminal preserved, got %#v", capture.Response.Usage)
	}
}

func TestStreamResponsesHandler_CompletedTerminalIgnoresLateNormalChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_completed_late_normal","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_completed_late_normal","model":"gpt-4o","status":"completed","output":[{"type":"message","id":"msg_completed_late_normal","role":"assistant","content":[{"type":"output_text","text":"stable success"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_completed_late_normal_extra","role":"assistant","content":[{"type":"output_text","text":"late chunk must be ignored"}]}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected completed terminal to ignore late normal chunk, got %+v", err)
	}
	if responseText != "" {
		t.Fatalf("expected canonical completed path to keep empty responseText, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected completed terminal usage to win, got %#v", usage)
	}

	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 1 {
		t.Fatalf("expected exactly one forwarded response.completed payload, got %q", body)
	}
	if strings.Contains(body, `"type":"response.output_item.done"`) {
		t.Fatalf("expected late normal event to be dropped after completion, got %q", body)
	}
	if strings.Count(body, `data: [DONE]`) != 1 {
		t.Fatalf("expected done marker to remain forwarded once, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected completed capture body to be stored")
	}

	var capture struct {
		Response *model.ResponsesResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	if capture.Response == nil {
		t.Fatalf("expected completed capture response to remain available")
	}
	if capture.Response.Status != "completed" {
		t.Fatalf("expected capture response status completed, got %q", capture.Response.Status)
	}
	if capture.Response.Usage.TotalTokens != 5 {
		t.Fatalf("expected capture usage from completed terminal preserved, got %#v", capture.Response.Usage)
	}
	if len(capture.Response.Output) != 1 {
		t.Fatalf("expected late normal chunk not to mutate completed output, got %#v", capture.Response.Output)
	}
	messageItem := capture.Response.Output[0]
	if messageItem.Type != "message" {
		t.Fatalf("expected completed output to stay as message, got %#v", messageItem)
	}
	content, ok := messageItem.Content.([]interface{})
	if !ok {
		t.Fatalf("expected completed content array, got %#v", messageItem.Content)
	}
	if len(content) != 1 {
		t.Fatalf("expected completed message content to remain unchanged, got %#v", content)
	}
	contentBlock, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected completed content block object, got %#v", content[0])
	}
	if contentBlock["text"] != "stable success" {
		t.Fatalf("expected completed content text preserved, got %#v", contentBlock)
	}
}

func TestStreamResponsesHandler_MissingCompletedSafeReconstruction(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_missing_completed","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":4,"output_tokens":0,"total_tokens":4}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"safe "}`,
		"",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"ignored noise"}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"rebuild"}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_missing_completed","role":"assistant","content":[{"type":"output_text","text":"safe rebuild"}]}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected missing completed stream to reconstruct safely, got %+v", err)
	}
	if responseText != "safe rebuild" {
		t.Fatalf("expected output deltas to remain aggregated, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("expected usage to survive safe reconstruction, got %#v", usage)
	}

	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 1 {
		t.Fatalf("expected synthetic response.completed to be forwarded exactly once, got %q", body)
	}
	if !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("expected synthetic completed payload to mark status completed, got %q", body)
	}
	if strings.Count(body, `data: [DONE]`) != 1 {
		t.Fatalf("expected one done marker for missing completed stream, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected reconstructed capture body to be stored")
	}

	var capture struct {
		Response *model.ResponsesResponse `json:"response"`
		Frames   []struct {
			Event string `json:"event"`
			Done  bool   `json:"done"`
		} `json:"frames"`
	}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal reconstructed capture: %v", err)
	}
	if capture.Response == nil {
		t.Fatalf("expected reconstructed capture response")
	}
	if capture.Response.ID != "resp_missing_completed" {
		t.Fatalf("expected reconstructed response id preserved, got %q", capture.Response.ID)
	}
	if capture.Response.Status != "completed" {
		t.Fatalf("expected reconstructed response status promoted to completed, got %q", capture.Response.Status)
	}
	if capture.Response.Usage.TotalTokens != 4 {
		t.Fatalf("expected reconstructed usage preserved, got %#v", capture.Response.Usage)
	}
	if len(capture.Response.Output) != 1 {
		t.Fatalf("expected reconstructed output item from fallback capture, got %#v", capture.Response.Output)
	}
	if capture.Response.Output[0].ID != "msg_missing_completed" {
		t.Fatalf("expected reconstructed output item preserved, got %#v", capture.Response.Output[0])
	}
	completedCount := 0
	doneCount := 0
	for _, frame := range capture.Frames {
		if frame.Event == "response.completed" {
			completedCount++
		}
		if frame.Done {
			doneCount++
		}
	}
	if completedCount != 1 {
		t.Fatalf("expected one synthetic response.completed frame in capture, got %#v", capture.Frames)
	}
	if doneCount != 1 {
		t.Fatalf("expected one done frame in capture, got %#v", capture.Frames)
	}
}

func TestConvertOpenAIChatToResponses_FailedTerminalDropsLateNormalChunk(t *testing.T) {
	chunks := []string{
		`data: {"error":{"code":"server_error","message":"terminal failure"}}`,
		`data: {"id":"resp_late_chunk","choices":[{"index":0,"delta":{"content":"must be ignored"},"finish_reason":null}]}`,
		`data: [DONE]`,
	}

	var param any
	var allEvents []string
	reqBody := []byte(`{"model":"codex-test"}`)
	for _, chunk := range chunks {
		allEvents = append(allEvents, ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)...)
	}

	failedPayloads := parseResponsesEventData(allEvents, "response.failed")
	if len(failedPayloads) != 1 {
		t.Fatalf("expected exactly one response.failed event, got %#v", allEvents)
	}
	if len(allEvents) != 1 {
		t.Fatalf("expected late normal chunk and done marker to be dropped after failed terminal, got %#v", allEvents)
	}
	if _, ok := failedPayloads[0]["response"]; !ok {
		t.Fatalf("expected failed terminal payload to include response object, got %#v", failedPayloads[0])
	}
}

func TestStreamResponsesHandler_DeltaNoiseDoesNotCorruptTerminalSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_noisy_terminal","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"noise one"}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"clean"}`,
		"",
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","delta":"noise two"}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":" text"}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_noisy_terminal","role":"assistant","content":[{"type":"output_text","text":"clean text"}]}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_noisy_terminal","model":"gpt-4o","output":[{"type":"message","id":"msg_noisy_terminal","role":"assistant","content":[{"type":"output_text","text":"clean text"}]}],"status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected noisy stream to complete successfully, got %+v", err)
	}
	if responseText != "clean text" {
		t.Fatalf("expected only output_text deltas in response text, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 7 {
		t.Fatalf("expected noisy stream usage preserved, got %#v", usage)
	}

	body := recorder.Body.String()
	completedMarker := `"type":"response.completed"`
	if strings.Count(body, completedMarker) != 1 {
		t.Fatalf("expected exactly one completed terminal despite delta noise, got %q", body)
	}
	completedIndex := strings.Index(body, completedMarker)
	doneIndex := strings.Index(body, `data: [DONE]`)
	if completedIndex < 0 || doneIndex < 0 || completedIndex >= doneIndex {
		t.Fatalf("expected completed terminal before done even with noise, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected noisy stream capture body to be stored")
	}

	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal noisy capture json: %v", err)
	}
	frames := capture["frames"].([]interface{})
	completedCount := 0
	deltaCount := 0
	for _, frame := range frames {
		fm := frame.(map[string]interface{})
		if fm["event"] == "response.completed" {
			completedCount++
		}
		if fm["event"] == "response.output_text.delta" {
			deltaCount++
			data := fm["data"].(map[string]interface{})
			if data["delta"] != "clean text" {
				t.Fatalf("expected noisy output deltas collapsed to clean text, got %#v", data["delta"])
			}
		}
		if event, _ := fm["event"].(string); strings.HasSuffix(event, ".delta") && event != "response.output_text.delta" {
			t.Fatalf("expected non-output delta noise to stay out of capture frames, got %#v", fm)
		}
	}
	if completedCount != 1 {
		t.Fatalf("expected one completed frame in noisy capture, got %#v", frames)
	}
	if deltaCount != 1 {
		t.Fatalf("expected one aggregated output_text delta frame in noisy capture, got %#v", frames)
	}
	respJSON := capture["response"].(map[string]interface{})
	if respJSON["status"] != "completed" {
		t.Fatalf("expected noisy capture response status completed, got %#v", respJSON["status"])
	}
	if respJSON["usage"].(map[string]interface{})["total_tokens"] != float64(7) {
		t.Fatalf("expected noisy capture usage preserved, got %#v", respJSON["usage"])
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
	if err != nil {
		t.Fatalf("expected first failed event to be handled in-stream, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from failed response, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected streaming response status 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame failed event not to be duplicated into SSE body, got %q", recorder.Body.String())
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

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected first failed event to be handled in-stream, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from first-frame failed event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected streaming response status 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame failed event not to be duplicated into SSE body, got %q", recorder.Body.String())
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

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected first failed invalid-request event to be handled in-stream, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from invalid-request failed event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected streaming response status 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame invalid-request failed event not to be duplicated into SSE body, got %q", recorder.Body.String())
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
	if err != nil {
		t.Fatalf("expected first error event to be handled in-stream, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected no SSE body written before first-frame error, got %q", recorder.Body.String())
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
	if err != nil {
		t.Fatalf("expected mid-stream error not to bubble JSON error, got %+v", err)
	}
	if responseText != "Hello" {
		t.Fatalf("expected partial response text Hello, got %q", responseText)
	}
	if usage == nil {
		t.Fatalf("expected usage from completed frames before error")
	}
	if strings.Contains(recorder.Body.String(), `should be ignored`) {
		t.Fatalf("expected events after error to be dropped from SSE body, got %q", recorder.Body.String())
	}
}

func TestStreamResponsesHandler_ErrorEventWithEmptyCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试场景：error 事件中不包含 code 字段，Code 为空字符串
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
	if err != nil {
		t.Fatalf("expected first error event with empty code to be handled in-stream, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame empty-code error not to be duplicated into SSE body, got %q", recorder.Body.String())
	}
}

func TestStreamResponsesHandler_ErrorPayloadTypeWithoutEventName(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`data: {"type":"error","code":"request_failed","message":"request temporarily unavailable, please try again later","sequence_number":0}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected payload type error without explicit event name to be handled in-stream, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from first-frame payload type error, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame payload-type error not to be duplicated into SSE body, got %q", recorder.Body.String())
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
	if err != nil {
		t.Fatalf("expected late error after completed not to bubble JSON error, got %+v", err)
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

func TestStreamResponsesHandler_CompletedThenLateErrorEventStillReturnsErrorWithoutPollutingCompletedCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_completed_late_error","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_completed_late_error","model":"gpt-4o","status":"completed","output":[{"type":"message","id":"msg_completed_late_error","role":"assistant","content":[{"type":"output_text","text":"stable success"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"server_error","message":"late error must still fail","sequence_number":2}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected late error after completed not to bubble JSON error, got %+v", err)
	}
	if responseText != "" {
		t.Fatalf("expected completed-then-error stream not to append plain chunk text, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected completed usage preserved before late error, got %#v", usage)
	}

	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 1 {
		t.Fatalf("expected exactly one forwarded response.completed payload, got %q", body)
	}
	if !strings.Contains(body, `late error must still fail`) {
		t.Fatalf("expected late error event to be forwarded instead of swallowed, got %q", body)
	}
	if strings.Contains(body, `"delta":"late error must still fail"`) {
		t.Fatalf("expected late error not to be treated as normal delta chunk, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected completed capture body to remain persisted despite late error")
	}
	if strings.Contains(body, `late chunk`) {
		t.Fatalf("expected completed payload not to be polluted as normal chunk, got %q", body)
	}
	if strings.Contains(body, `"type":"response.output_text.delta"`) {
		t.Fatalf("expected late error not to introduce output_text.delta pollution, got %q", body)
	}
}

func TestStreamResponsesHandler_CompletedThenLateNormalChunkIsDropped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_completed_drop_normal","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_completed_drop_normal","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"late chunk"}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected nil error when late normal chunk arrives after completed, got %#v", err)
	}
	if responseText != "" {
		t.Fatalf("expected late normal chunk to be dropped from responseText, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected completed usage preserved, got %#v", usage)
	}
	body := recorder.Body.String()
	if strings.Contains(body, `"delta":"late chunk"`) {
		t.Fatalf("expected late normal chunk not forwarded after completed, got %q", body)
	}
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("expected completed event to remain forwarded, got %q", body)
	}
	if !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected done marker to remain forwarded, got %q", body)
	}
}

func TestStreamResponsesHandler_CompletedThenLateFailedEventIsDropped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_completed_drop_failed","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_completed_drop_failed","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_completed_drop_failed","model":"gpt-4o","status":"failed","output":[],"error":{"code":"server_error","message":"late failed should be ignored"}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected nil error when late response.failed arrives after completed, got %#v", err)
	}
	if responseText != "" {
		t.Fatalf("expected no responseText from completed-then-failed stream, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected completed usage preserved, got %#v", usage)
	}
	body := recorder.Body.String()
	if strings.Contains(body, `late failed should be ignored`) {
		t.Fatalf("expected late response.failed not forwarded after completed, got %q", body)
	}
	if strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("expected late failed terminal payload dropped after completed, got %q", body)
	}
	if !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("expected completed event to remain forwarded, got %q", body)
	}
	if !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected done marker to remain forwarded, got %q", body)
	}
}

func TestStreamResponsesHandler_ErrorEventMissingMessageField(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
	if err != nil {
		t.Fatalf("expected missing-message error event after headers committed to return nil error, got %+v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame missing-message error not to be duplicated into SSE body, got %q", recorder.Body.String())
	}
}

func TestStreamResponsesHandler_ErrorEventMissingBothMessageAndCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
	if err != nil {
		t.Fatalf("expected minimal error event after headers committed to return nil error, got %+v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame minimal error not to be duplicated into SSE body, got %q", recorder.Body.String())
	}
}

func TestStreamResponsesHandler_ErrorEventWithExtraFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
	if err != nil {
		t.Fatalf("expected extra-field error event after headers committed to return nil error, got %+v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame extra-field error not to be duplicated into SSE body, got %q", recorder.Body.String())
	}
}

func TestStreamResponsesHandler_ErrorEventWithOnlyTypeField(t *testing.T) {
	gin.SetMode(gin.TestMode)

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
	if err != nil {
		t.Fatalf("expected type-only error event after headers committed to return nil error, got %+v", err)
	}
	if usage != nil {
		t.Fatalf("expected nil usage from error event, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame type-only error not to be duplicated into SSE body, got %q", recorder.Body.String())
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
	if err != nil {
		t.Fatalf("expected mixed stream error not to bubble JSON error, got %+v", err)
	}
	// 验证 responseText 包含 error 前的累积内容。
	if responseText != "Hello world" {
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
	if err != nil {
		t.Fatalf("expected repeated error events not to bubble JSON error, got %+v", err)
	}
	if responseText != "Hello" {
		t.Fatalf("expected partial response text Hello, got %q", responseText)
	}
	if usage == nil {
		t.Fatalf("expected usage from completed frames before error")
	}
	if strings.Contains(recorder.Body.String(), `second error message`) {
		t.Fatalf("expected only first error payload to remain meaningful, got %q", recorder.Body.String())
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

func TestStreamResponsesHandler_FirstTerminalErrorAfterHeaderWriteReturnsNilError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`event: error
data: {"message":"upstream failed","type":"server_error","code":"bad_response"}

`)),
	}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected already-written first-frame terminal error to not bubble biz error, got %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200 already written, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	_ = body
}

func TestStreamResponsesHandler_FlushesHeadersAfterFirstValidEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	writer := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_immediate_headers","model":"gpt-4o","output":[],"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected nil error, got %+v", err)
	}
	if writer.Code != http.StatusOK {
		t.Fatalf("expected status code 200 written for valid stream, got %d", writer.Code)
	}
	if writer.flushCount == 0 {
		t.Fatalf("expected at least one flush after stream start")
	}
	if got := writer.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected event-stream header, got %q", got)
	}
}

func TestStreamResponsesHandler_DoesNotAppendDoneOnAbnormalEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)

	chunks := []string{strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_abnormal_eof","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
	}, "\n") + "\n"}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: &errReader{chunks: chunks, err: io.ErrUnexpectedEOF}}

	err, responseText, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected abnormal eof after SSE start not to bubble JSON error, got %+v", err)
	}
	if responseText != "partial" {
		t.Fatalf("expected known partial text preserved, got %q", responseText)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("expected last known usage snapshot preserved, got %#v", usage)
	}
	body := recorder.Body.String()
	if strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected abnormal eof not to append done marker, got %q", body)
	}
	if rawBody := c.GetString(ctxkey.ResponseBody); rawBody == "" {
		t.Fatalf("expected incomplete stream to keep last known snapshot for observability")
	}
}

func TestStreamResponsesHandler_LargeEventErrorIsObservable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	large := strings.Repeat("x", 21*1024*1024)
	stream := strings.Join([]string{
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_large","model":"gpt-4o","output":[{"type":"message","id":"msg_large","role":"assistant","content":[{"type":"output_text","text":"` + large + `"}]}],"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected oversize first event after SSE start to stay in stream, got %+v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected SSE stream to keep 200 status once headers are flushed, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected oversize first event to emit terminal SSE error event, got %q", body)
	}
	if !strings.Contains(body, `"code":"bad_response"`) {
		t.Fatalf("expected terminal SSE error payload to expose bad_response code, got %q", body)
	}
	if !strings.Contains(body, `"message":"`) {
		t.Fatalf("expected terminal SSE error payload to expose read failure message, got %q", body)
	}
	if strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("expected oversize event not to be masked by done marker, got %q", body)
	}
	if strings.Contains(body, `{"error":`) {
		t.Fatalf("expected no appended JSON transport error body in SSE response, got %q", body)
	}
	if rawBody := c.GetString(ctxkey.ResponseBody); rawBody != "" {
		t.Fatalf("expected oversize malformed event not to serialize completed capture, got %q", rawBody)
	}
}

func TestStreamResponsesHandler_StreamErrorDoesNotFinalizeAsJSONError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_stream_err","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: error`,
		`data: {"type":"error","code":"server_error","message":"boom","sequence_number":1}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected stream error to be handled inside SSE without bubbling JSON error, got %+v", err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected SSE error event forwarded, got %q", body)
	}
	if strings.Contains(body, `{"error":`) {
		t.Fatalf("expected no appended JSON error body in SSE response, got %q", body)
	}
}

func TestStreamResponsesHandler_ReadErrorAfterStreamBeginsEmitsTerminalErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	streamReader := &errReader{
		chunks: []string{
			strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_stream_read_error","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
				"",
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","delta":"Hello"}`,
				"",
				"",
			}, "\n"),
		},
		err: io.ErrUnexpectedEOF,
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: streamReader}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected stream read error to stay in SSE channel, got %+v", err)
	}
	if usage == nil || usage.TotalTokens != 1 {
		t.Fatalf("expected last known usage snapshot preserved, got %#v", usage)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected terminal SSE error event after read error, got %q", body)
	}
	if !strings.Contains(body, `"message":"unexpected EOF"`) {
		t.Fatalf("expected terminal error payload to expose read failure, got %q", body)
	}
	if strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected read error not to be masked as completed, got %q", body)
	}
	if strings.Contains(body, `[DONE]`) {
		t.Fatalf("expected read error not to emit done marker, got %q", body)
	}
	if strings.Contains(body, `{"error":`) {
		t.Fatalf("expected no appended JSON error body in SSE response, got %q", body)
	}
	if rawBody := c.GetString(ctxkey.ResponseBody); rawBody == "" {
		t.Fatalf("expected capture body to be stored on stream read error")
	} else if !strings.Contains(rawBody, `"event":"response.output_text.delta"`) || !strings.Contains(rawBody, `"delta":"Hello"`) {
		t.Fatalf("expected last delta frame captured before read error, got %q", rawBody)
	}
}

func TestStreamResponsesHandler_FirstFrameResponseFailedPreservesUsageAndCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed_first","model":"gpt-4o","output":[],"status":"failed","error":{"message":"boom","type":"server_error","code":"request_failed"},"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected first response.failed frame to be handled in-stream, got %v", err)
	}
	if usage == nil {
		t.Fatalf("expected failed first-frame usage to be preserved")
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 5 || usage.TotalTokens != 8 {
		t.Fatalf("expected failed first-frame usage preserved, got %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected streaming response status 200, got %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected first-frame failed event not to be duplicated into SSE body, got %q", recorder.Body.String())
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected failed first-frame capture body to be stored")
	}
	if !strings.Contains(rawBody, `"status":"failed"`) || !strings.Contains(rawBody, `"total_tokens":8`) {
		t.Fatalf("expected failed first-frame capture to retain failure usage snapshot, got %q", rawBody)
	}
}

func TestStreamResponsesHandler_ResponseFailedAggregatesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_failed_usage","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_failed_usage","model":"gpt-4o","output":[],"status":"failed","error":{"message":"boom","type":"server_error","code":"request_failed"},"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected failed response to stay in SSE channel, got %+v", err)
	}
	if usage == nil {
		t.Fatalf("expected usage from failed response payload")
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 5 || usage.TotalTokens != 8 {
		t.Fatalf("expected failed response usage aggregated, got %#v", usage)
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected capture body to be stored")
	}
	if !strings.Contains(rawBody, `"status":"failed"`) {
		t.Fatalf("expected failed response snapshot captured, got %q", rawBody)
	}
	if !strings.Contains(rawBody, `"total_tokens":8`) {
		t.Fatalf("expected failed response usage captured, got %q", rawBody)
	}
}

func TestStreamResponsesHandler_MissingCompletedWithOutputSynthesizesCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_missing_completed_new","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":4,"output_tokens":0,"total_tokens":4}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_missing_completed_new","role":"assistant","content":[{"type":"output_text","text":"safe rebuild"}]}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected no transport error for incomplete stream, got %+v", err)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("expected last known usage snapshot preserved, got %#v", usage)
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected capture body to be stored")
	}
	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture: %v", err)
	}
	response, ok := capture["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected snapshot response preserved, got %#v", capture["response"])
	}
	if response["status"] != "completed" {
		t.Fatalf("expected missing completed stream with output to be promoted to completed, got %#v", response)
	}
}

func TestStreamResponsesHandler_MissingCompletedWithoutOutputOrUsageDoesNotSynthesize(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_missing_completed_empty","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected no transport error, got %+v", err)
	}
	if usage == nil {
		t.Fatalf("expected last known usage snapshot preserved")
	}
	if strings.Contains(recorder.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("expected no synthetic completed when neither output nor meaningful usage exists, got %q", recorder.Body.String())
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected capture body to be stored")
	}
	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture: %v", err)
	}
	response := capture["response"].(map[string]interface{})
	if response["status"] == "completed" {
		t.Fatalf("expected snapshot to stay incomplete, got %#v", response)
	}
}

func TestStreamResponsesHandler_ExplicitFailedDoesNotSynthesizeCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_no_synth_failed","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_no_synth_failed","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}`,
		"",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_no_synth_failed","model":"gpt-4o","status":"failed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"error":{"code":"server_error","message":"boom"}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected no transport error, got %+v", err)
	}
	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 0 {
		t.Fatalf("expected no synthetic completed after explicit failed, got %q", body)
	}
	if strings.Count(body, `"type":"response.failed"`) != 1 {
		t.Fatalf("expected failed payload preserved, got %q", body)
	}
}

func TestAdaptorSetupRequestHeader_UsesCommonHeadersAndContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	metaInfo := &meta.Meta{APIKey: "test-key", IsStream: true}
	adp := &Adaptor{}

	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := adp.SetupRequestHeader(c, upstreamReq, metaInfo); err != nil {
		t.Fatalf("setup request header: %v", err)
	}
	if got := upstreamReq.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("expected stream accept header from common logic, got %q", got)
	}
	if got := upstreamReq.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("expected authorization header preserved, got %q", got)
	}
	if upstreamReq.Context() != c.Request.Context() {
		t.Fatalf("expected upstream request to bind downstream context")
	}
}
