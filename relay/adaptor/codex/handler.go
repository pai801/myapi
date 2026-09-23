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
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/render"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/tidwall/gjson"
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
	"gpt-5.5",
	"gpt-5.4-mini",
	"gpt-5.4",
}

func DoResponsesResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
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

	// 按需扫描消费 usage（契约 6.3）：usage 域内畸形 JSON / 非法计费 basis 保留既有
	// `invalid_json_response` 出口；details 降级为缺失。非 usage 顶层字段类型不符不再使整体失败
	// （见 Sanctioned C3 Non-Usage Field Difference）。
	// 重复键差异（AC-9）：本路径经 gjson 取 first-wins，旧 typed 解码为 last-wins
	// （由 TestExtractResponsesUsageEquivalence 的 duplicate 用例锁定）。
	responsesUsage, err := extractResponsesUsage(responseBody)
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

	usage := responsesUsageToInternalUsage(&responsesUsage)

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
		// SSE 头已提交：失败已由 error 事件写回客户端，这里只回传错误供渠道失败记账。
		return streamFailureError(streamError), responseText, usage
	}

	if firstEventErr, ok := classifyTerminalStreamError(firstEvent); ok {
		if initialCapture, ok := buildInitialTerminalCapture(firstEvent); ok {
			usage = usageFromInitialCapture(initialCapture)
			finalizeStreamCapture(c, initialCapture, usage)
		}
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Log.Warnf("failed to close response body on initial terminal event path: %v", closeErr)
		}
		// SSE 头已提交：首帧即成失败终态时不得返回 nil，否则渠道被误记成功；错误对象仅供记账，
		// 框架层依据 Written() 抑制重试与 JSON 渲染，禁止再向客户端追加任何内容。
		return streamFailureError(firstEventErr), responseText, usage
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
		if !sawCompletedTerminal && !sawFailedTerminal {
			markCaptureResponseFailed(&capture, streamError)
			capture.Frames = append(capture.Frames, model.ResponsesStreamFrame{
				Event: "error",
				Data:  renderTerminalStreamErrorEvent(c, streamError),
			})
		}
		finalizeStreamCapture(c, &capture, usage)
		if sawCompletedTerminal {
			// 已见成功终态：迟到错误只保留在 capture 供观测，不判渠道失败（客户端已收到完整成功响应）。
			return nil, responseText, usage
		}
		// SSE 已提交后的流失败：回传非 nil 错误供渠道失败记账，禁止再向客户端追加任何写出。
		return streamFailureError(streamError), responseText, usage
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

	// EOF 无终态：SSE 已提交且上游未给出 completed/incomplete/failed 任一终态，判流失败供渠道记账；
	// 客户端流已按既有帧写出，禁止追加任何内容。
	if incompleteStream {
		return streamFailureError(streamError), responseText, usage
	}

	return nil, responseText, usage
}

func readSSEEvent(r *bufio.Reader, maxBytes int) (sseEvent, error) {
	var event sseEvent
	var dataLines []string
	var lineBuf []byte
	for {
		var isEOF bool
		for {
			// 用 ReadSlice 按内部缓冲分片读取并逐片做配额检查：ReadString 会在遇到 \n 前
			// 无界累积内存（配额只在整行读完后才生效），分片读取保证内存上界为 maxBytes
			frag, ferr := r.ReadSlice('\n')
			if len(frag) > 0 {
				// RawSize 包含换行符，作为安全上限使用，略大于实际 wire bytes。
				event.RawSize += len(frag)
				if event.RawSize > maxBytes {
					return sseEvent{}, fmt.Errorf("sse event too large: %d > %d", event.RawSize, maxBytes)
				}
				lineBuf = append(lineBuf, frag...)
			}
			if errors.Is(ferr, bufio.ErrBufferFull) {
				// 分片已满但行未结束，继续读下一段
				continue
			}
			if ferr != nil && !errors.Is(ferr, io.EOF) {
				return sseEvent{}, ferr
			}
			isEOF = errors.Is(ferr, io.EOF)
			break
		}
		line := string(lineBuf)
		lineBuf = lineBuf[:0]
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if event.Event != "" || len(dataLines) > 0 {
				event.Data = strings.Join(dataLines, "\n")
				event.Done = event.Data == done
				return event, nil
			}
			if isEOF {
				return sseEvent{}, io.EOF
			}
			continue
		}
		if strings.HasPrefix(trimmed, ":") {
			if isEOF {
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
		if isEOF {
			if event.Event != "" || len(dataLines) > 0 {
				event.Data = strings.Join(dataLines, "\n")
				event.Done = event.Data == done
				return event, nil
			}
			return sseEvent{}, io.ErrUnexpectedEOF
		}
	}
}

// probeResponsesEventType 对 well-formed JSON 按需探测顶层 type 字段，避免整帧 typed 解码。
// 仅当 payload 是合法 JSON 对象、顶层 type 存在且为非空字符串时返回 (type, true)；
// 空 payload、[DONE]、畸形 JSON、非对象根、type 缺失/null/非字符串/空串一律返回 ("", false)。
//
// 键匹配语义对齐改造前的消费方（json.Unmarshal 到 struct{Type string `json:"type"`}）：
// encoding/json 对字段名先精确匹配、再按 Unicode 简单折叠（foldName）匹配，故 `Type`/`TYPE`/
// `tYpE` 等大小写变体、以及转义拼写（`{"\u0054YPE":"x"}` 解码为 "TYPE"）都会命中。
// 本函数用 strings.EqualFold（与 encoding/json 的 foldName 同为 Unicode 简单折叠）复现该语义。
//
// 值选取与 encoding/json 对齐：大小写变体之间按文档序 last-wins（`{"type":"a","TYPE":"b"}` → "b"）；
// 显式 null 对 string 字段是无操作，不覆盖前值（`{"type":"a","TYPE":null}` → "a"）。
//
// 类型校验覆盖所有匹配键（含被 first-wins 跳过的重复键）：任一匹配键非 string/null 即整体
// 失败，对齐 encoding/json「任一匹配字段不可解析 → 整体 err」（`{"type":"a","type":123}` → ok=false）。
// 显式 null 是无操作：既不产出值，也不占用 first-wins 键位（`{"type":null,"type":"b"}` → "b"）。
//
// 与 encoding/json 的已知差异（契约 AC-9 声明的 sanctioned exception）：值选取上，字节完全相同的
// 重复键取「第一个」（first-wins），encoding/json 取「最后一个」（last-wins）；转义与字面拼写解码后
// 相同的键也按重复处理（`{"type":"a","\u0074ype":"b"}` → "a"）。该差异由 TestProbeResponsesEventType 锁定。
// 类型校验不受 first-wins 影响：所有匹配键都必须通过校验（见上）。
func probeResponsesEventType(payload []byte) (eventType string, ok bool) {
	if !json.Valid(payload) {
		return "", false
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() {
		return "", false
	}
	valid := true
	winningKey := ""
	root.ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(key.Str, "type") {
			return true
		}
		// 类型校验覆盖所有匹配键（含被 first-wins 跳过的重复键）：任一匹配键非 string/null
		// 即整体失败，精确复刻旧 typed 解码「任一匹配字段不可解析 → 整体 err」的语义
		// （契约 4.6 Step 3：non-string input 永不覆盖 eventType）。
		if value.Type != gjson.String && value.Type != gjson.Null {
			valid = false
			return false
		}
		// 显式 null 对 string 字段是无操作：既不产出值，也不占用 first-wins 的键位
		// （`{"type":null,"type":"b"}` 与旧实现一致得 "b"）。
		if value.Type == gjson.Null {
			return true
		}
		// 字节完全相同的重复键：first-wins，跳过后续同名字节键（契约 AC-9 sanctioned 差异）。
		if winningKey != "" && winningKey == key.Str {
			return true
		}
		winningKey = key.Str
		eventType = value.Str
		return true
	})
	if !valid || eventType == "" {
		return "", false
	}
	return eventType, true
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
		if probed, ok := probeResponsesEventType([]byte(payload)); ok {
			eventType = probed
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

	// probe 与完整解码的双重门禁：probe 对字节完全相同的重复键取 first-wins、对大小写变体取
	// last-wins（对齐旧 typed 探测），完整解码（typed）一律 last-wins。两者同时保留使新行为
	// 是旧行为的子集：仅当两道路径都判定为 response.failed 时才产出 capture。
	// 例如 {"type":"response.created","type":"response.failed"} 旧实现产出 capture，
	// 新实现被 probe 门禁拒绝返回 (nil, false)，属契约 AC-9 sanctioned 差异。
	if probed, ok := probeResponsesEventType([]byte(payload)); !ok || probed != "response.failed" {
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
	// 日志 ResponseBody 只存聚合后的完整 response（供日志列表查看返回数据），
	// 不再写入 frames 原始帧。frames 捕获逻辑保留，排查 SSE 流问题时打 debug 摘要。
	// 外层保留 {"response":...} 包装，与历史日志结构一致（仅少了 frames 字段）。
	if capture.Response != nil {
		if respJSON, err := json.Marshal(map[string]any{"response": capture.Response}); err == nil {
			c.Set(ctxkey.ResponseBody, string(respJSON))
		}
		logger.Log.Debugf("[finalizeStreamCapture] response %s stored, frames=%d", capture.Response.ID, len(capture.Frames))
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

// streamFailureError 把 SSE 已提交后的流失败映射为 502 upstream_error（与 converted 路径
// controller.responsesStreamFailureError 同口径，跨包不便复用故按同风格在本包构造）。
// 失败已由已写出的 SSE 事件表达，这里只回传渠道失败记账所需的错误对象；调用方不得据此
// 再向客户端追加任何内容（框架层 Written() 守卫会抑制重试与 JSON 渲染）。
// 复用既有错误码/类型（如 stream_read_error/bad_response），仅在缺失时按通用信息兜底。
func streamFailureError(errInfo model.Error) *model.ErrorWithStatusCode {
	if errInfo.Message == "" {
		errInfo.Message = "responses stream failed after SSE headers committed"
	}
	if errInfo.Type == "" {
		errInfo.Type = "upstream_error"
	}
	if errInfo.Code == nil || errInfo.Code == "" {
		errInfo.Code = "invalid_upstream_response"
	}
	return &model.ErrorWithStatusCode{Error: errInfo, StatusCode: http.StatusBadGateway}
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

// extractResponsesUsage validates a Responses body and extracts billing usage; malformed JSON or
// invalid billing fields return an error, absent/null fields map to zero, and invalid details are
// omitted.
//
// 计费语义（spec: Billing-critical fields SHALL fail rather than degrade）：三个 basis
// （input_tokens/output_tokens/total_tokens）严格失败——不可解析即返回 error，保留旧
// `invalid_json_response` 失败路径；details（input_tokens_details/output_tokens_details 及其子字段）
// 宽容降级为缺失/零值，不失败。`null` 一律映射为零值。
//
// Claude 扩展字段（cache_creation_* / cache_read_input_tokens / cache_ttl）不参与计费，按 details
// 口径宽容提取，以便与旧整体解码在正常样本上逐字段一致。
//
// 重复键差异（契约 AC-9 sanctioned exception）：本路径经 gjson 路径取值，字节完全相同的重复键取
// 「第一个」（first-wins）；旧 `encoding/json` typed 解码取「最后一个」（last-wins）。由
// TestExtractResponsesUsageEquivalence 的 duplicate 用例锁定。
func extractResponsesUsage(responseBody []byte) (usage model.ResponsesUsage, err error) {
	if !json.Valid(responseBody) {
		return model.ResponsesUsage{}, fmt.Errorf("responses body is not valid JSON")
	}
	root := gjson.ParseBytes(responseBody)
	if root.Type == gjson.Null {
		// 旧 typed 解码 `json.Unmarshal("null", &ResponsesResponse)` 成功且全部为零值。
		return model.ResponsesUsage{}, nil
	}
	if !root.IsObject() {
		// 数组/字符串/数字/布尔根：旧 typed 解码失败（非对象根无法解码为 struct）→ 保留
		// `invalid_json_response` 失败路径。
		return model.ResponsesUsage{}, fmt.Errorf("responses root must be an object, got %s", root.Type)
	}
	usageResult := root.Get("usage")
	if !usageResult.Exists() || usageResult.Type == gjson.Null {
		return model.ResponsesUsage{}, nil
	}
	if !usageResult.IsObject() {
		return model.ResponsesUsage{}, fmt.Errorf("usage must be an object, got %s", usageResult.Type)
	}
	var ok bool
	if usage.InputTokens, ok = codexStrictInt(usageResult.Get("input_tokens")); !ok {
		return model.ResponsesUsage{}, fmt.Errorf("usage.input_tokens is not a valid integer")
	}
	if usage.OutputTokens, ok = codexStrictInt(usageResult.Get("output_tokens")); !ok {
		return model.ResponsesUsage{}, fmt.Errorf("usage.output_tokens is not a valid integer")
	}
	if usage.TotalTokens, ok = codexStrictInt(usageResult.Get("total_tokens")); !ok {
		return model.ResponsesUsage{}, fmt.Errorf("usage.total_tokens is not a valid integer")
	}
	if value, detailOK := codexStrictInt(usageResult.Get("cache_creation_input_tokens")); detailOK {
		usage.CacheCreationInputTokens = value
	}
	if value, detailOK := codexStrictInt(usageResult.Get("cache_creation_5m_input_tokens")); detailOK {
		usage.CacheCreation5mInputTokens = value
	}
	if value, detailOK := codexStrictInt(usageResult.Get("cache_creation_1h_input_tokens")); detailOK {
		usage.CacheCreation1hInputTokens = value
	}
	if value, detailOK := codexStrictInt(usageResult.Get("cache_read_input_tokens")); detailOK {
		usage.CacheReadInputTokens = value
	}
	if value := usageResult.Get("cache_ttl"); value.Type == gjson.String {
		usage.CacheTTL = value.Str
	}
	usage.InputTokensDetails = codexTolerantInputDetails(usageResult.Get("input_tokens_details"))
	usage.OutputTokensDetails = codexTolerantOutputDetails(usageResult.Get("output_tokens_details"))
	return usage, nil
}

// codexStrictInt 复刻旧 typed 解码对 Go int 字段的语义：缺失/null → 0 且合法；其余必须是精确的
// 十进制 int64（ParseInt 拒绝小数、指数形式与 int64 溢出），否则 ok=false。
// 平台假设：仅在 64 位平台（int == int64）下与旧 typed int 语义一致。
func codexStrictInt(result gjson.Result) (int, bool) {
	if !result.Exists() || result.Type == gjson.Null {
		return 0, true
	}
	if result.Type != gjson.Number {
		return 0, false
	}
	value, err := strconv.ParseInt(result.Raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(value), true
}

// codexTolerantInputDetails：非对象/null/缺失 → nil；对象内任一 int 子字段不可解析 → 该字段零值。
func codexTolerantInputDetails(result gjson.Result) *model.InputTokensDetails {
	if !result.IsObject() {
		return nil
	}
	details := &model.InputTokensDetails{}
	if value, ok := codexStrictInt(result.Get("cached_tokens")); ok {
		details.CachedTokens = value
	}
	if value, ok := codexStrictInt(result.Get("cache_write_tokens")); ok {
		details.CacheWriteTokens = value
	}
	return details
}

// codexTolerantOutputDetails：与 codexTolerantInputDetails 同构，对应 output_tokens_details。
func codexTolerantOutputDetails(result gjson.Result) *model.OutputTokensDetails {
	if !result.IsObject() {
		return nil
	}
	details := &model.OutputTokensDetails{}
	if value, ok := codexStrictInt(result.Get("reasoning_tokens")); ok {
		details.ReasoningTokens = value
	}
	if value, ok := codexStrictInt(result.Get("accepted_prediction_tokens")); ok {
		details.AcceptedPredictionTokens = value
	}
	if value, ok := codexStrictInt(result.Get("rejected_prediction_tokens")); ok {
		details.RejectedPredictionTokens = value
	}
	if value, ok := codexStrictInt(result.Get("audio_tokens")); ok {
		details.AudioTokens = value
	}
	if value, ok := codexStrictInt(result.Get("text_tokens")); ok {
		details.TextTokens = value
	}
	return details
}

// responsesUsageToInternalUsage 把 Responses wire usage（§6）映射为网关内部 Usage（Chat §8 details 字段名）。
// 计费公式仍只看总数与 cached_tokens：cache_write/reasoning 等只进入 detail 承载，供日志与后续策略使用。
func responsesUsageToInternalUsage(source *model.ResponsesUsage) *model.Usage {
	if source == nil {
		return nil
	}
	usage := &model.Usage{
		PromptTokens:     source.InputTokens,
		CompletionTokens: source.OutputTokens,
		TotalTokens:      source.TotalTokens,
	}
	if d := source.InputTokensDetails; d != nil && (d.CachedTokens != 0 || d.CacheWriteTokens != 0) {
		usage.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens:     d.CachedTokens,
			CacheWriteTokens: d.CacheWriteTokens,
		}
	}
	if d := source.OutputTokensDetails; d != nil && (d.ReasoningTokens != 0 || d.AcceptedPredictionTokens != 0 || d.RejectedPredictionTokens != 0 || d.AudioTokens != 0 || d.TextTokens != 0) {
		usage.CompletionTokensDetails = &model.CompletionTokensDetails{
			ReasoningTokens:          d.ReasoningTokens,
			AcceptedPredictionTokens: d.AcceptedPredictionTokens,
			RejectedPredictionTokens: d.RejectedPredictionTokens,
			AudioTokens:              d.AudioTokens,
			TextTokens:               d.TextTokens,
		}
	}
	return usage
}

func setUsageFromResponsesUsage(target **model.Usage, source *model.ResponsesUsage) {
	if source == nil {
		return
	}
	*target = responsesUsageToInternalUsage(source)
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
	currentFrame           **model.ResponsesStreamFrame
	deltaFrame             **model.ResponsesStreamFrame
	deltaText              *strings.Builder
	capture                *model.ResponsesStreamCapture
	usage                  **model.Usage
	outputItems            *[]model.ResponsesItem
	outputItemByID         *map[string]int
	skippedItemIDs         *map[string]struct{}
	doneRendered           *bool
	responseText           *string
	streamError            *model.Error
	sawFailedTerminal      *bool
	sawCompletedTerminal   *bool
	sentSyntheticCompleted *bool
	sawDone                *bool
	lastEventType          *string
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
		if probed, ok := probeResponsesEventType([]byte(payload)); ok {
			eventType = probed
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

	// response.incomplete 是合法成功的终止状态（length/content_filter 截断，非失败）：
	// 复用 completed 的终态哨兵以停止等待、抑制迟到事件与合成 completed；
	// 与 completed 的区分保留在事件 payload 及 capture 快照的 response.status 上
	if eventType == "response.completed" || eventType == "response.incomplete" {
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

	if (eventType == "response.completed" || eventType == "response.incomplete") && streamResponse.Response != nil {
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
