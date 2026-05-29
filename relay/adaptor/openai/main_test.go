package openai

import (
	"encoding/json"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
	"github.com/songquanpeng/one-api/relay/model"
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
