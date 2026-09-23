package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/pai801/myapi/common/render"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
	"github.com/tidwall/gjson"
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
			result, err := chatAccumulator.addPayload([]byte(payload))
			if err != nil {
				logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
				render.StringData(c, data) // if error happened, pass the data to client
				continue                   // just ignore the error
			}
			if result.ChoiceCount == 0 && result.Usage == nil {
				// but for empty choice and no usage, we should not pass it to client, this is for azure
				continue // just ignore empty choice
			}
			render.StringData(c, data)
			responseText += result.ResponseText
			if result.Usage != nil {
				usage = result.Usage
			}
		case relaymode.Completions:
			// 转发必须先于解析：解析失败只影响文本累加，不影响已转发的字节。
			render.StringData(c, data)
			text, err := extractCompletionsStreamText([]byte(data[dataPrefixLength:]))
			if err != nil {
				logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
				continue
			}
			responseText += text
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

// chatStreamPayloadResult is the consumer-visible observation from one valid chat stream frame.
type chatStreamPayloadResult struct {
	ResponseText string
	Usage        *model.Usage
	ChoiceCount  int
}

// addPayload validates and observes one chat frame while updating the accumulator once; malformed or contract-invalid frames return an error without producing derived values.
func (a *chatStreamAccumulator) addPayload(payload []byte) (result chatStreamPayloadResult, err error) {
	if !json.Valid(payload) {
		return chatStreamPayloadResult{}, fmt.Errorf("chat stream frame is not valid JSON")
	}
	root := gjson.ParseBytes(payload)
	if root.Type == gjson.Null {
		// data: null 维持现状：不转发、不累积，且不产出任何派生值。
		return chatStreamPayloadResult{}, nil
	}
	if !root.IsObject() {
		return chatStreamPayloadResult{}, fmt.Errorf("chat stream frame root must be an object, got %s", root.Type)
	}
	frame, err := parseChatStreamFrame(root)
	if err != nil {
		return chatStreamPayloadResult{}, err
	}
	if hasUnrepresentableNumber(root) {
		// 旧实现的两步语义：typed 解码成功（文本与 usage 已提取）后，map 解码因超出
		// float64 范围的数字（如 1e400）失败，该帧「不累积但保留派生值」。此处仅跳过
		// 累积，仍返回 frame.result()，以保持 StreamHandler 的文本/usage 与改造前一致。
		return frame.result(), nil
	}
	// 原子性：完整校验（并完成派生值提取）通过后，才一次性写入累积器。
	a.applyFrame(frame)
	return frame.result(), nil
}

// extractCompletionsStreamText validates one completions frame and returns concatenated
// choice text in document order; missing choices return an empty string, while malformed
// JSON or a type-invalid choices/choices[].text value returns an error so the caller
// keeps its existing skip-and-forward behavior.
//
// 类型化字段严格（对齐旧 typed 解码的 `json.Unmarshal` 到 CompletionsStreamResponse）：
// `choices` 必须是数组，`choices[].text` / `choices[].finish_reason` 存在时必须为 string。
//
// 键匹配为 exact-then-fold 大小写不敏感，与 `encoding/json` struct 解码对齐；重复键 first-wins
// 为 sanctioned 差异（详见 foldMatch）。库间差异：`encoding/json` 把字符串中的非法 UTF-8 净化为
// U+FFFD，而 `gjson` 保留原始字节，此处不做修正。
func extractCompletionsStreamText(payload []byte) (responseText string, err error) {
	if !json.Valid(payload) {
		return "", fmt.Errorf("completions stream frame is not valid JSON")
	}
	root := gjson.ParseBytes(payload)
	if root.Type == gjson.Null {
		// data: null 维持旧 typed 行为：Unmarshal 成功且 Choices 为 nil，累加空串。
		return "", nil
	}
	if !root.IsObject() {
		return "", fmt.Errorf("completions stream frame root must be an object, got %s", root.Type)
	}
	choices, hasChoices, err := foldArrayValidated(root, "choices", validateCompletionsChoices)
	if err != nil {
		return "", fmt.Errorf("choices: %w", err)
	}
	if !hasChoices {
		return "", nil
	}
	var sb strings.Builder
	for _, element := range choices.Array() {
		if element.Type == gjson.Null {
			// typed 切片元素为 null 时该元素解出零值 struct（text 为空串），贡献空串。
			continue
		}
		if !element.IsObject() {
			return "", fmt.Errorf("choices element: expected object or null, got %s", element.Type)
		}
		text, textErr := foldString(element, "text")
		if textErr != nil {
			return "", fmt.Errorf("choices.text: %w", textErr)
		}
		// finish_reason 为 typed string：仅校验（含大小写变体与重复键），不参与文本累加。
		if _, finishErr := foldString(element, "finish_reason"); finishErr != nil {
			return "", fmt.Errorf("choices.finish_reason: %w", finishErr)
		}
		sb.WriteString(text)
	}
	return sb.String(), nil
}

// chatStreamFrame 承载单帧「校验 + 提取」的中间结果；校验失败时不得触碰累积器。
type chatStreamFrame struct {
	id                string
	object            string
	created           int64
	model             string
	systemFingerprint string
	usageMap          map[string]any
	usage             *model.Usage
	choices           []chatStreamFrameChoice
	choiceCount       int
	responseText      string
}

type chatStreamFrameChoice struct {
	index        int
	finishReason any
	hasFinish    bool
	delta        *chatStreamFrameDelta
}

type chatStreamFrameDelta struct {
	role            string
	content         string
	hasFunctionCall bool
	functionName    string
	functionArgs    string
	toolCalls       []chatStreamFrameToolCall
}

type chatStreamFrameToolCall struct {
	index    int
	id       string
	typeName string
	function chatStreamFrameFunction
}

type chatStreamFrameFunction struct {
	name      string
	arguments string
}

func (frame *chatStreamFrame) result() chatStreamPayloadResult {
	return chatStreamPayloadResult{
		ResponseText: frame.responseText,
		Usage:        frame.usage,
		ChoiceCount:  frame.choiceCount,
	}
}

// parseChatStreamFrame 单次扫描提取顶层元数据与 choices。类型化字段严格（存在且类型不符即报错），
// 非类型化字段沿用累积器的既有宽松 map 语义，使转发决策与累积状态在同一帧上保持一致。
//
// 所有帧字段的键匹配均为 exact-then-fold 大小写不敏感，与 encoding/json struct 解码对齐；
// 重复键 first-wins 为 sanctioned 差异（详见 foldMatch）。
// 库间差异：encoding/json 把字符串中的非法 UTF-8 净化为 U+FFFD，gjson 保留原始字节，此处不做修正。
//
// 类型校验覆盖所有匹配键（含被 first-wins 跳过的重复键）：例如
// `{"usage":{"prompt_tokens":5,"prompt_tokens":1e400}}` 与 `{"id":"x","id":1e400}` 中，被跳过的
// 重复键类型不符即整帧失败（与旧 typed 解码 last-wins 的整帧失败结论一致）。该行为由
// TestChatStreamAccumulatorDuplicateKeysFirstWins 与 TestChatStreamCaseVariantCoexistenceAndDuplicates 锁定。
func parseChatStreamFrame(root gjson.Result) (*chatStreamFrame, error) {
	frame := &chatStreamFrame{}
	var err error
	if frame.id, err = foldString(root, "id"); err != nil {
		return nil, fmt.Errorf("id: %w", err)
	}
	if frame.object, err = foldString(root, "object"); err != nil {
		return nil, fmt.Errorf("object: %w", err)
	}
	if frame.created, err = foldInt64(root, "created"); err != nil {
		return nil, fmt.Errorf("created: %w", err)
	}
	if frame.model, err = foldString(root, "model"); err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	// system_fingerprint 非 typed 字段（旧 ChatCompletionsStreamResponse 未声明）：宽松——
	// 仅 JSON string 时捕获，其余类型忽略且不报错，与旧 map 路径 (raw["system_fingerprint"].(string)) 一致。
	frame.systemFingerprint = foldLooseString(root, "system_fingerprint")

	usageResult, hasUsage, usageErr := foldObjectValidated(root, "usage", validateChatStreamUsage)
	if usageErr != nil {
		return nil, fmt.Errorf("usage: %w", usageErr)
	}
	if hasUsage {
		usageMap, ok := usageResult.Value().(map[string]any)
		if !ok {
			return nil, fmt.Errorf("usage: expected object")
		}
		builtUsage, buildErr := buildChatStreamUsage(usageResult)
		if buildErr != nil {
			return nil, buildErr
		}
		frame.usageMap = usageMap
		frame.usage = builtUsage
	}

	choicesResult, hasChoices, choicesErr := foldArrayValidated(root, "choices", validateChatStreamChoices)
	if choicesErr != nil {
		return nil, fmt.Errorf("choices: %w", choicesErr)
	}
	if hasChoices {
		elements := choicesResult.Array()
		frame.choiceCount = len(elements)
		for _, element := range elements {
			if element.Type == gjson.Null {
				continue
			}
			if !element.IsObject() {
				return nil, fmt.Errorf("choices element: expected object or null, got %s", element.Type)
			}
			choice, choiceErr := parseChatStreamChoice(element)
			if choiceErr != nil {
				return nil, choiceErr
			}
			frame.choices = append(frame.choices, choice)
			if choice.delta != nil {
				frame.responseText += choice.delta.content
			}
		}
	}
	return frame, nil
}

func parseChatStreamChoice(element gjson.Result) (chatStreamFrameChoice, error) {
	choice := chatStreamFrameChoice{}
	var err error
	if choice.index, err = foldInt(element, "index"); err != nil {
		return choice, fmt.Errorf("choices.index: %w", err)
	}
	finishReason, hasFinish, finishErr := foldNullableString(element, "finish_reason")
	if finishErr != nil {
		return choice, fmt.Errorf("choices.finish_reason: %w", finishErr)
	}
	if hasFinish {
		choice.finishReason = finishReason
		choice.hasFinish = true
	}
	deltaResult, hasDelta, deltaErr := foldObjectValidated(element, "delta", validateChatStreamDelta)
	if deltaErr != nil {
		return choice, fmt.Errorf("choices.delta: %w", deltaErr)
	}
	if hasDelta {
		delta, parseErr := parseChatStreamDelta(deltaResult)
		if parseErr != nil {
			return choice, parseErr
		}
		choice.delta = delta
	}
	return choice, nil
}

func parseChatStreamDelta(deltaResult gjson.Result) (*chatStreamFrameDelta, error) {
	delta := &chatStreamFrameDelta{}
	var err error
	if delta.role, err = foldString(deltaResult, "role"); err != nil {
		return nil, fmt.Errorf("choices.delta.role: %w", err)
	}
	// refusal/name/tool_call_id 为 typed 字段（model.Message 中分别为 *string/*string/string），
	// 旧 typed 解码会校验类型；此处仅校验、不捕获（累积器不使用它们）。
	if _, err = foldString(deltaResult, "refusal"); err != nil {
		return nil, fmt.Errorf("choices.delta.refusal: %w", err)
	}
	if _, err = foldString(deltaResult, "name"); err != nil {
		return nil, fmt.Errorf("choices.delta.name: %w", err)
	}
	if _, err = foldString(deltaResult, "tool_call_id"); err != nil {
		return nil, fmt.Errorf("choices.delta.tool_call_id: %w", err)
	}
	// content 为 typed any 字段（model.Message.Content）：仅 String 参与累积与文本产出，
	// 其余类型贡献空串（对齐 conv.AsString 语义）；但含超出 float64 范围的数字时，
	// 旧 typed 解码到 any 会失败并整帧拒绝，故须显式报错以保持等价。
	content, _, contentErr := foldAny(deltaResult, "content")
	if contentErr != nil {
		return nil, fmt.Errorf("choices.delta.content: %w", contentErr)
	}
	if content.Type == gjson.String {
		delta.content = content.String()
	}
	// reasoning_content 同为 typed any 字段（model.Message.ReasoningContent），规则同上。
	if _, _, reasoningErr := foldAny(deltaResult, "reasoning_content"); reasoningErr != nil {
		return nil, fmt.Errorf("choices.delta.reasoning_content: %w", reasoningErr)
	}
	// function_call 为任意类型：非 Object 被忽略；legacy 字段走宽松 map 语义（非 String 忽略），
	// 与 tool_call.function 的严格 name 规则不同——后者在 typed Message 中受 string 约束。
	if functionCall, ok := foldLooseObject(deltaResult, "function_call"); ok {
		delta.hasFunctionCall = true
		delta.functionName = looseStringValue(foldLooseResult(functionCall, "name"))
		delta.functionArgs = looseStringValue(foldLooseResult(functionCall, "arguments"))
	}
	toolCalls, hasToolCalls, toolCallsErr := foldArrayValidated(deltaResult, "tool_calls", validateChatStreamToolCalls)
	if toolCallsErr != nil {
		return nil, fmt.Errorf("choices.delta.tool_calls: %w", toolCallsErr)
	}
	if hasToolCalls {
		for _, element := range toolCalls.Array() {
			if element.Type == gjson.Null {
				continue
			}
			if !element.IsObject() {
				return nil, fmt.Errorf("choices.delta.tool_calls element: expected object or null, got %s", element.Type)
			}
			toolCall, toolCallErr := parseChatStreamToolCall(element)
			if toolCallErr != nil {
				return nil, toolCallErr
			}
			delta.toolCalls = append(delta.toolCalls, toolCall)
		}
	}
	return delta, nil
}

func parseChatStreamToolCall(element gjson.Result) (chatStreamFrameToolCall, error) {
	// index 为非类型化字段：Number 截断为 int，非 Number 或非有限数取 0，不报错。
	toolCall := chatStreamFrameToolCall{index: foldLooseInt(element, "index")}
	var err error
	if toolCall.id, err = foldString(element, "id"); err != nil {
		return toolCall, fmt.Errorf("tool_calls.id: %w", err)
	}
	if toolCall.typeName, err = foldString(element, "type"); err != nil {
		return toolCall, fmt.Errorf("tool_calls.type: %w", err)
	}
	functionResult, hasFunction, functionErr := foldObjectValidated(element, "function", validateChatStreamFunction)
	if functionErr != nil {
		return toolCall, fmt.Errorf("tool_calls.function: %w", functionErr)
	}
	if hasFunction {
		if toolCall.function.name, err = foldString(functionResult, "name"); err != nil {
			return toolCall, fmt.Errorf("tool_calls.function.name: %w", err)
		}
		// description (string) 与 strict (*bool) 为 typed 字段（model.Function），仅校验不捕获。
		if _, err = foldString(functionResult, "description"); err != nil {
			return toolCall, fmt.Errorf("tool_calls.function.description: %w", err)
		}
		if err = foldBool(functionResult, "strict"); err != nil {
			return toolCall, fmt.Errorf("tool_calls.function.strict: %w", err)
		}
		// arguments / parameters 为 typed any 字段（model.Function.Arguments/Parameters）：
		// 仅 String 参与累积；含超出 float64 范围的数字时旧 typed 解码会整帧失败，须显式报错。
		arguments, _, argumentsErr := foldAny(functionResult, "arguments")
		if argumentsErr != nil {
			return toolCall, fmt.Errorf("tool_calls.function.arguments: %w", argumentsErr)
		}
		if _, _, parametersErr := foldAny(functionResult, "parameters"); parametersErr != nil {
			return toolCall, fmt.Errorf("tool_calls.function.parameters: %w", parametersErr)
		}
		toolCall.function.arguments = looseStringValue(arguments)
	}
	return toolCall, nil
}

// buildChatStreamUsage 构造返回给消费者的 *model.Usage。三个 basis 严格（精确 int64）；
// details 与旧 typed 路径一致：缺失/null 视为 nil 指针，非 Object 或已知 int 字段类型不符即报错。
// 说明：detail 类型不符时整帧拒绝，与旧 typed 路径一致，保留旧计费行为（不扣费）。
// 键匹配为 exact-then-fold 大小写不敏感，与 encoding/json struct 解码对齐；重复键 first-wins
// 为 sanctioned 差异（详见 foldMatch）。
// 非胜出的匹配键（大小写变体、被 first-wins 跳过的重复键）由 deepValidate 递归校验到目标类型
// （契约 4.2 Step 4）。
func buildChatStreamUsage(usageResult gjson.Result) (*model.Usage, error) {
	usage := &model.Usage{}
	var err error
	if usage.PromptTokens, err = foldInt(usageResult, "prompt_tokens"); err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens: %w", err)
	}
	if usage.CompletionTokens, err = foldInt(usageResult, "completion_tokens"); err != nil {
		return nil, fmt.Errorf("usage.completion_tokens: %w", err)
	}
	if usage.TotalTokens, err = foldInt(usageResult, "total_tokens"); err != nil {
		return nil, fmt.Errorf("usage.total_tokens: %w", err)
	}
	if usage.PromptTokensDetails, err = buildPromptTokensDetails(usageResult); err != nil {
		return nil, err
	}
	if usage.CompletionTokensDetails, err = buildCompletionTokensDetails(usageResult); err != nil {
		return nil, err
	}
	return usage, nil
}

// buildPromptTokensDetails 提取 *model.PromptTokensDetails：缺失/null 返回 nil；非胜出匹配键
// 递归校验到 *model.PromptTokensDetails。
func buildPromptTokensDetails(usageResult gjson.Result) (*model.PromptTokensDetails, error) {
	result, ok, err := foldObjectValidated(usageResult, "prompt_tokens_details", validatePromptTokensDetails)
	if err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens_details: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return parsePromptTokensDetails(result)
}

// parsePromptTokensDetails 把已确认的 JSON 对象解析为 *model.PromptTokensDetails。
func parsePromptTokensDetails(result gjson.Result) (*model.PromptTokensDetails, error) {
	details := &model.PromptTokensDetails{}
	var err error
	if details.CachedTokens, err = foldInt(result, "cached_tokens"); err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens_details.cached_tokens: %w", err)
	}
	if details.CacheWriteTokens, err = foldInt(result, "cache_write_tokens"); err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens_details.cache_write_tokens: %w", err)
	}
	if details.AudioTokens, err = foldInt(result, "audio_tokens"); err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens_details.audio_tokens: %w", err)
	}
	if details.TextTokens, err = foldInt(result, "text_tokens"); err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens_details.text_tokens: %w", err)
	}
	if details.ImageTokens, err = foldInt(result, "image_tokens"); err != nil {
		return nil, fmt.Errorf("usage.prompt_tokens_details.image_tokens: %w", err)
	}
	return details, nil
}

// buildCompletionTokensDetails 提取 *model.CompletionTokensDetails：缺失/null 返回 nil；非胜出
// 匹配键递归校验到 *model.CompletionTokensDetails。
func buildCompletionTokensDetails(usageResult gjson.Result) (*model.CompletionTokensDetails, error) {
	result, ok, err := foldObjectValidated(usageResult, "completion_tokens_details", validateCompletionTokensDetails)
	if err != nil {
		return nil, fmt.Errorf("usage.completion_tokens_details: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return parseCompletionTokensDetails(result)
}

// parseCompletionTokensDetails 把已确认的 JSON 对象解析为 *model.CompletionTokensDetails。
func parseCompletionTokensDetails(result gjson.Result) (*model.CompletionTokensDetails, error) {
	details := &model.CompletionTokensDetails{}
	var err error
	if details.ReasoningTokens, err = foldInt(result, "reasoning_tokens"); err != nil {
		return nil, fmt.Errorf("usage.completion_tokens_details.reasoning_tokens: %w", err)
	}
	if details.AcceptedPredictionTokens, err = foldInt(result, "accepted_prediction_tokens"); err != nil {
		return nil, fmt.Errorf("usage.completion_tokens_details.accepted_prediction_tokens: %w", err)
	}
	if details.RejectedPredictionTokens, err = foldInt(result, "rejected_prediction_tokens"); err != nil {
		return nil, fmt.Errorf("usage.completion_tokens_details.rejected_prediction_tokens: %w", err)
	}
	if details.AudioTokens, err = foldInt(result, "audio_tokens"); err != nil {
		return nil, fmt.Errorf("usage.completion_tokens_details.audio_tokens: %w", err)
	}
	if details.TextTokens, err = foldInt(result, "text_tokens"); err != nil {
		return nil, fmt.Errorf("usage.completion_tokens_details.text_tokens: %w", err)
	}
	return details, nil
}

// applyFrame 把已完整校验的帧一次性写入累积器；其字段语义与旧 map 实现逐条等价。
func (a *chatStreamAccumulator) applyFrame(frame *chatStreamFrame) {
	if frame.id != "" {
		a.id = frame.id
	}
	if frame.object != "" {
		a.object = frame.object
	}
	if frame.created != 0 {
		a.created = frame.created
	}
	if frame.model != "" {
		a.model = frame.model
	}
	if frame.systemFingerprint != "" {
		a.systemFingerprint = frame.systemFingerprint
	}
	if frame.usageMap != nil {
		a.usage = frame.usageMap
	}
	for _, choiceFrame := range frame.choices {
		choice := a.choice(choiceFrame.index)
		if choiceFrame.hasFinish {
			choice.finishReason = choiceFrame.finishReason
		}
		if choiceFrame.delta == nil {
			continue
		}
		choice.applyDelta(choiceFrame.delta)
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

func (c *chatStreamChoiceAccumulator) applyDelta(delta *chatStreamFrameDelta) {
	if delta.role != "" {
		c.role = delta.role
	}
	c.content += delta.content
	if delta.hasFunctionCall {
		if c.functionCall == nil {
			c.functionCall = &chatStreamFunctionCallAccumulator{}
		}
		if delta.functionName != "" {
			c.functionCall.name = delta.functionName
		}
		c.functionCall.arguments += delta.functionArgs
	}
	for _, toolCallFrame := range delta.toolCalls {
		toolCall, ok := c.toolCalls[toolCallFrame.index]
		if !ok {
			toolCall = &chatStreamToolCallAccumulator{index: toolCallFrame.index}
			c.toolCalls[toolCallFrame.index] = toolCall
		}
		if toolCallFrame.id != "" {
			toolCall.id = toolCallFrame.id
		}
		if toolCallFrame.typeName != "" {
			toolCall.typeValue = toolCallFrame.typeName
		}
		if toolCallFrame.function.name != "" {
			toolCall.function.name = toolCallFrame.function.name
		}
		toolCall.function.arguments += toolCallFrame.function.arguments
	}
}

// nullPolicy 描述「匹配键的值为显式 JSON null」时如何影响取值，按目标 Go 字段类型区分
// （契约 4.2 Step 3 / 4.4 Step 2）：
//
//   - nullIsNoOp：string 目标字段（Go `string`）。null 是无操作——既不产出值，也不占用
//     first-wins 键位，且不阻断后续匹配键的 last-wins 覆盖（`{"type":"a","TYPE":null}` → "a"）。
//   - nullResetsToZero：非 string 目标字段（object / array / int / bool / 浮点 / 指针）。
//     null 参与文档序 last-wins：若 null 是文档中最后一个匹配键，目标字段覆盖为零值
//     （nil / 0 / false）；该规则同样适用于嵌套字段（如 `choices[].index` 是 int、`usage` 是对象、
//     `finish_reason` 是 *string）。
//
// 与 `encoding/json` 的对照说明：`encoding/json` 对非指针标量（int/bool/float/string）的 null 一律
// 无操作，对指针/切片/映射/接口的 null 置 nil。改造前的累积产物由「typed 解码 + map 累积」两步
// 产生，两步对 null 的结论并不一致（map 路径对重复键取 last-wins，null 覆盖为 nil → 零值）。
// 契约 4.2 选定「非 string 一律零值」这一统一口径，使单次扫描同时服务转发/文本/usage 与累积 body；
// 对 usage/choices/finish_reason 等指针与容器字段该口径与 typed 解码一致，对 int 标量
// （如 created/index）则与改造前的 map 累积路径一致。该差异由
// TestChatStreamNullDestinationTypeRules 的对照探针显式记录。
type nullPolicy int

const (
	// nullIsNoOp：string 目标字段的 null 无操作。
	nullIsNoOp nullPolicy = iota
	// nullResetsToZero：非 string 目标字段的 null 参与 last-wins 并覆盖为零值。
	nullResetsToZero
)

// foldMatch 在 root 对象中按 exact-then-fold 语义查找 target 键，并返回被选中的值。
//
// 键匹配为 exact-then-fold 大小写不敏感，与 `encoding/json` struct 解码对齐：先精确匹配，失败回退
// Unicode 简单折叠匹配（`strings.EqualFold`，等价于 `encoding/json` 的 foldName），故 `id`/`Id`/
// `ID`、转义拼写（`{"\u0069d":...}` 解码为 `id`）都会命中。库间差异：`encoding/json` 把字符串中的
// 非法 UTF-8 净化为 U+FFFD，而 `gjson` 保留原始字节，此处不做修正。
//
// 值选取与 `encoding/json` 对齐：大小写变体之间按文档序 last-wins（`{"id":"a","ID":"b"}` → "b"）。
// 显式 `null` 的语义按 policy 区分（见 nullPolicy）：string 目标字段的 null 是无操作，不产出值、
// 不占用 first-wins 键位、不阻断后续覆盖；非 string 目标字段的 null 参与 last-wins 并覆盖为零值。
//
// 与 `encoding/json` 的已知差异（契约 AC-9 声明的 sanctioned exception）：字节完全相同的重复键
// （以及转义与字面拼写解码后相同的键）取「第一个」（first-wins），而 `encoding/json` 取「最后一个」
// （last-wins）。该差异由 TestChatStreamAccumulatorDuplicateKeysFirstWins 锁定。非 string 目标的
// 显式 null 不占用 first-wins 键位，故后续同名键仍可覆盖（`{"created":1,"created":null}` → 0）。
//
// 类型校验覆盖所有匹配键（含被 first-wins/last-wins 跳过的键，契约 4.2 Step 4）：胜出键由调用方
// 按目标类型完整解析（validate 在此仅做浅层类型校验），其余匹配键走 deepValidate 递归到目标类型；
// 任一匹配键无法解析到目标类型即整体失败，对齐 `encoding/json` 的递归校验语义。所有校验函数
// 对 `null` 必须放行（null 不是类型错误）。
func foldMatch(root gjson.Result, target string, policy nullPolicy, validate func(gjson.Result) error, deepValidate func(gjson.Result) error) (selected gjson.Result, found bool, err error) {
	if !root.IsObject() {
		return gjson.Result{}, false, nil
	}
	// validateOther 校验「不参与取值」的匹配键（被 first-wins/last-wins 跳过的键，以及被
	// 大小写变体或显式 null 覆盖的旧胜出键）：优先递归到目标类型（deepValidate），否则浅层校验。
	validateOther := func(value gjson.Result) error {
		if deepValidate != nil {
			return deepValidate(value)
		}
		if validate != nil {
			return validate(value)
		}
		return nil
	}
	winningKey := ""
	root.ForEach(func(key, value gjson.Result) bool {
		// 先精确匹配（快路径），失败再回退 Unicode 简单折叠匹配。
		if key.Str != target && !strings.EqualFold(key.Str, target) {
			return true
		}
		if value.Type == gjson.Null && policy == nullIsNoOp {
			// string 目标字段：null 是无操作——不产出值、不占用 first-wins 键位、不阻断后续覆盖。
			if validateErr := validateOther(value); validateErr != nil {
				err = validateErr
				return false
			}
			return true
		}
		if value.Type == gjson.Null {
			// 非 string 目标字段：null 参与文档序 last-wins，覆盖为零值，且不占用 first-wins 键位。
			if found {
				// 旧胜出键被 null 覆盖：仍须递归校验（契约 4.2 Step 4）。
				if validateErr := validateOther(selected); validateErr != nil {
					err = validateErr
					return false
				}
			}
			selected = value
			found = true
			winningKey = ""
			return true
		}
		// 字节（解码后）完全相同的重复键：first-wins（AC-9 sanctioned 差异）；仍须递归校验。
		if winningKey != "" && winningKey == key.Str {
			if validateErr := validateOther(value); validateErr != nil {
				err = validateErr
				return false
			}
			return true
		}
		if found {
			// 大小写变体覆盖旧胜出键：旧值不再参与取值，仍须递归校验（契约 4.2 Step 4）。
			if validateErr := validateOther(selected); validateErr != nil {
				err = validateErr
				return false
			}
		}
		winningKey = key.Str
		selected = value
		found = true
		return true
	})
	if err != nil {
		return gjson.Result{}, false, err
	}
	if found && validate != nil {
		// 胜出键由调用方按目标类型完整解析，此处仅做浅层类型校验。
		if validateErr := validate(selected); validateErr != nil {
			return gjson.Result{}, false, validateErr
		}
	}
	return selected, found, nil
}

// foldTarget 描述一次对象扫描中的一个目标键及其既有 foldMatch 语义。
// 它是只读配置：key 必须在 strings.EqualFold 意义下互不相同（C1 调用点均为编译期固定目标集合）。
type foldTarget struct {
	key          string
	policy       nullPolicy
	validate     func(gjson.Result) error
	deepValidate func(gjson.Result) error
}

// foldSelection 保存一个目标键完成折叠后的选值状态。
// selected 与 foldMatch 的返回值逐位一致；目标出错时 selected 为零值 Result、found 为 false。
type foldSelection struct {
	selected gjson.Result
	found    bool
}

// foldCollectMaxTargets 是多目标折叠原语支持的目标数上界：C1 最大目标数为 message 的 7，取 8 留余量。
const foldCollectMaxTargets = 8

// foldCollect 单次遍历对象，按声明顺序把每个目标的 foldMatch 等价结果写入调用方提供的 selections；
// 任一错误按目标声明顺序优先返回。
//
// 前置条件（调用点均为编译期常量，无需运行时守卫）：
//
//   - len(selections) == len(targets)；
//   - 0 <= len(targets) <= foldCollectMaxTargets。
//
// selections 由调用方提供：C1 调用点传入栈上定长数组的切片
// （如 var buf [foldCollectMaxTargets]foldSelection; foldCollect(root, targets, buf[:len(targets)])），
// 使本函数不产生额外堆分配。调用方可在循环中复用一个定长缓冲；本函数对 [0, len(targets))
// 的每个下标都会写入（非对象根写零值，正常路径写最终 selection），因此复用缓冲不会被旧值污染。
//
// 非对象根：selections 全部置零并返回 nil，与逐个 foldMatch 等价。
// 若 len(targets) > foldCollectMaxTargets 则 panic：这是编译期即可知的编程错误（C1 全部调用点固定 ≤7），
// 不属于输入数据可触发的运行时错误。
//
// 语义与「对每个 target 逐个调用 foldMatch」完全等价：每个目标独立维护选值（exact-then-fold 匹配、
// first-wins 重复键、大小写变体 last-wins、nullPolicy）与首个错误，扫描结束后按 targets 声明顺序
// 裁决全局首错。对象只遍历一次，时间 O(members × targets)，辅助空间 O(targets)，不保存出现次数相关的
// 无界切片。
func foldCollect(root gjson.Result, targets []foldTarget, selections []foldSelection) error {
	if len(targets) > foldCollectMaxTargets {
		panic("foldCollect: len(targets) exceeds foldCollectMaxTargets")
	}
	// foldState 是单个目标的折叠状态：与 foldMatch 的局部变量一一对应，另加首个错误。
	// 辅助空间仅随目标数增长，不随对象成员数累积；使用栈上定长数组避免堆分配。
	type foldState struct {
		selected   gjson.Result
		found      bool
		winningKey string
		err        error
	}
	var statesBuf [foldCollectMaxTargets]foldState
	states := statesBuf[:len(targets)]

	if !root.IsObject() {
		// 非对象根：与逐个 foldMatch 一致，全部置零且不报错。
		for i := range targets {
			selections[i] = foldSelection{}
		}
		return nil
	}

	root.ForEach(func(key, value gjson.Result) bool {
		for i := range targets {
			target := &targets[i]
			// 先精确匹配（快路径），失败再回退 Unicode 简单折叠匹配。
			if key.Str != target.key && !strings.EqualFold(key.Str, target.key) {
				continue
			}
			state := &states[i]
			// 该目标已因先前错误停止：与单独调用 foldMatch 的停止位置一致，后续成员不再影响它。
			if state.err != nil {
				continue
			}
			// validateOther 校验「不参与取值」的匹配键（被 first-wins/last-wins 跳过的键，以及被
			// 大小写变体或显式 null 覆盖的旧胜出键）：优先递归到目标类型（deepValidate），否则浅层校验。
			validateOther := func(value gjson.Result) error {
				if target.deepValidate != nil {
					return target.deepValidate(value)
				}
				if target.validate != nil {
					return target.validate(value)
				}
				return nil
			}
			if value.Type == gjson.Null && target.policy == nullIsNoOp {
				// string 目标字段：null 是无操作——不产出值、不占用 first-wins 键位、不阻断后续覆盖。
				if validateErr := validateOther(value); validateErr != nil {
					state.err = validateErr
				}
				continue
			}
			if value.Type == gjson.Null {
				// 非 string 目标字段：null 参与文档序 last-wins，覆盖为零值，且不占用 first-wins 键位。
				if state.found {
					// 旧胜出键被 null 覆盖：仍须递归校验。
					if validateErr := validateOther(state.selected); validateErr != nil {
						state.err = validateErr
						continue
					}
				}
				state.selected = value
				state.found = true
				state.winningKey = ""
				continue
			}
			// 字节（解码后）完全相同的重复键：first-wins；仍须递归校验。
			if state.winningKey != "" && state.winningKey == key.Str {
				if validateErr := validateOther(value); validateErr != nil {
					state.err = validateErr
				}
				continue
			}
			if state.found {
				// 大小写变体覆盖旧胜出键：旧值不再参与取值，仍须递归校验。
				if validateErr := validateOther(state.selected); validateErr != nil {
					state.err = validateErr
					continue
				}
			}
			state.winningKey = key.Str
			state.selected = value
			state.found = true
		}
		return true
	})

	// 扫描结束后对每个目标执行胜出值的浅层校验（与 foldMatch 尾部一致）；
	// 扫描阶段已出错的目标不再执行，从而保持「跳过/被覆盖值错误优先于最终胜出值错误」的停止位置。
	for i := range targets {
		state := &states[i]
		if state.err != nil {
			continue
		}
		if state.found && targets[i].validate != nil {
			if validateErr := targets[i].validate(state.selected); validateErr != nil {
				state.err = validateErr
			}
		}
	}

	// 按目标声明顺序确定全局首错；出错目标返回零值 selection（对齐 foldMatch 出错时的返回值）。
	for i := range targets {
		if states[i].err != nil {
			selections[i] = foldSelection{}
			continue
		}
		selections[i] = foldSelection{selected: states[i].selected, found: states[i].found}
	}
	for i := range targets {
		if states[i].err != nil {
			return states[i].err
		}
	}
	return nil
}

// validateString 校验匹配键为非 null 的 JSON string（对齐 encoding/json 对 Go string 字段的严格性）。
func validateString(value gjson.Result) error {
	if value.Type != gjson.Null && value.Type != gjson.String {
		return fmt.Errorf("expected string, got %s", value.Type)
	}
	return nil
}

// validateExactInt 校验匹配键为非 null 的精确十进制 int64（拒绝小数、指数形式与溢出）。
func validateExactInt(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if value.Type != gjson.Number {
		return fmt.Errorf("expected integer, got %s", value.Type)
	}
	if _, err := strconv.ParseInt(value.Raw, 10, 64); err != nil {
		return fmt.Errorf("expected integer, got %s", value.Raw)
	}
	return nil
}

// validateBool 校验匹配键为非 null 的布尔（旧 typed *bool 语义）。
func validateBool(value gjson.Result) error {
	if value.Type != gjson.Null && value.Type != gjson.True && value.Type != gjson.False {
		return fmt.Errorf("expected boolean, got %s", value.Type)
	}
	return nil
}

// validateObject 校验匹配键为非 null 的 JSON 对象。
func validateObject(value gjson.Result) error {
	if value.Type != gjson.Null && !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	return nil
}

// validateArray 校验匹配键为非 null 的 JSON 数组。
func validateArray(value gjson.Result) error {
	if value.Type != gjson.Null && !value.IsArray() {
		return fmt.Errorf("expected array, got %s", value.Type)
	}
	return nil
}

// validateAny 校验匹配键不含超出 float64 表示范围的数字（旧 typed any 字段解码失败即整帧失败）。
func validateAny(value gjson.Result) error {
	if hasUnrepresentableNumber(value) {
		return fmt.Errorf("contains a number outside float64 range")
	}
	return nil
}

// validateChatStreamUsage 把匹配值递归校验为 *model.Usage 目标类型（契约 4.2 Step 4）：
// null 合法（零值）；非对象或任一嵌套字段无法解析为 model.Usage 即报错。
func validateChatStreamUsage(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	_, err := buildChatStreamUsage(value)
	return err
}

// validateChatStreamChoices 把匹配值递归校验为 choice 对象数组目标类型（契约 4.2 Step 4）：
// null 合法；非数组、非对象元素或任一 choice 字段无法解析即报错。
func validateChatStreamChoices(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsArray() {
		return fmt.Errorf("expected array, got %s", value.Type)
	}
	for _, element := range value.Array() {
		if element.Type == gjson.Null {
			continue
		}
		if !element.IsObject() {
			return fmt.Errorf("choices element: expected object or null, got %s", element.Type)
		}
		if _, err := parseChatStreamChoice(element); err != nil {
			return err
		}
	}
	return nil
}

// validateChatStreamDelta 把匹配值递归校验为 model.Message 目标类型（契约 4.2 Step 4）：
// null 合法；非对象或任一 delta 字段无法解析即报错。
func validateChatStreamDelta(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	_, err := parseChatStreamDelta(value)
	return err
}

// validateChatStreamToolCalls 把匹配值递归校验为 []model.Tool 目标类型（契约 4.2 Step 4）：
// null 合法；非数组、非对象元素或任一 tool_call 字段无法解析即报错。
func validateChatStreamToolCalls(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsArray() {
		return fmt.Errorf("expected array, got %s", value.Type)
	}
	for _, element := range value.Array() {
		if element.Type == gjson.Null {
			continue
		}
		if !element.IsObject() {
			return fmt.Errorf("tool_calls element: expected object or null, got %s", element.Type)
		}
		if _, err := parseChatStreamToolCall(element); err != nil {
			return err
		}
	}
	return nil
}

// validateChatStreamFunction 把匹配值递归校验为 model.Function 目标类型（契约 4.2 Step 4）：
// null 合法；非对象或任一 function 字段无法解析即报错。
func validateChatStreamFunction(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	return validateFunctionFields(value)
}

// validateFunctionFields 校验 JSON 对象的各字段能否解析为 model.Function：
// name/description 为 Go string，strict 为 *bool，arguments/parameters 为 any。
func validateFunctionFields(functionResult gjson.Result) error {
	var err error
	if _, err = foldString(functionResult, "name"); err != nil {
		return fmt.Errorf("tool_calls.function.name: %w", err)
	}
	if _, err = foldString(functionResult, "description"); err != nil {
		return fmt.Errorf("tool_calls.function.description: %w", err)
	}
	if err = foldBool(functionResult, "strict"); err != nil {
		return fmt.Errorf("tool_calls.function.strict: %w", err)
	}
	if _, _, err = foldAny(functionResult, "arguments"); err != nil {
		return fmt.Errorf("tool_calls.function.arguments: %w", err)
	}
	if _, _, err = foldAny(functionResult, "parameters"); err != nil {
		return fmt.Errorf("tool_calls.function.parameters: %w", err)
	}
	return nil
}

// validatePromptTokensDetails 把匹配值递归校验为 *model.PromptTokensDetails 目标类型（契约 4.2 Step 4）。
func validatePromptTokensDetails(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	_, err := parsePromptTokensDetails(value)
	return err
}

// validateCompletionTokensDetails 把匹配值递归校验为 *model.CompletionTokensDetails 目标类型（契约 4.2 Step 4）。
func validateCompletionTokensDetails(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	_, err := parseCompletionTokensDetails(value)
	return err
}

// validateCompletionsChoices 把匹配值递归校验为 Completions 的 choice 数组目标类型（契约 4.4 Step 2）：
// null 合法；非数组、非对象元素或任一 `text`/`finish_reason`（均为 Go string）无法解析即报错。
func validateCompletionsChoices(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsArray() {
		return fmt.Errorf("expected array, got %s", value.Type)
	}
	for _, element := range value.Array() {
		if element.Type == gjson.Null {
			continue
		}
		if !element.IsObject() {
			return fmt.Errorf("choices element: expected object or null, got %s", element.Type)
		}
		if _, err := foldString(element, "text"); err != nil {
			return fmt.Errorf("choices.text: %w", err)
		}
		if _, err := foldString(element, "finish_reason"); err != nil {
			return fmt.Errorf("choices.finish_reason: %w", err)
		}
	}
	return nil
}

// foldString 返回类型化 string 字段的值：缺失/null 视为零值（null 为无操作），任一匹配键非 String 即报错。
func foldString(root gjson.Result, target string) (string, error) {
	selected, found, err := foldMatch(root, target, nullIsNoOp, validateString, nil)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return selected.String(), nil
}

// foldNullableString 返回可空 string 指针字段（如 finish_reason）的值与「是否存在非 null 值」标志。
// finish_reason 在旧 typed 结构体中为 *string（指针），故显式 null 覆盖为 nil（零值），
// 与 `encoding/json` 的指针语义一致（`{"finish_reason":"stop","finish_reason":null}` → nil）。
func foldNullableString(root gjson.Result, target string) (string, bool, error) {
	selected, found, err := foldMatch(root, target, nullResetsToZero, validateString, nil)
	if err != nil {
		return "", false, err
	}
	if !found || selected.Type == gjson.Null {
		return "", false, nil
	}
	return selected.String(), true, nil
}

// foldInt64 返回类型化 int64 字段的值：缺失/null 视为 0，其余必须是精确的十进制 int64。
func foldInt64(root gjson.Result, target string) (int64, error) {
	selected, found, err := foldMatch(root, target, nullResetsToZero, validateExactInt, nil)
	if err != nil {
		return 0, err
	}
	if !found || selected.Type == gjson.Null {
		return 0, nil
	}
	return strconv.ParseInt(selected.Raw, 10, 64)
}

// foldInt 返回类型化 int 字段的值（对齐旧 typed 路径对 Go int 字段的解码）。
func foldInt(root gjson.Result, target string) (int, error) {
	value, err := foldInt64(root, target)
	if err != nil {
		return 0, err
	}
	return int(value), nil
}

// foldBool 仅校验类型化 *bool 字段：缺失/null 合法，任一匹配键非布尔即报错。
func foldBool(root gjson.Result, target string) error {
	_, _, err := foldMatch(root, target, nullResetsToZero, validateBool, nil)
	return err
}

// foldObjectValidated 返回对象字段的值：缺失/null 视为不存在，任一匹配键非 Object 即报错；
// 非胜出的匹配键由 deepValidate 递归校验到目标 Go 类型（契约 4.2 Step 4）。
func foldObjectValidated(root gjson.Result, target string, deepValidate func(gjson.Result) error) (gjson.Result, bool, error) {
	selected, found, err := foldMatch(root, target, nullResetsToZero, validateObject, deepValidate)
	if err != nil {
		return gjson.Result{}, false, err
	}
	if !found || selected.Type == gjson.Null {
		return gjson.Result{}, false, nil
	}
	return selected, true, nil
}

// foldArrayValidated 返回数组字段的值：缺失/null 视为不存在，任一匹配键非 Array 即报错；
// 非胜出的匹配键由 deepValidate 递归校验到目标 Go 类型（契约 4.2 Step 4）。
func foldArrayValidated(root gjson.Result, target string, deepValidate func(gjson.Result) error) (gjson.Result, bool, error) {
	selected, found, err := foldMatch(root, target, nullResetsToZero, validateArray, deepValidate)
	if err != nil {
		return gjson.Result{}, false, err
	}
	if !found || selected.Type == gjson.Null {
		return gjson.Result{}, false, nil
	}
	return selected, true, nil
}

// foldAny 返回非类型化 any 字段的选中值：任一匹配键含超出 float64 范围的数字即报错。
// 显式 null 参与 last-wins，返回 null Result（消费者按 conv.AsString 语义产出零值）。
func foldAny(root gjson.Result, target string) (gjson.Result, bool, error) {
	return foldMatch(root, target, nullResetsToZero, validateAny, nil)
}

// foldLooseString 沿用旧 map 断言语义：仅选中的 JSON string 产出其值，其余类型忽略为空串。
// 非 string 匹配键不报错（区别于 foldString 的严格语义）。
//
// system_fingerprint 在累积器中为 Go `string` 目标，故显式 null 是无操作：不产出值，
// 也不占用 first-wins 键位、不阻断后续覆盖（契约 4.2 Step 3 的 string 目标规则）。
func foldLooseString(root gjson.Result, target string) string {
	selected, found, err := foldMatch(root, target, nullIsNoOp, nil, nil)
	if err != nil || !found {
		return ""
	}
	return looseStringValue(selected)
}

// foldLooseResult 返回宽松字段的选中值（无类型校验）：缺失时返回零值 Result。
// 显式 null 参与 last-wins（与旧 map 路径一致），返回 null Result。
func foldLooseResult(root gjson.Result, target string) gjson.Result {
	selected, found, err := foldMatch(root, target, nullResetsToZero, nil, nil)
	if err != nil || !found {
		return gjson.Result{}
	}
	return selected
}

// foldLooseObject 返回宽松对象字段的选中值：仅当选中的匹配值为 Object 时返回 ok=true，
// 非 Object（含 null）一律忽略且不报错（区别于 foldObject 的严格语义）。
func foldLooseObject(root gjson.Result, target string) (gjson.Result, bool) {
	selected := foldLooseResult(root, target)
	if !selected.IsObject() {
		return gjson.Result{}, false
	}
	return selected, true
}

// foldLooseInt 是 tool_call.index 的非类型化读取：Number 截断为 int，非 Number 或非有限数取 0。
func foldLooseInt(root gjson.Result, target string) int {
	result := foldLooseResult(root, target)
	if result.Type != gjson.Number {
		return 0
	}
	if math.IsNaN(result.Num) || math.IsInf(result.Num, 0) {
		return 0
	}
	return int(result.Num)
}

// hasUnrepresentableNumber 递归检测 JSON 中是否存在超出 float64 表示范围的数字。
// 旧实现的第二步（json.Unmarshal 到 map[string]any）对此类数字会失败并跳过该帧的累积；
// 本路径不物化 map，需显式检测以决定「是否跳过累积」，从而保持累积 body 与改造前等价
// （spec: Reconstructed output SHALL be unchanged）。注意：此检测不拒绝整帧——文本与 usage
// 已由 typed 等价的校验/提取阶段产出，必须保留。
func hasUnrepresentableNumber(result gjson.Result) bool {
	switch result.Type {
	case gjson.Number:
		// gjson 对超出 float64 范围的数字取值为 ±Inf（与 strconv.ParseFloat 的报错判定等价，
		// 已由 20 万例随机数字 fuzz 验证）；下溢到 0 的字面量（如 1e-400）取值为有限数，
		// 与 encoding/json 不报错的行为一致。
		return math.IsInf(result.Num, 0)
	case gjson.JSON:
		found := false
		result.ForEach(func(_, value gjson.Result) bool {
			if hasUnrepresentableNumber(value) {
				found = true
				return false
			}
			return true
		})
		return found
	default:
		return false
	}
}

// looseStringValue 沿用旧 map 断言语义：仅 JSON string 产出其值，其余类型忽略为空串。
func looseStringValue(result gjson.Result) string {
	if result.Type != gjson.String {
		return ""
	}
	return result.String()
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

// textResponseExtraction contains only fields consumed by the non-stream handler.
//
// 计费语义（spec: Billing-critical fields SHALL fail rather than degrade / Non-billing detail unparseable）：
// 三个 basis（prompt_tokens/completion_tokens/total_tokens）严格——不可解析即整份提取失败；details 宽容——
// 不可解析视为该 detail 缺失（指针 nil），不影响 basis。`null` 一律映射为零值。
//
// 重复键差异（契约 AC-9 sanctioned exception）：本路径按 gjson 路径取值，字节完全相同的重复键
// 「first-wins」；旧 `encoding/json` typed 解码为「last-wins」。大小写变体为文档序 last-wins，
// 与旧实现一致；转义与字面拼写解码后相同的键按重复处理（first-wins）。由
// TestExtractTextResponseEquivalence 的 duplicate 用例锁定。
type textResponseExtraction struct {
	Error          *model.Error
	Usage          model.Usage
	ChoiceContents []string
}

// extractTextResponse validates one upstream chat response and extracts error, billing usage, and
// choice content; malformed JSON, invalid billing fields, or invalid choices return an error, null
// billing integers map to zero, and invalid detail fields are omitted.
//
// 库间差异（sanctioned）：`encoding/json` 把字符串中的非法 UTF-8 净化为 U+FFFD，`gjson` 保留原始字节，
// 此处不做修正。
func extractTextResponse(responseBody []byte) (result textResponseExtraction, err error) {
	if !json.Valid(responseBody) {
		return textResponseExtraction{}, fmt.Errorf("chat response body is not valid JSON")
	}
	root := gjson.ParseBytes(responseBody)
	if root.Type == gjson.Null {
		// 旧 typed 解码 `json.Unmarshal("null", &SlimTextResponse)` 成功且全部为零值。
		return textResponseExtraction{}, nil
	}
	if !root.IsObject() {
		// 数组/字符串/数字/布尔根：旧 typed 解码失败（非对象根无法解码为 struct）。
		return textResponseExtraction{}, fmt.Errorf("chat response root must be an object, got %s", root.Type)
	}

	// 单次 foldCollect 收集顶层 error(对象)/usage(对象)/choices(数组)：声明顺序锁定
	// error -> usage -> choices 的错误优先级，gjson 对象只遍历一次（契约 3.3 root）。
	rootTargets := []foldTarget{
		{key: "error", policy: nullResetsToZero, validate: validateObject, deepValidate: validateTextResponseError},
		{key: "usage", policy: nullResetsToZero, validate: validateObject, deepValidate: validateTextResponseUsage},
		{key: "choices", policy: nullResetsToZero, validate: validateArray, deepValidate: validateTextResponseChoices},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(rootTargets)]
	collectErr := foldCollect(root, rootTargets, selections)
	// foldCollect 只返回聚合首错，无法区分字段，也无法与「胜出值的解析错误」交错。仅错误路径用逐目标
	// foldMatch 回放定位首个出错目标（成功路径不额外扫描），从而按既有顺序与包装文案返回。
	errIdx := -1
	if collectErr != nil {
		for i := range rootTargets {
			if _, _, e := foldMatch(root, rootTargets[i].key, rootTargets[i].policy, rootTargets[i].validate, rootTargets[i].deepValidate); e != nil {
				errIdx = i
				break
			}
		}
	}

	// error 阶段：foldCollect 的深层校验/胜出值解析错误都包装为 "error: ..."。
	if errIdx == 0 {
		return textResponseExtraction{}, fmt.Errorf("error: %w", collectErr)
	}
	errorSel := selections[0]
	if errorSel.found && errorSel.selected.Type != gjson.Null {
		parsed, parseErr := parseTextResponseError(errorSel.selected)
		if parseErr != nil {
			return textResponseExtraction{}, fmt.Errorf("error: %w", parseErr)
		}
		// 旧 Handler 的触发条件为 `textResponse.Error.Type != ""`：Error 仅在 Type 非空时
		// 才转成上游错误返回，其余情况（缺失/null/`{}`/仅 message）继续透传。此处沿用同一
		// 判据，使 `result.Error != nil` 与「旧实现返回上游错误」严格等价。
		if parsed.Type != "" {
			result.Error = &parsed
		}
	}

	// usage 阶段：深层校验错误包装为 "usage: ..."；胜出值解析错误沿用既有「不额外包装」行为。
	if errIdx == 1 {
		return textResponseExtraction{}, fmt.Errorf("usage: %w", collectErr)
	}
	usageSel := selections[1]
	if usageSel.found && usageSel.selected.Type != gjson.Null {
		parsedUsage, parseErr := parseTextResponseUsage(usageSel.selected)
		if parseErr != nil {
			return textResponseExtraction{}, parseErr
		}
		result.Usage = parsedUsage
	}

	// choices 阶段：深层校验错误包装为 "choices: ..."；元素解析错误沿用既有「不额外包装」行为。
	if errIdx == 2 {
		return textResponseExtraction{}, fmt.Errorf("choices: %w", collectErr)
	}
	choicesSel := selections[2]
	if choicesSel.found && choicesSel.selected.Type != gjson.Null {
		for _, element := range choicesSel.selected.Array() {
			// foldCollect 仅对「被跳过的匹配键」递归校验到目标类型；胜出键的完整递归校验由调用方
			// 完成（与 extractCompletionsStreamText 同一约定），故此处显式解析每个 choice。
			content, choiceErr := parseTextResponseChoiceContent(element)
			if choiceErr != nil {
				return textResponseExtraction{}, choiceErr
			}
			result.ChoiceContents = append(result.ChoiceContents, content)
		}
	}
	return result, nil
}

// parseTextResponseChoiceContent 完整校验一个 choice 并按 model.Message.StringContent 的语义
// 产出其 content 文本：非对象元素报错（null 元素产出空串，对齐旧 typed 切片零值）。
//
// 单次 foldCollect 收集 index/finish_reason/message（契约 3.3 choice）；胜出 message 由
// parseTextResponseMessageContent 单次扫描完成校验并取回 content，不再二次 foldAny。
func parseTextResponseChoiceContent(element gjson.Result) (string, error) {
	if element.Type == gjson.Null {
		return "", nil
	}
	if !element.IsObject() {
		return "", fmt.Errorf("choices element: expected object or null, got %s", element.Type)
	}
	targets := []foldTarget{
		{key: "index", policy: nullResetsToZero, validate: validateExactInt},
		{key: "finish_reason", policy: nullIsNoOp, validate: validateString},
		{key: "message", policy: nullResetsToZero, validate: validateObject, deepValidate: validateTextResponseMessage},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	collectErr := foldCollect(element, targets, selections)
	if collectErr != nil {
		fields := [...]string{"index", "finish_reason", "message"}
		for i := range targets {
			if _, _, e := foldMatch(element, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
				return "", fmt.Errorf("choices.%s: %w", fields[i], e)
			}
		}
		return "", collectErr
	}
	messageSel := selections[2]
	if !messageSel.found || messageSel.selected.Type == gjson.Null {
		return "", nil
	}
	// foldCollect 仅对「被跳过的匹配键」递归校验；胜出 message 由 parseTextResponseMessageContent
	// 完整校验并同时返回已折叠的 content（单次扫描）。
	contentResult, err := parseTextResponseMessageContent(messageSel.selected)
	if err != nil {
		return "", err
	}
	return contentResultToText(contentResult), nil
}

// validateTextResponseError 校验 error 目标类型（model.Error）：null 合法；非对象或
// message/type/param 非 string、code 含超出 float64 范围的数字即报错。
//
// 字段校验复用 parseTextResponseError（其字段校验/文案与旧实现逐字一致），从而单次 foldCollect
// 完成 message/type/param/code 的校验。
func validateTextResponseError(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsObject() {
		return fmt.Errorf("expected object, got %s", value.Type)
	}
	_, err := parseTextResponseError(value)
	return err
}

// parseTextResponseError 把已确认的 JSON 对象解析为 model.Error。
//
// 单次 foldCollect 收集 message/type/param(string, nullIsNoOp)/code(any, nullResetsToZero)；
// 声明顺序锁定 message -> type -> param -> code 的错误优先级（契约 3.3 error）。
func parseTextResponseError(value gjson.Result) (model.Error, error) {
	targets := []foldTarget{
		{key: "message", policy: nullIsNoOp, validate: validateString},
		{key: "type", policy: nullIsNoOp, validate: validateString},
		{key: "param", policy: nullIsNoOp, validate: validateString},
		{key: "code", policy: nullResetsToZero, validate: validateAny},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	collectErr := foldCollect(value, targets, selections)
	if collectErr != nil {
		fields := [...]string{"message", "type", "param", "code"}
		for i := range targets {
			if _, _, e := foldMatch(value, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
				return model.Error{}, fmt.Errorf("error.%s: %w", fields[i], e)
			}
		}
		return model.Error{}, collectErr
	}
	parsed := model.Error{}
	if selections[0].found {
		parsed.Message = selections[0].selected.String()
	}
	if selections[1].found {
		parsed.Type = selections[1].selected.String()
	}
	if selections[2].found {
		parsed.Param = selections[2].selected.String()
	}
	if selections[3].found && selections[3].selected.Type != gjson.Null {
		parsed.Code = selections[3].selected.Value()
	}
	return parsed, nil
}

// parseTextResponseUsage 严格解析三个 basis、宽容解析 details：details 不可解析视为缺失。
//
// 单次 foldCollect 收集三个 basis（int, nullResetsToZero, validateExactInt）；胜出 basis 的最终
// int64 解析仍需显式完成（foldCollect 只做折叠与浅校验，不负责 ParseInt）。details 由 tolerant*
// 各自单次扫描获取，保持宽容降级。
func parseTextResponseUsage(usageResult gjson.Result) (model.Usage, error) {
	parsed := model.Usage{}
	targets := []foldTarget{
		{key: "prompt_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "completion_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "total_tokens", policy: nullResetsToZero, validate: validateExactInt},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	collectErr := foldCollect(usageResult, targets, selections)
	if collectErr != nil {
		fields := [...]string{"prompt_tokens", "completion_tokens", "total_tokens"}
		for i := range targets {
			if _, _, e := foldMatch(usageResult, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
				return parsed, fmt.Errorf("usage.%s: %w", fields[i], e)
			}
		}
		return parsed, collectErr
	}
	// 胜出 basis 的最终解析：缺失/null/出错（selection 归零）均为 0；胜出值已由 validateExactInt
	// 保证是精确十进制 int64，故 Int() 与 strconv.ParseInt(Raw,10,64) 等价。
	parsed.PromptTokens = int(selections[0].selected.Int())
	parsed.CompletionTokens = int(selections[1].selected.Int())
	parsed.TotalTokens = int(selections[2].selected.Int())
	parsed.PromptTokensDetails = tolerantPromptTokensDetails(usageResult)
	parsed.CompletionTokensDetails = tolerantCompletionTokensDetails(usageResult)
	return parsed, nil
}

// validateTextResponseUsage 递归校验 usage 目标类型：三个 basis 严格（不可解析即报错），
// details 宽容（不可解析不报错，由解析阶段降级为缺失）。用于非胜出匹配键的递归校验
// （对齐 encoding/json 对全部重复键的校验语义，但与 C1 声明的 details 宽容一致）。
func validateTextResponseUsage(usageResult gjson.Result) error {
	_, err := parseTextResponseUsage(usageResult)
	return err
}

// tolerantPromptTokensDetails 宽容读取 prompt_tokens_details：非对象/null/缺失 → nil；
// 对象内任一 int 子字段不可解析 → 该子字段按零值（视为缺失），不失败。
//
// 对象内 5 个 int 子字段由单次 foldCollect 取得；聚合错误被忽略，出错子字段的 selection 归零
// 即实现「逐字段降级」。
func tolerantPromptTokensDetails(usageResult gjson.Result) *model.PromptTokensDetails {
	detailsResult, ok, err := foldObjectValidated(usageResult, "prompt_tokens_details", nil)
	if err != nil || !ok {
		return nil
	}
	details := &model.PromptTokensDetails{}
	targets := []foldTarget{
		{key: "cached_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "cache_write_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "audio_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "text_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "image_tokens", policy: nullResetsToZero, validate: validateExactInt},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	foldCollect(detailsResult, targets, selections)
	// 逐字段降级：缺失/null/出错（selection 归零）均为 0；胜出值已由 validateExactInt 保证精确。
	details.CachedTokens = int(selections[0].selected.Int())
	details.CacheWriteTokens = int(selections[1].selected.Int())
	details.AudioTokens = int(selections[2].selected.Int())
	details.TextTokens = int(selections[3].selected.Int())
	details.ImageTokens = int(selections[4].selected.Int())
	return details
}

// tolerantCompletionTokensDetails 与 tolerantPromptTokensDetails 同构，对应 completion_tokens_details。
func tolerantCompletionTokensDetails(usageResult gjson.Result) *model.CompletionTokensDetails {
	detailsResult, ok, err := foldObjectValidated(usageResult, "completion_tokens_details", nil)
	if err != nil || !ok {
		return nil
	}
	details := &model.CompletionTokensDetails{}
	targets := []foldTarget{
		{key: "reasoning_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "accepted_prediction_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "rejected_prediction_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "audio_tokens", policy: nullResetsToZero, validate: validateExactInt},
		{key: "text_tokens", policy: nullResetsToZero, validate: validateExactInt},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	foldCollect(detailsResult, targets, selections)
	details.ReasoningTokens = int(selections[0].selected.Int())
	details.AcceptedPredictionTokens = int(selections[1].selected.Int())
	details.RejectedPredictionTokens = int(selections[2].selected.Int())
	details.AudioTokens = int(selections[3].selected.Int())
	details.TextTokens = int(selections[4].selected.Int())
	return details
}

// validateTextResponseChoices 把匹配值递归校验为 []TextResponseChoice 目标类型：null 合法；
// 非数组、非对象元素或任一 choice 字段无法解析即报错（对齐旧 typed 解码的严格性）。
func validateTextResponseChoices(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsArray() {
		return fmt.Errorf("expected array, got %s", value.Type)
	}
	for _, element := range value.Array() {
		if element.Type == gjson.Null {
			continue
		}
		if !element.IsObject() {
			return fmt.Errorf("choices element: expected object or null, got %s", element.Type)
		}
		if err := validateTextResponseChoice(element); err != nil {
			return err
		}
	}
	return nil
}

// validateTextResponseChoice 校验 index(int)/finish_reason(string)/message(model.Message)，
// 用于 choices 数组中「被跳过的匹配键」的递归校验。
//
// 单次 foldCollect 收集 index/finish_reason/message；声明顺序锁定错误优先级（契约 3.3 choice）。
// 与旧实现一致，胜出 message 仅做浅层对象校验，其字段的完整校验由调用方完成。
func validateTextResponseChoice(element gjson.Result) error {
	targets := []foldTarget{
		{key: "index", policy: nullResetsToZero, validate: validateExactInt},
		{key: "finish_reason", policy: nullIsNoOp, validate: validateString},
		{key: "message", policy: nullResetsToZero, validate: validateObject, deepValidate: validateTextResponseMessage},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	collectErr := foldCollect(element, targets, selectionsBuf[:len(targets)])
	if collectErr == nil {
		return nil
	}
	fields := [...]string{"index", "finish_reason", "message"}
	for i := range targets {
		if _, _, e := foldMatch(element, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
			return fmt.Errorf("choices.%s: %w", fields[i], e)
		}
	}
	return fmt.Errorf("choices.message: %w", collectErr)
}

// parseTextResponseMessageContent 单次扫描并完整校验 message，同时返回已折叠的 content 结果。
//
// 单次 foldCollect 收集 role/refusal/name/tool_call_id(string, nullIsNoOp)/content/reasoning_content
// (any, nullResetsToZero)/tool_calls(array, nullResetsToZero, deepValidate=validateTextResponseToolCalls)；
// 声明顺序锁定 role -> refusal -> name -> tool_call_id -> content -> reasoning_content -> tool_calls
// 的错误优先级（契约 3.3 message）。返回的 content 即 foldAny(message,"content") 的等价选值。
func parseTextResponseMessageContent(value gjson.Result) (gjson.Result, error) {
	targets := []foldTarget{
		{key: "role", policy: nullIsNoOp, validate: validateString},
		{key: "refusal", policy: nullIsNoOp, validate: validateString},
		{key: "name", policy: nullIsNoOp, validate: validateString},
		{key: "tool_call_id", policy: nullIsNoOp, validate: validateString},
		{key: "content", policy: nullResetsToZero, validate: validateAny},
		{key: "reasoning_content", policy: nullResetsToZero, validate: validateAny},
		{key: "tool_calls", policy: nullResetsToZero, validate: validateArray, deepValidate: validateTextResponseToolCalls},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	collectErr := foldCollect(value, targets, selections)
	if collectErr != nil {
		fields := [...]string{"role", "refusal", "name", "tool_call_id", "content", "reasoning_content", "tool_calls"}
		for i := range targets {
			if _, _, e := foldMatch(value, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
				return gjson.Result{}, fmt.Errorf("message.%s: %w", fields[i], e)
			}
		}
		return gjson.Result{}, collectErr
	}
	return selections[4].selected, nil
}

// validateTextResponseMessage 校验 model.Message 目标类型：role/refusal/name/tool_call_id 为
// string（refusal/name 为 *string，null 视为零值），content/reasoning_content 为 any，
// tool_calls 递归校验为 []model.Tool。复用 parseTextResponseMessageContent 并丢弃 content，
// 从而对胜出 message 只扫描一次。
func validateTextResponseMessage(value gjson.Result) error {
	_, err := parseTextResponseMessageContent(value)
	return err
}

// validateTextResponseToolCalls 把匹配值递归校验为 []model.Tool：null 合法；非数组、
// 非对象元素或任一 tool 字段无法解析即报错。
//
// 每个元素单次 foldCollect 收集 id/type/function；声明顺序锁定错误优先级（契约 3.3 tool call）。
// 与旧实现一致，胜出 function 仅做浅层对象校验，其字段的完整校验由 validateTextResponseFunctionFields
// 在「被跳过的 function 值」路径上完成。
func validateTextResponseToolCalls(value gjson.Result) error {
	if value.Type == gjson.Null {
		return nil
	}
	if !value.IsArray() {
		return fmt.Errorf("expected array, got %s", value.Type)
	}
	targets := []foldTarget{
		{key: "id", policy: nullIsNoOp, validate: validateString},
		{key: "type", policy: nullIsNoOp, validate: validateString},
		{key: "function", policy: nullResetsToZero, validate: validateObject, deepValidate: validateTextResponseFunctionFields},
	}
	// 逐元素循环复用一个栈上定长缓冲：foldCollect 每轮都会写满 [0,len(targets))，复用安全。
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	selections := selectionsBuf[:len(targets)]
	for _, element := range value.Array() {
		if element.Type == gjson.Null {
			continue
		}
		if !element.IsObject() {
			return fmt.Errorf("tool_calls element: expected object or null, got %s", element.Type)
		}
		collectErr := foldCollect(element, targets, selections)
		if collectErr == nil {
			continue
		}
		fields := [...]string{"id", "type", "function"}
		for i := range targets {
			if _, _, e := foldMatch(element, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
				return fmt.Errorf("tool_calls.%s: %w", fields[i], e)
			}
		}
		return fmt.Errorf("tool_calls.function: %w", collectErr)
	}
	return nil
}

// validateTextResponseFunctionFields 校验 C1 tool call function（model.Function）：name/description
// 为 Go string，strict 为 *bool，arguments/parameters 为 any。独立于流式路径共享的
// validateFunctionFields，避免迁移影响模块 B；错误文案与其保持一致。
//
// 单次 foldCollect 收集 name/description(string, nullIsNoOp)/strict(bool, nullResetsToZero)/
// arguments/parameters(any, nullResetsToZero)；声明顺序锁定错误优先级（契约 3.3 function）。
func validateTextResponseFunctionFields(value gjson.Result) error {
	targets := []foldTarget{
		{key: "name", policy: nullIsNoOp, validate: validateString},
		{key: "description", policy: nullIsNoOp, validate: validateString},
		{key: "strict", policy: nullResetsToZero, validate: validateBool},
		{key: "arguments", policy: nullResetsToZero, validate: validateAny},
		{key: "parameters", policy: nullResetsToZero, validate: validateAny},
	}
	var selectionsBuf [foldCollectMaxTargets]foldSelection
	collectErr := foldCollect(value, targets, selectionsBuf[:len(targets)])
	if collectErr == nil {
		return nil
	}
	fields := [...]string{"name", "description", "strict", "arguments", "parameters"}
	for i := range targets {
		if _, _, e := foldMatch(value, targets[i].key, targets[i].policy, targets[i].validate, targets[i].deepValidate); e != nil {
			return fmt.Errorf("tool_calls.function.%s: %w", fields[i], e)
		}
	}
	return fmt.Errorf("tool_calls.function.arguments: %w", collectErr)
}

// contentResultToText 复刻 model.Message.StringContent 的字符串化语义：string 直接返回；
// 数组仅拼接对象元素中 type == "text" 的字符串 text；其余类型产出空串。
// 注意数组元素内的 type/text 沿用旧 map 精确键查找（大小写敏感），与 encoding/json 的
// map[string]any 语义一致。
func contentResultToText(result gjson.Result) string {
	switch result.Type {
	case gjson.String:
		return result.Str
	case gjson.JSON:
		if !result.IsArray() {
			return ""
		}
		var builder strings.Builder
		for _, item := range result.Array() {
			if !item.IsObject() {
				continue
			}
			if item.Get("type").Str != model.ContentTypeText {
				continue
			}
			if text := item.Get("text"); text.Type == gjson.String {
				builder.WriteString(text.Str)
			}
		}
		return builder.String()
	default:
		return ""
	}
}

func Handler(c *gin.Context, resp *http.Response, promptTokens int, modelName string) (*model.ErrorWithStatusCode, *model.Usage) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	// 按需扫描消费字段（契约 6.1）：畸形 JSON / 非法计费 basis / 非法 choices 仍保留
	// `unmarshal_response_body_failed` HTTP 500 出口；非法 details 视为缺失，`null` 整数映射零值。
	// 重复键差异（AC-9）：本路径经 gjson 取 first-wins，旧 typed 解码为 last-wins。
	extraction, err := extractTextResponse(responseBody)
	if err != nil {
		return ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	if extraction.Error != nil {
		return &model.ErrorWithStatusCode{
			Error:      *extraction.Error,
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

	if extraction.Usage.TotalTokens == 0 || (extraction.Usage.PromptTokens == 0 && extraction.Usage.CompletionTokens == 0) {
		completionTokens := 0
		for _, choiceContent := range extraction.ChoiceContents {
			completionTokens += CountTokenText(choiceContent, modelName)
		}
		extraction.Usage = model.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	c.Set(ctxkey.ResponseBody, string(responseBody))
	return nil, &extraction.Usage
}
