package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
	. "github.com/smartystreets/goconvey/convey"
)

func TestBuildStreamResponseBody(t *testing.T) {
	Convey("buildStreamResponseBody assembles valid JSON", t, func() {
		responseText := "Hello!"
		usage := &model.Usage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		}
		modelName := "gpt-4-turbo"

		jsonStr := buildStreamResponseBody(responseText, usage, modelName)

		// Verify it's valid JSON
		var result map[string]interface{}
		err := json.Unmarshal([]byte(jsonStr), &result)
		So(err, ShouldBeNil)

		// Check top-level fields
		So(result["id"], ShouldNotBeEmpty)
		So(result["object"], ShouldEqual, "chat.completion")
		So(result["created"], ShouldNotBeNil)
		So(result["model"], ShouldEqual, modelName)

		// Check choices
		choices, ok := result["choices"].([]interface{})
		So(ok, ShouldBeTrue)
		So(len(choices), ShouldEqual, 1)

		choice, ok := choices[0].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(choice["index"], ShouldEqual, 0)

		message, ok := choice["message"].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(message["role"], ShouldEqual, "assistant")
		So(message["content"], ShouldEqual, responseText)

		finishReason, ok := choice["finish_reason"].(string)
		So(ok, ShouldBeTrue)
		So(finishReason, ShouldEqual, "stop")

		// Check usage
		usageResult, ok := result["usage"].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(usageResult["prompt_tokens"], ShouldEqual, 10)
		So(usageResult["completion_tokens"], ShouldEqual, 20)
		So(usageResult["total_tokens"], ShouldEqual, 30)
	})
}

func TestChatCompletionsStreamResponseBodyAggregatesRealFieldsAndUsageDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-real","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4o-mini","system_fingerprint":"fp_abc","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-real","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4o-mini","system_fingerprint":"fp_abc","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-real","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4o-mini","system_fingerprint":"fp_abc","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"length"}]}`,
		`data: {"id":"chatcmpl-real","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4o-mini","system_fingerprint":"fp_abc","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":7},"completion_tokens_details":{"reasoning_tokens":1}}}`,
		`data: [DONE]`,
	}, "\n")

	body, usage, rawStream := doOpenAIStreamResponseWithRaw(t, stream, "fallback-model", 10)

	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 2 || usage.TotalTokens != 12 {
		t.Fatalf("unexpected billing usage: %#v", usage)
	}
	if body["id"] != "chatcmpl-real" {
		t.Fatalf("expected real id, got %#v", body["id"])
	}
	if body["object"] != "chat.completion" {
		t.Fatalf("expected final chat.completion object, got %#v", body["object"])
	}
	if body["created"].(float64) != 1710000000 {
		t.Fatalf("expected real created, got %#v", body["created"])
	}
	if body["model"] != "gpt-4o-mini" {
		t.Fatalf("expected real model, got %#v", body["model"])
	}
	if body["system_fingerprint"] != "fp_abc" {
		t.Fatalf("expected system_fingerprint, got %#v", body["system_fingerprint"])
	}
	choice := body["choices"].([]interface{})[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	if message["role"] != "assistant" || message["content"] != "Hello world" {
		t.Fatalf("unexpected message: %#v", message)
	}
	if choice["finish_reason"] != "length" {
		t.Fatalf("expected real finish_reason, got %#v", choice["finish_reason"])
	}
	usageBody := body["usage"].(map[string]interface{})
	if usageBody["prompt_tokens"].(float64) != 10 || usageBody["completion_tokens"].(float64) != 2 || usageBody["total_tokens"].(float64) != 12 {
		t.Fatalf("unexpected response usage: %#v", usageBody)
	}
	promptDetails := usageBody["prompt_tokens_details"].(map[string]interface{})
	if promptDetails["cached_tokens"].(float64) != 7 {
		t.Fatalf("expected cached_tokens detail, got %#v", promptDetails)
	}
	if !strings.Contains(rawStream, `"prompt_tokens_details":{"cached_tokens":7}`) {
		t.Fatalf("expected usage-only chunk forwarded to client, got raw stream: %s", rawStream)
	}
}

func TestChatCompletionsStreamResponseBodyAggregatesToolCallArgumentDeltas(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-tools","object":"chat.completion.chunk","created":1710000001,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-tools","object":"chat.completion.chunk","created":1710000001,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"abc\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")

	body, _ := doOpenAIStreamResponse(t, stream, "gpt-4o", 1)
	choice := body["choices"].([]interface{})[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	toolCalls := message["tool_calls"].([]interface{})
	toolCall := toolCalls[0].(map[string]interface{})
	function := toolCall["function"].(map[string]interface{})
	if toolCall["id"] != "call_1" || toolCall["type"] != "function" || function["name"] != "lookup" {
		t.Fatalf("unexpected tool call metadata: %#v", toolCall)
	}
	if function["arguments"] != `{"q":"abc"}` {
		t.Fatalf("unexpected aggregated arguments: %#v", function["arguments"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("expected tool_calls finish_reason, got %#v", choice["finish_reason"])
	}
}

func TestChatCompletionsStreamResponseBodyAggregatesFunctionCallDeltas(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-function","object":"chat.completion.chunk","created":1710000004,"model":"gpt-4-0613","choices":[{"index":0,"delta":{"role":"assistant","function_call":{"name":"lookup_weather","arguments":"{\"city\":"}},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-function","object":"chat.completion.chunk","created":1710000004,"model":"gpt-4-0613","choices":[{"index":0,"delta":{"function_call":{"arguments":"\"Paris\"}"}},"finish_reason":"function_call"}]}`,
		`data: [DONE]`,
	}, "\n")

	body, _ := doOpenAIStreamResponse(t, stream, "gpt-4-0613", 1)
	choice := body["choices"].([]interface{})[0].(map[string]interface{})
	message := choice["message"].(map[string]interface{})
	functionCall := message["function_call"].(map[string]interface{})
	if functionCall["name"] != "lookup_weather" {
		t.Fatalf("expected function_call name, got %#v", functionCall)
	}
	if functionCall["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("unexpected aggregated arguments: %#v", functionCall["arguments"])
	}
	if choice["finish_reason"] != "function_call" {
		t.Fatalf("expected function_call finish_reason, got %#v", choice["finish_reason"])
	}
}

func TestChatCompletionsStreamResponseBodyAggregatesMultipleChoicesByIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-multi","object":"chat.completion.chunk","created":1710000002,"model":"gpt-4o","choices":[{"index":1,"delta":{"role":"assistant","content":"B1"},"finish_reason":null},{"index":0,"delta":{"role":"assistant","content":"A1"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-multi","object":"chat.completion.chunk","created":1710000002,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"A2"},"finish_reason":"stop"},{"index":1,"delta":{"content":"B2"},"finish_reason":"length"}]}`,
		`data: [DONE]`,
	}, "\n")

	body, _ := doOpenAIStreamResponse(t, stream, "gpt-4o", 1)
	choices := body["choices"].([]interface{})
	first := choices[0].(map[string]interface{})
	second := choices[1].(map[string]interface{})
	if first["index"].(float64) != 0 || first["message"].(map[string]interface{})["content"] != "A1A2" || first["finish_reason"] != "stop" {
		t.Fatalf("unexpected first choice: %#v", first)
	}
	if second["index"].(float64) != 1 || second["message"].(map[string]interface{})["content"] != "B1B2" || second["finish_reason"] != "length" {
		t.Fatalf("unexpected second choice: %#v", second)
	}
}

func TestChatCompletionsStreamResponseBodyFallsBackToEstimatedUsageWithoutUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-no-usage","object":"chat.completion.chunk","created":1710000003,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n")

	body, usage := doOpenAIStreamResponse(t, stream, "gpt-4o", 5)
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens <= 0 || usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		t.Fatalf("expected estimated billing usage, got %#v", usage)
	}
	usageBody := body["usage"].(map[string]interface{})
	if usageBody["prompt_tokens"].(float64) != float64(usage.PromptTokens) || usageBody["completion_tokens"].(float64) != float64(usage.CompletionTokens) || usageBody["total_tokens"].(float64) != float64(usage.TotalTokens) {
		t.Fatalf("expected fallback usage in response body, got body=%#v usage=%#v", usageBody, usage)
	}
}

// TestChatStreamAccumulatorBuildResponseBodyByteBaseline 冻结改造前
// chatStreamAccumulator.buildResponseBody() 的精确字节输出（AC-6 字节基线）。
//
// 用途：任务 4.2 会把 addPayload 的多次 map 解码改造为单次扫描，本测试用于验证改造
// 前后 buildResponseBody() 的输出逐字节一致（仅语义相等的 JSON 相等不足以证明无回归）。
//
// 说明：want 的字节序由 encoding/json 对 map 键的字典序排序决定，choices/tool_calls
// 的内部顺序由 buildChoices/buildToolCalls 中的 sort.Ints 保证，因此输出完全确定。
func TestChatStreamAccumulatorBuildResponseBodyByteBaseline(t *testing.T) {
	// 有序、确定性的帧序列：覆盖顶层字段、usage 明细、乱序多 choice、role/content
	// 增量、null 与终态 finish_reason、多个 tool_calls 参数分片、legacy function_call。
	frames := []string{
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":2,"delta":{"role":"assistant","content":"C1"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":0,"delta":{"role":"assistant","content":"A1","tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"lookup_b","arguments":"{\"b\":"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":3,"delta":{"role":"assistant","function_call":{"name":"legacy_lookup","arguments":"{\"x\":"}},"finish_reason":null}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":1,"delta":{"role":"assistant","content":"B1"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":0,"delta":{"content":"A2","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup_a","arguments":"{\"a\":"}}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":1,"delta":{"content":"B2"},"finish_reason":"length"}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":2,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":3,"delta":{"function_call":{"arguments":"1}"}},"finish_reason":"function_call"}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"bee\"}"}},{"index":0,"function":{"arguments":"\"aye\"}"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-baseline","object":"chat.completion.chunk","created":1710000099,"model":"gpt-4o-mini","system_fingerprint":"fp_baseline_001","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
	}

	acc := newChatStreamAccumulator()
	for _, frame := range frames {
		if _, err := acc.addPayload([]byte(frame)); err != nil {
			t.Fatalf("valid frame must not error: %v (frame=%s)", err, frame)
		}
	}
	got := acc.buildResponseBody()

	// 改造前 buildResponseBody() 的真实字节输出（实测捕获，非手算）。
	want := `{"choices":[{"finish_reason":"tool_calls","index":0,"message":{"content":"A1A2","role":"assistant","tool_calls":[{"function":{"arguments":"{\"a\":\"aye\"}","name":"lookup_a"},"id":"call_a","index":0,"type":"function"},{"function":{"arguments":"{\"b\":\"bee\"}","name":"lookup_b"},"id":"call_b","index":1,"type":"function"}]}},{"finish_reason":"length","index":1,"message":{"content":"B1B2","role":"assistant"}},{"finish_reason":"stop","index":2,"message":{"content":"C1","role":"assistant"}},{"finish_reason":"function_call","index":3,"message":{"content":"","function_call":{"arguments":"{\"x\":1}","name":"legacy_lookup"},"role":"assistant"}}],"created":1710000099,"id":"chatcmpl-baseline","model":"gpt-4o-mini","object":"chat.completion","system_fingerprint":"fp_baseline_001","usage":{"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":2},"prompt_tokens":11,"prompt_tokens_details":{"cached_tokens":3},"total_tokens":18}}`

	if got != want {
		t.Fatalf("buildResponseBody byte baseline mismatch:\nwant=%q\n got=%q", want, got)
	}

	// 期望的 JSON 必须仍然可解析。
	if !json.Valid([]byte(got)) {
		t.Fatalf("baseline output is not valid JSON: %q", got)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("baseline output unmarshal failed: %v", err)
	}
}

// TestChatCompletionsStreamForwardsMalformedFrameWithoutValues 验证畸形帧被透传但不贡献任何派生值。
// 契约 4.2 Test contract：malformed payloads are forwarded but contribute no values。
func TestChatCompletionsStreamForwardsMalformedFrameWithoutValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	normalFrame := `data: {"id":"chatcmpl-malformed","object":"chat.completion.chunk","created":1710000200,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":"stop"}]}`
	malformedFrame := `data: {"choices":[`
	stream := strings.Join([]string{normalFrame, malformedFrame, `data: [DONE]`}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage, responseBody := StreamHandler(c, resp, relaymode.ChatCompletions)
	if err != nil {
		t.Fatalf("StreamHandler returned error: %+v", err)
	}
	if responseText != "Hello" {
		t.Fatalf("expected only normal frame content, got %q", responseText)
	}
	if usage != nil {
		t.Fatalf("expected nil usage without usage frame, got %#v", usage)
	}
	forwarded := recorder.Body.String()
	if !strings.Contains(forwarded, normalFrame) {
		t.Fatalf("expected normal frame forwarded, got %q", forwarded)
	}
	if !strings.Contains(forwarded, malformedFrame) {
		t.Fatalf("expected malformed frame forwarded, got %q", forwarded)
	}
	if !strings.Contains(forwarded, "[DONE]") {
		t.Fatalf("expected [DONE] forwarded, got %q", forwarded)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(responseBody), &body); err != nil {
		t.Fatalf("response body must be valid JSON: %v (body=%s)", err, responseBody)
	}
	choice := body["choices"].([]interface{})[0].(map[string]interface{})
	if choice["message"].(map[string]interface{})["content"] != "Hello" {
		t.Fatalf("expected malformed frame not to pollute accumulation, got body=%s", responseBody)
	}
}

// TestChatCompletionsStreamAzureEmptyFrameSuppression 验证 Azure 空帧抑制规则保持不变：
// choices 为空且无 usage 的帧不转发；choices 为空但带 usage 的帧被转发。
func TestChatCompletionsStreamAzureEmptyFrameSuppression(t *testing.T) {
	gin.SetMode(gin.TestMode)
	emptyFrame := `data: {"id":"x","choices":[]}`
	usageFrame := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}`
	stream := strings.Join([]string{emptyFrame, usageFrame, `data: [DONE]`}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, _, usage, _ := StreamHandler(c, resp, relaymode.ChatCompletions)
	if err != nil {
		t.Fatalf("StreamHandler returned error: %+v", err)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.TotalTokens != 3 {
		t.Fatalf("expected usage frame to be observed, got %#v", usage)
	}
	forwarded := recorder.Body.String()
	if strings.Contains(forwarded, `"id":"x"`) {
		t.Fatalf("expected empty no-usage frame suppressed, got %q", forwarded)
	}
	if !strings.Contains(forwarded, usageFrame) {
		t.Fatalf("expected empty usage frame forwarded, got %q", forwarded)
	}
}

// TestChatCompletionsStreamKeepsTextFromNonFiniteUnknownFieldFrame 锁定 StreamHandler 层回归：
// B 类帧（typed 成功、map 失败，如 unknown 字段含 1e400）不得因「跳过累积」而丢失其文本。
// 旧两步实现中 typed 提取的 responseText 与 usage 均保留，本测试确保 StreamHandler 的
// responseText 仍包含该帧内容（否则 completion tokens 预估偏低 → 计费回归）。
func TestChatCompletionsStreamKeepsTextFromNonFiniteUnknownFieldFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frame := `data: {"unknown":1e400,"choices":[{"index":0,"delta":{"content":"hello"}}]}`
	stream := strings.Join([]string{frame, `data: [DONE]`}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, _, _ := StreamHandler(c, resp, relaymode.ChatCompletions)
	if err != nil {
		t.Fatalf("StreamHandler returned error: %+v", err)
	}
	if responseText != "hello" {
		t.Fatalf("B 类帧文本必须保留在 responseText，got %q", responseText)
	}
	if !strings.Contains(recorder.Body.String(), frame) {
		t.Fatalf("B 类帧必须被转发，got %q", recorder.Body.String())
	}
}

func doOpenAIStreamResponse(t *testing.T, stream string, actualModelName string, promptTokens int) (map[string]interface{}, *model.Usage) {
	body, usage, _ := doOpenAIStreamResponseWithRaw(t, stream, actualModelName, promptTokens)
	return body, usage
}

func doOpenAIStreamResponseWithRaw(t *testing.T, stream string, actualModelName string, promptTokens int) (map[string]interface{}, *model.Usage, string) {
	t.Helper()
	originalApproximateTokenEnabled := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() {
		config.ApproximateTokenEnabled = originalApproximateTokenEnabled
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
	usage, err := (&Adaptor{}).DoResponse(c, resp, &meta.Meta{
		IsStream:        true,
		Mode:            relaymode.ChatCompletions,
		ActualModelName: actualModelName,
		PromptTokens:    promptTokens,
	})
	if err != nil {
		t.Fatalf("DoResponse returned error: %+v", err)
	}
	rawBody := c.GetString(ctxkey.ResponseBody)
	if rawBody == "" {
		t.Fatalf("expected response body in context")
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(rawBody), &body); err != nil {
		t.Fatalf("unmarshal response body: %v; raw=%s", err, rawBody)
	}
	return body, usage, recorder.Body.String()
}

// TestChatStreamAccumulatorTypeStrictnessMatchesTypedDecode 锁定 addPayload 的类型严格性与旧
// 「typed 解码 + map 累积」两步组合的等价性：
//   - typed 结构体中声明了具体类型的字段 → 严格（存在且类型不符即 err）；
//   - any 类型字段与结构体中不存在的字段 → 宽松（忽略、不报错）。
//
// 必须接受的帧不得返回 err；必须拒绝的帧返回 err 且不得触碰累积器（原子性）。
func TestChatStreamAccumulatorTypeStrictnessMatchesTypedDecode(t *testing.T) {
	bodyHasKey := func(body, key string) bool {
		if body == "" {
			// buildResponseBody 在无 choices 且无 usage 时返回空串，视为不含任何键。
			return false
		}
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("accumulated body must be valid JSON: %v (body=%q)", err, body)
		}
		_, ok := parsed[key]
		return ok
	}

	acceptCases := []struct {
		name  string
		frame string
		check func(t *testing.T, result chatStreamPayloadResult, body string)
	}{
		{
			// 缺口 1：system_fingerprint 非 typed 字段，非 string 时忽略且不报错；不捕获即不出现在 body。
			name:  "system_fingerprint_number_is_loose",
			frame: `{"system_fingerprint":123,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				var parsed map[string]interface{}
				if err := json.Unmarshal([]byte(body), &parsed); err != nil {
					t.Fatalf("body must be valid JSON: %v (body=%q)", err, body)
				}
				choice := parsed["choices"].([]interface{})[0].(map[string]interface{})
				if choice["message"].(map[string]interface{})["content"] != "c" {
					t.Fatalf("expected content %q accumulated, got body=%s", "c", body)
				}
				if bodyHasKey(body, "system_fingerprint") {
					t.Fatalf("non-string system_fingerprint must not be captured, got body=%s", body)
				}
			},
		},
		{
			name:  "system_fingerprint_null_is_loose",
			frame: `{"system_fingerprint":null,"choices":[]}`,
		},
		{
			name:  "system_fingerprint_object_is_loose",
			frame: `{"system_fingerprint":{"a":1},"choices":[]}`,
		},
		{
			name:  "delta_refusal_and_name_null_are_typed_ok",
			frame: `{"choices":[{"index":0,"delta":{"refusal":null,"name":null}}]}`,
		},
		{
			name:  "function_strict_false_is_typed_ok",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"strict":false}}]}}]}`,
		},
		{
			name:  "function_strict_null_is_typed_ok",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"strict":null}}]}}]}`,
		},
		{
			// usage 区域含非有限数属 B 类帧：typed 成功 → err==nil 且 Usage 非 nil；map 失败 → 不累积。
			name:  "usage_non_finite_unknown_field_skips_accumulation",
			frame: `{"choices":[],"usage":{"prompt_tokens":1,"x":1e400}}`,
			check: func(t *testing.T, result chatStreamPayloadResult, body string) {
				if result.Usage == nil || result.Usage.PromptTokens != 1 {
					t.Fatalf("B 类帧必须保留 typed 提取的 usage，got %#v", result.Usage)
				}
				if body != "" {
					t.Fatalf("B 类帧必须跳过累积（body 为空），got %q", body)
				}
			},
		},
	}

	for _, tc := range acceptCases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(tc.frame))
			if err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}
			if tc.check != nil {
				tc.check(t, result, acc.buildResponseBody())
			}
		})
	}

	rejectCases := []struct {
		name  string
		frame string
	}{
		{"delta_refusal_number_rejected", `{"choices":[{"index":0,"delta":{"refusal":123}}]}`},
		{"delta_name_number_rejected", `{"choices":[{"index":0,"delta":{"name":123}}]}`},
		{"delta_tool_call_id_number_rejected", `{"choices":[{"index":0,"delta":{"tool_call_id":123}}]}`},
		{"function_description_number_rejected", `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"description":123}}]}}]}`},
		{"function_strict_number_rejected", `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"strict":123}}]}}]}`},
		// 注：usage 区域含非有限数（如 {"prompt_tokens":1,"x":1e400}）属 B 类帧（typed 成功、map 失败），
		// 行为是「保留派生值、仅跳过累积」而非整帧拒绝，其断言由
		// TestChatStreamAccumulatorNonFiniteNumbersSkipAccumulation 覆盖。
		// detail 类型不符：旧 typed 解码整帧失败 → 整帧拒绝（不再降级为「detail 视为缺失」）。
		// 该断言由上一轮的 err==nil 修正为 err!=nil，以恢复旧 typed 等价性并保留旧计费行为（不扣费）。
		{"usage_detail_wrong_type_rejected", `{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":"x"}}`},
	}

	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			// 预置一个有效帧，使「校验失败不触碰累积器」的断言具有可观测的基线状态。
			seed := `{"id":"seed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"seed"},"finish_reason":null}]}`
			if _, err := acc.addPayload([]byte(seed)); err != nil {
				t.Fatalf("seed frame must be accepted: %v", err)
			}
			before := acc.buildResponseBody()
			if _, err := acc.addPayload([]byte(tc.frame)); err == nil {
				t.Fatalf("frame must be rejected, got err=nil (frame=%s)", tc.frame)
			}
			after := acc.buildResponseBody()
			if before != after {
				t.Fatalf("rejected frame must not touch accumulator:\nbefore=%q\n after=%q", before, after)
			}
		})
	}
}

// TestChatStreamAccumulatorNonFiniteNumbersSkipAccumulation 锁定「超出 float64 范围的数字」的三分类行为，
// 与旧「typed 解码 + map 累积」两步实现的语义逐例对齐：
//
//	A 类：typed 解码失败（类型化字段承载该数字）→ 整帧拒绝，不产出任何派生值；
//	B 类：typed 成功、map 解码失败（数字落在宽松/未知字段）→ 保留 typed 提取的文本与 usage，仅跳过累积；
//	C 类：两者都成功（有限数或下溢到 0 的数）→ 正常累积。
//
// 背景：旧实现先 typed 解码，成功后才 json.Unmarshal 到 map[string]any 做累积；map 解码失败即
// 直接 return，该帧「不累积但保留派生值」。新路径不物化 map，故以 hasUnrepresentableNumber
// 显式检测「是否跳过累积」，保持累积 body 与旧路径逐例等价（spec: Reconstructed output SHALL be unchanged）。
func TestChatStreamAccumulatorNonFiniteNumbersSkipAccumulation(t *testing.T) {
	seed := `{"id":"seed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"seed"},"finish_reason":null}]}`
	newSeededAccumulator := func(t *testing.T) (*chatStreamAccumulator, string) {
		t.Helper()
		acc := newChatStreamAccumulator()
		if _, err := acc.addPayload([]byte(seed)); err != nil {
			t.Fatalf("seed frame must be accepted: %v", err)
		}
		return acc, acc.buildResponseBody()
	}

	// B 类：err==nil（派生值保留），但累积器逐字节不变。
	bClassFrames := []struct {
		name  string
		frame string
		check func(t *testing.T, result chatStreamPayloadResult)
	}{
		{
			name:  "unknown_top_level_with_content",
			frame: `{"unknown":1e400,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.ResponseText != "c" || result.ChoiceCount != 1 {
					t.Fatalf("B 类帧必须保留文本与 choice 计数，got %#v", result)
				}
			},
		},
		{
			name:  "unknown_negative_top_level_empty_choices",
			frame: `{"unknown":-1e400,"choices":[]}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.ChoiceCount != 0 || result.Usage != nil {
					t.Fatalf("B 类空帧不得产出 usage/choice，got %#v", result)
				}
			},
		},
		{
			name:  "unknown_1e309_top_level_empty_choices",
			frame: `{"unknown":1e309,"choices":[]}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.ChoiceCount != 0 || result.Usage != nil {
					t.Fatalf("B 类空帧不得产出 usage/choice，got %#v", result)
				}
			},
		},
		{
			name:  "system_fingerprint_non_finite_with_content",
			frame: `{"system_fingerprint":1e400,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.ResponseText != "c" {
					t.Fatalf("B 类帧必须保留文本，got %#v", result)
				}
			},
		},
		{
			name:  "usage_unknown_field_non_finite",
			frame: `{"choices":[],"usage":{"prompt_tokens":1,"x":1e400}}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.Usage == nil || result.Usage.PromptTokens != 1 {
					t.Fatalf("B 类帧必须保留 typed 提取的 usage，got %#v", result.Usage)
				}
			},
		},
		{
			name:  "nested_deep_non_finite_with_content",
			frame: `{"a":{"b":{"c":[1,2,{"d":1e400}]}},"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.ResponseText != "c" {
					t.Fatalf("B 类帧必须保留文本，got %#v", result)
				}
			},
		},
		{
			name:  "unknown_delta_field_non_finite_with_content",
			frame: `{"choices":[{"index":0,"delta":{"zzz":1e400,"content":"c"}}]}`,
			check: func(t *testing.T, result chatStreamPayloadResult) {
				if result.ResponseText != "c" {
					t.Fatalf("B 类帧必须保留文本，got %#v", result)
				}
			},
		},
		{
			name:  "loose_function_call_arguments_non_finite",
			frame: `{"choices":[{"index":0,"delta":{"function_call":{"arguments":1e400}}}]}`,
		},
	}
	for _, tc := range bClassFrames {
		t.Run("B/"+tc.name, func(t *testing.T) {
			acc, before := newSeededAccumulator(t)
			result, err := acc.addPayload([]byte(tc.frame))
			if err != nil {
				t.Fatalf("B 类帧必须被接受（仅跳过累积），got err=%v (frame=%s)", err, tc.frame)
			}
			if after := acc.buildResponseBody(); after != before {
				t.Fatalf("B 类帧必须不触碰累积器:\nbefore=%q\n after=%q", before, after)
			}
			if tc.check != nil {
				tc.check(t, result)
			}
		})
	}

	// A 类：typed 字段承载该数字 → 整帧拒绝（err != nil）。
	// content/reasoning_content/tool_calls.function.arguments/parameters 在旧 typed 结构体中为
	// any（model.Message.Content/ReasoningContent、model.Function.Arguments/Parameters），
	// encoding/json 解码到 any 时遇到超出 float64 范围的数字同样整帧失败 → 属 A 类。
	aClassFrames := []string{
		`{"choices":[{"index":0,"delta":{"content":1e400}}]}`,
		`{"choices":[{"index":0,"delta":{"content":[1e400]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":{"a":1e400}}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":1e400}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":1e400}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"parameters":1e400}}]}}]}`,
		`{"id":1e400,"choices":[]}`,
		`{"choices":[],"usage":{"prompt_tokens":1e400}}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":1e400}}}`,
	}
	for _, frame := range aClassFrames {
		t.Run("A/"+frame, func(t *testing.T) {
			acc, before := newSeededAccumulator(t)
			if _, err := acc.addPayload([]byte(frame)); err == nil {
				t.Fatalf("A 类帧必须被整帧拒绝，got err=nil (frame=%s)", frame)
			}
			if after := acc.buildResponseBody(); after != before {
				t.Fatalf("被拒帧不得触碰累积器:\nbefore=%q\n after=%q", before, after)
			}
		})
	}

	// C 类：有限/可表示（含下溢到 0）的数字 → err==nil 且正常累积。
	cClassFrames := []string{
		`{"unknown":1e308,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
		`{"unknown":1e-400,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
		`{"unknown":1e-324,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
		`{"unknown":123456789012345678901234567890123456789012345678901234567890,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
	}
	for _, frame := range cClassFrames {
		t.Run("C/"+frame, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(frame))
			if err != nil {
				t.Fatalf("C 类帧必须被接受，got err=%v (frame=%s)", err, frame)
			}
			if result.ResponseText != "c" {
				t.Fatalf("C 类帧必须产出文本，got %#v", result)
			}
			body := acc.buildResponseBody()
			if body == "" || !strings.Contains(body, `"content":"c"`) {
				t.Fatalf("C 类帧必须正常累积，got body=%q", body)
			}
		})
	}

	// C 类（any 字段承载有限数 1e308）：可表示 → 正常累积，body 非空但不含文本 "c"。
	cClassAnyFieldFrames := []string{
		`{"choices":[{"index":0,"delta":{"content":1e308}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":1e308}}]}}]}`,
	}
	for _, frame := range cClassAnyFieldFrames {
		t.Run("C-any/"+frame, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(frame))
			if err != nil {
				t.Fatalf("C 类帧必须被接受，got err=%v (frame=%s)", err, frame)
			}
			if result.ResponseText != "" {
				t.Fatalf("C 类有限数帧不得产出文本，got %#v", result)
			}
			if body := acc.buildResponseBody(); body == "" {
				t.Fatalf("C 类帧必须正常累积（body 非空），got body=%q", body)
			}
		})
	}
}

// TestChatStreamAccumulatorUsageDetailTypeMismatchRejectsFrame 锁定 usage detail 类型不符时整帧拒绝。
//
// 背景：旧 typed 解码中 detail 字段为 *PromptTokensDetails/*CompletionTokensDetails，类型不符即整帧
// 解码失败 → StreamHandler 走 error 分支 continue → usage 保持 nil → 不扣费。新路径必须保留该行为，
// 以维持「唯一可观测行为变更为 DEC-C2-1」的声明。
func TestChatStreamAccumulatorUsageDetailTypeMismatchRejectsFrame(t *testing.T) {
	rejectFrames := []string{
		`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":"x"}}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":123}}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":[1]}}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens_details":"x"}}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":"x"}}}`,
		// 小数/指数形式：旧 typed int 解码失败。
		`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":1e2}}}`,
	}
	for _, frame := range rejectFrames {
		t.Run("reject/"+frame, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			if _, err := acc.addPayload([]byte(frame)); err == nil {
				t.Fatalf("frame with detail type mismatch must be rejected, got err=nil (frame=%s)", frame)
			}
		})
	}

	// 必须接受：null 视为缺失（nil 指针或 0），未知字段忽略。
	t.Run("accept/detail_null_is_missing", func(t *testing.T) {
		acc := newChatStreamAccumulator()
		if _, err := acc.addPayload([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":null}}`)); err != nil {
			t.Fatalf("null detail must be accepted, got err=%v", err)
		}
	})
	t.Run("accept/detail_field_null_is_zero", func(t *testing.T) {
		acc := newChatStreamAccumulator()
		if _, err := acc.addPayload([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":null}}}`)); err != nil {
			t.Fatalf("null detail field must be accepted, got err=%v", err)
		}
	})
	t.Run("accept/detail_known_and_unknown_fields", func(t *testing.T) {
		acc := newChatStreamAccumulator()
		if _, err := acc.addPayload([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":7,"unknown_field":"x"}}}`)); err != nil {
			t.Fatalf("known+unknown detail fields must be accepted, got err=%v", err)
		}
		// 累积 body 中的 usage 为原始 map（旧行为：未知字段原样保留），此处仅验证已知字段被正确读取。
		body := acc.buildResponseBody()
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("body must be valid JSON: %v (body=%q)", err, body)
		}
		details := parsed["usage"].(map[string]interface{})["prompt_tokens_details"].(map[string]interface{})
		if details["cached_tokens"].(float64) != 7 {
			t.Fatalf("expected cached_tokens=7, got %#v", details)
		}
	})
}

// TestChatStreamAccumulatorDuplicateKeysFirstWins 锁定重复键的取值语义（契约 AC-9 声明的 sanctioned exception）。
//
// 背景：契约 AC-9 规定「Duplicate keys SHALL be resolved deterministically and documented」。旧实现
// （typed 解码 + map 累积）依赖 encoding/json，对重复键取「最后一个」（last-wins）；新实现单次扫描对
// **字节（解码后）完全相同**的重复键取「第一个」（first-wins）。本测试以 first-wins 生效值为断言期望。
//
// 类型校验覆盖所有匹配键（含被 first-wins 跳过的重复键，契约 4.2 Step 3）：任一匹配键类型不符即整帧
// 失败，对齐 encoding/json「任一匹配字段不可解析 → 整体 err」。故以下两类帧必须被拒绝（与旧 typed
// 解码一致，且由 encoding/json 实测确认 err!=nil）：
//   - `{"usage":{"prompt_tokens":5,"prompt_tokens":1e400}}`：第二个键含超出 float64 范围的数字，
//     旧 last-wins 取 1e400 → typed 解码失败 → 整帧被拒；新实现同样整帧拒绝。
//   - `{"id":"x","id":1e400,"choices":[]}`：旧 last-wins 取 1e400 → typed 解码失败 → 整帧被拒；
//     新实现同样整帧拒绝。
func TestChatStreamAccumulatorDuplicateKeysFirstWins(t *testing.T) {
	// 注：以下各例的旧行为（last-wins）均经临时探针以 encoding/json 解码到等价 typed 结构实测确认；
	// 断言值取新实现的 first-wins 生效值（wantErr 例除外，见上方说明）。
	cases := []struct {
		name    string
		frame   string
		wantErr bool
		check   func(t *testing.T, acc *chatStreamAccumulator, result chatStreamPayloadResult, body string)
	}{
		{
			// 旧 last-wins：prompt_tokens=2；新 first-wins：prompt_tokens=1。
			name:  "usage_prompt_tokens",
			frame: `{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens":2}}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, result chatStreamPayloadResult, body string) {
				if result.Usage == nil || result.Usage.PromptTokens != 1 {
					t.Fatalf("first-wins 期望 prompt_tokens=1，got %#v", result.Usage)
				}
				if !strings.Contains(body, `"prompt_tokens":1`) {
					t.Fatalf("累积 body 应含 first-wins 的 prompt_tokens=1，got body=%q", body)
				}
			},
		},
		{
			// 类型校验覆盖所有匹配键：第二个重复键含 1e400（超出 float64 范围），旧 typed 解码失败 →
			// 整帧拒绝；新实现同样整帧拒绝（first-wins 不豁免被跳过键的类型校验）。
			name:    "usage_prompt_tokens_second_is_non_finite",
			frame:   `{"choices":[],"usage":{"prompt_tokens":5,"prompt_tokens":1e400}}`,
			wantErr: true,
		},
		{
			// 旧 last-wins：cached_tokens=2；新 first-wins：cached_tokens=1。
			name:  "usage_prompt_tokens_details_cached_tokens",
			frame: `{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":1,"cached_tokens":2}}}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, result chatStreamPayloadResult, body string) {
				if result.Usage == nil || result.Usage.PromptTokensDetails == nil || result.Usage.PromptTokensDetails.CachedTokens != 1 {
					t.Fatalf("first-wins 期望 cached_tokens=1，got %#v", result.Usage)
				}
				if !strings.Contains(body, `"cached_tokens":1`) {
					t.Fatalf("累积 body 应含 first-wins 的 cached_tokens=1，got body=%q", body)
				}
			},
		},
		{
			// 旧 last-wins：id="b"；新 first-wins：id="a"。
			// 注：该帧 choices 为空且无 usage，buildResponseBody() 按 Azure 抑制规则返回空串，
			// 故此处直接断言累积器内部 first-wins 取值（acc.id）。
			name:  "top_level_id",
			frame: `{"id":"a","id":"b","choices":[]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, _ chatStreamPayloadResult, body string) {
				if acc.id != "a" {
					t.Fatalf("first-wins 期望 acc.id=\"a\"，got %q", acc.id)
				}
				if body != "" {
					t.Fatalf("无 choices 且无 usage 的帧必须被抑制（body 为空），got body=%q", body)
				}
			},
		},
		{
			// 类型校验覆盖所有匹配键：第二个重复键 1e400 非 string，旧 typed 解码失败 → 整帧拒绝；
			// 新实现同样整帧拒绝（first-wins 不豁免被跳过键的类型校验）。
			name:    "top_level_id_second_is_non_finite",
			frame:   `{"id":"x","id":1e400,"choices":[]}`,
			wantErr: true,
		},
		{
			// 旧 last-wins：content="b"；新 first-wins：content="a"。
			name:  "delta_content",
			frame: `{"choices":[{"index":0,"delta":{"content":"a","content":"b"}}]}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, result chatStreamPayloadResult, body string) {
				if result.ResponseText != "a" {
					t.Fatalf("first-wins 期望 ResponseText=\"a\"，got %q", result.ResponseText)
				}
				if !strings.Contains(body, `"content":"a"`) {
					t.Fatalf("累积 body 应含 first-wins 的 content=\"a\"，got body=%q", body)
				}
			},
		},
		{
			// 旧 last-wins：choices 取后一个（content="b"）；新 first-wins：choices 取前一个（content="a"）。
			name:  "top_level_choices",
			frame: `{"choices":[{"index":0,"delta":{"content":"a"}}],"choices":[{"index":0,"delta":{"content":"b"}}]}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, result chatStreamPayloadResult, body string) {
				if result.ResponseText != "a" {
					t.Fatalf("first-wins 期望 ResponseText=\"a\"，got %q", result.ResponseText)
				}
				if !strings.Contains(body, `"content":"a"`) {
					t.Fatalf("累积 body 应含 first-wins 的 content=\"a\"，got body=%q", body)
				}
			},
		},
		{
			// 旧 last-wins：created=2；新 first-wins：created=1。
			// 注：该帧 choices 为空且无 usage，buildResponseBody() 返回空串，故断言累积器内部取值（acc.created）。
			name:  "top_level_created",
			frame: `{"created":1,"created":2,"choices":[]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, _ chatStreamPayloadResult, body string) {
				if acc.created != 1 {
					t.Fatalf("first-wins 期望 acc.created=1，got %d", acc.created)
				}
				if body != "" {
					t.Fatalf("无 choices 且无 usage 的帧必须被抑制（body 为空），got body=%q", body)
				}
			},
		},
		{
			// 旧 map 路径取 index=1（last-wins），新 first-wins 取 index=0。
			name:  "tool_call_index",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"index":1,"function":{"arguments":"a"}}]}}]}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, _ chatStreamPayloadResult, body string) {
				if !strings.Contains(body, `"tool_calls":[{"function":{"arguments":"a"},"index":0}]`) {
					t.Fatalf("累积 body 应含 first-wins 的 tool_call index=0，got body=%q", body)
				}
			},
		},
		{
			// 旧 last-wins：finish_reason="length"；新 first-wins：finish_reason="stop"。
			name:  "choice_finish_reason",
			frame: `{"choices":[{"index":0,"delta":{},"finish_reason":"stop","finish_reason":"length"}]}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, _ chatStreamPayloadResult, body string) {
				if !strings.Contains(body, `"finish_reason":"stop"`) {
					t.Fatalf("累积 body 应含 first-wins 的 finish_reason=\"stop\"，got body=%q", body)
				}
			},
		},
		{
			// 旧 last-wins：arguments="y"；新 first-wins：arguments="x"。
			name:  "tool_call_function_arguments",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"x","arguments":"y"}}]}}]}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, _ chatStreamPayloadResult, body string) {
				if !strings.Contains(body, `"arguments":"x"`) {
					t.Fatalf("累积 body 应含 first-wins 的 arguments=\"x\"，got body=%q", body)
				}
			},
		},
		{
			// 旧 last-wins：arguments="y"；新 first-wins：arguments="x"。
			name:  "legacy_function_call_arguments",
			frame: `{"choices":[{"index":0,"delta":{"function_call":{"arguments":"x","arguments":"y"}}}]}`,
			check: func(t *testing.T, _ *chatStreamAccumulator, _ chatStreamPayloadResult, body string) {
				if !strings.Contains(body, `"function_call":{"arguments":"x"}`) {
					t.Fatalf("累积 body 应含 first-wins 的 function_call arguments=\"x\"，got body=%q", body)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(tc.frame))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("含类型不符重复键的帧必须被整帧拒绝（err!=nil），got err=nil (frame=%s)", tc.frame)
				}
				// 被拒帧不得产出派生值，也不得触碰累积器。
				if result.ResponseText != "" || result.Usage != nil || result.ChoiceCount != 0 {
					t.Fatalf("被拒帧不得产出派生值，got %#v (frame=%s)", result, tc.frame)
				}
				if body := acc.buildResponseBody(); body != "" {
					t.Fatalf("被拒帧不得触碰累积器（body 为空），got body=%q", body)
				}
				return
			}
			if err != nil {
				t.Fatalf("first-wins 帧必须被接受（err=nil），got err=%v (frame=%s)", err, tc.frame)
			}
			tc.check(t, acc, result, acc.buildResponseBody())
		})
	}
}

// TestChatStreamAccumulatorTypeBoundaries 为任务 4.2 改造后的帧接口补类型边界测试（契约 4.3）。
//
// 覆盖三条降级规则与相关 typed/宽松字段的边界：
//   - delta.content 为非字符串（bool/number/object/array/null）时不报错且贡献空串，
//     对齐 conv.AsString 语义；含超出 float64 范围数字时仍整帧拒绝（A 类，见
//     TestChatStreamAccumulatorNonFiniteNumbersSkipAccumulation）。
//   - choices 缺失/null 不报错且不产出误导性累积值；非数组整帧拒绝。
//   - usage 缺失/null 时 result.Usage 保持 nil；非对象整帧拒绝。
//   - choices 数组元素 null 跳过但计入 ChoiceCount；非对象非 null 元素整帧拒绝。
//   - finish_reason 缺失/null 合法；非 String 整帧拒绝。
//   - tool_calls[].index 与 system_fingerprint 为非 typed 字段，宽松不报错。
//   - delta.role 为 typed string，缺失/null 合法、非 String 整帧拒绝。
//
// 重复键（duplicate target keys）的 first-wins 语义与文档声明见
// TestChatStreamAccumulatorDuplicateKeysFirstWins（契约 AC-9）。
func TestChatStreamAccumulatorTypeBoundaries(t *testing.T) {
	// seedFrame 用于 wantErr 用例：先累积一个合法帧，再喂入非法帧，验证累积器未被触碰（原子性）。
	seedFrame := `{"id":"seed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"seed"},"finish_reason":null}]}`

	parseBody := func(t *testing.T, name, body string) map[string]interface{} {
		t.Helper()
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("[%s] body must be valid JSON: %v (body=%q)", name, err, body)
		}
		return parsed
	}
	firstChoice := func(t *testing.T, name, body string) map[string]interface{} {
		t.Helper()
		choices, ok := parseBody(t, name, body)["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			t.Fatalf("[%s] body must contain choices, got body=%q", name, body)
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			t.Fatalf("[%s] choices[0] must be an object, got body=%q", name, body)
		}
		return choice
	}
	firstMessage := func(t *testing.T, name, body string) map[string]interface{} {
		t.Helper()
		message, ok := firstChoice(t, name, body)["message"].(map[string]interface{})
		if !ok {
			t.Fatalf("[%s] choice must contain message, got body=%q", name, body)
		}
		return message
	}
	firstToolCallIndex := func(t *testing.T, name, body string) int {
		t.Helper()
		toolCalls, ok := firstMessage(t, name, body)["tool_calls"].([]interface{})
		if !ok || len(toolCalls) == 0 {
			t.Fatalf("[%s] message must contain tool_calls, got body=%q", name, body)
		}
		toolCall, ok := toolCalls[0].(map[string]interface{})
		if !ok {
			t.Fatalf("[%s] tool_calls[0] must be an object, got body=%q", name, body)
		}
		index, ok := toolCall["index"].(float64)
		if !ok {
			t.Fatalf("[%s] tool_calls[0].index must be a number, got body=%q", name, body)
		}
		return int(index)
	}
	// messageContent 返回 choices[0].message.content（累积器对每个 choice 恒写 content 键）。
	messageContent := func(t *testing.T, name, body string) string {
		t.Helper()
		content, ok := firstMessage(t, name, body)["content"].(string)
		if !ok {
			t.Fatalf("[%s] message.content must be a string, got body=%q", name, body)
		}
		return content
	}

	cases := []struct {
		name         string
		body         string
		wantErr      bool
		wantText     string
		wantChoices  int
		usageChecked bool
		wantUsageNil bool
		// wantBodyEmpty 为 true 时断言 buildResponseBody() == ""（Azure 抑制规则）。
		wantBodyEmpty bool
		check         func(t *testing.T, result chatStreamPayloadResult, body string)
	}{
		// A. delta.content 非字符串 → 不报错、贡献空串（Steps 第 1 条）。
		{
			name: "A/content_true", body: `{"choices":[{"index":0,"delta":{"content":true}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_true", body); got != "" {
					t.Fatalf("[A/content_true] non-string content must contribute empty string, got %q (body=%q)", got, body)
				}
			},
		},
		{
			name: "A/content_number", body: `{"choices":[{"index":0,"delta":{"content":123}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_number", body); got != "" {
					t.Fatalf("[A/content_number] non-string content must contribute empty string, got %q (body=%q)", got, body)
				}
			},
		},
		{
			name: "A/content_fraction", body: `{"choices":[{"index":0,"delta":{"content":1.5}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_fraction", body); got != "" {
					t.Fatalf("[A/content_fraction] non-string content must contribute empty string, got %q (body=%q)", got, body)
				}
			},
		},
		{
			name: "A/content_object", body: `{"choices":[{"index":0,"delta":{"content":{"a":1}}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_object", body); got != "" {
					t.Fatalf("[A/content_object] non-string content must contribute empty string, got %q (body=%q)", got, body)
				}
			},
		},
		{
			name: "A/content_array", body: `{"choices":[{"index":0,"delta":{"content":[1,2]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_array", body); got != "" {
					t.Fatalf("[A/content_array] non-string content must contribute empty string, got %q (body=%q)", got, body)
				}
			},
		},
		{
			name: "A/content_null", body: `{"choices":[{"index":0,"delta":{"content":null}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_null", body); got != "" {
					t.Fatalf("[A/content_null] null content must contribute empty string, got %q (body=%q)", got, body)
				}
			},
		},
		{
			// 对照：字符串 content 正常产出文本。
			name: "A/content_string_control", body: `{"choices":[{"index":0,"delta":{"content":"abc"}}]}`,
			wantChoices: 1, wantText: "abc",
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "A/content_string_control", body); got != "abc" {
					t.Fatalf("[A/content_string_control] string content must be accumulated, got %q (body=%q)", got, body)
				}
			},
		},

		// B. choices 缺失 / null / 非数组（Steps 第 2 条）。
		{
			name: "B/choices_missing", body: `{"id":"x"}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{
			name: "B/choices_null", body: `{"id":"x","choices":null}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{name: "B/choices_number", body: `{"choices":123}`, wantErr: true},
		{name: "B/choices_string", body: `{"choices":"x"}`, wantErr: true},
		{name: "B/choices_object", body: `{"choices":{}}`, wantErr: true},
		{name: "B/choices_boolean", body: `{"choices":true}`, wantErr: true},

		// C. usage 缺失 / null / 非对象（Steps 第 3 条）。
		{
			name: "C/usage_missing", body: `{"choices":[]}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{
			name: "C/usage_null", body: `{"choices":[],"usage":null}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{name: "C/usage_string", body: `{"choices":[],"usage":"x"}`, wantErr: true},
		{name: "C/usage_number", body: `{"choices":[],"usage":123}`, wantErr: true},
		{name: "C/usage_array", body: `{"choices":[],"usage":[1]}`, wantErr: true},
		{name: "C/usage_boolean", body: `{"choices":[],"usage":true}`, wantErr: true},
		{
			// 对照：空对象 usage 是合法对象，产出全零的非 nil usage（不被 Azure 抑制）。
			name: "C/usage_empty_object_control", body: `{"choices":[],"usage":{}}`,
			usageChecked: true,
		},

		// D. choices 数组元素 null / 非对象。
		{
			name: "D/choices_single_null_element", body: `{"choices":[null]}`,
			wantChoices: 1, usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{
			name: "D/choices_two_null_elements", body: `{"choices":[null,null]}`,
			wantChoices: 2, usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{name: "D/choices_number_element", body: `{"choices":[123]}`, wantErr: true},
		{name: "D/choices_string_element", body: `{"choices":["x"]}`, wantErr: true},
		{
			name: "D/choices_null_then_object", body: `{"choices":[null,{"index":0,"delta":{"content":"c"}}]}`,
			wantChoices: 2, wantText: "c",
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := messageContent(t, "D/choices_null_then_object", body); got != "c" {
					t.Fatalf("[D/choices_null_then_object] object element must accumulate, got %q (body=%q)", got, body)
				}
			},
		},

		// E. finish_reason 边界。
		{
			name: "E/finish_reason_null", body: `{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if reason := firstChoice(t, "E/finish_reason_null", body)["finish_reason"]; reason != nil {
					t.Fatalf("[E/finish_reason_null] finish_reason must remain null, got %#v (body=%q)", reason, body)
				}
			},
		},
		{
			name: "E/finish_reason_string", body: `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if reason := firstChoice(t, "E/finish_reason_string", body)["finish_reason"]; reason != "stop" {
					t.Fatalf("[E/finish_reason_string] finish_reason must be %q, got %#v (body=%q)", "stop", reason, body)
				}
			},
		},
		{name: "E/finish_reason_number", body: `{"choices":[{"index":0,"delta":{},"finish_reason":123}]}`, wantErr: true},
		{name: "E/finish_reason_object", body: `{"choices":[{"index":0,"delta":{},"finish_reason":{}}]}`, wantErr: true},

		// F. tool_calls[].index 宽松 int 边界（非 typed 字段）。
		{
			name: "F/tool_index_string_becomes_zero", body: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":"3","function":{"arguments":"a"}}]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := firstToolCallIndex(t, "F/tool_index_string_becomes_zero", body); got != 0 {
					t.Fatalf("[F/tool_index_string_becomes_zero] non-number index must become 0, got %d (body=%q)", got, body)
				}
			},
		},
		{
			name: "F/tool_index_missing_becomes_zero", body: `{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"a"}}]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := firstToolCallIndex(t, "F/tool_index_missing_becomes_zero", body); got != 0 {
					t.Fatalf("[F/tool_index_missing_becomes_zero] missing index must become 0, got %d (body=%q)", got, body)
				}
			},
		},
		{
			name: "F/tool_index_null_becomes_zero", body: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":null,"function":{"arguments":"a"}}]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := firstToolCallIndex(t, "F/tool_index_null_becomes_zero", body); got != 0 {
					t.Fatalf("[F/tool_index_null_becomes_zero] null index must become 0, got %d (body=%q)", got, body)
				}
			},
		},
		{
			name: "F/tool_index_positive_fraction_truncates", body: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1.9,"function":{"arguments":"a"}}]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := firstToolCallIndex(t, "F/tool_index_positive_fraction_truncates", body); got != 1 {
					t.Fatalf("[F/tool_index_positive_fraction_truncates] 1.9 must truncate to 1, got %d (body=%q)", got, body)
				}
			},
		},
		{
			name: "F/tool_index_negative_fraction_truncates", body: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":-1.9,"function":{"arguments":"a"}}]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := firstToolCallIndex(t, "F/tool_index_negative_fraction_truncates", body); got != -1 {
					t.Fatalf("[F/tool_index_negative_fraction_truncates] -1.9 must truncate to -1, got %d (body=%q)", got, body)
				}
			},
		},
		{
			name: "F/tool_index_integer_kept", body: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"arguments":"a"}}]}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if got := firstToolCallIndex(t, "F/tool_index_integer_kept", body); got != 2 {
					t.Fatalf("[F/tool_index_integer_kept] index 2 must be kept, got %d (body=%q)", got, body)
				}
			},
		},

		// G. delta.role 边界（typed string）。
		{
			name: "G/role_null", body: `{"choices":[{"index":0,"delta":{"role":null}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if _, ok := firstMessage(t, "G/role_null", body)["role"]; ok {
					t.Fatalf("[G/role_null] null role must not be captured, got body=%q", body)
				}
			},
		},
		{
			name: "G/role_string", body: `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			wantChoices: 1,
			check: func(t *testing.T, _ chatStreamPayloadResult, body string) {
				if role := firstMessage(t, "G/role_string", body)["role"]; role != "assistant" {
					t.Fatalf("[G/role_string] role must be %q, got %#v (body=%q)", "assistant", role, body)
				}
			},
		},
		{name: "G/role_number", body: `{"choices":[{"index":0,"delta":{"role":123}}]}`, wantErr: true},

		// H. system_fingerprint 宽松（非 typed 字段）。
		{
			name: "H/system_fingerprint_number", body: `{"system_fingerprint":123,"choices":[]}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{
			name: "H/system_fingerprint_null", body: `{"system_fingerprint":null,"choices":[]}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
		{
			name: "H/system_fingerprint_object", body: `{"system_fingerprint":{"a":1},"choices":[]}`,
			usageChecked: true, wantUsageNil: true, wantBodyEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr {
				acc := newChatStreamAccumulator()
				if _, err := acc.addPayload([]byte(seedFrame)); err != nil {
					t.Fatalf("[%s] seed frame must be accepted: %v (body=%q)", tc.name, err, tc.body)
				}
				before := acc.buildResponseBody()
				result, err := acc.addPayload([]byte(tc.body))
				if err == nil {
					t.Fatalf("[%s] frame must be rejected, got err=nil (body=%q)", tc.name, tc.body)
				}
				if after := acc.buildResponseBody(); after != before {
					t.Fatalf("[%s] rejected frame must not touch accumulator:\nbefore=%q\n after=%q (body=%q)", tc.name, before, after, tc.body)
				}
				if result.ResponseText != "" || result.Usage != nil || result.ChoiceCount != 0 {
					t.Fatalf("[%s] rejected frame must not produce derived values, got %#v (body=%q)", tc.name, result, tc.body)
				}
				return
			}

			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(tc.body))
			if err != nil {
				t.Fatalf("[%s] frame must be accepted, got err=%v (body=%q)", tc.name, err, tc.body)
			}
			if result.ResponseText != tc.wantText {
				t.Fatalf("[%s] ResponseText=%q, want %q (body=%q)", tc.name, result.ResponseText, tc.wantText, tc.body)
			}
			if result.ChoiceCount != tc.wantChoices {
				t.Fatalf("[%s] ChoiceCount=%d, want %d (body=%q)", tc.name, result.ChoiceCount, tc.wantChoices, tc.body)
			}
			if tc.usageChecked {
				if tc.wantUsageNil && result.Usage != nil {
					t.Fatalf("[%s] Usage must be nil, got %#v (body=%q)", tc.name, result.Usage, tc.body)
				}
				if !tc.wantUsageNil && result.Usage == nil {
					t.Fatalf("[%s] Usage must be non-nil, got nil (body=%q)", tc.name, tc.body)
				}
			}
			body := acc.buildResponseBody()
			if tc.wantBodyEmpty && body != "" {
				t.Fatalf("[%s] buildResponseBody() must be empty, got %q (body=%q)", tc.name, body, tc.body)
			}
			if tc.check != nil {
				tc.check(t, result, body)
			}
		})
	}
}

// TestExtractCompletionsStreamText 覆盖任务 4.4 的 Completions 帧文本提取契约：
// 按文档顺序累加 `choices[].text`；缺失/null choices 返回空串；畸形 JSON 或类型不符的
// `choices`/`choices[].text`/`choices[].finish_reason` 返回错误，使 StreamHandler 保持
// 「跳过累加但已转发」的既有行为。
//
// 重复键（AC-9）：gjson 取「第一个」（first-wins），而旧实现经 encoding/json 取「最后一个」
// （last-wins）。该 sanctioned exception 由 duplicate_keys_first_wins 用例显式锁定。
func TestExtractCompletionsStreamText(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantText string
		wantErr  bool
	}{
		// 正常累加（顺序敏感）。
		{name: "single_choice", payload: `{"choices":[{"text":"Hello"}]}`, wantText: "Hello"},
		{name: "multi_choice_order", payload: `{"choices":[{"text":"A"},{"text":"B"},{"text":"C"}]}`, wantText: "ABC"},
		{name: "empty_choices", payload: `{"choices":[]}`, wantText: ""},
		{name: "missing_choices", payload: `{}`, wantText: ""},
		{name: "null_choices", payload: `{"choices":null}`, wantText: ""},
		{name: "null_root", payload: `null`, wantText: ""},

		// 类型边界（严格 → 报错）。
		{name: "choices_number", payload: `{"choices":123}`, wantErr: true},
		{name: "choices_string", payload: `{"choices":"x"}`, wantErr: true},
		{name: "text_number", payload: `{"choices":[{"text":123}]}`, wantErr: true},
		{name: "text_object", payload: `{"choices":[{"text":{}}]}`, wantErr: true},
		{name: "text_array", payload: `{"choices":[{"text":[1]}]}`, wantErr: true},
		{name: "text_bool", payload: `{"choices":[{"text":true}]}`, wantErr: true},
		{name: "finish_reason_number", payload: `{"choices":[{"finish_reason":123}]}`, wantErr: true},

		// 宽松/零值。
		{name: "text_null", payload: `{"choices":[{"text":null}]}`, wantText: ""},
		{name: "finish_reason_null", payload: `{"choices":[{"finish_reason":null}]}`, wantText: ""},
		{name: "text_empty_string", payload: `{"choices":[{"text":""}]}`, wantText: ""},
		{name: "single_null_element", payload: `{"choices":[null]}`, wantText: ""},
		{name: "null_then_object", payload: `{"choices":[null,{"text":"A"}]}`, wantText: "A"},
		{name: "object_null_object", payload: `{"choices":[{"text":"A"},null,{"text":"B"}]}`, wantText: "AB"},
		{name: "number_element", payload: `{"choices":[{"text":"A"},123,{"text":"B"}]}`, wantErr: true},

		// 畸形。
		{name: "truncated", payload: `{"choices":[`, wantErr: true},
		{name: "trailing_garbage", payload: `{"choices":[{"text":"A"}]} extra`, wantErr: true},
		{name: "empty_payload", payload: ``, wantErr: true},
		{name: "non_object_root", payload: `[1,2]`, wantErr: true},

		// 重复键（AC-9）：gjson first-wins 取 "A"；旧 encoding/json last-wins 取 "B"。
		{name: "duplicate_keys_first_wins", payload: `{"choices":[{"text":"A","text":"B"}]}`, wantText: "A"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotText, err := extractCompletionsStreamText([]byte(tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("[%s] expected error, got nil (payload=%q, text=%q)", tc.name, tc.payload, gotText)
				}
				// 契约：出错时不产出任何文本。
				if gotText != "" {
					t.Fatalf("[%s] error path must return empty text, got %q (payload=%q)", tc.name, gotText, tc.payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("[%s] expected no error, got %v (payload=%q)", tc.name, err, tc.payload)
			}
			if gotText != tc.wantText {
				t.Fatalf("[%s] text=%q, want %q (payload=%q)", tc.name, gotText, tc.wantText, tc.payload)
			}
		})
	}
}

// legacyCompletionsStreamText 是任务 4.4 改造前的旧实现（typed 解码到 CompletionsStreamResponse
// 后按序累加 choice.Text），保留在测试中作为等价性基线；CompletionsStreamResponse 类型被保留正是为此。
func legacyCompletionsStreamText(payload []byte) (string, error) {
	var streamResponse CompletionsStreamResponse
	if err := json.Unmarshal(payload, &streamResponse); err != nil {
		return "", err
	}
	text := ""
	for _, choice := range streamResponse.Choices {
		text += choice.Text
	}
	return text, nil
}

// TestExtractCompletionsStreamTextMatchesLegacyTypedDecode 对同一批 payload 逐例比较新实现与
// 旧 typed 解码实现的 (text, err) 结论，证明改造未改变可观测行为。
//
// 唯一被有意排除的类别是「重复键」：旧 encoding/json 为 last-wins，新 gjson 为 first-wins
// （契约 AC-9 声明的 sanctioned exception），故重复键样本不进入本等价性对照，而由
// TestExtractCompletionsStreamText 的 duplicate_keys_first_wins 用例单独锁定。
func TestExtractCompletionsStreamTextMatchesLegacyTypedDecode(t *testing.T) {
	payloads := []string{
		// 任务明确要求的关键例。
		`{"choices":[{"text":"A"},{"text":"B"}]}`,
		`{"choices":123}`,
		`{"choices":[{"text":123}]}`,
		`{"choices":[{"text":null}]}`,
		`null`,
		`[1,2]`,
		// 其余正常/边界样本。
		`{"choices":[{"text":"Hello"}]}`,
		`{"choices":[]}`,
		`{}`,
		`{"choices":null}`,
		`{"choices":"x"}`,
		`{"choices":[{"text":{}}]}`,
		`{"choices":[{"text":[1]}]}`,
		`{"choices":[{"text":true}]}`,
		`{"choices":[{"finish_reason":123}]}`,
		`{"choices":[{"finish_reason":null}]}`,
		`{"choices":[{"text":""}]}`,
		`{"choices":[null]}`,
		`{"choices":[null,{"text":"A"}]}`,
		`{"choices":[{"text":"A"},null,{"text":"B"}]}`,
		`{"choices":[{"text":"A"},123,{"text":"B"}]}`,
		`{"choices":[`,
		`{"choices":[{"text":"A"}]} extra`,
		``,
		`{"choices":[{"text":"A","finish_reason":"stop"}]}`,
	}

	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			oldText, oldErr := legacyCompletionsStreamText([]byte(payload))
			newText, newErr := extractCompletionsStreamText([]byte(payload))
			if (oldErr == nil) != (newErr == nil) {
				t.Fatalf("error classification mismatch for %q: old err=%v, new err=%v", payload, oldErr, newErr)
			}
			if oldErr != nil {
				// 两侧都失败：不要求错误文本相同，仅要求都拒绝且不产出文本。
				if newText != "" {
					t.Fatalf("rejected payload %q must return empty text, got %q", payload, newText)
				}
				return
			}
			if oldText != newText {
				t.Fatalf("text mismatch for %q: old=%q, new=%q", payload, oldText, newText)
			}
		})
	}
}

// TestCompletionsStreamAccumulatesMultipleChoicesAndForwardsMalformed 覆盖任务 4.4 Steps 第 1、3 条：
// 多 choice、跨帧累加结果与改造前一致；类型非法帧被原样转发但不贡献文本。
func TestCompletionsStreamAccumulatesMultipleChoicesAndForwardsMalformed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	frameA := `data: {"choices":[{"text":"A"},{"text":"B"}]}`
	frameC := `data: {"choices":[{"text":"C"}]}`
	malformedFrame := `data: {"choices":123}`
	doneFrame := `data: [DONE]`
	stream := strings.Join([]string{frameA, frameC, malformedFrame, doneFrame}, "\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}

	err, responseText, usage, responseBody := StreamHandler(c, resp, relaymode.Completions)
	if err != nil {
		t.Fatalf("StreamHandler returned error: %+v", err)
	}
	if responseText != "ABC" {
		t.Fatalf("expected multi-choice/multi-frame accumulation %q, got %q", "ABC", responseText)
	}
	if usage != nil {
		t.Fatalf("expected nil usage for completions path, got %#v", usage)
	}
	if responseBody != "" {
		t.Fatalf("expected empty reconstructed body for completions path, got %q", responseBody)
	}
	// 转发行为：全部 4 行（含非法帧与 [DONE]）逐字节透传。
	forwarded := recorder.Body.String()
	for _, frame := range []string{frameA, frameC, malformedFrame, doneFrame} {
		if !strings.Contains(forwarded, frame) {
			t.Fatalf("expected frame forwarded verbatim %q, got %q", frame, forwarded)
		}
	}
}

// chatStreamCaseInsensitiveMatrix 是「同一帧的大小写变体与规范形式结论一致」的端到端探针。
//
// 背景：旧实现经 encoding/json 解码到 struct，字段名匹配大小写不敏感（先精确、再 Unicode 简单折叠）；
// 单次扫描改造一度退化为 gjson 的大小写敏感 Get，导致 `{"Choices":[...]}` 等合法帧被静默丢弃
// （转发、文本累加、usage 计费三项均发生未声明变更）。本测试锁定修复后的 exact-then-fold 语义：
// 每个规范小写帧与其大小写变体必须产出完全相同的 (转发, responseText, usage, reconstructedBody)。
func TestChatStreamCaseInsensitiveMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	runStream := func(t *testing.T, stream string, relayMode int) (string, string, *model.Usage, string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream))}
		err, responseText, usage, responseBody := StreamHandler(c, resp, relayMode)
		if err != nil {
			t.Fatalf("StreamHandler returned error: %+v", err)
		}
		return recorder.Body.String(), responseText, usage, responseBody
	}
	usageString := func(usage *model.Usage) string {
		if usage == nil {
			return "<nil>"
		}
		return fmt.Sprintf("%d/%d/%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}

	// 每一组给出规范小写帧与等价大小写变体；断言四元组逐项一致。
	chatGroups := []struct {
		name             string
		canonical        string
		variant          string
		skipBodyEquality bool
	}{
		{
			name:      "top_level_and_nested_fields",
			canonical: `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1710000500,"model":"gpt-4o","system_fingerprint":"fp_x","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`,
			variant:   `{"ID":"chatcmpl-x","OBJECT":"chat.completion.chunk","CREATED":1710000500,"MODEL":"gpt-4o","SYSTEM_FINGERPRINT":"fp_x","CHOICES":[{"INDEX":0,"DELTA":{"ROLE":"assistant","CONTENT":"hello"},"FINISH_REASON":"stop"}]}`,
		},
		{
			// reviewer 复现的原始缺陷帧：仅 choices 首字母大写、嵌套 Index/Delta/Content 首字母大写。
			name:      "reviewer_repro_choices_capitalized",
			canonical: `{"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			variant:   `{"Choices":[{"Index":0,"Delta":{"Content":"hello"}}]}`,
		},
		{
			// 混合大小写（title case 顶层 + 嵌套）。
			name:      "title_case",
			canonical: `{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			variant:   `{"Choices":[{"Index":0,"Delta":{"Content":"x"}}]}`,
		},
		{
			// 转义拼写解码后与规范键相同。
			name:      "escaped_keys",
			canonical: `{"choices":[{"index":0,"delta":{"content":"e"}}]}`,
			variant:   `{"\u0063hoices":[{"\u0069ndex":0,"\u0064elta":{"\u0063ontent":"e"}}]}`,
		},
		{
			// usage 及其全部子字段大小写变体。usage 的累积 body 保留原始键名大小写，故跳过 body 字节比对。
			name:             "usage_and_details",
			canonical:        `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":2,"cache_write_tokens":1,"audio_tokens":1,"text_tokens":1,"image_tokens":1},"completion_tokens_details":{"reasoning_tokens":2,"accepted_prediction_tokens":1,"rejected_prediction_tokens":1,"audio_tokens":1,"text_tokens":1}}}`,
			variant:          `{"CHOICES":[],"USAGE":{"PROMPT_TOKENS":3,"COMPLETION_TOKENS":4,"TOTAL_TOKENS":7,"PROMPT_TOKENS_DETAILS":{"CACHED_TOKENS":2,"CACHE_WRITE_TOKENS":1,"AUDIO_TOKENS":1,"TEXT_TOKENS":1,"IMAGE_TOKENS":1},"COMPLETION_TOKENS_DETAILS":{"REASONING_TOKENS":2,"ACCEPTED_PREDICTION_TOKENS":1,"REJECTED_PREDICTION_TOKENS":1,"AUDIO_TOKENS":1,"TEXT_TOKENS":1}}}`,
			skipBodyEquality: true,
		},
		{
			// tool_calls 及其 function/name/arguments、type/id 变体。
			name:      "tool_calls",
			canonical: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
			variant:   `{"CHOICES":[{"INDEX":0,"DELTA":{"TOOL_CALLS":[{"INDEX":0,"ID":"call_1","TYPE":"function","FUNCTION":{"NAME":"lookup","ARGUMENTS":"{\"q\":"}}]}}]}`,
		},
		{
			// legacy function_call 变体。
			name:      "legacy_function_call",
			canonical: `{"choices":[{"index":0,"delta":{"function_call":{"name":"lookup","arguments":"{}"}}}]}`,
			variant:   `{"CHOICES":[{"INDEX":0,"DELTA":{"FUNCTION_CALL":{"NAME":"lookup","ARGUMENTS":"{}"}}}]}`,
		},
	}

	for _, group := range chatGroups {
		t.Run("chat/"+group.name, func(t *testing.T) {
			canonFwd, canonText, canonUsage, canonBody := runStream(t, "data: "+group.canonical+"\ndata: [DONE]", relaymode.ChatCompletions)
			varFwd, varText, varUsage, varBody := runStream(t, "data: "+group.variant+"\ndata: [DONE]", relaymode.ChatCompletions)

			// 转发决策一致：两个帧都被逐字节透传（变体未被 Azure 空帧规则静默丢弃）。
			if !strings.Contains(canonFwd, group.canonical) {
				t.Fatalf("canonical frame must be forwarded verbatim, got %q", canonFwd)
			}
			if !strings.Contains(varFwd, group.variant) {
				t.Fatalf("case-variant frame must be forwarded verbatim, got %q", varFwd)
			}
			// 派生结论（文本累加、usage 计费）必须逐项一致。
			if canonText != varText {
				t.Fatalf("responseText mismatch: canonical=%q variant=%q", canonText, varText)
			}
			if usageString(canonUsage) != usageString(varUsage) {
				t.Fatalf("usage mismatch: canonical=%s variant=%s", usageString(canonUsage), usageString(varUsage))
			}
			// 重建 body 逐字节一致。例外：usage 的累积 body 直接承载原始 map（键名保留原大小写，
			// 改造前后一致），故大小写变体帧的 body 字节天然不同；此时仅比对 *model.Usage 计费结论。
			if !group.skipBodyEquality && canonBody != varBody {
				t.Fatalf("reconstructed body mismatch:\ncanonical=%q\n  variant=%q", canonBody, varBody)
			}
		})
	}

	// Completions 分支的大小写变体：`{"choices":[{"Text":"A"}]}` 等。
	completionsGroups := []struct {
		name      string
		canonical string
		variant   string
		wantText  string
	}{
		{
			name:      "choices_text",
			canonical: `{"choices":[{"text":"A"},{"text":"B"}]}`,
			variant:   `{"Choices":[{"Text":"A"},{"Text":"B"}]}`,
			wantText:  "AB",
		},
		{
			name:      "title_case",
			canonical: `{"choices":[{"text":"A"}]}`,
			variant:   `{"Choices":[{"Text":"A"}]}`,
			wantText:  "A",
		},
	}
	for _, group := range completionsGroups {
		t.Run("completions/"+group.name, func(t *testing.T) {
			canonFwd, canonText, _, _ := runStream(t, "data: "+group.canonical+"\ndata: [DONE]", relaymode.Completions)
			varFwd, varText, _, _ := runStream(t, "data: "+group.variant+"\ndata: [DONE]", relaymode.Completions)
			if !strings.Contains(canonFwd, group.canonical) || !strings.Contains(varFwd, group.variant) {
				t.Fatalf("frames must be forwarded verbatim: canonical=%q variant=%q", canonFwd, varFwd)
			}
			if canonText != group.wantText || varText != group.wantText {
				t.Fatalf("completions text mismatch: canonical=%q variant=%q want=%q", canonText, varText, group.wantText)
			}
		})
	}
}

// TestChatStreamCaseVariantCoexistenceAndDuplicates 锁定大小写变体共存与字节重复键的取值语义：
//   - 大小写变体共存：文档序 last-wins（`{"choices":A,"Choices":B}` → B），对齐 encoding/json；
//   - 字节完全相同的重复键：first-wins（AC-9 sanctioned 差异），对齐 gjson；
//   - 类型校验覆盖所有匹配键：任一匹配键类型不符即整帧失败（含被 first-wins 跳过的键）。
func TestChatStreamCaseVariantCoexistenceAndDuplicates(t *testing.T) {
	runPayload := func(t *testing.T, payload string) (chatStreamPayloadResult, error) {
		t.Helper()
		acc := newChatStreamAccumulator()
		return acc.addPayload([]byte(payload))
	}

	t.Run("variant_coexistence_last_wins_lower_first", func(t *testing.T) {
		result, err := runPayload(t, `{"choices":[{"index":0,"delta":{"content":"A"}}],"Choices":[{"index":0,"delta":{"content":"B"}}]}`)
		if err != nil {
			t.Fatalf("frame must be accepted: %v", err)
		}
		if result.ResponseText != "B" {
			t.Fatalf("文档序 last-wins 期望 \"B\"，got %q", result.ResponseText)
		}
	})

	t.Run("variant_coexistence_last_wins_upper_first", func(t *testing.T) {
		result, err := runPayload(t, `{"Choices":[{"index":0,"delta":{"content":"B"}}],"choices":[{"index":0,"delta":{"content":"A"}}]}`)
		if err != nil {
			t.Fatalf("frame must be accepted: %v", err)
		}
		if result.ResponseText != "A" {
			t.Fatalf("文档序 last-wins 期望 \"A\"，got %q", result.ResponseText)
		}
	})

	t.Run("byte_identical_duplicate_first_wins", func(t *testing.T) {
		result, err := runPayload(t, `{"choices":[{"index":0,"delta":{"content":"A"}}],"choices":[{"index":0,"delta":{"content":"B"}}]}`)
		if err != nil {
			t.Fatalf("frame must be accepted: %v", err)
		}
		if result.ResponseText != "A" {
			t.Fatalf("字节重复键 first-wins 期望 \"A\"，got %q", result.ResponseText)
		}
	})

	t.Run("usage_variant_last_wins", func(t *testing.T) {
		result, err := runPayload(t, `{"choices":[],"usage":{"prompt_tokens":1,"Prompt_Tokens":2}}`)
		if err != nil {
			t.Fatalf("frame must be accepted: %v", err)
		}
		if result.Usage == nil || result.Usage.PromptTokens != 2 {
			t.Fatalf("文档序 last-wins 期望 prompt_tokens=2，got %#v", result.Usage)
		}
	})

	t.Run("usage_byte_duplicate_first_wins", func(t *testing.T) {
		result, err := runPayload(t, `{"choices":[],"usage":{"prompt_tokens":1,"prompt_tokens":2}}`)
		if err != nil {
			t.Fatalf("frame must be accepted: %v", err)
		}
		if result.Usage == nil || result.Usage.PromptTokens != 1 {
			t.Fatalf("字节重复键 first-wins 期望 prompt_tokens=1，got %#v", result.Usage)
		}
	})

	// 类型校验覆盖所有匹配键：即使变体/重复键会被 first-wins/last-wins 跳过，类型不符仍整帧失败。
	rejectFrames := []struct {
		name    string
		payload string
	}{
		{"variant_choices_wrong_type", `{"Choices":"nope"}`},
		{"canonical_choices_wrong_type", `{"choices":"nope"}`},
		{"variant_then_canonical_choices_wrong_type", `{"Choices":[],"choices":"nope"}`},
		{"canonical_then_variant_choices_wrong_type", `{"choices":[],"Choices":"nope"}`},
		{"variant_usage_wrong_type", `{"choices":[],"Usage":"nope"}`},
		{"duplicate_id_wrong_type", `{"id":"x","id":123,"choices":[]}`},
		{"variant_created_wrong_type", `{"choices":[],"Created":"nope"}`},
		{"variant_usage_basis_wrong_type", `{"choices":[],"Usage":{"Prompt_Tokens":"nope"}}`},
	}
	for _, tc := range rejectFrames {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			seed := `{"id":"seed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"seed"}}]}`
			if _, err := acc.addPayload([]byte(seed)); err != nil {
				t.Fatalf("seed frame must be accepted: %v", err)
			}
			before := acc.buildResponseBody()
			result, err := acc.addPayload([]byte(tc.payload))
			if err == nil {
				t.Fatalf("type-invalid matching key must fail the whole frame, got err=nil (payload=%s)", tc.payload)
			}
			if result.ResponseText != "" || result.Usage != nil || result.ChoiceCount != 0 {
				t.Fatalf("rejected frame must not produce derived values, got %#v (payload=%s)", result, tc.payload)
			}
			if after := acc.buildResponseBody(); after != before {
				t.Fatalf("rejected frame must not touch accumulator:\nbefore=%q\n after=%q", before, after)
			}
		})
	}
}

// TestExtractCompletionsStreamTextCaseInsensitive 锁定 Completions 分支的大小写不敏感提取：
// `{"choices":[{"Text":"A"}]}` 等变体与规范形式结果一致；类型校验覆盖所有匹配键。
func TestExtractCompletionsStreamTextCaseInsensitive(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantText string
		wantErr  bool
	}{
		{name: "uppercase_choices_text", payload: `{"Choices":[{"Text":"A"}]}`, wantText: "A"},
		{name: "title_case_choices_text", payload: `{"Choices":[{"Text":"A"}]}`, wantText: "A"},
		{name: "mixed_variants_multi", payload: `{"choices":[{"text":"A"},{"Text":"B"},{"TEXT":"C"}]}`, wantText: "ABC"},
		{name: "escaped_keys", payload: `{"\u0063hoices":[{"\u0074ext":"A"}]}`, wantText: "A"},
		{name: "unicode_fold_choices", payload: `{"CHOICES":[{"TEXT":"A"}]}`, wantText: "A"},
		// 类型校验覆盖所有匹配键。
		{name: "uppercase_choices_wrong_type", payload: `{"Choices":"nope"}`, wantErr: true},
		{name: "uppercase_text_wrong_type", payload: `{"choices":[{"Text":123}]}`, wantErr: true},
		{name: "variant_then_canonical_wrong_type", payload: `{"Choices":[{"Text":"A"}],"choices":"nope"}`, wantErr: true},
		{name: "duplicate_text_first_wins", payload: `{"choices":[{"text":"A","text":"B"}]}`, wantText: "A"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotText, err := extractCompletionsStreamText([]byte(tc.payload))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (payload=%q, text=%q)", tc.payload, gotText)
				}
				if gotText != "" {
					t.Fatalf("error path must return empty text, got %q", gotText)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v (payload=%q)", err, tc.payload)
			}
			if gotText != tc.wantText {
				t.Fatalf("text=%q, want %q (payload=%q)", gotText, tc.wantText, tc.payload)
			}
		})
	}
}

// TestChatStreamCaseInsensitiveMatchesEncodingJSON 对同一批帧比较新实现与 encoding/json struct 解码的
// 可观测结论，证明大小写不敏感匹配已恢复。排除类别：字节完全相同的重复键（first-wins sanctioned 差异）。
func TestChatStreamCaseInsensitiveMatchesEncodingJSON(t *testing.T) {
	type legacyUsage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	type legacyChoice struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	}
	type legacyResponse struct {
		Choices []legacyChoice `json:"choices"`
		Usage   *legacyUsage   `json:"usage"`
	}

	frames := []string{
		`{"Choices":[{"Index":0,"Delta":{"Content":"hello"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"Choices":[{"Index":0,"Delta":{"Content":"c"}}],"Usage":{"Prompt_Tokens":3,"Completion_Tokens":4,"Total_Tokens":7}}`,
		`{"choices":[{"Index":0,"Delta":{"Content":"x"}}]}`,
		`{"CHOICES":[{"INDEX":0,"DELTA":{"CONTENT":"y"}}]}`,
	}

	for _, frame := range frames {
		t.Run(frame, func(t *testing.T) {
			var legacy legacyResponse
			if err := json.Unmarshal([]byte(frame), &legacy); err != nil {
				t.Fatalf("legacy decode failed: %v (frame=%s)", err, frame)
			}
			legacyText := ""
			for _, choice := range legacy.Choices {
				legacyText += choice.Delta.Content
			}

			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(frame))
			if err != nil {
				t.Fatalf("new implementation rejected a valid frame: %v (frame=%s)", err, frame)
			}
			if result.ResponseText != legacyText {
				t.Fatalf("responseText mismatch: new=%q legacy=%q (frame=%s)", result.ResponseText, legacyText, frame)
			}
			if (result.Usage == nil) != (legacy.Usage == nil) {
				t.Fatalf("usage presence mismatch: new=%#v legacy=%#v (frame=%s)", result.Usage, legacy.Usage, frame)
			}
			if result.Usage != nil && (result.Usage.PromptTokens != legacy.Usage.PromptTokens ||
				result.Usage.CompletionTokens != legacy.Usage.CompletionTokens ||
				result.Usage.TotalTokens != legacy.Usage.TotalTokens) {
				t.Fatalf("usage mismatch: new=%#v legacy=%#v (frame=%s)", result.Usage, legacy.Usage, frame)
			}
		})
	}
}

// TestChatStreamInvalidUTF8RemainsRaw 锁定已声明的库间差异（sanctioned，不修正）：
// encoding/json 把字符串中的非法 UTF-8 净化为 U+FFFD，而 gjson 保留原始字节。
// 该差异由契约「Sanctioned Invalid UTF-8 Difference」声明，样本不参与 AC-6 字节相等。
func TestChatStreamInvalidUTF8RemainsRaw(t *testing.T) {
	// 非法 UTF-8 字节 \xff\xfe 位于 delta.content 中；json.Valid 仍为 true（JSON 语法合法）。
	frame := "{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\xff\xfe\"}}]}"
	if !json.Valid([]byte(frame)) {
		t.Fatalf("frame must be syntactically valid JSON")
	}

	acc := newChatStreamAccumulator()
	result, err := acc.addPayload([]byte(frame))
	if err != nil {
		t.Fatalf("frame must be accepted: %v", err)
	}
	if result.ResponseText != "\xff\xfe" {
		t.Fatalf("gjson 必须保留原始字节（不被净化为 U+FFFD），got %v", []byte(result.ResponseText))
	}

	// 对照：encoding/json 会净化为 U+FFFD。
	var legacy ChatCompletionsStreamResponse
	if err := json.Unmarshal([]byte(frame), &legacy); err != nil {
		t.Fatalf("legacy decode failed: %v", err)
	}
	if legacy.Choices[0].Delta.Content != "\ufffd\ufffd" {
		t.Fatalf("legacy encoding/json 应净化为 U+FFFD，got %q", legacy.Choices[0].Delta.Content)
	}
}

// TestChatStreamNullDestinationTypeRules 锁定缺陷 D1 的修复：显式 `null` 的处理按目标字段类型区分
// （契约 4.2 Step 3 / 4.4 Step 2）。
//
//   - string 目标字段（Go string，如 id/object/model/role/delta.content）：null 是无操作——既不产出值，
//     也不占用 first-wins 键位，且不阻断后续匹配键的 last-wins 覆盖。
//   - 非 string 目标字段（object / array / int / bool / 浮点 / 指针，如 usage/choices/created/index/
//     finish_reason）：null 参与文档序 last-wins；若 null 是文档中最后一个匹配键，目标字段覆盖为
//     零值（nil / 0 / false）。
//
// 对照口径说明（重要）：改造前的累积产物来自「typed 解码 + map 累积」两步。
//   - 指针/容器/接口目标（usage *model.Usage、choices 切片、finish_reason *string、content any）：
//     两步结论一致，且与 `encoding/json` typed 解码一致——null 覆盖为零值/nil。故这些行同时与
//     encoding/json typed 解码逐字段对照。
//   - int 标量目标（created/index）：`encoding/json` typed 解码对 int 的 null 是 no-op（保留旧值），
//     而旧 map 累积路径取 last-wins（null → 0）。契约 4.2 选定「非 string 一律零值」，与旧 map
//     累积路径一致；本测试对这些行断言契约零值，并以 oldMapCreatedFor 探针复现旧 map 路径结论。
func TestChatStreamNullDestinationTypeRules(t *testing.T) {
	// referenceCases：目标为指针/容器/接口，与 encoding/json typed 解码逐字段对照。
	referenceCases := []struct {
		name  string
		frame string
		check func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, acc *chatStreamAccumulator)
	}{
		{
			name:  "usage_case_variant_trailing_null",
			frame: `{"usage":{"prompt_tokens":5},"Usage":null}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, _ *chatStreamAccumulator) {
				if legacy.Usage != nil {
					t.Fatalf("baseline: encoding/json 期望 usage=nil，got %#v", legacy.Usage)
				}
				if result.Usage != nil {
					t.Fatalf("D1: usage 应为 nil，got %#v", result.Usage)
				}
			},
		},
		{
			name:  "usage_identical_duplicate_trailing_null",
			frame: `{"usage":{"prompt_tokens":5},"usage":null}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, _ *chatStreamAccumulator) {
				if legacy.Usage != nil {
					t.Fatalf("baseline: encoding/json 期望 usage=nil，got %#v", legacy.Usage)
				}
				if result.Usage != nil {
					t.Fatalf("D1: usage 应为 nil，got %#v", result.Usage)
				}
			},
		},
		{
			name:  "choices_case_variant_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"content":"a"}}],"Choices":null}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, _ *chatStreamAccumulator) {
				if len(legacy.Choices) != 0 {
					t.Fatalf("baseline: encoding/json 期望 choices 为空，got %d", len(legacy.Choices))
				}
				if result.ChoiceCount != 0 || result.ResponseText != "" {
					t.Fatalf("D1: choices=null 应产出 0 choice 与空文本，got %#v", result)
				}
			},
		},
		{
			name:  "choices_identical_duplicate_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"content":"a"}}],"choices":null}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, _ *chatStreamAccumulator) {
				if len(legacy.Choices) != 0 {
					t.Fatalf("baseline: encoding/json 期望 choices 为空，got %d", len(legacy.Choices))
				}
				if result.ChoiceCount != 0 || result.ResponseText != "" {
					t.Fatalf("D1: choices=null 应产出 0 choice 与空文本，got %#v", result)
				}
			},
		},
		{
			name:  "finish_reason_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{},"finish_reason":"stop","finish_reason":null}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, _ chatStreamPayloadResult, acc *chatStreamAccumulator) {
				if legacy.Choices[0].FinishReason != nil {
					t.Fatalf("baseline: encoding/json 期望 finish_reason=nil，got %q", *legacy.Choices[0].FinishReason)
				}
				choice, ok := acc.choices[0]
				if !ok || choice.finishReason != nil {
					t.Fatalf("D1: finish_reason 应为 nil，got %#v", choice)
				}
			},
		},
		{
			name:  "content_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"content":"a","content":null}}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, _ *chatStreamAccumulator) {
				if legacy.Choices[0].Delta.Content != nil {
					t.Fatalf("baseline: encoding/json 期望 content=nil，got %#v", legacy.Choices[0].Delta.Content)
				}
				if result.ResponseText != "" {
					t.Fatalf("D1: content=null 应贡献空串，got %q", result.ResponseText)
				}
			},
		},
		{
			// delta.content 为 Go `any`（非 string 目标）：大小写变体末位 null 覆盖为 nil，贡献空串，
			// 与 encoding/json typed 解码（any 字段 last-wins 置 nil）一致。
			name:  "content_case_variant_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"content":"a","CONTENT":null}}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, result chatStreamPayloadResult, _ *chatStreamAccumulator) {
				if legacy.Choices[0].Delta.Content != nil {
					t.Fatalf("baseline: encoding/json 期望 content=nil，got %#v", legacy.Choices[0].Delta.Content)
				}
				if result.ResponseText != "" {
					t.Fatalf("D1: content=null 应贡献空串，got %q", result.ResponseText)
				}
			},
		},
	}

	for _, tc := range referenceCases {
		t.Run("reference/"+tc.name, func(t *testing.T) {
			var legacy ChatCompletionsStreamResponse
			if err := json.Unmarshal([]byte(tc.frame), &legacy); err != nil {
				t.Fatalf("baseline decode failed: %v (frame=%s)", err, tc.frame)
			}
			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(tc.frame))
			if err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}
			tc.check(t, &legacy, result, acc)
		})
	}

	// intCases：目标为 int 标量。契约 4.2 / AC-6 Byte Baseline 要求：非 string 目标字段的末位 null
	// 覆盖为零值。该口径与旧 map 累积路径在「字节完全相同的重复键」上一致（见下方 cross-check），
	// 对大小写变体则按契约统一为零值（AC-6 明确要求 `{"created":1,"Created":null}` → created=0）。
	intCases := []struct {
		name  string
		frame string
		want  int64
		// crossCheckOldMap 为 true 时同时复现旧 map 累积路径结论并要求一致。
		crossCheckOldMap bool
	}{
		{
			name:             "created_identical_duplicate_trailing_null",
			frame:            `{"created":1,"created":null,"id":"x","choices":[{"index":0,"delta":{"content":"a"}}]}`,
			want:             0,
			crossCheckOldMap: true,
		},
		{
			// AC-6 Byte Baseline 明确要求：`{"created":1,"Created":null,...}` yields `created=0`。
			name:  "created_case_variant_trailing_null",
			frame: `{"created":1,"Created":null,"id":"x","choices":[{"index":0,"delta":{"content":"a"}}]}`,
			want:  0,
		},
		{
			// null 在前、值在后：末位匹配键为 1，故 created=1（null 不占用 first-wins 键位）。
			name:  "created_null_then_value",
			frame: `{"created":null,"created":1,"id":"x","choices":[{"index":0,"delta":{"content":"a"}}]}`,
			want:  1,
		},
	}
	for _, tc := range intCases {
		t.Run("int/"+tc.name, func(t *testing.T) {
			acc := newChatStreamAccumulator()
			if _, err := acc.addPayload([]byte(tc.frame)); err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}
			if acc.created != tc.want {
				t.Fatalf("D1: created 应为 %d（契约非 string null 零值规则），got %d (frame=%s)", tc.want, acc.created, tc.frame)
			}
			if tc.crossCheckOldMap {
				// 复现旧 map 累积路径结论，证明零值覆盖与旧累积产物一致（而非凭空新增差异）。
				if got := oldMapCreatedFor(t, tc.frame); got != tc.want {
					t.Fatalf("旧 map 累积路径期望 created=%d，got %d (frame=%s)", tc.want, got, tc.frame)
				}
			}
		})
	}

	// choiceIndex 目标为 int：末位 null 覆盖为 0。
	t.Run("int/choice_index_trailing_null", func(t *testing.T) {
		frame := `{"choices":[{"index":3,"index":null,"delta":{"content":"a"}}]}`
		acc := newChatStreamAccumulator()
		if _, err := acc.addPayload([]byte(frame)); err != nil {
			t.Fatalf("frame must be accepted, got err=%v", err)
		}
		choice, ok := acc.choices[0]
		if !ok || choice.index != 0 {
			t.Fatalf("D1: choices[].index 应为 0，got %#v", choice)
		}
	})

	// stringCases：string 目标字段的 null 是无操作——既不产出值，也不阻断后续覆盖。
	stringCases := []struct {
		name  string
		frame string
		check func(t *testing.T, legacy *ChatCompletionsStreamResponse, acc *chatStreamAccumulator)
	}{
		{
			name:  "id_case_variant_trailing_null_noop",
			frame: `{"id":"a","ID":null,"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, acc *chatStreamAccumulator) {
				if legacy.Id != "a" {
					t.Fatalf("baseline: encoding/json 期望 id=%q，got %q", "a", legacy.Id)
				}
				if acc.id != "a" {
					t.Fatalf("D1: string null 为 no-op，id 应为 %q，got %q", "a", acc.id)
				}
			},
		},
		{
			name:  "id_null_then_value_not_blocked",
			frame: `{"id":null,"id":"b","choices":[{"index":0,"delta":{"content":"x"}}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, acc *chatStreamAccumulator) {
				if legacy.Id != "b" {
					t.Fatalf("baseline: encoding/json 期望 id=%q，got %q", "b", legacy.Id)
				}
				if acc.id != "b" {
					t.Fatalf("D1: null 不占用 first-wins 键位，后续键应生效为 %q，got %q", "b", acc.id)
				}
			},
		},
		{
			name:  "model_case_variant_trailing_null_noop",
			frame: `{"model":"m","MODEL":null,"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, acc *chatStreamAccumulator) {
				if legacy.Model != "m" || acc.model != "m" {
					t.Fatalf("D1: string null 为 no-op，model 应为 %q，legacy=%q new=%q", "m", legacy.Model, acc.model)
				}
			},
		},
		{
			name:  "role_case_variant_trailing_null_noop",
			frame: `{"choices":[{"index":0,"delta":{"role":"assistant","ROLE":null}}]}`,
			check: func(t *testing.T, legacy *ChatCompletionsStreamResponse, acc *chatStreamAccumulator) {
				if legacy.Choices[0].Delta.Role != "assistant" {
					t.Fatalf("baseline: encoding/json 期望 role=%q，got %q", "assistant", legacy.Choices[0].Delta.Role)
				}
				if acc.choices[0].role != "assistant" {
					t.Fatalf("D1: string null 为 no-op，role 应为 %q，got %q", "assistant", acc.choices[0].role)
				}
			},
		},
	}
	for _, tc := range stringCases {
		t.Run("string/"+tc.name, func(t *testing.T) {
			var legacy ChatCompletionsStreamResponse
			if err := json.Unmarshal([]byte(tc.frame), &legacy); err != nil {
				t.Fatalf("baseline decode failed: %v (frame=%s)", err, tc.frame)
			}
			acc := newChatStreamAccumulator()
			if _, err := acc.addPayload([]byte(tc.frame)); err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}
			tc.check(t, &legacy, acc)
		})
	}
}

// oldMapCreatedFor 复现改造前 map 累积路径对顶层 created 的取值：case-sensitive map 查找 + null→0。
func oldMapCreatedFor(t *testing.T, frame string) int64 {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(frame), &raw); err != nil {
		t.Fatalf("map decode failed: %v (frame=%s)", err, frame)
	}
	switch typed := raw["created"].(type) {
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

// TestChatStreamSkippedMatchingKeyRecursiveValidation 锁定缺陷 D2 的修复：校验必须递归到目标字段
// 类型可解析的深度，被跳过的匹配键（大小写变体、被 first-wins 跳过的重复键）若无法完整解析为
// 目标 Go 类型，整帧失败（契约 4.2 Step 4）。每行均与 encoding/json 的递归校验结论对照。
func TestChatStreamSkippedMatchingKeyRecursiveValidation(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantReject bool
	}{
		// 胜出者合法，被跳过者嵌套内容非法 → 整帧失败。
		{name: "usage_bad_then_good", frame: `{"usage":{"prompt_tokens":"x"},"Usage":{"prompt_tokens":5}}`, wantReject: true},
		{name: "usage_details_bad_then_good", frame: `{"usage":{"prompt_tokens":1,"prompt_tokens_details":"x"},"Usage":{"prompt_tokens":5}}`, wantReject: true},
		{name: "choices_numbers_then_empty", frame: `{"choices":[1,2],"CHOICES":[]}`, wantReject: true},
		{name: "choices_bad_role_then_good", frame: `{"choices":[{"index":0,"delta":{"role":123}}],"CHOICES":[{"index":0,"delta":{"role":"assistant"}}]}`, wantReject: true},
		{name: "choices_bad_finish_then_good", frame: `{"choices":[{"index":0,"finish_reason":123}],"CHOICES":[{"index":0,"finish_reason":"stop"}]}`, wantReject: true},
		{name: "choices_good_then_bad", frame: `{"choices":[{"index":0,"delta":{"content":"a"}}],"CHOICES":[{"index":0,"delta":{"role":123}}]}`, wantReject: true},
		{name: "usage_good_then_bad", frame: `{"usage":{"prompt_tokens":5},"Usage":{"prompt_tokens":"x"}}`, wantReject: true},
		{name: "choices_bad_nested_content_non_finite", frame: `{"choices":[{"index":0,"delta":{"content":"a"}}],"Choices":[{"index":0,"delta":{"content":[1e400]}}]}`, wantReject: true},
		{name: "tool_calls_bad_then_good", frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":123}}]}}],"CHOICES":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"ok"}}]}}]}`, wantReject: true},
		// 被跳过者合法（含未知字段与非有限数落在未知字段）→ 必须接受（对齐 encoding/json）。
		{name: "usage_unknown_non_finite_then_good", frame: `{"usage":{"prompt_tokens":1,"x":1e400},"Usage":{"prompt_tokens":5}}`, wantReject: false},
		{name: "choices_unknown_non_finite_then_good", frame: `{"choices":[{"index":0,"unknown":1e400}],"Choices":[{"index":1}]}`, wantReject: false},
		{name: "usage_null_then_good", frame: `{"usage":null,"Usage":{"prompt_tokens":5}}`, wantReject: false},
		{name: "choices_null_then_good", frame: `{"choices":null,"Choices":[{"index":1}]}`, wantReject: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 对照：encoding/json 对目标类型的递归校验结论（仅用 typed 结构体判定 err）。
			var legacy ChatCompletionsStreamResponse
			legacyErr := json.Unmarshal([]byte(tc.frame), &legacy)

			acc := newChatStreamAccumulator()
			seed := `{"id":"seed","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"seed"}}]}`
			if _, err := acc.addPayload([]byte(seed)); err != nil {
				t.Fatalf("seed frame must be accepted: %v", err)
			}
			before := acc.buildResponseBody()
			result, err := acc.addPayload([]byte(tc.frame))

			if tc.wantReject {
				if err == nil {
					t.Fatalf("frame must be rejected, got err=nil (frame=%s)", tc.frame)
				}
				if legacyErr == nil {
					t.Fatalf("baseline disagreement: encoding/json accepted %s but new impl rejected (%v)", tc.frame, err)
				}
				if result.ResponseText != "" || result.Usage != nil || result.ChoiceCount != 0 {
					t.Fatalf("rejected frame must not produce derived values, got %#v (frame=%s)", result, tc.frame)
				}
				if after := acc.buildResponseBody(); after != before {
					t.Fatalf("rejected frame must not touch accumulator:\nbefore=%q\n after=%q (frame=%s)", before, after, tc.frame)
				}
				return
			}
			if err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}
			if legacyErr != nil {
				t.Fatalf("baseline disagreement: encoding/json rejected %s (%v) but new impl accepted", tc.frame, legacyErr)
			}
		})
	}
}

// TestCompletionsStreamNullAndRecursiveValidation 覆盖任务 4.4 对 Completions 分支的同一组语义：
// string `choices[].text` 的 null 为 no-op；array `choices` 的 null 参与 last-wins 覆盖为 nil；
// 被跳过的匹配键必须递归校验到目标类型，非法即整帧失败。每行与旧 typed 解码对照。
func TestCompletionsStreamNullAndRecursiveValidation(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		wantText   string
		wantReject bool
	}{
		// D1：array choices 的 null 覆盖为 nil（无文本）。
		{name: "choices_case_variant_trailing_null", payload: `{"choices":[{"text":"a"}],"Choices":null}`, wantText: ""},
		{name: "choices_identical_duplicate_trailing_null", payload: `{"choices":[{"text":"a"}],"choices":null}`, wantText: ""},
		// D1：string text 的 null 是 no-op，不阻断后续覆盖。
		{name: "text_null_then_value", payload: `{"choices":[{"text":null,"text":"b"}]}`, wantText: "b"},
		{name: "text_value_then_null_noop", payload: `{"choices":[{"text":"a","text":null}]}`, wantText: "a"},
		{name: "text_case_variant_trailing_null_noop", payload: `{"choices":[{"text":"a","TEXT":null}]}`, wantText: "a"},
		// D2：被跳过的匹配键递归校验失败 → 整帧失败。
		{name: "choices_numbers_then_empty", payload: `{"choices":[1,2],"CHOICES":[]}`, wantReject: true},
		{name: "choices_bad_text_then_good", payload: `{"choices":[{"text":123}],"CHOICES":[{"text":"ok"}]}`, wantReject: true},
		{name: "choices_bad_finish_then_good", payload: `{"choices":[{"finish_reason":123}],"CHOICES":[{"finish_reason":"stop"}]}`, wantReject: true},
		{name: "choices_good_then_bad", payload: `{"choices":[{"text":"a"}],"CHOICES":[{"text":123}]}`, wantReject: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacyText, legacyErr := legacyCompletionsStreamText([]byte(tc.payload))
			gotText, err := extractCompletionsStreamText([]byte(tc.payload))

			if tc.wantReject {
				if err == nil {
					t.Fatalf("frame must be rejected, got err=nil (payload=%s)", tc.payload)
				}
				if legacyErr == nil {
					t.Fatalf("baseline disagreement: legacy accepted %s but new impl rejected (%v)", tc.payload, err)
				}
				if gotText != "" {
					t.Fatalf("rejected frame must return empty text, got %q", gotText)
				}
				return
			}
			if err != nil {
				t.Fatalf("frame must be accepted, got err=%v (payload=%s)", err, tc.payload)
			}
			if legacyErr != nil {
				t.Fatalf("baseline disagreement: legacy rejected %s (%v) but new impl accepted", tc.payload, legacyErr)
			}
			if gotText != legacyText || gotText != tc.wantText {
				t.Fatalf("text mismatch: legacy=%q new=%q want=%q (payload=%s)", legacyText, gotText, tc.wantText, tc.payload)
			}
		})
	}
}

// TestChatStreamStringTargetNullNoOpFixture 锁定契约 AC-6 Byte Baseline 中声明的 string 目标
// 字面 fixture：`{"type":"a","TYPE":null}` yields type `"a"`，因为 string 目标字段的显式 `null`
// 是无操作——既不产出值、不占用 first-wins 键位，也不阻断后续匹配键的 last-wins 覆盖。
//
// 该 fixture 此前仅由 TestChatStreamNullDestinationTypeRules 覆盖 id/model/role 三个 string 目标，
// tool_calls[].type 等其余 string 目标未被测试锁定；本测试补齐 AC-6 声明的字面样本。
//
// 每例均与 encoding/json 的 typed 解码（struct string 字段对 null 的 no-op 语义）对照。
func TestChatStreamStringTargetNullNoOpFixture(t *testing.T) {
	// legacyStringFields 镜像本用例涉及的 string 目标字段的 typed 解码视图。
	type legacyToolCall struct {
		Id       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	type legacyChoice struct {
		Index int `json:"index"`
		Delta struct {
			Role      string           `json:"role"`
			ToolCalls []legacyToolCall `json:"tool_calls"`
		} `json:"delta"`
	}
	type legacyStringFields struct {
		Id                string         `json:"id"`
		Object            string         `json:"object"`
		Model             string         `json:"model"`
		SystemFingerprint string         `json:"system_fingerprint"`
		Choices           []legacyChoice `json:"choices"`
	}

	cases := []struct {
		name  string
		frame string
		check func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields)
	}{
		{
			// 契约 AC-6 声明的字面 fixture：`{"type":"a","TYPE":null}` yields type "a"。
			name:  "tool_call_type_case_variant_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"a","TYPE":null,"function":{"arguments":"{}"}}]}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if got := legacy.Choices[0].Delta.ToolCalls[0].Type; got != "a" {
					t.Fatalf("baseline: encoding/json 期望 type=%q，got %q", "a", got)
				}
				toolCall, ok := acc.choices[0].toolCalls[0]
				if !ok {
					t.Fatalf("tool_calls[0] 缺失，choices=%#v", acc.choices)
				}
				if toolCall.typeValue != "a" {
					t.Fatalf("string null 为 no-op，type 应为 %q，got %q", "a", toolCall.typeValue)
				}
			},
		},
		{
			// 字节完全相同的重复键，末位 null：first-wins 保留 "a"，null 不覆盖。
			name:  "tool_call_type_identical_duplicate_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"a","type":null,"function":{"arguments":"{}"}}]}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if got := legacy.Choices[0].Delta.ToolCalls[0].Type; got != "a" {
					t.Fatalf("baseline: encoding/json 期望 type=%q，got %q", "a", got)
				}
				if got := acc.choices[0].toolCalls[0].typeValue; got != "a" {
					t.Fatalf("string null 为 no-op，type 应为 %q，got %q", "a", got)
				}
			},
		},
		{
			// null 在前、值在后：null 不占用 first-wins 键位，后续 `type` 生效。
			name:  "tool_call_type_null_then_value",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"TYPE":null,"type":"a","function":{"arguments":"{}"}}]}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if got := legacy.Choices[0].Delta.ToolCalls[0].Type; got != "a" {
					t.Fatalf("baseline: encoding/json 期望 type=%q，got %q", "a", got)
				}
				if got := acc.choices[0].toolCalls[0].typeValue; got != "a" {
					t.Fatalf("null 不占用 first-wins 键位，type 应为 %q，got %q", "a", got)
				}
			},
		},
		{
			// object 为 string 目标：末位大小写变体 null 是无操作，保留 "chat.completion.chunk"。
			name:  "object_case_variant_trailing_null",
			frame: `{"object":"chat.completion.chunk","OBJECT":null,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if legacy.Object != "chat.completion.chunk" {
					t.Fatalf("baseline: encoding/json 期望 object=%q，got %q", "chat.completion.chunk", legacy.Object)
				}
				if acc.object != "chat.completion.chunk" {
					t.Fatalf("string null 为 no-op，object 应为 %q，got %q", "chat.completion.chunk", acc.object)
				}
				// 端到端：buildResponseBody 把 chunk object 归一化为 chat.completion。
				var body map[string]interface{}
				if err := json.Unmarshal([]byte(acc.buildResponseBody()), &body); err != nil {
					t.Fatalf("body must be valid JSON: %v", err)
				}
				if body["object"] != "chat.completion" {
					t.Fatalf("body.object 应为 %q，got %#v", "chat.completion", body["object"])
				}
			},
		},
		{
			// system_fingerprint 为 string 目标（宽松捕获路径）：末位大小写变体 null 是无操作。
			name:  "system_fingerprint_case_variant_trailing_null",
			frame: `{"system_fingerprint":"fp","SYSTEM_FINGERPRINT":null,"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if legacy.SystemFingerprint != "fp" {
					t.Fatalf("baseline: encoding/json 期望 system_fingerprint=%q，got %q", "fp", legacy.SystemFingerprint)
				}
				if acc.systemFingerprint != "fp" {
					t.Fatalf("string null 为 no-op，system_fingerprint 应为 %q，got %q", "fp", acc.systemFingerprint)
				}
			},
		},
		{
			// tool_calls[].id 为 string 目标：末位大小写变体 null 是无操作。
			name:  "tool_call_id_case_variant_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","ID":null,"function":{"arguments":"{}"}}]}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if got := legacy.Choices[0].Delta.ToolCalls[0].Id; got != "call_1" {
					t.Fatalf("baseline: encoding/json 期望 id=%q，got %q", "call_1", got)
				}
				if got := acc.choices[0].toolCalls[0].id; got != "call_1" {
					t.Fatalf("string null 为 no-op，id 应为 %q，got %q", "call_1", got)
				}
			},
		},
		{
			// tool_calls[].function.name 为 string 目标：末位大小写变体 null 是无操作。
			name:  "tool_call_function_name_case_variant_trailing_null",
			frame: `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup","NAME":null,"arguments":"{}"}}]}}]}`,
			check: func(t *testing.T, acc *chatStreamAccumulator, legacy legacyStringFields) {
				if got := legacy.Choices[0].Delta.ToolCalls[0].Function.Name; got != "lookup" {
					t.Fatalf("baseline: encoding/json 期望 name=%q，got %q", "lookup", got)
				}
				if got := acc.choices[0].toolCalls[0].function.name; got != "lookup" {
					t.Fatalf("string null 为 no-op，name 应为 %q，got %q", "lookup", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var legacy legacyStringFields
			if err := json.Unmarshal([]byte(tc.frame), &legacy); err != nil {
				t.Fatalf("baseline decode failed: %v (frame=%s)", err, tc.frame)
			}
			acc := newChatStreamAccumulator()
			if _, err := acc.addPayload([]byte(tc.frame)); err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}
			tc.check(t, acc, legacy)
		})
	}
}

// TestChatStreamUnicodeSimpleFoldVariants 锁定 exact-then-fold 键匹配的 Unicode 简单折叠语义：
// `strings.EqualFold` 基于 `unicode.SimpleFold`，long s `ſ` (U+017F) 与 `s` 折叠等价，故
// `{"ſystem_fingerprint":"fp"}`、`{"choiceſ":[...]}` 与规范拼写命中同一目标字段。
//
// 折叠变体与规范拼写共存时按文档序 last-wins（对齐 encoding/json 的 foldName）。每例均与
// encoding/json 的 typed 解码对照。契约 AC-6 Byte Baseline 要求锁定 Unicode simple-fold 变体。
func TestChatStreamUnicodeSimpleFoldVariants(t *testing.T) {
	// 前置断言：折叠语义前提，避免运行环境差异导致本测试静默失效。
	if !strings.EqualFold("ſ", "s") {
		t.Fatalf("前提失效：strings.EqualFold(\"ſ\", \"s\") 必须为 true")
	}

	type legacyFold struct {
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Index int `json:"index"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}

	cases := []struct {
		name        string
		frame       string
		wantText    string
		wantFP      string
		wantChoices int
	}{
		{
			// `ſystem_fingerprint` 折叠命中 `system_fingerprint`。
			name:        "system_fingerprint_long_s",
			frame:       `{"ſystem_fingerprint":"fp","choices":[{"index":0,"delta":{"content":"c"}}]}`,
			wantText:    "c",
			wantFP:      "fp",
			wantChoices: 1,
		},
		{
			// `choiceſ` 折叠命中 `choices`（单键）。
			name:        "choices_long_s_single",
			frame:       `{"choiceſ":[{"index":0,"delta":{"content":"c"}}]}`,
			wantText:    "c",
			wantFP:      "",
			wantChoices: 1,
		},
		{
			// 规范拼写在前、折叠变体在后：文档序 last-wins 取折叠变体 "d"。
			name:        "choices_long_s_last_wins",
			frame:       `{"choices":[{"index":0,"delta":{"content":"c"}}],"choiceſ":[{"index":0,"delta":{"content":"d"}}]}`,
			wantText:    "d",
			wantFP:      "",
			wantChoices: 1,
		},
		{
			// 折叠变体在前、规范拼写在后：文档序 last-wins 取规范拼写 "c"。
			name:        "choices_long_s_first_last_wins",
			frame:       `{"choiceſ":[{"index":0,"delta":{"content":"d"}}],"choices":[{"index":0,"delta":{"content":"c"}}]}`,
			wantText:    "c",
			wantFP:      "",
			wantChoices: 1,
		},
		{
			// system_fingerprint 折叠变体共存：文档序 last-wins 取 "fp2"。
			name:        "system_fingerprint_long_s_last_wins",
			frame:       `{"system_fingerprint":"fp1","ſystem_fingerprint":"fp2","choices":[]}`,
			wantText:    "",
			wantFP:      "fp2",
			wantChoices: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var legacy legacyFold
			if err := json.Unmarshal([]byte(tc.frame), &legacy); err != nil {
				t.Fatalf("baseline decode failed: %v (frame=%s)", err, tc.frame)
			}
			legacyText := ""
			for _, choice := range legacy.Choices {
				legacyText += choice.Delta.Content
			}

			acc := newChatStreamAccumulator()
			result, err := acc.addPayload([]byte(tc.frame))
			if err != nil {
				t.Fatalf("frame must be accepted, got err=%v (frame=%s)", err, tc.frame)
			}

			// 与 encoding/json 对照：折叠变体必须命中同一目标字段且 last-wins 结论一致。
			if legacy.SystemFingerprint != tc.wantFP {
				t.Fatalf("baseline: encoding/json 期望 system_fingerprint=%q，got %q (frame=%s)", tc.wantFP, legacy.SystemFingerprint, tc.frame)
			}
			if legacyText != tc.wantText {
				t.Fatalf("baseline: encoding/json 期望 text=%q，got %q (frame=%s)", tc.wantText, legacyText, tc.frame)
			}

			if result.ResponseText != tc.wantText {
				t.Fatalf("Unicode 折叠期望 text=%q，got %q (frame=%s)", tc.wantText, result.ResponseText, tc.frame)
			}
			if result.ChoiceCount != tc.wantChoices {
				t.Fatalf("Unicode 折叠期望 ChoiceCount=%d，got %d (frame=%s)", tc.wantChoices, result.ChoiceCount, tc.frame)
			}
			if acc.systemFingerprint != tc.wantFP {
				t.Fatalf("Unicode 折叠期望 system_fingerprint=%q，got %q (frame=%s)", tc.wantFP, acc.systemFingerprint, tc.frame)
			}
		})
	}
}

// =============================================================================
// 模块 E / 任务 5.1：C1（openai.Handler）新旧实现等价性对照测试
//
// 对照方式：对同一份响应样本，分别用「旧实现基线」（真实 `encoding/json` 解码到
// SlimTextResponse，即当前 Handler 的行为）与「契约定义的新提取路径」`extractTextResponse`
// 产出结论，逐字段比较 Usage（含 details）、上游 Error（含 code）与用于估算的 choice content。
//
// 基线为真实 `encoding/json` 调用，期望值不硬编码（唯一硬编码的是各样本的
// old/new 结果分类，用于记录已声明差异）。
//
// 已声明差异（非回归）：
//   - duplicate：新路径 first-wins，旧实现 last-wins（契约 AC-9 sanctioned exception）。
//   - detailTolerant：details 子字段不可解析时，旧实现整份解码失败（HTTP 500），新实现
//     按 spec「Non-billing detail unparseable → treat as absent」删除该 detail 并保留 basis
//     （模块 C 6.1 声明行为）。
// =============================================================================

func c1LegacyBaseline(t *testing.T, body string) c1LegacyOutcome {
	t.Helper()
	var textResponse SlimTextResponse
	err := json.Unmarshal([]byte(body), &textResponse)
	outcome := c1LegacyOutcome{decodeErr: err != nil}
	if err != nil {
		return outcome
	}
	if textResponse.Error.Type != "" {
		errCopy := textResponse.Error
		outcome.upstreamErr = &errCopy
		return outcome
	}
	outcome.usage = textResponse.Usage
	for _, choice := range textResponse.Choices {
		outcome.contents = append(outcome.contents, choice.Message.StringContent())
	}
	return outcome
}

// c1LegacyOutcome 是旧实现（encoding/json 基线）的可观测结果。
type c1LegacyOutcome struct {
	decodeErr   bool
	upstreamErr *model.Error
	usage       model.Usage
	contents    []string
}

// TestExtractTextResponseEquivalence 为 C1 建立新旧实现对照测试骨架。
func TestExtractTextResponseEquivalence(t *testing.T) {
	type sample struct {
		name    string
		body    string
		legacy  string // "success" | "error" | "upstreamError"
		current string // "success" | "error" | "upstreamError"
		// declared 记录已声明差异；等于 "" 时要求新旧逐字段严格相等。
		declared string // "" | "firstWins" | "detailTolerant"
	}

	const usageNormal = `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`

	cases := []sample{
		// ---- 正常 ----
		{name: "normal_bases_only", body: `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":` + usageNormal + `}`, legacy: "success", current: "success"},
		{name: "normal_with_details", body: `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":7,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":9,"accepted_prediction_tokens":1}},"choices":[{"message":{"content":"ok"}}]}`, legacy: "success", current: "success"},
		{name: "normal_without_details", body: `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3},"choices":[{"message":{"content":"ok"}}]}`, legacy: "success", current: "success"},
		{name: "normal_multiple_choices", body: `{"choices":[{"message":{"content":"a"}},{"message":{"content":"b"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, legacy: "success", current: "success"},
		{name: "normal_array_content_text_part", body: `{"choices":[{"message":{"content":["a",{"type":"text","text":"b"},5]}}]}`, legacy: "success", current: "success"},

		// ---- 缺失 ----
		{name: "missing_usage", body: `{"choices":[{"message":{"content":"a"}}]}`, legacy: "success", current: "success"},
		{name: "missing_usage_null", body: `{"choices":[{"message":{"content":"a"}}],"usage":null}`, legacy: "success", current: "success"},
		{name: "missing_choices", body: `{"usage":` + usageNormal + `}`, legacy: "success", current: "success"},
		{name: "missing_null_billing_integers", body: `{"usage":{"prompt_tokens":null,"completion_tokens":null,"total_tokens":null}}`, legacy: "success", current: "success"},

		// ---- 类型不匹配（basis / usage / choices）----
		{name: "mismatch_basis_string", body: `{"usage":{"prompt_tokens":"x"}}`, legacy: "error", current: "error"},
		{name: "mismatch_basis_array", body: `{"usage":{"prompt_tokens":[1]}}`, legacy: "error", current: "error"},
		{name: "mismatch_basis_object", body: `{"usage":{"prompt_tokens":{"a":1}}}`, legacy: "error", current: "error"},
		{name: "mismatch_basis_bool", body: `{"usage":{"prompt_tokens":true}}`, legacy: "error", current: "error"},
		{name: "mismatch_usage_number", body: `{"usage":5}`, legacy: "error", current: "error"},
		{name: "mismatch_usage_string", body: `{"usage":"x"}`, legacy: "error", current: "error"},
		{name: "mismatch_choices_number", body: `{"choices":5}`, legacy: "error", current: "error"},
		{name: "mismatch_choices_element_number", body: `{"choices":[5]}`, legacy: "error", current: "error"},
		{name: "mismatch_choice_index_string", body: `{"choices":[{"index":"x"}]}`, legacy: "error", current: "error"},

		// ---- 零值 ----
		{name: "zero_all_bases", body: `{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, legacy: "success", current: "success"},
		{name: "zero_empty_usage_object", body: `{"usage":{}}`, legacy: "success", current: "success"},

		// ---- 极大值 ----
		{name: "extreme_maxint32_adjacent", body: `{"usage":{"prompt_tokens":2147483646,"completion_tokens":2147483647,"total_tokens":2147483647}}`, legacy: "success", current: "success"},
		{name: "extreme_maxint64", body: `{"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":0,"total_tokens":9223372036854775807}}`, legacy: "success", current: "success"},

		// ---- 小数 / 指数 ----
		{name: "fractional_5_7", body: `{"usage":{"prompt_tokens":5.7}}`, legacy: "error", current: "error"},
		{name: "fractional_5_0", body: `{"usage":{"prompt_tokens":5.0}}`, legacy: "error", current: "error"},
		{name: "fractional_exponential_1e2", body: `{"usage":{"prompt_tokens":1e2}}`, legacy: "error", current: "error"},

		// ---- 负数 ----
		{name: "negative_bases", body: `{"usage":{"prompt_tokens":-1,"completion_tokens":-2,"total_tokens":-3}}`, legacy: "success", current: "success"},

		// ---- 畸形 JSON ----
		{name: "malformed_unclosed_object", body: `{"usage":{"prompt_tokens":5`, legacy: "error", current: "error"},
		{name: "malformed_truncated_string", body: `{"usage":{"prompt_tokens":5,`, legacy: "error", current: "error"},
		{name: "malformed_trailing_garbage", body: `{"usage":{"prompt_tokens":5}}garbage`, legacy: "error", current: "error"},

		// ---- 非对象根 ----
		{name: "root_null", body: `null`, legacy: "success", current: "success"},
		{name: "root_array", body: `[]`, legacy: "error", current: "error"},
		{name: "root_number", body: `5`, legacy: "error", current: "error"},
		{name: "root_string", body: `"x"`, legacy: "error", current: "error"},
		{name: "root_bool", body: `true`, legacy: "error", current: "error"},

		// ---- 重复键（AC-9 sanctioned：first-wins vs last-wins）----
		{name: "duplicate_basis_same_bytes", body: `{"usage":{"prompt_tokens":5,"prompt_tokens":9}}`, legacy: "success", current: "success", declared: "firstWins"},
		{name: "duplicate_usage_object", body: `{"usage":{"prompt_tokens":5},"usage":{"prompt_tokens":9}}`, legacy: "success", current: "success", declared: "firstWins"},

		// ---- details 类型不匹配（声明差异：旧 500，新按缺失降级）----
		{name: "detail_mismatch_prompt_not_object", body: `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":"x"}}`, legacy: "error", current: "success", declared: "detailTolerant"},
		{name: "detail_mismatch_prompt_field", body: `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":"x"}}}`, legacy: "error", current: "success", declared: "detailTolerant"},
		{name: "detail_mismatch_completion_not_object", body: `{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"completion_tokens_details":5}}`, legacy: "error", current: "success", declared: "detailTolerant"},

		// ---- 上游错误 ----
		{name: "upstream_error_full", body: `{"error":{"message":"bad","type":"invalid_request_error","code":"x"}}`, legacy: "upstreamError", current: "upstreamError"},
		{name: "upstream_error_code_object", body: `{"error":{"message":"bad","type":"t","code":{"k":1}}}`, legacy: "upstreamError", current: "upstreamError"},
		{name: "error_empty_object_stays_success", body: `{"error":{}}`, legacy: "success", current: "success"},
		{name: "error_message_only_stays_success", body: `{"error":{"message":"m"}}`, legacy: "success", current: "success"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			legacy := c1LegacyBaseline(t, tc.body)
			current, currentErr := extractTextResponse([]byte(tc.body))

			// 1. 旧实现分类必须与表中记录一致（基线自证）。
			legacyOutcome := classifyC1Legacy(legacy)
			if legacyOutcome != tc.legacy {
				t.Fatalf("legacy baseline classification mismatch: 表=%s 实得=%s (body=%s)", tc.legacy, legacyOutcome, tc.body)
			}

			// 2. 新实现分类。
			currentOutcome := "success"
			if currentErr != nil {
				currentOutcome = "error"
			} else if current.Error != nil {
				currentOutcome = "upstreamError"
			}
			if currentOutcome != tc.current {
				t.Fatalf("new extraction classification mismatch: 表=%s 实得=%s err=%v (body=%s)", tc.current, currentOutcome, currentErr, tc.body)
			}

			switch tc.current {
			case "error":
				if currentErr == nil {
					t.Fatalf("expected extraction error, got nil (body=%s)", tc.body)
				}
				return
			case "upstreamError":
				if legacy.upstreamErr == nil {
					t.Fatalf("legacy should have produced upstream error (body=%s)", tc.body)
				}
				if !reflect.DeepEqual(*legacy.upstreamErr, *current.Error) {
					t.Fatalf("upstream error mismatch:\n legacy=%+v\n current=%+v", *legacy.upstreamErr, *current.Error)
				}
				return
			}

			// 3. success：逐字段比较。已声明差异样本改由下方 declared 分支断言其专属结论。
			if tc.declared == "" {
				if !reflect.DeepEqual(legacy.usage, current.Usage) {
					t.Fatalf("usage mismatch:\n legacy=%+v\n current=%+v (body=%s)", legacy.usage, current.Usage, tc.body)
				}
				if !reflect.DeepEqual(legacy.contents, current.ChoiceContents) {
					t.Fatalf("choice content mismatch:\n legacy=%q\n current=%q (body=%s)", legacy.contents, current.ChoiceContents, tc.body)
				}
			}

			// 4. 已声明差异的额外断言。
			switch tc.declared {
			case "firstWins":
				// 旧实现 last-wins：样本设计为 5 然后 9 → 旧得 9；新得 5（first-wins，AC-9）。
				if legacy.usage.PromptTokens != 9 {
					t.Fatalf("expected legacy last-wins=9, got %d", legacy.usage.PromptTokens)
				}
				if current.Usage.PromptTokens != 5 {
					t.Fatalf("expected new first-wins=5, got %d", current.Usage.PromptTokens)
				}
				if !reflect.DeepEqual(legacy.contents, current.ChoiceContents) {
					t.Fatalf("duplicate sample should not differ in choice content: legacy=%q current=%q", legacy.contents, current.ChoiceContents)
				}
			case "detailTolerant":
				// 旧实现整份失败（legacy=="error"），新实现按 spec「非计费 detail 不可解析 → 视为缺失」
				// 删除该 detail 且保留 basis。
				if current.Usage.PromptTokens != 1 || current.Usage.CompletionTokens != 2 || current.Usage.TotalTokens != 3 {
					t.Fatalf("expected bases retained under detail degradation, got %+v", current.Usage)
				}
				if current.Usage.PromptTokensDetails != nil {
					if current.Usage.PromptTokensDetails.CachedTokens != 0 || current.Usage.PromptTokensDetails.CacheWriteTokens != 0 {
						t.Fatalf("expected invalid prompt detail omitted, got %+v", current.Usage.PromptTokensDetails)
					}
				}
				if current.Usage.CompletionTokensDetails != nil {
					if current.Usage.CompletionTokensDetails.ReasoningTokens != 0 {
						t.Fatalf("expected invalid completion detail omitted, got %+v", current.Usage.CompletionTokensDetails)
					}
				}
			}
		})
	}
}

// classifyC1Legacy 把旧基线结果归类，与表中 legacy 列对应。
func classifyC1Legacy(outcome c1LegacyOutcome) string {
	if outcome.decodeErr {
		return "error"
	}
	if outcome.upstreamErr != nil {
		return "upstreamError"
	}
	return "success"
}

// =============================================================================
// 任务 7.1（reviewer advisory）：C1 openai.Handler 入口级断言
//
// 5.1 矩阵只覆盖 extractTextResponse 纯函数；入口 Handler 的
//   1) body 逐字节透传（转发原响应体，不得改动/截断）
//   2) ctxkey.ResponseBody 写入
//   3) 提取失败保留既有 HTTP 500 路径（unmarshal_response_body_failed）
// 在此补齐，锁定「纯函数正确」到「入口正确接线」的最后一公里。
// =============================================================================

// newHandlerTestResponse 构造供 Handler 消费的可观测 *http.Response。
func newHandlerTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestOpenAIHandlerEntryLevelPassthroughAndStorage 断言成功路径下 Handler：
//   - 原响应体逐字节写入 c.Writer；
//   - ctxkey.ResponseBody 保存原始字节；
//   - 上游 usage 缺失时按既有逻辑用 choice content 估算（保留估计行为）。
func TestOpenAIHandlerEntryLevelPassthroughAndStorage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// usage 缺失分支会走 CountTokenText 估算；避免依赖 tiktoken 词表下载，改用近似估算。
	origApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = origApproximate })

	for _, tc := range []struct {
		name string
		body string
	}{
		{"normal_with_usage", `{"id":"chatcmpl_1","object":"chat.completion","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`},
		{"normal_without_usage", `{"id":"chatcmpl_2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}]}`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			resp := newHandlerTestResponse(http.StatusOK, tc.body)

			relayErr, usage := Handler(c, resp, 7, "gpt-4o")

			if relayErr != nil {
				t.Fatalf("expected no relay error, got %+v", relayErr)
			}
			if usage == nil {
				t.Fatalf("expected non-nil usage")
			}
			// body 逐字节透传（不得 JSON 重新序列化）。
			if recorder.Body.String() != tc.body {
				t.Fatalf("expected byte-identical passthrough:\nwant=%q\n got=%q", tc.body, recorder.Body.String())
			}
			// ctxkey.ResponseBody 保存原始字节。
			if c.GetString(ctxkey.ResponseBody) != tc.body {
				t.Fatalf("expected ctxkey.ResponseBody to hold original bytes, got %q", c.GetString(ctxkey.ResponseBody))
			}
		})
	}
}

// TestOpenAIHandlerEntryLevelMalformedReturnsHTTP500 断言 Handler 入口对不支持/畸形输入的
// 既有 HTTP 500 出口（`unmarshal_response_body_failed`）与上游错误透传保持不变，且失败路径
// 绝不透传 body、绝不写 ctxkey.ResponseBody。
func TestOpenAIHandlerEntryLevelMalformedReturnsHTTP500(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 需要 HTTP 500 的畸形/类型不符输入（与 5.1 矩阵的 current=error 行一致）。
	errorBodies := []struct {
		name string
		body string
	}{
		{"malformed_unclosed", `{"usage":{"prompt_tokens":5`},
		{"malformed_trailing_garbage", `{"usage":{"prompt_tokens":5}}garbage`},
		{"mismatch_basis_string", `{"usage":{"prompt_tokens":"x"}}`},
		{"mismatch_choices_number", `{"choices":5}`},
		{"root_array", `[]`},
	}
	for _, tc := range errorBodies {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			resp := newHandlerTestResponse(http.StatusOK, tc.body)

			relayErr, usage := Handler(c, resp, 1, "gpt-4o")

			if relayErr == nil {
				t.Fatalf("[%s] expected HTTP 500 relay error, got nil", tc.name)
			}
			if relayErr.StatusCode != http.StatusInternalServerError {
				t.Fatalf("[%s] expected HTTP 500, got %d", tc.name, relayErr.StatusCode)
			}
			if fmt.Sprint(relayErr.Error.Code) != "unmarshal_response_body_failed" {
				t.Fatalf("[%s] expected code unmarshal_response_body_failed, got %q", tc.name, fmt.Sprint(relayErr.Error.Code))
			}
			if usage != nil {
				t.Fatalf("[%s] failure path must not return usage, got %+v", tc.name, usage)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("[%s] failure path must not write body, got %q", tc.name, recorder.Body.String())
			}
			if c.GetString(ctxkey.ResponseBody) != "" {
				t.Fatalf("[%s] failure path must not store ctxkey.ResponseBody", tc.name)
			}
		})
	}

	t.Run("upstream_error_passthrough", func(t *testing.T) {
		body := `{"error":{"message":"bad","type":"invalid_request_error","code":"x"}}`
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := newHandlerTestResponse(http.StatusBadRequest, body)

		relayErr, usage := Handler(c, resp, 1, "gpt-4o")
		if relayErr == nil {
			t.Fatalf("expected upstream error relayed, got nil")
		}
		if relayErr.Error.Message != "bad" {
			t.Fatalf("expected upstream error message forwarded, got %q", relayErr.Error.Message)
		}
		if usage != nil {
			t.Fatalf("upstream error path must not return usage, got %+v", usage)
		}
	})
}

// =============================================================================
// 任务 7.3：JSON 热路径基准 —— 代表性 chat 流式帧 / 非流式 usage 响应（改造前后对比）
// =============================================================================

// benchmarkChatStreamFrame 生成一份覆盖顶层元数据 + choices 的代表性 chat SSE 帧
// （约 1KB，接近真实 delta 帧规模）。
func benchmarkChatStreamFrame() []byte {
	var sb strings.Builder
	sb.WriteString(`{"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1710000000,"model":"gpt-4o","system_fingerprint":"fp_bench","choices":[{"index":0,"delta":{"role":"assistant","content":"`)
	sb.WriteString(strings.Repeat("y", 512))
	sb.WriteString(`"},"finish_reason":null}],"usage":{"prompt_tokens":128,"completion_tokens":64,"total_tokens":192,"prompt_tokens_details":{"cached_tokens":16,"cache_write_tokens":0}}}`)
	return []byte(sb.String())
}

// benchmarkNonStreamUsageResponse 生成一份含 details 的非流式 chat 响应体（约 1KB）。
func benchmarkNonStreamUsageResponse() []byte {
	var sb strings.Builder
	sb.WriteString(`{"id":"chatcmpl-bench","object":"chat.completion","created":1710000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"`)
	sb.WriteString(strings.Repeat("z", 512))
	sb.WriteString(`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":128,"completion_tokens":64,"total_tokens":192,"prompt_tokens_details":{"cached_tokens":16,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":8,"accepted_prediction_tokens":1,"rejected_prediction_tokens":0,"audio_tokens":0,"text_tokens":56}}}`)
	return []byte(sb.String())
}

// legacyChatAccumulator 内联复刻改造前的 chatStreamAccumulator（map[string]any 路径），
// 仅用于 7.3 基准的「改造前」对照；不参与任何生产或功能测试。
type legacyChatAccumulator struct {
	id                string
	object            string
	created           int64
	model             string
	systemFingerprint string
	usage             map[string]any
	choices           map[int]*legacyChatChoice
}

type legacyChatChoice struct {
	index   int
	role    string
	content string
}

func newLegacyChatAccumulator() *legacyChatAccumulator {
	return &legacyChatAccumulator{choices: make(map[int]*legacyChatChoice)}
}

// legacyAddPayload 复刻改造前 addPayload：json.Unmarshal 到 map[string]any 后累积。
func (a *legacyChatAccumulator) legacyAddPayload(payload []byte) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return
	}
	if value, ok := raw["id"].(string); ok && value != "" {
		a.id = value
	}
	if value, ok := raw["object"].(string); ok && value != "" {
		a.object = value
	}
	if value, ok := raw["created"].(float64); ok {
		a.created = int64(value)
	}
	if value, ok := raw["model"].(string); ok && value != "" {
		a.model = value
	}
	if value, ok := raw["system_fingerprint"].(string); ok && value != "" {
		a.systemFingerprint = value
	}
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
		index := 0
		if idx, ok := choiceMap["index"].(float64); ok {
			index = int(idx)
		}
		choice, ok := a.choices[index]
		if !ok {
			choice = &legacyChatChoice{index: index}
			a.choices[index] = choice
		}
		delta, ok := choiceMap["delta"].(map[string]any)
		if !ok {
			continue
		}
		if role, ok := delta["role"].(string); ok && role != "" {
			choice.role = role
		}
		if content, ok := delta["content"].(string); ok {
			choice.content += content
		}
	}
}

// BenchmarkJsonParserHotPathChatStreamFrame 对比 chat 流式帧的改造前后解析开销。
//
// 改造前 StreamHandler 对同一帧解析两遍：`json.Unmarshal(→ChatCompletionsStreamResponse)`
// 取文本/usage，再 `chatAccumulator.addPayload` 内部 `json.Unmarshal(→map[string]any)` 累积。
// 故 before 子基准同时执行 typed 解码与 legacy map 累积；after 子基准执行单遍 on-demand addPayload。
//
// 契约 7.3 / AC-5：至少一项（ns/op 或 allocs/op）改善。
func BenchmarkJsonParserHotPathChatStreamFrame(b *testing.B) {
	frame := benchmarkChatStreamFrame()

	b.Run("before_double_decode_typed_plus_map", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(frame)))
		for i := 0; i < b.N; i++ {
			var streamResponse ChatCompletionsStreamResponse
			if err := json.Unmarshal(frame, &streamResponse); err != nil {
				b.Fatalf("unmarshal failed: %v", err)
			}
			legacy := newLegacyChatAccumulator()
			legacy.legacyAddPayload(frame)
			for _, choice := range streamResponse.Choices {
				_ = choice.Delta.Content
			}
			_ = streamResponse.Usage
		}
	})

	b.Run("after_single_pass_on_demand", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(frame)))
		for i := 0; i < b.N; i++ {
			acc := newChatStreamAccumulator()
			if _, err := acc.addPayload(frame); err != nil {
				b.Fatalf("addPayload failed: %v", err)
			}
		}
	})
}

// BenchmarkJsonParserHotPathNonStreamUsage 对比非流式 usage 响应的改造前后提取开销：
//   - before_typed_decode：改造前 `json.Unmarshal(→SlimTextResponse)` 全量反序列化；
//   - after_on_demand：改造后 `extractTextResponse` 的 on-demand 提取。
//
// 契约 7.3 / AC-5：至少一项（ns/op 或 allocs/op）改善。C1 已知 ns/op 回归、allocs 大幅下降，
// 数据在 7.3 报告中如实记录（作为 documented measurement evidence）。
func BenchmarkJsonParserHotPathNonStreamUsage(b *testing.B) {
	body := benchmarkNonStreamUsageResponse()

	b.Run("before_typed_decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			var textResponse SlimTextResponse
			if err := json.Unmarshal(body, &textResponse); err != nil {
				b.Fatalf("unmarshal failed: %v", err)
			}
			_ = textResponse.Usage
		}
	})

	b.Run("after_on_demand", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			if _, err := extractTextResponse(body); err != nil {
				b.Fatalf("extractTextResponse failed: %v", err)
			}
		}
	})
}

// benchmarkC1SmallResponse 生成约 331B 的小 chat 响应（reviewer 报告的 C1 回归最小样本）。
func benchmarkC1SmallResponse() []byte {
	return []byte(`{"id":"chatcmpl-small","object":"chat.completion","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
}

// benchmarkC1SingleChoiceLargeResponse 生成约 13KB 的单 choice 大响应（reviewer 报告的中间样本）。
func benchmarkC1SingleChoiceLargeResponse() []byte {
	var sb strings.Builder
	sb.WriteString(`{"id":"chatcmpl-large","object":"chat.completion","created":1700000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"`)
	sb.WriteString(strings.Repeat("a", 12*1024))
	sb.WriteString(`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	return []byte(sb.String())
}

// benchmarkC1ManyChoiceLargeResponse 生成约 96KB 的 40-choice 大响应（reviewer 报告的最大样本）。
func benchmarkC1ManyChoiceLargeResponse() []byte {
	var sb strings.Builder
	sb.WriteString(`{"id":"chatcmpl-many","object":"chat.completion","created":1700000000,"model":"gpt-4o","choices":[`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"index":`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(`,"message":{"role":"assistant","content":"`)
		sb.WriteString(strings.Repeat("b", 2300))
		sb.WriteString(`"},"finish_reason":"stop"}`)
	}
	sb.WriteString(`],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	return []byte(sb.String())
}

// BenchmarkJsonParserHotPathNonStreamUsageSizes 逐规模复现 reviewer 报告的 C1 ns/op 回归：
//   - 331B 小响应、13KB 单 choice、96KB 40-choice；
//   - 每个规模给出 before（encoding/json → SlimTextResponse）与 after（extractTextResponse）。
//
// 测量证据：after 的 ns/op 高于 before（根因 json.Valid 全量扫描 + gjson.ParseBytes 内部整份拷贝），
// 但 B/op 与 allocs/op 显著下降。该回归在 7.3 报告中如实记录为 documented measurement evidence。
func BenchmarkJsonParserHotPathNonStreamUsageSizes(b *testing.B) {
	sizes := []struct {
		name string
		body []byte
	}{
		{"small_331B", benchmarkC1SmallResponse()},
		{"single_choice_13KB", benchmarkC1SingleChoiceLargeResponse()},
		{"many_choice_96KB", benchmarkC1ManyChoiceLargeResponse()},
	}

	for _, s := range sizes {
		s := s
		b.Run(s.name+"/before_typed_decode", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(s.body)))
			for i := 0; i < b.N; i++ {
				var textResponse SlimTextResponse
				if err := json.Unmarshal(s.body, &textResponse); err != nil {
					b.Fatalf("unmarshal failed: %v", err)
				}
				_ = textResponse.Usage
			}
		})
		b.Run(s.name+"/after_on_demand", func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(s.body)))
			for i := 0; i < b.N; i++ {
				if _, err := extractTextResponse(s.body); err != nil {
					b.Fatalf("extractTextResponse failed: %v", err)
				}
			}
		})
	}
}
