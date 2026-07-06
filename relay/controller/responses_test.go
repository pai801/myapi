package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
)

type errReader struct {
	data []byte
	step int
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	switch r.step {
	case 0:
		r.step++
		if len(r.data) == 0 {
			return 0, io.EOF
		}
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	case 1:
		r.step++
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	default:
		return 0, io.EOF
	}
}

type errAfterFirstReadReader struct {
	data   string
	off    int
	firstN int
	err    error
}

func (r *errAfterFirstReadReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	if r.firstN == 0 {
		r.firstN = len(r.data)
	}
	n := copy(p, r.data[r.off:min(len(r.data), r.off+r.firstN)])
	r.off += n
	if r.off >= len(r.data) && r.err != nil {
		return n, r.err
	}
	return n, nil
}

func TestForwardChatResponsesStream_HandlesLargeEventBeyondScannerLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	largeContent := strings.Repeat("x", 11*1024*1024)
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_large","choices":[{"index":0,"delta":{"role":"assistant","content":"` + largeContent + `"},"finish_reason":null}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected large chat SSE event to be processed without scanner limit failure, got %v", err)
	}
	if result.StreamErrored {
		t.Fatalf("expected large chat SSE event to finish without stream error")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.output_text.delta`) {
		t.Fatalf("expected output_text delta event in converted stream")
	}
	if !strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected completed event emitted after [DONE]")
	}
	if !strings.Contains(body, `chatcmpl_large`) {
		t.Fatalf("expected response id to be preserved in converted stream")
	}
}

func TestForwardChatResponsesStream_PreservesMultiLineDataPayloadAsSingleJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_multiline","choices":[{"index":0,`,
		`data: "delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected multiline data payload to be processed, got %v", err)
	}
	if result.StreamErrored {
		t.Fatalf("expected multiline payload to finish without stream error")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.output_text.delta`) {
		t.Fatalf("expected output_text delta event in converted stream, got %q", body)
	}
	if strings.Count(body, `event: response.output_text.delta`) != 1 {
		t.Fatalf("expected multiline JSON to remain one payload, got %q", body)
	}
	if !strings.Contains(body, `"delta":"Hello"`) {
		t.Fatalf("expected multiline content to merge into Hello, got %q", body)
	}
}

func TestForwardChatResponsesStream_CompletedEOFDoesNotMarkFailedTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_success","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_success","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil && err != io.EOF {
		t.Fatalf("expected success stream to finish cleanly, got %v", err)
	}
	if !result.SuccessTerminal {
		t.Fatalf("expected success terminal")
	}
	if result.FailedTerminal {
		t.Fatalf("expected no failed terminal on eof after success")
	}
	if result.StreamErrored {
		t.Fatalf("expected no stream error on success terminal")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected completed event, got %q", body)
	}
}

func TestForwardChatResponsesStream_FailedConvertedErrorDataDoesNotCountAsSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream failed","type":"server_error","code":"bad_response"}}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil && err != io.EOF {
		t.Fatalf("expected converted error stream to finish without read error, got %v", err)
	}
	if !result.FailedTerminal {
		t.Fatalf("expected failed terminal")
	}
	if result.SuccessTerminal {
		t.Fatalf("expected failed stream not to be treated as success")
	}
	if result.FailureError == nil || result.FailureError.Message != "upstream failed" {
		t.Fatalf("expected converted error payload captured, got %#v", result.FailureError)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) && !strings.Contains(body, `event: response.failed`) {
		t.Fatalf("expected terminal error event, got %q", body)
	}
}

func TestForwardChatResponsesStream_ReturnsUnexpectedEOFWithoutCompletedOnTruncatedTail(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_partial","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		"",
		`data`,
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err == nil {
		t.Fatalf("expected truncated tail to return error")
	}
	if !result.StreamErrored {
		t.Fatalf("expected truncated tail to report stream error")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.output_text.delta`) {
		t.Fatalf("expected valid frames before truncation to be flushed, got %q", body)
	}
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected truncated tail to emit terminal error event, got %q", body)
	}
	if strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected no completed event after truncated tail, got %q", body)
	}
	if strings.Contains(body, `[DONE]`) {
		t.Fatalf("expected no done marker after truncated tail, got %q", body)
	}
}

func TestForwardChatResponsesStream_EmitsTerminalErrorEventOnReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	streamReader := &errReader{
		data: []byte(strings.Join([]string{
			`data: {"id":"chatcmpl_stream_error","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
			"",
			"",
		}, "\n")),
		err: io.ErrUnexpectedEOF,
	}

	var converterState any
	result, err := forwardChatResponsesStream(c, streamReader, []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err == nil {
		t.Fatalf("expected read failure to be returned")
	}
	if !result.StreamErrored {
		t.Fatalf("expected read failure to report stream error")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected terminal error event after read failure, got %q", body)
	}
	if !strings.Contains(body, `"message":"unexpected EOF"`) {
		t.Fatalf("expected terminal error payload to expose read failure, got %q", body)
	}
	if strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected no completed event after read failure, got %q", body)
	}
	if strings.Contains(body, `[DONE]`) {
		t.Fatalf("expected no done marker after read failure, got %q", body)
	}
}

func TestForwardChatResponsesStream_TruncatedTailBlocksCompletedCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_partial_capture","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		"",
		`data`,
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err == nil {
		t.Fatalf("expected truncated tail to return error")
	}
	if !result.StreamErrored {
		t.Fatalf("expected truncated tail to report stream error")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected terminal error event after truncation, got %q", body)
	}
	if strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected no completed event after truncation, got %q", body)
	}
}

func TestForwardChatResponsesStream_ReturnsErrorOnReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	streamReader := &errReader{
		data: []byte(`data: {"id":"chatcmpl_stream_error","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}` + "\n\n"),
		err:  io.ErrUnexpectedEOF,
	}

	var converterState any
	result, err := forwardChatResponsesStream(c, streamReader, []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err == nil {
		t.Fatalf("expected read failure to be returned")
	}
	if !result.StreamErrored {
		t.Fatalf("expected read failure to report stream error")
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected terminal error event after read failure, got %q", body)
	}
}

func TestForwardChatResponsesStream_CompletedThenReadErrorDoesNotRetainCompletedBodyForCaller(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	requestBody := []byte(`{"model":"gpt-4o"}`)
	streamReader := &errAfterFirstReadReader{
		data: strings.Join([]string{
			`data: {"id":"chatcmpl_completed_before_error","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n"),
		err: io.ErrUnexpectedEOF,
	}

	var converterState any
	result, err := forwardChatResponsesStream(c, streamReader, requestBody, &converterState, false)
	if err == nil {
		t.Fatalf("expected completed-then-read-error stream to return error")
	}
	if !result.StreamErrored {
		t.Fatalf("expected late read error after completed signal to mark stream errored")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) {
		t.Fatalf("expected terminal error event after transport read failure, got %q", body)
	}
	if strings.Contains(body, `[DONE]`) {
		t.Fatalf("expected read error after completion to suppress done marker, got %q", body)
	}
}

func TestForwardChatResponsesStream_CleanEOFSynthesizesCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_missing_terminal","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected clean EOF without terminal to finish cleanly, got %v", err)
	}
	if result.StreamErrored || result.FailedTerminal {
		t.Fatalf("expected clean EOF without terminal to stay successful, got %+v", result)
	}
	if !result.SuccessTerminal {
		t.Fatalf("expected synthesized completed event on clean EOF with accumulated state")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.output_text.delta`) {
		t.Fatalf("expected valid frames before eof to be flushed, got %q", body)
	}
	if !strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected synthesized completed event on clean EOF with accumulated state, got %q", body)
	}
}

func TestForwardChatResponsesStream_FailedTerminalMarksStreamErrored(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream failed","type":"server_error","code":"request_failed"}}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected failed terminal event stream to finish cleanly, got %v", err)
	}
	if !result.FailedTerminal || !result.StreamErrored {
		t.Fatalf("expected failed terminal event to mark stream errored, got %+v", result)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.failed`) {
		t.Fatalf("expected failed terminal event in converted output, got %q", body)
	}
}

func TestRelayResponsesConverted_StreamFailedTerminalAfterHeadersReturnsNil(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","stream":true,"input":"hello"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream failed","type":"server_error","code":"bad_response"}}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o","stream":true,"input":"hello"}`), &converterState, false)
	if err != nil && err != io.EOF {
		t.Fatalf("expected converted error stream to finish without read error, got %v", err)
	}
	if !result.FailedTerminal {
		t.Fatalf("expected failed terminal from converted error stream")
	}
	if result.SuccessTerminal {
		t.Fatalf("expected failed terminal not to be treated as success")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed stream to keep HTTP 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: error`) && !strings.Contains(body, `event: response.failed`) {
		t.Fatalf("expected SSE failure event in body, got %q", body)
	}
	if strings.Contains(body, `{"error":`) && !strings.Contains(body, `data: {"error"`) {
		t.Fatalf("expected no synthesized top-level JSON error body, got %q", body)
	}
}

func TestRelayResponsesConverted_StreamFailedTerminalStaysSSEForConvertedChatError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream failed","type":"server_error","code":"bad_response"}}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o","stream":true}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected converted chat error stream to finish cleanly, got %v", err)
	}
	if !result.FailedTerminal {
		t.Fatalf("expected failed terminal")
	}

	if got := recorder.Code; got != http.StatusOK {
		t.Fatalf("expected status 200 after SSE started, got %d", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected SSE content type, got %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.failed`) && !strings.Contains(body, `event: error`) {
		t.Fatalf("expected failure SSE event, got %q", body)
	}
	if strings.Contains(body, `status_code`) || strings.Contains(body, `"status":502`) {
		t.Fatalf("expected no HTTP 502-style payload after SSE headers committed, got %q", body)
	}
}

func TestRelayResponsesConverted_StreamFailedTerminalStaysSSEForResponseFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream failed","type":"server_error","code":"request_failed"}}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o","stream":true}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected response.failed stream to finish cleanly, got %v", err)
	}
	if !result.FailedTerminal {
		t.Fatalf("expected failed terminal")
	}

	if got := recorder.Code; got != http.StatusOK {
		t.Fatalf("expected status 200 after SSE started, got %d", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("expected SSE content type, got %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.failed`) {
		t.Fatalf("expected response.failed SSE event, got %q", body)
	}
	if strings.Contains(body, `status_code`) || strings.Contains(body, `"status":502`) {
		t.Fatalf("expected no HTTP 502-style payload after SSE headers committed, got %q", body)
	}
}
