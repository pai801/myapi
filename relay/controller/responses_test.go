package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/ctxkey"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/apitype"
	metaPkg "github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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

func TestForwardChatResponsesStream_IncompleteTerminalStopsReadWithoutFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	// incomplete 终态后上游连接被掐断：若终态不被识别，转发循环会继续读至传输错误，
	// 使成功的截断响应被误判为 FailedTerminal（上层据此回滚额度）
	streamReader := &errAfterFirstReadReader{
		data: strings.Join([]string{
			`data: {"id":"chatcmpl_incomplete","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n") + "\n",
		err: io.ErrUnexpectedEOF,
	}

	var converterState any
	result, err := forwardChatResponsesStream(c, streamReader, []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil {
		t.Fatalf("expected incomplete terminal to stop reading before transport error, got %v", err)
	}
	if !result.SuccessTerminal || !result.IncompleteTerminal || !result.TerminalSeen {
		t.Fatalf("expected incomplete terminal to be recognized as success terminal, got %+v", result)
	}
	if result.FailedTerminal || result.StreamErrored || result.FailureError != nil {
		t.Fatalf("expected incomplete terminal not to be treated as failure, got %+v", result)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.incomplete`) {
		t.Fatalf("expected incomplete terminal event forwarded to client, got %q", body)
	}
	if strings.Count(body, `event: response.incomplete`) != 1 {
		t.Fatalf("expected exactly one incomplete terminal event, got %q", body)
	}
	if strings.Contains(body, `event: response.completed`) {
		t.Fatalf("expected incomplete terminal not to be rewritten as completed, got %q", body)
	}
	if strings.Contains(body, `event: error`) {
		t.Fatalf("expected no terminal error event after early return on incomplete terminal, got %q", body)
	}
}

func TestForwardChatResponsesStream_IncompleteTerminalExposesUsageAndBodyForBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_incomplete_usage","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	requestBody := []byte(`{"model":"gpt-4o"}`)
	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), requestBody, &converterState, false)
	if err != nil {
		t.Fatalf("expected incomplete terminal stream to finish cleanly, got %v", err)
	}
	if !result.SuccessTerminal || !result.IncompleteTerminal {
		t.Fatalf("expected incomplete terminal to be recognized, got %+v", result)
	}
	if result.StreamErrored || result.FailedTerminal {
		t.Fatalf("expected incomplete terminal to stay on success path, got %+v", result)
	}

	// 与 relayResponsesConverted 消费口径一致：usage 与日志响应体从转换器状态提取
	pt, ct, tt, _ := codex.GetStreamUsage(converterState)
	if pt != 2 || ct != 3 || tt != 5 {
		t.Fatalf("expected usage 2/3/5 extracted on incomplete terminal, got %d/%d/%d", pt, ct, tt)
	}
	terminalBody := codex.GetStreamCompletedBody(converterState, requestBody)
	if terminalBody == nil {
		t.Fatalf("expected terminal response body available for logging on incomplete terminal")
	}
	if !strings.Contains(string(terminalBody), `"status":"incomplete"`) {
		t.Fatalf("expected terminal body to keep incomplete status for distinction, got %s", terminalBody)
	}
	if !strings.Contains(string(terminalBody), `max_output_tokens`) {
		t.Fatalf("expected incomplete_details reason preserved in terminal body, got %s", terminalBody)
	}
}

func TestParseConvertedEventMeta_TerminalClassification(t *testing.T) {
	completed := parseConvertedEventMeta("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	if !completed.Completed || completed.Incomplete || completed.Failed {
		t.Fatalf("expected completed meta to set only Completed, got %+v", completed)
	}

	incomplete := parseConvertedEventMeta("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n")
	if !incomplete.Incomplete || incomplete.Completed || incomplete.Failed {
		t.Fatalf("expected incomplete meta to set only Incomplete, got %+v", incomplete)
	}

	failed := parseConvertedEventMeta("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"boom\",\"type\":\"server_error\",\"code\":\"request_failed\"}}}\n\n")
	if !failed.Failed || failed.Completed || failed.Incomplete {
		t.Fatalf("expected failed meta to set only Failed, got %+v", failed)
	}
	if failed.StreamErr == nil || failed.StreamErr.Message != "boom" {
		t.Fatalf("expected failed meta to carry stream error, got %+v", failed.StreamErr)
	}

	normal := parseConvertedEventMeta("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
	if normal.Completed || normal.Incomplete || normal.Failed {
		t.Fatalf("expected non-terminal meta to set no terminal flag, got %+v", normal)
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

func TestHandleResponsesDirectNonStream_PassthroughAndUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	body := `{"id":"resp_x","object":"response","status":"completed","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":40}},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, relayErr := handleResponsesDirectNonStream(c, resp)
	if relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}
	if usage == nil || usage.PromptTokens != 100 || usage.CompletionTokens != 50 || usage.TotalTokens != 150 {
		t.Fatalf("expected usage 100/50/150, got %+v", usage)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 40 {
		t.Fatalf("expected cached tokens 40, got %+v", usage.PromptTokensDetails)
	}
	if recorder.Body.String() != body {
		t.Fatalf("expected passthrough body, got %q", recorder.Body.String())
	}
	if c.GetString(ctxkey.ResponseBody) != body {
		t.Fatalf("expected response body stored in ctx, got %q", c.GetString(ctxkey.ResponseBody))
	}
}

// TestHandleResponsesDirectNonStreamUsageDetailsReachInternalUsage 锁定直连 /v1/responses 的 usage 消费：
// cached/cache_write 与 reasoning details 必须进入内部 Usage，同时原 body 字节保持上游原样（报告一 P1-11）。
func TestHandleResponsesDirectNonStreamUsageDetailsReachInternalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	body := `{"id":"resp_d","object":"response","created_at":1700000000,"status":"completed","model":"deepseek-reasoner","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":12},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":10,"accepted_prediction_tokens":2,"rejected_prediction_tokens":1,"audio_tokens":3,"text_tokens":34},"total_tokens":150}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, relayErr := handleResponsesDirectNonStream(c, resp)
	if relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}
	if usage == nil {
		t.Fatalf("expected usage extracted")
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 40 || usage.PromptTokensDetails.CacheWriteTokens != 12 {
		t.Fatalf("expected cached=40 cache_write=12 in internal usage, got %#v", usage.PromptTokensDetails)
	}
	if usage.CompletionTokensDetails == nil {
		t.Fatalf("expected completion tokens details, got nil")
	}
	cd := usage.CompletionTokensDetails
	if cd.ReasoningTokens != 10 || cd.AcceptedPredictionTokens != 2 || cd.RejectedPredictionTokens != 1 || cd.AudioTokens != 3 || cd.TextTokens != 34 {
		t.Fatalf("expected reasoning/accepted/rejected/audio/text details, got %#v", cd)
	}
	if recorder.Body.String() != body {
		t.Fatalf("direct passthrough body must stay byte-identical, got %q", recorder.Body.String())
	}
}

func TestHandleResponsesDirectNonStream_ErrorBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad effort","type":"invalid_request_error","code":"invalid_request_error"}}`)),
	}

	_, relayErr := handleResponsesDirectNonStream(c, resp)
	if relayErr == nil {
		t.Fatal("expected relay error for error body")
	}
	if !strings.Contains(relayErr.Error.Message, "bad effort") {
		t.Fatalf("expected error message forwarded, got %q", relayErr.Error.Message)
	}
}

func TestHandleResponsesDirectStream_PassthroughAndUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_s","object":"response","status":"in_progress"}}`,
		"",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[{"type":"output_text","text":"","annotations":[]}]}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" World"}`,
		"",
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello World","annotations":[]}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_s","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}

	usage, relayErr := handleResponsesDirectStream(c, resp)
	if relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 20 || usage.TotalTokens != 30 {
		t.Fatalf("expected usage 10/20/30 from response.completed, got %+v", usage)
	}
	got := recorder.Body.String()
	for _, want := range []string{`event: response.created`, `event: response.output_item.added`, `event: response.output_text.delta`, `event: response.output_text.done`, `event: response.completed`, `"input_tokens":10`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected SSE passthrough containing %q, got %q", want, got)
		}
	}

	// 日志记录：SSE 流应合并为完整 response JSON（delta 拼接 + 快照吸收）
	var merged map[string]any
	if err := json.Unmarshal([]byte(c.GetString(ctxkey.ResponseBody)), &merged); err != nil {
		t.Fatalf("expected merged JSON body in ctx, got %q (err %v)", c.GetString(ctxkey.ResponseBody), err)
	}
	if merged["id"] != "resp_s" || merged["status"] != "completed" {
		t.Fatalf("expected id/status absorbed from snapshots, got %+v", merged)
	}
	output, _ := merged["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 merged output item, got %+v", output)
	}
	item, _ := output[0].(map[string]any)
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content part, got %+v", content)
	}
	part, _ := content[0].(map[string]any)
	if part["text"] != "Hello World" {
		t.Fatalf("expected delta merged text \"Hello World\", got %q", part["text"])
	}
}

func TestResponsesStreamAccumulator_MergesFunctionCallArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"get_weather","arguments":"","status":"in_progress"}}`,
		"",
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"city\":\""}`,
		"",
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"beijing\"}"}`,
		"",
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"city\":\"beijing\"}"}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_fc","object":"response","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}

	if _, relayErr := handleResponsesDirectStream(c, resp); relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}

	var merged map[string]any
	if err := json.Unmarshal([]byte(c.GetString(ctxkey.ResponseBody)), &merged); err != nil {
		t.Fatalf("expected merged JSON body in ctx, got %q (err %v)", c.GetString(ctxkey.ResponseBody), err)
	}
	output, _ := merged["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 merged function_call item, got %+v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "get_weather" {
		t.Fatalf("expected function_call item, got %+v", item)
	}
	if item["arguments"] != `{"city":"beijing"}` {
		t.Fatalf("expected merged arguments, got %q", item["arguments"])
	}
}

func TestHandleResponsesDirectStream_NoUsageEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_s"}}`,
			"",
		}, "\n"))),
	}

	usage, relayErr := handleResponsesDirectStream(c, resp)
	if relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}
	if usage == nil {
		t.Fatal("expected non-nil usage (empty) so quota rollback path works")
	}
	if usage.TotalTokens != 0 {
		t.Fatalf("expected zero usage, got %+v", usage)
	}
	gotBody := c.GetString(ctxkey.ResponseBody)
	if gotBody == "" {
		t.Fatal("expected merged JSON body stored in ctx")
	}
	var gotMap map[string]any
	if err := json.Unmarshal([]byte(gotBody), &gotMap); err != nil {
		t.Fatalf("expected valid JSON in ctx, got %q (err %v)", gotBody, err)
	}
	if gotMap["id"] != "resp_s" {
		t.Fatalf("expected created snapshot merged, got %q", gotBody)
	}
	if _, ok := gotMap["output"].([]any); !ok {
		t.Fatalf("expected output array present, got %q", gotBody)
	}
}

// =============================================================================
// T6 契约测试：relayResponsesConverted 错误边界（fix-plan Task T6）
// =============================================================================

// newConvertedRelayTestContext 构造带 Responses 请求体的测试上下文与可观测假 upstream
// （调用计数 + 捕获的 chat 请求体，供"丢弃后上游收到无对应字段/工具"断言）。
func newConvertedRelayTestContext(body string) (*gin.Context, *httptest.ResponseRecorder, *httptest.Server, *int32, *string) {
	var upstreamCalls int32
	var capturedChatRequest string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		if raw, err := io.ReadAll(r.Body); err == nil {
			capturedChatRequest = string(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_unexpected","object":"chat.completion","created":1,"choices":[],"model":"m"}`))
	}))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder, upstream, &upstreamCalls, &capturedChatRequest
}

func convertedRelayMeta(upstreamURL string) *metaPkg.Meta {
	return &metaPkg.Meta{
		APIType:         apitype.OpenAI,
		ChannelType:     1,
		ChannelId:       42,
		UserId:          7,
		Group:           "default",
		OriginModelName: "gpt-test",
		ActualModelName: "gpt-test",
		BaseURL:         upstreamURL,
		RequestURLPath:  "/v1/responses",
	}
}

// waitResponsesQuotaRestored 轮询等待额度恢复到期望值：relayResponsesConverted 成功路径的
// post-consume 在独立 goroutine 中落库，等待其在断言/清理前完成，避免测试进程访问已关闭 DB。
func waitResponsesQuotaRestored(t *testing.T, db *gorm.DB, userId int, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got int64
		if err := db.Model(&dbmodel.User{}).Where("id = ?", userId).Select("quota").Find(&got).Error; err == nil && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("quota did not settle back to %d", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRelayResponsesConvertedDropsUnmappableAndReachesUpstream 锁定能力性丢弃策略的完整链路：
// 非能力性不可映射的 Responses 请求（item_reference / DN-6 白名单外工具 / DN-7 ultrafast）
// 在请求转换层丢弃后必须放行到 chat 上游，上游收到的 chat 请求不含对应字段/工具。
func TestRelayResponsesConvertedDropsUnmappableAndReachesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 完整链路会真实发出上游请求：初始化 relay HTTP 客户端（测试进程内全局，一次性幂等）。
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	Convey("G: 能力性不可映射 Responses 请求 + 可观测假 upstream | W: relayResponsesConverted | T: 丢弃后放行、上游无对应字段/工具", t, func() {
		Convey("item_reference 丢弃、合法消息保留 → 200，上游调用 1 次且 chat 请求无 item_reference", func() {
			c, recorder, upstream, calls, captured := newConvertedRelayTestContext(`{"model":"gpt-test","input":[{"type":"message","role":"user","content":"hi"},{"type":"item_reference","id":"itm_1"}]}`)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.UserId = testUserId

			relayErr := relayResponsesConverted(c, meta)
			So(relayErr, ShouldBeNil)
			So(atomic.LoadInt32(calls), ShouldEqual, 1)
			So(strings.Contains(*captured, "item_reference"), ShouldBeFalse)
			So(strings.Contains(*captured, `"content":"hi"`), ShouldBeTrue)
			So(recorder.Code, ShouldEqual, http.StatusOK)
			waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		})

		Convey("DN-6 白名单外内置工具丢弃 → 200，上游 chat 请求无 web_search/tools", func() {
			c, recorder, upstream, calls, captured := newConvertedRelayTestContext(`{"model":"gpt-test","input":[{"type":"message","role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.UserId = testUserId

			relayErr := relayResponsesConverted(c, meta)
			So(relayErr, ShouldBeNil)
			So(atomic.LoadInt32(calls), ShouldEqual, 1)
			So(strings.Contains(*captured, "web_search"), ShouldBeFalse)
			So(strings.Contains(*captured, `"tools"`), ShouldBeFalse)
			So(recorder.Code, ShouldEqual, http.StatusOK)
			waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		})

		Convey("DN-7 service_tier ultrafast 丢弃 → 200，上游 chat 请求无 ultrafast", func() {
			c, recorder, upstream, calls, captured := newConvertedRelayTestContext(`{"model":"gpt-test","input":[{"type":"message","role":"user","content":"hi"}],"service_tier":"ultrafast"}`)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.UserId = testUserId

			relayErr := relayResponsesConverted(c, meta)
			So(relayErr, ShouldBeNil)
			So(atomic.LoadInt32(calls), ShouldEqual, 1)
			So(strings.Contains(*captured, "ultrafast"), ShouldBeFalse)
			So(recorder.Code, ShouldEqual, http.StatusOK)
			waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		})
	})
}

func TestRelayResponsesConvertedStopsBeforeUpstreamOnConversionError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	Convey("G: 结构性非法/空 messages Responses 请求 + 可观测假 upstream | W: relayResponsesConverted | T: HTTP 400、无预扣费、upstream 调用次数 0", t, func() {
		Convey("非法 JSON 请求 → 400 code=invalid_source_json（稳定机器码）", func() {
			c, recorder, upstream, calls, _ := newConvertedRelayTestContext(`{"model": `)
			defer upstream.Close()

			relayErr := relayResponsesConverted(c, convertedRelayMeta(upstream.URL))
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadRequest)
			So(relayErr.Error.Type, ShouldEqual, "invalid_request_error")
			So(relayErr.Error.Code, ShouldEqual, model.CodeInvalidSourceJSON)
			// 转换错误发生在额度查询/减少与 DoRequest 前：本测试未初始化 DB/quota 依赖，
			// 若执行顺序回退，CacheGetUserQuota 失败会返回 500 get_user_quota_failed 而非 400；
			// 假 upstream 计数 0 直接锁定未发出上游请求，recorder 无 200/响应体。
			So(atomic.LoadInt32(calls), ShouldEqual, 0)
			So(recorder.Body.Len(), ShouldEqual, 0)
		})

		Convey("能力性内容全部丢弃后 messages 为空 → 400 unsupported_mapping（空上下文无法执行，非能力性拒绝）", func() {
			c, recorder, upstream, calls, _ := newConvertedRelayTestContext(`{"model":"gpt-test","input":[{"type":"item_reference","id":"itm_1"}]}`)
			defer upstream.Close()

			relayErr := relayResponsesConverted(c, convertedRelayMeta(upstream.URL))
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadRequest)
			So(relayErr.Error.Type, ShouldEqual, "invalid_request_error")
			So(relayErr.Error.Code, ShouldEqual, model.CodeUnsupportedMapping)
			So(atomic.LoadInt32(calls), ShouldEqual, 0)
			So(recorder.Body.Len(), ShouldEqual, 0)
		})
	})
}

func TestRelayResponsesConvertedNonStreamRejectsMalformedUpstreamWith502(t *testing.T) {
	gin.SetMode(gin.TestMode)

	Convey("G: 畸形 Chat 上游响应 | W: 非流式转换写回边界 | T: 502 invalid_upstream_response，无 200、无原样透传", t, func() {
		Convey("上游 body 非法 JSON → helper 返回 502，未写 200、未设置成功 body", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			// ctx 不含预扣费标记：rollback helper 为 no-op（真实链路中该值由预扣费写入后由 helper 回滚）
			ctx := context.Background()
			ctxMeta := convertedRelayMeta("http://127.0.0.1:0")

			converted, usage, relayErr := convertAndWriteResponsesNonStream(c, ctx, ctxMeta, []byte(`{"id": `), "gpt-test", false, []byte(`{"model":"gpt-test","input":"hi"}`))
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadGateway)
			So(relayErr.Error.Type, ShouldEqual, "upstream_error")
			So(relayErr.Error.Code, ShouldEqual, "invalid_upstream_response")
			So(strings.Contains(relayErr.Error.Message, model.CodeInvalidSourceJSON), ShouldBeTrue)
			So(converted, ShouldBeNil)
			So(usage, ShouldBeNil)
			// 不得把 chat.completion 原样发给 Responses 客户端：转换失败前不得写 200/成功 body
			So(recorder.Body.Len(), ShouldEqual, 0)
		})

		Convey("上游 tool call 缺 id → 502 malformed（消息携带稳定机器码），无透传", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctxMeta := convertedRelayMeta("http://127.0.0.1:0")
			badUpstream := `{"id":"chatcmpl_bad","object":"chat.completion","created":1700000000,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`

			converted, _, relayErr := convertAndWriteResponsesNonStream(c, context.Background(), ctxMeta, []byte(badUpstream), "gpt-test", false, []byte(`{"model":"gpt-test","input":"hi"}`))
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadGateway)
			So(strings.Contains(relayErr.Error.Message, model.CodeMalformedToolCall), ShouldBeTrue)
			So(converted, ShouldBeNil)
			So(recorder.Body.Len(), ShouldEqual, 0)
		})

		Convey("合法上游 Chat 响应 → 200 写 Responses 对象（非 chat.completion 透传）且 usage 提取", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctxMeta := convertedRelayMeta("http://127.0.0.1:0")
			goodUpstream := `{"id":"chatcmpl_ok","object":"chat.completion","created":1700000000,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`

			converted, usage, relayErr := convertAndWriteResponsesNonStream(c, context.Background(), ctxMeta, []byte(goodUpstream), "gpt-test", false, []byte(`{"model":"gpt-test","input":"hi"}`))
			So(relayErr, ShouldBeNil)
			So(converted, ShouldNotBeNil)
			var top map[string]interface{}
			So(json.Unmarshal(converted, &top), ShouldBeNil)
			So(top["object"], ShouldEqual, "response")
			_, isChatCompletion := top["choices"]
			So(isChatCompletion, ShouldBeFalse)
			So(recorder.Code, ShouldEqual, http.StatusOK)
			body := recorder.Body.String()
			So(strings.Contains(body, "\"object\":\"response\""), ShouldBeTrue)
			So(usage != nil && usage.PromptTokens == 11 && usage.CompletionTokens == 7, ShouldBeTrue)
			So(usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens == 3, ShouldBeTrue)
		})
	})
}

// ============================================================================
// T7（报告一 P0-1 stream/P1-10）：forwardChatResponsesStream 转换错误终态链。
// SSE header 已提交后出错：只按 Responses 协议写错误事件终态（unique response.failed），
// 停止读流并置 FailedTerminal/StreamErrored/FailureError（controller 据此回滚预扣费），
// 不得再附加 JSON error body，不得合成 completed。
// ============================================================================

// assertSSEFramesOnly 断言已提交 body 只由 SSE 帧行组成（event:/data: 前缀 + 空行分隔）。
func assertSSEFramesOnly(t *testing.T, body string) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "event: ") && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-SSE content appended after committed stream header: %q (body=%q)", line, body)
		}
	}
}

func TestForwardChatResponsesStream_MalformedChunkWritesFailedTerminalAndStops(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_mal","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_mal","choices":[{"index":0,"delta":{"content":"broken",`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err == nil {
		t.Fatalf("expected conversion error surfaced from forwarder, got nil")
	}
	if !result.FailedTerminal || !result.StreamErrored {
		t.Fatalf("expected FailedTerminal+StreamErrored flags, got %+v", result)
	}
	if result.SuccessTerminal || result.IncompleteTerminal {
		t.Fatalf("malformed stream must not be success, got %+v", result)
	}
	if result.FailureError == nil || fmt.Sprint(result.FailureError.Code) != model.CodeInvalidStreamEvent {
		t.Fatalf("expected FailureError code invalid_stream_event, got %#v", result.FailureError)
	}
	body := recorder.Body.String()
	if strings.Count(body, "event: response.failed") != 1 {
		t.Fatalf("expected exactly one failed terminal event, got %q", body)
	}
	if strings.Contains(body, "event: response.completed") || strings.Contains(body, "event: response.incomplete") {
		t.Fatalf("malformed chunk must not produce success terminal, got %q", body)
	}
	assertSSEFramesOnly(t, body)
}

func TestForwardChatResponsesStream_MalformedToolLifecycleFailsWithoutOrphanDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"id":"chatcmpl_mtool","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_m1","type":"function","function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_mtool","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`), &converterState, false)
	if err == nil {
		t.Fatalf("expected malformed_tool_call error surfaced from forwarder")
	}
	if !result.FailedTerminal || !result.StreamErrored {
		t.Fatalf("expected failed flags, got %+v", result)
	}
	if result.FailureError == nil || fmt.Sprint(result.FailureError.Code) != model.CodeMalformedToolCall {
		t.Fatalf("expected FailureError code malformed_tool_call, got %#v", result.FailureError)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("malformed tool call must not produce completed, got %q", body)
	}
	if strings.Contains(body, "event: response.output_item.done") {
		t.Fatalf("never-added tool call must not emit orphan output_item.done, got %q", body)
	}
	if strings.Count(body, "event: response.failed") != 1 {
		t.Fatalf("expected exactly one failed terminal, got %q", body)
	}
	assertSSEFramesOnly(t, body)
}

func TestForwardChatResponsesStream_UpstreamErrorChunkRemainsNonConversionError(t *testing.T) {
	// 协议合法的上游 error chunk（chat 兼容上游在 200 SSE 内报错）：
	// 转换器产出 failed 终态但不得升级为转换错误（err 为 nil，仅 FailedTerminal 标志驱动回滚）
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream busy","type":"server_error","code":"server_error"}}`,
		"",
	}, "\n")

	var converterState any
	result, err := forwardChatResponsesStream(c, strings.NewReader(stream), []byte(`{"model":"gpt-4o"}`), &converterState, false)
	if err != nil {
		t.Fatalf("upstream error chunk is a legal terminal, expected nil conversion error, got %v", err)
	}
	if !result.FailedTerminal || result.FailureError == nil || result.FailureError.Message != "upstream busy" {
		t.Fatalf("expected failed terminal flags with upstream message, got %+v", result)
	}
}

// TestResponsesStreamFailureError_CommittedStreamFailureReturnsNonNil 锁定 P1-2 修复：
// codex 方向转换流在 SSE 已提交后失败必须回传非 nil 的 502 错误，供渠道失败记账消费；
// 复用 forwarder 已归类的 FailureError（稳定机器码），缺失时以 streamErr/通用信息兜底。
func TestResponsesStreamFailureError_CommittedStreamFailureReturnsNonNil(t *testing.T) {
	Convey("已提交 SSE 后的流失败错误映射", t, func() {
		Convey("FailureError 优先并保留稳定机器码", func() {
			result := chatResponsesStreamResult{
				StreamErrored:  true,
				FailedTerminal: true,
				FailureError:   &model.Error{Message: "upstream failed", Type: "upstream_error", Code: model.CodeInvalidStreamEvent},
			}
			relayErr := responsesStreamFailureError(result, errors.New("read err"))
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadGateway)
			So(relayErr.Error.Message, ShouldEqual, "upstream failed")
			So(relayErr.Error.Type, ShouldEqual, "upstream_error")
			So(fmt.Sprint(relayErr.Error.Code), ShouldEqual, model.CodeInvalidStreamEvent)
		})

		Convey("无 FailureError 时以 streamErr 兜底", func() {
			result := chatResponsesStreamResult{StreamErrored: true, FailedTerminal: true}
			relayErr := responsesStreamFailureError(result, errors.New("transport boom"))
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadGateway)
			So(strings.Contains(relayErr.Error.Message, "transport boom"), ShouldBeTrue)
			So(relayErr.Error.Type, ShouldEqual, "upstream_error")
			So(relayErr.Error.Code, ShouldEqual, "invalid_upstream_response")
		})

		Convey("仅 FailedTerminal 无细节仍返回非 nil 502", func() {
			relayErr := responsesStreamFailureError(chatResponsesStreamResult{FailedTerminal: true}, nil)
			So(relayErr, ShouldNotBeNil)
			So(relayErr.StatusCode, ShouldEqual, http.StatusBadGateway)
			So(relayErr.Error.Message, ShouldNotBeEmpty)
			So(relayErr.Error.Code, ShouldEqual, "invalid_upstream_response")
		})
	})
}

// responsesBillingTestDBSeq 为每个计费链路测试生成独立的内存 sqlite 库名，隔离用例间数据。
var responsesBillingTestDBSeq int64

// setupResponsesBillingTestDB 构造内存 sqlite 并注入全局 DB/关闭 Redis，供 relayResponsesConverted
// 预扣费与回滚链路做可观测断言；T.Cleanup 恢复全局状态，避免污染同包其他用例。
func setupResponsesBillingTestDB(t *testing.T, userId int, quota int64) *gorm.DB {
	t.Helper()

	prevRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = prevRedisEnabled })

	dsn := fmt.Sprintf("file:responses_billing_%d?mode=memory&cache=shared", atomic.AddInt64(&responsesBillingTestDBSeq, 1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	// 串行化连接：内存 sqlite 下并发写（post-consume 回滚 goroutine 与断言查询）会触发 SQLITE_BUSY，
	// 令回滚写丢失、额度无法复原；单连接消除该竞态。
	if sqlDB, dbErr := db.DB(); dbErr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(&dbmodel.User{}); err != nil {
		t.Fatalf("migrate user table: %v", err)
	}
	if err := db.Create(&dbmodel.User{Id: userId, Username: fmt.Sprintf("responses-billing-%d", userId), Quota: quota}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	prevDB := dbmodel.DB
	dbmodel.DB = db
	t.Cleanup(func() {
		dbmodel.DB = prevDB
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestRelayResponsesConverted_CommittedStreamFailureReturns502AndRollsBackOnce 锁定批次一 P2-1 分支级回归：
// 走完整 relayResponsesConverted 链路（httptest 假上游返回畸形 Chat SSE）驱动 :342 失败分支。
// 反向验证：把 :342 改回 return nil 时，本用例必须因 relayErr == nil 变红。
func TestRelayResponsesConverted_CommittedStreamFailureReturns502AndRollsBackOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 完整链路会真实发出上游请求：初始化 relay HTTP 客户端（测试进程内全局，一次性幂等）。
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// 统计额度 UPDATE 次数（构造器注册在 AutoMigrate/seed 之后，避免迁移噪声）：
	// 预扣 1 次 Decrease + 失败回滚 1 次 Increase = 2；若误入 post-consume 会多出第 3 次。
	var quotaUpdates int32
	db.Callback().Update().Before("gorm:update").Register("test:count_quota_updates", func(tx *gorm.DB) {
		atomic.AddInt32(&quotaUpdates, 1)
	})

	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 首帧即为畸形 Chat chunk：SSE 头已提交后转换失败，触发失败终态分支。
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_mal\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"broken\",\n\n"))
	}))
	defer upstream.Close()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	// input 用数组形态：estimateResponsesPromptTokens 走 len*100 估算，无需初始化 tiktoken。
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	ctxMeta := convertedRelayMeta(upstream.URL)
	ctxMeta.UserId = testUserId

	relayErr := relayResponsesConverted(c, ctxMeta)

	// 1) 核心断言：SSE 已提交后的流失败必须回传非 nil 502（改回 return nil 立即变红）。
	if relayErr == nil {
		t.Fatal("expected non-nil 502 error for committed-stream failure, got nil")
	}
	if relayErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d (%+v)", relayErr.StatusCode, relayErr.Error)
	}
	if relayErr.Error.Type != "upstream_error" {
		t.Fatalf("expected upstream_error type, got %q", relayErr.Error.Type)
	}
	// 保留 forwarder 已归类的稳定机器码（畸形 chunk → invalid_stream_event），不得被兜底值覆盖。
	if got := fmt.Sprint(relayErr.Error.Code); got != model.CodeInvalidStreamEvent {
		t.Fatalf("expected preserved code %q, got %q", model.CodeInvalidStreamEvent, got)
	}
	// 已提交的客户端流保持 200，且只能包含 SSE 帧，不得追加 JSON 错误体/第二个终态。
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected committed SSE to keep HTTP 200, got %d", recorder.Code)
	}
	assertSSEFramesOnly(t, recorder.Body.String())

	// 2) 计费闭环：预扣费恰好回滚一次，未进入 post-consume。
	var gotQuota int64
	if err := db.Model(&dbmodel.User{}).Where("id = ?", testUserId).Select("quota").Find(&gotQuota).Error; err != nil {
		t.Fatalf("read back quota: %v", err)
	}
	if gotQuota != initialQuota {
		t.Fatalf("expected quota rolled back to %d, got %d", initialQuota, gotQuota)
	}
	if n := atomic.LoadInt32(&quotaUpdates); n != 2 {
		t.Fatalf("expected exactly 2 quota UPDATEs (pre-consume + single rollback), got %d", n)
	}
	// 注：post-consume 在失败终态下不会提交消费日志（totalTokens=0 早退），此处仅作旁证；
	// 决定性的区分来自上方 relayErr 非 nil（旧实现 return nil 时该断言失败）。
	if n := atomic.LoadInt32(&upstreamCalls); n != 1 {
		t.Fatalf("expected exactly 1 upstream call, got %d", n)
	}
}
