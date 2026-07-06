package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/pai801/myapi/common/render"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/conv"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

const (
	dataPrefix       = "data: "
	done             = "[DONE]"
	dataPrefixLength = len(dataPrefix)
)

func StreamHandler(c *gin.Context, resp *http.Response, relayMode int) (*model.ErrorWithStatusCode, string, *model.Usage, string) {
	responseText := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, constant.ScannerBufferInitial), constant.ScannerBufferMax)
	scanner.Split(bufio.ScanLines)
	var usage *model.Usage
	var chatAccumulator *chatStreamAccumulator
	if relayMode == relaymode.ChatCompletions {
		chatAccumulator = newChatStreamAccumulator()
	}

	common.SetEventStreamHeaders(c)

	doneRendered := false
	for scanner.Scan() {
		data := scanner.Text()
		if len(data) < dataPrefixLength { // ignore blank line or wrong format
			continue
		}
		if data[:dataPrefixLength] != dataPrefix && data[:dataPrefixLength] != done {
			continue
		}
		if strings.HasPrefix(data[dataPrefixLength:], done) {
			render.StringData(c, data)
			doneRendered = true
			continue
		}
		switch relayMode {
		case relaymode.ChatCompletions:
			payload := data[dataPrefixLength:]
			var streamResponse ChatCompletionsStreamResponse
			err := json.Unmarshal([]byte(payload), &streamResponse)
			if err != nil {
				logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
				render.StringData(c, data) // if error happened, pass the data to client
				continue                   // just ignore the error
			}
			chatAccumulator.addPayload([]byte(payload))
			if len(streamResponse.Choices) == 0 && streamResponse.Usage == nil {
				// but for empty choice and no usage, we should not pass it to client, this is for azure
				continue // just ignore empty choice
			}
			render.StringData(c, data)
			for _, choice := range streamResponse.Choices {
				responseText += conv.AsString(choice.Delta.Content)
			}
			if streamResponse.Usage != nil {
				usage = streamResponse.Usage
			}
		case relaymode.Completions:
			render.StringData(c, data)
			var streamResponse CompletionsStreamResponse
			err := json.Unmarshal([]byte(data[dataPrefixLength:]), &streamResponse)
			if err != nil {
				logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
				continue
			}
			for _, choice := range streamResponse.Choices {
				responseText += choice.Text
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Log.Errorf("error reading stream: " + err.Error())
	}

	if !doneRendered {
		render.Done(c)
	}

	err := resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), "", nil, ""
	}

	responseBody := ""
	if chatAccumulator != nil {
		responseBody = chatAccumulator.buildResponseBody()
	}
	return nil, responseText, usage, responseBody
}

type chatStreamAccumulator struct {
	id                string
	object            string
	created           int64
	model             string
	systemFingerprint string
	usage             map[string]any
	choices           map[int]*chatStreamChoiceAccumulator
}

type chatStreamChoiceAccumulator struct {
	index        int
	role         string
	content      string
	finishReason any
	toolCalls    map[int]*chatStreamToolCallAccumulator
	functionCall *chatStreamFunctionCallAccumulator
}

type chatStreamToolCallAccumulator struct {
	index     int
	id        string
	typeValue string
	function  chatStreamFunctionCallAccumulator
}

type chatStreamFunctionCallAccumulator struct {
	name      string
	arguments string
}

func newChatStreamAccumulator() *chatStreamAccumulator {
	return &chatStreamAccumulator{choices: make(map[int]*chatStreamChoiceAccumulator)}
}

func (a *chatStreamAccumulator) addPayload(payload []byte) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return
	}
	a.captureTopLevel(raw)
	if usage, ok := raw["usage"].(map[string]any); ok {
		a.usage = usage
	}
	choices, ok := raw["choices"].([]any)
	if !ok {
		return
	}
	for _, choiceValue := range choices {
		choiceMap, ok := choiceValue.(map[string]any)
		if !ok {
			continue
		}
		index := intFromAny(choiceMap["index"])
		choice := a.choice(index)
		if finishReason, exists := choiceMap["finish_reason"]; exists && finishReason != nil {
			choice.finishReason = finishReason
		}
		delta, ok := choiceMap["delta"].(map[string]any)
		if !ok {
			continue
		}
		choice.addDelta(delta)
	}
}

func (a *chatStreamAccumulator) captureTopLevel(raw map[string]any) {
	if value, ok := raw["id"].(string); ok && value != "" {
		a.id = value
	}
	if value, ok := raw["object"].(string); ok && value != "" {
		a.object = value
	}
	if value := int64FromAny(raw["created"]); value != 0 {
		a.created = value
	}
	if value, ok := raw["model"].(string); ok && value != "" {
		a.model = value
	}
	if value, ok := raw["system_fingerprint"].(string); ok && value != "" {
		a.systemFingerprint = value
	}
}

func (a *chatStreamAccumulator) choice(index int) *chatStreamChoiceAccumulator {
	choice, ok := a.choices[index]
	if !ok {
		choice = &chatStreamChoiceAccumulator{index: index, toolCalls: make(map[int]*chatStreamToolCallAccumulator)}
		a.choices[index] = choice
	}
	return choice
}

func (c *chatStreamChoiceAccumulator) addDelta(delta map[string]any) {
	if roleValue, ok := delta["role"].(string); ok && roleValue != "" {
		c.role = roleValue
	}
	if contentValue, ok := delta["content"].(string); ok {
		c.content += contentValue
	}
	if functionCall, ok := delta["function_call"].(map[string]any); ok {
		if c.functionCall == nil {
			c.functionCall = &chatStreamFunctionCallAccumulator{}
		}
		c.functionCall.add(functionCall)
	}
	toolCalls, ok := delta["tool_calls"].([]any)
	if !ok {
		return
	}
	for _, toolCallValue := range toolCalls {
		toolCallMap, ok := toolCallValue.(map[string]any)
		if !ok {
			continue
		}
		index := intFromAny(toolCallMap["index"])
		toolCall, ok := c.toolCalls[index]
		if !ok {
			toolCall = &chatStreamToolCallAccumulator{index: index}
			c.toolCalls[index] = toolCall
		}
		toolCall.add(toolCallMap)
	}
}

func (t *chatStreamToolCallAccumulator) add(raw map[string]any) {
	if value, ok := raw["id"].(string); ok && value != "" {
		t.id = value
	}
	if value, ok := raw["type"].(string); ok && value != "" {
		t.typeValue = value
	}
	if function, ok := raw["function"].(map[string]any); ok {
		t.function.add(function)
	}
}

func (f *chatStreamFunctionCallAccumulator) add(raw map[string]any) {
	if value, ok := raw["name"].(string); ok && value != "" {
		f.name = value
	}
	if value, ok := raw["arguments"].(string); ok {
		f.arguments += value
	}
}

func (a *chatStreamAccumulator) buildResponseBody() string {
	if len(a.choices) == 0 && a.usage == nil {
		return ""
	}
	object := a.object
	if object == "chat.completion.chunk" {
		object = "chat.completion"
	}
	response := map[string]any{
		"id":      a.id,
		"object":  object,
		"created": a.created,
		"model":   a.model,
		"choices": a.buildChoices(),
	}
	if a.systemFingerprint != "" {
		response["system_fingerprint"] = a.systemFingerprint
	}
	if a.usage != nil {
		response["usage"] = a.usage
	}
	data, err := json.Marshal(response)
	if err != nil {
		logger.Log.Errorf("chat stream response body marshal failed: " + err.Error())
		return ""
	}
	return string(data)
}

func (a *chatStreamAccumulator) buildChoices() []map[string]any {
	indexes := make([]int, 0, len(a.choices))
	for index := range a.choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	choices := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		choice := a.choices[index]
		message := map[string]any{}
		if choice.role != "" {
			message["role"] = choice.role
		}
		message["content"] = choice.content
		if len(choice.toolCalls) > 0 {
			message["tool_calls"] = choice.buildToolCalls()
		}
		if choice.functionCall != nil {
			message["function_call"] = choice.functionCall.asMap()
		}
		choices = append(choices, map[string]any{
			"index":         choice.index,
			"message":       message,
			"finish_reason": choice.finishReason,
		})
	}
	return choices
}

func (c *chatStreamChoiceAccumulator) buildToolCalls() []map[string]any {
	indexes := make([]int, 0, len(c.toolCalls))
	for index := range c.toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	toolCalls := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		toolCall := c.toolCalls[index]
		toolCallMap := map[string]any{
			"index":    toolCall.index,
			"function": toolCall.function.asMap(),
		}
		if toolCall.id != "" {
			toolCallMap["id"] = toolCall.id
		}
		if toolCall.typeValue != "" {
			toolCallMap["type"] = toolCall.typeValue
		}
		toolCalls = append(toolCalls, toolCallMap)
	}
	return toolCalls
}

func (f *chatStreamFunctionCallAccumulator) asMap() map[string]any {
	function := map[string]any{}
	if f.name != "" {
		function["name"] = f.name
	}
	function["arguments"] = f.arguments
	return function
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return 0
	}
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

func buildStreamResponseBody(responseText string, usage *model.Usage, modelName string) string {
	type streamChoice struct {
		Index        int           `json:"index"`
		Message      model.Message `json:"message"`
		FinishReason string        `json:"finish_reason"`
	}
	type streamResponse struct {
		Id      string         `json:"id"`
		Object  string         `json:"object"`
		Created int64          `json:"created"`
		Model   string         `json:"model"`
		Choices []streamChoice `json:"choices"`
		Usage   *model.Usage   `json:"usage"`
	}
	resp := streamResponse{
		Id:      "chatcmpl-xxx",
		Object:  "chat.completion",
		Created: 1234567890,
		Model:   modelName,
		Choices: []streamChoice{
			{
				Index: 0,
				Message: model.Message{
					Role:    "assistant",
					Content: responseText,
				},
				FinishReason: "stop",
			},
		},
		Usage: usage,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		logger.Log.Errorf("buildStreamResponseBody marshal failed: " + err.Error())
		return ""
	}
	return string(data)
}

func ensureStreamResponseBodyUsage(responseBody string, usage *model.Usage) string {
	if usage == nil {
		return responseBody
	}
	var response map[string]any
	if err := json.Unmarshal([]byte(responseBody), &response); err != nil {
		logger.Log.Errorf("ensureStreamResponseBodyUsage unmarshal failed: " + err.Error())
		return responseBody
	}
	if _, ok := response["usage"]; ok {
		return responseBody
	}
	response["usage"] = usage
	data, err := json.Marshal(response)
	if err != nil {
		logger.Log.Errorf("ensureStreamResponseBodyUsage marshal failed: " + err.Error())
		return responseBody
	}
	return string(data)
}

func Handler(c *gin.Context, resp *http.Response, promptTokens int, modelName string) (*model.ErrorWithStatusCode, *model.Usage) {
	var textResponse SlimTextResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	err = json.Unmarshal(responseBody, &textResponse)
	if err != nil {
		return ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	if textResponse.Error.Type != "" {
		return &model.ErrorWithStatusCode{
			Error:      textResponse.Error,
			StatusCode: resp.StatusCode,
		}, nil
	}
	// Reset response body
	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))

	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the HTTPClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return ErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}

	if textResponse.Usage.TotalTokens == 0 || (textResponse.Usage.PromptTokens == 0 && textResponse.Usage.CompletionTokens == 0) {
		completionTokens := 0
		for _, choice := range textResponse.Choices {
			completionTokens += CountTokenText(choice.Message.StringContent(), modelName)
		}
		textResponse.Usage = model.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	c.Set(ctxkey.ResponseBody, string(responseBody))
	return nil, &textResponse.Usage
}
