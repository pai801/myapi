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

func TestReadSSEEvent_LineLargerThanReaderBuffer(t *testing.T) {
	// 单行超过 reader 内部缓冲，触发 ErrBufferFull 分片拼装路径，语义须与整行读取一致
	payload := strings.Repeat("x", 5000)
	r := bufio.NewReaderSize(strings.NewReader("data: "+payload+"\n\n"), 1024)

	event, err := readSSEEvent(r, maxSSEEventBytes)
	if err != nil {
		t.Fatalf("readSSEEvent returned error: %v", err)
	}
	if event.Data != payload {
		t.Fatalf("expected payload preserved across fragments, got len=%d", len(event.Data))
	}
}

func TestReadSSEEvent_RejectsEventOverQuotaWithinSingleLine(t *testing.T) {
	// 超限须在分片读取过程中即时触发，而非整行拼装完成后才检查
	payload := strings.Repeat("x", 5000)
	r := bufio.NewReaderSize(strings.NewReader("data: "+payload+"\n\n"), 1024)

	_, err := readSSEEvent(r, 1024)
	if err == nil || !strings.Contains(err.Error(), "sse event too large") {
		t.Fatalf("expected sse event too large error, got %v", err)
	}
}

func TestReadSSEEvent_RejectsEventOverQuotaAcrossLines(t *testing.T) {
	// RawSize 跨行累计，多行 data 累计超限同样须即时中断
	r := bufio.NewReaderSize(strings.NewReader("data: aaa\ndata: bbb\ndata: ccc\n\n"), 16)

	_, err := readSSEEvent(r, 20)
	if err == nil || !strings.Contains(err.Error(), "sse event too large") {
		t.Fatalf("expected sse event too large error, got %v", err)
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

func TestStreamResponsesHandler_StoresAggregatedResponseOnly(t *testing.T) {
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
	if _, ok := capture["frames"]; ok {
		t.Fatalf("did not expect frames array in stored response body")
	}

	respJSON, ok := capture["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected completed response in capture, got %#v", capture["response"])
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
	// 聚合结果：completed 快照 output 为空时，output_items 累积兜底（保留完整 item 快照）
	output, ok := respJSON["output"].([]interface{})
	if !ok || len(output) != 1 {
		t.Fatalf("expected fallback output item aggregated, got %#v", output)
	}
	item := output[0].(map[string]interface{})
	content, _ := item["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected one content block, got %#v", content)
	}
	part := content[0].(map[string]interface{})
	if part["text"] != "Hello" {
		t.Fatalf("expected aggregated output text Hello, got %#v", part["text"])
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
		ev, evErr := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
		if evErr != nil {
			t.Fatalf("unexpected conversion error on valid stream chunk %q: %v", chunk, evErr)
		}
		allEvents = append(allEvents, ev...)
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
	// 新 done 策略（协议 §7：每 item 恒且仅一条 done，done 携带最终完整 content）：
	// text 段关闭（tool 到达）不再提前发 message done，唯一 done 在终态补发点发出，
	// 故本流 done 顺序为 [function_call, message]，两者均先于 response.completed。
	// 旧断言「message done 先于 tool done」锁定的是「item 级早关 done」实现策略
	// （该策略导致同 item done 内容增补/重复 done，与 marked done 一次性语义矛盾），非协议要求。
	if len(outputDonePayloads) != 2 {
		t.Fatalf("expected 2 output_item.done events, got %#v", outputDonePayloads)
	}
	firstItem, _ := outputDonePayloads[0]["item"].(map[string]interface{})
	secondItem, _ := outputDonePayloads[1]["item"].(map[string]interface{})
	if firstItem["type"] != "function_call" || firstItem["id"] != "fc_call_interleaved_conv" || firstItem["status"] != "completed" {
		t.Fatalf("expected first done item to be completed function call, got %#v", firstItem)
	}
	if secondItem["type"] != "message" || secondItem["id"] != "msg_resp_interleaved_conv_0" {
		t.Fatalf("expected second (terminal-flushed) done item to be canonical message, got %#v", secondItem)
	}
	if msgContent, _ := secondItem["content"].([]interface{}); len(msgContent) != 1 ||
		msgContent[0].(map[string]interface{})["text"] != "assistant first" {
		t.Fatalf("expected message done to carry final full text content, got %#v", secondItem["content"])
	}
	if messageDoneIndex < 0 {
		t.Fatalf("expected message completion event in generated SSE, got %#v", allEvents)
	}
	if messageDoneIndex <= toolDoneIndex {
		t.Fatalf("expected message completion after tool completion (terminal flush), got %#v", allEvents)
	}
	if completedIndex <= messageDoneIndex {
		t.Fatalf("expected response.completed after all item completions, got %#v", allEvents)
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
	if err == nil {
		t.Fatal("expected failed terminal after SSE start to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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

// TestStreamResponsesHandler_CommittedReadFailureReturnsErrorWithoutAppendingOutput 锁定批次二契约：
// SSE 已提交后发生上游读错误时必须回传非 nil 502 供渠道失败记账，且只按既有 SSE 帧写出——
// 不得追加 JSON HTTP 错误体、伪造 [DONE] 或成功终态，也不得重复终态事件。
func TestStreamResponsesHandler_CommittedReadFailureReturnsErrorWithoutAppendingOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	chunks := []string{strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_read_fail","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
	}, "\n") + "\n"}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: &errReader{chunks: chunks, err: io.ErrUnexpectedEOF}}

	err, responseText, _ := StreamResponsesHandler(c, resp)
	if err == nil {
		t.Fatal("expected committed read failure to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
	}
	if responseText != "partial" {
		t.Fatalf("expected preserved partial text, got %q", responseText)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed SSE to keep HTTP 200, got %d", recorder.Code)
	}

	body := recorder.Body.String()
	// 已提交客户端流只能由 SSE 帧行组成：不得追加 JSON HTTP 错误体。
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-SSE content appended after committed stream header: %q (body=%q)", line, body)
		}
	}
	if strings.Contains(body, `{"error":`) {
		t.Fatalf("expected no JSON HTTP error body appended, got %q", body)
	}
	if strings.Count(body, `data: [DONE]`) != 0 {
		t.Fatalf("expected no fabricated [DONE] on failure, got %q", body)
	}
	if strings.Count(body, `"type":"response.completed"`) != 0 {
		t.Fatalf("expected no fabricated success terminal, got %q", body)
	}
	if strings.Count(body, `"type":"error"`) != 1 {
		t.Fatalf("expected exactly one terminal error event, got %q", body)
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
	if err == nil {
		t.Fatal("expected duplicate failed terminal after SSE start to return non-nil 502")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
			if err == nil {
				t.Fatal("expected failed terminal to return non-nil 502 for channel failure accounting")
			}
			if err.StatusCode != http.StatusBadGateway {
				t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
		ev, evErr := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
		if evErr != nil {
			t.Fatalf("unexpected conversion error on valid stream chunk %q: %v", chunk, evErr)
		}
		allEvents = append(allEvents, ev...)
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
	if _, ok := capture["frames"]; ok {
		t.Fatalf("did not expect frames array in stored response body")
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

	// 首帧即 response.failed：SSE 头已提交，失败只经事件表达，但必须回传非 nil 错误供渠道失败记账
	// （重试由框架层 Written() 守卫抑制，不再依赖错误码映射）。
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
		t.Fatal("expected first failed event to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected server-error failed event to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected invalid-request failed event to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected first error event to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatalf("expected mid-stream error to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected first error event with empty code to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected payload type error without explicit event name to return non-nil 502")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected missing-message error event after headers committed to return non-nil 502")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected minimal error event after headers committed to return non-nil 502")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected extra-field error event after headers committed to return non-nil 502")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected type-only error event after headers committed to return non-nil 502")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatalf("expected mixed stream error to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatalf("expected repeated error events to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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

func TestStreamResponsesHandler_FirstTerminalErrorAfterHeaderWriteReturnsFailureError(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected first-frame terminal error after header write to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200 already written, got %d", recorder.Code)
	}
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
	if err == nil {
		t.Fatal("expected abnormal eof after SSE start to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected oversize first event after SSE start to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected stream error to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected stream read error to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	} else if !strings.Contains(rawBody, `"status":"failed"`) || !strings.Contains(rawBody, `"message":"unexpected EOF"`) {
		t.Fatalf("expected aggregated response marked failed with read error details, got %q", rawBody)
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
	if err == nil {
		t.Fatal("expected first response.failed frame to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected failed response to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
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
	if err == nil {
		t.Fatal("expected explicit response.failed to return non-nil 502 for channel failure accounting")
	}
	if err.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", err.StatusCode)
	}
	body := recorder.Body.String()
	if strings.Count(body, `"type":"response.completed"`) != 0 {
		t.Fatalf("expected no synthetic completed after explicit failed, got %q", body)
	}
	if strings.Count(body, `"type":"response.failed"`) != 1 {
		t.Fatalf("expected failed payload preserved, got %q", body)
	}
}

func TestStreamResponsesHandler_IncompleteTerminalTreatedAsSuccessTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 上游以 response.incomplete 截断收尾（无 [DONE]）：应识别为成功终态，
	// 迟到的 completed 事件必须丢弃，不得改写 incomplete 终态
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_incomplete","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_incomplete","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}`,
		"",
		`event: response.incomplete`,
		`data: {"type":"response.incomplete","response":{"id":"resp_incomplete","model":"gpt-4o","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"incomplete_details":{"reason":"max_output_tokens"}}}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_incomplete","model":"gpt-4o","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":9,"total_tokens":10}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("expected incomplete terminal to be a success terminal without error, got %+v", err)
	}
	if usage == nil || usage.PromptTokens != 1 || usage.CompletionTokens != 2 || usage.TotalTokens != 3 {
		t.Fatalf("expected usage extracted on incomplete terminal same as completed, got %#v", usage)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.incomplete`) {
		t.Fatalf("expected incomplete terminal event forwarded, got %q", body)
	}
	if !strings.Contains(body, `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("expected raw incomplete_details payload preserved for client, got %q", body)
	}
	if strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected late completed event dropped after incomplete terminal, got %q", body)
	}
	// 成功终态识别后按 completed 同口径补发 done 收尾
	if !strings.Contains(body, `[DONE]`) {
		t.Fatalf("expected done marker emitted for recognized incomplete terminal, got %q", body)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body stored in context")
	}
	if !strings.Contains(rawBody, `"status":"incomplete"`) || strings.Contains(rawBody, `"status":"failed"`) {
		t.Fatalf("expected capture kept distinguishable incomplete status, got %q", rawBody)
	}
	if strings.Contains(rawBody, `"total_tokens":10`) {
		t.Fatalf("expected late completed usage not to pollute capture, got %q", rawBody)
	}
	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	respJSON, ok := capture["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected response snapshot in capture, got %#v", capture["response"])
	}
	output, ok := respJSON["output"].([]interface{})
	if !ok || len(output) != 1 {
		t.Fatalf("expected aggregated output item on incomplete terminal, got %#v", respJSON["output"])
	}
}

func TestStreamResponsesHandler_ReadErrorAfterIncompleteTerminalKeepsIncompleteCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// incomplete 终态识别后出现传输错误：与 completed 同口径，不得把捕获改写为 failed 或补发 error 事件
	streamReader := &errReader{
		chunks: []string{
			strings.Join([]string{
				`event: response.created`,
				`data: {"type":"response.created","response":{"id":"resp_incomplete_read_err","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
				"",
				`event: response.incomplete`,
				`data: {"type":"response.incomplete","response":{"id":"resp_incomplete_read_err","model":"gpt-4o","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"incomplete_details":{"reason":"content_filter"}}}`,
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
		t.Fatalf("expected read error after incomplete terminal to stay silent, got %+v", err)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected usage snapshot preserved, got %#v", usage)
	}
	body := recorder.Body.String()
	if strings.Contains(body, `event: error`) {
		t.Fatalf("expected no terminal error event when read error follows incomplete terminal, got %q", body)
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if !strings.Contains(rawBody, `"status":"incomplete"`) || strings.Contains(rawBody, `"status":"failed"`) {
		t.Fatalf("expected capture status stays incomplete after post-terminal read error, got %q", rawBody)
	}
}

func TestStreamResponsesHandler_PreservesIncompleteDetailsInCaptureSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 快照经 ResponsesResponse 结构体反序列化/序列化中介，incomplete_details 缺字段会在日志 capture 中丢失
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_inc_details","model":"gpt-4o","output":[],"status":"in_progress","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`,
		"",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"type":"message","id":"msg_inc_details","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}`,
		"",
		`event: response.incomplete`,
		`data: {"type":"response.incomplete","response":{"id":"resp_inc_details","model":"gpt-4o","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"incomplete_details":{"reason":"max_output_tokens"}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, _ := StreamResponsesHandler(c, resp)
	if err != nil {
		t.Fatalf("stream handler returned error: %+v", err)
	}

	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body stored in context")
	}
	if !strings.Contains(rawBody, `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("expected incomplete_details preserved in capture snapshot, got %q", rawBody)
	}

	var capture map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture json: %v", err)
	}
	respJSON, ok := capture["response"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected response snapshot in capture, got %#v", capture["response"])
	}
	if respJSON["status"] != "incomplete" {
		t.Fatalf("expected capture status incomplete, got %#v", respJSON["status"])
	}
	details, ok := respJSON["incomplete_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected incomplete_details object in capture snapshot, got %#v", respJSON["incomplete_details"])
	}
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("expected truncation reason preserved for log consumers, got %#v", details["reason"])
	}
}

// TestResponsesUsageDetailsReachCaptureAndInternalUsage 锁定共享 usage details 承载（报告一 P1-11 / 报告二 20、26）：
// response.completed 的 cached/cache_write/reasoning/accepted/rejected/audio/text details
// 必须在 capture 重序列化后保留协议键名，并可在网关内部 Usage 中读取；直连原始 body 字节不得被改写。
func TestResponsesUsageDetailsReachCaptureAndInternalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const completedPayload = `{"type":"response.completed","response":{"id":"resp_details","object":"response","created_at":1700000000,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-5-codex","output":[],"previous_response_id":"resp_prev_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":12},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":10,"accepted_prediction_tokens":2,"rejected_prediction_tokens":1,"audio_tokens":3,"text_tokens":34},"total_tokens":150}}}`

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_details","object":"response","created_at":1700000000,"model":"gpt-5-codex","output":[],"status":"in_progress","previous_response_id":"resp_prev_1"}}`,
		"",
		`event: response.completed`,
		`data: ` + completedPayload,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	errWithCode, _, usage := StreamResponsesHandler(c, resp)
	if errWithCode != nil {
		t.Fatalf("stream handler returned error: %+v", errWithCode)
	}

	// 1. 转发给客户端的原始帧字节必须原样保留（capture 的 wire tag 修正不得改写透传 body）
	if !strings.Contains(recorder.Body.String(), completedPayload) {
		t.Fatalf("expected forwarded SSE body to keep the original completed frame bytes, got %q", recorder.Body.String())
	}

	// 2. capture 重序列化后协议键名与 details 数值全保留
	var capture struct {
		Response map[string]any `json:"response"`
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if err := json.Unmarshal([]byte(rawBody), &capture); err != nil {
		t.Fatalf("unmarshal capture snapshot: %v (body=%s)", err, rawBody)
	}
	snap := capture.Response
	if snap == nil {
		t.Fatalf("expected response snapshot in capture, got %s", rawBody)
	}
	if _, ok := snap["created"]; ok {
		t.Fatalf("capture snapshot must not emit protocol-illegal key \"created\", got %s", rawBody)
	}
	if _, ok := snap["previous_id"]; ok {
		t.Fatalf("capture snapshot must not emit protocol-illegal key \"previous_id\", got %s", rawBody)
	}
	if snap["created_at"] != float64(1700000000) {
		t.Fatalf("expected created_at preserved in capture snapshot, got %#v", snap["created_at"])
	}
	if snap["previous_response_id"] != "resp_prev_1" {
		t.Fatalf("expected previous_response_id preserved in capture snapshot, got %#v", snap["previous_response_id"])
	}
	if snap["object"] != "response" {
		t.Fatalf("expected object preserved in capture snapshot, got %#v", snap["object"])
	}
	usageJSON, ok := snap["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage object in capture snapshot, got %#v", snap["usage"])
	}
	inputDetails, ok := usageJSON["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("expected input_tokens_details in capture snapshot, got %#v", usageJSON["input_tokens_details"])
	}
	if inputDetails["cached_tokens"] != float64(40) || inputDetails["cache_write_tokens"] != float64(12) {
		t.Fatalf("expected cached/cache_write tokens preserved in capture snapshot, got %#v", inputDetails)
	}
	outputDetails, ok := usageJSON["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("expected output_tokens_details in capture snapshot, got %#v", usageJSON["output_tokens_details"])
	}
	for key, want := range map[string]float64{
		"reasoning_tokens":           10,
		"accepted_prediction_tokens": 2,
		"rejected_prediction_tokens": 1,
		"audio_tokens":               3,
		"text_tokens":                34,
	} {
		if outputDetails[key] != want {
			t.Fatalf("expected output detail %s=%v in capture snapshot, got %#v", key, want, outputDetails)
		}
	}

	// 3. 内部 Usage details 可读（计费与日志消费路径）
	if usage == nil {
		t.Fatalf("expected usage extracted from completed stream")
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 50 || usage.TotalTokens != 150 {
		t.Fatalf("expected totals 100/50/150, got %#v", usage)
	}
	if usage.PromptTokensDetails == nil {
		t.Fatalf("expected prompt tokens details carrying cached/cache_write, got nil")
	}
	if usage.PromptTokensDetails.CachedTokens != 40 || usage.PromptTokensDetails.CacheWriteTokens != 12 {
		t.Fatalf("expected cached=40 cache_write=12 in internal usage, got %#v", usage.PromptTokensDetails)
	}
	if usage.CompletionTokensDetails == nil {
		t.Fatalf("expected completion tokens details, got nil")
	}
	if usage.CompletionTokensDetails.ReasoningTokens != 10 ||
		usage.CompletionTokensDetails.AcceptedPredictionTokens != 2 ||
		usage.CompletionTokensDetails.RejectedPredictionTokens != 1 ||
		usage.CompletionTokensDetails.AudioTokens != 3 ||
		usage.CompletionTokensDetails.TextTokens != 34 {
		t.Fatalf("expected reasoning/accepted/rejected/audio/text details, got %#v", usage.CompletionTokensDetails)
	}

	// 4. 直连非流式：原 body 字节不改写，同时内部 usage 仍携带 details
	nonStreamRecorder := httptest.NewRecorder()
	nonStreamCtx, _ := gin.CreateTestContext(nonStreamRecorder)
	nonStreamCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := `{"id":"resp_details","object":"response","created_at":1700000000,"status":"completed","model":"gpt-5-codex","output":[],"previous_response_id":"resp_prev_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":12},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":10,"accepted_prediction_tokens":2,"rejected_prediction_tokens":1,"audio_tokens":3,"text_tokens":34},"total_tokens":150}}`
	directResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	directUsage, directErr := DoResponsesResponse(nonStreamCtx, directResp, &meta.Meta{})
	if directErr != nil {
		t.Fatalf("direct non-stream handler returned error: %+v", directErr)
	}
	if nonStreamRecorder.Body.String() != body {
		t.Fatalf("direct non-stream body must stay byte-identical, got %q", nonStreamRecorder.Body.String())
	}
	if directUsage == nil || directUsage.PromptTokensDetails == nil || directUsage.PromptTokensDetails.CacheWriteTokens != 12 {
		t.Fatalf("expected direct usage carrying cache_write detail, got %#v", directUsage)
	}
	if directUsage == nil || directUsage.CompletionTokensDetails == nil || directUsage.CompletionTokensDetails.ReasoningTokens != 10 {
		t.Fatalf("expected direct usage carrying reasoning detail, got %#v", directUsage)
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
