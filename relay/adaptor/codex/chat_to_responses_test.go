package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pai801/myapi/relay/model"
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

// mustConvertEvents 是测试适配层：把转换错误显式暴露为 panic（不 swallow error）。
// 仅用于历史合法流式回归样本——若某合法样本触发 error，说明转换器回归破坏或样本畸形，测试立即失败。
// 需要断言 error 行为的新用例直接调用 ConvertOpenAIChatToResponsesWithContext 的 (events, error) 形式。
func mustConvertEvents(events []string, err error) []string {
	if err != nil {
		panic(err)
	}
	return events
}

// sendChunks 模拟发送一系列 SSE chunk 并返回最终完成事件
func sendChunks(chunks []string, fallback bool) []string {
	var param any
	var allEvents []string
	for _, chunk := range chunks {
		events := mustConvertEvents(ConvertOpenAIChatToResponses(nil, nil, []byte(chunk), &param, fallback))
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

// parseOutputItemDoneArguments 返回指定 item type 的 output_item.done 事件里 item.arguments 的原始 gjson
// 结果，用于区分「内嵌 JSON 对象」与「字符串」两种形态（tool_search_call 应为对象）。
func parseOutputItemDoneArguments(events []string, itemType string) gjson.Result {
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
		if parsed.Get("item.type").String() != itemType {
			continue
		}
		return parsed.Get("item.arguments")
	}
	return gjson.Result{}
}

// assertSSEDataLinesValidJSON 断言所有 SSE 事件的 data: 行均为合法 JSON（[DONE] 哨兵除外），
// 且 data 载荷必须是单物理行。
// 解析类 helper（parseOutputItemDoneArguments 等）内部有 `if !gjson.Valid(dataStr) { continue }`，
// 会静默跳过已损坏帧导致回归假通过；本 helper 显式拦下非法帧，保护 tool_search_call 内嵌对象
// 注入未闭合 JSON 破坏外层帧的 P0 场景。
// 单行性断言用于挡住「SetRaw 把含裸换行的对象原样注入，破坏 SSE 帧」的 P1 场景：gjson.Valid 允许
// LF/CR 作为 token 间空白，会把跨物理行的载荷当「合法的含空白 JSON」放行，但 SSE 写出层是逐字写出
// （`data: %s\n\n`），未做按行 `data: ` 前缀归一化，客户端按物理行拆分后会丢掉无 `data: ` 前缀的行。
func assertSSEDataLinesValidJSON(t *testing.T, events []string) {
	t.Helper()
	for _, evt := range events {
		idx := indexOf(evt, "data: ")
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len("data: "):])
		if dataStr == "[DONE]" {
			continue
		}
		if strings.ContainsAny(dataStr, "\n\r") {
			t.Fatalf("SSE data 载荷含裸换行（必须单行，否则客户端按物理行拆分后丢帧）: %q", dataStr)
		}
		if !gjson.Valid(dataStr) {
			t.Fatalf("SSE data 行不是合法 JSON: %s", dataStr)
		}
	}
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, true))
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
		So(addedTool["id"], ShouldEqual, "ts_call_ts_1")
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
		So(doneTool["id"], ShouldEqual, "ts_call_ts_1")
		So(doneTool["call_id"], ShouldEqual, "call_ts_1")
		So(doneTool["arguments"], ShouldResemble, map[string]interface{}{"query": "multi-agent subagent spawn utility"})
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
		So(tool["id"], ShouldEqual, "ts_call_ts_1")
		So(tool["call_id"], ShouldEqual, "call_ts_1")
		So(tool["name"], ShouldEqual, "tool_search")
		So(tool["arguments"], ShouldResemble, map[string]interface{}{"query": "multi-agent subagent spawn utility"})
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
			allEvents = append(allEvents, ev...)
		}

		added := parseOutputItemAdded(allEvents)
		So(len(added), ShouldEqual, 1)
		addedTool := added[0]["item"].(map[string]interface{})
		So(addedTool["type"], ShouldEqual, "web_search_call")
		So(addedTool["id"], ShouldEqual, "ws_call_ws_1")

		done := parseOutputItemDone(allEvents)
		So(len(done), ShouldEqual, 1)
		So(done[0]["type"], ShouldEqual, "web_search_call")
		So(done[0]["id"], ShouldEqual, "ws_call_ws_1")
		So(done[0]["arguments"], ShouldEqual, `{"query":"golang"}`)

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
		So(tool["id"], ShouldEqual, "ws_call_ws_1")
		So(tool["call_id"], ShouldEqual, "call_ws_1")
		So(tool["name"], ShouldEqual, "web_search")
		So(tool["arguments"], ShouldEqual, `{"query":"golang"}`)
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
		So(searchDeltas[0]["item_id"], ShouldEqual, "ts_call_ts_lifecycle")
		So(searchDeltas[0]["delta"], ShouldEqual, `{"query":`)
		So(searchDeltas[1]["delta"], ShouldEqual, `"utility subagent"}`)

		searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
		So(len(searchDone), ShouldEqual, 1)
		So(searchDone[0]["item_id"], ShouldEqual, "ts_call_ts_lifecycle")
		So(searchDone[0]["query"], ShouldEqual, "utility subagent")

		addedPos := indexOfToolEvent(allEvents, "response.output_item.added", `"tool_search_call"`)
		inProgressPos := indexOfToolEvent(allEvents, "response.tool_search_call.in_progress", `"ts_call_ts_lifecycle"`)
		searchingPos := indexOfToolEvent(allEvents, "response.tool_search_call.searching", `"ts_call_ts_lifecycle"`)
		deltaPos := indexOfToolEvent(allEvents, "response.tool_search_call.search_query.delta", `"ts_call_ts_lifecycle"`)
		searchDonePos := indexOfToolEvent(allEvents, "response.tool_search_call.search_query.done", `"ts_call_ts_lifecycle"`)
		completedPos := indexOfToolEvent(allEvents, "response.tool_search_call.completed", `"ts_call_ts_lifecycle"`)
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
		So(searchDone[0]["item_id"], ShouldEqual, "ws_call_ws_lifecycle")
		So(searchDone[0]["query"], ShouldEqual, "golang")
	})
}

func TestConvertOpenAIChatToResponses_BuiltinToolItemsUseStructuredArguments(t *testing.T) {
	Convey("builtin tool output item 使用 client execution（仅 tool_search）与 tool_search 对象 arguments、web_search 字符串 arguments、ts_/ws_ 前缀", t, func() {
		Convey("tool_search added done completed output 均使用对象 arguments 且 item id 为 ts 前缀", func() {
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
				ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			So(addedTool["id"], ShouldEqual, "ts_call_tsc_shape")
			So(addedTool["execution"], ShouldEqual, "client")
			So(addedTool["arguments"], ShouldEqual, "")

			done := parseOutputItemDone(allEvents)
			var doneTool map[string]interface{}
			for _, item := range done {
				if item["type"] == "tool_search_call" {
					doneTool = item
				}
			}
			So(doneTool, ShouldNotBeNil)
			So(doneTool["id"], ShouldEqual, "ts_call_tsc_shape")
			So(doneTool["execution"], ShouldEqual, "client")
			So(doneTool["arguments"], ShouldResemble, map[string]interface{}{"query": "spawn agent", "limit": float64(5)})

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
			So(outputTool["id"], ShouldEqual, "ts_call_tsc_shape")
			So(outputTool["execution"], ShouldEqual, "client")
			So(outputTool["arguments"], ShouldResemble, map[string]interface{}{"query": "spawn agent", "limit": float64(5)})

			searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
			So(len(searchDone), ShouldEqual, 1)
			So(searchDone[0]["item_id"], ShouldEqual, "ts_call_tsc_shape")
		})

		Convey("web_search 合法 JSON 原样透传字符串、无 execution 字段且 id 为 ws 前缀", func() {
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
				ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			So(addedTool["id"], ShouldEqual, "ws_call_wsc_shape")
			So(addedTool, ShouldNotContainKey, "execution")
			So(addedTool["arguments"], ShouldEqual, "")

			done := parseOutputItemDone(allEvents)
			var doneTool map[string]interface{}
			for _, item := range done {
				if item["type"] == "web_search_call" {
					doneTool = item
				}
			}
			So(doneTool, ShouldNotBeNil)
			So(doneTool["id"], ShouldEqual, "ws_call_wsc_shape")
			So(doneTool, ShouldNotContainKey, "execution")
			So(doneTool["arguments"], ShouldEqual, `{"query":"golang","limit":3}`)

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
			So(outputTool, ShouldNotContainKey, "execution")
			So(outputTool["arguments"], ShouldEqual, `{"query":"golang","limit":3}`)
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
				ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			So(doneTool["id"], ShouldEqual, "ts_call_tsc_raw_args")
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
				ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
				ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
				allEvents = append(allEvents, ev...)
			}

			searchDone := parseBuiltinToolLifecycleEvents(allEvents, "response.tool_search_call.search_query.done")
			So(len(searchDone), ShouldEqual, 1)
			So(searchDone[0]["query"], ShouldEqual, `{"keyword":"utility subagent"}`)
		})
	})
}

func TestConvertOpenAIChatToResponses_ToolSearchCallArgumentsInlineJSONObject(t *testing.T) {
	Convey("tool_search_call 的 arguments 是内嵌 JSON 对象而非字符串（与 function_call 口径不同）", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts_obj","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_obj","type":"function","function":{"name":"tool_search","arguments":"{\"query\":"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_obj","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_obj","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
			allEvents = append(allEvents, ev...)
		}

		// 流式 output_item.done：item.arguments 必须是内嵌对象，query 可直接取值（非字符串外层）
		doneArgs := parseOutputItemDoneArguments(allEvents, "tool_search_call")
		So(doneArgs.Exists(), ShouldBeTrue)
		So(doneArgs.IsObject(), ShouldBeTrue)
		So(doneArgs.Get("query").Exists(), ShouldBeTrue)
		So(doneArgs.Get("query").Type, ShouldEqual, gjson.String)
		So(doneArgs.Get("query").String(), ShouldEqual, "x")

		// 完整 tool_calls 定稿（response.completed 的 output）同型
		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		var completedTool map[string]interface{}
		for _, outputItem := range output {
			item := outputItem.(map[string]interface{})
			if item["type"] == "tool_search_call" {
				completedTool = item
			}
		}
		So(completedTool, ShouldNotBeNil)
		So(completedTool["arguments"], ShouldResemble, map[string]interface{}{"query": "x"})
	})
}

// TestConvertOpenAIChatToResponses_ToolSearchCallArgumentsNewlineObjectKeepsSSEFrameSingleLine 保护 P1-1：
// arguments 是「美化打印」的合法对象（含裸换行/首尾空白）时，gjson.Valid 允许 LF/CR 作为 token 间空白，
// 若据此走 sjson.SetRaw 会把裸换行原样注入 SSE data 载荷；SSE 写出层逐字写出、不做按行 `data: ` 前缀
// 归一化，客户端按物理行拆分后会丢掉无前缀的行导致载荷截断。必须：形态保持内嵌对象，且 data 载荷单行。
func TestConvertOpenAIChatToResponses_ToolSearchCallArgumentsNewlineObjectKeepsSSEFrameSingleLine(t *testing.T) {
	Convey("tool_search_call arguments 为含裸换行的合法对象时 done 帧保持对象且 data 载荷单行", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts_nl","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_nl","type":"function","function":{"name":"tool_search","arguments":"{\n  \"query\": \"x\"\n}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_nl","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
			allEvents = append(allEvents, ev...)
		}

		// 关键：含裸换行的对象不得以 SetRaw 原样注入；整帧合法且 data 载荷单行。
		assertSSEDataLinesValidJSON(t, allEvents)

		// 形态不得退化：done 帧 item.arguments 仍是内嵌对象，query 可直接取值。
		doneArgs := parseOutputItemDoneArguments(allEvents, "tool_search_call")
		So(doneArgs.Exists(), ShouldBeTrue)
		So(doneArgs.IsObject(), ShouldBeTrue)
		So(doneArgs.Get("query").Exists(), ShouldBeTrue)
		So(doneArgs.Get("query").String(), ShouldEqual, "x")

		// completed 输出同口径：对象而非字符串。
		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		var completedTool map[string]interface{}
		for _, outputItem := range output {
			item := outputItem.(map[string]interface{})
			if item["type"] == "tool_search_call" {
				completedTool = item
			}
		}
		So(completedTool, ShouldNotBeNil)
		So(completedTool["arguments"], ShouldResemble, map[string]interface{}{"query": "x"})
	})
}

func TestConvertOpenAIChatToResponses_ToolSearchCallArgumentsNonJSONFallsBackToString(t *testing.T) {
	Convey("tool_search_call arguments 非法 JSON 时回落字符串且外层 item JSON 仍可解析", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts_bad","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_bad","type":"function","function":{"name":"tool_search","arguments":"not json"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_bad","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
			allEvents = append(allEvents, ev...)
		}

		doneArgs := parseOutputItemDoneArguments(allEvents, "tool_search_call")
		So(doneArgs.Exists(), ShouldBeTrue)
		So(doneArgs.IsObject(), ShouldBeFalse)
		So(doneArgs.Type, ShouldEqual, gjson.String)
		So(doneArgs.String(), ShouldEqual, "not json")

		// 垃圾入垃圾出：外层 data JSON 必须仍合法，禁止非法 JSON 破坏 item
		for _, evt := range allEvents {
			if !strings.Contains(evt, "event: response.output_item.done") {
				continue
			}
			idx := indexOf(evt, "data: ")
			So(idx, ShouldBeGreaterThanOrEqualTo, 0)
			So(gjson.Valid(trimSpace(evt[idx+len("data: "):])), ShouldBeTrue)
		}

		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		var completedTool map[string]interface{}
		for _, outputItem := range output {
			item := outputItem.(map[string]interface{})
			if item["type"] == "tool_search_call" {
				completedTool = item
			}
		}
		So(completedTool, ShouldNotBeNil)
		So(completedTool["arguments"], ShouldEqual, "not json")
	})
}

// TestConvertOpenAIChatToResponses_ToolSearchCallTruncatedArgumentsFallsBackToString 保护 P0-1：
// arguments 是截断的未闭合对象前缀（如 max_output_tokens 截断/流中断）时，gjson.Parse 会前缀宽容地
// 判为 object，若据此 SetRaw 会注入未闭合 JSON 使整条 output_item.done 帧非法；必须回落字符串
// 且整帧仍是合法 JSON。
func TestConvertOpenAIChatToResponses_ToolSearchCallTruncatedArgumentsFallsBackToString(t *testing.T) {
	Convey("tool_search_call arguments 为截断对象前缀时回落字符串且 done 帧仍是合法 JSON", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts_trunc","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_trunc","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"x"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_trunc","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
			allEvents = append(allEvents, ev...)
		}

		// 关键：截断前缀不得被当作对象内嵌而破坏外层帧。
		assertSSEDataLinesValidJSON(t, allEvents)

		doneArgs := parseOutputItemDoneArguments(allEvents, "tool_search_call")
		So(doneArgs.Exists(), ShouldBeTrue)
		So(doneArgs.IsObject(), ShouldBeFalse)
		So(doneArgs.Type, ShouldEqual, gjson.String)
		So(doneArgs.String(), ShouldEqual, `{"query":"x`)

		// completed 输出同口径：字符串，且整体合法。
		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		var completedTool map[string]interface{}
		for _, outputItem := range output {
			item := outputItem.(map[string]interface{})
			if item["type"] == "tool_search_call" {
				completedTool = item
			}
		}
		So(completedTool, ShouldNotBeNil)
		So(completedTool["arguments"], ShouldEqual, `{"query":"x`)
	})
}

// TestConvertOpenAIChatToResponses_ToolSearchCallNullArgumentsFallsBackToString 锁定 P2-1 口径：
// json.Unmarshal("null", &map) 返回 err==nil 但 map==nil，若不显式判空会让 completed 输出 JSON null；
// 统一 helper 后 null 在流式 done 与 completed 均回落字符串 "null"。
func TestConvertOpenAIChatToResponses_ToolSearchCallNullArgumentsFallsBackToString(t *testing.T) {
	Convey("tool_search_call arguments 为 null 时流式 done 与 completed 均为字符串 \"null\"", t, func() {
		chunks := []string{
			`data: {"id":"resp_ts_null","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ts_null","type":"function","function":{"name":"tool_search","arguments":"null"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ts_null","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}

		var param any
		var allEvents []string
		reqBody := codexRequestWithBuiltinTool("tool_search")
		for _, chunk := range chunks {
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
			allEvents = append(allEvents, ev...)
		}

		assertSSEDataLinesValidJSON(t, allEvents)

		doneArgs := parseOutputItemDoneArguments(allEvents, "tool_search_call")
		So(doneArgs.Exists(), ShouldBeTrue)
		So(doneArgs.IsObject(), ShouldBeFalse)
		So(doneArgs.Type, ShouldEqual, gjson.String)
		So(doneArgs.String(), ShouldEqual, "null")

		output := parseCompletedOutput(allEvents)
		So(output, ShouldNotBeNil)
		var completedTool map[string]interface{}
		for _, outputItem := range output {
			item := outputItem.(map[string]interface{})
			if item["type"] == "tool_search_call" {
				completedTool = item
			}
		}
		So(completedTool, ShouldNotBeNil)
		So(completedTool["arguments"], ShouldEqual, "null")
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, true))
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
		So(addedTool["id"], ShouldEqual, "ws_call_ws_space")
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
		So(tool["id"], ShouldEqual, "ws_call_ws_space")
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(nil, nil, []byte(chunk), &paramNew, false))
			newEvents = append(newEvents, ev...)
		}

		// 用旧 5 参数入口（实际现在也是 thin wrapper，行为应一致）
		var paramOld any
		var oldEvents []string
		for _, chunk := range chunks {
			ev := mustConvertEvents(ConvertOpenAIChatToResponses(nil, nil, []byte(chunk), &paramOld, false))
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
		events := mustConvertEvents(ConvertOpenAIChatToResponses(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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

// parseTerminalResponse 提取最后一个终态事件（response.completed / response.incomplete / response.failed）的
// response 对象与事件 type。
func parseTerminalResponse(events []string) (map[string]interface{}, string) {
	var lastType string
	var lastResp map[string]interface{}
	for _, evt := range events {
		isTerminal := strings.Contains(evt, "event: response.completed") ||
			strings.Contains(evt, "event: response.incomplete") ||
			strings.Contains(evt, "event: response.failed")
		if !isTerminal {
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
		lastType = parsed.Get("type").String()
		raw := parsed.Get("response")
		if raw.Exists() && raw.IsObject() {
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(raw.Raw), &m); err == nil {
				lastResp = m
			}
		}
	}
	return lastResp, lastType
}

// parseRefusalDeltaEvents 解析所有 response.refusal.delta 事件的 delta 字段。
func parseRefusalDeltaEvents(events []string) []string {
	var out []string
	for _, evt := range events {
		if !strings.Contains(evt, "event: response.refusal.delta") {
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
		if parsed.Get("type").String() != "response.refusal.delta" {
			continue
		}
		out = append(out, parsed.Get("delta").String())
	}
	return out
}

// parseTerminalOutput 从最后一个终态事件（response.completed / response.incomplete）中提取 response.output 数组。
// 与 parseCompletedOutput 的区别：incomplete 终态的 output 也能提取（length 截断场景）。
func parseTerminalOutput(events []string) []interface{} {
	for i := len(events) - 1; i >= 0; i-- {
		evt := events[i]
		if !strings.Contains(evt, "event: response.completed") && !strings.Contains(evt, "event: response.incomplete") {
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
		if parsed.Get("type").String() != "response.completed" && parsed.Get("type").String() != "response.incomplete" {
			continue
		}
		output := parsed.Get("response.output")
		if output.Exists() && output.IsArray() {
			var result []interface{}
			json.Unmarshal([]byte(output.Raw), &result)
			return result
		}
		return nil
	}
	return nil
}

// parseRefusalDoneEvents 解析所有 response.refusal.done 事件的 refusal 全文。
func parseRefusalDoneEvents(events []string) []string {
	var out []string
	for _, evt := range events {
		if !strings.Contains(evt, "event: response.refusal.done") {
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
		if parsed.Get("type").String() != "response.refusal.done" {
			continue
		}
		out = append(out, parsed.Get("refusal").String())
	}
	return out
}

// parseEventMeta 解析指定事件类型（可选 needle 过滤 data 内容）的 [item_id, output_index] 对，
// 用于断言同一 item 的事件 output_index 恒定、跨 item 唯一并与终态数组一致。
// output_item.added/done 事件的 id 位于 item.id 而非顶层 item_id，需回退提取。
func parseEventMeta(events []string, eventType, needle string) [][2]string {
	var out [][2]string
	for _, evt := range events {
		if !strings.Contains(evt, "event: "+eventType) {
			continue
		}
		if needle != "" && !strings.Contains(evt, needle) {
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
		itemID := parsed.Get("item_id").String()
		if itemID == "" {
			itemID = parsed.Get("item.id").String()
		}
		out = append(out, [2]string{itemID, parsed.Get("output_index").String()})
	}
	return out
}

func TestConvertOpenAIChatToResponses_RefusalStreaming(t *testing.T) {
	Convey("流式 delta.refusal → response.refusal.delta/done + refusal message item", t, func() {

		Convey("T1: 多块 refusal → 事件序列完整、completed output 含 refusal part", func() {
			chunks := []string{
				`data: {"id":"resp_ref","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I cannot help"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref","choices":[{"index":0,"delta":{"refusal":" with that request."},"finish_reason":null}]}`,
				`data: {"id":"resp_ref","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, true)

			eventTypes := collectEventTypes(events)
			expected := []string{
				"response.created",
				"response.in_progress",
				"response.output_item.added",
				"response.content_part.added",
				"response.refusal.delta",
				"response.refusal.delta",
				"response.refusal.done",
				"response.content_part.done",
				"response.output_item.done",
				"response.completed",
			}
			So(eventTypes, ShouldResemble, expected)

			deltas := parseRefusalDeltaEvents(events)
			So(deltas, ShouldResemble, []string{"I cannot help", " with that request."})

			dones := parseRefusalDoneEvents(events)
			So(dones, ShouldResemble, []string{"I cannot help with that request."})

			// content_part.added 的 part 应为 refusal 类型
			var refusalPartSeen bool
			for _, evt := range events {
				if !strings.Contains(evt, "event: response.content_part.added") {
					continue
				}
				idx := indexOf(evt, "data: ")
				if idx < 0 {
					continue
				}
				dataStr := trimSpace(evt[idx+len("data: "):])
				parsed := gjson.Parse(dataStr)
				if parsed.Get("part.type").String() == "refusal" {
					refusalPartSeen = true
				}
			}
			So(refusalPartSeen, ShouldBeTrue)

			output := parseCompletedOutput(events)
			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 1)
			msg := output[0].(map[string]interface{})
			So(msg["type"], ShouldEqual, "message")
			So(msg["role"], ShouldEqual, "assistant")
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 1)
			part := content[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "refusal")
			So(part["refusal"], ShouldEqual, "I cannot help with that request.")
		})

		Convey("T2: 无 refusal 增量 → 不产生任何 refusal 事件", func() {
			chunks := []string{
				`data: {"id":"resp_noref","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
				`data: {"id":"resp_noref","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, true)

			So(len(parseRefusalDeltaEvents(events)), ShouldEqual, 0)
			So(len(parseRefusalDoneEvents(events)), ShouldEqual, 0)
			So(collectEventTypes(events), ShouldNotContain, "response.refusal.delta")
			So(collectEventTypes(events), ShouldNotContain, "response.refusal.done")

			output := parseCompletedOutput(events)
			content := output[0].(map[string]interface{})["content"].([]interface{})
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "output_text")
		})

		Convey("T3: refusal 增量之后接 tool_calls → 先关闭 refusal block 再发 tool 事件，output_index 跨 item 唯一且与终态数组一致", func() {
			chunks := []string{
				`data: {"id":"resp_ref_tool","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I'll decline"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ref_tool","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_tool","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			refusalDonePos := indexOfToolEvent(events, "response.refusal.done", "")
			funcAddedPos := indexOfToolEvent(events, "response.output_item.added", `"function_call"`)
			So(refusalDonePos, ShouldBeGreaterThanOrEqualTo, 0)
			So(funcAddedPos, ShouldBeGreaterThan, refusalDonePos)

			// 同一 refusal item 的所有事件 output_index 恒定（打开时记录的 0），item_id 一致
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done", "response.output_item.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					// content_part 事件需过滤出 refusal part（text 块关闭也会发 content_part.done）
					needle = `"refusal"`
				} else if evtType == "response.output_item.done" {
					// output_item.done 覆盖 message 与 function_call 两类 item，仅断言 refusal message 的
					needle = `"message"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_tool_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// function_call 事件 output_index=1（refusal item 已占据 0，customToolOutputIndex 偏移）
			fcDeltaMeta := parseEventMeta(events, "response.function_call_arguments.delta", "")
			So(fcDeltaMeta, ShouldNotBeEmpty)
			for _, pair := range fcDeltaMeta {
				So(pair[0], ShouldEqual, "fc_call_ref_tool")
				So(pair[1], ShouldEqual, "1")
			}
			fcAddedMeta := parseEventMeta(events, "response.output_item.added", `"function_call"`)
			So(fcAddedMeta, ShouldNotBeEmpty)
			So(fcAddedMeta[0][0], ShouldEqual, "fc_call_ref_tool")
			So(fcAddedMeta[0][1], ShouldEqual, "1")

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_tool_0")
			refusalPart := output[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "I'll decline")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "function_call")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "fc_call_ref_tool")
		})

		Convey("T4: refusal → reasoning 乱序 → refusal 保持打开时 index 0，reasoning 顺延 1，终态数组一致", func() {
			chunks := []string{
				`data: {"id":"resp_ref_rs","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I cannot"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_rs","choices":[{"index":0,"delta":{"reasoning_content":"think step"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_rs","choices":[{"index":0,"delta":{"refusal":" comply"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_rs","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 所有 refusal 事件：item_id=msg_resp_ref_rs_0、output_index=0（含 reasoning 到达后的 delta）
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.output_item.added", "response.content_part.added", "response.content_part.done", "response.output_item.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				} else if evtType == "response.output_item.added" || evtType == "response.output_item.done" {
					// output_item.added/done 也覆盖 reasoning item 的事件，仅断言 refusal message 的
					needle = `"message"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_rs_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// reasoning 事件：item_id=rs_resp_ref_rs_1、output_index=1（refusal 占据 0 后顺延，不冲突）
			for _, evtType := range []string{"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done"} {
				needle := `"reasoning"`
				if evtType != "response.output_item.added" && evtType != "response.output_item.done" {
					// 该事件类型仅 reasoning 块产生，无需 needle 过滤；且其 data 不含 "reasoning" 字面量
					// （part.type 为 summary_text），带 needle 反而过滤空
					needle = ""
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "rs_resp_ref_rs_1")
					So(pair[1], ShouldEqual, "1")
				}
			}

			// 终态数组：refusal 在 0、reasoning 在 1（按流式已定 index 还原顺序）
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_rs_0")
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "message")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "rs_resp_ref_rs_1")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "reasoning")
		})

		Convey("T5: text → refusal 防御路径 → 复用同一 message item 追加 refusal part，不重复 item id", func() {
			chunks := []string{
				`data: {"id":"resp_txt_ref","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
				`data: {"id":"resp_txt_ref","choices":[{"index":0,"delta":{"refusal":"cannot comply"},"finish_reason":null}]}`,
				`data: {"id":"resp_txt_ref","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// message item added/done 各恰好一次（协议 docs/responses-protocol.md §7：marked done
			// 为一次性状态转换，每 item 恰好一条 added/done）。text 段关闭不发 item done，
			// 唯一 done 在终态补发点发出，携带 text+refusal 合并的最终完整 content，
			// 与终态 response.output 逐项一致
			addedMsg := parseEventMeta(events, "response.output_item.added", `"message"`)
			So(len(addedMsg), ShouldEqual, 1)
			So(addedMsg[0][0], ShouldEqual, "msg_resp_txt_ref_0")
			doneMsg := parseEventMeta(events, "response.output_item.done", `"message"`)
			So(len(doneMsg), ShouldEqual, 1)
			So(doneMsg[0], ShouldResemble, [2]string{"msg_resp_txt_ref_0", "0"})
			// 唯一 done 的 content 与终态逐项一致（追加定稿 = item 定稿，非过期快照）
			t5Done := parseMessageItemLastDoneContents(events)["msg_resp_txt_ref_0"]
			t5Terminal := terminalMessageContents(events)["msg_resp_txt_ref_0"]
			t5DoneJSON, _ := json.Marshal(t5Done)
			t5TerminalJSON, _ := json.Marshal(t5Terminal)
			So("msg_resp_txt_ref_0|"+string(t5DoneJSON), ShouldEqual, "msg_resp_txt_ref_0|"+string(t5TerminalJSON))
			So(len(t5Done), ShouldEqual, 2)
			So(t5Done[1].(map[string]interface{})["type"], ShouldEqual, "refusal")

			// refusal part 追加到同一 item：output_index=0，content_index=1（第二个 part）
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done"} {
				meta := parseEventMeta(events, evtType, "")
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_txt_ref_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// 终态数组只有 1 个 message item，content = [output_text, refusal] 合并
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			msg := output[0].(map[string]interface{})
			So(msg["id"], ShouldEqual, "msg_resp_txt_ref_0")
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "output_text")
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "partial")
			So(content[1].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(content[1].(map[string]interface{})["refusal"], ShouldEqual, "cannot comply")
		})

		Convey("T6: 流中途出错（failed 路径）→ refusal block 正常关闭后进入 response.failed", func() {
			chunks := []string{
				`data: {"id":"resp_fail_ref","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I refuse"},"finish_reason":null}]}`,
				`data: {"error": {"message": "upstream boom", "type": "server_error"}}`,
			}
			events := sendChunks(chunks, false)

			eventTypes := collectEventTypes(events)
			So(eventTypes, ShouldContain, "response.refusal.done")
			So(eventTypes, ShouldContain, "response.content_part.done")
			So(eventTypes, ShouldContain, "response.output_item.done")
			// failed 终态统一补发点：已 added 的 refusal item 恒且仅一条 done 且先于 response.failed
			So(parseMessageItemDoneIDs(events), ShouldResemble, []string{"msg_resp_fail_ref_0"})
			failPosNow := indexOfToolEvent(events, "response.failed", "")
			donePosNow := -1
			for i, evt := range events {
				d := dataOfEvent(evt)
				if d.Get("type").String() == "response.output_item.done" && d.Get("item.type").String() == "message" {
					donePosNow = i
				}
			}
			So(failPosNow, ShouldBeGreaterThan, donePosNow)
			failPos := indexOfToolEvent(events, "response.failed", "")
			refusalDonePos := indexOfToolEvent(events, "response.refusal.done", "")
			So(failPos, ShouldBeGreaterThanOrEqualTo, 0)
			So(refusalDonePos, ShouldBeGreaterThanOrEqualTo, 0)
			So(failPos, ShouldBeGreaterThan, refusalDonePos)

			_, termType := parseTerminalResponse(events)
			So(termType, ShouldEqual, "response.failed")
		})

		Convey("T7: refusal → text 乱序 → 先关 refusal 块，text 顺延 index 1，终态 [refusal, text]", func() {
			chunks := []string{
				`data: {"id":"resp_ref_txt","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt","choices":[{"index":0,"delta":{"content":"fallback text"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// refusal 关闭事件齐全（refusal.done / content_part.done 在 text 打开前；
			// item 级 output_item.done 统一由终态补发点在终态事件前发出）
			refusalDonePos := indexOfToolEvent(events, "response.refusal.done", "")
			So(refusalDonePos, ShouldBeGreaterThanOrEqualTo, 0)
			So(indexOfToolEvent(events, "response.content_part.done", `"refusal"`), ShouldBeGreaterThanOrEqualTo, 0)
			textAddedPos := -1
			for i, evt := range events {
				if strings.Contains(evt, "event: response.output_item.added") && strings.Contains(evt, "msg_resp_ref_txt_1") {
					textAddedPos = i
					break
				}
			}
			So(textAddedPos, ShouldBeGreaterThanOrEqualTo, 0)
			So(textAddedPos, ShouldBeGreaterThan, refusalDonePos)

			// 两个 message item 的 output_item.added：id 唯一（text 不复用 msg_0）、index 跨 item 唯一
			addedMsg := parseEventMeta(events, "response.output_item.added", `"message"`)
			So(len(addedMsg), ShouldEqual, 2)
			So(addedMsg[0], ShouldResemble, [2]string{"msg_resp_ref_txt_0", "0"})
			So(addedMsg[1], ShouldResemble, [2]string{"msg_resp_ref_txt_1", "1"})

			// refusal item 全部事件：id=msg_0、output_index=0（打开时记录，不随 text 后到重算）
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_txt_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// text item 全部事件：id=msg_1、output_index=1（refusal 已占 0 后顺延，不撞 index）
			for _, evtType := range []string{"response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"output_text"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_txt_1")
					So(pair[1], ShouldEqual, "1")
				}
			}

			// 终态数组：refusal 在前、text 在后（与流式 output_index 一致）
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			refusalMsg := output[0].(map[string]interface{})
			So(refusalMsg["id"], ShouldEqual, "msg_resp_ref_txt_0")
			refusalPart := refusalMsg["content"].([]interface{})[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "cannot do")
			textMsg := output[1].(map[string]interface{})
			So(textMsg["id"], ShouldEqual, "msg_resp_ref_txt_1")
			textPart := textMsg["content"].([]interface{})[0].(map[string]interface{})
			So(textPart["type"], ShouldEqual, "output_text")
			So(textPart["text"], ShouldEqual, "fallback text")
		})

		Convey("T8: refusal → reasoning → text 乱序 → refusal 0、reasoning 1、text 顺延 2，终态数组一致", func() {
			chunks := []string{
				`data: {"id":"resp_ref_rs_txt","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can't"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_rs_txt","choices":[{"index":0,"delta":{"reasoning_content":"think step"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_rs_txt","choices":[{"index":0,"delta":{"content":"but text"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_rs_txt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// refusal item 全部事件：id=msg_0、output_index=0（含 reasoning 到达后，不重算）
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_rs_txt_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// reasoning item 全部事件：id=rs_1、output_index=1（refusal 占 0 后顺延）
			for _, evtType := range []string{"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done"} {
				needle := ""
				if evtType == "response.output_item.added" || evtType == "response.output_item.done" {
					needle = `"reasoning"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "rs_resp_ref_rs_txt_1")
					So(pair[1], ShouldEqual, "1")
				}
			}

			// text item 全部事件：id=msg_2、output_index=2（reasoning 已占 1 后再顺延，不撞任何 item）
			for _, evtType := range []string{"response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"output_text"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_rs_txt_2")
					So(pair[1], ShouldEqual, "2")
				}
			}

			// 三个 item 的 output_item.added：id 全局唯一、index 跨 item 唯一
			addedAll := parseEventMeta(events, "response.output_item.added", "")
			So(len(addedAll), ShouldEqual, 3)
			So(addedAll[0], ShouldResemble, [2]string{"msg_resp_ref_rs_txt_0", "0"})
			So(addedAll[1], ShouldResemble, [2]string{"rs_resp_ref_rs_txt_1", "1"})
			So(addedAll[2], ShouldResemble, [2]string{"msg_resp_ref_rs_txt_2", "2"})

			// refusal 关闭事件先于 text 打开（互斥切换边界清晰）
			refusalDonePos := indexOfToolEvent(events, "response.refusal.done", "")
			textAddedPos := -1
			for i, evt := range events {
				if strings.Contains(evt, "event: response.output_item.added") && strings.Contains(evt, "msg_resp_ref_rs_txt_2") {
					textAddedPos = i
					break
				}
			}
			So(refusalDonePos, ShouldBeGreaterThanOrEqualTo, 0)
			So(textAddedPos, ShouldBeGreaterThanOrEqualTo, 0)
			So(textAddedPos, ShouldBeGreaterThan, refusalDonePos)

			// 终态数组：[refusal(0), reasoning(1), text(2)] 与流式输出还原一致
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 3)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_rs_txt_0")
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "message")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "rs_resp_ref_rs_txt_1")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "reasoning")
			So(output[2].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_rs_txt_2")
			So(output[2].(map[string]interface{})["type"], ShouldEqual, "message")
			textPart := output[2].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
			So(textPart["type"], ShouldEqual, "output_text")
			So(textPart["text"], ShouldEqual, "but text")
		})

		Convey("T9: refusal → text → tool_calls 乱序 → tool 偏移 2（refusal 0、text 1 均占位），流式事件与终态数组一致", func() {
			chunks := []string{
				`data: {"id":"resp_ref_txt_tool","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I decline"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt_tool","choices":[{"index":0,"delta":{"content":"but here is a note"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt_tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_rtt","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt_tool","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 三个 item 的 output_item.added：id 全局唯一、index 跨 item 唯一（tool 不再撞 text 的 1）
			addedAll := parseEventMeta(events, "response.output_item.added", "")
			So(addedAll, ShouldResemble, [][2]string{
				{"msg_resp_ref_txt_tool_0", "0"},
				{"msg_resp_ref_txt_tool_1", "1"},
				{"fc_call_rtt", "2"},
			})

			// refusal item 全部事件：id=msg_0、output_index=0
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_txt_tool_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// text item 全部事件：id=msg_1、output_index=1
			for _, evtType := range []string{"response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"output_text"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_txt_tool_1")
					So(pair[1], ShouldEqual, "1")
				}
			}

			// function_call 事件全部 output_index=2（refusal 0 + text 1 均占位后 tool 顺延）
			for _, evtType := range []string{"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done"} {
				needle := ""
				if evtType == "response.output_item.added" || evtType == "response.output_item.done" {
					needle = `"function_call"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "fc_call_rtt")
					So(pair[1], ShouldEqual, "2")
				}
			}

			// 终态数组：[refusal(0), text(1), tool(2)] 与流式事件一致
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 3)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_txt_tool_0")
			refusalPart := output[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "I decline")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_txt_tool_1")
			textPart := output[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
			So(textPart["type"], ShouldEqual, "output_text")
			So(textPart["text"], ShouldEqual, "but here is a note")
			So(output[2].(map[string]interface{})["type"], ShouldEqual, "function_call")
			So(output[2].(map[string]interface{})["id"], ShouldEqual, "fc_call_rtt")
		})

		Convey("T10: refusal → text → reasoning 乱序（三重违规）→ reasoning 顺延 2，text 快照 index 1 恒定，终态数组一致", func() {
			chunks := []string{
				`data: {"id":"resp_ref_txt_rs","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can't"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt_rs","choices":[{"index":0,"delta":{"content":"partial reply"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt_rs","choices":[{"index":0,"delta":{"reasoning_content":"think late"},"finish_reason":null}]}`,
				`data: {"id":"resp_ref_txt_rs","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 三个 item 的 output_item.added：id 全局唯一、index 跨 item 唯一（reasoning 2 不撞 text 1）
			addedAll := parseEventMeta(events, "response.output_item.added", "")
			So(addedAll, ShouldResemble, [][2]string{
				{"msg_resp_ref_txt_rs_0", "0"},
				{"msg_resp_ref_txt_rs_1", "1"},
				{"rs_resp_ref_txt_rs_2", "2"},
			})

			// refusal item 全部事件：id=msg_0、output_index=0
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_txt_rs_0")
					So(pair[1], ShouldEqual, "0")
				}
			}

			// text item 全部事件（含 reasoning 到达后的 done 系列）：id=msg_1、output_index=1 恒定
			for _, evtType := range []string{"response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"output_text"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "msg_resp_ref_txt_rs_1")
					So(pair[1], ShouldEqual, "1")
				}
			}

			// text item 的唯一 output_item.done 在终态补发点发出（晚于 refusal 的 done），
			// 取最后一个 message done；每个 message item 恒且仅一条 done
			msgDone := parseEventMeta(events, "response.output_item.done", `"message"`)
			So(msgDone, ShouldNotBeEmpty)
			So(msgDone[len(msgDone)-1], ShouldResemble, [2]string{"msg_resp_ref_txt_rs_1", "1"})
			So(parseMessageItemDoneIDs(events), ShouldResemble,
				[]string{"msg_resp_ref_txt_rs_0", "msg_resp_ref_txt_rs_1"})

			// reasoning item 全部事件：id=rs_2、output_index=2
			for _, evtType := range []string{"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done"} {
				needle := ""
				if evtType == "response.output_item.added" || evtType == "response.output_item.done" {
					needle = `"reasoning"`
				}
				meta := parseEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, pair := range meta {
					So(pair[0], ShouldEqual, "rs_resp_ref_txt_rs_2")
					So(pair[1], ShouldEqual, "2")
				}
			}

			// 终态数组：[refusal(0), text(1), reasoning(2)]，text 位置与 id 后缀（_1）一致
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 3)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_txt_rs_0")
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "message")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "msg_resp_ref_txt_rs_1")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "message")
			textPart := output[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
			So(textPart["type"], ShouldEqual, "output_text")
			So(textPart["text"], ShouldEqual, "partial reply")
			So(output[2].(map[string]interface{})["id"], ShouldEqual, "rs_resp_ref_txt_rs_2")
			So(output[2].(map[string]interface{})["type"], ShouldEqual, "reasoning")
		})
	})
}

func TestConvertOpenAIChatToResponses_FinishReasonLength(t *testing.T) {
	Convey("流式 finish_reason=length → response.incomplete 语义", t, func() {

		Convey("T1: content 被截断 → 终态为 incomplete，含 incomplete_details 与 truncated，output 保留部分文本", func() {
			chunks := []string{
				`data: {"id":"resp_len","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello par"},"finish_reason":null}]}`,
				`data: {"id":"resp_len","choices":[{"index":0,"delta":{"content":"tial"},"finish_reason":null}]}`,
				`data: {"id":"resp_len","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			resp, termType := parseTerminalResponse(events)
			So(termType, ShouldEqual, "response.incomplete")
			So(resp["status"], ShouldEqual, "incomplete")
			details := resp["incomplete_details"].(map[string]interface{})
			So(details["reason"], ShouldEqual, "max_output_tokens")
			So(resp["truncated"], ShouldEqual, true)

			// 被截断的部分文本仍保留在 message item
			var msgContent string
			for _, evt := range events {
				if !strings.Contains(evt, "event: response.output_text.done") {
					continue
				}
				idx := indexOf(evt, "data: ")
				if idx < 0 {
					continue
				}
				dataStr := trimSpace(evt[idx+len("data: "):])
				msgContent = gjson.Parse(dataStr).Get("text").String()
			}
			So(msgContent, ShouldEqual, "Hello partial")
		})

		Convey("T2: tool_calls 结尾且 finish_reason=length → function_call 正常定稿且终态 incomplete", func() {
			chunks := []string{
				`data: {"id":"resp_len_tool","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_len_tool","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_len_tool","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_len_tool","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
				`data: [DONE]`,
			}
			var param any
			var allEvents []string
			reqBody := codexRequestWithFunction()
			for _, chunk := range chunks {
				ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
				allEvents = append(allEvents, ev...)
			}

			resp, termType := parseTerminalResponse(allEvents)
			So(termType, ShouldEqual, "response.incomplete")
			So(resp["status"], ShouldEqual, "incomplete")

			output := parseTerminalOutput(allEvents)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "function_call")
			So(output[0].(map[string]interface{})["arguments"], ShouldEqual, `{"city":"SF"}`)
		})

		Convey("T3: finish chunk 无 delta 仅带 finish_reason → 仍识别为 incomplete", func() {
			chunks := []string{
				`data: {"id":"resp_len_nodelta","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
				`data: {"id":"resp_len_nodelta","choices":[{"index":0,"finish_reason":"length"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			_, termType := parseTerminalResponse(events)
			So(termType, ShouldEqual, "response.incomplete")

			// 无 delta 的 finish chunk 不产生业务事件，但块在 [DONE] 时正常关闭
			output := parseTerminalOutput(events)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "message")
		})

		Convey("T4: finish_reason=stop → 终态为 completed，无 incomplete 字段", func() {
			chunks := []string{
				`data: {"id":"resp_stop","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":null}]}`,
				`data: {"id":"resp_stop","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			resp, termType := parseTerminalResponse(events)
			So(termType, ShouldEqual, "response.completed")
			So(resp["status"], ShouldEqual, "completed")
			// P2-3：统一终态快照与 T6 非流式一致 —— responses §4 规范形态下
			// incomplete_details/truncated 恒存在（completed 分别为 null/false），
			// 旧断言「省略二者」是非规范形态，已按协议重写。
			detailsVal, hasDetails := resp["incomplete_details"]
			So(hasDetails, ShouldBeTrue)
			So(detailsVal, ShouldBeNil)
			truncVal, hasTruncated := resp["truncated"]
			So(hasTruncated, ShouldBeTrue)
			So(truncVal, ShouldEqual, false)
		})

		Convey("T5: finish_reason=content_filter → 终态 incomplete + reason=content_filter + truncated", func() {
			chunks := []string{
				`data: {"id":"resp_cf","choices":[{"index":0,"delta":{"role":"assistant","content":"flagged"},"finish_reason":null}]}`,
				`data: {"id":"resp_cf","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			resp, termType := parseTerminalResponse(events)
			So(termType, ShouldEqual, "response.incomplete")
			So(resp["status"], ShouldEqual, "incomplete")
			details := resp["incomplete_details"].(map[string]interface{})
			So(details["reason"], ShouldEqual, "content_filter")
			So(resp["truncated"], ShouldEqual, true)
		})
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
			ev := mustConvertEvents(ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, false))
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

// parseCompletedUsage 从事件列表中提取 response.completed 事件的 usage 对象。
func parseCompletedUsage(events []string) map[string]interface{} {
	for _, evt := range events {
		dataPrefix := "data: "
		idx := indexOf(evt, dataPrefix)
		if idx < 0 {
			continue
		}
		dataStr := trimSpace(evt[idx+len(dataPrefix):])
		if !gjson.Valid(dataStr) {
			continue
		}
		resp := gjson.Parse(dataStr)
		if resp.Get("type").String() != "response.completed" {
			continue
		}
		usage := resp.Get("response.usage")
		if !usage.Exists() {
			continue
		}
		var result map[string]interface{}
		json.Unmarshal([]byte(usage.Raw), &result)
		return result
	}
	return nil
}

func TestConvertOpenAIChatToResponses_UsageInputTokensKeepsCached(t *testing.T) {
	Convey("流式 usage：input_tokens 保持总输入口径（含 cached），cached 仅经 details 表达（docs §6 与非流式 parseUsage 对齐）", t, func() {
		Convey("OpenAI 上游含 cached：不扣除，total 透传不重复累计", func() {
			chunks := []string{
				`data: {"id":"resp_uc1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`data: {"id":"resp_uc1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":60}}}`,
				`data: [DONE]`,
			}
			usage := parseCompletedUsage(sendChunks(chunks, false))
			So(usage, ShouldNotBeNil)
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["output_tokens"], ShouldEqual, float64(50))
			So(usage["total_tokens"], ShouldEqual, float64(150))
			details, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(details["cached_tokens"], ShouldEqual, float64(60))
		})

		Convey("OpenAI 上游无 cached：details 仍恒存在（responses §6 必填），子字段 0 也输出", func() {
			chunks := []string{
				`data: {"id":"resp_un1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`data: {"id":"resp_un1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`,
				`data: [DONE]`,
			}
			usage := parseCompletedUsage(sendChunks(chunks, false))
			So(usage, ShouldNotBeNil)
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["output_tokens"], ShouldEqual, float64(50))
			So(usage["total_tokens"], ShouldEqual, float64(150))
			details, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(details["cached_tokens"], ShouldEqual, float64(0))
			So(details["cache_write_tokens"], ShouldEqual, float64(0))
			outDetails, ok := usage["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails["reasoning_tokens"], ShouldEqual, float64(0))
		})

		Convey("OpenAI 上游无 total：total = input + output，不把 cached 再加一遍", func() {
			chunks := []string{
				`data: {"id":"resp_unt","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`data: {"id":"resp_unt","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":60}}}`,
				`data: [DONE]`,
			}
			usage := parseCompletedUsage(sendChunks(chunks, false))
			So(usage, ShouldNotBeNil)
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["total_tokens"], ShouldEqual, float64(150))
			details, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(details["cached_tokens"], ShouldEqual, float64(60))
		})

		Convey("Claude 上游：input_tokens 保持上游原值，total 汇集 cache 三件套", func() {
			chunks := []string{
				`data: {"id":"resp_cc1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`data: {"id":"resp_cc1","choices":[],"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":60}}`,
				`data: [DONE]`,
			}
			usage := parseCompletedUsage(sendChunks(chunks, false))
			So(usage, ShouldNotBeNil)
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["output_tokens"], ShouldEqual, float64(50))
			So(usage["total_tokens"], ShouldEqual, float64(210))
			So(usage["cache_read_input_tokens"], ShouldEqual, float64(60))
			// Claude 路径的 cached 同时落入标准字段 input_tokens_details.cached_tokens（responses §6）
			details, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(details["cached_tokens"], ShouldEqual, float64(60))
		})
	})
}

// parseInterleaveEventMeta 解析指定事件的 [item_id, output_index, content_index]。
// 与 parseEventMeta 的差别：额外带出 content_index（断言 refusal part 与 text part 不撞号），
// 字段缺失（如 output_item.added 无 content_index）时返回空串。
func parseInterleaveEventMeta(events []string, eventType, needle string) [][3]string {
	var out [][3]string
	for _, evt := range events {
		if !strings.Contains(evt, "event: "+eventType) {
			continue
		}
		if needle != "" && !strings.Contains(evt, needle) {
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
		itemID := parsed.Get("item_id").String()
		if itemID == "" {
			itemID = parsed.Get("item.id").String()
		}
		contentIndex := ""
		if v := parsed.Get("content_index"); v.Exists() {
			contentIndex = v.String()
		}
		out = append(out, [3]string{itemID, parsed.Get("output_index").String(), contentIndex})
	}
	return out
}

func TestConvertOpenAIChatToResponses_RefusalInterleaveIndexConsistency(t *testing.T) {
	Convey("refusal 与 text 违规交错：同一 item 事件 output_index 恒定、不重复发 output_item.added、终态数组可还原", t, func() {

		Convey("R1: refusal → text → refusal → 第三段 refusal 复用独立 item 的 id/index 快照", func() {
			chunks := []string{
				`data: {"id":"resp_iv1","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do A"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv1","choices":[{"index":0,"delta":{"content":"but text B"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv1","choices":[{"index":0,"delta":{"refusal":"then refuse C"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// message item 只 added 两次：refusal 独立 item（0）与 text item（1），
			// 复用路径不得用同 ID 再发 output_item.added
			So(parseEventMeta(events, "response.output_item.added", `"message"`), ShouldResemble, [][2]string{
				{"msg_resp_iv1_0", "0"},
				{"msg_resp_iv1_1", "1"},
			})

			// refusal 全部事件锁定同一 item：id=msg_0、output_index=0、content_index=0
			// （修复前：追加/复用分支按 ReasoningPartAdded 重算 index，与已发的 added/delta 矛盾）
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				}
				meta := parseInterleaveEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, triple := range meta {
					So(triple[0], ShouldEqual, "msg_resp_iv1_0")
					So(triple[1], ShouldEqual, "0")
					So(triple[2], ShouldEqual, "0")
				}
			}
			// 视为同一 refusal part 的续写：不重发 content_part.added
			So(len(parseInterleaveEventMeta(events, "response.content_part.added", `"refusal"`)), ShouldEqual, 1)

			// text 事件独立占 index 1，不与 refusal item 撞号
			for _, evtType := range []string{"response.output_text.delta", "response.output_text.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"output_text"`
				}
				meta := parseInterleaveEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, triple := range meta {
					So(triple[0], ShouldEqual, "msg_resp_iv1_1")
					So(triple[1], ShouldEqual, "1")
				}
			}

			// 终态数组：[refusal(合并全文), text]，与流式各 item 的 index 一致
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			refusalMsg := output[0].(map[string]interface{})
			So(refusalMsg["id"], ShouldEqual, "msg_resp_iv1_0")
			refusalPart := refusalMsg["content"].([]interface{})[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "cannot do Athen refuse C")
			textMsg := output[1].(map[string]interface{})
			So(textMsg["id"], ShouldEqual, "msg_resp_iv1_1")
			textPart := textMsg["content"].([]interface{})[0].(map[string]interface{})
			So(textPart["type"], ShouldEqual, "output_text")
			So(textPart["text"], ShouldEqual, "but text B")

			// 最后一个 refusal.done 携带合并全文，与终态 item 一致
			dones := parseRefusalDoneEvents(events)
			So(len(dones), ShouldEqual, 2)
			So(dones[len(dones)-1], ShouldEqual, "cannot do Athen refuse C")

			// FAIL-1 锁定（reviewer 探测）：独立 refusal item 复用续写会二次关闭
			// closeRefusalBlock，旧实现每次 close 都发 output_item.done → 同 id 两条 done
			// 且第一条为过期快照。新策略 done 只在终态统一补发点发出：
			// 每个 message item 恒且仅一条 done、先于 response.completed、content 与终态逐项一致
			assertDoneTerminalConsistency(events)
			So(parseMessageItemDoneIDs(events), ShouldResemble,
				[]string{"msg_resp_iv1_0", "msg_resp_iv1_1"})
		})

		Convey("R2: refusal → text → refusal → text → 违规重开的 text 块显式丢弃且不污染 item/终态", func() {
			chunks := []string{
				`data: {"id":"resp_iv2","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do A"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv2","choices":[{"index":0,"delta":{"content":"but text B"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv2","choices":[{"index":0,"delta":{"refusal":"then refuse C"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv2","choices":[{"index":0,"delta":{"content":"late text D should be dropped"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 丢弃决策（二选一取「丢弃 + Warn」而非「另分配新 item id/index」）：
			// 重开 text 会给同一 msg id 再发 added 且 content_index 从 0 重计，与既有 refusal part 冲突；
			// 新建 item 则要与 reasoning/tool 的 index 推导（customToolOutputIndex）联动偏移，改动面过大。
			So(strings.Contains(strings.Join(events, ""), "late text D should be dropped"), ShouldBeFalse)
			So(len(parseInterleaveEventMeta(events, "response.output_text.delta", "")), ShouldEqual, 1)
			So(parseInterleaveEventMeta(events, "response.output_text.done", ""), ShouldResemble,
				[][3]string{{"msg_resp_iv2_1", "1", "0"}})
			So(parseEventMeta(events, "response.output_item.added", `"message"`), ShouldResemble, [][2]string{
				{"msg_resp_iv2_0", "0"},
				{"msg_resp_iv2_1", "1"},
			})
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done"} {
				meta := parseInterleaveEventMeta(events, evtType, "")
				So(meta, ShouldNotBeEmpty)
				for _, triple := range meta {
					So(triple[0], ShouldEqual, "msg_resp_iv2_0")
					So(triple[1], ShouldEqual, "0")
				}
			}

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_iv2_0")
			So(output[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["refusal"],
				ShouldEqual, "cannot do Athen refuse C")
			textMsg := output[1].(map[string]interface{})
			So(textMsg["id"], ShouldEqual, "msg_resp_iv2_1")
			So(textMsg["content"].([]interface{})[0].(map[string]interface{})["text"], ShouldEqual, "but text B")
		})

		Convey("R3: text → reasoning → refusal（追加模式）→ refusal part 继承 text item 的 index 快照", func() {
			chunks := []string{
				`data: {"id":"resp_iv3","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv3","choices":[{"index":0,"delta":{"reasoning_content":"think late"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv3","choices":[{"index":0,"delta":{"refusal":"cannot comply"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 追加模式：refusal part 挂在已关闭的 text message item 上，
			// 所有 refusal 事件必须复用该 item 的 index 快照（0），而非按 ReasoningPartAdded 重算成 1
			So(parseEventMeta(events, "response.output_item.added", `"message"`), ShouldResemble,
				[][2]string{{"msg_resp_iv3_0", "0"}})
			for _, evtType := range []string{"response.refusal.delta", "response.refusal.done", "response.content_part.added", "response.content_part.done"} {
				needle := ""
				if evtType == "response.content_part.added" || evtType == "response.content_part.done" {
					needle = `"refusal"`
				}
				meta := parseInterleaveEventMeta(events, evtType, needle)
				So(meta, ShouldNotBeEmpty)
				for _, triple := range meta {
					So(triple[0], ShouldEqual, "msg_resp_iv3_0")
					So(triple[1], ShouldEqual, "0")
					// refusal 是消息的第二个 part（0 是 output_text），不得与 text part 撞 content_index
					So(triple[2], ShouldEqual, "1")
				}
			}
			for _, evtType := range []string{"response.output_text.delta", "response.output_text.done"} {
				for _, triple := range parseInterleaveEventMeta(events, evtType, "") {
					So(triple[0], ShouldEqual, "msg_resp_iv3_0")
					So(triple[1], ShouldEqual, "0")
				}
			}
			// reasoning 独立 item 占 1（text 已占 0 后顺延）
			for _, evtType := range []string{"response.output_item.added", "response.reasoning_summary_text.delta", "response.output_item.done"} {
				needle := ""
				if evtType == "response.output_item.added" || evtType == "response.output_item.done" {
					needle = `"reasoning"`
				}
				for _, triple := range parseInterleaveEventMeta(events, evtType, needle) {
					So(triple[0], ShouldEqual, "rs_resp_iv3_1")
					So(triple[1], ShouldEqual, "1")
				}
			}

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			msg := output[0].(map[string]interface{})
			So(msg["id"], ShouldEqual, "msg_resp_iv3_0")
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "output_text")
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "partial")
			So(content[1].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(content[1].(map[string]interface{})["refusal"], ShouldEqual, "cannot comply")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "rs_resp_iv3_1")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "reasoning")
		})

		Convey("R4: refusal → text → tool → text → 第二段属正常多段输出，按新 output_text part 追加不丢内容", func() {
			chunks := []string{
				`data: {"id":"resp_iv4","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do A"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv4","choices":[{"index":0,"delta":{"content":"first segment"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_iv4","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_iv4","choices":[{"index":0,"delta":{"content":"second segment"},"finish_reason":null}]}`,
				`data: {"id":"resp_iv4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 守卫不得因粘滞的 RefusalItemInOutput 永久丢弃后续 text 段：第二段必须保留
			So(strings.Contains(strings.Join(events, ""), "second segment"), ShouldBeTrue)

			// message item 仍只 added 一次；重开段复用同一 item 与 output_index，不重发 added
			So(parseEventMeta(events, "response.output_item.added", `"message"`), ShouldResemble, [][2]string{
				{"msg_resp_iv4_0", "0"},
				{"msg_resp_iv4_1", "1"},
			})
			// tool 偏移：refusal 0 + text 1 → function_call 2
			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble,
				[][2]string{{"fc_call_iv4", "2"}})

			// 两段 text 各自一个 content part：content_index 递增，item id / output_index 恒定
			So(parseInterleaveEventMeta(events, "response.content_part.added", `"output_text"`), ShouldResemble, [][3]string{
				{"msg_resp_iv4_1", "1", "0"},
				{"msg_resp_iv4_1", "1", "1"},
			})
			So(parseInterleaveEventMeta(events, "response.output_text.delta", ""), ShouldResemble, [][3]string{
				{"msg_resp_iv4_1", "1", "0"},
				{"msg_resp_iv4_1", "1", "1"},
			})
			// done 系列按「段」而非全文，避免两段 part 内容重复
			So(parseOutputTextDoneByPart(events), ShouldResemble, [][2]string{
				{"0", "first segment"},
				{"1", "second segment"},
			})
			// refusal item 不受影响：仍占 index 0 / content_index 0
			for _, triple := range parseInterleaveEventMeta(events, "response.refusal.delta", "") {
				So(triple[0], ShouldEqual, "msg_resp_iv4_0")
				So(triple[1], ShouldEqual, "0")
				So(triple[2], ShouldEqual, "0")
			}

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 3)
			msg := output[1].(map[string]interface{})
			So(msg["id"], ShouldEqual, "msg_resp_iv4_1")
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "first segment")
			So(content[1].(map[string]interface{})["text"], ShouldEqual, "second segment")
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_iv4_0")
			So(output[2].(map[string]interface{})["id"], ShouldEqual, "fc_call_iv4")
		})
	})
}

// parseOutputTextDoneByPart 解析 response.output_text.done 事件的 [content_index, text]，
// 用于断言多段 text 的 done 按各自 part 而非累积全文发出。
func parseOutputTextDoneByPart(events []string) [][2]string {
	var out [][2]string
	for _, evt := range events {
		if !strings.Contains(evt, "event: response.output_text.done") {
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
		if parsed.Get("type").String() != "response.output_text.done" {
			continue
		}
		out = append(out, [2]string{parsed.Get("content_index").String(), parsed.Get("text").String()})
	}
	return out
}

func TestConvertOpenAIChatToResponses_ToolCallsWithoutIndex(t *testing.T) {
	Convey("上游畸形：delta.tool_calls 缺 index → 按到达顺序分配稳定 key，禁止静默覆盖导致 arguments 串包", t, func() {

		Convey("W1: 两个 call 均无 index，各自 arguments 落到各自 key", func() {
			chunks := []string{
				`data: {"id":"resp_noidx","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_noidx","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_noidx","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_noidx","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"UTC\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_noidx","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			fcDeltas := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.delta")
			So(len(fcDeltas), ShouldEqual, 4)
			So(fcDeltas[0]["item_id"], ShouldEqual, "fc_call_a1")
			So(fcDeltas[0]["delta"], ShouldEqual, `{"city":`)
			So(fcDeltas[1]["item_id"], ShouldEqual, "fc_call_a1")
			So(fcDeltas[1]["delta"], ShouldEqual, `"Paris"}`)
			So(fcDeltas[2]["item_id"], ShouldEqual, "fc_call_b1")
			So(fcDeltas[2]["delta"], ShouldEqual, `{"tz":`)
			So(fcDeltas[3]["item_id"], ShouldEqual, "fc_call_b1")
			So(fcDeltas[3]["delta"], ShouldEqual, `"UTC"}`)

			fcDones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(fcDones), ShouldEqual, 2)
			So(fcDones[0]["item_id"], ShouldEqual, "fc_call_a1")
			So(fcDones[0]["arguments"], ShouldEqual, `{"city":"Paris"}`)
			So(fcDones[1]["item_id"], ShouldEqual, "fc_call_b1")
			So(fcDones[1]["arguments"], ShouldEqual, `{"tz":"UTC"}`)

			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble, [][2]string{
				{"fc_call_a1", "0"},
				{"fc_call_b1", "1"},
			})

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			item0 := output[0].(map[string]interface{})
			So(item0["type"], ShouldEqual, "function_call")
			So(item0["id"], ShouldEqual, "fc_call_a1")
			So(item0["call_id"], ShouldEqual, "call_a1")
			So(item0["name"], ShouldEqual, "get_weather")
			So(item0["arguments"], ShouldEqual, `{"city":"Paris"}`)
			item1 := output[1].(map[string]interface{})
			So(item1["id"], ShouldEqual, "fc_call_b1")
			So(item1["call_id"], ShouldEqual, "call_b1")
			So(item1["name"], ShouldEqual, "get_time")
			So(item1["arguments"], ShouldEqual, `{"tz":"UTC"}`)
		})

		Convey("W2: 每个 chunk 重发同一 id 且无 index → 按 id 复用既有 key，不拆成两个 item", func() {
			chunks := []string{
				`data: {"id":"resp_rep","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_rep","type":"function","function":{"name":"get_weather","arguments":"{\"a\":1"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_rep","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_rep","type":"function","function":{"arguments":",\"b\":2}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_rep","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble,
				[][2]string{{"fc_call_rep", "0"}})
			fcDones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(fcDones), ShouldEqual, 1)
			So(fcDones[0]["arguments"], ShouldEqual, `{"a":1,"b":2}`)
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "fc_call_rep")
			So(output[0].(map[string]interface{})["arguments"], ShouldEqual, `{"a":1,"b":2}`)
		})

		Convey("W3: 首个 delta 既无 index 也无 id（无法关联）→ 跳过并 Warn，不落到 0 号 key 污染后续 call", func() {
			chunks := []string{
				`data: {"id":"resp_orphan","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"orphan\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_orphan","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_ok","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_orphan","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			So(strings.Contains(strings.Join(events, ""), "\"orphan\""), ShouldBeFalse)
			fcDeltas := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.delta")
			So(len(fcDeltas), ShouldEqual, 1)
			So(fcDeltas[0]["item_id"], ShouldEqual, "fc_call_ok")
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "fc_call_ok")
			So(output[0].(map[string]interface{})["arguments"], ShouldEqual, "{}")
		})

		Convey("W4: 匿名A → 显式 index 0（异 call id）→ 匿名A 续写 → 双向都不串包", func() {
			// 匿名 call 先占 key 0；显式 index:0 的 call_x 争同一 key → 丢弃该 delta（不接管），
			// 之后匿名 A 的无 index delta 仍落到 A 自己的 item
			chunks := []string{
				`data: {"id":"resp_mix","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"f_a","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mix","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"f_x","arguments":"{\"xpolluted\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mix","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"A\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mix","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			joined := strings.Join(events, "")
			// 争用 key 的显式 delta 整体丢弃：call id / name / arguments 都不出现在任何事件里
			So(strings.Contains(joined, "call_x"), ShouldBeFalse)
			So(strings.Contains(joined, "f_x"), ShouldBeFalse)
			So(strings.Contains(joined, "xpolluted"), ShouldBeFalse)

			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble,
				[][2]string{{"fc_call_a", "0"}})
			fcDeltas := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.delta")
			So(len(fcDeltas), ShouldEqual, 2)
			So(fcDeltas[0]["item_id"], ShouldEqual, "fc_call_a")
			So(fcDeltas[0]["delta"], ShouldEqual, `{"a":`)
			So(fcDeltas[1]["item_id"], ShouldEqual, "fc_call_a")
			So(fcDeltas[1]["delta"], ShouldEqual, `"A"}`)

			fcDones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(fcDones), ShouldEqual, 1)
			So(fcDones[0]["arguments"], ShouldEqual, `{"a":"A"}`)

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["id"], ShouldEqual, "fc_call_a")
			So(item["name"], ShouldEqual, "f_a")
			So(item["arguments"], ShouldEqual, `{"a":"A"}`)
		})

		Convey("W5: 匿名A → 显式 index 0（无 id 的 arguments delta）→ 同样按 key 争用丢弃，不静默并入", func() {
			chunks := []string{
				`data: {"id":"resp_mix2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_w5","type":"function","function":{"name":"get_weather","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mix2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"secret_x\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mix2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 规则锁定：落在匿名 key 上的显式 index delta 一律要求 call id 一致，
			// 缺 id 时无法证明归属，按争用处理丢弃（歧义 delta 不猜），A 的参数保持纯净
			So(strings.Contains(strings.Join(events, ""), "secret_x"), ShouldBeFalse)
			fcDeltas := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.delta")
			So(len(fcDeltas), ShouldEqual, 1)
			So(fcDeltas[0]["item_id"], ShouldEqual, "fc_call_w5")
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["arguments"], ShouldEqual, `{"a":1}`)
		})
	})
}

func TestConvertOpenAIChatToResponses_UsageDetailsRequiredFields(t *testing.T) {
	Convey("responses §6 usage 必填：details 对象恒存在 + chat §8.1 cache_write_tokens 映射", t, func() {

		Convey("U1: usage 全 0 → details 对象与子字段仍恒存在", func() {
			chunks := []string{
				`data: {"id":"resp_u0","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`data: {"id":"resp_u0","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
				`data: [DONE]`,
			}
			usage := parseCompletedUsage(sendChunks(chunks, false))
			So(usage, ShouldNotBeNil)
			inDetails, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(inDetails["cached_tokens"], ShouldEqual, float64(0))
			So(inDetails["cache_write_tokens"], ShouldEqual, float64(0))
			outDetails, ok := usage["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails["reasoning_tokens"], ShouldEqual, float64(0))
		})

		Convey("U2: 仅 cache_write_tokens>0 → 映射进 input_tokens_details，缺失的 cached_tokens 补 0", func() {
			chunks := []string{
				`data: {"id":"resp_uw","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
				`data: {"id":"resp_uw","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cache_write_tokens":40}}}`,
				`data: [DONE]`,
			}
			usage := parseCompletedUsage(sendChunks(chunks, false))
			So(usage, ShouldNotBeNil)
			So(usage["input_tokens"], ShouldEqual, float64(120))
			So(usage["total_tokens"], ShouldEqual, float64(150))
			inDetails, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(inDetails["cache_write_tokens"], ShouldEqual, float64(40))
			So(inDetails["cached_tokens"], ShouldEqual, float64(0))
			outDetails, ok := usage["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails["reasoning_tokens"], ShouldEqual, float64(0))
		})
	})
}

// =============================================================================
// 问题1：匿名（缺 index）tool_call key 分配与显式 index 双向争用（串包防线）
// =============================================================================

func TestConvertOpenAIChatToResponses_ToolCallsMixedIndexCollision(t *testing.T) {
	Convey("匿名 key 与显式 index 混用：显式 delta 撞匿名 key 按 call id 判归属整条丢弃，禁止接管 FuncArgsBuf/FuncCallIDs", t, func() {

		Convey("T1: 匿名A→匿名B（不同 id）分占两个 key，arguments 各归各 item", func() {
			chunks := []string{
				`data: {"id":"resp_mixc1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"b\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"2}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// A、B 各占一个 key（0/1），不互相覆盖
			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble, [][2]string{
				{"fc_call_a", "0"},
				{"fc_call_b", "1"},
			})
			deltas := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.delta")
			So(len(deltas), ShouldEqual, 4)
			So(deltas[0]["item_id"], ShouldEqual, "fc_call_a")
			So(deltas[1]["item_id"], ShouldEqual, "fc_call_a")
			So(deltas[2]["item_id"], ShouldEqual, "fc_call_b")
			So(deltas[3]["item_id"], ShouldEqual, "fc_call_b")
			dones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(dones), ShouldEqual, 2)
			So(dones[0]["arguments"], ShouldEqual, `{"a":1}`)
			So(dones[1]["arguments"], ShouldEqual, `{"b":2}`)
		})

		Convey("T2: 匿名A→显式0（call_x）冲突 → 显式 delta 整条丢弃不接管不合并", func() {
			chunks := []string{
				`data: {"id":"resp_mixc2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_time","arguments":"{\"x\":2}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"evil\":3}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 撞匿名 key 的显式 delta（含缺 id 的续写）全部丢弃：id/name/args 不留任何痕迹
			joined := strings.Join(events, "")
			So(strings.Contains(joined, "call_x"), ShouldBeFalse)
			So(strings.Contains(joined, "get_time"), ShouldBeFalse)
			So(strings.Contains(joined, "evil"), ShouldBeFalse)
			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble,
				[][2]string{{"fc_call_a", "0"}})
			dones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(dones), ShouldEqual, 1)
			So(dones[0]["item_id"], ShouldEqual, "fc_call_a")
			// FuncCallIDs[0] 未被覆盖：A 的 item 仍是 call_a
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["id"], ShouldEqual, "fc_call_a")
			So(item["call_id"], ShouldEqual, "call_a")
			So(item["name"], ShouldEqual, "get_weather")
			So(item["arguments"], ShouldEqual, `{"a":1}`)
		})

		Convey("T3: 匿名A→显式0(call_x)→匿名A 纯 arguments 续写 → A 与 call_x 的 arguments 全文互不污染", func() {
			chunks := []string{
				`data: {"id":"resp_mixc3","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"part\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"get_time","arguments":"{\"evil\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc3","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"A\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"arguments":"\"X\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			joined := strings.Join(events, "")
			// 路径②封闭性：显式接管被丢弃拦死，key0 归属始终是 call_a，
			// A 的无 index 续写落回自己的 item，call_x 的两条 args delta 均不得混入
			So(strings.Contains(joined, "evil"), ShouldBeFalse)
			So(strings.Contains(joined, "\"X\""), ShouldBeFalse)
			So(strings.Contains(joined, "call_x"), ShouldBeFalse)
			dones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(dones), ShouldEqual, 1)
			So(dones[0]["item_id"], ShouldEqual, "fc_call_a")
			So(dones[0]["arguments"], ShouldEqual, `{"part":"A"}`)
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["arguments"], ShouldEqual, `{"part":"A"}`)
		})

		Convey("T4: 匿名A→显式 index:2 正常流→新匿名B → 显式正常流与新匿名分配均不误伤", func() {
			chunks := []string{
				`data: {"id":"resp_mixc4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc4","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_t","type":"function","function":{"name":"get_time","arguments":"{\"t\":"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc4","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"arguments":"\"T\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc4","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"get_date","arguments":"{\"b\":1}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mixc4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 未撞匿名 key 的显式 delta 正常放行；匿名 B 分配跳过被显式 index 占用的 key（0 被 A、2 被 T 占 → B 得 1）
			deltas := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.delta")
			So(len(deltas), ShouldEqual, 4)
			dones := parseFunctionCallArgumentEvents(events, "response.function_call_arguments.done")
			So(len(dones), ShouldEqual, 3)
			So(dones[0]["item_id"], ShouldEqual, "fc_call_a")
			So(dones[0]["arguments"], ShouldEqual, `{"a":1}`)
			So(dones[1]["item_id"], ShouldEqual, "fc_call_b")
			So(dones[1]["arguments"], ShouldEqual, `{"b":1}`)
			So(dones[2]["item_id"], ShouldEqual, "fc_call_t")
			So(dones[2]["arguments"], ShouldEqual, `{"t":"T"}`)
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 3)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "fc_call_a")
			So(output[1].(map[string]interface{})["id"], ShouldEqual, "fc_call_b")
			So(output[2].(map[string]interface{})["id"], ShouldEqual, "fc_call_t")
		})
	})
}

// =============================================================================
// 问题2：RefusalViolationSeen 一次性违规窗口 —— 多段 text 不误伤，紧随 refusal 的直接 text 丢弃
// =============================================================================

func TestConvertOpenAIChatToResponses_MultiSegmentTextAfterRefusal(t *testing.T) {
	Convey("refusal→text→tool→text 第二段 text 保留；紧随 refusal 关闭/重开的直接 text 违规丢弃（R2 不回归）", t, func() {

		Convey("M1: refusal→text→tool→text → 第二段为同 item 第二个 output_text part，不重发 output_item.added，终态含两段 text", func() {
			chunks := []string{
				`data: {"id":"resp_mst","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do"},"finish_reason":null}]}`,
				`data: {"id":"resp_mst","choices":[{"index":0,"delta":{"content":"first seg"},"finish_reason":null}]}`,
				`data: {"id":"resp_mst","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_mst","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_mst","choices":[{"index":0,"delta":{"content":"second seg"},"finish_reason":null}]}`,
				`data: {"id":"resp_mst","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 守卫不得因 refusal 曾占据 output 而永久丢弃后续 text 段
			So(strings.Contains(strings.Join(events, ""), "second seg"), ShouldBeTrue)

			// message item 只 added 两次（refusal 独立 item + text item）；第二段重开复用同一 item 不重发 added
			So(parseEventMeta(events, "response.output_item.added", `"message"`), ShouldResemble, [][2]string{
				{"msg_resp_mst_0", "0"},
				{"msg_resp_mst_1", "1"},
			})
			// 实现决策锁定：tool 分段后的第二段 text 归并进同一 message item 的新 output_text part
			// （content_index 递增、item id / output_index 沿用首段快照），终态数组为
			// [refusal item, message item(两个 output_text part), function_call]
			So(parseInterleaveEventMeta(events, "response.content_part.added", `"output_text"`), ShouldResemble, [][3]string{
				{"msg_resp_mst_1", "1", "0"},
				{"msg_resp_mst_1", "1", "1"},
			})
			So(parseInterleaveEventMeta(events, "response.output_text.delta", ""), ShouldResemble, [][3]string{
				{"msg_resp_mst_1", "1", "0"},
				{"msg_resp_mst_1", "1", "1"},
			})
			// done 按段而非累积全文，content_index 与 added/delta 对齐
			So(parseOutputTextDoneByPart(events), ShouldResemble, [][2]string{
				{"0", "first seg"},
				{"1", "second seg"},
			})
			// tool 偏移：refusal 0 + text 1 → function_call 2
			So(parseEventMeta(events, "response.output_item.added", `"function_call"`), ShouldResemble,
				[][2]string{{"fc_call_mst", "2"}})

			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 3)
			So(output[0].(map[string]interface{})["id"], ShouldEqual, "msg_resp_mst_0")
			So(output[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["refusal"],
				ShouldEqual, "cannot do")
			msg := output[1].(map[string]interface{})
			So(msg["id"], ShouldEqual, "msg_resp_mst_1")
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "output_text")
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "first seg")
			So(content[1].(map[string]interface{})["type"], ShouldEqual, "output_text")
			So(content[1].(map[string]interface{})["text"], ShouldEqual, "second seg")
			So(output[2].(map[string]interface{})["id"], ShouldEqual, "fc_call_mst")
		})

		Convey("M2: R2 不回归 → refusal→text→refusal→text 紧随重开 refusal 的直接 text 违规丢弃", func() {
			chunks := []string{
				`data: {"id":"resp_mstv","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do A"},"finish_reason":null}]}`,
				`data: {"id":"resp_mstv","choices":[{"index":0,"delta":{"content":"but text B"},"finish_reason":null}]}`,
				`data: {"id":"resp_mstv","choices":[{"index":0,"delta":{"refusal":"then refuse C"},"finish_reason":null}]}`,
				`data: {"id":"resp_mstv","choices":[{"index":0,"delta":{"content":"late text D must drop"},"finish_reason":null}]}`,
				`data: {"id":"resp_mstv","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 「紧随 refusal 关闭/进行中的直接 text」判违规：D 整段丢弃，不污染 item 与终态
			joined := strings.Join(events, "")
			So(strings.Contains(joined, "late text D must drop"), ShouldBeFalse)
			So(len(parseInterleaveEventMeta(events, "response.output_text.delta", "")), ShouldEqual, 1)
			So(parseOutputTextDoneByPart(events), ShouldResemble, [][2]string{{"0", "but text B"}})
			So(parseEventMeta(events, "response.output_item.added", `"message"`), ShouldResemble, [][2]string{
				{"msg_resp_mstv_0", "0"},
				{"msg_resp_mstv_1", "1"},
			})
			output := parseCompletedOutput(events)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})["refusal"],
				ShouldEqual, "cannot do Athen refuse C")
			textMsg := output[1].(map[string]interface{})
			So(textMsg["id"], ShouldEqual, "msg_resp_mstv_1")
			So(textMsg["content"].([]interface{})[0].(map[string]interface{})["text"], ShouldEqual, "but text B")
		})
	})
}

// parseMessageItemLastDoneContents 解析每个 message item 末条 response.output_item.done 的
// content 数组。协议 docs/responses-protocol.md §7 语义为每 item 恰好一条 added/done，
// 正常情况下每个 id 只出现一次；map 取末值形态同时保留对重复 done 的容错解析，
// 重复性由 parseMessageItemDoneIDs 单独断言。
func parseMessageItemLastDoneContents(events []string) map[string][]interface{} {
	out := make(map[string][]interface{})
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
		if parsed.Get("type").String() != "response.output_item.done" ||
			parsed.Get("item.type").String() != "message" {
			continue
		}
		var content []interface{}
		if err := json.Unmarshal([]byte(parsed.Get("item.content").Raw), &content); err != nil {
			continue
		}
		out[parsed.Get("item.id").String()] = content
	}
	return out
}

// parseMessageItemDoneIDs 按事件顺序返回 message item 的 response.output_item.done 的
// item id 列表，用于断言「同 item 恒且仅一条 done」（协议 §7：marked done 一次性）。
func parseMessageItemDoneIDs(events []string) []string {
	var ids []string
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
		if parsed.Get("type").String() != "response.output_item.done" ||
			parsed.Get("item.type").String() != "message" {
			continue
		}
		ids = append(ids, parsed.Get("item.id").String())
	}
	return ids
}

// terminalMessageContents 解析终态 response.output 中各 message item 的 content 数组，
// 用于与流式唯一 output_item.done 逐项比对（协议 docs/responses-protocol.md §7：
// done 携带该 item 的最终完整 content）。
func terminalMessageContents(events []string) map[string][]interface{} {
	out := make(map[string][]interface{})
	for _, o := range parseCompletedOutput(events) {
		m, ok := o.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "message" {
			continue
		}
		id, _ := m["id"].(string)
		if c, ok := m["content"].([]interface{}); ok {
			out[id] = c
		}
	}
	return out
}

// assertDoneTerminalConsistency 统一校验：每个终态 message item 的 output_item.done
// 恒且仅 1 条、位于终态事件（completed/incomplete/failed 由调用侧流保证）之前、
// 且 content 与终态 response.output 逐项一致（协议 §7：done 携带最终完整 content）。
func assertDoneTerminalConsistency(events []string) {
	lastDone := parseMessageItemLastDoneContents(events)
	terminal := terminalMessageContents(events)
	doneIDs := parseMessageItemDoneIDs(events)
	So(len(terminal), ShouldBeGreaterThan, 0)
	// 位置锁定：全部 message done 均紧邻终态事件之前（终态统一补发点策略）
	terminalPos := -1
	donePos := -1
	msgDoneSeen := false
	for i, evt := range events {
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
		if parsed.Get("type").String() == "response.output_item.done" &&
			parsed.Get("item.type").String() == "message" {
			donePos = i
			msgDoneSeen = true
		}
	}
	for i, evt := range events {
		d := dataOfEvent(evt)
		if s := d.Get("type").String(); s == "response.completed" || s == "response.incomplete" || s == "response.failed" {
			terminalPos = i
		}
	}
	So(msgDoneSeen, ShouldBeTrue)
	So(terminalPos, ShouldBeGreaterThan, donePos)
	for id, want := range terminal {
		// 恒且仅一条 done（协议 §7 marked done 一次性状态转换）
		count := 0
		for _, got := range doneIDs {
			if got == id {
				count++
			}
		}
		So(count, ShouldEqual, 1)
		got, ok := lastDone[id]
		So(ok, ShouldBeTrue)
		// 两侧均已过 json.Unmarshal（map 键 marshal 时排序归一），
		// JSON 串相等 ⇔ 逐项内容相等；id 前缀入串，多 item 场景可直接定位
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		// 失败输出中 actual=唯一 done、expected=终态 content，id 前缀便于定位
		So(id+"|"+string(gotJSON), ShouldEqual, id+"|"+string(wantJSON))
	}
}

// dataOfEvent 提取 SSE 事件 data 行的 gjson 解析结果（非法/缺失返回零值）。
func dataOfEvent(evt string) gjson.Result {
	idx := indexOf(evt, "data: ")
	if idx < 0 {
		return gjson.Result{}
	}
	dataStr := trimSpace(evt[idx+len("data: "):])
	if !gjson.Valid(dataStr) {
		return gjson.Result{}
	}
	return gjson.Parse(dataStr)
}

func TestConvertOpenAIChatToResponses_MessageItemDoneMatchesTerminalContent(t *testing.T) {
	Convey("message item 唯一 output_item.done（恒且仅一条）的 content 与终态 response.output 逐项一致", t, func() {

		Convey("D1: refusal 独立分支 → text → finish → 各 message item 末条 done 与终态一致", func() {
			chunks := []string{
				`data: {"id":"resp_dmc1","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do A"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc1","choices":[{"index":0,"delta":{"content":"but text B"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)
			So(len(terminalMessageContents(events)), ShouldEqual, 2)
			assertDoneTerminalConsistency(events)
		})

		Convey("D2: refusal 独立分支 → 文本追加段（tool 分段后第二段）→ finish → 末条 done 携带完整两段 text", func() {
			chunks := []string{
				`data: {"id":"resp_dmc2","choices":[{"index":0,"delta":{"role":"assistant","refusal":"cannot do A"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc2","choices":[{"index":0,"delta":{"content":"first seg"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_dmc2","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc2","choices":[{"index":0,"delta":{"content":"second seg"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)
			assertDoneTerminalConsistency(events)
			// 具体锁定（reviewer 指令）：text item 的 output_item.done 条数恒等于 1，
			// 且该唯一 done 同时含两段 text——段关闭不提前发 item done，唯一 done 在终态
			// 补发点现取完整快照，杜绝「第一段关闭即定稿」的过期快照与重复 done
			var textDoneCount int
			for _, got := range parseMessageItemDoneIDs(events) {
				if got == "msg_resp_dmc2_1" {
					textDoneCount++
				}
			}
			So(textDoneCount, ShouldEqual, 1)
			textDone := parseMessageItemLastDoneContents(events)["msg_resp_dmc2_1"]
			So(len(textDone), ShouldEqual, 2)
			So(textDone[0].(map[string]interface{})["text"], ShouldEqual, "first seg")
			So(textDone[1].(map[string]interface{})["text"], ShouldEqual, "second seg")
		})

		Convey("D3: text → refusal 追加同 item → finish → 末条 done 携带 text+refusal 合并 content（修复回归锁定）", func() {
			chunks := []string{
				`data: {"id":"resp_dmc3","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc3","choices":[{"index":0,"delta":{"refusal":"I cannot comply"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)
			assertDoneTerminalConsistency(events)
			// 修复回归锁定：该流唯一 message item 恰好一条 done（refusal 追加定稿即 item
			// 定稿），done 携带 text+refusal 合并完整 content 且先于 response.completed；
			// 旧缺陷（done 停留在仅 text parts 的过期快照 / done 缺位）均不回归
			var d3DoneCount int
			for _, got := range parseMessageItemDoneIDs(events) {
				if got == "msg_resp_dmc3_0" {
					d3DoneCount++
				}
			}
			So(d3DoneCount, ShouldEqual, 1)
			d3DonePos := indexOfToolEvent(events, "response.output_item.done", "msg_resp_dmc3_0")
			d3CompletedPos := indexOfToolEvent(events, "response.completed", "")
			So(d3DonePos, ShouldBeGreaterThan, -1)
			So(d3CompletedPos, ShouldBeGreaterThan, d3DonePos)
			done := parseMessageItemLastDoneContents(events)["msg_resp_dmc3_0"]
			So(len(done), ShouldEqual, 2)
			So(done[0].(map[string]interface{})["type"], ShouldEqual, "output_text")
			So(done[0].(map[string]interface{})["text"], ShouldEqual, "Hello")
			So(done[1].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(done[1].(map[string]interface{})["refusal"], ShouldEqual, "I cannot comply")
		})

		Convey("D4: text → refusal 追加 → tool → refusal 重开续写 → finish → 唯一 done 含两段 refusal 合并全文（FAIL-2 回归锁定）", func() {
			chunks := []string{
				`data: {"id":"resp_dmc4","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc4","choices":[{"index":0,"delta":{"refusal":"cannot A"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_dmc4","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc4","choices":[{"index":0,"delta":{"refusal":"then refuse B"},"finish_reason":null}]}`,
				`data: {"id":"resp_dmc4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			}
			events := sendChunks(chunks, false)

			// 旧缺陷：追加 refusal 定稿点发 done（置幂等位）后 refusal 重开续写继续增长
			// RefusalBuf → 唯一 done 缺第二段 refusal，与终态不一致。
			// 新策略：done 只在终态补发点从 outputs 数组同源生成，续写不漏。
			assertDoneTerminalConsistency(events)
			So(parseMessageItemDoneIDs(events), ShouldResemble, []string{"msg_resp_dmc4_0"})
			// refusal 续写复用同 item：不重发 output_item.added
			So(len(parseEventMeta(events, "response.output_item.added", `"message"`)), ShouldEqual, 1)
			// 具体锁定 done content：text 段 + 两段 refusal 合并全文（RefusalBuf 合并语义）
			done := parseMessageItemLastDoneContents(events)["msg_resp_dmc4_0"]
			So(len(done), ShouldEqual, 2)
			So(done[0].(map[string]interface{})["text"], ShouldEqual, "partial")
			So(done[1].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(done[1].(map[string]interface{})["refusal"], ShouldEqual, "cannot Athen refuse B")
		})
	})
}

// ============================================================================
// T7 契约测试（报告一 P0-1(stream)/P0-4、P1-8/P1-9/P1-10、P2-2/P2-3）
// 上游样本严格按 chat §7（chat.completion.chunk + [DONE]）构造，产物按协议解码。
// ============================================================================

// t7Payloads 提取指定事件类型所有帧的 data payload（payload type 字段必须与事件名一致）。
func t7Payloads(events []string, eventType string) []gjson.Result {
	var out []gjson.Result
	for _, evt := range events {
		lines := strings.Split(evt, "\n")
		if len(lines) == 0 || lines[0] != "event: "+eventType {
			continue
		}
		for _, line := range lines[1:] {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if !gjson.Valid(data) {
				continue
			}
			parsed := gjson.Parse(data)
			if parsed.Get("type").String() == eventType {
				out = append(out, parsed)
			}
		}
	}
	return out
}

// t7FrameIndex 返回首个包含指定事件帧的下标，找不到返回 -1。
func t7FrameIndex(events []string, eventType string) int {
	for i, evt := range events {
		if strings.HasPrefix(evt, "event: "+eventType+"\n") {
			return i
		}
	}
	return -1
}

// t7RunStream 模拟 forwarder 按序喂入 chunk；出现转换错误后继续喂入，
// 验证终态后不再产出成功事件。返回全部事件与首个转换错误。
func t7RunStream(reqBody []byte, chunks []string, fallback bool) ([]string, error) {
	var param any
	var all []string
	var firstErr error
	for _, chunk := range chunks {
		events, err := ConvertOpenAIChatToResponsesWithContext(reqBody, nil, []byte(chunk), &param, fallback)
		all = append(all, events...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return all, firstErr
}

// t7ConversionErrorCode 断言错误为 *model.ProtocolConversionError 并提取稳定机器码。
func t7ConversionErrorCode(t *testing.T, err error) string {
	t.Helper()
	var pce *model.ProtocolConversionError
	if !errors.As(err, &pce) {
		t.Fatalf("expected *model.ProtocolConversionError, got %#v", err)
	}
	return pce.Code
}

// t7AssertNoOrphanDone 断言每个 output_item.done 都有同 item id 的 output_item.added
// （responses §7 item 生命周期，P1-10）。
func t7AssertNoOrphanDone(t *testing.T, events []string) {
	t.Helper()
	added := map[string]bool{}
	for _, p := range t7Payloads(events, "response.output_item.added") {
		added[p.Get("item.id").String()] = true
	}
	for _, p := range t7Payloads(events, "response.output_item.done") {
		id := p.Get("item.id").String()
		if !added[id] {
			t.Fatalf("orphan output_item.done without matching added: item id %q", id)
		}
	}
}

func TestConvertOpenAIChatToResponsesRejectsMalformedChunkAndToolLifecycle(t *testing.T) {
	// G: 畸形 JSON 或缺 id/name 的 tool delta 后接 [DONE] | W: converter/forwarder | T: error/failed 终态，无 completed，无 orphan output_item.done

	t.Run("M1 畸形 JSON chunk 后接 [DONE] 不得合成 completed", func(t *testing.T) {
		chunks := []string{
			`data: {"id":"resp_mal1","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
			`data: {"id":"resp_mal1","choices":[{"index":0,"delta":{"content":"broken",`,
			`data: [DONE]`,
		}
		events, err := t7RunStream([]byte(`{"model":"gpt-4o"}`), chunks, false)
		if err == nil {
			t.Fatalf("expected invalid_stream_event error for malformed chunk")
		}
		if code := t7ConversionErrorCode(t, err); code != model.CodeInvalidStreamEvent {
			t.Fatalf("expected code invalid_stream_event, got %q", code)
		}
		if n := len(t7Payloads(events, "response.completed")); n != 0 {
			t.Fatalf("expected no completed terminal after malformed chunk, got %d", n)
		}
		if n := len(t7Payloads(events, "response.incomplete")); n != 0 {
			t.Fatalf("expected no incomplete terminal after malformed chunk, got %d", n)
		}
		failed := t7Payloads(events, "response.failed")
		if len(failed) != 1 {
			t.Fatalf("expected exactly one failed terminal, got %d", len(failed))
		}
		if got := failed[0].Get("response.status").String(); got != "failed" {
			t.Fatalf("expected failed terminal status=failed, got %q", got)
		}
		if got := failed[0].Get("response.error.code").String(); got != model.CodeInvalidStreamEvent {
			t.Fatalf("expected failed terminal error.code=invalid_stream_event, got %q", got)
		}
		t7AssertNoOrphanDone(t, events)
	})

	t.Run("M2 合法 JSON 非 object chunk 同为无效流事件", func(t *testing.T) {
		chunks := []string{`data: [1,2,3]`, `data: [DONE]`}
		events, err := t7RunStream([]byte(`{"model":"gpt-4o"}`), chunks, false)
		if err == nil {
			t.Fatalf("expected invalid_stream_event error for non-object chunk")
		}
		if code := t7ConversionErrorCode(t, err); code != model.CodeInvalidStreamEvent {
			t.Fatalf("expected code invalid_stream_event, got %q", code)
		}
		if n := len(t7Payloads(events, "response.completed")); n != 0 {
			t.Fatalf("expected no completed terminal, got %d", n)
		}
		if n := len(t7Payloads(events, "response.failed")); n != 1 {
			t.Fatalf("expected exactly one failed terminal, got %d", n)
		}
		t7AssertNoOrphanDone(t, events)
	})

	t.Run("M3 tool delta 缺 name 未 added，close 转 malformed_tool_call 失败终态", func(t *testing.T) {
		chunks := []string{
			`data: {"id":"resp_mal3","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_m3","type":"function","function":{"arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_mal3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		events, err := t7RunStream(codexRequestWithFunction(), chunks, false)
		if err == nil {
			t.Fatalf("expected malformed_tool_call error for tool delta without name")
		}
		if code := t7ConversionErrorCode(t, err); code != model.CodeMalformedToolCall {
			t.Fatalf("expected code malformed_tool_call, got %q", code)
		}
		if n := len(t7Payloads(events, "response.completed")); n != 0 {
			t.Fatalf("expected no completed terminal, got %d", n)
		}
		if n := len(t7Payloads(events, "response.failed")); n != 1 {
			t.Fatalf("expected exactly one failed terminal, got %d", n)
		}
		if n := len(t7Payloads(events, "response.output_item.added")); n != 0 {
			t.Fatalf("expected no output_item.added for call without name, got %d", n)
		}
		for _, p := range t7Payloads(events, "response.output_item.done") {
			t.Fatalf("expected no output_item.done for never-added call, got %s", p.Raw)
		}
		t7AssertNoOrphanDone(t, events)
	})

	t.Run("M4 tool delta 缺 id 有 name 同样走失败终态", func(t *testing.T) {
		chunks := []string{
			`data: {"id":"resp_mal4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_mal4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		events, err := t7RunStream(codexRequestWithFunction(), chunks, false)
		if err == nil {
			t.Fatalf("expected malformed_tool_call error for tool delta without id")
		}
		if code := t7ConversionErrorCode(t, err); code != model.CodeMalformedToolCall {
			t.Fatalf("expected code malformed_tool_call, got %q", code)
		}
		if n := len(t7Payloads(events, "response.failed")); n != 1 {
			t.Fatalf("expected exactly one failed terminal, got %d", n)
		}
		if n := len(t7Payloads(events, "response.completed")); n != 0 {
			t.Fatalf("expected no completed terminal, got %d", n)
		}
		t7AssertNoOrphanDone(t, events)
	})
}

func TestConvertOpenAIChatToResponsesCustomDoneAndUsageDetails(t *testing.T) {
	// G: 合法 custom call + 完整 Chat usage details | W: 完成流 | T: custom input delta/done 成对，usage details 全保留且无文本估算

	t.Run("D1 custom close 序列含 custom_tool_call_input.done 且 final input 一致", func(t *testing.T) {
		chunks := []string{
			`data: {"id":"resp_ct","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_ct_1","type":"function","function":{"name":"my_grammar","arguments":"{\"input\":\"hello grammar\"}"}}]},"finish_reason":null}]}`,
			`data: {"id":"resp_ct","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		events, err := t7RunStream(codexRequestWithPlainCustom(), chunks, false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		deltas := t7Payloads(events, "response.custom_tool_call_input.delta")
		dones := t7Payloads(events, "response.custom_tool_call_input.done")
		if len(deltas) != 1 || len(dones) != 1 {
			t.Fatalf("expected paired custom input delta/done, got deltas=%d dones=%d", len(deltas), len(dones))
		}
		var itemDone gjson.Result
		found := false
		for _, p := range t7Payloads(events, "response.output_item.done") {
			if p.Get("item.id").String() == "ctc_call_ct_1" {
				itemDone = p
				found = true
			}
		}
		if !found {
			t.Fatalf("expected output_item.done for ctc_call_ct_1")
		}
		if deltas[0].Get("delta").String() != "hello grammar" {
			t.Fatalf("expected delta input 'hello grammar', got %q", deltas[0].Get("delta").String())
		}
		if dones[0].Get("input").String() != "hello grammar" {
			t.Fatalf("expected custom_tool_call_input.done input 'hello grammar', got %q", dones[0].Get("input").String())
		}
		// .done final input 与 output_item.done.item.input 一致（P1-8）
		if dones[0].Get("input").String() != itemDone.Get("item.input").String() {
			t.Fatalf("custom_tool_call_input.done input %q != output_item.done item.input %q",
				dones[0].Get("input").String(), itemDone.Get("item.input").String())
		}
		if dones[0].Get("item_id").String() != "ctc_call_ct_1" {
			t.Fatalf("expected done item_id ctc_call_ct_1, got %q", dones[0].Get("item_id").String())
		}
		// 顺序：delta < done < output_item.done（responses §7 生命周期）
		iDelta, iDone, iItem := t7FrameIndex(events, "response.custom_tool_call_input.delta"), t7FrameIndex(events, "response.custom_tool_call_input.done"), -1
		for i, evt := range events {
			if strings.HasPrefix(evt, "event: response.output_item.done\n") && strings.Contains(evt, "ctc_call_ct_1") {
				iItem = i
			}
		}
		if !(iDelta >= 0 && iDone > iDelta && iItem > iDone) {
			t.Fatalf("expected ordering delta(%d) < done(%d) < item.done(%d)", iDelta, iDone, iItem)
		}
		t7AssertNoOrphanDone(t, events)
	})

	t.Run("D2 Chat usage completion details 全保留且与非流式字段集合一致", func(t *testing.T) {
		usageJSON := `"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":3,"accepted_prediction_tokens":5,"rejected_prediction_tokens":1,"audio_tokens":2,"text_tokens":7}}`
		chunks := []string{
			`data: {"id":"resp_ct2","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`,
			`data: {"id":"resp_ct2","choices":[],` + usageJSON + `}`,
			`data: [DONE]`,
		}
		events, err := t7RunStream([]byte(`{"model":"gpt-4o"}`), chunks, false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		usage := parseCompletedUsage(events)
		if usage == nil {
			t.Fatalf("expected completed usage")
		}
		od, ok := usage["output_tokens_details"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected output_tokens_details object, got %#v", usage["output_tokens_details"])
		}
		for key, want := range map[string]float64{
			"reasoning_tokens": 3, "accepted_prediction_tokens": 5,
			"rejected_prediction_tokens": 1, "audio_tokens": 2, "text_tokens": 7,
		} {
			if got := od[key]; got != want {
				t.Fatalf("expected output_tokens_details.%s=%v, got %#v", key, want, got)
			}
		}
		id, ok := usage["input_tokens_details"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected input_tokens_details object")
		}
		if id["cached_tokens"] != float64(4) || id["cache_write_tokens"] != float64(2) {
			t.Fatalf("expected cached/cache_write 4/2, got %#v", id)
		}

		// 与非流式（T6 同一段 usage）对照：details 字段集合与值完全一致（P1-9 同源映射）
		chatBody := []byte(`{"id":"chatcmpl_ct2","object":"chat.completion","created":1700000042,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` + usageJSON + `}`)
		reqBody := []byte(`{"model":"gpt-4o"}`)
		nonStream, nsErr := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-4o", false, reqBody)
		if nsErr != nil {
			t.Fatalf("non-stream conversion failed: %v", nsErr)
		}
		nsParsed := gjson.GetBytes(nonStream, "usage")
		streamDetails := gjson.Parse(gjson.Parse(trimSpace(extractT7Data(events, "response.completed"))).Get("response.usage").Raw)
		for _, path := range []string{"output_tokens_details", "input_tokens_details"} {
			nsKeys := sortedDetailKeys(nsParsed.Get(path))
			stKeys := sortedDetailKeys(streamDetails.Get(path))
			if !reflect.DeepEqual(nsKeys, stKeys) {
				t.Fatalf("usage %s field set mismatch: non-stream=%v stream=%v", path, nsKeys, stKeys)
			}
		}
	})

	t.Run("D3 reasoning 文本不得估算 reasoning_tokens（P0-4）", func(t *testing.T) {
		longReasoning := strings.Repeat("推", 200) // UTF-8 每字 3 字节，旧字节估算路径会伪造出非 0 token
		chunks := []string{
			`data: {"id":"resp_ct3","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"` + longReasoning + `"},"finish_reason":null}]}`,
			`data: {"id":"resp_ct3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"resp_ct3","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}`,
			`data: [DONE]`,
		}
		events, err := t7RunStream([]byte(`{"model":"gpt-4o"}`), chunks, false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		usage := parseCompletedUsage(events)
		od, ok := usage["output_tokens_details"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected output_tokens_details object")
		}
		if od["reasoning_tokens"] != float64(0) {
			t.Fatalf("expected reasoning_tokens=0 without upstream usage detail, got %#v (estimation path must be removed)", od["reasoning_tokens"])
		}
	})
}

// extractT7Data 返回指定事件帧的 data payload 字符串。
func extractT7Data(events []string, eventType string) string {
	for _, evt := range events {
		if !strings.HasPrefix(evt, "event: "+eventType+"\n") {
			continue
		}
		for _, line := range strings.Split(evt, "\n") {
			if strings.HasPrefix(line, "data: ") {
				return strings.TrimPrefix(line, "data: ")
			}
		}
	}
	return ""
}

// sortedDetailKeys 提取 usage details 对象的键名并排序，供流式/非流式对照。
func sortedDetailKeys(obj gjson.Result) []string {
	if !obj.IsObject() {
		return nil
	}
	var keys []string
	obj.ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return true
	})
	sort.Strings(keys)
	return keys
}

func TestConvertOpenAIChatToResponsesCreatedAndRequestSnapshot(t *testing.T) {
	// G: chunk.created 与原请求 text/modalities/store/service_tier/user | W: 完成流 | T: 全事件/终态 created_at 使用上游值，终态字段与请求一致

	reqBody := []byte(`{"model":"codex-snap","instructions":"sys","text":{"format":{"type":"text","verbosity":"low"}},"modalities":["text"],"store":false,"service_tier":"default","user":"user-42","temperature":0.3,"reasoning":{"effort":"low"},"parallel_tool_calls":true,"max_output_tokens":128,"metadata":{"k":"v"},"tool_choice":"auto","previous_response_id":"resp_prev_9"}`)

	validChunks := func(withCreated bool) []string {
		created := ""
		if withCreated {
			created = `"created":1700000042,`
		}
		return []string{
			`data: {` + created + `"id":"resp_cr","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
			`data: {` + created + `"id":"resp_cr","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
	}

	t.Run("C1 首个合法 chunk 的 created 贯穿 created/in_progress/终态（P2-2）", func(t *testing.T) {
		events, err := t7RunStream(reqBody, validChunks(true), false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		for _, evtType := range []string{"response.created", "response.in_progress", "response.completed"} {
			payloads := t7Payloads(events, evtType)
			if len(payloads) == 0 {
				t.Fatalf("expected %s event", evtType)
			}
			if got := payloads[0].Get("response.created_at").Int(); got != 1700000042 {
				t.Fatalf("expected %s response.created_at=1700000042, got %d", evtType, got)
			}
		}
	})

	t.Run("C2 chunk 缺失 created 时回落网关时间", func(t *testing.T) {
		base := time.Now().Unix() - 1
		events, err := t7RunStream(reqBody, validChunks(false), false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		payloads := t7Payloads(events, "response.completed")
		if len(payloads) == 0 {
			t.Fatalf("expected completed event")
		}
		got := payloads[0].Get("response.created_at").Int()
		if got < base || got > time.Now().Unix()+1 {
			t.Fatalf("expected gateway-time fallback created_at in [%d, %d], got %d", base, time.Now().Unix()+1, got)
		}
	})

	t.Run("C3 终态快照回显 text/modalities/store/service_tier/user 及现有字段（P2-3）", func(t *testing.T) {
		events, err := t7RunStream(reqBody, validChunks(true), false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		resp := gjson.Parse(extractT7Data(events, "response.completed")).Get("response")
		if !resp.IsObject() {
			t.Fatalf("expected completed response object")
		}
		checks := map[string]interface{}{
			"status":                "completed",
			"model":                 "codex-snap",
			"instructions":          "sys",
			"service_tier":          "default",
			"user":                  "user-42",
			"previous_response_id":  "resp_prev_9",
			"tool_choice":           "auto",
			"text.format.type":      "text",
			"text.format.verbosity": "low",
			"modalities.0":          "text",
			"reasoning.effort":      "low",
			"metadata.k":            "v",
		}
		for path, want := range checks {
			if got := resp.Get(path).Value(); !reflect.DeepEqual(got, want) {
				t.Fatalf("expected response.%s=%v, got %#v", path, want, got)
			}
		}
		if v := resp.Get("store"); !v.Exists() || v.Type != gjson.False {
			t.Fatalf("expected response.store=false echoed, got %#v", v.Raw)
		}
		if v := resp.Get("temperature"); v.Float() != 0.3 {
			t.Fatalf("expected response.temperature=0.3, got %s", v.Raw)
		}
		if v := resp.Get("max_output_tokens"); v.Int() != 128 {
			t.Fatalf("expected response.max_output_tokens=128, got %s", v.Raw)
		}
		if v := resp.Get("parallel_tool_calls"); !v.Bool() {
			t.Fatalf("expected response.parallel_tool_calls=true, got %s", v.Raw)
		}
	})

	t.Run("C4 流式终态与非流式回显字段集合一致", func(t *testing.T) {
		events, err := t7RunStream(reqBody, validChunks(true), false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		var streamResp map[string]interface{}
		if e := json.Unmarshal([]byte(gjson.Parse(extractT7Data(events, "response.completed")).Get("response").Raw), &streamResp); e != nil {
			t.Fatalf("parse stream terminal response: %v", e)
		}
		chatBody := []byte(`{"id":"chatcmpl_snap","object":"chat.completion","created":1700000042,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
		nonStream, nsErr := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-4o", false, reqBody)
		if nsErr != nil {
			t.Fatalf("non-stream conversion failed: %v", nsErr)
		}
		var nsResp map[string]interface{}
		if e := json.Unmarshal(nonStream, &nsResp); e != nil {
			t.Fatalf("parse non-stream response: %v", e)
		}
		// 契约 T7 State/Data：至少 text/modalities/store/service_tier/user + T6 现有回显清单
		echoKeys := []string{
			"instructions", "max_output_tokens", "parallel_tool_calls", "reasoning",
			"temperature", "tool_choice", "tools", "top_p", "metadata", "text",
			"modalities", "store", "service_tier", "previous_response_id", "user",
		}
		for _, key := range echoKeys {
			_, inStream := streamResp[key]
			_, inNS := nsResp[key]
			if inStream != inNS {
				t.Fatalf("echo field %q presence mismatch: stream=%v non-stream=%v", key, inStream, inNS)
			}
			if inStream && !reflect.DeepEqual(streamResp[key], nsResp[key]) {
				t.Fatalf("echo field %q value mismatch: stream=%#v non-stream=%#v", key, streamResp[key], nsResp[key])
			}
		}
	})

	t.Run("C5 failed 终态同样使用统一快照回显", func(t *testing.T) {
		chunks := []string{
			`data: {"created":1700000042,"id":"resp_cr5","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`,
			`data: {"error":{"message":"boom","type":"server_error","code":"server_error"}}`,
			`data: [DONE]`,
		}
		events, err := t7RunStream(reqBody, chunks, false)
		if err != nil {
			t.Fatalf("unexpected conversion error: %v", err)
		}
		failed := t7Payloads(events, "response.failed")
		if len(failed) != 1 {
			t.Fatalf("expected exactly one failed terminal, got %d", len(failed))
		}
		resp := failed[0].Get("response")
		if resp.Get("status").String() != "failed" {
			t.Fatalf("expected status failed, got %s", resp.Get("status").Raw)
		}
		if resp.Get("error.code").String() != "server_error" {
			t.Fatalf("expected upstream error code preserved, got %s", resp.Get("error.code").Raw)
		}
		for _, path := range []string{"service_tier", "user", "text.format.type", "modalities.0", "instructions"} {
			if !resp.Get(path).Exists() {
				t.Fatalf("expected failed terminal to echo response.%s (unified snapshot)", path)
			}
		}
		if got := resp.Get("created_at").Int(); got != 1700000042 {
			t.Fatalf("expected failed terminal created_at=1700000042, got %d", got)
		}
		if len(t7Payloads(events, "response.completed")) != 0 {
			t.Fatalf("expected no completed after failed terminal")
		}
		t7AssertNoOrphanDone(t, events)
	})
}

// P2-1：FirstChunk 重置块必须与 TerminalEvent 对称重置 ConversionError。
// 生产路径每请求新建 state（残留错误场景不可达），此处直接构造「state 内
// ConversionError 已置位 + FirstChunk=true」的第二段流起点，锁定状态不变量：
// 残留错误不得经重置块存活并强制新流 [DONE] 走 failed。
func TestChatToResponsesFirstChunkResetClearsResidualConversionError(t *testing.T) {
	req := []byte(`{"model":"gpt-4o"}`)
	st := &chatToResponsesState{
		FuncArgsBuf:   make(map[int]*strings.Builder),
		FuncNames:     make(map[int]string),
		FuncCallIDs:   make(map[int]string),
		FuncItemAdded: make(map[int]bool),
		FirstChunk:    true,
		// 上一段流 failConversion 残留的转换错误（TerminalEvent 已由重置块语义清零）
		ConversionError: errResponse(model.CodeInvalidStreamEvent, "data", errors.New("previous stream malformed chunk")),
	}
	var param any = st

	// 新流首 chunk：重置块执行后不得因残留错误报错，且字段被清零
	if _, err := ConvertOpenAIChatToResponsesWithContext(req, nil,
		[]byte(`data: {"id":"resp_reuse1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
		&param, false); err != nil {
		t.Fatalf("first chunk of reused stream must not inherit residual error, got %v", err)
	}
	if st.ConversionError != nil {
		t.Fatalf("FirstChunk reset block must clear ConversionError (symmetric with TerminalEvent), got %v", st.ConversionError)
	}

	// [DONE]：无残留错误则正常合成 completed，禁止 failed
	events, err := ConvertOpenAIChatToResponsesWithContext(req, nil, []byte(`data: [DONE]`), &param, false)
	if err != nil {
		t.Fatalf("[DONE] after reset must not be forced failed by residual error, got %v", err)
	}
	if n := len(t7Payloads(events, "response.failed")); n != 0 {
		t.Fatalf("expected no failed terminal for reused clean stream, got %d", n)
	}
	if n := len(t7Payloads(events, "response.completed")); n != 1 {
		t.Fatalf("expected exactly one completed terminal, got %d", n)
	}
}

// P2-2：generateCompletedEvents 的 ConversionError 分支在终态快照构建失败
// （genErr 非 nil，经原请求非法 JSON 触发）时，返回错误必须是合并错误：
// 主转换错误仍可经 errors.As 提取稳定机器码，genErr 诊断信息不得丢弃。
func TestGenerateCompletedEventsMergesConversionErrorWithGenErr(t *testing.T) {
	st := &chatToResponsesState{
		FuncArgsBuf:   make(map[int]*strings.Builder),
		FuncNames:     make(map[int]string),
		FuncCallIDs:   make(map[int]string),
		FuncItemAdded: make(map[int]bool),
		ResponseID:    "resp_merge1",
		CreatedAt:     1700000000,
	}
	mainErr := errResponse(model.CodeInvalidStreamEvent, "data", errors.New("malformed upstream chunk"))
	st.ConversionError = mainErr

	// 原请求非法 JSON → buildTerminalResponseSnapshot 失败 → generateFailedEvents 返回 genErr
	events, err := st.generateCompletedEvents([]byte(`{"instructions":`))
	if err == nil {
		t.Fatalf("expected non-nil merged error when genErr != nil")
	}
	if !errors.Is(err, mainErr) {
		t.Fatalf("merged error must keep main conversion error (errors.Is), got %v", err)
	}
	var pce *model.ProtocolConversionError
	if !errors.As(err, &pce) {
		t.Fatalf("expected *model.ProtocolConversionError extractable from merged error, got %#v", err)
	}
	if pce.Code != model.CodeInvalidStreamEvent {
		t.Fatalf("errors.As must extract main machine code %q, got %q", model.CodeInvalidStreamEvent, pce.Code)
	}
	if !strings.Contains(err.Error(), model.CodeInvalidSourceJSON) {
		t.Fatalf("genErr diagnostics (%s) must be preserved in merged error, got %v", model.CodeInvalidSourceJSON, err)
	}

	failed := t7Payloads(events, "response.failed")
	if len(failed) != 1 {
		t.Fatalf("expected exactly one failed terminal, got %d", len(failed))
	}
	if got := failed[0].Get("response.error.code").String(); got != model.CodeInvalidStreamEvent {
		t.Fatalf("failed terminal error.code must be main machine code, got %q", got)
	}
	if st.TerminalEvent != responsesTerminalFailed {
		t.Fatalf("expected TerminalEvent=%s, got %q", responsesTerminalFailed, st.TerminalEvent)
	}
}
