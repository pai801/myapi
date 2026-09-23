package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/apitype"
	metaPkg "github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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

// newDirectRelayTestContext 构造带 Responses 请求体的测试上下文与可观测假 upstream，
// 上游固定返回 500。用于 relayResponsesDirect：畸形请求体须「软失败」继续到上游，
// 而上游非 200 使函数在到达上游后即失败返回，规避成功路径 post-consume 的异步
// goroutine（避免测试清理关闭 DB 后仍被 goroutine 触碰）。
func newDirectRelayTestContext(body string) (*gin.Context, *httptest.ResponseRecorder, *httptest.Server, *int32, *string) {
	var upstreamCalls int32
	var capturedBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		if raw, err := io.ReadAll(r.Body); err == nil {
			capturedBody = string(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom","type":"server_error"}}`))
	}))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, recorder, upstream, &upstreamCalls, &capturedBody
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

// TestRelayResponsesConvertedRejectsMalformedJSONWithoutDerivingMeta 锁定 D 模块畸形 JSON 硬失败语义：
// 截断对象 / 截断字符串 / trailing garbage 均须在转换与上游工作之前返回 HTTP 400
// invalid_request_error / CodeInvalidSourceJSON，且绝不从半截 JSON 派生 model / stream。
func TestRelayResponsesConvertedRejectsMalformedJSONWithoutDerivingMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		body string
	}{
		{"截断对象", `{"model":"x"`},
		{"截断字符串", `{"model":"x`},
		{"trailing garbage", `{"model":"x"} extra`},
	}

	Convey("G: 畸形 JSON Responses 请求 + 可观测假 upstream | W: relayResponsesConverted | T: 400 invalid_source_json 且不派生 meta", t, func() {
		for _, tc := range cases {
			tc := tc
			Convey(tc.name+" → 400 invalid_request_error/invalid_source_json", func() {
				c, recorder, upstream, calls, _ := newConvertedRelayTestContext(tc.body)
				defer upstream.Close()
				meta := convertedRelayMeta(upstream.URL)
				meta.OriginModelName = ""
				meta.ActualModelName = ""
				meta.IsStream = false

				relayErr := relayResponsesConverted(c, meta)

				So(relayErr, ShouldNotBeNil)
				So(relayErr.StatusCode, ShouldEqual, http.StatusBadRequest)
				So(relayErr.Error.Type, ShouldEqual, "invalid_request_error")
				So(relayErr.Error.Code, ShouldEqual, model.CodeInvalidSourceJSON)
				// 硬失败发生在转换与上游工作之前：无上游请求、recorder 无成功响应体。
				So(atomic.LoadInt32(calls), ShouldEqual, 0)
				So(recorder.Body.Len(), ShouldEqual, 0)
				// 绝不从半截 JSON 派生 model / stream。
				So(meta.OriginModelName, ShouldEqual, "")
				So(meta.ActualModelName, ShouldEqual, "")
				So(meta.IsStream, ShouldBeFalse)
			})
		}
	})
}

// TestRelayResponsesDirectMalformedJSONDoesNotPanicOrDeriveMeta 锁定 relayResponsesDirect 侧畸形
// JSON 的软失败语义（与 relayResponsesConverted 的硬失败语义相对）：
// 截断对象 / 截断字符串 / trailing garbage / 良构但非对象根（[1,2]）均须
//  1. 不 panic、不 abort；
//  2. 绝不从半截 JSON 派生 model / stream —— meta 三字段保持调用前的哨兵值；
//  3. 仅 warn 后继续后续流程（放行到上游）。
//
// 上游固定返回 500，使函数在到达上游后即失败返回，规避成功路径 post-consume 的异步
// goroutine（否则测试清理关闭 DB 后该 goroutine 仍会触碰 DB，导致不稳定）。
func TestRelayResponsesDirectMalformedJSONDoesNotPanicOrDeriveMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// relayResponsesDirect 的完整链路会真实发出上游请求：初始化 relay HTTP 客户端（进程内一次性幂等）。
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	// 复用已封装的「内存 sqlite + 关闭 Redis」夹具：CacheGetUserQuota / DecreaseUserQuota 依赖 DB。
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// 哨兵值刻意与任何可从 fixture 派生的值不同：
	// 若畸形 body 被误当作良构解析，OriginModelName 会变成 "x"、IsStream 会被改成 false，从而变红。
	// ActualModelName 用空值（而非非空哨兵）：生产代码仅在 `ActualModelName == ""` 时派生，
	// 若非空则派生被守卫短路、断言恒真而失去反证力；置空后一旦误派生为 "x" 即变红。
	const sentinelOrigin = "sentinel-origin"

	cases := []struct {
		name string
		body string
	}{
		{"截断对象", `{"model":"x"`},
		{"截断字符串", `{"model":"x`},
		{"trailing garbage", `{"model":"x"} extra`},
		{"良构但非对象根", `[1,2]`},
	}

	Convey("G: 畸形/非对象根 Responses 请求 + 可观测假 upstream | W: relayResponsesDirect | T: 不 panic、不派生 meta、软失败放行上游", t, func() {
		for _, tc := range cases {
			tc := tc
			Convey(tc.name+" → 不 panic、meta 不被污染、软失败到达上游", func() {
				c, _, upstream, calls, _ := newDirectRelayTestContext(tc.body)
				defer upstream.Close()
				meta := convertedRelayMeta(upstream.URL)
				meta.UserId = testUserId
				meta.OriginModelName = sentinelOrigin
				meta.ActualModelName = ""
				meta.IsStream = true

				var relayErr *model.ErrorWithStatusCode
				// 1) 不 panic：畸形 JSON 绝不触发 panic。
				require.NotPanics(t, func() {
					relayErr = relayResponsesDirect(c, meta)
				})

				// 2) 绝不从半截 JSON 派生 model / stream：三字段保持调用前值。
				//    （IsStream 哨兵为 true，若被误派生为 fixture 的 stream 默认 false 即变红。）
				So(meta.OriginModelName, ShouldEqual, sentinelOrigin)
				So(meta.ActualModelName, ShouldEqual, "")
				So(meta.IsStream, ShouldBeTrue)

				// 3) 软失败：仅 warn 后继续后续流程，确实放行到上游（未在解析阶段 abort）。
				So(atomic.LoadInt32(calls), ShouldEqual, 1)
				// 上游返回 500 → 函数以其错误返回（而非解析阶段的 400/500）；这正说明解析阶段未硬失败。
				So(relayErr, ShouldNotBeNil)
				So(relayErr.StatusCode, ShouldEqual, http.StatusInternalServerError)

				// 失败路径已回滚预扣费，额度复原，避免污染同包其他用例。
				waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
			})
		}
	})
}

// TestRelayResponsesDirectNormalRequestMetaEquivalence 锁定任务 3.1 的正常请求等价性：
// 惰性提取改造后，relayResponsesDirect 从良构请求体派生的 OriginModelName / ActualModelName /
// IsStream 必须与改造前（git show HEAD:relay/controller/responses.go 的
// map[string]interface{} + json.Unmarshal 实现）一致，并锁定缓存命中与缓存缺失两条路径同语义。
//
// 改造前基线（旧实现）：
//
//	var req map[string]interface{}
//	_ = json.Unmarshal(requestBody, &req)
//	if modelName, ok := req["model"].(string); ok { ctxMeta.OriginModelName = modelName }
//	if ctxMeta.ActualModelName == "" {
//	    if modelName, ok := req["model"].(string); ok { ...映射或原样... }
//	}
//	if stream, ok := req["stream"].(bool); ok { ctxMeta.IsStream = stream }
//
// 已知且契约已声明的差异：旧实现的 model 查找是大小写敏感的精确键（`req["model"]`），
// 新实现的 model 大小写不敏感（与共享缓存结论对齐）。故 `{"Model":"x"}` 旧行为得 ""、
// 新行为得 "x"；stream 保持精确键语义，两条路径对 `Stream` 变体均不派生。
func TestRelayResponsesDirectNormalRequestMetaEquivalence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// relayResponsesDirect 的完整链路会真实发出上游请求：初始化 relay HTTP 客户端（进程内一次性幂等）。
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// run 驱动一次 relayResponsesDirect（上游固定 500，软失败返回前 meta 已派生），
	// 返回派生后的三字段。metadata 非 nil 表示预置「缓存命中」结论（等价 middleware 已扫过同一 body）。
	run := func(t *testing.T, body string, mapping map[string]string, presetActual string, metadata *ctxkey.RequestBodyMetadata) (origin, actual string, isStream bool) {
		t.Helper()
		c, _, upstream, calls, _ := newDirectRelayTestContext(body)
		defer upstream.Close()
		meta := convertedRelayMeta(upstream.URL)
		meta.UserId = testUserId
		meta.OriginModelName = ""
		meta.ActualModelName = presetActual
		meta.IsStream = false
		meta.ModelMapping = mapping
		if metadata != nil {
			c.Set(ctxkey.KeyRequestBodyMetadata, *metadata)
		}

		relayErr := relayResponsesDirect(c, meta)
		if relayErr == nil {
			t.Fatalf("上游固定 500，期望返回错误而非 nil")
		}
		if got := atomic.LoadInt32(calls); got != 1 {
			t.Fatalf("期望恰好 1 次上游调用，实际 %d", got)
		}
		waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		return meta.OriginModelName, meta.ActualModelName, meta.IsStream
	}

	t.Run("基本用例：{\"model\":\"x\",\"stream\":true}", func(t *testing.T) {
		origin, actual, isStream := run(t, `{"model":"x","stream":true}`, nil, "", nil)
		if origin != "x" || actual != "x" || !isStream {
			t.Fatalf("got origin=%q actual=%q isStream=%v, want x/x/true", origin, actual, isStream)
		}
	})

	t.Run("model 映射命中：Origin=x、Actual=映射值", func(t *testing.T) {
		origin, actual, _ := run(t, `{"model":"x","stream":true}`, map[string]string{"x": "y"}, "", nil)
		if origin != "x" || actual != "y" {
			t.Fatalf("got origin=%q actual=%q, want x/y", origin, actual)
		}
	})

	t.Run("model 映射未命中：Actual=model 原值", func(t *testing.T) {
		origin, actual, _ := run(t, `{"model":"x","stream":true}`, map[string]string{"other": "z"}, "", nil)
		if origin != "x" || actual != "x" {
			t.Fatalf("got origin=%q actual=%q, want x/x", origin, actual)
		}
	})

	t.Run("stream=false：IsStream=false", func(t *testing.T) {
		_, _, isStream := run(t, `{"model":"x","stream":false}`, nil, "", nil)
		if isStream {
			t.Fatalf("got isStream=true, want false")
		}
	})

	t.Run("stream 缺失：IsStream=零值 false（与全新 meta 初值一致）", func(t *testing.T) {
		// 缺失 stream 是「合法零值」（缓存与回退同语义，middleware 对缺失键 StreamValid 保持 true、
		// Stream 保持零值 false）；此处 meta 初值即为零值，故结果与旧实现（初值 false 时保持 false）一致。
		_, _, isStream := run(t, `{"model":"x"}`, nil, "", nil)
		if isStream {
			t.Fatalf("got isStream=true, want zero-value false")
		}
	})

	t.Run("ActualModelName 已预置：不被 body 覆盖，Origin 仍从 body 派生", func(t *testing.T) {
		origin, actual, _ := run(t, `{"model":"x","stream":true}`, nil, "preset", nil)
		if origin != "x" || actual != "preset" {
			t.Fatalf("got origin=%q actual=%q, want x/preset", origin, actual)
		}
	})

	t.Run("缓存命中 vs 缓存缺失一致性：三字段一致", func(t *testing.T) {
		cases := []struct {
			name       string
			body       string
			metadata   *ctxkey.RequestBodyMetadata
			wantOrigin string
			wantActual string
			wantStream bool
		}{
			{
				name:       "model+stream 精确键",
				body:       `{"model":"x","stream":true}`,
				metadata:   &ctxkey.RequestBodyMetadata{WellFormed: true, Model: "x", ModelValid: true, Stream: true, StreamValid: true},
				wantOrigin: "x", wantActual: "x", wantStream: true,
			},
			{
				// `Stream` 变体在缓存（middleware 精确键）与回退（gjson 精确键）两路均为键缺失，
				// 合法零值 → 不派生 IsStream；`Model` 变体两路均大小写不敏感 → "x"。
				name:       "Model+Stream 大小写变体",
				body:       `{"Model":"x","Stream":true}`,
				metadata:   &ctxkey.RequestBodyMetadata{WellFormed: true, Model: "x", ModelValid: true, Stream: false, StreamValid: true},
				wantOrigin: "x", wantActual: "x", wantStream: false,
			},
			{
				// null 变体对 string 字段是无操作（与 encoding/json 及 middleware 一致）：
				// 缓存与回退两路都保留前一个字符串变体的 "a"，不得被 null 清零。
				name:       "model 字符串 + Model:null 变体",
				body:       `{"model":"a","Model":null}`,
				metadata:   &ctxkey.RequestBodyMetadata{WellFormed: true, Model: "a", ModelValid: true, Stream: false, StreamValid: true},
				wantOrigin: "a", wantActual: "a", wantStream: false,
			},
			{
				// 同上，验证大小写变体顺序无关：MODEL 先写 "a"，model:null 不覆盖。
				name:       "MODEL 字符串 + model:null 变体",
				body:       `{"MODEL":"a","model":null}`,
				metadata:   &ctxkey.RequestBodyMetadata{WellFormed: true, Model: "a", ModelValid: true, Stream: false, StreamValid: true},
				wantOrigin: "a", wantActual: "a", wantStream: false,
			},
			{
				// 重复 stream 键取值差异（AC-9）：gjson.Get 取首个（first-wins），
				// middleware 共享缓存亦为 first-wins；缓存命中与回退两路必须一致。
				name:       "stream 重复键 first-wins",
				body:       `{"model":"x","stream":true,"stream":false}`,
				metadata:   &ctxkey.RequestBodyMetadata{WellFormed: true, Model: "x", ModelValid: true, Stream: true, StreamValid: true},
				wantOrigin: "x", wantActual: "x", wantStream: true,
			},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				fo, fa, fs := run(t, tc.body, nil, "", nil)
				co, ca, cs := run(t, tc.body, nil, "", tc.metadata)
				t.Logf("PROBE %s: body=%s fallback=(origin=%q,actual=%q,stream=%v) cache=(origin=%q,actual=%q,stream=%v)", tc.name, tc.body, fo, fa, fs, co, ca, cs)
				if fo != co || fa != ca || fs != cs {
					t.Fatalf("缓存命中/缺失不一致：fallback=(%q,%q,%v) cache=(%q,%q,%v)", fo, fa, fs, co, ca, cs)
				}
				if fo != tc.wantOrigin || fa != tc.wantActual || fs != tc.wantStream {
					t.Fatalf("got origin=%q actual=%q isStream=%v, want %q/%q/%v", fo, fa, fs, tc.wantOrigin, tc.wantActual, tc.wantStream)
				}
			})
		}
	})

	// 缺口 1 自证：回退路径的 model 提取必须与缓存路径一致（大小写不敏感）。
	// 修复前回退用 gjson 精确键，`{"Model":"x"}` 会得 ""，与缓存命中得 "x" 相矛盾。
	t.Run("缺口1自证：{\"Model\":\"x\"} 缓存命中/缺失两路 OriginModelName 一致", func(t *testing.T) {
		cacheMeta := &ctxkey.RequestBodyMetadata{WellFormed: true, Model: "x", ModelValid: true, Stream: false, StreamValid: true}
		fallbackOrigin, fallbackActual, _ := run(t, `{"Model":"x"}`, nil, "", nil)
		cacheOrigin, cacheActual, _ := run(t, `{"Model":"x"}`, nil, "", cacheMeta)
		t.Logf("PROBE {\"Model\":\"x\"}: fallback=(origin=%q,actual=%q) cache=(origin=%q,actual=%q)", fallbackOrigin, fallbackActual, cacheOrigin, cacheActual)
		if fallbackOrigin != cacheOrigin || fallbackActual != cacheActual {
			t.Fatalf("缓存命中/缺失不一致：fallback=(%q,%q) cache=(%q,%q)", fallbackOrigin, fallbackActual, cacheOrigin, cacheActual)
		}
		if fallbackOrigin != "x" {
			t.Fatalf("大小写不敏感提取应得 \"x\"，实际 %q", fallbackOrigin)
		}
	})
}

// TestRelayResponsesDirectModelRewriteUsesSJSON 锁定任务 3.4 的 sjson 原地改写语义：
// 触发条件不变（OriginModelName != ActualModelName 且 ActualModelName != ""），
// 触发时仅替换 model 字段、其余字段逐字节保留、结果仍为合法 JSON；
// 未触发时上游收到的 body 必须逐字节等于原始 body。
func TestRelayResponsesDirectModelRewriteUsesSJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId = 7
		// 「重复键+float64 溢出值」用例的 body 含 max_output_tokens:1e400，预扣额度会将其
		// clamp 到 maxOutputTokensCap（1e6）后计入，估算总额 ≈ 1_000_510 > 1e6 会在额度闸门
		// 被拦截、无法到达上游。该用例验证的是模型改写而非额度逻辑，故将夹具额度抬到高于估算值。
		initialQuota = int64(2_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// drive 驱动一次 relayResponsesDirect 并返回上游实际收到的 body（上游固定 500，改写发生在 DoRequest 前）。
	// presetOrigin 模拟生产 GetByContext 从 ctxkey.RequestModel 派生的初始 OriginModelName：
	// 良构 body 时 middleware 已按 body 的 model 写入（调用方与 body 的 model 保持一致），
	// 畸形 / 非对象根 body 时 middleware 提取失败、distributor 兜底为 "auto"（生产一致）。
	drive := func(t *testing.T, body, presetOrigin, presetActual string) string {
		t.Helper()
		c, _, upstream, calls, captured := newDirectRelayTestContext(body)
		defer upstream.Close()
		meta := convertedRelayMeta(upstream.URL)
		meta.UserId = testUserId
		meta.OriginModelName = presetOrigin
		meta.ActualModelName = presetActual
		meta.IsStream = false

		relayErr := relayResponsesDirect(c, meta)
		if relayErr == nil {
			t.Fatalf("上游固定 500，期望返回错误而非 nil")
		}
		if got := atomic.LoadInt32(calls); got != 1 {
			t.Fatalf("期望恰好 1 次上游调用，实际 %d", got)
		}
		waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		return *captured
	}

	t.Run("改写成功：目标字段更新、其余字段不变、结果可被 json.Valid 解析", func(t *testing.T) {
		body := `{"model":"origin-model","stream":false,"nested":{"a":1,"b":"two","deep":{"c":[1,2]}},"arr":[1,"x",true,null],"num":42,"float":3.5,"str":"hello","flag":true,"obj":{"k":"v"}}`
		// 生产一致：middleware 从良构 body 的 model 派生 RequestModel="origin-model"，
		// distributor 写入 SuggestedModel="mapped-model" → 触发改写。
		got := drive(t, body, "origin-model", "mapped-model")

		if !json.Valid([]byte(got)) {
			t.Fatalf("改写结果必须是合法 JSON: %q", got)
		}
		if model := gjson.Get(got, "model").String(); model != "mapped-model" {
			t.Fatalf("model 字段应被改写为 mapped-model，实际 %q（body=%q）", model, got)
		}
		// 其余字段（嵌套对象/数组/数字/字符串/布尔/null）逐字段比较原始字节表示，必须完全不变。
		paths := []string{
			"stream",
			"nested.a", "nested.b", "nested.deep.c.0", "nested.deep.c.1",
			"arr.0", "arr.1", "arr.2", "arr.3",
			"num", "float", "str", "flag", "obj.k",
		}
		for _, path := range paths {
			want := gjson.Get(body, path)
			gotV := gjson.Get(got, path)
			if gotV.Raw != want.Raw {
				t.Errorf("字段 %s 应保持不变：want raw=%q got raw=%q", path, want.Raw, gotV.Raw)
			}
		}
	})

	t.Run("无触发（OriginModelName == ActualModelName）：body 逐字节等于原始 body", func(t *testing.T) {
		body := `{"model":"x","a":1,"nested":{"b":true}}`
		// 生产一致：middleware 从 body 派生 RequestModel="x"、distributor 写 SuggestedModel="x"
		// → OriginModelName == ActualModelName → 不触发改写。
		got := drive(t, body, "x", "x")
		if got != body {
			t.Fatalf("未触发改写时 body 必须逐字节不变：want=%q got=%q", body, got)
		}
	})

	t.Run("无触发（ActualModelName 为空）：body 逐字节等于原始 body", func(t *testing.T) {
		body := `{"a":1,"nested":{"b":true}}`
		// 生产一致：body 无 model → middleware 提取空、distributor 兜底 RequestModel="auto"；
		// 此处 ActualModelName 保持空 → 触发条件因 ActualModelName == "" 短路 → 不触发改写。
		got := drive(t, body, "auto", "")
		if got != body {
			t.Fatalf("未触发改写时 body 必须逐字节不变：want=%q got=%q", body, got)
		}
	})

	// 改写失败子用例（规格缺口 2 的第 3 项）：sjson.SetBytes 对「数组根」输入会因
	// "cannot set array element for non-numeric key 'model'" 失败。该分支端到端可达：
	// 数组根 body 非对象 → 不派生 OriginModelName（middleware 提取失败后 distributor 兜底为 "auto"），
	// 预置的 ActualModelName 非空 → 触发条件 OriginModelName != ActualModelName && ActualModelName != ""
	// 成立 → body 非良构对象（守卫拦截，不做 sjson 改写）→ 原 body 透传
	// （/v1/responses 不在 shouldCheckModel 白名单，数组根请求不会被 middleware 400 拦截）。
	t.Run("改写失败保留原 body：数组根 + 预置 ActualModelName", func(t *testing.T) {
		body := `[1,2]`
		got := drive(t, body, "auto", "mapped")
		if got != body {
			t.Fatalf("改写失败时上游 body 必须逐字节等于原 body：want=%q got=%q", body, got)
		}
	})

	// 畸形 body 的改写失败用例（规格缺口 2）：sjson.SetBytes 对畸形 JSON 不返回错误，而是静默
	// 「修复」/污染后返回 nil（如 `{"model":"x` → `,"model":"deepseek-chat"}`），若不守卫会把
	// 损坏/非法 body 转发上游。守卫要求 body 为良构 JSON 对象，故下列 body 一律保留原样。
	// 触发条件与生产一致：畸形 body 时 middleware 提取失败 → distributor 兜底 RequestModel="auto"
	// （OriginModelName="auto"）；distributor 写入 SuggestedModel="deepseek-chat"（ActualModelName）。
	malformedCases := []struct {
		name string
		body string
	}{
		{"截断对象", `{"model":"x"`},
		{"截断字符串", `{"model":"x`},
		{"trailing garbage", `{"model":"x"} extra`},
		{"非法内容", `{invalid`},
		{"null 根", `null`},
		{"数字根", `123`},
	}
	for _, tc := range malformedCases {
		tc := tc
		t.Run("畸形 body 不改写且逐字节透传："+tc.name, func(t *testing.T) {
			got := drive(t, tc.body, "auto", "deepseek-chat")
			t.Logf("SELFCHECK body=%q upstream_received=%q equal=%v", tc.body, got, got == tc.body)
			if got != tc.body {
				t.Fatalf("畸形 body 必须逐字节透传（不得被污染）：want=%q got=%q", tc.body, got)
			}
		})
	}

	// B1 回归：model 键重复时 sjson.SetBytes 只替换首个匹配键，上游按文档序 last-wins
	// 会取到未被映射的后续键，导致渠道模型映射被静默绕过。修复后此类 body 走 gjson 键重建
	// （剔除全部 model 变体键后追加单个规范 model 键），产出只含一个 model 键的良构 body，
	// 且非 model 字段保留原始字节表示（含 encoding/json 无法反序列化的值）。
	// 触发条件与生产一致：OriginModelName="a"、ActualModelName="mapped-model"。
	deepNest := strings.Repeat(`{"a":`, 10001) + `1` + strings.Repeat(`}`, 10001)
	duplicateKeyCases := []struct {
		name                 string
		body                 string
		encodingJSONParsable bool
	}{
		{"字节相同重复键", `{"model":"a","model":"b"}`, true},
		{"转义等价键", `{"model":"a","\u006dodel":"b"}`, true},
		{"大小写变体键", `{"model":"a","Model":"b"}`, true},
		{"重复键+float64 溢出值", `{"model":"a","Model":"b","max_output_tokens":1e400}`, true},
		{"重复键+巨整数", `{"model":"a","Model":"b","n":123456789012345678901234567890123456789012345678901234567890}`, true},
		{"重复键+超深嵌套", `{"model":"a","Model":"b","n":` + deepNest + `}`, false},
	}
	for _, tc := range duplicateKeyCases {
		tc := tc
		t.Run("重复 model 键不被绕过（last-wins 生效值=mapped-model）："+tc.name, func(t *testing.T) {
			got := drive(t, tc.body, "a", "mapped-model")

			// json.Valid 只做语法检查、不做数值范围检查，故 1e400 / 巨整数仍为 true；
			// 超深嵌套受 encoding/json 的 10000 层限制返回 false，跳过该检查。
			if tc.encodingJSONParsable && !json.Valid([]byte(got)) {
				t.Fatalf("改写结果必须是合法 JSON: %q", got)
			}
			// 上游 last-wins 语义：用 gjson 读取（首个匹配键）不足以证明唯一性，
			// 故先断言 body 中 model 变体键恰好剩 1 个（重建消除了全部重复变体），
			// 再断言其值为映射值。
			if gotCount := countModelKeys([]byte(got)); gotCount != 1 {
				t.Fatalf("重建后 model 变体键应恰好 1 个，实际 %d（body=%q）", gotCount, got)
			}
			if modelValue := gjson.Get(got, "model").String(); modelValue != "mapped-model" {
				t.Fatalf("model 生效值应为 mapped-model（映射不得被绕过），实际 %q（body=%q）", modelValue, got)
			}
			t.Logf("SELFCHECK body=%q upstream_received=%q model_count=%d model_value=%q", tc.body, got, countModelKeys([]byte(got)), gjson.Get(got, "model").String())
			// 非 model 字段保真：重建沿用原始字节表示，未被归一化/篡改。
			if tc.name == "重复键+float64 溢出值" {
				if raw := gjson.Get(got, "max_output_tokens").Raw; raw != "1e400" {
					t.Fatalf("非 model 字段必须保留原始字节表示：want=%q got=%q（body=%q）", "1e400", raw, got)
				}
				t.Logf("SELFCHECK non_model_field_raw=%q", gjson.Get(got, "max_output_tokens").Raw)
			}
		})
	}
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

// TestEstimateResponsesMaxOutputTokens 锁定 max_output_tokens 估算口径：
// 仅数字类型且为正时返回（小数截断），超 cap 时 clamp 到 maxOutputTokensCap，其余一律 0。
func TestEstimateResponsesMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"absent", `{}`, 0},
		{"null", `{"max_output_tokens":null}`, 0},
		{"string", `{"max_output_tokens":"5"}`, 0},
		{"array", `{"max_output_tokens":[5]}`, 0},
		{"object", `{"max_output_tokens":{"v":5}}`, 0},
		{"bool", `{"max_output_tokens":true}`, 0},
		{"zero", `{"max_output_tokens":0}`, 0},
		{"negative", `{"max_output_tokens":-1}`, 0},
		{"decimal truncates", `{"max_output_tokens":5.7}`, 5},
		{"integral float", `{"max_output_tokens":5.0}`, 5},
		{"exponent", `{"max_output_tokens":1e2}`, 100},
		{"just below cap", `{"max_output_tokens":999999}`, 999999},
		{"at cap", `{"max_output_tokens":1000000}`, maxOutputTokensCap},
		{"above cap", `{"max_output_tokens":1000001}`, maxOutputTokensCap},
		{"overflow scale", `{"max_output_tokens":1e300}`, maxOutputTokensCap},
		{"non-object root", `123`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateResponsesMaxOutputTokens(gjson.Parse(tc.body)); got != tc.want {
				t.Fatalf("estimateResponsesMaxOutputTokens(%s) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
	// 畸形文档行为由调用方闸门（responsesPreConsumeEstimates）与入口测试锁定：
	// 估算器本身不做全量校验，故不再断言畸形 body 返回 0（见 TestResponsesPreConsumeEstimatesCallerGate）。
}

// TestEstimateResponsesPromptTokens 锁定 prompt token 估算口径：
// instructions/input 字符串按 token 计入、input 数组 len*100、tools 数组 len*200、最小值 10 兜底，
// 非字符串/非数组类型不计入，畸形或非对象根返回 0。
func TestEstimateResponsesPromptTokens(t *testing.T) {
	// 启用近似 token 模式：避免触发 tiktoken 初始化（网络/耗时），并让字符串计数稳定可断言。
	origApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = origApproximate })

	// 近似模式下 token 数 = int(len*0.38)，200 字符 ≈ 76 token，稳定高于 10 的兜底。
	longText := strings.Repeat("a", 200)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"empty object falls back to 10", `{}`, 10},
		{"input array counts len*100", `{"input":[1,2,3]}`, 300},
		{"tools array counts len*200", `{"tools":[{},{}]}`, 400},
		{"input array plus tools array", `{"input":[1],"tools":[{}]}`, 300},
		{"empty input array falls back to 10", `{"input":[]}`, 10},
		{"empty tools array falls back to 10", `{"tools":[]}`, 10},
		{"input object not counted", `{"input":{}}`, 10},
		{"tools object not counted", `{"tools":{}}`, 10},
		{"instructions empty string not counted", `{"instructions":""}`, 10},
		{"instructions non-string not counted", `{"instructions":123}`, 10},
		{"input non-string non-array not counted", `{"input":123}`, 10},
		{"null input not counted", `{"input":null}`, 10},
		{"non-object root returns zero", `[1,2,3]`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateResponsesPromptTokens(gjson.Parse(tc.body)); got != tc.want {
				t.Fatalf("estimateResponsesPromptTokens(%s) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
	// 畸形文档行为由调用方闸门（responsesPreConsumeEstimates）与入口测试锁定：
	// 估算器本身不做全量校验，故不再断言畸形 body 返回 0（见 TestResponsesPreConsumeEstimatesCallerGate）。

	t.Run("instructions non-empty string counted above floor", func(t *testing.T) {
		got := estimateResponsesPromptTokens(gjson.Parse(`{"instructions":"` + longText + `"}`))
		if got <= 10 {
			t.Fatalf("expected instructions tokens above the 10 floor, got %d", got)
		}
	})

	t.Run("input string counted above floor", func(t *testing.T) {
		got := estimateResponsesPromptTokens(gjson.Parse(`{"input":"` + longText + `"}`))
		if got <= 10 {
			t.Fatalf("expected input string tokens above the 10 floor, got %d", got)
		}
	})

	t.Run("instructions string adds on top of input array", func(t *testing.T) {
		withInstr := estimateResponsesPromptTokens(gjson.Parse(`{"instructions":"` + longText + `","input":[1,2]}`))
		withoutInstr := estimateResponsesPromptTokens(gjson.Parse(`{"input":[1,2]}`))
		if withInstr <= withoutInstr {
			t.Fatalf("expected instructions to raise the estimate: with=%d without=%d", withInstr, withoutInstr)
		}
	})
}

// =============================================================================
// Task 4.5：handleResponsesDirectStream 单次扫描取帧 type / usage 词法提取
// =============================================================================

// TestResponsesStreamAccumulatorAddPayloadResult 表驱动验证 addPayload 返回的
// type 与 completed usage 词法提取语义（复刻旧 typed json.Unmarshal 到 responsesUsage）。
func TestResponsesStreamAccumulatorAddPayloadResult(t *testing.T) {
	acc := newResponsesStreamAccumulator()

	tests := []struct {
		name      string
		payload   string
		wantErr   bool // 期望 addPayload 返回 error 且不产出派生值
		wantType  string
		wantUsage bool // 期望 Usage 非 nil
		in        int
		out       int
		tot       int
	}{
		{
			name:      "completed with valid usage",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.completed",
			wantUsage: true, in: 10, out: 20, tot: 30,
		},
		{
			name:      "non-completed event with usage ignored",
			payload:   `{"type":"response.created","response":{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.created",
			wantUsage: false,
		},
		{
			name:      "completed with string basis",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":"x","output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "completed with fractional basis",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":5.7,"output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			// 关键：词法路径的核心价值——map[string]any 会把 1e2 归一化为 100，无法复刻 typed int 语义
			name:      "completed with exponential basis",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":1e2,"output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "completed with trailing-zero float basis",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":100.0,"output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "completed with null basis maps to zero",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":null,"output_tokens":null,"total_tokens":null}}}`,
			wantType:  "response.completed",
			wantUsage: true, in: 0, out: 0, tot: 0,
		},
		{
			name:      "completed without response",
			payload:   `{"type":"response.completed","response":{}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "completed with null usage",
			payload:   `{"type":"response.completed","response":{"usage":null}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "non-string type",
			payload:   `{"type":123}`,
			wantErr:   true,
			wantType:  "",
			wantUsage: false,
		},
		{
			name:      "malformed payload",
			payload:   `{"type":"`,
			wantErr:   true,
			wantType:  "",
			wantUsage: false,
		},
		{
			name:      "null root",
			payload:   `null`,
			wantType:  "",
			wantUsage: false,
		},
		{
			name:      "completed with int64 overflow basis",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":123456789012345678901234567890,"output_tokens":20,"total_tokens":30}}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "completed with invalid details object type",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":"x"}}}`,
			wantType:  "response.completed",
			wantUsage: false,
		},
		{
			name:      "completed with null details keeps basis",
			payload:   `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":null}}}`,
			wantType:  "response.completed",
			wantUsage: true, in: 1, out: 1, tot: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := acc.addPayload([]byte(tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (Type=%q Usage=%v)", got.Type, got.Usage)
				}
				if got.Type != "" || got.Usage != nil {
					t.Fatalf("error case must not produce derived values, got Type=%q Usage=%v", got.Type, got.Usage)
				}
				return
			}
			if err != nil {
				t.Fatalf("addPayload returned error: %v", err)
			}
			if got.Type != tc.wantType {
				t.Fatalf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if tc.wantUsage {
				if got.Usage == nil {
					t.Fatalf("Usage = nil, want non-nil")
				}
				if got.Usage.InputTokens != tc.in || got.Usage.OutputTokens != tc.out || got.Usage.TotalTokens != tc.tot {
					t.Fatalf("Usage = %d/%d/%d, want %d/%d/%d", got.Usage.InputTokens, got.Usage.OutputTokens, got.Usage.TotalTokens, tc.in, tc.out, tc.tot)
				}
			} else if got.Usage != nil {
				t.Fatalf("Usage = %+v, want nil", got.Usage)
			}
		})
	}
}

// TestResponsesStreamUsageDuplicateKeysFirstWins 锁定 responses usage 重复键的取值差异
// （契约 AC-9：`Duplicate keys SHALL be resolved deterministically and documented`）。
// 本路径经 gjson 按路径取值，重复键取「第一个」（first-wins）；旧实现经 encoding/json
// typed 解码取「最后一个」（last-wins）。每条用例注释同时记录旧 last-wins 值作为对照。
// 本测试同时覆盖契约 7.4 对 Responses usage 重复键测试的要求（7.4 执行时交叉引用即可）。
func TestResponsesStreamUsageDuplicateKeysFirstWins(t *testing.T) {
	tests := []struct {
		name           string
		payload        string
		wantIn         int
		wantOut        int
		wantTot        int
		wantCached     int
		wantCachedUsed bool
	}{
		{
			// 重复 response 键：旧 last-wins 得 9/9/18
			name:    "duplicate response key",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}},"response":{"usage":{"input_tokens":9,"output_tokens":9,"total_tokens":18}}}`,
			wantIn:  1, wantOut: 2, wantTot: 3,
		},
		{
			// 重复 usage 键：旧 last-wins 得 9/9/18
			name:    "duplicate usage key",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"usage":{"input_tokens":9,"output_tokens":9,"total_tokens":18}}}`,
			wantIn:  1, wantOut: 2, wantTot: 3,
		},
		{
			// 重复 input_tokens 键：旧 last-wins 得 9/2/3
			name:    "duplicate input_tokens key",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":1,"input_tokens":9,"output_tokens":2,"total_tokens":3}}}`,
			wantIn:  1, wantOut: 2, wantTot: 3,
		},
		{
			// 重复 cached_tokens 键：旧 last-wins 得 9
			name:    "duplicate cached_tokens key",
			payload: `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":{"cached_tokens":5,"cached_tokens":9}}}}`,
			wantIn:  1, wantOut: 2, wantTot: 3,
			wantCached: 5, wantCachedUsed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc := newResponsesStreamAccumulator()
			got, err := acc.addPayload([]byte(tc.payload))
			if err != nil {
				t.Fatalf("addPayload returned error: %v", err)
			}
			if got.Usage == nil {
				t.Fatal("Usage = nil, want non-nil")
			}
			if got.Usage.InputTokens != tc.wantIn || got.Usage.OutputTokens != tc.wantOut || got.Usage.TotalTokens != tc.wantTot {
				t.Fatalf("Usage = %d/%d/%d, want %d/%d/%d", got.Usage.InputTokens, got.Usage.OutputTokens, got.Usage.TotalTokens, tc.wantIn, tc.wantOut, tc.wantTot)
			}
			if tc.wantCachedUsed {
				if got.Usage.InputTokensDetails == nil {
					t.Fatal("InputTokensDetails = nil, want non-nil")
				}
				if got.Usage.InputTokensDetails.CachedTokens != tc.wantCached {
					t.Fatalf("CachedTokens = %d, want %d", got.Usage.InputTokensDetails.CachedTokens, tc.wantCached)
				}
			}
		})
	}
}

// TestHandleResponsesDirectStream_UsageOnlyFromCompletedEvent 验证 usage 仅采信
// response.completed 帧：非 completed 帧即使携带 usage 也不产出。
func TestHandleResponsesDirectStream_UsageOnlyFromCompletedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"resp_s","usage":{"input_tokens":99,"output_tokens":99,"total_tokens":198}}}`,
			"",
			`event: response.in_progress`,
			`data: {"type":"response.in_progress","response":{"id":"resp_s","usage":{"input_tokens":88,"output_tokens":88,"total_tokens":176}}}`,
			"",
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_s","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`,
			"",
		}, "\n"))),
	}

	usage, relayErr := handleResponsesDirectStream(c, resp)
	if relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 20 || usage.TotalTokens != 30 {
		t.Fatalf("expected usage 10/20/30 from response.completed only, got %+v", usage)
	}
}

// TestHandleResponsesDirectStream_CompletedWithoutUsage 验证 completed 帧但 usage
// 不可用时的覆盖语义（除 AC-9 声明的重复键 first-wins 差异外，与旧 typed 解码等价
// `if err == nil && Type==completed && Response != nil`）：
//   - response 为非 null 对象且 usage 缺失/null → 覆盖为 nil 并最终兜底为零值；
//   - response 缺失 / typed 解码会失败（response 或 usage 非对象、usage 字段类型不符）→ 保持前值。
func TestHandleResponsesDirectStream_CompletedWithoutUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	valid := `data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`
	missingUsage := `data: {"type":"response.completed","response":{"id":"r2"}}`
	nullUsage := `data: {"type":"response.completed","response":{"id":"r2","usage":null}}`
	invalidUsage := `data: {"type":"response.completed","response":{"id":"r2","usage":{"input_tokens":"x","output_tokens":1,"total_tokens":2}}}`
	absentResponse := `data: {"type":"response.completed"}`

	tests := []struct {
		name       string
		frames     []string
		wantPrompt int
		wantComp   int
		wantTotal  int
	}{
		{"completed with missing usage", []string{`event: response.created`, `data: {"type":"response.created","response":{"id":"resp_s"}}`, "", `event: response.completed`, `data: {"type":"response.completed","response":{"id":"resp_s","status":"completed"}}`, ""}, 0, 0, 0},
		{"valid then missing usage overrides to zero", []string{valid, missingUsage}, 0, 0, 0},
		{"valid then null usage overrides to zero", []string{valid, nullUsage}, 0, 0, 0},
		{"valid then invalid usage keeps previous", []string{valid, invalidUsage}, 10, 20, 30},
		{"valid then absent response keeps previous", []string{valid, absentResponse}, 10, 20, 30},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(strings.Join(tc.frames, "\n"))),
			}

			usage, relayErr := handleResponsesDirectStream(c, resp)
			if relayErr != nil {
				t.Fatalf("expected no relay error, got %+v", relayErr)
			}
			if usage == nil {
				t.Fatal("expected non-nil zero usage so quota rollback path works")
			}
			if usage.PromptTokens != tc.wantPrompt || usage.CompletionTokens != tc.wantComp || usage.TotalTokens != tc.wantTotal {
				t.Fatalf("usage = %d/%d/%d, want %d/%d/%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, tc.wantPrompt, tc.wantComp, tc.wantTotal)
			}
		})
	}
}

// TestHandleResponsesDirectStream_MalformedFramesForwardedWithoutUsage 验证畸形帧
// 被原样转发、不产出 usage、不 panic。
func TestHandleResponsesDirectStream_MalformedFramesForwardedWithoutUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: response.created`,
			`data: {"type":`,
			"",
			`data: not-json-at-all`,
			"",
			`data: {"type":123}`,
			"",
		}, "\n"))),
	}

	usage, relayErr := handleResponsesDirectStream(c, resp)
	if relayErr != nil {
		t.Fatalf("expected no relay error, got %+v", relayErr)
	}
	if usage == nil || usage.TotalTokens != 0 {
		t.Fatalf("expected zero usage for malformed-only stream, got %+v", usage)
	}
	got := recorder.Body.String()
	for _, want := range []string{`data: {"type":`, `data: not-json-at-all`, `data: {"type":123}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected malformed frame forwarded verbatim %q, got %q", want, got)
		}
	}
}

// =============================================================================
// 缺陷 1：converted 路径根类型错误码（null → invalid_source_shape；其余非对象根 → invalid_source_json）
// =============================================================================

// TestRelayResponsesConvertedRootTypeErrorCodes 锁定缺陷 1 修复：复刻旧实现
// json.Unmarshal(body, &map[string]interface{}) 的根类型语义：
//   - null 根反序列化成功（map 保持 nil）→ 继续到转换层 → invalid_request_error / invalid_source_shape；
//   - 数组/字符串/数字/布尔根反序列化失败 → invalid_request_error / invalid_source_json。
func TestRelayResponsesConvertedRootTypeErrorCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name     string
		body     string
		wantCode string
		// metadata 非 nil 表示预置 middleware 缓存结论（缓存命中路径）；
		// nil 表示缓存缺失，走 json.Valid + 惰性根回退。
		metadata *ctxkey.RequestBodyMetadata
	}{
		{"null 根走既有 invalid_source_shape", `null`, model.CodeInvalidSourceShape, nil},
		{"null 根缓存命中仍走 invalid_source_shape", `null`, model.CodeInvalidSourceShape,
			&ctxkey.RequestBodyMetadata{WellFormed: true, Model: "", ModelValid: true, Stream: false, StreamValid: true}},
		{"数组根 invalid_source_json", `[1,2]`, model.CodeInvalidSourceJSON, nil},
		{"字符串根 invalid_source_json", `"x"`, model.CodeInvalidSourceJSON, nil},
		{"数字根 invalid_source_json", `123`, model.CodeInvalidSourceJSON, nil},
		{"布尔根 invalid_source_json", `true`, model.CodeInvalidSourceJSON, nil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, upstream, calls, _ := newConvertedRelayTestContext(tc.body)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.OriginModelName = ""
			meta.ActualModelName = ""
			meta.IsStream = false
			if tc.metadata != nil {
				c.Set(ctxkey.KeyRequestBodyMetadata, *tc.metadata)
			}

			relayErr := relayResponsesConverted(c, meta)

			if relayErr == nil {
				t.Fatalf("expected 400 error, got nil")
			}
			if relayErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected HTTP 400, got %d", relayErr.StatusCode)
			}
			if relayErr.Error.Type != "invalid_request_error" {
				t.Fatalf("expected invalid_request_error, got %q", relayErr.Error.Type)
			}
			if got := fmt.Sprint(relayErr.Error.Code); got != tc.wantCode {
				t.Fatalf("expected code %q, got %q", tc.wantCode, got)
			}
			// 错误发生在转换/额度/上游工作之前。
			if got := atomic.LoadInt32(calls); got != 0 {
				t.Fatalf("expected no upstream call, got %d", got)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("expected empty recorder body, got %q", recorder.Body.String())
			}
		})
	}
}

// TestRelayResponsesTypeMismatchDoesNotDeriveMeta 补齐任务 7.1 对 D 改造点（relayResponsesDirect /
// relayResponsesConverted / model 改写）的 AC-2 覆盖：model / stream 存在但类型不符时，按设计规则 R2
// 绝不产出派生值（不误导地把数字/数组/对象/布尔当作字符串或布尔）。
//
// 与 AC-1（畸形 JSON）的区别：body 本身良构（json.Valid=true、WellFormed=true），失败点仅在字段类型。
// 旧实现用 `req["model"].(string)` / `req["stream"].(bool)` 类型断言，断言失败即不派生；新实现必须等价。
//
// 两条路径的期望：
//   - relayResponsesDirect：软失败——不派生非法字段，继续放行上游（上游固定 500 → 500 错误）；
//   - relayResponsesConverted：请求体良构且 model 类型不符时，转换层按既有 invalid_source_shape 失败；
//     关键断言是「不派生非法字段」这一 R2 结论。
func TestRelayResponsesTypeMismatchDoesNotDeriveMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// 所有样本均良构 JSON，失败点在字段类型（R2）。
	// R2 是**逐字段**结论：model 类型不符不阻碍合法 stream 的派生，反之亦然。
	// modelInvalid 表示 model 类型不符（不得派生 model）；wantStream 为该样本下 stream 的合法派生结论。
	tMismatchCases := []struct {
		name         string
		body         string
		wantModel    string // 期望派生的 model（"" 表示不得派生）
		wantStream   bool   // 期望派生的 stream 结论（true 仅当 stream 为合法布尔 true）
		modelInvalid bool   // model 本身是否类型不符
	}{
		{"model number", `{"model":123,"stream":true}`, "", true, true},
		{"model array", `{"model":[1],"stream":true}`, "", true, true},
		{"model object", `{"model":{},"stream":true}`, "", true, true},
		{"model bool", `{"model":true,"stream":true}`, "", true, true},
		{"stream number", `{"model":"x","stream":1}`, "x", false, false},
		{"stream string", `{"model":"x","stream":"true"}`, "x", false, false},
		{"stream array", `{"model":"x","stream":[]}`, "x", false, false},
		{"stream object", `{"model":"x","stream":{}}`, "x", false, false},
	}

	t.Run("direct: 类型不符不派生非法字段，软失败放行上游", func(t *testing.T) {
		for _, tc := range tMismatchCases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				if !json.Valid([]byte(tc.body)) {
					t.Fatalf("fixture 必须是良构 JSON（AC-2 与 AC-1 的区别）: %s", tc.body)
				}
				c, _, upstream, calls, _ := newDirectRelayTestContext(tc.body)
				defer upstream.Close()
				meta := convertedRelayMeta(upstream.URL)
				meta.UserId = testUserId
				// 哨兵：ActualModelName 置空，IsStream 置 false。
				meta.OriginModelName = "sentinel-origin"
				meta.ActualModelName = ""
				meta.IsStream = false

				var relayErr *model.ErrorWithStatusCode
				require.NotPanics(t, func() { relayErr = relayResponsesDirect(c, meta) })

				// R2 逐字段：model 类型不符 → 不得派生（保留哨兵）；model 合法 → 正常派生。
				if tc.modelInvalid {
					if meta.OriginModelName != "sentinel-origin" {
						t.Fatalf("[%s] model 类型不符不得派生 OriginModelName，实际 %q", tc.name, meta.OriginModelName)
					}
					if meta.ActualModelName != "" {
						t.Fatalf("[%s] model 类型不符不得派生 ActualModelName，实际 %q", tc.name, meta.ActualModelName)
					}
				} else {
					if meta.OriginModelName != tc.wantModel || meta.ActualModelName != tc.wantModel {
						t.Fatalf("[%s] 合法 model 应派生 %q，实际 origin=%q actual=%q", tc.name, tc.wantModel, meta.OriginModelName, meta.ActualModelName)
					}
				}
				// R2 逐字段：stream 非法 → 不得派生 true；stream 合法布尔 true → 正常派生 true。
				if meta.IsStream != tc.wantStream {
					t.Fatalf("[%s] IsStream=%v，期望 %v", tc.name, meta.IsStream, tc.wantStream)
				}
				if got := atomic.LoadInt32(calls); got != 1 {
					t.Fatalf("[%s] 软失败应放行上游恰好 1 次，实际 %d", tc.name, got)
				}
				if relayErr == nil || relayErr.StatusCode != http.StatusInternalServerError {
					t.Fatalf("[%s] 上游固定 500，期望 500 错误，实际 %+v", tc.name, relayErr)
				}
				waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
			})
		}
	})

	t.Run("converted: 类型不符不派生非法字段", func(t *testing.T) {
		for _, tc := range tMismatchCases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				c, _, upstream, calls, _ := newConvertedRelayTestContext(tc.body)
				defer upstream.Close()
				meta := convertedRelayMeta(upstream.URL)
				meta.UserId = testUserId
				// ActualModelName 置空以真正走 model 提取回退路径。
				meta.OriginModelName = "sentinel-origin"
				meta.ActualModelName = ""
				meta.IsStream = true // 哨兵：relayResponsesConverted 会显式赋值为提取结论

				var relayErr *model.ErrorWithStatusCode
				require.NotPanics(t, func() { relayErr = relayResponsesConverted(c, meta) })

				// R2：model 类型不符 → 回退提取 valid=false → 不派生（ctxMeta.ActualModelName 保持空串，
				// 转换层用空 model 继续并按既有 unsupported_mapping 400 返回）。
				if meta.ActualModelName != "" {
					t.Fatalf("[%s] model 类型不符不得派生 ActualModelName，实际 %q", tc.name, meta.ActualModelName)
				}
				// R2：IsStream 被显式赋值为 stream 提取结论（非法 stream → false，合法 true → true）。
				if meta.IsStream != tc.wantStream {
					t.Fatalf("[%s] IsStream=%v，期望 %v", tc.name, meta.IsStream, tc.wantStream)
				}
				// fixture 未提供 input → 转换层以既有 400 拒绝，不触达上游。
				if relayErr == nil {
					t.Fatalf("[%s] 期望转换层返回错误，实际 nil", tc.name)
				}
				if got := atomic.LoadInt32(calls); got != 0 {
					t.Fatalf("[%s] 类型不符样本不应触达上游，实际 %d", tc.name, got)
				}
			})
		}
	})
}

// =============================================================================
// 缺陷 2：良构性结论复用——估算器不再全量校验，改由调用方闸门承担
// =============================================================================

// TestResponsesPreConsumeEstimatesCallerGate 锁定缺陷 2：估算器的良构性闸门由调用方承担。
// 仅良构对象根参与估算；畸形 / null / 非对象 / 超深嵌套根一律返回 (0,0)，复刻旧实现
// json.Unmarshal 失败或 null 根导致 map 为 nil 时两个估算器均返回 0 的语义。
func TestResponsesPreConsumeEstimatesCallerGate(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 10001) + `1` + strings.Repeat(`}`, 10001)

	cases := []struct {
		name       string
		body       string
		wantPrompt int
		wantMax    int
	}{
		{"良构对象根参与估算", `{"input":[1,2],"max_output_tokens":100}`, 200, 100},
		{"畸形根不参与估算", `{"input":[1,2],"max_output_tokens":100`, 0, 0},
		{"null 根不参与估算", `null`, 0, 0},
		{"数组根不参与估算", `[1,2]`, 0, 0},
		{"超深嵌套对象不参与估算", `{"max_output_tokens":100,"n":` + deep + `}`, 0, 0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			root := gjson.ParseBytes(body)
			rootKind := responsesRootKindOf(root, json.Valid(body))
			prompt, maxOut := responsesPreConsumeEstimates(root, rootKind)
			t.Logf("PROBEGATE body=%.30q jsonValid=%v rootKind=%d prompt=%d max=%d", tc.body, json.Valid(body), rootKind, prompt, maxOut)
			if prompt != tc.wantPrompt || maxOut != tc.wantMax {
				t.Fatalf("responsesPreConsumeEstimates(%s) = (%d,%d), want (%d,%d)", tc.body, prompt, maxOut, tc.wantPrompt, tc.wantMax)
			}
		})
	}
}

// TestRelayResponsesDirectMalformedBodySkipsEstimators 锁定缺陷 2 的入口语义：
// 畸形 / 超深嵌套 body（内含极大 max_output_tokens）不得喂给估算器，否则预扣额度会超过用户
// 余额而在上游之前 403；修复后估算被调用方闸门拦掉（按旧实现 map==nil 语义返回 0），请求正常放行。
//
// 反证力：被驳回的旧实现中估算器自身用 gjson.Valid 判定良构（对超深嵌套返回 true），会读到
// max_output_tokens 并 clamp 到 1e6，使预扣 > 1e6 → 403；本用例的「超深嵌套」子用例即可变红。
func TestRelayResponsesDirectMalformedBodySkipsEstimators(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	deep := strings.Repeat(`{"a":`, 10001) + `1` + strings.Repeat(`}`, 10001)

	cases := []struct {
		name string
		body string
	}{
		{"畸形 JSON + 极大 max_output_tokens", `{"model":"x","max_output_tokens":999999999`},
		{"超深嵌套对象 + 极大 max_output_tokens", `{"model":"x","max_output_tokens":999999999,"n":` + deep + `}`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, _, upstream, calls, _ := newDirectRelayTestContext(tc.body)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.UserId = testUserId
			// Origin == Actual（非空）→ 不触发 model 改写，聚焦估算闸门。
			meta.OriginModelName = "gpt-test"
			meta.ActualModelName = "gpt-test"
			meta.IsStream = false

			relayErr := relayResponsesDirect(c, meta)

			// 未被 403 拦截：请求确实到达上游（上游固定 500）。
			if got := atomic.LoadInt32(calls); got != 1 {
				t.Fatalf("expected upstream reached exactly once (not blocked by 403), got %d", got)
			}
			if relayErr == nil || relayErr.StatusCode != http.StatusInternalServerError {
				t.Fatalf("expected upstream 500, got %+v", relayErr)
			}
			waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		})
	}
}

// =============================================================================
// 缺陷 3：深嵌套良构性结论在缓存命中与回退两路一致
// =============================================================================

// TestRelayResponsesDirectDeepNestedWellFormednessCacheFallbackConsistency 锁定缺陷 3 修复：
// 对深度 ≥10000 的良构对象（json.Valid 因 10000 层上限判 false、gjson.Valid 判 true），
// 回退路径必须与 middleware 缓存结论一致——两者都不派生 model / stream。
func TestRelayResponsesDirectDeepNestedWellFormednessCacheFallbackConsistency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	deep := strings.Repeat(`{"a":`, 10001) + `1` + strings.Repeat(`}`, 10001)
	body := `{"model":"x","n":` + deep + `}`

	// 库间差异正是缺陷 3 的分叉来源：json.Valid=false（与 middleware 一致），gjson.Valid=true。
	if json.Valid([]byte(body)) {
		t.Fatalf("fixture 期望 json.Valid=false（超深嵌套超过 10000 层）")
	}
	if !gjson.Valid(body) {
		t.Fatalf("fixture 期望 gjson.Valid=true（无深度上限）")
	}

	run := func(t *testing.T, metadata *ctxkey.RequestBodyMetadata) (origin, actual string, isStream bool) {
		t.Helper()
		c, _, upstream, calls, _ := newDirectRelayTestContext(body)
		defer upstream.Close()
		meta := convertedRelayMeta(upstream.URL)
		meta.UserId = testUserId
		// Origin == Actual（非空）→ 不触发 model 改写，聚焦派生结论。
		meta.OriginModelName = "sentinel-origin"
		meta.ActualModelName = "sentinel-origin"
		meta.IsStream = true // 哨兵：若被误派生为 body 的零值 false 即变红
		if metadata != nil {
			c.Set(ctxkey.KeyRequestBodyMetadata, *metadata)
		}

		relayErr := relayResponsesDirect(c, meta)
		if relayErr == nil {
			t.Fatalf("上游固定 500，期望返回错误而非 nil")
		}
		if got := atomic.LoadInt32(calls); got != 1 {
			t.Fatalf("期望恰好 1 次上游调用，实际 %d", got)
		}
		waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		return meta.OriginModelName, meta.ActualModelName, meta.IsStream
	}

	// 回退路径（无缓存）：json.Valid=false → 不派生。
	fallbackOrigin, fallbackActual, fallbackStream := run(t, nil)
	// 缓存命中路径（middleware 对同一 bytes 的 WellFormed=false 结论）：不派生。
	cacheOrigin, cacheActual, cacheStream := run(t, &ctxkey.RequestBodyMetadata{WellFormed: false})

	t.Logf("PROBEDEPTH fallback=(%q,%q,%v) cache=(%q,%q,%v)", fallbackOrigin, fallbackActual, fallbackStream, cacheOrigin, cacheActual, cacheStream)

	if fallbackOrigin != cacheOrigin || fallbackActual != cacheActual || fallbackStream != cacheStream {
		t.Fatalf("深嵌套 body 缓存命中/回退结论不一致：fallback=(%q,%q,%v) cache=(%q,%q,%v)",
			fallbackOrigin, fallbackActual, fallbackStream, cacheOrigin, cacheActual, cacheStream)
	}
	if fallbackOrigin != "sentinel-origin" || fallbackActual != "sentinel-origin" || !fallbackStream {
		t.Fatalf("深嵌套 body 不得派生 model / stream：got origin=%q actual=%q stream=%v",
			fallbackOrigin, fallbackActual, fallbackStream)
	}
}

// =============================================================================
// 缺陷 2 性能证据：良构性结论复用（ns/op、B/op、allocs/op）
// =============================================================================

// responsesBenchBody 生成约 300KB 的良构 Responses 请求体，规模与缺陷 2 实测一致。
func responsesBenchBody() []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-test","stream":false,"instructions":"`)
	sb.WriteString(strings.Repeat("a", 64*1024))
	sb.WriteString(`","input":[`)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"role":"user","content":"`)
		sb.WriteString(strings.Repeat("b", 100))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`],"max_output_tokens":1024}`)
	return []byte(sb.String())
}

// BenchmarkResponsesRequestWellFormedness 为缺陷 2 提供修复前后的 ns/op、B/op、allocs/op 证据：
//   - before_three_validations_plus_estimators：修复前路径（同一 body 被 gjson.Valid 全量校验 3 次 + 估算）；
//   - after_cache_miss_one_json_valid_plus_estimators：修复后缓存缺失（json.Valid 判定 1 次 + 估算）；
//   - after_cache_hit_estimators_only：修复后缓存命中（0 次全量校验，仅两个估算器纯字段读取）。
func BenchmarkResponsesRequestWellFormedness(b *testing.B) {
	origApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	defer func() { config.ApproximateTokenEnabled = origApproximate }()

	body := responsesBenchBody()
	root := gjson.ParseBytes(body)

	b.Run("before_three_validations_plus_estimators", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = root.IsObject() && gjson.Valid(root.Raw)
			_ = gjson.ValidBytes(body)
			_ = gjson.ValidBytes(body)
			_, _ = responsesPreConsumeEstimates(root, responsesRootObject)
		}
	})

	b.Run("after_cache_miss_one_json_valid_plus_estimators", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = json.Valid(body)
			_, _ = responsesPreConsumeEstimates(root, responsesRootObject)
		}
	})

	b.Run("after_cache_hit_estimators_only", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = responsesPreConsumeEstimates(root, responsesRootObject)
		}
	})
}

// =============================================================================
// 缺陷 4：超 float64 范围数值字面量的语义漂移（json.Valid 为 true 但旧 json.Unmarshal 失败）
// =============================================================================

// TestResponsesSemanticWellFormedRejectsUnrepresentableNumber 锁定语义级良构闸门
// （responsesSemanticWellFormed + hasUnrepresentableNumber）与旧实现 `json.Unmarshal` 判定等价：
//   - 超出 float64 表示范围的数值字面量（±Inf）→ 判为「不良构」；
//   - 下溢到 0 的字面量（1e-400）与任意精度整数 → 有限数，仍为良构（encoding/json 亦不报错）；
//   - 非对象根不做数值检测（旧实现本就因根类型失败）。
func TestResponsesSemanticWellFormedRejectsUnrepresentableNumber(t *testing.T) {
	// 先用 encoding/json 建立基线：新旧判定必须逐例一致。
	cases := []struct {
		name          string
		body          string
		syntactValid  bool // json.Valid 结论
		wantSemantic  bool // responsesSemanticWellFormed 结论
		wantUnmarshal bool // 旧 json.Unmarshal 到 map 是否成功
	}{
		{"1e400 超上界", `{"model":"gpt-test","max_output_tokens":1e400}`, true, false, false},
		{"1e309 超上界", `{"max_output_tokens":1e309}`, true, false, false},
		{"1.7976931348623159e308 略超 MaxFloat64", `{"n":1.7976931348623159e308}`, true, false, false},
		{"1.7976931348623157e308 恰为 MaxFloat64", `{"n":1.7976931348623157e308}`, true, true, true},
		{"1e308 有限", `{"n":1e308}`, true, true, true},
		{"1e-400 下溢到 0 仍有限", `{"n":1e-400}`, true, true, true},
		{"60 位巨整数仍为有限 float64", `{"n":123456789012345678901234567890123456789012345678901234567890}`, true, true, true},
		{"嵌套数组内超界值", `{"a":1e400,"b":{"c":[1,2e400]}}`, true, false, false},
		{"非对象根不做数值检测", `1e400`, true, true, false},
		{"普通对象", `{"model":"x","stream":true}`, true, true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			root := gjson.ParseBytes([]byte(tc.body))
			if got := json.Valid([]byte(tc.body)); got != tc.syntactValid {
				t.Fatalf("fixture 期望 json.Valid=%v，实际 %v", tc.syntactValid, got)
			}
			var m map[string]any
			unmarshalOK := json.Unmarshal([]byte(tc.body), &m) == nil
			if unmarshalOK != tc.wantUnmarshal {
				t.Fatalf("fixture 期望 json.Unmarshal 成功=%v，实际 %v", tc.wantUnmarshal, unmarshalOK)
			}
			got := responsesSemanticWellFormed(root, json.Valid([]byte(tc.body)))
			t.Logf("PROBESEMANTIC body=%s jsonValid=%v semantic=%v unmarshalOK=%v", tc.body, tc.syntactValid, got, unmarshalOK)
			if got != tc.wantSemantic {
				t.Fatalf("responsesSemanticWellFormed=%v，期望 %v", got, tc.wantSemantic)
			}
		})
	}
}

// TestRelayResponsesDirectUnrepresentableNumberSoftFailsAndPassesUpstream 锁定缺陷 4 的 direct 侧语义：
// `{"model":"gpt-test","max_output_tokens":1e400}` 语法良构（json.Valid=true）但旧 json.Unmarshal 失败，
// 故：
//  1. 不派生 model / stream（meta 三字段保持哨兵值）；
//  2. 估算产出 (0, 0)（不回落到 maxOutputTokensCap 的 1e6），用户额度 600000 时不被 403 拦截，
//     软失败放行到上游（upstreamCalls=1）；
//  3. 缓存命中（预置 WellFormed=true + Model）与缓存缺失两条路径结论完全一致——缓存携带的
//     WellFormed 源自 json.Valid，必须由语义闸门在两路补同一检测，否则缓存命中会分叉为 403。
func TestRelayResponsesDirectUnrepresentableNumberSoftFailsAndPassesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId = 7
		// 600000 < maxOutputTokensCap(1e6)：若 1e400 被 clamp 后计入预扣，必然 403；
		// 修复后估算为 (0,0)，预扣 500 远超低于余额，正常放行。
		initialQuota = int64(600000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	const body = `{"model":"gpt-test","max_output_tokens":1e400}`
	if !json.Valid([]byte(body)) {
		t.Fatalf("fixture 期望 json.Valid=true（语法良构），否则无法体现语义漂移")
	}

	run := func(t *testing.T, metadata *ctxkey.RequestBodyMetadata) (origin, actual string, isStream bool, calls int32, relayErr *model.ErrorWithStatusCode) {
		t.Helper()
		c, _, upstream, callsPtr, _ := newDirectRelayTestContext(body)
		defer upstream.Close()
		meta := convertedRelayMeta(upstream.URL)
		meta.UserId = testUserId
		// 哨兵：ActualModelName 预置为非空（生产 distributor 兜底 / 缓存已派生），
		// 这样「不被 1e400 覆盖」可通过 ActualModelName 保持非空来反证；Origin 用可辨识哨兵。
		meta.OriginModelName = "sentinel-origin"
		meta.ActualModelName = "gpt-test"
		meta.IsStream = true
		if metadata != nil {
			c.Set(ctxkey.KeyRequestBodyMetadata, *metadata)
		}
		relayErr = relayResponsesDirect(c, meta)
		return meta.OriginModelName, meta.ActualModelName, meta.IsStream, atomic.LoadInt32(callsPtr), relayErr
	}

	// 缓存缺失（回退）：json.Valid=true → 语义闸门翻转为不良构 → 不派生、估算 (0,0)、放行上游。
	fallbackOrigin, fallbackActual, fallbackStream, fallbackCalls, fallbackErr := run(t, nil)
	// 缓存命中：middleware 对同一 bytes 的 WellFormed=true + Model="gpt-test" 结论权威，
	// 但语义闸门必须同样翻转，否则会派生 model 并 403（这正是缺陷 4 的分叉点）。
	cacheOrigin, cacheActual, cacheStream, cacheCalls, cacheErr := run(t, &ctxkey.RequestBodyMetadata{
		WellFormed: true, Model: "gpt-test", ModelValid: true, Stream: false, StreamValid: true,
	})

	t.Logf("PROBEUNREP fallback=(origin=%q,actual=%q,stream=%v,calls=%d,err=%v) cache=(origin=%q,actual=%q,stream=%v,calls=%d,err=%v)",
		fallbackOrigin, fallbackActual, fallbackStream, fallbackCalls, fallbackErr,
		cacheOrigin, cacheActual, cacheStream, cacheCalls, cacheErr)

	// 核心：两路都未被 403 拦截，均到达上游（上游固定 500）。
	for name, r := range map[string]struct {
		calls int32
		err   *model.ErrorWithStatusCode
	}{"fallback": {fallbackCalls, fallbackErr}, "cache": {cacheCalls, cacheErr}} {
		if r.calls != 1 {
			t.Fatalf("%s: 期望放行到上游（calls=1，未被 403 拦截），实际 calls=%d err=%+v", name, r.calls, r.err)
		}
		if r.err == nil || r.err.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s: 期望到达上游后的 500（解析阶段未硬失败），实际 %+v", name, r.err)
		}
	}
	// 两路结论一致（缓存命中不得因 cached WellFormed=true 而分叉）。
	if fallbackOrigin != cacheOrigin || fallbackActual != cacheActual || fallbackStream != cacheStream {
		t.Fatalf("缓存命中/缺失不一致：fallback=(%q,%q,%v) cache=(%q,%q,%v)",
			fallbackOrigin, fallbackActual, fallbackStream, cacheOrigin, cacheActual, cacheStream)
	}
	// 不强求 (origin, actual) 的具体值：ActualModelName 预置非空本来就不会被 body 覆盖，
	// 关键是两侧一致且未因 1e400 触发任何估算异常。IsStream 保持调用前哨兵 true。
	if !fallbackStream || !cacheStream {
		t.Fatalf("1e400 不得派生 stream：fallback=%v cache=%v", fallbackStream, cacheStream)
	}

	waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
}

// TestRelayResponsesConvertedUnrepresentableNumberReturnsInvalidSourceJSON 锁定缺陷 4 的 converted 侧语义：
// 同一 body 必须在转换与上游工作之前返回 HTTP 400 invalid_request_error / CodeInvalidSourceJSON，
// 且不派生 model / stream；缓存命中与缓存缺失两路一致。
func TestRelayResponsesConvertedUnrepresentableNumberReturnsInvalidSourceJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const body = `{"model":"gpt-test","max_output_tokens":1e400}`

	cases := []struct {
		name     string
		metadata *ctxkey.RequestBodyMetadata
	}{
		{"缓存缺失（回退 json.Valid + 语义闸门）", nil},
		{"缓存命中（WellFormed=true + Model 已派生）", &ctxkey.RequestBodyMetadata{
			WellFormed: true, Model: "gpt-test", ModelValid: true, Stream: false, StreamValid: true,
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, upstream, calls, _ := newConvertedRelayTestContext(body)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.OriginModelName = "sentinel-origin"
			meta.ActualModelName = ""
			meta.IsStream = true
			if tc.metadata != nil {
				c.Set(ctxkey.KeyRequestBodyMetadata, *tc.metadata)
			}

			relayErr := relayResponsesConverted(c, meta)
			t.Logf("PROBEUNREPCONV metadata=%v err=%+v", tc.metadata, relayErr)

			if relayErr == nil {
				t.Fatal("期望 400 错误，实际 nil")
			}
			if relayErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("期望 HTTP 400，实际 %d", relayErr.StatusCode)
			}
			if relayErr.Error.Type != "invalid_request_error" {
				t.Fatalf("期望 invalid_request_error，实际 %q", relayErr.Error.Type)
			}
			if got := fmt.Sprint(relayErr.Error.Code); got != model.CodeInvalidSourceJSON {
				t.Fatalf("期望 code %q，实际 %q", model.CodeInvalidSourceJSON, got)
			}
			// 硬失败发生在转换/额度/上游工作之前。
			if got := atomic.LoadInt32(calls); got != 0 {
				t.Fatalf("期望无上游调用，实际 %d", got)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("期望无响应体，实际 %q", recorder.Body.String())
			}
			// 绝不从该 body 派生 model / stream。
			if meta.OriginModelName != "sentinel-origin" || meta.ActualModelName != "" || !meta.IsStream {
				t.Fatalf("不得派生 model/stream：origin=%q actual=%q stream=%v",
					meta.OriginModelName, meta.ActualModelName, meta.IsStream)
			}
		})
	}
}

// TestRelayResponsesConvertedStreamDuplicateKeysFirstWins 补齐 3.2 converted 路径的 stream 重复键
// first-wins 测试（3.1 direct 路径已有）：gjson.Get / middleware 缓存均取首个 `stream` 键，
// 与 encoding/json 的 last-wins 相对。用「首 false、次 true」锁定 first-wins（若误取 last-wins 会得 true）。
func TestRelayResponsesConvertedStreamDuplicateKeysFirstWins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// 首 false、次 true：first-wins=false（非流式路径），last-wins 会得 true（流式路径）。
	// 附带一条 message input，使请求通过转换层、真实走非流式路径（空 input 会在转换层提前 400）。
	const body = `{"model":"gpt-test","stream":false,"stream":true,"input":[{"type":"message","role":"user","content":"hi"}]}`

	cases := []struct {
		name     string
		metadata *ctxkey.RequestBodyMetadata
	}{
		{"缓存缺失（回退 gjson first-wins）", nil},
		{"缓存命中（middleware first-wins）", &ctxkey.RequestBodyMetadata{
			WellFormed: true, Model: "gpt-test", ModelValid: true, Stream: false, StreamValid: true,
		}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c, recorder, upstream, _, _ := newConvertedRelayTestContext(body)
			defer upstream.Close()
			meta := convertedRelayMeta(upstream.URL)
			meta.UserId = testUserId
			meta.OriginModelName = "gpt-test"
			meta.ActualModelName = "gpt-test"
			// relayResponsesConverted 会无条件用派生结论覆盖 IsStream；此处初值仅避免歧义。
			meta.IsStream = true
			if tc.metadata != nil {
				c.Set(ctxkey.KeyRequestBodyMetadata, *tc.metadata)
			}

			relayErr := relayResponsesConverted(c, meta)
			t.Logf("PROBECONVDUP metadata=%v isStream=%v recorderCode=%d err=%+v", tc.metadata, meta.IsStream, recorder.Code, relayErr)

			// stream 重复键首 false、次 true：first-wins=false（非流式），last-wins 会得 true。
			// 缓存命中与缓存缺失两路必须一致（均为 false）。
			if meta.IsStream {
				t.Fatalf("stream 重复键必须 first-wins 得 false，实际 %v", meta.IsStream)
			}
			waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
		})
	}
}

// =============================================================================
// Sanctioned Invalid UTF-8 Difference：D model extraction 的非法 UTF-8 样本
// =============================================================================

// TestResponsesModelExtractionPreservesInvalidUTF8RawBytes 锁定契约
// `Sanctioned Invalid UTF-8 Difference` 对 D model extraction 的要求：
// `{"model":"\xff\xfe"}` 中 `gjson` 保留原始字节（\xff\xfe），而 encoding/json 会把非法
// UTF-8 净化为 U+FFFD（EF BF BD）。该差异属已声明的解析库差异，不做代码修正。
// 契约允许按需做输入转换，故本用例对比两库行为并断言 gjson 路径不净化。
func TestResponsesModelExtractionPreservesInvalidUTF8RawBytes(t *testing.T) {
	// raw 为两个非法 UTF-8 字节（0xFF 0xFE），JSON 字符串内容按字节原样嵌入。
	raw := "\xff\xfe"
	body := []byte(`{"model":"` + raw + `"}`)

	// D model extraction 走 extractModelCaseInsensitive（回退路径）与 middleware 缓存两条路，
	// 两者底层均为 gjson / jsonparser，必须保留原始字节。
	model, valid := extractModelCaseInsensitive(body)
	t.Logf("PROBEUTF8 gjson-path modelBytes=%v valid=%v rawBytes=%v equalRaw=%v",
		[]byte(model), valid, []byte(raw), model == raw)
	if !valid {
		t.Fatalf("非法 UTF-8 model 仍是合法 string，valid 应为 true")
	}
	if model != raw {
		t.Fatalf("gjson 路径必须保留原始字节：want bytes=%v got bytes=%v", []byte(raw), []byte(model))
	}
	if strings.Contains(model, "\uFFFD") {
		t.Fatalf("gjson 路径不得净化为 U+FFFD：%q", model)
	}

	// 对照声明 sanctioned 差异：encoding/json 会把同一字节净化为 U+FFFD。
	var viaEncodingJSON struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &viaEncodingJSON); err != nil {
		t.Fatalf("encoding/json 解码意外失败：%v", err)
	}
	t.Logf("PROBEUTF8 encoding/json-path modelBytes=%v", []byte(viaEncodingJSON.Model))
	if !strings.Contains(viaEncodingJSON.Model, "\uFFFD") {
		t.Fatalf("fixture 期望 encoding/json 净化为 U+FFFD，实际 bytes=%v", []byte(viaEncodingJSON.Model))
	}
	if viaEncodingJSON.Model == raw {
		t.Fatalf("fixture 期望两库行为不同，但 encoding/json 也保留了原始字节")
	}
}

// TestResponsesModelExtractionDuplicateKeys 锁定 D model extraction（extractModelCaseInsensitive）
// 的重复键取值差异（契约 AC-9 / 任务 7.4 对 model 提取点的交叉引用要求）：
//   - 字节完全相同的重复键取「第一个」（first-wins），旧 encoding/json typed 解码取「最后一个」（last-wins）；
//   - 大小写变体之间按文档序 last-wins（与 encoding/json 一致）；
//   - 转义与字面拼写解码后相同的键按重复处理（first-wins）。
//
// 缓存命中路径的同一语义由 middleware 的 buildRequestBodyMetadata 承担（其重复键结论由
// TestGetRequestModelDuplicateModelFirstWins 锁定），两路结论必须一致，故此处仅锁定回退路径。
func TestResponsesModelExtractionDuplicateKeys(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantModel  string
		wantValid  bool
		wantLegacy string // encoding/json last-wins 基线
	}{
		{"byte-identical duplicate first-wins", `{"model":"a","model":"b"}`, "a", true, "b"},
		{"escaped duplicate first-wins", `{"model":"a","\u006dodel":"b"}`, "a", true, "b"},
		{"case variants last-wins", `{"model":"a","Model":"b"}`, "b", true, "b"},
		{"case variants reversed last-wins", `{"Model":"b","model":"a"}`, "a", true, "a"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			model, valid := extractModelCaseInsensitive([]byte(tc.body))
			if model != tc.wantModel || valid != tc.wantValid {
				t.Fatalf("extractModelCaseInsensitive(%s) = (%q, %v), want (%q, %v)", tc.body, model, valid, tc.wantModel, tc.wantValid)
			}

			// encoding/json last-wins 基线（仅作对照，证明差异被锁定）。
			var viaEncodingJSON struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(tc.body), &viaEncodingJSON); err != nil {
				t.Fatalf("encoding/json 解码意外失败：%v", err)
			}
			if viaEncodingJSON.Model != tc.wantLegacy {
				t.Fatalf("fixture 期望 encoding/json last-wins=%q，实际 %q", tc.wantLegacy, viaEncodingJSON.Model)
			}
		})
	}
}

// TestRelayResponsesDirectModelRewriteRejectsLeadingGarbage 锁定优化项 3：
// 守卫必须针对完整 body 判定词法良构性，gjson.Parse 会跳过前导非空白字节（如 `\x00{...}`），
// 只看惰性根的 Raw 会把前缀垃圾一并放行、被 sjson 原样保留后转发上游。
func TestRelayResponsesDirectModelRewriteRejectsLeadingGarbage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client.Init()

	const (
		testUserId   = 7
		initialQuota = int64(1_000_000)
	)
	db := setupResponsesBillingTestDB(t, testUserId, initialQuota)

	// `\x00` 不是空白，json.Valid 与 gjson.ValidBytes 对完整 body 均为 false；
	// 但 gjson.Parse 的惰性根 Raw 只含 `{"model":"a"}`（IsObject=true，gjson.Valid(Raw)=true），
	// 修复前的守卫会因此放行改写、把前缀垃圾原样保留后转发。
	const body = "\x00{\"model\":\"a\"}"
	if json.Valid([]byte(body)) {
		t.Fatalf("fixture 期望 json.Valid=false（前导 NUL 非法）")
	}
	if gjson.ValidBytes([]byte(body)) {
		t.Fatalf("fixture 期望完整 body 的 gjson.ValidBytes=false（前导 NUL 非法）")
	}
	if !gjson.ParseBytes([]byte(body)).IsObject() || !gjson.Valid(gjson.ParseBytes([]byte(body)).Raw) {
		t.Fatalf("fixture 期望惰性根 Raw 恰为良构对象（这正是修复前的误放行来源）")
	}

	c, _, upstream, calls, captured := newDirectRelayTestContext(body)
	defer upstream.Close()
	meta := convertedRelayMeta(upstream.URL)
	meta.UserId = testUserId
	meta.OriginModelName = "a"
	meta.ActualModelName = "mapped-model" // 触发改写条件
	meta.IsStream = false

	relayErr := relayResponsesDirect(c, meta)
	if relayErr == nil {
		t.Fatalf("上游固定 500，期望返回错误而非 nil")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("期望恰好 1 次上游调用，实际 %d", got)
	}
	t.Logf("PROBELEADING bodyBytes=%v upstreamReceivedBytes=%v", []byte(body), []byte(*captured))
	if *captured != body {
		t.Fatalf("前导垃圾 body 必须逐字节透传（不得被 sjson 改写保留前缀）：want=%q got=%q", body, *captured)
	}
	waitResponsesQuotaRestored(t, db, testUserId, initialQuota)
}

// BenchmarkResponsesSemanticWellFormedness 量化超范围数值检测（缺陷 4）的额外开销：
//   - json_valid_only：仅 json.Valid（改造前的语法级闸门）；
//   - unrep_only：仅 hasUnrepresentableNumber（缺陷 4 新增的语义检测本身）；
//   - json_valid_plus_unrep：json.Valid + hasUnrepresentableNumber（修复后的语义级闸门）；
//   - old_json_unmarshal_map：旧实现的全量 json.Unmarshal（语义基线的性能参照）。
//
// 300KB 良构体上 hasUnrepresentableNumber 是单次 gjson 惰性树的递归遍历（无分配），
// json_valid_plus_unrep 相对 json_valid_only 的差值即为缺陷 4 的净代价；两者合计仍显著
// 低于旧实现全量 json.Unmarshal（后者伴随大量 map/interface 分配）。
func BenchmarkResponsesSemanticWellFormedness(b *testing.B) {
	body := responsesBenchBody()
	root := gjson.ParseBytes(body)

	b.Run("json_valid_only", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = json.Valid(body)
		}
	})

	b.Run("unrep_only", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = hasUnrepresentableNumber(root)
		}
	})

	b.Run("json_valid_plus_unrep", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = responsesSemanticWellFormed(root, json.Valid(body))
		}
	})

	b.Run("old_json_unmarshal_map", func(b *testing.B) {
		b.ReportAllocs()
		var m map[string]any
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_ = json.Unmarshal(body, &m)
		}
	})
}

// =============================================================================
// 模块 E / 任务 5.2：C2（handleResponsesDirectNonStream）新旧实现对照测试
//
// 对照方式：同一份 body 分别跑「旧实现」（真实 handleResponsesDirectNonStream，
// 其内部 `_ = json.Unmarshal(responseBody, &payload)` 忽略错误）与「新提取路径」
// `extractResponsesDirectNonStreamPayload`（再把 usage 经 toModelUsage 映射，与旧实现出口对齐）。
//
// 唯一有意行为变更（DEC-C2-1）：计费 basis 不可解析时，旧实现按「部分解析值」扣费，
// 新实现整体视为 usage 缺失、不扣费。其余样本（正常/缺失/零值/负值/极大值/重复键/
// details 降级/上游错误/畸形 JSON）必须与旧实现等价。
// =============================================================================

// c2LegacyOutcome 是旧实现 handleResponsesDirectNonStream 的可观测结果。
type c2LegacyOutcome struct {
	usage       *model.Usage
	relayErr    *model.ErrorWithStatusCode
	passthrough string
	storedBody  string
}

// runC2Legacy 内联复刻改造前的 handleResponsesDirectNonStream 行为（契约 5.2 的「旧实现」基线）：
// 与 C1/C3 基线同一约定，直接使用 `encoding/json`，不依赖已被模块 C 接线到新提取路径的生产入口，
// 从而保证对照测试比较的始终是「真正的旧实现」与「新提取路径」。
//
// 旧实现关键语义：`var payload struct{ Error *model.Error; Usage *responsesUsage }` +
// `_ = json.Unmarshal(responseBody, &payload)`（忽略解码错误，可能保留部分解析值 → 部分扣费）。
func runC2Legacy(t *testing.T, body string) c2LegacyOutcome {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	// ---- 旧实现主体（内联基线）----
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("legacy read body failed: %v", err)
	}
	_ = resp.Body.Close()
	var payload struct {
		Error *model.Error    `json:"error"`
		Usage *responsesUsage `json:"usage"`
	}
	_ = json.Unmarshal(responseBody, &payload)
	if payload.Error != nil && payload.Error.Message != "" {
		return c2LegacyOutcome{
			relayErr:    &model.ErrorWithStatusCode{Error: *payload.Error, StatusCode: resp.StatusCode},
			passthrough: recorder.Body.String(),
			storedBody:  c.GetString(ctxkey.ResponseBody),
		}
	}
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(responseBody)
	c.Set(ctxkey.ResponseBody, string(responseBody))

	return c2LegacyOutcome{
		usage:       payload.Usage.toModelUsage(),
		passthrough: recorder.Body.String(),
		storedBody:  c.GetString(ctxkey.ResponseBody),
	}
}

// TestResponsesNonStreamEquivalence 为 C2 建立新旧实现对照测试（与旧实现等价的样本）。
func TestResponsesNonStreamEquivalence(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "normal_bases_only", body: `{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`},
		{name: "normal_with_details", body: `{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":12},"output_tokens_details":{"reasoning_tokens":10,"accepted_prediction_tokens":2,"rejected_prediction_tokens":1,"audio_tokens":3,"text_tokens":34}}}`},
		{name: "missing_usage", body: `{}`},
		{name: "missing_usage_null", body: `{"usage":null}`},
		{name: "zero_empty_usage_object", body: `{"usage":{}}`},
		{name: "zero_all_bases", body: `{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`},
		{name: "negative_bases", body: `{"usage":{"input_tokens":-1,"output_tokens":-2,"total_tokens":-3}}`},
		{name: "extreme_maxint32", body: `{"usage":{"input_tokens":2147483646,"output_tokens":2147483647,"total_tokens":2147483647}}`},
		{name: "extreme_maxint64", body: `{"usage":{"input_tokens":9223372036854775807,"output_tokens":0,"total_tokens":9223372036854775807}}`},
		{name: "detail_mismatch_not_object", body: `{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":"x"}}`},
		{name: "detail_mismatch_field", body: `{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":{"cached_tokens":"x"}}}`},
		{name: "detail_partial_field_mismatch", body: `{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":"x"}}}`},
		{name: "upstream_error", body: `{"error":{"message":"bad effort","type":"invalid_request_error","code":"invalid_request_error"}}`},
		{name: "upstream_error_with_usage", body: `{"error":{"message":"bad"},"usage":{"input_tokens":5}}`},
		{name: "malformed_unclosed", body: `{"usage":{"input_tokens":5`},
		{name: "malformed_trailing_garbage", body: `{"usage":{"input_tokens":5}}garbage`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			legacy := runC2Legacy(t, tc.body)
			extraction := extractResponsesDirectNonStreamPayload([]byte(tc.body))

			// 上游错误等价：旧 relayErr 非 nil ⟺ 新提取出的 Error 有非空 Message。
			legacyHasErr := legacy.relayErr != nil
			currentHasErr := extraction.Error != nil && extraction.Error.Message != ""
			if legacyHasErr != currentHasErr {
				t.Fatalf("upstream error presence mismatch: legacy=%v current=%v (extraction=%+v)", legacyHasErr, currentHasErr, extraction)
			}
			if legacyHasErr {
				// 错误路径：旧实现不转发 body（直接返回错误），不写 ctxkey.ResponseBody。
				if legacy.passthrough != "" {
					t.Fatalf("expected no passthrough on legacy error path, got %q", legacy.passthrough)
				}
				if legacy.relayErr.Error.Message != extraction.Error.Message {
					t.Fatalf("upstream error message mismatch: legacy=%q current=%q", legacy.relayErr.Error.Message, extraction.Error.Message)
				}
				if legacy.usage != nil {
					t.Fatalf("expected no legacy usage on error path, got %+v", legacy.usage)
				}
				return
			}

			// 非错误路径：透传字节必须逐字节保持上游原样，且写入 ctxkey.ResponseBody。
			if legacy.passthrough != tc.body {
				t.Fatalf("legacy passthrough mismatch: got %q want %q", legacy.passthrough, tc.body)
			}
			if legacy.storedBody != tc.body {
				t.Fatalf("legacy ctxkey.ResponseBody mismatch: got %q want %q", legacy.storedBody, tc.body)
			}

			var currentUsage *model.Usage
			if extraction.Usage != nil {
				currentUsage = extraction.Usage.toModelUsage()
			}
			if !reflect.DeepEqual(legacy.usage, currentUsage) {
				t.Fatalf("usage mismatch:\n legacy=%+v\n current=%+v", legacy.usage, currentUsage)
			}
		})
	}
}

// TestResponsesNonStreamDuplicateKeysFirstWins 锁定 C2 重复键 first-wins 的 sanctioned 差异：
// 旧 encoding/json typed 解码 last-wins，新 gjson 路径 first-wins。
func TestResponsesNonStreamDuplicateKeysFirstWins(t *testing.T) {
	legacy := runC2Legacy(t, `{"usage":{"input_tokens":5,"input_tokens":9}}`)
	if legacy.usage == nil || legacy.usage.PromptTokens != 9 {
		t.Fatalf("expected legacy last-wins input_tokens=9, got %+v", legacy.usage)
	}
	extraction := extractResponsesDirectNonStreamPayload([]byte(`{"usage":{"input_tokens":5,"input_tokens":9}}`))
	if extraction.Usage == nil || extraction.Usage.InputTokens != 5 {
		t.Fatalf("expected new first-wins input_tokens=5, got %+v", extraction.Usage)
	}
}

// TestResponsesNonStreamDEC_C2_1InvalidBasisNoCharge 锁定 DEC-C2-1。
//
// 变更前证据（实测，见本测试下方断言 `legacy`）：
//
//	body = {"usage":{"input_tokens":5,"output_tokens":"x"}}
//	旧实现 `json.Unmarshal` 失败但被 `_ =` 忽略，payload.Usage 保留部分解析值
//	{input_tokens:5, output_tokens:0, total_tokens:0}，经 toModelUsage 得
//	{PromptTokens:5, CompletionTokens:0, TotalTokens:0} → 会按 5 个 prompt token 扣费。
//
// DEC-C2-1（有意变更）：主计费字段不可解析时整体视为 usage 缺失、不扣费（欠费优于按部分数据扣费）。
// 变更后：extractResponsesDirectNonStreamPayload 返回 UsageInvalid=true 且 Usage=nil。
func TestResponsesNonStreamDEC_C2_1InvalidBasisNoCharge(t *testing.T) {
	const body = `{"usage":{"input_tokens":5,"output_tokens":"x"}}`

	// 1. 现状证据：旧实现确实产生部分扣费输入 {5 0 0}。
	legacy := runC2Legacy(t, body)
	if legacy.relayErr != nil {
		t.Fatalf("legacy should not fail the request, got %+v", legacy.relayErr)
	}
	if legacy.usage == nil || legacy.usage.PromptTokens != 5 || legacy.usage.CompletionTokens != 0 || legacy.usage.TotalTokens != 0 {
		t.Fatalf("DEC-C2-1 evidence: expected legacy partial charge input {5 0 0}, got %+v", legacy.usage)
	}
	if legacy.passthrough != body {
		t.Fatalf("legacy passthrough must stay byte-identical, got %q", legacy.passthrough)
	}

	// 2. 新期望行为：usage 为 nil、UsageInvalid=true、不产生扣费输入。
	extraction := extractResponsesDirectNonStreamPayload([]byte(body))
	if !extraction.WellFormed {
		t.Fatalf("expected body to be well-formed")
	}
	if !extraction.UsageInvalid {
		t.Fatalf("expected UsageInvalid=true under DEC-C2-1, got %+v", extraction)
	}
	if extraction.Usage != nil {
		t.Fatalf("expected nil usage (no charge) under DEC-C2-1, got %+v", extraction.Usage)
	}
	if extraction.Error != nil {
		t.Fatalf("expected no derived upstream error, got %+v", extraction.Error)
	}
}

// TestResponsesNonStreamDEC_C2_1MismatchMatrix 覆盖各主字段类型不符/小数/指数样本：
// 全部要求 usage 为 nil、UsageInvalid=true、不扣费（DEC-C2-1），并同时验证旧实现的部分扣费证据。
func TestResponsesNonStreamDEC_C2_1MismatchMatrix(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "output_tokens_string", body: `{"usage":{"input_tokens":5,"output_tokens":"x"}}`},
		{name: "total_tokens_string", body: `{"usage":{"input_tokens":5,"output_tokens":6,"total_tokens":"x"}}`},
		{name: "input_tokens_array", body: `{"usage":{"input_tokens":[1]}}`},
		{name: "input_tokens_object", body: `{"usage":{"input_tokens":{"a":1}}}`},
		{name: "input_tokens_bool", body: `{"usage":{"input_tokens":true}}`},
		{name: "usage_number", body: `{"usage":5}`},
		{name: "usage_string", body: `{"usage":"x"}`},
		{name: "fractional_5_7", body: `{"usage":{"input_tokens":5.7}}`},
		{name: "fractional_5_0", body: `{"usage":{"input_tokens":5.0}}`},
		{name: "fractional_exponential_1e2", body: `{"usage":{"input_tokens":1e2}}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			legacy := runC2Legacy(t, tc.body)
			// 证据：旧实现不会 abort，且可能产出部分 usage（不为 nil）→ 这正是 DEC-C2-1 修复点。
			if legacy.relayErr != nil {
				t.Fatalf("legacy should not fail the request, got %+v", legacy.relayErr)
			}
			if legacy.passthrough != tc.body {
				t.Fatalf("legacy passthrough must stay byte-identical, got %q", legacy.passthrough)
			}
			extraction := extractResponsesDirectNonStreamPayload([]byte(tc.body))
			if !extraction.UsageInvalid {
				t.Fatalf("expected UsageInvalid=true (DEC-C2-1), got %+v", extraction)
			}
			if extraction.Usage != nil {
				t.Fatalf("expected nil usage (no charge) (DEC-C2-1), got %+v", extraction.Usage)
			}
		})
	}
}

// c2WarnRecorder 捕获 Warnf 输出，其余方法委托给真实 logger（与 helper_actual_channel_test.go
// 的 recordingLogger 同构，仅额外覆盖 Warnf）。
type c2WarnRecorder struct {
	logger.ILogger
	warns []string
}

func (r *c2WarnRecorder) Warnf(format string, args ...interface{}) {
	r.warns = append(r.warns, fmt.Sprintf(format, args...))
}

// TestResponsesNonStreamDEC_C2_1WarningObservable 断言契约 6.2 Step 5 的「warning 可观测」：
// 生产入口 handleResponsesDirectNonStream 在 UsageInvalid（DEC-C2-1）时必须发出 warning 日志，
// 同时 usage 为 nil（不扣费）、上游 body 逐字节透传、ctxkey.ResponseBody 照常存储。
func TestResponsesNonStreamDEC_C2_1WarningObservable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const body = `{"usage":{"input_tokens":5,"output_tokens":"x"}}`

	spy := &c2WarnRecorder{ILogger: logger.Log}
	original := logger.Log
	logger.Log = spy
	t.Cleanup(func() { logger.Log = original })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	usage, relayErr := handleResponsesDirectNonStream(c, resp)

	if relayErr != nil {
		t.Fatalf("DEC-C2-1 must not turn invalid basis into a relay error, got %+v", relayErr)
	}
	if usage != nil {
		t.Fatalf("expected nil usage (no charge) under DEC-C2-1, got %+v", usage)
	}
	if recorder.Body.String() != body {
		t.Fatalf("passthrough must stay byte-identical, got %q", recorder.Body.String())
	}
	if c.GetString(ctxkey.ResponseBody) != body {
		t.Fatalf("ctxkey.ResponseBody must hold original bytes, got %q", c.GetString(ctxkey.ResponseBody))
	}
	if len(spy.warns) == 0 {
		t.Fatalf("expected an observable warning for UsageInvalid under DEC-C2-1, got none")
	}
	if !strings.Contains(spy.warns[0], "DEC-C2-1") {
		t.Fatalf("expected warning to reference DEC-C2-1, got %q", spy.warns[0])
	}
}

// =============================================================================
// 任务 7.3：JSON 热路径基准 —— Responses 多字段请求（model/stream 惰性读取，改造前后对比）
// =============================================================================

// responsesMultiFieldRequestBody 生成约 40KB 的良构 Responses 请求体，含 model/stream/
// instructions/input/tools/max_output_tokens 等多个字段，代表 D 模块的惰性读取场景。
func responsesMultiFieldRequestBody() []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-test","stream":false,"instructions":"system prompt","max_output_tokens":2048,"input":[`)
	for i := 0; i < 120; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"message","role":"user","content":"`)
		sb.WriteString(strings.Repeat("c", 250))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`],"tools":[{"type":"function","name":"tool_a","parameters":{"type":"object"}},{"type":"function","name":"tool_b","parameters":{"type":"object"}}]}`)
	return []byte(sb.String())
}

// BenchmarkJsonParserHotPathResponsesMultiField 对比 D 模块 Responses 请求的改造前后读取开销：
//
//   - before_full_map_unmarshal：改造前 `json.Unmarshal(→map[string]interface{})` 物化整份请求，
//     再从 map 取 model/stream 并驱动两个估算器；
//   - after_lazy_single_valid：改造后一次 json.Valid 预检 + gjson 惰性根按需读取 model/stream
//     与两个估算器（估算器不再各自全量校验）。
//
// 契约 7.3 / AC-5：至少一项（ns/op 或 allocs/op）改善。
func BenchmarkJsonParserHotPathResponsesMultiField(b *testing.B) {
	body := responsesMultiFieldRequestBody()

	b.Run("before_full_map_unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		origApproximate := config.ApproximateTokenEnabled
		config.ApproximateTokenEnabled = true
		b.Cleanup(func() { config.ApproximateTokenEnabled = origApproximate })
		for i := 0; i < b.N; i++ {
			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err != nil {
				b.Fatalf("unmarshal failed: %v", err)
			}
			_, _ = req["model"].(string)
			_, _ = req["stream"].(bool)
			if input, ok := req["input"].([]interface{}); ok {
				_ = len(input) * 100
			}
			if tools, ok := req["tools"].([]interface{}); ok {
				_ = len(tools) * 200
			}
		}
	})

	b.Run("after_lazy_single_valid", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		origApproximate := config.ApproximateTokenEnabled
		config.ApproximateTokenEnabled = true
		b.Cleanup(func() { config.ApproximateTokenEnabled = origApproximate })
		for i := 0; i < b.N; i++ {
			_ = json.Valid(body)
			root := gjson.ParseBytes(body)
			rootKind := responsesRootKindOf(root, true)
			_, _ = extractModelCaseInsensitive(body)
			_ = root.Get("stream")
			_, _ = responsesPreConsumeEstimates(root, rootKind)
		}
	})
}

// benchmarkC2LargeResponse 生成约 86KB 的直连 /v1/responses 非流式响应体（reviewer 报告的
// C2 B/op 上升样本：新路径 ~90KB/op vs 旧路径 ~552B/op）。
func benchmarkC2LargeResponse() []byte {
	var sb strings.Builder
	sb.WriteString(`{"id":"resp_big","object":"response","status":"completed","model":"deepseek-reasoner","output":[`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`)
		sb.WriteString(strings.Repeat("r", 2000))
		sb.WriteString(`"}]}`)
	}
	sb.WriteString(`],"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}`)
	return []byte(sb.String())
}

// BenchmarkJsonParserHotPathDirectNonStreamUsage 对比 C2（handleResponsesDirectNonStream）的
// 非流式 usage 提取改造前后开销：
//   - before_ignored_partial_decode：旧实现 `_ = json.Unmarshal(→匿名 struct{Error,Usage})`，
//     仅解码 usage 子集（无关 output 字段被跳过 → 分配极小）；
//   - after_on_demand：新实现 `extractResponsesDirectNonStreamPayload` 先做 json.Valid 全量扫描，
//     再经 gjson 按路径取 usage。
//
// 测量证据：新路径延迟通常更低（省去无关字段解析），但 B/op 因 json.Valid 全量读取 + gjson 内部
// 拷贝而上升。该现象在 7.3 报告中如实记录为 documented measurement evidence。
func BenchmarkJsonParserHotPathDirectNonStreamUsage(b *testing.B) {
	body := benchmarkC2LargeResponse()

	b.Run("before_ignored_partial_decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			var payload struct {
				Error *model.Error    `json:"error"`
				Usage *responsesUsage `json:"usage"`
			}
			_ = json.Unmarshal(body, &payload)
			if payload.Usage != nil {
				_ = payload.Usage.toModelUsage()
			}
		}
	})

	b.Run("after_on_demand", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			extraction := extractResponsesDirectNonStreamPayload(body)
			if extraction.Usage != nil {
				_ = extraction.Usage.toModelUsage()
			}
		}
	})
}
