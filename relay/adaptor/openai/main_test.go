package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
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
