package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/render"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
)

const maxSSEEventBytes = constant.ScannerBufferMax * 2

type sseEvent struct {
	Event   string
	Data    string
	ID      string
	RawSize int
	Done    bool
}

type SSEEvent struct {
	Event   string
	Data    string
	ID      string
	RawSize int
	Done    bool
}

func (e SSEEvent) String() string {
	var b strings.Builder
	if e.Event != "" {
		b.WriteString("event: ")
		b.WriteString(e.Event)
		b.WriteByte('\n')
	}
	if e.Data != "" {
		for i, line := range strings.Split(e.Data, "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("data: ")
			b.WriteString(line)
		}
	}
	return b.String()
}

const (
	dataPrefix        = "data: "
	eventPrefix       = "event: "
	done              = "[DONE]"
	dataPrefixLength  = len(dataPrefix)
	eventPrefixLength = len(eventPrefix)
)

var ModelList = []string{
	"gpt-4o",
	"gpt-4o-mini",
	"gpt-4-turbo",
	"gpt-4",
	"gpt-3.5-turbo",
	"o1",
	"o1-mini",
}

func DoResponsesResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	var textResponse model.ResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "read_response_body_failed", err)
		return nil, ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	err = resp.Body.Close()
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "close_response_body_failed", err)
		return nil, ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
	}

	err = json.Unmarshal(responseBody, &textResponse)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "invalid_json_response", err)
		return nil, ErrorWrapper(err, "invalid_json_response", http.StatusInternalServerError)
	}

	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))

	for k, v := range resp.Header {
		for _, vv := range v {
			c.Writer.Header().Add(k, vv)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "copy_response_body_failed", err)
		return nil, ErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
	}
	err = resp.Body.Close()
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "close_response_body_failed", err)
		return nil, ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
	}

	usage := &model.Usage{
		PromptTokens:     textResponse.Usage.InputTokens,
		CompletionTokens: textResponse.Usage.OutputTokens,
		TotalTokens:      textResponse.Usage.TotalTokens,
	}
	// 如果有缓存命中的token，设置到 PromptTokensDetails 中
	if textResponse.Usage.InputTokensDetails != nil && textResponse.Usage.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens: textResponse.Usage.InputTokensDetails.CachedTokens,
		}
	}

	c.Set(ctxkey.ResponseBody, string(responseBody))
	return usage, nil
}

func StreamResponsesHandler(c *gin.Context, resp *http.Response) (*model.ErrorWithStatusCode, string, *model.Usage) {
	responseText := ""
	reader := bufio.NewReaderSize(resp.Body, constant.ScannerBufferInitial)
	var usage *model.Usage
	capture := model.ResponsesStreamCapture{}
	var currentFrame *model.ResponsesStreamFrame
	var deltaText strings.Builder
	var deltaFrame *model.ResponsesStreamFrame
	sawFailedTerminal := false
	sawCompletedTerminal := false
	sentSyntheticCompleted := false
	incompleteStream := false
	streamBegan := false
	sawDone := false
	lastEventType := ""
	eventCount := 0
	var outputItems []model.ResponsesItem
	var outputItemByID = map[string]int{}
	var skippedItemIDs = map[string]struct{}{}

	flushFrame := func() {
		if currentFrame != nil {
			capture.Frames = append(capture.Frames, *currentFrame)
			currentFrame = nil
		}
	}
	flushDeltaFrame := func() {
		if deltaFrame != nil {
			payload := map[string]any{
				"type":  "response.output_text.delta",
				"delta": deltaText.String(),
			}
			if data, err := json.Marshal(payload); err == nil {
				deltaFrame.Data = json.RawMessage(data)
				capture.Frames = append(capture.Frames, *deltaFrame)
			}
			deltaFrame = nil
			deltaText.Reset()
		}
	}

	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()
	streamBegan = true

	doneRendered := false
	var streamError model.Error
	firstEvent, err := readSSEEvent(reader, maxSSEEventBytes)
	if err != nil {
		streamError = buildStreamReadError(err)
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Log.Warnf("failed to close response body on initial stream read error path: %v", closeErr)
		}
		rawErrorPayload := renderTerminalStreamErrorEventPayload(streamError)
		render.EventData(c, "error", rawErrorPayload)
		capture.Frames = append(capture.Frames, model.ResponsesStreamFrame{
			Event: "error",
			Data:  json.RawMessage(rawErrorPayload),
		})
		finalizeStreamCapture(c, &capture, usage)
		return nil, responseText, usage
	}

	if firstEventErr, ok := classifyTerminalStreamError(firstEvent); ok {
		if initialCapture, ok := buildInitialTerminalCapture(firstEvent); ok {
			usage = usageFromInitialCapture(initialCapture)
			finalizeStreamCapture(c, initialCapture, usage)
		}
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Log.Warnf("failed to close response body on initial terminal event path: %v", closeErr)
		}
		statusCode := mapFailedErrorToStatusCode(fmt.Sprintf("%v", firstEventErr.Code), firstEventErr.Type, firstEventErr.Message)
		if streamBegan {
			return nil, responseText, usage
		}
		return &model.ErrorWithStatusCode{Error: firstEventErr, StatusCode: statusCode}, responseText, usage
	}

	eventCount++
	state := &sseProcessState{
		currentFrame:           &currentFrame,
		deltaFrame:             &deltaFrame,
		deltaText:              &deltaText,
		capture:                &capture,
		usage:                  &usage,
		outputItems:            &outputItems,
		outputItemByID:         &outputItemByID,
		skippedItemIDs:         &skippedItemIDs,
		doneRendered:           &doneRendered,
		responseText:           &responseText,
		streamError:            &streamError,
		sawFailedTerminal:      &sawFailedTerminal,
		sawCompletedTerminal:   &sawCompletedTerminal,
		sentSyntheticCompleted: &sentSyntheticCompleted,
		sawDone:                &sawDone,
		lastEventType:          &lastEventType,
	}
	flushers := flushCallbacks{flushFrame: flushFrame, flushDeltaFrame: flushDeltaFrame}
	processSSEEvent(firstEvent, state, flushers, c)
	for {
		event, err := readSSEEvent(reader, maxSSEEventBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, context.Canceled) && (sawCompletedTerminal || sawFailedTerminal) {
				logger.Log.Debugf("[StreamResponsesHandler] stream context canceled after terminal event: user_text=%q done_rendered=%v frames=%d last_event=%q",
					responseText, doneRendered, len(capture.Frames), lastEventType)
				break
			}
			incompleteStream = true
			logger.Log.Errorf("[StreamResponsesHandler] stream read error: %v user_text=%q done_rendered=%v frames=%d",
				err, responseText, doneRendered, len(capture.Frames))
			if streamError == (model.Error{}) {
				streamError = buildStreamReadError(err)
			}
			break
		}
		eventCount++
		processSSEEvent(event, state, flushers, c)
	}

	logger.Log.Infof("[StreamResponsesHandler] stream done: frames=%d events=%d output_items=%d text_len=%d usage=%v done=%v upstream_completed=%v synthetic_completed=%v incomplete=%v stream_error=%v failed_terminal=%v last_event=%q",
		len(capture.Frames), eventCount, len(outputItems), len(responseText),
		func() string {
			if usage == nil {
				return "nil"
			}
			return fmt.Sprintf("p%d_c%d_t%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
		}(),
		sawDone,
		sawCompletedTerminal && !sentSyntheticCompleted,
		sentSyntheticCompleted,
		incompleteStream,
		streamError.Message != "",
		sawFailedTerminal,
		lastEventType)

	if !doneRendered && !sawCompletedTerminal {
		incompleteStream = true
	}

	if !doneRendered && !incompleteStream {
		render.Done(c)
	}

	if streamError.Message != "" {
		flushDeltaFrame()
		flushFrame()
		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body on stream error path: %v", err)
		}
		if streamBegan {
			if !sawCompletedTerminal && !sawFailedTerminal {
				markCaptureResponseFailed(&capture, streamError)
				capture.Frames = append(capture.Frames, model.ResponsesStreamFrame{
					Event: "error",
					Data:  renderTerminalStreamErrorEvent(c, streamError),
				})
			}
			finalizeStreamCapture(c, &capture, usage)
			return nil, responseText, usage
		}
		errCode := fmt.Sprintf("%v", streamError.Code)
		statusCode := mapFailedErrorToStatusCode(errCode, streamError.Type, streamError.Message)
		return &model.ErrorWithStatusCode{Error: streamError, StatusCode: statusCode}, responseText, usage
	}

	flushFrame()
	flushDeltaFrame()
	if !incompleteStream {
		finalizeMissingCompletedCapture(&capture, &usage, outputItems, doneRendered)
	}
	finalizeStreamCapture(c, &capture, usage)

	err = resp.Body.Close()
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "close_response_body_failed", err)
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), "", nil
	}

	return nil, responseText, usage
}

func readSSEEvent(r *bufio.Reader, maxBytes int) (sseEvent, error) {
	var event sseEvent
	var dataLines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return sseEvent{}, err
		}
		if len(line) > 0 {
			// RawSize 包含换行符，作为安全上限使用，略大于实际 wire bytes。
		event.RawSize += len(line)
			if event.RawSize > maxBytes {
				return sseEvent{}, fmt.Errorf("sse event too large: %d > %d", event.RawSize, maxBytes)
			}
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if event.Event != "" || len(dataLines) > 0 {
				event.Data = strings.Join(dataLines, "\n")
				event.Done = event.Data == done
				return event, nil
			}
			if errors.Is(err, io.EOF) {
				return sseEvent{}, io.EOF
			}
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			if errors.Is(err, io.EOF) {
				return sseEvent{}, io.ErrUnexpectedEOF
			}
			continue
		}
		fieldName, fieldValue, ok := parseSSEField(trimmed)
		if ok {
			switch fieldName {
			case "event":
				event.Event = fieldValue
			case "data":
				dataLines = append(dataLines, fieldValue)
		case "id":
			event.ID = fieldValue
			// 记录 Last-Event-ID 用于调试
			if fieldValue != "" {
				logger.Log.Debugf("[readSSEEvent] received id: %s", fieldValue)
			}
			}
		}
		if errors.Is(err, io.EOF) {
			if event.Event != "" || len(dataLines) > 0 {
				event.Data = strings.Join(dataLines, "\n")
				event.Done = event.Data == done
				return event, nil
			}
			return sseEvent{}, io.ErrUnexpectedEOF
		}
	}
}

func classifyTerminalStreamError(event sseEvent) (model.Error, bool) {
	payload := event.Data
	eventType := event.Event
	if payload == "" && eventType == "" {
		return model.Error{}, false
	}
	if payload == done {
		return model.Error{}, false
	}
	if payload != "" {
		var eventProbe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(payload), &eventProbe); err == nil && eventProbe.Type != "" {
			eventType = eventProbe.Type
		}
	}

	if event.Event == "error" || eventType == "error" {
		return parseStreamErrorEvent(payload)
	}

	if eventType != "response.failed" {
		return model.Error{}, false
	}

	var streamResponse model.ResponsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &streamResponse); err != nil {
		return model.Error{}, false
	}
	return buildFailedStreamError(streamResponse.Response), true
}

func buildInitialTerminalCapture(event sseEvent) (*model.ResponsesStreamCapture, bool) {
	payload := event.Data
	if payload == "" || payload == done {
		return nil, false
	}

	var streamResponse model.ResponsesStreamEvent
	if err := json.Unmarshal([]byte(payload), &streamResponse); err != nil {
		return nil, false
	}

	if streamResponse.Type != "response.failed" {
		return nil, false
	}

	capture := &model.ResponsesStreamCapture{}
	if streamResponse.Usage != nil {
		capture.Usage = streamResponse.Usage
	}
	if streamResponse.Response != nil {
		rememberResponseSnapshot(capture, streamResponse.Response)
		capture.Usage = &streamResponse.Response.Usage
		capture.Response.Status = "failed"
		capture.Response.Error = streamResponse.Response.Error
	}
	return capture, capture.Response != nil || capture.Usage != nil
}

func usageFromInitialCapture(capture *model.ResponsesStreamCapture) *model.Usage {
	if capture == nil {
		return nil
	}
	var source *model.ResponsesUsage
	if capture.Usage != nil {
		source = capture.Usage
	} else if capture.Response != nil {
		source = &capture.Response.Usage
	}
	if source == nil {
		return nil
	}
	if !responsesUsagePresent(source) {
		return nil
	}
	var usage *model.Usage
	setUsageFromResponsesUsage(&usage, source)
	return usage
}

func ReadSSEEvent(r *bufio.Reader, maxBytes int) (SSEEvent, error) {
	event, err := readSSEEvent(r, maxBytes)
	if err != nil {
		return SSEEvent{}, err
	}
	return SSEEvent(event), nil
}

func parseSSEField(line string) (string, string, bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	name := line[:idx]
	value := line[idx+1:]
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}
	return name, value, true
}

func finalizeStreamCapture(c *gin.Context, capture *model.ResponsesStreamCapture, usage *model.Usage) {
	if capture == nil {
		return
	}
	if capture.Response != nil {
		if capture.Usage == nil {
			capture.Usage = &capture.Response.Usage
		}
		if capture.Usage != nil {
			capture.Response.Usage = *capture.Usage
		}
	} else if usage == nil {
		return
	}
	if len(capture.Frames) > 0 || capture.Response != nil {
		if respJSON, err := json.Marshal(capture); err == nil {
			c.Set(ctxkey.ResponseBody, string(respJSON))
		}
	}
}

// mapFailedErrorToStatusCode 根据错误码/类型映射到 HTTP 状态码，便于网关重试逻辑判断
func mapFailedErrorToStatusCode(code, errType, message string) int {
	codeLower := strings.ToLower(code)
	typeLower := strings.ToLower(errType)
	msgLower := strings.ToLower(message)

	// 429 - 限流相关，触发重试
	if strings.Contains(codeLower, "rate_limit") ||
		strings.Contains(codeLower, "rate-limit") ||
		strings.Contains(msgLower, "rate limit") ||
		strings.Contains(msgLower, "concurrency limit") ||
		strings.Contains(msgLower, "too many requests") {
		return http.StatusTooManyRequests
	}

	// 5xx - 服务端错误，触发重试
	if strings.Contains(codeLower, "server_error") ||
		// Codex API 已知的临时性错误码，表示请求失败但非客户端问题，映射到 502 便于网关重试
		strings.Contains(codeLower, "request_failed") ||
		strings.Contains(typeLower, "server_error") ||
		strings.Contains(typeLower, "unavailable") ||
		strings.Contains(typeLower, "internal_error") ||
		strings.Contains(msgLower, "internal server error") ||
		strings.Contains(msgLower, "service unavailable") ||
		strings.Contains(msgLower, "bad gateway") ||
		strings.Contains(msgLower, "request timeout") ||
		strings.Contains(msgLower, "timed out") ||
		strings.Contains(msgLower, "deadline exceeded") ||
		strings.Contains(msgLower, "connection timeout") ||
		// 服务暂时不可用的通用消息模式，属于服务端临时故障，映射到 502 便于网关重试
		strings.Contains(msgLower, "temporarily unavailable") {
		return http.StatusBadGateway
	}

	// 4xx - 客户端错误，默认不重试
	return http.StatusBadRequest
}

// parseStreamErrorEvent 从 SSE payload 中解析 error 事件，返回构造好的 Error 对象。
// 若 payload 解析失败则返回零值和 false。
func parseStreamErrorEvent(payload string) (model.Error, bool) {
	var errEvent model.ResponseStreamErrorEvent
	if err := json.Unmarshal([]byte(payload), &errEvent); err != nil {
		return model.Error{}, false
	}
	errMsg := "upstream stream error"
	errCode := "server_error"
	if errEvent.Message != "" {
		errMsg = errEvent.Message
	}
	if errEvent.Code != "" {
		errCode = errEvent.Code
	}
	return model.Error{
		Message: errMsg,
		Type:    "upstream_error",
		Code:    errCode,
	}, true
}

func buildStreamReadError(err error) model.Error {
	message := "empty upstream stream"
	if err != nil && !errors.Is(err, io.EOF) {
		message = err.Error()
	}
	return model.Error{
		Message: message,
		Type:    "stream_read_error",
		Code:    "bad_response",
	}
}

func renderTerminalStreamErrorEventPayload(streamErr model.Error) string {
	payload, err := json.Marshal(model.ResponseStreamErrorEvent{
		Type:    "error",
		Code:    fmt.Sprintf("%v", streamErr.Code),
		Message: streamErr.Message,
	})
	if err != nil {
		logger.Log.Warnf("failed to marshal terminal stream error event: %v", err)
		return `{"type":"error","code":"bad_response","message":"upstream stream error"}`
	}
	return string(payload)
}

func renderTerminalStreamErrorEvent(c *gin.Context, streamErr model.Error) json.RawMessage {
	payload := renderTerminalStreamErrorEventPayload(streamErr)
	render.EventData(c, "error", payload)
	return json.RawMessage(payload)
}

func RenderTerminalStreamReadErrorEvent(c *gin.Context, err error) {
	renderTerminalStreamErrorEvent(c, buildStreamReadError(err))
}

func markCaptureResponseFailed(capture *model.ResponsesStreamCapture, streamErr model.Error) {
	if capture == nil || capture.Response == nil || capture.Response.Status == "completed" {
		return
	}
	capture.Response.Status = "failed"
	capture.Response.Error = &model.ResponseError{
		Code:    fmt.Sprintf("%v", streamErr.Code),
		Message: streamErr.Message,
		Type:    streamErr.Type,
	}
}

func setUsageFromResponsesUsage(target **model.Usage, source *model.ResponsesUsage) {
	if source == nil {
		return
	}
	*target = &model.Usage{
		PromptTokens:     source.InputTokens,
		CompletionTokens: source.OutputTokens,
		TotalTokens:      source.TotalTokens,
	}
	if source.InputTokensDetails != nil && source.InputTokensDetails.CachedTokens > 0 {
		(*target).PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens: source.InputTokensDetails.CachedTokens,
		}
	}
}

func finalizeCompletedCapture(capture *model.ResponsesStreamCapture, usage **model.Usage, completedResponse *model.ResponsesResponse, outputItems []model.ResponsesItem) {
	if completedResponse == nil {
		return
	}

	respCopy := *completedResponse
	if len(respCopy.Output) == 0 && len(outputItems) > 0 {
		respCopy.Output = append([]model.ResponsesItem(nil), outputItems...)
	}
	if capture.Usage != nil {
		respCopy.Usage = *capture.Usage
	} else {
		capture.Usage = &respCopy.Usage
	}
	setUsageFromResponsesUsage(usage, &respCopy.Usage)
	capture.Response = &respCopy
}

func rememberResponseSnapshot(capture *model.ResponsesStreamCapture, response *model.ResponsesResponse) {
	if response == nil {
		return
	}
	respCopy := *response
	if capture.Usage != nil {
		respCopy.Usage = *capture.Usage
	}
	capture.Response = &respCopy
}

func canSafelyFinalizeMissingCompleted(capture *model.ResponsesStreamCapture, outputItems []model.ResponsesItem, doneRendered bool) bool {
	if !doneRendered || capture == nil || capture.Response == nil {
		return false
	}
	if capture.Response.Status == "failed" || capture.Response.ID == "" || capture.Response.Model == "" {
		return false
	}
	hasOutput := len(capture.Response.Output) > 0 || len(outputItems) > 0
	hasUsage := responsesUsagePresent(capture.Usage) || responsesUsagePresent(&capture.Response.Usage)
	if !hasOutput && !hasUsage {
		return false
	}
	return true
}

func responsesUsagePresent(usage *model.ResponsesUsage) bool {
	if usage == nil {
		return false
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 {
		return true
	}
	if usage.InputTokensDetails != nil || usage.OutputTokensDetails != nil {
		return true
	}
	if usage.CacheCreationInputTokens != 0 || usage.CacheCreation5mInputTokens != 0 || usage.CacheCreation1hInputTokens != 0 || usage.CacheReadInputTokens != 0 {
		return true
	}
	return usage.CacheTTL != ""
}

func buildSyntheticCompletedResponse(capture *model.ResponsesStreamCapture, outputItems []model.ResponsesItem) *model.ResponsesResponse {
	if capture == nil || capture.Response == nil {
		return nil
	}
	respCopy := *capture.Response
	respCopy.Status = "completed"
	if len(outputItems) > 0 {
		respCopy.Output = append([]model.ResponsesItem(nil), outputItems...)
	}
	if capture.Usage != nil {
		respCopy.Usage = *capture.Usage
	}
	return &respCopy
}

func buildSyntheticCompletedPayload(capture *model.ResponsesStreamCapture, outputItems []model.ResponsesItem) ([]byte, *model.ResponsesResponse, bool) {
	completedResponse := buildSyntheticCompletedResponse(capture, outputItems)
	if completedResponse == nil {
		return nil, nil, false
	}
	payload, err := json.Marshal(map[string]any{
		"type":     "response.completed",
		"response": completedResponse,
	})
	if err != nil {
		logger.Log.Warnf("failed to marshal synthetic response.completed payload: %v", err)
		return nil, nil, false
	}
	return payload, completedResponse, true
}

func finalizeMissingCompletedCapture(capture *model.ResponsesStreamCapture, usage **model.Usage, outputItems []model.ResponsesItem, doneRendered bool) {
	if capture == nil || capture.Response == nil || capture.Response.Status == "completed" {
		return
	}
	if len(capture.Response.Output) == 0 && len(outputItems) > 0 {
		capture.Response.Output = append([]model.ResponsesItem(nil), outputItems...)
	}
	if capture.Usage != nil {
		capture.Response.Usage = *capture.Usage
		setUsageFromResponsesUsage(usage, capture.Usage)
	}
}

func buildFailedStreamError(resp *model.ResponsesResponse) model.Error {
	errMsg := "upstream response failed"
	errType := "upstream_error"
	errCode := "response_failed"
	if resp != nil && resp.Error != nil {
		if resp.Error.Message != "" {
			errMsg = resp.Error.Message
		}
		if resp.Error.Type != "" {
			errType = resp.Error.Type
		}
		if resp.Error.Code != "" {
			errCode = resp.Error.Code
		}
	}
	return model.Error{
		Message: errMsg,
		Type:    errType,
		Code:    errCode,
	}
}

type sseProcessState struct {
	currentFrame        **model.ResponsesStreamFrame
	deltaFrame          **model.ResponsesStreamFrame
	deltaText           *strings.Builder
	capture             *model.ResponsesStreamCapture
	usage               **model.Usage
	outputItems         *[]model.ResponsesItem
	outputItemByID      *map[string]int
	skippedItemIDs      *map[string]struct{}
	doneRendered        *bool
	responseText        *string
	streamError         *model.Error
	sawFailedTerminal   *bool
	sawCompletedTerminal   *bool
	sentSyntheticCompleted *bool
	sawDone             *bool
	lastEventType       *string
}

type flushCallbacks struct {
	flushFrame      func()
	flushDeltaFrame func()
}

func processSSEEvent(
	event sseEvent,
	state *sseProcessState,
	flushers flushCallbacks,
	c *gin.Context,
) {
	flushers.flushFrame()
	payload := event.Data
	eventType := event.Event
	if state.lastEventType != nil {
		*state.lastEventType = eventType
	}
	if payload == "" && eventType == "" {
		return
	}
	if payload != done {
		var eventProbe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(payload), &eventProbe); err == nil && eventProbe.Type != "" {
			eventType = eventProbe.Type
		}
	}

	if *state.sawFailedTerminal && eventType != "" && eventType != "error" {
		if strings.HasPrefix(payload, done) {
			render.EventData(c, event.Event, payload)
			*state.doneRendered = true
		}
		return
	}
	if *state.sawCompletedTerminal && payload != done && eventType != "error" {
		return
	}
	if state.streamError != nil && *state.streamError != (model.Error{}) && eventType != "error" && payload != done {
		return
	}

	if strings.HasPrefix(payload, done) {
		flushers.flushDeltaFrame()
		if state.sawDone != nil {
			*state.sawDone = true
		}
		if !*state.sawCompletedTerminal && !*state.sawFailedTerminal && state.streamError != nil && *state.streamError == (model.Error{}) && canSafelyFinalizeMissingCompleted(state.capture, *state.outputItems, true) {
			if syntheticPayload, syntheticResponse, ok := buildSyntheticCompletedPayload(state.capture, *state.outputItems); ok {
				render.EventData(c, "response.completed", string(syntheticPayload))
				state.capture.Frames = append(state.capture.Frames, model.ResponsesStreamFrame{
					Event: "response.completed",
					Data:  json.RawMessage(syntheticPayload),
				})
				finalizeCompletedCapture(state.capture, state.usage, syntheticResponse, *state.outputItems)
				*state.sawCompletedTerminal = true
				if state.sentSyntheticCompleted != nil {
					*state.sentSyntheticCompleted = true
				}
				if state.lastEventType != nil {
					*state.lastEventType = "response.completed(synthetic)"
				}
			}
		}
		if *state.currentFrame == nil {
			*state.currentFrame = &model.ResponsesStreamFrame{Event: event.Event}
		}
		if (*state.currentFrame).Event == "" {
			(*state.currentFrame).Event = event.Event
		}
		(*state.currentFrame).Data = json.RawMessage(`"[DONE]"`)
		(*state.currentFrame).Done = true
		render.EventData(c, event.Event, payload)
		*state.doneRendered = true
		if state.lastEventType != nil && *state.lastEventType == "" {
			*state.lastEventType = done
		}
		return
	}

	var streamResponse model.ResponsesStreamEvent
	err := json.Unmarshal([]byte(payload), &streamResponse)
	if err != nil {
		logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
		render.EventData(c, event.Event, payload)
		return
	}

	eventType = streamResponse.Type
	if eventType == "" {
		eventType = event.Event
	}
	if state.lastEventType != nil && eventType != "" {
		*state.lastEventType = eventType
	}

	if event.Event == "error" || eventType == "error" {
		if state.streamError != nil && *state.streamError != (model.Error{}) {
			return
		}
		render.EventData(c, "error", payload)
		// 设计意图：仅记录第一个 error 事件。后续错误事件不再覆盖，避免丢失首次错误信息。
		if state.streamError != nil && *state.streamError == (model.Error{}) {
			// 注意：streamResponse 已在上方解析，但 ResponsesStreamEvent 不包含 error 事件的 Message/Code 字段，
			// 必须使用 ResponseStreamErrorEvent 重新解析以获取错误详情。
			if errEvent, ok := parseStreamErrorEvent(payload); ok {
				*state.streamError = errEvent
				// model.Error.Code 是 any 类型，使用 fmt.Sprintf 进行防御性转换
				errCode := fmt.Sprintf("%v", errEvent.Code)
				logger.Log.Warnf("stream error event detected: event=%s code=%s message=%s", event.Event, errCode, errEvent.Message)
			}
		}
		return
	}

	if eventType == "response.failed" {
		if streamResponse.Usage != nil {
			state.capture.Usage = streamResponse.Usage
			setUsageFromResponsesUsage(state.usage, streamResponse.Usage)
		}
		if streamResponse.Response != nil {
			if responsesUsagePresent(&streamResponse.Response.Usage) || state.capture.Usage == nil {
				state.capture.Usage = &streamResponse.Response.Usage
				setUsageFromResponsesUsage(state.usage, &streamResponse.Response.Usage)
			}
		}
		render.EventData(c, "response.failed", payload)
		*state.sawFailedTerminal = true
		if streamResponse.Response != nil {
			rememberResponseSnapshot(state.capture, streamResponse.Response)
			if state.capture.Usage != nil {
				state.capture.Response.Usage = *state.capture.Usage
			}
			state.capture.Response.Status = "failed"
			state.capture.Response.Error = streamResponse.Response.Error
		}
		if state.streamError != nil && *state.streamError == (model.Error{}) {
			failedErr := buildFailedStreamError(streamResponse.Response)
			*state.streamError = failedErr
			errCode := fmt.Sprintf("%v", failedErr.Code)
			logger.Log.Warnf("stream failed event detected: code=%s message=%s", errCode, failedErr.Message)
		}
		return
	}

	if eventType == "response.completed" {
		*state.sawCompletedTerminal = true
	}

	render.EventData(c, eventType, payload)

	if eventType == "response.output_text.delta" {
		if streamResponse.Delta != nil {
			if s, ok := streamResponse.Delta.(string); ok {
				state.deltaText.WriteString(s)
				*state.responseText += s
			}
		}
		if *state.deltaFrame == nil {
			*state.deltaFrame = &model.ResponsesStreamFrame{Event: eventType}
		}
		return
	}
	if strings.HasSuffix(eventType, ".delta") {
		return
	}

	flushers.flushDeltaFrame()

	if *state.currentFrame == nil {
		*state.currentFrame = &model.ResponsesStreamFrame{Event: event.Event}
	}
	if (*state.currentFrame).Event == "" {
		(*state.currentFrame).Event = event.Event
	}
	(*state.currentFrame).Data = json.RawMessage(payload)

	if eventType == "response.output_item.added" || eventType == "response.output_item.done" {
		if streamResponse.Item == nil {
			return
		}
		itemID := streamResponse.Item.ID
		if itemID == "" {
			itemID = streamResponse.Item.CallID
		}
		if itemID == "" {
			logger.Log.Errorf("skip output item without id")
			return
		}
		if !shouldKeepResponsesOutputItem(streamResponse.Item.Type) {
			(*state.skippedItemIDs)[itemID] = struct{}{}
			return
		}
		if _, skipped := (*state.skippedItemIDs)[itemID]; skipped {
			return
		}
		if eventType == "response.output_item.added" {
			(*state.outputItemByID)[itemID] = len(*state.outputItems)
			*state.outputItems = append(*state.outputItems, *streamResponse.Item)
		} else if idx, ok := (*state.outputItemByID)[itemID]; ok && idx < len(*state.outputItems) {
			(*state.outputItems)[idx] = *streamResponse.Item
		} else {
			*state.outputItems = append(*state.outputItems, *streamResponse.Item)
		}
	}

	if streamResponse.Usage != nil {
		state.capture.Usage = streamResponse.Usage
		setUsageFromResponsesUsage(state.usage, streamResponse.Usage)
	}

	if streamResponse.Response != nil {
		rememberResponseSnapshot(state.capture, streamResponse.Response)
		if responsesUsagePresent(&streamResponse.Response.Usage) || state.capture.Usage == nil {
			state.capture.Usage = &streamResponse.Response.Usage
			setUsageFromResponsesUsage(state.usage, &streamResponse.Response.Usage)
		}
	}

	if eventType == "response.completed" && streamResponse.Response != nil {
		finalizeCompletedCapture(state.capture, state.usage, streamResponse.Response, *state.outputItems)
	}
}

func shouldKeepResponsesOutputItem(itemType string) bool {
	switch itemType {
	case "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "tool_search_call":
		return true
	default:
		return false
	}
}

func ErrorWrapper(err error, code string, statusCode int) *model.ErrorWithStatusCode {
	return &model.ErrorWithStatusCode{
		Error: model.Error{
			Message: err.Error(),
			Type:    "one_api_error",
			Param:   "",
			Code:    code,
		},
		StatusCode: statusCode,
	}
}

// appendToFile 追加内容到文件（文件不存在则创建）
func AppendToFile(filename string, content string) {
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, err = f.WriteString(content)
	if err != nil {
		fmt.Println("追加文件报错", filename, err)
	}
}
