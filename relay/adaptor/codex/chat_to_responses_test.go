package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
	"github.com/tidwall/gjson"
)

func TestMain(m *testing.M) {
	oldWD, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	tempDir, err := os.MkdirTemp("", "myapi-codex-tests-*")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(tempDir); err != nil {
		_ = os.RemoveAll(tempDir)
		panic(err)
	}

	code := m.Run()

	_ = os.Chdir(oldWD)
	_ = os.RemoveAll(tempDir)
	os.Exit(code)
}

// parseCompletedOutput 从 generateCompletedEvents 的返回中提取 response.completed 事件，
// 再解析其中的 output 数组。
func parseCompletedOutput(events []string) []interface{} {
	for _, evt := range events {
		if len(evt) < 6 {
			continue
		}
		// SSE 格式: event: response.completed\ndata: {...}\n\n
		// 查找 data: 后面的 JSON
		dataPrefix := "data: "
		idx := indexOf(evt, dataPrefix)
		if idx < 0 {
			continue
		}
		dataStr := evt[idx+len(dataPrefix):]
		dataStr = trimSpace(dataStr)

		if !gjson.Valid(dataStr) {
			continue
		}
		resp := gjson.Parse(dataStr)
		if resp.Get("type").String() == "response.completed" {
			output := resp.Get("response.output")
			if output.Exists() && output.IsArray() {
				var result []interface{}
				json.Unmarshal([]byte(output.Raw), &result)
				return result
			}
			return nil
		}
	}
	return nil
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// sendChunks 模拟发送一系列 SSE chunk 并返回最终完成事件
func sendChunks(chunks []string, fallback bool) []string {
	var param any
	var allEvents []string
	for _, chunk := range chunks {
		events := ConvertOpenAIChatToResponses(nil, nil, []byte(chunk), &param, fallback)
		allEvents = append(allEvents, events...)
	}
	return allEvents
}

func TestConvertOpenAIChatToResponses_FallbackReasoning(t *testing.T) {
	Convey("ConvertOpenAIChatToResponses 的 reasoning 兜底逻辑", t, func() {

		Convey("T1: 仅 reasoning_content 无 content，开关=true → output 含 reasoning + message", func() {
			chunks := []string{
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Step 1思考"},"finish_reason":null}]}`,
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"reasoning_content":" Step 2思考"},"finish_reason":null}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, true)
			output := parseCompletedOutput(events)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 2)

			// 第一个是 reasoning
			item0, ok := output[0].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(item0["type"], ShouldEqual, "reasoning")

			// 第二个是 message（兜底）
			item1, ok := output[1].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(item1["type"], ShouldEqual, "message")
			So(item1["role"], ShouldEqual, "assistant")
			content, ok := item1["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			content0, ok := content[0].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(content0["type"], ShouldEqual, "output_text")
			So(content0["text"], ShouldEqual, "Step 1思考 Step 2思考")
		})

		Convey("T2: 仅 reasoning_content 无 content，开关=false → output 只有 reasoning", func() {
			chunks := []string{
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"思考中"},"finish_reason":null}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)
			output := parseCompletedOutput(events)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 1)
			item0, ok := output[0].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(item0["type"], ShouldEqual, "reasoning")
		})

		Convey("T3: 正常 reasoning + content，开关=true → output 含 reasoning + message（原行为）", func() {
			chunks := []string{
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"思考中"},"finish_reason":null}]}`,
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"content":" World"},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, true)
			output := parseCompletedOutput(events)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "reasoning")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "message")
			// 消息内容来自 content，不是 reasoning 兜底
			content := output[1].(map[string]interface{})["content"].([]interface{})
			text := content[0].(map[string]interface{})["text"].(string)
			So(text, ShouldEqual, "Hello World")
		})

		Convey("T4: 仅 content 无 reasoning，开关=true → output 只有 message（不触发兜底）", func() {
			chunks := []string{
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, true)
			output := parseCompletedOutput(events)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "message")
		})

		Convey("T5: 仅 reasoning，开关=true，流式事件序列完整", func() {
			chunks := []string{
				`data: {"id":"resp_test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"思考过程"},"finish_reason":null}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, true)

			// 收集所有事件类型
			var eventTypes []string
			for _, evt := range events {
				if len(evt) < 6 {
					continue
				}
				if idx := indexOf(evt, "event: "); idx >= 0 {
					endIdx := indexOf(evt[idx+7:], "\n")
					if endIdx >= 0 {
						eventTypes = append(eventTypes, evt[idx+7:idx+7+endIdx])
					}
				}
			}

			// 预期的事件序列
			expected := []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.reasoning_summary_part.added",
				"response.reasoning_summary_text.delta",
				"response.reasoning_summary_text.done",
				"response.reasoning_summary_part.done",
				"response.output_item.done",
				// 以下是兜底 message 事件
				"response.output_item.added",
				"response.content_part.added",
				"response.output_text.delta",
				"response.output_text.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.completed",
			}

			So(eventTypes, ShouldResemble, expected)

			// 验证 output 数组
			output := parseCompletedOutput(events)
			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "reasoning")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "message")
		})
	})
}

func TestSendChunksEmpty(t *testing.T) {
	// 辅助测试：确保辅助函数正常工作
	Convey("sendChunks with empty input", t, func() {
		events := sendChunks([]string{}, true)
		So(len(events), ShouldEqual, 0)
	})
}

// ensure fmt import is used (for the format strings in fallback code)
var _ = fmt.Sprintf

// =============================================================================
// #5 修复：流式 output_item.added 事件必须带 name/namespace/input（与 #4 非流式对称）
// =============================================================================

// codexRequestWithNamespace 构造一份带 namespace 工具的 codex Responses 请求 JSON。
func codexRequestWithNamespace() []byte {
	return []byte(`{
		"model": "codex-test",
		"tools": [
			{
				"type": "namespace",
				"name": "myapp__",
				"tools": [
					{"type": "function", "name": "exec", "description": "run cmd", "parameters": {"type": "object"}}
				]
			}
		]
	}`)
}

// codexRequestWithFunction 构造一份带普通 function 工具的 codex Responses 请求 JSON。
func codexRequestWithFunction() []byte {
	return []byte(`{
		"model": "codex-test",
		"tools": [
			{"type": "function", "name": "get_weather", "description": "Get weather", "parameters": {"type": "object"}}
		]
	}`)
}

func codexRequestWithBuiltinTool(toolType string) []byte {
	return []byte(fmt.Sprintf(`{
		"model": "codex-test",
		"tools": [
			{"type": %q, "name": %q, "description": "builtin tool", "parameters": {"type": "object"}}
		]
	}`, toolType, toolType))
}

// codexRequestWithApplyPatch 构造一份带 apply_patch 自定义工具的 codex Responses 请求 JSON。
func codexRequestWithApplyPatch() []byte {
	return []byte(`{
		"model": "codex-test",
		"tools": [
			{"type": "custom", "name": "apply_patch", "description": "patch files"}
		]
	}`)
}

// codexRequestWithPlainCustom 构造一份带普通 custom 工具的 codex Responses 请求 JSON。
func codexRequestWithPlainCustom() []byte {
	return []byte(`{
		"model": "codex-test",
		"tools": [
			{"type": "custom", "name": "my_grammar", "description": "user grammar"}
		]
	}`)
}

// parseOutputItemAdded 解析所有 response.output_item.added 事件，返回 item 字典列表。
// 仅返回 item 字段，不含 sequence_number / output_index 等。
func parseOutputItemAdded(events []string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, evt := range events {
		if !strings.Contains(evt, "event: response.output_item.added") {
			continue
		}
		idx := indexOf(evt, "data: ")
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len("data: "):])
		if !gjson.Valid(dataStr) {
			continue
		}
		parsed := gjson.Parse(dataStr)
		if parsed.Get("type").String() != "response.output_item.added" {
			continue
		}
		item := parsed.Get("item")
		if !item.Exists() {
			continue
		}
		out = append(out, map[string]interface{}{
			"raw":  dataStr,
			"item": map[string]interface{}(nil),
		})
		// 解析 item 字段
		var itemMap map[string]interface{}
		if err := json.Unmarshal([]byte(item.Raw), &itemMap); err == nil {
			out[len(out)-1]["item"] = itemMap
		}
	}
	return out
}

// parseOutputItemDone 解析所有 response.output_item.done 事件，返回 item 字典列表。
func parseOutputItemDone(events []string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, evt := range events {
		if !strings.Contains(evt, "event: response.output_item.done") {
			continue
		}
		idx := indexOf(evt, "data: ")
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len("data: "):])
		if !gjson.Valid(dataStr) {
			continue
		}
		parsed := gjson.Parse(dataStr)
		if parsed.Get("type").String() != "response.output_item.done" {
			continue
		}
		item := parsed.Get("item")
		if !item.Exists() {
			continue
		}
		var itemMap map[string]interface{}
		if err := json.Unmarshal([]byte(item.Raw), &itemMap); err == nil {
			out = append(out, itemMap)
		}
	}
	return out
}

func parseOutputItemEventSummaries(events []string, eventType string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, evt := range events {
		if !strings.Contains(evt, "event: "+eventType) {
			continue
		}
		idx := indexOf(evt, "data: ")
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len("data: "):])
		if !gjson.Valid(dataStr) {
			continue
		}
		parsed := gjson.Parse(dataStr)
		if parsed.Get("type").String() != eventType {
			continue
		}
		out = append(out, map[string]interface{}{
			"output_index": parsed.Get("output_index").Int(),
			"id":           parsed.Get("item.id").String(),
			"type":         parsed.Get("item.type").String(),
		})
	}
	return out
}

func parseFunctionCallArgumentEvents(events []string, eventType string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, evt := range events {
		if !strings.Contains(evt, "event: "+eventType) {
			continue
		}
		idx := indexOf(evt, "data: ")
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len("data: "):])
		if !gjson.Valid(dataStr) {
			continue
		}
		parsed := gjson.Parse(dataStr)
		if parsed.Get("type").String() != eventType {
			continue
		}
		out = append(out, map[string]interface{}{
			"item_id":   parsed.Get("item_id").String(),
			"arguments": parsed.Get("arguments").String(),
			"delta":     parsed.Get("delta").String(),
		})
	}
	return out
}

func parseBuiltinToolLifecycleEvents(events []string, eventType string) []map[string]interface{} {
	var out []map[string]interface{}
	for _, evt := range events {
		if !strings.Contains(evt, "event: "+eventType) {
			continue
		}
		idx := indexOf(evt, "data: ")
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len("data: "):])
		if !gjson.Valid(dataStr) {
			continue
		}
		parsed := gjson.Parse(dataStr)
		if parsed.Get("type").String() != eventType {
			continue
		}
		out = append(out, map[string]interface{}{
			"item_id":      parsed.Get("item_id").String(),
			"output_index": parsed.Get("output_index").Int(),
			"delta":        parsed.Get("delta").String(),
			"query":        parsed.Get("query").String(),
		})
	}
	return out
}

func collectEventTypes(events []string) []string {
	var eventTypes []string
	for _, evt := range events {
		if idx := indexOf(evt, "event: "); idx >= 0 {
			endIdx := indexOf(evt[idx+7:], "\n")
			if endIdx >= 0 {
				eventTypes = append(eventTypes, evt[idx+7:idx+7+endIdx])
			}
		}
	}
	return eventTypes
}

func indexOfToolEvent(events []string, eventType string, needle string) int {
	for i, evt := range events {
		if !strings.Contains(evt, "event: "+eventType) {
			continue
		}
		if needle != "" && !strings.Contains(evt, needle) {
			continue
		}
		return i
	}
	return -1
}

func assertUniqueOutputIndexes(items []map[string]interface{}) {
	seen := map[int64]bool{}
	for _, item := range items {
		idx := item["output_index"].(int64)
		So(seen[idx], ShouldBeFalse)
		seen[idx] = true
	}
}

func TestConvertOpenAIChatToResponses_ReasoningWhitespaceAndBuiltinToolSearch(t *testing.T) {
	Convey("reasoning + whitespace-only content + tool_search 应兜底 message 且 final output 保持 builtin 类型", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Need spawn"},"finish_reason":null}]}`,
			`data: {"id":"resp_ts","choices":[{"index":0,"delta":{"reasoning_content":" utility agent"},"finish_reason":null}]}`,
			`data: {"id":"resp_ts","choices":[{"index":0,"delta":{"content":"\n\n"},"finish_reason":null}]}`,
			`data: {"id":"resp_ts","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ts_1","type":"function","function":{"name":"tool_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"multi-agent subagent spawn utility\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, true)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)
		var addedTool map[string]interface{}
		for _, addedItem := range added {
			item := addedItem["item"].(map[string]interface{})
			if item["type"] == "tool_search_call" {
				addedTool = item
			}
		}
		So(addedTool, ShouldNotBeNil)
		So(addedTool["type"], ShouldEqual, "tool_search_call")
		So(addedTool["id"], ShouldEqual, "tsc_call_ts_1")
		So(addedTool["call_id"], ShouldEqual, "call_ts_1")

		done := parseOutputItemDone(allEvents)
		var doneTool map[string]interface{}
		for _, item := range done {
			if item["type"] == "tool_search_call" {
				doneTool = item
			}
		}
		So(doneTool, ShouldNotBeNil)
		So(doneTool["type"], ShouldEqual, "tool_search_call")
		So(doneTool["id"], ShouldEqual, "tsc_call_ts_1")
		So(doneTool["call_id"], ShouldEqual, "call_ts_1")
		doneArgs, ok := doneTool["arguments"].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(doneArgs["query"], ShouldEqual, "multi-agent subagent spawn utility")
		fcDeltas := parseFunctionCallArgumentEvents(allEvents, "response.function_call_arguments.delta")
		fcDones := parseFunctionCallArgumentEvents(allEvents, "response.function_call_arguments.done")
		for _, evt := range fcDeltas {
			So(evt["item_id"], ShouldNotEqual, "fc_call_ts_1")
		}
		for _, evt := range fcDones {
			So(evt["item_id"], ShouldNotEqual, "fc_call_ts_1")
		}

		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		So(len(output), ShouldEqual, 3)
		So(output[0].(map[string]interface{})["type"], ShouldEqual, "reasoning")
		msg := output[1].(map[string]interface{})
		So(msg["type"], ShouldEqual, "message")
		content := msg["content"].([]interface{})
		So(content[0].(map[string]interface{})["text"], ShouldEqual, "Need spawn utility agent")

		tool := output[2].(map[string]interface{})
		So(tool["type"], ShouldEqual, "tool_search_call")
		So(tool["id"], ShouldEqual, "tsc_call_ts_1")
		So(tool["call_id"], ShouldEqual, "call_ts_1")
		So(tool["name"], ShouldEqual, "tool_search")
		toolArgs, ok := tool["arguments"].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(toolArgs["query"], ShouldEqual, "multi-agent subagent spawn utility")
		So(tool["status"], ShouldEqual, "completed")

		completedIDs := map[string]string{}
		for _, outputItem := range output {
			item := outputItem.(map[string]interface{})
			completedIDs[item["id"].(string)] = item["type"].(string)
			if item["type"] == "message" {
				content := item["content"].([]interface{})
				So(content[0].(map[string]interface{})["text"], ShouldNotEqual, "\n\n")
			}
		}

		addedSummaries := parseOutputItemEventSummaries(allEvents, "response.output_item.added")
		doneSummaries := parseOutputItemEventSummaries(allEvents, "response.output_item.done")
		So(len(addedSummaries), ShouldEqual, 3)
		So(len(doneSummaries), ShouldEqual, 3)
		assertUniqueOutputIndexes(addedSummaries)
		assertUniqueOutputIndexes(doneSummaries)
		for _, item := range addedSummaries {
			So(completedIDs[item["id"].(string)], ShouldEqual, item["type"].(string))
		}
		for _, item := range doneSummaries {
			So(completedIDs[item["id"].(string)], ShouldEqual, item["type"].(string))
		}
	})
}

func TestConvertOpenAIChatToResponses_CompletedOutput_WebSearchBuiltin(t *testing.T) {
	Convey("web_search final completed output 不降级为 function_call", t, func() {
		chunks := []string{
			`data: {"id":"resp_ws","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ws_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"golang\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ws","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("web_search")
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)
		So(len(added), ShouldEqual, 1)
		addedTool := added[0]["item"].(map[string]interface{})
		So(addedTool["type"], ShouldEqual, "web_search_call")
		So(addedTool["id"], ShouldEqual, "wsc_call_ws_1")

		done := parseOutputItemDone(allEvents)
		So(len(done), ShouldEqual, 1)
		So(done[0]["type"], ShouldEqual, "web_search_call")
		So(done[0]["id"], ShouldEqual, "wsc_call_ws_1")
		doneArgs, ok := done[0]["arguments"].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(doneArgs["query"], ShouldEqual, "golang")

		fcDeltas := parseFunctionCallArgumentEvents(allEvents, "response.function_call_arguments.delta")
		fcDones := parseFunctionCallArgumentEvents(allEvents, "response.function_call_arguments.done")
		for _, evt := range fcDeltas {
			So(evt["item_id"], ShouldNotEqual, "fc_call_ws_1")
		}
		for _, evt := range fcDones {
			So(evt["item_id"], ShouldNotEqual, "fc_call_ws_1")
		}

		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		So(len(output), ShouldEqual, 1)
		tool := output[0].(map[string]interface{})
		So(tool["type"], ShouldEqual, "web_search_call")
		So(tool["id"], ShouldEqual, "wsc_call_ws_1")
		So(tool["call_id"], ShouldEqual, "call_ws_1")
		So(tool["name"], ShouldEqual, "web_search")
		toolArgs, ok := tool["arguments"].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(toolArgs["query"], ShouldEqual, "golang")
		So(tool["status"], ShouldEqual, "completed")
	})
}

func TestConvertOpenAIChatToResponses_PlainFunctionKeepsArgumentDeltaAndDone(t *testing.T) {
	Convey("普通 function 仍发 function_call_arguments delta/done", t, func() {
		chunks := []string{
			`data: {"id":"resp_plain_args","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_plain_args","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_plain_args","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_plain_args","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithFunction()
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		fcDeltas := parseFunctionCallArgumentEvents(allEvents, "response.function_call_arguments.delta")
		So(len(fcDeltas), ShouldEqual, 2)
		So(fcDeltas[0]["item_id"], ShouldEqual, "fc_call_plain_args")
		So(fcDeltas[0]["delta"], ShouldEqual, `{"city":`)
		So(fcDeltas[1]["item_id"], ShouldEqual, "fc_call_plain_args")
		So(fcDeltas[1]["delta"], ShouldEqual, `"Paris"}`)

		fcDones := parseFunctionCallArgumentEvents(allEvents, "response.function_call_arguments.done")
		So(len(fcDones), ShouldEqual, 1)
		So(fcDones[0]["item_id"], ShouldEqual, "fc_call_plain_args")
		So(fcDones[0]["arguments"], ShouldEqual, `{"city":"Paris"}`)
	})
}

func TestConvertOpenAIChatToResponses_ToolSearchEmitsLifecycleAndSearchQueryEvents(t *testing.T) {
	Convey("builtin tool_search 发射 lifecycle 与 search_query 事件", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts_lifecycle","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_lifecycle","type":"function","function":{"name":"tool_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_lifecycle","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"utility subagent\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_lifecycle","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		eventTypes := collectEventTypes(allEvents)
		So(eventTypes, ShouldContain, "response.tool_search_call.in_progress")
		So(eventTypes, ShouldContain, "response.tool_search_call.searching")
		So(eventTypes, ShouldContain, "response.tool_search_call.search_query.delta")
		So(eventTypes, ShouldContain, "response.tool_search_call.search_query.done")
		So(eventTypes, ShouldContain, "response.tool_search_call.completed")

		searchDeltas := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.delta")
		So(len(searchDeltas), ShouldEqual, 2)
		So(searchDeltas[0]["item_id"], ShouldEqual, "tsc_call_ts_lifecycle")
		So(searchDeltas[0]["delta"], ShouldEqual, `{"query":`)
		So(searchDeltas[1]["delta"], ShouldEqual, `"utility subagent"}`)

		searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
		So(len(searchDone), ShouldEqual, 1)
		So(searchDone[0]["item_id"], ShouldEqual, "tsc_call_ts_lifecycle")
		So(searchDone[0]["query"], ShouldEqual, "utility subagent")

		addedPos := indexOfToolEvent(allEvents, "response.output_item.added", `"tool_search_call"`)
		inProgressPos := indexOfToolEvent(allEvents, "response.tool_search_call.in_progress", `"tsc_call_ts_lifecycle"`)
		searchingPos := indexOfToolEvent(allEvents, "response.tool_search_call.searching", `"tsc_call_ts_lifecycle"`)
		deltaPos := indexOfToolEvent(allEvents, "response.tool_search_call.search_query.delta", `"tsc_call_ts_lifecycle"`)
		searchDonePos := indexOfToolEvent(allEvents, "response.tool_search_call.search_query.done", `"tsc_call_ts_lifecycle"`)
		completedPos := indexOfToolEvent(allEvents, "response.tool_search_call.completed", `"tsc_call_ts_lifecycle"`)
		donePos := indexOfToolEvent(allEvents, "response.output_item.done", `"tool_search_call"`)
		So(addedPos, ShouldBeGreaterThanOrEqualTo, 0)
		So(inProgressPos, ShouldBeGreaterThan, addedPos)
		So(searchingPos, ShouldBeGreaterThan, inProgressPos)
		So(deltaPos, ShouldBeGreaterThan, searchingPos)
		So(searchDonePos, ShouldBeGreaterThan, deltaPos)
		So(completedPos, ShouldBeGreaterThan, searchDonePos)
		So(donePos, ShouldBeGreaterThan, completedPos)
	})
}

func TestConvertOpenAIChatToResponses_WebSearchEmitsLifecycleEvents(t *testing.T) {
	Convey("builtin web_search 发射 lifecycle 与 search_query 事件", t, func() {
		chunks := []string{
			`data: {"id":"resp_ws_lifecycle","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ws_lifecycle","type":"function","function":{"name":"web_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ws_lifecycle","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"golang\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ws_lifecycle","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("web_search")
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		eventTypes := collectEventTypes(allEvents)
		So(eventTypes, ShouldContain, "response.web_search_call.in_progress")
		So(eventTypes, ShouldContain, "response.web_search_call.searching")
		So(eventTypes, ShouldContain, "response.web_search_call.search_query.delta")
		So(eventTypes, ShouldContain, "response.web_search_call.search_query.done")
		So(eventTypes, ShouldContain, "response.web_search_call.completed")

		searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.web_search_call.search_query.done")
		So(len(searchDone), ShouldEqual, 1)
		So(searchDone[0]["item_id"], ShouldEqual, "wsc_call_ws_lifecycle")
		So(searchDone[0]["query"], ShouldEqual, "golang")
	})
}

func TestConvertOpenAIChatToResponses_BuiltinToolItemsUseStructuredArguments(t *testing.T) {
	Convey("builtin tool output item 使用 client execution 与结构化 arguments", t, func() {
		Convey("tool_search added done completed output 均使用 object arguments 且 item id 为 tsc 前缀", func() {
			chunks := []string{
				`data: {"id":"resp_tsc_shape","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_tsc_shape","type":"function","function":{"name":"tool_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_tsc_shape","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"spawn agent\",\"limit\":5}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_tsc_shape","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}

			var param any
			var allEvents []string
			reqBody := codexRequestWithBuiltinTool("tool_search")
			for _, chunk := range chunks {
				ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
				allEvents = append(allEvents, ev...)
			}

			added := parseOutputItemAdded(allEvents)
			var addedTool map[string]interface{}
			for _, addedItem := range added {
				item := addedItem["item"].(map[string]interface{})
				if item["type"] == "tool_search_call" {
					addedTool = item
				}
			}
			So(addedTool, ShouldNotBeNil)
			So(addedTool["id"], ShouldEqual, "tsc_call_tsc_shape")
			So(addedTool["execution"], ShouldEqual, "client")
			args, ok := addedTool["arguments"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(args, ShouldResemble, map[string]interface{}{})

			done := parseOutputItemDone(allEvents)
			var doneTool map[string]interface{}
			for _, item := range done {
				if item["type"] == "tool_search_call" {
					doneTool = item
				}
			}
			So(doneTool, ShouldNotBeNil)
			So(doneTool["id"], ShouldEqual, "tsc_call_tsc_shape")
			So(doneTool["execution"], ShouldEqual, "client")
			doneArgs, ok := doneTool["arguments"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(doneArgs["query"], ShouldEqual, "spawn agent")
			So(doneArgs["limit"], ShouldEqual, float64(5))

			output := parseCompletedOutput(allEvents)
			So(output, ShouldNotBeNil)
			var outputTool map[string]interface{}
			for _, outputItem := range output {
				item := outputItem.(map[string]interface{})
				if item["type"] == "tool_search_call" {
					outputTool = item
				}
			}
			So(outputTool, ShouldNotBeNil)
			So(outputTool["id"], ShouldEqual, "tsc_call_tsc_shape")
			So(outputTool["execution"], ShouldEqual, "client")
			outputArgs, ok := outputTool["arguments"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outputArgs["query"], ShouldEqual, "spawn agent")
			So(outputArgs["limit"], ShouldEqual, float64(5))

			searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
			So(len(searchDone), ShouldEqual, 1)
			So(searchDone[0]["item_id"], ShouldEqual, "tsc_call_tsc_shape")
		})

		Convey("web_search 合法 JSON 不双重字符串化并带 client execution", func() {
			chunks := []string{
				`data: {"id":"resp_wsc_shape","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_wsc_shape","type":"function","function":{"name":"web_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_wsc_shape","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"golang\",\"limit\":3}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_wsc_shape","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}

			var param any
			var allEvents []string
			reqBody := codexRequestWithBuiltinTool("web_search")
			for _, chunk := range chunks {
				ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
				allEvents = append(allEvents, ev...)
			}

			added := parseOutputItemAdded(allEvents)
			var addedTool map[string]interface{}
			for _, addedItem := range added {
				item := addedItem["item"].(map[string]interface{})
				if item["type"] == "web_search_call" {
					addedTool = item
				}
			}
			So(addedTool, ShouldNotBeNil)
			So(addedTool["execution"], ShouldEqual, "client")
			addedArgs, ok := addedTool["arguments"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(addedArgs, ShouldResemble, map[string]interface{}{})

			done := parseOutputItemDone(allEvents)
			var doneTool map[string]interface{}
			for _, item := range done {
				if item["type"] == "web_search_call" {
					doneTool = item
				}
			}
			So(doneTool, ShouldNotBeNil)
			So(doneTool["execution"], ShouldEqual, "client")
			doneArgs, ok := doneTool["arguments"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(doneArgs["query"], ShouldEqual, "golang")
			So(doneArgs["limit"], ShouldEqual, float64(3))

			output := parseCompletedOutput(allEvents)
			So(output, ShouldNotBeNil)
			var outputTool map[string]interface{}
			for _, outputItem := range output {
				item := outputItem.(map[string]interface{})
				if item["type"] == "web_search_call" {
					outputTool = item
				}
			}
			So(outputTool, ShouldNotBeNil)
			So(outputTool["execution"], ShouldEqual, "client")
			outputArgs, ok := outputTool["arguments"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outputArgs["query"], ShouldEqual, "golang")
			So(outputArgs["limit"], ShouldEqual, float64(3))
		})

		Convey("builtin tool 非合法 JSON 仍保持字符串兼容", func() {
			chunks := []string{
				`data: {"id":"resp_tsc_raw_args","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_tsc_raw_args","type":"function","function":{"name":"tool_search","arguments":"raw query text"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_tsc_raw_args","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}

			var param any
			var allEvents []string
			reqBody := codexRequestWithBuiltinTool("tool_search")
			for _, chunk := range chunks {
				ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
				allEvents = append(allEvents, ev...)
			}

			done := parseOutputItemDone(allEvents)
			var doneTool map[string]interface{}
			for _, item := range done {
				if item["type"] == "tool_search_call" {
					doneTool = item
				}
			}
			So(doneTool, ShouldNotBeNil)
			So(doneTool["execution"], ShouldEqual, "client")
			So(doneTool["arguments"], ShouldEqual, "raw query text")
		})
	})
}

func TestConvertOpenAIChatToResponses_ToolSearchSearchQueryDoneFallsBackToRawArguments(t *testing.T) {
	Convey("tool_search search_query.done 在非对象或缺少 query 字段时回退原始 arguments", t, func() {
		Convey("arguments 不是合法 JSON 时，query 回退为原始字符串", func() {
			chunks := []string{
				`data: {"id":"resp_ts_raw_fallback","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_raw_fallback","type":"function","function":{"name":"tool_search","arguments":"raw query text"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_ts_raw_fallback","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}

			var param any
			var allEvents []string
			reqBody := codexRequestWithBuiltinTool("tool_search")
			for _, chunk := range chunks {
				ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
				allEvents = append(allEvents, ev...)
			}

			searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
			So(len(searchDone), ShouldEqual, 1)
			So(searchDone[0]["query"], ShouldEqual, "raw query text")
		})

		Convey("arguments 是 JSON object 但没有 query 字段时，query 回退为原始 JSON 字符串", func() {
			chunks := []string{
				`data: {"id":"resp_ts_missing_query","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_missing_query","type":"function","function":{"name":"tool_search","arguments":"{\"keyword\":\"utility subagent\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_ts_missing_query","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}

			var param any
			var allEvents []string
			reqBody := codexRequestWithBuiltinTool("tool_search")
			for _, chunk := range chunks {
				ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
				allEvents = append(allEvents, ev...)
			}

			searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
			So(len(searchDone), ShouldEqual, 1)
			So(searchDone[0]["query"], ShouldEqual, `{"keyword":"utility subagent"}`)
		})
	})
}

func TestConvertOpenAIChatToResponses_PreservesWhitespaceOnlyContent(t *testing.T) {
	Convey("纯空白正文无 reasoning 时 completed output 保留原始 message", t, func() {
		chunks := []string{
			`data: {"id":"resp_space","choices":[{"index":0,"delta":{"role":"assistant","content":"\n \t"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}

		events := sendChunks(chunks, true)
		output := parseCompletedOutput(events)

		So(output, ShouldNotBeNil)
		So(len(output), ShouldEqual, 1)
		msg := output[0].(map[string]interface{})
		So(msg["type"], ShouldEqual, "message")
		content := msg["content"].([]interface{})
		So(content[0].(map[string]interface{})["text"], ShouldEqual, "\n \t")
	})
}

func TestConvertOpenAIChatToResponses_PreservesLeadingWhitespaceChunks(t *testing.T) {
	Convey("前导空白分片正文 completed output 保留完整文本", t, func() {
		chunks := []string{
			`data: {"id":"resp_leading","choices":[{"index":0,"delta":{"role":"assistant","content":"\n"},"finish_reason":null}]}`,
			`data: {"id":"resp_leading","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}

		events := sendChunks(chunks, true)
		output := parseCompletedOutput(events)

		So(output, ShouldNotBeNil)
		So(len(output), ShouldEqual, 1)
		msg := output[0].(map[string]interface{})
		So(msg["type"], ShouldEqual, "message")
		content := msg["content"].([]interface{})
		So(content[0].(map[string]interface{})["text"], ShouldEqual, "\nHello")
	})
}

func TestConvertOpenAIChatToResponses_LeadingWhitespaceBeforeToolKeepsMessageAndToolIndex(t *testing.T) {
	Convey("前导空白 + tool call 不吞 message，tool 输出索引跟随 message", t, func() {
		chunks := []string{
			`data: {"id":"resp_space_tool","choices":[{"index":0,"delta":{"role":"assistant","content":"\n"},"finish_reason":null}]}`,
			`data: {"id":"resp_space_tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ws_space","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"go\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_space_tool","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("web_search")
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, true)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)
		var addedTool map[string]interface{}
		for _, addedItem := range added {
			item := addedItem["item"].(map[string]interface{})
			if item["type"] == "web_search_call" {
				addedTool = item
			}
		}
		So(addedTool, ShouldNotBeNil)
		So(addedTool["id"], ShouldEqual, "wsc_call_ws_space")
		addedSummaries := parseOutputItemEventSummaries(allEvents, "response.output_item.added")
		doneSummaries := parseOutputItemEventSummaries(allEvents, "response.output_item.done")
		assertUniqueOutputIndexes(addedSummaries)
		assertUniqueOutputIndexes(doneSummaries)

		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		So(len(output), ShouldEqual, 2)
		msg := output[0].(map[string]interface{})
		So(msg["type"], ShouldEqual, "message")
		content := msg["content"].([]interface{})
		So(content[0].(map[string]interface{})["text"], ShouldEqual, "\n")
		tool := output[1].(map[string]interface{})
		So(tool["type"], ShouldEqual, "web_search_call")
		So(tool["id"], ShouldEqual, "wsc_call_ws_space")
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_Namespace(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 流式 first chunk 含 namespace 工具调用", t, func() {
		// 上游 chat 协议 first chunk 同时携带 id + function.name（典型流式行为）
		chunks := []string{
			`data: {"id":"resp_ns","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ns_1","type":"function","function":{"name":"myapp__exec","arguments":""}}]},"finish_reason":null}]}`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithNamespace()
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)

		So(len(added), ShouldEqual, 1)
		item := added[0]["item"].(map[string]interface{})
		So(item["type"], ShouldEqual, "function_call")
		So(item["name"], ShouldEqual, "exec")
		So(item["namespace"], ShouldEqual, "myapp__")
		So(item["call_id"], ShouldEqual, "call_ns_1")
		So(item["id"], ShouldEqual, "fc_call_ns_1")
		So(item["arguments"], ShouldEqual, "")
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_PlainFunction(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 流式 first chunk 含普通 function 工具调用", t, func() {
		chunks := []string{
			`data: {"id":"resp_pf","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_pf_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithFunction()
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)

		So(len(added), ShouldEqual, 1)
		item := added[0]["item"].(map[string]interface{})
		So(item["type"], ShouldEqual, "function_call")
		So(item["name"], ShouldEqual, "get_weather")
		// 普通 function 无 namespace 字段
		_, hasNs := item["namespace"]
		So(hasNs, ShouldBeFalse)
		So(item["call_id"], ShouldEqual, "call_pf_1")
		So(item["id"], ShouldEqual, "fc_call_pf_1")
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_CustomTool(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 流式 first chunk 含 custom proxy 工具调用（apply_patch）", t, func() {
		chunks := []string{
			`data: {"id":"resp_ap","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ap_1","type":"function","function":{"name":"apply_patch_add_file","arguments":"{\"path\":\"a.txt\",\"content\":\"x\"}"}}]},"finish_reason":null}]}`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithApplyPatch()
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)

		So(len(added), ShouldEqual, 1)
		item := added[0]["item"].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["name"], ShouldEqual, "apply_patch")
		// 流式 first chunk 时 input 字段尚未完整还原，addToolCallItemIfNeeded 暂留空字符串
		// 完整 input 在 closeFuncBlocks 时由 reconstructCustomToolCallInput 还原（见 ChatFlowContinuity 测试）
		So(item["input"], ShouldEqual, "")
		So(item["call_id"], ShouldEqual, "call_ap_1")
		So(item["id"], ShouldEqual, "ctc_call_ap_1")
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_UnknownTool(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 流式 first chunk 含未知 tool_call（不在 CodexCtx 中）", t, func() {
		chunks := []string{
			`data: {"id":"resp_uk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_uk_1","type":"function","function":{"name":"unknown_fn","arguments":""}}]},"finish_reason":null}]}`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithFunction() // 请求里只注册 get_weather
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)

		So(len(added), ShouldEqual, 1)
		item := added[0]["item"].(map[string]interface{})
		// 未知 tool 走 function_call fallback，name 保留原值
		So(item["type"], ShouldEqual, "function_call")
		So(item["name"], ShouldEqual, "unknown_fn")
		_, hasNs := item["namespace"]
		So(hasNs, ShouldBeFalse)
		So(item["call_id"], ShouldEqual, "call_uk_1")
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_NilRequest(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 传 nil originalRequestRawJSON 时退化", t, func() {
		// 上游 first chunk 含 namespace 风格 tool_call
		chunks := []string{
			`data: {"id":"resp_nil","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_nil_1","type":"function","function":{"name":"myapp__exec","arguments":""}}]},"finish_reason":null}]}`,
		}

		// 用新入口但传 nil
		var paramNew any
		var newEvents []string
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(nil, nil, []byte(chunk), &paramNew, false)
			newEvents = append(newEvents, ev...)
		}

		// 用旧 5 参数入口（实际现在也是 thin wrapper，行为应一致）
		var paramOld any
		var oldEvents []string
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponses(nil, nil, []byte(chunk), &paramOld, false)
			oldEvents = append(oldEvents, ev...)
		}

		// 新旧入口输出在 nil 场景下必须完全一致（100% 行为兼容）
		So(len(newEvents), ShouldEqual, len(oldEvents))
		for i := range newEvents {
			So(newEvents[i], ShouldEqual, oldEvents[i])
		}

		// 新入口在 nil 时：name 字段保留原值（OpenAINameForFunctionTool fallback），无 namespace 字段
		added := parseOutputItemAdded(newEvents)
		So(len(added), ShouldEqual, 1)
		item := added[0]["item"].(map[string]interface{})
		So(item["type"], ShouldEqual, "function_call")
		So(item["name"], ShouldEqual, "myapp__exec")
		_, hasNs := item["namespace"]
		So(hasNs, ShouldBeFalse)
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_WithOriginalRequest(t *testing.T) {
	Convey("ConvertOpenAIChatToResponses: 传入 originalRequestRawJSON 时应保留 namespace 与 custom_tool_call 语义", t, func() {
		reqBody := []byte(`{
			"model": "codex-test",
			"tools": [
				{"type": "custom", "name": "apply_patch", "description": "patch files"},
				{
					"type": "namespace",
					"name": "team__",
					"tools": [
						{"type": "function", "name": "spawn_agent", "description": "spawn an agent", "parameters": {"type": "object"}}
					]
				}
			]
		}`)
		chunk := `data: {"id":"resp_sem","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_sem_a","type":"function","function":{"name":"apply_patch_add_file","arguments":"{\"path\":\"a.txt\",\"content\":\"x\"}"}},{"index":1,"id":"call_sem_b","type":"function","function":{"name":"team__spawn_agent","arguments":"{}"}}]},"finish_reason":null}]}`

		var param any
		events := ConvertOpenAIChatToResponses(reqBody, nil, []byte(chunk), &param, false)
		added := parseOutputItemAdded(events)

		So(len(added), ShouldEqual, 2)

		customItem := added[0]["item"].(map[string]interface{})
		So(customItem["type"], ShouldEqual, "custom_tool_call")
		So(customItem["name"], ShouldEqual, "apply_patch")
		So(customItem["call_id"], ShouldEqual, "call_sem_a")
		So(customItem["id"], ShouldEqual, "ctc_call_sem_a")

		nsItem := added[1]["item"].(map[string]interface{})
		So(nsItem["type"], ShouldEqual, "function_call")
		So(nsItem["name"], ShouldEqual, "spawn_agent")
		So(nsItem["namespace"], ShouldEqual, "team__")
		So(nsItem["call_id"], ShouldEqual, "call_sem_b")
		So(nsItem["id"], ShouldEqual, "fc_call_sem_b")
	})
}

func TestConvertOpenAIChatToResponses_DeferredNamespaceToolsFromInput(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: input 中发现的 deferred namespace tools 也应参与 Chat→Responses function_call 反扁平化", t, func() {
		reqBody := []byte(`{
			"model": "codex-test",
			"tools": [
				{"type": "function", "name": "existing_tool", "description": "existing", "parameters": {"type": "object", "properties": {}}}
			],
			"input": [
				{
					"type": "tool_search_output",
					"call_id": "ts_call_3",
					"tools": [
						{
							"type": "namespace",
							"name": "multi_agent_v1",
							"tools": [
								{
									"type": "function",
									"name": "spawn_agent",
									"description": "spawn an agent",
									"parameters": {
										"type": "object",
										"properties": {
											"agent_type": {"type": "string"},
											"message": {"type": "string"}
										}
									}
								}
							]
						}
					]
				}
			]
		}`)

		chunks := []string{
			`data: {"id":"resp_deferred_ns","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_deferred_ns_1","type":"function","function":{"name":"multi_agent_v1__spawn_agent","arguments":"{\"agent_type\":\"utility\",\"message\":\"delegate work\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_deferred_ns","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)
		So(len(added), ShouldEqual, 1)
		addedItem := added[0]["item"].(map[string]interface{})
		So(addedItem["type"], ShouldEqual, "function_call")
		So(addedItem["name"], ShouldEqual, "spawn_agent")
		So(addedItem["namespace"], ShouldEqual, "multi_agent_v1")
		So(addedItem["call_id"], ShouldEqual, "call_deferred_ns_1")
		So(addedItem["arguments"], ShouldEqual, `{"agent_type":"utility","message":"delegate work"}`)

		done := parseOutputItemDone(allEvents)
		So(len(done), ShouldEqual, 1)
		doneItem := done[0]
		So(doneItem["type"], ShouldEqual, "function_call")
		So(doneItem["name"], ShouldEqual, "spawn_agent")
		So(doneItem["namespace"], ShouldEqual, "multi_agent_v1")
		So(doneItem["call_id"], ShouldEqual, "call_deferred_ns_1")
		So(doneItem["arguments"], ShouldEqual, `{"agent_type":"utility","message":"delegate work"}`)

		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		So(len(output), ShouldEqual, 1)
		fcItem := output[0].(map[string]interface{})
		So(fcItem["type"], ShouldEqual, "function_call")
		So(fcItem["name"], ShouldEqual, "spawn_agent")
		So(fcItem["namespace"], ShouldEqual, "multi_agent_v1")
		So(fcItem["call_id"], ShouldEqual, "call_deferred_ns_1")
		So(fcItem["arguments"], ShouldEqual, `{"agent_type":"utility","message":"delegate work"}`)
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_ChatFlowContinuity(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 完整流式流程（含多 chunk arguments 累积 + closeFuncBlocks）", t, func() {
		// 模拟 OpenAI 风格流式 tool_calls：first chunk 给 id+name，后续 chunk 累积 arguments
		chunks := []string{
			// chunk 1: id + name + arguments 第一段
			`data: {"id":"resp_cf","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_cf_1","type":"function","function":{"name":"myapp__exec","arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
			// chunk 2: arguments 第二段（无 id 字段，OpenAI 流式标准行为）
			`data: {"id":"resp_cf","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
			// chunk 3: finish_reason 触发 closeFuncBlocks
			`data: {"id":"resp_cf","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithNamespace()
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		// 1. output_item.added 事件带 name + namespace
		added := parseOutputItemAdded(allEvents)
		So(len(added), ShouldEqual, 1)
		item := added[0]["item"].(map[string]interface{})
		So(item["type"], ShouldEqual, "function_call")
		So(item["name"], ShouldEqual, "exec")
		So(item["namespace"], ShouldEqual, "myapp__")
		So(item["call_id"], ShouldEqual, "call_cf_1")
		So(item["id"], ShouldEqual, "fc_call_cf_1")

		// 2. closeFuncBlocks 输出的 output_item.done 与 #4 非流式对称：id=fc_<callID>, status=completed, name=exec, namespace=myapp__
		output := parseCompletedOutput(allEvents)
		So(len(output), ShouldEqual, 1)
		fcItem, ok := output[0].(map[string]interface{})
		So(ok, ShouldBeTrue)
		So(fcItem["type"], ShouldEqual, "function_call")
		So(fcItem["id"], ShouldEqual, "fc_call_cf_1")
		So(fcItem["status"], ShouldEqual, "completed")
		So(fcItem["name"], ShouldEqual, "exec")
		So(fcItem["namespace"], ShouldEqual, "myapp__")
		So(fcItem["call_id"], ShouldEqual, "call_cf_1")
		So(fcItem["arguments"], ShouldEqual, "{\"cmd\":\"ls\"}")
	})
}

func TestConvertOpenAIChatToResponses_OutputItemAdded_MultipleTools(t *testing.T) {
	Convey("ConvertOpenAIChatToResponsesWithContext: 同一流里含 2 个 tool_calls（先 apply_patch 后 exec）", t, func() {
		// 请求里同时注册 apply_patch（custom）和 myapp__exec（namespace）
		reqBody := []byte(`{
			"model": "codex-test",
			"tools": [
				{"type": "custom", "name": "apply_patch", "description": "patch files"},
				{
					"type": "namespace",
					"name": "myapp__",
					"tools": [
						{"type": "function", "name": "exec", "description": "run cmd", "parameters": {"type": "object"}}
					]
				}
			]
		}`)

		// 同一流里两个 tool_calls：index=0 是 apply_patch 代理，index=1 是 namespace 工具
		chunks := []string{
			`data: {"id":"resp_mt","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_mt_a","type":"function","function":{"name":"apply_patch_add_file","arguments":"{\"path\":\"a.txt\",\"content\":\"x\"}"}},{"index":1,"id":"call_mt_b","type":"function","function":{"name":"myapp__exec","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_mt","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		for _, chunk := range chunks {
			ev := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false)
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)
		So(len(added), ShouldEqual, 2)

		// 第一个：apply_patch → custom_tool_call, name=apply_patch
		item0 := added[0]["item"].(map[string]interface{})
		So(item0["type"], ShouldEqual, "custom_tool_call")
		So(item0["name"], ShouldEqual, "apply_patch")
		So(item0["call_id"], ShouldEqual, "call_mt_a")
		So(item0["id"], ShouldEqual, "ctc_call_mt_a")

		// 第二个：myapp__exec → function_call, name=exec, namespace=myapp__
		item1 := added[1]["item"].(map[string]interface{})
		So(item1["type"], ShouldEqual, "function_call")
		So(item1["name"], ShouldEqual, "exec")
		So(item1["namespace"], ShouldEqual, "myapp__")
		So(item1["call_id"], ShouldEqual, "call_mt_b")
		So(item1["id"], ShouldEqual, "fc_call_mt_b")
	})
}
