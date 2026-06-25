package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/render"
	"github.com/songquanpeng/one-api/relay/meta"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/model"
)

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

	c.Set(ctxkey.ResponseBody, string(responseBody))
	return usage, nil
}

func StreamResponsesHandler(c *gin.Context, resp *http.Response) (*model.ErrorWithStatusCode, string, *model.Usage) {
	responseText := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, constant.ScannerBufferInitial), constant.ScannerBufferMax)
	scanner.Split(bufio.ScanLines)
	var usage *model.Usage
	capture := model.ResponsesStreamCapture{}
	var currentFrame *model.ResponsesStreamFrame
	var deltaText strings.Builder
	var deltaFrame *model.ResponsesStreamFrame
	var pendingEvent string
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

	// 预读首帧：检测 response.failed 事件，若失败则直接返回错误让网关触发重试
	var prereadLines []string
	firstEventIsFailed := false
	var failedStatusCode int
	var failedErr *model.Error
	{
		eventName := ""
		dataPayload := ""
		eventComplete := false
		for scanner.Scan() {
			line := scanner.Text()
			prereadLines = append(prereadLines, line)
			if line == "" {
				if eventName != "" || dataPayload != "" {
					eventComplete = true
					break
				}
				continue
			}
			if strings.HasPrefix(line, eventPrefix) {
				eventName = strings.TrimSpace(line[eventPrefixLength:])
				continue
			}
			if strings.HasPrefix(line, dataPrefix) {
				dataPayload = line[dataPrefixLength:]
				continue
			}
		}
		// 如果读到了 event 和 data，但还没遇到空行就到流末尾了，也认为事件完成
		if !eventComplete && (eventName != "" || dataPayload != "") {
			eventComplete = true
		}
		if eventComplete && eventName == "response.failed" && dataPayload != "" {
			var streamResp model.ResponsesStreamEvent
			if err := json.Unmarshal([]byte(dataPayload), &streamResp); err != nil {
				logger.Log.Debugf("preread: failed to parse response.failed event: %v", err)
			} else if streamResp.Response == nil || streamResp.Response.Status != "failed" {
				logger.Log.Debugf("preread: response.failed event with unexpected structure")
			} else {
				firstEventIsFailed = true
				resp := streamResp.Response
				errMsg := "upstream response failed"
				errType := "upstream_error"
				errCode := "response_failed"
				if resp.Error != nil {
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
				failedErr = &model.Error{
					Message: errMsg,
					Type:    errType,
					Code:    errCode,
				}
				// 根据错误类型映射 HTTP 状态码，便于网关判断是否重试
				failedStatusCode = mapFailedErrorToStatusCode(errCode, errType, errMsg)
			}
		}
	}
	if firstEventIsFailed && failedErr != nil {
		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body on error path: %v", err)
		}
		return &model.ErrorWithStatusCode{
			Error:      *failedErr,
			StatusCode: failedStatusCode,
		}, "", nil
	}

	// 首帧不是失败事件，开始正常流式转发
	common.SetEventStreamHeaders(c)

	doneRendered := false

	// 先把预读的行回放出去，并走一遍状态机
	for _, line := range prereadLines {
		processStreamLine(line, &pendingEvent, &currentFrame, &deltaFrame, &deltaText, &capture, &usage, &outputItems, &outputItemByID, &skippedItemIDs, flushFrame, flushDeltaFrame, c, &doneRendered, &responseText)
	}

	for scanner.Scan() {
		line := scanner.Text()
		processStreamLine(line, &pendingEvent, &currentFrame, &deltaFrame, &deltaText, &capture, &usage, &outputItems, &outputItemByID, &skippedItemIDs, flushFrame, flushDeltaFrame, c, &doneRendered, &responseText)
	}

	if err := scanner.Err(); err != nil {
		logger.Log.Errorf("error reading stream: " + err.Error())
	}

	if !doneRendered {
		render.Done(c)
	}

	flushFrame()
	flushDeltaFrame()
	if len(capture.Frames) > 0 || capture.Response != nil {
		if capture.Response != nil {
			if capture.Usage == nil {
				capture.Usage = &capture.Response.Usage
			}
			if capture.Usage != nil {
				capture.Response.Usage = *capture.Usage
			}
		}
		if respJSON, err := json.Marshal(capture); err == nil {
			c.Set(ctxkey.ResponseBody, string(respJSON))
		}
	}

	err := resp.Body.Close()
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "close_response_body_failed", err)
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), "", nil
	}

	return nil, responseText, usage
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
		strings.Contains(typeLower, "server_error") ||
		strings.Contains(typeLower, "unavailable") ||
		strings.Contains(typeLower, "internal_error") ||
		strings.Contains(msgLower, "internal server error") ||
		strings.Contains(msgLower, "service unavailable") ||
		strings.Contains(msgLower, "bad gateway") ||
		strings.Contains(msgLower, "request timeout") ||
		strings.Contains(msgLower, "timed out") ||
		strings.Contains(msgLower, "deadline exceeded") ||
		strings.Contains(msgLower, "connection timeout") {
		return http.StatusBadGateway
	}

	// 4xx - 客户端错误，默认不重试
	return http.StatusBadRequest
}

// processStreamLine 处理单行 SSE 数据，抽取为函数以支持预读回放和正常循环复用
func processStreamLine(
	line string,
	pendingEvent *string,
	currentFrame **model.ResponsesStreamFrame,
	deltaFrame **model.ResponsesStreamFrame,
	deltaText *strings.Builder,
	capture *model.ResponsesStreamCapture,
	usage **model.Usage,
	outputItems *[]model.ResponsesItem,
	outputItemByID *map[string]int,
	skippedItemIDs *map[string]struct{},
	flushFrame func(),
	flushDeltaFrame func(),
	c *gin.Context,
	doneRendered *bool,
	responseText *string,
) {
	if line == "" {
		flushFrame()
		return
	}
	if strings.HasPrefix(line, eventPrefix) {
		flushFrame()
		*pendingEvent = strings.TrimSpace(line[eventPrefixLength:])
		return
	}
	if len(line) < dataPrefixLength || line[:dataPrefixLength] != dataPrefix {
		return
	}

	payload := line[dataPrefixLength:]
	if strings.HasPrefix(payload, done) {
		flushDeltaFrame()
		if *currentFrame == nil {
			*currentFrame = &model.ResponsesStreamFrame{Event: *pendingEvent}
		}
		if (*currentFrame).Event == "" {
			(*currentFrame).Event = *pendingEvent
		}
		(*currentFrame).Data = json.RawMessage(`"[DONE]"`)
		(*currentFrame).Done = true
		*pendingEvent = ""
		render.StringData(c, line)
		*doneRendered = true
		return
	}

	var streamResponse model.ResponsesStreamEvent
	err := json.Unmarshal([]byte(payload), &streamResponse)
	if err != nil {
		logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
		render.StringData(c, line)
		return
	}
	render.StringData(c, line)

	if streamResponse.Type == "response.output_text.delta" {
		if streamResponse.Delta != nil {
			if s, ok := streamResponse.Delta.(string); ok {
				deltaText.WriteString(s)
				*responseText += s
			}
		}
		if *deltaFrame == nil {
			*deltaFrame = &model.ResponsesStreamFrame{Event: streamResponse.Type}
		}
		*pendingEvent = ""
		return
	}
	if strings.HasSuffix(streamResponse.Type, ".delta") {
		*pendingEvent = ""
		return
	}

	flushDeltaFrame()

	if *currentFrame == nil {
		*currentFrame = &model.ResponsesStreamFrame{Event: *pendingEvent}
	}
	if (*currentFrame).Event == "" {
		(*currentFrame).Event = *pendingEvent
	}
	(*currentFrame).Data = json.RawMessage(payload)
	*pendingEvent = ""

	if streamResponse.Type == "response.output_item.added" || streamResponse.Type == "response.output_item.done" {
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
			(*skippedItemIDs)[itemID] = struct{}{}
			return
		}
		if _, skipped := (*skippedItemIDs)[itemID]; skipped {
			return
		}
		if streamResponse.Type == "response.output_item.added" {
			(*outputItemByID)[itemID] = len(*outputItems)
			*outputItems = append(*outputItems, *streamResponse.Item)
		} else if idx, ok := (*outputItemByID)[itemID]; ok && idx < len(*outputItems) {
			(*outputItems)[idx] = *streamResponse.Item
		} else {
			*outputItems = append(*outputItems, *streamResponse.Item)
		}
	}

	if streamResponse.Usage != nil {
		capture.Usage = streamResponse.Usage
		*usage = &model.Usage{
			PromptTokens:     streamResponse.Usage.InputTokens,
			CompletionTokens: streamResponse.Usage.OutputTokens,
			TotalTokens:      streamResponse.Usage.TotalTokens,
		}
	}

	if streamResponse.Response != nil && streamResponse.Response.Usage.TotalTokens > 0 {
		capture.Usage = &streamResponse.Response.Usage
		*usage = &model.Usage{
			PromptTokens:     streamResponse.Response.Usage.InputTokens,
			CompletionTokens: streamResponse.Response.Usage.OutputTokens,
			TotalTokens:      streamResponse.Response.Usage.TotalTokens,
		}
	}

	if streamResponse.Type == "response.completed" && streamResponse.Response != nil {
		resp := streamResponse.Response
		if len(resp.Output) == 0 && len(*outputItems) > 0 {
			resp.Output = *outputItems
		}
		capture.Response = resp
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
