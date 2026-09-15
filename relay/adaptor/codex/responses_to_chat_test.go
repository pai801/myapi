package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pai801/myapi/common/logger"
	relaymodel "github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/tidwall/gjson"
)

// ===== T6 契约适配 wrapper：按最终签名调用真实函数并断言成功路径无错误 =====

func convReqRaw(modelName string, raw []byte, stream bool) []byte {
	out, err := ConvertResponsesToChatRequest(modelName, raw, stream)
	So(err, ShouldBeNil)
	return out
}

func convRespRaw(body []byte, model string, fb bool) []byte {
	out, err := ConvertChatResponseToResponses(body, model, fb)
	So(err, ShouldBeNil)
	return out
}

func convRespCtxRaw(body []byte, model string, fb bool, req []byte) []byte {
	out, err := ConvertChatResponseToResponsesWithContext(body, model, fb, req)
	So(err, ShouldBeNil)
	return out
}

func convItem(item map[string]interface{}, state *inputConversionState) map[string]interface{} {
	out, err := convertInputItem(item, state)
	So(err, ShouldBeNil)
	return out
}

func convMsgs(input interface{}) []interface{} {
	out, err := convertInputToMessages(input)
	So(err, ShouldBeNil)
	return out
}

func convContent(content []interface{}, role string) interface{} {
	out, err := convertContentArray(content, role)
	So(err, ShouldBeNil)
	return out
}

func convTools(tools []interface{}) []interface{} {
	out, err := convertToolsToOpenAI(tools)
	So(err, ShouldBeNil)
	return out
}

func convFnCallItem(item map[string]interface{}) map[string]interface{} {
	out, err := convertFunctionCallItem(item)
	So(err, ShouldBeNil)
	return out
}

func convMsgToOutput(message map[string]interface{}, ctx *CodexToolContext) []interface{} {
	out, err := convertChatMessageToOutput(message, ctx, "resp_test")
	So(err, ShouldBeNil)
	return out
}

// soPCE 断言 err 为共享协议转换错误（契约 §1.3）且 Code 为期望的稳定机器码。
func soPCE(err error, code string) {
	pce, ok := err.(*relaymodel.ProtocolConversionError)
	So(ok, ShouldBeTrue)
	So(pce.Code, ShouldEqual, code)
}

// strictDecode 以 DisallowUnknownFields 级别解码（契约 §1.1：样本与产物必须能按协议严格 schema 解码）。
func strictDecode(src string, target interface{}) error {
	dec := json.NewDecoder(strings.NewReader(src))
	dec.DisallowUnknownFields()
	return dec.Decode(target)
}

// parseOutputArray 从 ConvertChatResponseToResponses 的结果中提取 output 数组
func parseOutputArray(respBytes []byte) []interface{} {
	var resp map[string]interface{}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil
	}
	if output, ok := resp["output"].([]interface{}); ok {
		return output
	}
	return nil
}

func parseChatRequest(reqBytes []byte) map[string]interface{} {
	var req map[string]interface{}
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		return nil
	}
	return req
}

func TestConvertChatResponseToResponses_FallbackReasoning(t *testing.T) {
	Convey("ConvertChatResponseToResponses 的 reasoning 兜底逻辑（非流式）", t, func() {

		Convey("T1(非流式): 仅 reasoning_content 无 content，开关=true → output 含 reasoning + message", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_test",
				"created": 1700000000,
				"model":   "deepseek-flash",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":              "assistant",
							"reasoning_content": "深度思考过程",
						},
						"finish_reason": "stop",
					},
				},
			}
			chatBody, _ := json.Marshal(chatResp)

			result := convRespRaw(chatBody, "deepseek-flash", true)
			output := parseOutputArray(result)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "reasoning")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "message")
			So(output[1].(map[string]interface{})["role"], ShouldEqual, "assistant")
			content := output[1].(map[string]interface{})["content"].([]interface{})
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "深度思考过程")
		})

		Convey("T2(非流式): 仅 reasoning_content 无 content，开关=false → output 只含 reasoning", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_test",
				"created": 1700000000,
				"model":   "deepseek-flash",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":              "assistant",
							"reasoning_content": "深度思考过程",
						},
						"finish_reason": "stop",
					},
				},
			}
			chatBody, _ := json.Marshal(chatResp)

			result := convRespRaw(chatBody, "deepseek-flash", false)
			output := parseOutputArray(result)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 1)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "reasoning")
		})

		Convey("T3(非流式): 正常 reasoning + content，开关=true → output 含 reasoning + message（原行为）", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_test",
				"created": 1700000000,
				"model":   "deepseek-flash",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":              "assistant",
							"content":           "Hello World",
							"reasoning_content": "思考中",
						},
						"finish_reason": "stop",
					},
				},
			}
			chatBody, _ := json.Marshal(chatResp)

			result := convRespRaw(chatBody, "deepseek-flash", true)
			output := parseOutputArray(result)

			So(output, ShouldNotBeNil)
			So(len(output), ShouldEqual, 2)
			So(output[0].(map[string]interface{})["type"], ShouldEqual, "reasoning")
			So(output[1].(map[string]interface{})["type"], ShouldEqual, "message")
			// content 应来自正常 content 字段，不是 reasoning 兜底
			content := output[1].(map[string]interface{})["content"].([]interface{})
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "Hello World")
		})
	})
}

func TestConvertToolsToOpenAI_FunctionTool(t *testing.T) {
	Convey("convertToolsToOpenAI: type:function 工具保持现有行为（回归保护）", t, func() {

		Convey("单个 function 工具 → 输出 1 个标准 function tool，name/description/parameters 不变", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type":        "function",
					"name":        "get_weather",
					"description": "Get current weather",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{"type": "string"},
						},
						"required": []interface{}{"location"},
					},
				},
			}

			result := convTools(tools)

			So(len(result), ShouldEqual, 1)
			out := result[0].(map[string]interface{})
			So(out["type"], ShouldEqual, "function")
			fn := out["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "get_weather")
			So(fn["description"], ShouldEqual, "Get current weather")
			params := fn["parameters"].(map[string]interface{})
			So(params["type"], ShouldEqual, "object")
		})

		Convey("支持嵌套 function 结构（type:function + function:{...}）", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name":        "sum_two",
						"description": "Add two numbers",
						"parameters": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{},
						},
					},
				},
			}

			result := convTools(tools)

			So(len(result), ShouldEqual, 1)
			fn := result[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "sum_two")
			So(fn["description"], ShouldEqual, "Add two numbers")
		})
	})
}

func TestConvertToolsToOpenAI_CustomTool(t *testing.T) {
	Convey("convertToolsToOpenAI: type:custom 工具扁平化为 input:string 的 function", t, func() {

		Convey("单个 custom 工具 → 1 个 function tool，parameters 包含 input 字符串", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type":        "custom",
					"name":        "user_grammar",
					"description": "user-defined grammar",
				},
			}

			result := convTools(tools)

			So(len(result), ShouldEqual, 1)
			out := result[0].(map[string]interface{})
			So(out["type"], ShouldEqual, "function")
			fn := out["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "user_grammar")
			So(fn["description"], ShouldEqual, "user-defined grammar")
			params := fn["parameters"].(map[string]interface{})
			So(params["type"], ShouldEqual, "object")
			props := params["properties"].(map[string]interface{})
			inputProp := props["input"].(map[string]interface{})
			So(inputProp["type"], ShouldEqual, "string")
			So(inputProp["description"], ShouldEqual, "raw tool input")
			required := params["required"].([]interface{})
			So(len(required), ShouldEqual, 1)
			So(required[0], ShouldEqual, "input")
		})

		Convey("custom 工具缺 description 时回退到默认值", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type": "custom",
					"name": "no_desc",
				},
			}

			result := convTools(tools)

			So(len(result), ShouldEqual, 1)
			fn := result[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "no_desc")
			So(fn["description"], ShouldNotEqual, "")
		})
	})
}

func TestConvertToolsToOpenAI_ApplyPatchTool(t *testing.T) {
	Convey("convertToolsToOpenAI: type:custom 且 name=apply_patch 注册主工具 + 5 个代理子工具", t, func() {

		tools := []interface{}{
			map[string]interface{}{
				"type":        "custom",
				"name":        "apply_patch",
				"description": "patch files",
			},
		}

		result := convTools(tools)

		So(len(result), ShouldEqual, 6)

		names := make([]string, 0, 6)
		for _, r := range result {
			fn := r.(map[string]interface{})["function"].(map[string]interface{})
			names = append(names, fn["name"].(string))
		}
		So(names, ShouldResemble, []string{
			"apply_patch",
			"apply_patch_add_file",
			"apply_patch_delete_file",
			"apply_patch_update_file",
			"apply_patch_replace_file",
			"apply_patch_batch",
		})

		byName := map[string]map[string]interface{}{}
		for _, r := range result {
			fn := r.(map[string]interface{})["function"].(map[string]interface{})
			byName[fn["name"].(string)] = fn
		}

		mainParams := byName["apply_patch"]["parameters"].(map[string]interface{})
		So(mainParams["type"], ShouldEqual, "object")
		mainInput := mainParams["properties"].(map[string]interface{})["input"].(map[string]interface{})
		So(mainInput["type"], ShouldEqual, "string")

		addParams := byName["apply_patch_add_file"]["parameters"].(map[string]interface{})
		addProps := addParams["properties"].(map[string]interface{})
		So(addProps["path"].(map[string]interface{})["type"], ShouldEqual, "string")
		So(addProps["content"].(map[string]interface{})["type"], ShouldEqual, "string")

		delParams := byName["apply_patch_delete_file"]["parameters"].(map[string]interface{})
		So(delParams["properties"].(map[string]interface{})["path"].(map[string]interface{})["type"], ShouldEqual, "string")

		updParams := byName["apply_patch_update_file"]["parameters"].(map[string]interface{})
		updProps := updParams["properties"].(map[string]interface{})
		So(updProps["path"].(map[string]interface{})["type"], ShouldEqual, "string")
		So(updProps["move_to"].(map[string]interface{})["type"], ShouldEqual, "string")
		So(updProps["hunks"].(map[string]interface{})["type"], ShouldEqual, "array")

		rplParams := byName["apply_patch_replace_file"]["parameters"].(map[string]interface{})
		rplProps := rplParams["properties"].(map[string]interface{})
		So(rplProps["path"].(map[string]interface{})["type"], ShouldEqual, "string")
		So(rplProps["content"].(map[string]interface{})["type"], ShouldEqual, "string")

		batchParams := byName["apply_patch_batch"]["parameters"].(map[string]interface{})
		So(batchParams["properties"].(map[string]interface{})["operations"].(map[string]interface{})["type"], ShouldEqual, "array")
	})
}

func TestConvertToolsToOpenAI_NamespaceTool(t *testing.T) {
	Convey("convertToolsToOpenAI: type:namespace 工具把 child function 扁平化", t, func() {

		tools := []interface{}{
			map[string]interface{}{
				"type": "namespace",
				"name": "myapp__",
				"tools": []interface{}{
					map[string]interface{}{
						"type":        "function",
						"name":        "create",
						"description": "create resource",
						"parameters": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{},
						},
					},
					map[string]interface{}{
						"type":        "function",
						"name":        "delete",
						"description": "delete resource",
						"parameters": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{},
						},
					},
				},
			},
		}

		result := convTools(tools)

		So(len(result), ShouldEqual, 2)
		names := make(map[string]map[string]interface{}, 2)
		for _, r := range result {
			fn := r.(map[string]interface{})["function"].(map[string]interface{})
			names[fn["name"].(string)] = fn
		}
		So(names["myapp__create"], ShouldNotBeNil)
		So(names["myapp__delete"], ShouldNotBeNil)
		So(names["myapp__create"]["description"], ShouldEqual, "create resource")
		So(names["myapp__delete"]["description"], ShouldEqual, "delete resource")
	})
}

func TestConvertToolsToOpenAI_BuiltinTool(t *testing.T) {
	Convey("convertToolsToOpenAI: DN-6 白名单外内建工具（web_search/local_shell/computer_use）丢弃且不报错，不再 400", t, func() {
		for _, typ := range []string{"web_search", "local_shell", "computer_use"} {
			out, err := convertToolsToOpenAI([]interface{}{
				map[string]interface{}{"type": typ, "name": "custom_named"},
			})
			So(err, ShouldBeNil)
			So(len(out), ShouldEqual, 0)
		}
	})

	Convey("DN-6 白名单外工具与合法 function 混排 → 仅丢弃前者，合法工具保留", t, func() {
		out, err := convertToolsToOpenAI([]interface{}{
			map[string]interface{}{"type": "web_search"},
			map[string]interface{}{"type": "function", "name": "get_weather"},
		})
		So(err, ShouldBeNil)
		So(len(out), ShouldEqual, 1)
		fn := out[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "get_weather")
	})

	Convey("DN-6 白名单边界：tool_search（execution=client 客户端代理执行闭环）继续按现有回归转换", t, func() {
		out, err := convertToolsToOpenAI([]interface{}{
			map[string]interface{}{"type": "tool_search"},
		})
		So(err, ShouldBeNil)
		So(len(out), ShouldEqual, 1)
		fn := out[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "tool_search")
	})
}

func TestConvertResponsesToChatRequest_ToolSearchPreservesMetadata(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: tool_search 保留原始 description 与 parameters", t, func() {
		rawDescription := "Deferred tool discovery. Multi-agent tools: Spawn and manage sub-agents"
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": "find tools",
			"tools": [{
				"type": "tool_search",
				"description": "` + rawDescription + `",
				"parameters": {
					"type": "object",
					"properties": {
						"query": {"type": "string", "description": "tool metadata query"},
						"limit": {"type": "integer", "description": "maximum result count"}
					},
					"required": ["query"]
				}
			}]
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		tools := chatReq["tools"].([]interface{})
		So(len(tools), ShouldEqual, 1)
		fn := tools[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "tool_search")
		So(fn["description"], ShouldEqual, rawDescription)
		So(fn["description"], ShouldNotEqual, "built-in tool")
		params := fn["parameters"].(map[string]interface{})
		props := params["properties"].(map[string]interface{})
		So(props["query"], ShouldNotBeNil)
		So(props["limit"], ShouldNotBeNil)
		_, hasInput := props["input"]
		So(hasInput, ShouldBeFalse)
		required := params["required"].([]interface{})
		So(required, ShouldResemble, []interface{}{"query"})
	})
}

func TestConvertResponsesToChatRequest_ToolSearchNestedFunctionPreservesMetadata(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: 嵌套 function 形态 tool_search 保留原始 metadata", t, func() {
		rawDescription := "Nested deferred discovery. Multi-agent tools: Spawn and manage sub-agents"
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": "find tools",
			"tools": [{
				"type": "tool_search",
				"function": {
					"name": "codex_tool_search",
					"description": "` + rawDescription + `",
					"parameters": {
						"type": "object",
						"properties": {
							"query": {"type": "string"},
							"limit": {"type": "integer"}
						},
						"required": ["query"]
					}
				}
			}]
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		tools := chatReq["tools"].([]interface{})
		So(len(tools), ShouldEqual, 1)
		fn := tools[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "codex_tool_search")
		So(fn["description"], ShouldEqual, rawDescription)
		params := fn["parameters"].(map[string]interface{})
		props := params["properties"].(map[string]interface{})
		So(props["query"], ShouldNotBeNil)
		So(props["limit"], ShouldNotBeNil)
		_, hasInput := props["input"]
		So(hasInput, ShouldBeFalse)
	})
}

func TestConvertResponsesToChatRequest_BuiltinToolOutputItems(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: 兼容真实 builtin tool output item 类型", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": [
				{"type": "tool_search_call", "call_id": "ts_call_1", "name": "tool_search", "arguments": {"query": "agent tool"}},
				{"type": "web_search_call", "call_id": "ws_call_1", "name": "web_search", "arguments": {"query": "One API"}},
				{
					"type": "tool_search_output",
					"call_id": "ts_call_1",
					"output": {
						"query": "agent tool",
						"tools": [
							{"name": "utility", "description": "utility tool"}
						]
					}
				},
				{
					"type": "web_search_output",
					"call_id": "ws_call_1",
					"output": {
						"results": [
							{"title": "One API", "url": "https://example.com"}
						]
					}
				}
			]
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		messages, ok := chatReq["messages"].([]interface{})
		So(ok, ShouldBeTrue)
		// 相邻两个 call 合并为一条 assistant.tool_calls[]，随后两条配对 output
		So(len(messages), ShouldEqual, 3)

		toolSearchOutput := messages[1].(map[string]interface{})
		So(toolSearchOutput["role"], ShouldEqual, "tool")
		So(toolSearchOutput["tool_call_id"], ShouldEqual, "ts_call_1")
		toolSearchContent, ok := toolSearchOutput["content"].(string)
		So(ok, ShouldBeTrue)
		So(toolSearchContent, ShouldContainSubstring, `"query":"agent tool"`)
		So(toolSearchContent, ShouldContainSubstring, `"tools":[{"description":"utility tool","name":"utility"}]`)

		webSearchOutput := messages[2].(map[string]interface{})
		So(webSearchOutput["role"], ShouldEqual, "tool")
		So(webSearchOutput["tool_call_id"], ShouldEqual, "ws_call_1")
		webSearchContent, ok := webSearchOutput["content"].(string)
		So(ok, ShouldBeTrue)
		So(webSearchContent, ShouldContainSubstring, `"results":[{"title":"One API","url":"https://example.com"}]`)
	})
}

func TestConvertResponsesToChatRequest_BuiltinToolOutputPayloadFallback(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: builtin tool output 缺少 output 时回退序列化协议 payload", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": [
				{"type": "tool_search_call", "call_id": "ts_call_2", "name": "tool_search", "arguments": {"query": "agents"}},
				{"type": "web_search_call", "call_id": "ws_call_2", "name": "web_search", "arguments": {"query": "One API"}},
				{
					"type": "tool_search_output",
					"call_id": "ts_call_2",
					"status": "completed",
					"tools": [
						{
							"type": "namespace",
							"name": "multi_agent_v1",
							"tools": [
								{"type": "function", "name": "spawn_agent", "description": "spawn an agent", "parameters": {"type": "object", "properties": {}}}
							]
						}
					]
				},
				{
					"type": "web_search_output",
					"call_id": "ws_call_2",
					"status": "completed",
					"results": [
						{"title": "One API", "url": "https://example.com"}
					]
				}
			]
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 3)

		toolSearchOutput := messages[1].(map[string]interface{})
		So(toolSearchOutput["role"], ShouldEqual, "tool")
		toolSearchContent, ok := toolSearchOutput["content"].(string)
		So(ok, ShouldBeTrue)
		So(toolSearchContent, ShouldNotEqual, "")
		So(toolSearchContent, ShouldContainSubstring, `"multi_agent_v1"`)
		So(toolSearchContent, ShouldContainSubstring, `"spawn_agent"`)

		webSearchOutput := messages[2].(map[string]interface{})
		So(webSearchOutput["role"], ShouldEqual, "tool")
		webSearchContent, ok := webSearchOutput["content"].(string)
		So(ok, ShouldBeTrue)
		So(webSearchContent, ShouldNotEqual, "")
		So(webSearchContent, ShouldContainSubstring, `"results":[{"title":"One API","url":"https://example.com"}]`)
	})
}

func TestConvertResponsesToChatRequest_MergesDiscoveredNamespaceTools(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: 合并 input 中 tool_search_output 发现的 namespace tools 到 Chat 顶层 tools", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "function", "name": "existing_tool", "description": "existing", "parameters": {"type": "object", "properties": {}}}
			],
			"input": [
				{"type": "tool_search_call", "call_id": "ts_call_3", "name": "tool_search", "arguments": {"query": "agents"}},
				{
					"type": "tool_search_output",
					"call_id": "ts_call_3",
					"tools": [
						{
							"type": "namespace",
							"name": "multi_agent_v1",
							"tools": [
								{"type": "function", "name": "spawn_agent", "description": "spawn an agent", "parameters": {"type": "object", "properties": {"task": {"type": "string"}}}},
								{"type": "function", "name": "wait_agent", "description": "wait an agent", "parameters": {"type": "object", "properties": {"id": {"type": "string"}}}}
							]
						}
					]
				}
			]
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		tools := chatReq["tools"].([]interface{})
		So(len(tools), ShouldEqual, 3)

		byName := map[string]map[string]interface{}{}
		for _, raw := range tools {
			fn := raw.(map[string]interface{})["function"].(map[string]interface{})
			byName[fn["name"].(string)] = fn
		}

		So(byName["existing_tool"], ShouldNotBeNil)
		So(byName["multi_agent_v1__spawn_agent"], ShouldNotBeNil)
		So(byName["multi_agent_v1__wait_agent"], ShouldNotBeNil)
		So(byName["multi_agent_v1__spawn_agent"]["description"], ShouldEqual, "spawn an agent")
	})
}

func TestConvertToolsToOpenAI_MixedTools(t *testing.T) {
	Convey("convertToolsToOpenAI: function + custom + namespace 混合，顺序与数量正确", t, func() {

		tools := []interface{}{
			map[string]interface{}{
				"type":        "function",
				"name":        "fn_a",
				"description": "a",
			},
			map[string]interface{}{
				"type":        "custom",
				"name":        "grammar_b",
				"description": "b",
			},
			map[string]interface{}{
				"type": "namespace",
				"name": "ns__",
				"tools": []interface{}{
					map[string]interface{}{
						"type":        "function",
						"name":        "c1",
						"description": "c1",
					},
					map[string]interface{}{
						"type":        "function",
						"name":        "c2",
						"description": "c2",
					},
				},
			},
		}

		result := convTools(tools)

		So(len(result), ShouldEqual, 4)
		names := make([]string, 0, 4)
		for _, r := range result {
			fn := r.(map[string]interface{})["function"].(map[string]interface{})
			names = append(names, fn["name"].(string))
		}
		So(names, ShouldResemble, []string{"fn_a", "grammar_b", "ns__c1", "ns__c2"})
	})
}

func TestConvertToolsToOpenAI_EmptyTools(t *testing.T) {
	Convey("convertToolsToOpenAI: 空输入不 panic，返回空结果", t, func() {

		Convey("nil 切片", func() {
			result := convTools(nil)
			So(len(result), ShouldEqual, 0)
		})

		Convey("空切片", func() {
			result := convTools([]interface{}{})
			So(len(result), ShouldEqual, 0)
		})
	})
}

func TestConvertFunctionCallItem_NoNamespace(t *testing.T) {
	Convey("convertFunctionCallItem: 无 namespace 字段时保持现有行为（回归保护）", t, func() {
		item := map[string]interface{}{
			"type":      "function_call",
			"call_id":   "c1",
			"name":      "read_file",
			"arguments": "{\"path\":\"a.txt\"}",
		}

		result := convFnCallItem(item)

		So(result["role"], ShouldEqual, "assistant")
		tcs, ok := result["tool_calls"].([]interface{})
		So(ok, ShouldBeTrue)
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c1")
		So(tc["type"], ShouldEqual, "function")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "read_file")
		So(fn["arguments"], ShouldEqual, "{\"path\":\"a.txt\"}")
	})
}

func TestConvertFunctionCallItem_WithNamespaceDoubleUnderscore(t *testing.T) {
	Convey("convertFunctionCallItem: namespace 以 __ 结尾时拼接为 ns+name", t, func() {
		item := map[string]interface{}{
			"type":      "function_call",
			"call_id":   "c2",
			"namespace": "myapp__",
			"name":      "exec",
			"arguments": "{}",
		}

		result := convFnCallItem(item)

		So(result["role"], ShouldEqual, "assistant")
		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c2")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "myapp__exec")
		So(fn["arguments"], ShouldEqual, "{}")
	})
}

func TestConvertFunctionCallItem_WithNamespaceNoSuffix(t *testing.T) {
	Convey("convertFunctionCallItem: namespace 不带 __ 后缀时 fallback 为 ns+__+name", t, func() {
		item := map[string]interface{}{
			"type":      "function_call",
			"call_id":   "c3",
			"namespace": "shell",
			"name":      "run",
			"arguments": "{}",
		}

		result := convFnCallItem(item)

		So(result["role"], ShouldEqual, "assistant")
		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c3")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "shell__run")
		So(fn["arguments"], ShouldEqual, "{}")
	})
}

func TestConvertFunctionCallItem_EmptyArguments(t *testing.T) {
	Convey("convertFunctionCallItem: arguments 为空时 fallback 为 \"{}\"（回归保护）", t, func() {
		item := map[string]interface{}{
			"type":    "function_call",
			"call_id": "c4",
			"name":    "noop",
		}

		result := convFnCallItem(item)

		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "noop")
		So(fn["arguments"], ShouldEqual, "{}")
	})
}

func TestConvertFunctionCallItem_EmptyNamespace(t *testing.T) {
	Convey("convertFunctionCallItem: namespace 字段存在但为空字符串时按无 namespace 处理", t, func() {
		item := map[string]interface{}{
			"type":      "function_call",
			"call_id":   "c5",
			"namespace": "",
			"name":      "foo",
			"arguments": "{}",
		}

		result := convFnCallItem(item)

		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "foo")
		So(fn["arguments"], ShouldEqual, "{}")
	})
}

func TestConvertFunctionCallItem_NamespaceWithoutName(t *testing.T) {
	Convey("convertFunctionCallItem: 有 namespace 无 name 时按 flattenNamespaceToolName 规约返回 namespace", t, func() {
		item := map[string]interface{}{
			"type":      "function_call",
			"call_id":   "c6",
			"namespace": "myapp__",
			"name":      "",
			"arguments": "{}",
		}

		result := convFnCallItem(item)

		So(result["role"], ShouldEqual, "assistant")
		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c6")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "myapp__")
		So(fn["arguments"], ShouldEqual, "{}")
	})
}

// chatRespWithToolCalls 构造一个仅含单个 tool_call 的 chat 响应 JSON。
func chatRespWithToolCalls(toolName, arguments, callID string) []byte {
	chatResp := map[string]interface{}{
		"id":      "chat_tc",
		"created": 1700000000,
		"model":   "gpt-test",
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"message": map[string]interface{}{
					"role": "assistant",
					"tool_calls": []interface{}{
						map[string]interface{}{
							"id":   callID,
							"type": "function",
							"function": map[string]interface{}{
								"name":      toolName,
								"arguments": arguments,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
	b, _ := json.Marshal(chatResp)
	return b
}

// findItemByType 从 output 数组中按 type 过滤返回第一个匹配的 item。
func findItemByType(output []interface{}, t string) map[string]interface{} {
	for _, o := range output {
		if om, ok := o.(map[string]interface{}); ok {
			if typ, _ := om["type"].(string); typ == t {
				return om
			}
		}
	}
	return nil
}

func TestConvertChatResponseToResponses_BackwardCompat(t *testing.T) {
	Convey("ConvertChatResponseToResponses（3 参数旧入口）: 含 tool_calls 时行为完全不变", t, func() {

		Convey("namespace 风格 tool_call name → 输出 function_call.name=原 name，无 namespace 字段，无 id/status", func() {
			chatBody := chatRespWithToolCalls("myapp__exec", "{\"cmd\":\"ls\"}", "call_001")

			result := convRespRaw(chatBody, "gpt-test", false)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "function_call")
			So(item["call_id"], ShouldEqual, "call_001")
			So(item["name"], ShouldEqual, "myapp__exec")
			So(item["arguments"], ShouldEqual, "{\"cmd\":\"ls\"}")
			// 老行为：不写 id / status / namespace
			_, hasID := item["id"]
			So(hasID, ShouldBeFalse)
			_, hasStatus := item["status"]
			So(hasStatus, ShouldBeFalse)
			_, hasNs := item["namespace"]
			So(hasNs, ShouldBeFalse)
		})

		Convey("自定义工具代理名 → 旧入口仅作为普通 function_call 输出，不变 custom_tool_call", func() {
			chatBody := chatRespWithToolCalls("apply_patch_add_file", "{\"path\":\"a.txt\",\"content\":\"x\"}", "call_002")

			result := convRespRaw(chatBody, "gpt-test", false)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "function_call")
			So(item["name"], ShouldEqual, "apply_patch_add_file")
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_NamespaceAndCustomSemantics(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 保留 namespace / custom_tool_call / status / input / output 语义", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
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
		chatResp := map[string]interface{}{
			"id":      "chat_sem",
			"created": 1700000000,
			"model":   "gpt-test",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role": "assistant",
						"tool_calls": []interface{}{
							map[string]interface{}{
								"id":   "call_a",
								"type": "function",
								"function": map[string]interface{}{
									"name":      "apply_patch_add_file",
									"arguments": "{\"path\":\"a.txt\",\"content\":\"x\"}",
								},
							},
							map[string]interface{}{
								"id":   "call_b",
								"type": "function",
								"function": map[string]interface{}{
									"name":      "team__spawn_agent",
									"arguments": "{}",
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
		}
		chatBody, _ := json.Marshal(chatResp)

		result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 2)

		customItem := output[0].(map[string]interface{})
		So(customItem["type"], ShouldEqual, "custom_tool_call")
		So(customItem["name"], ShouldEqual, "apply_patch")
		So(customItem["call_id"], ShouldEqual, "call_a")
		So(customItem["id"], ShouldEqual, "ctc_call_a")
		So(customItem["status"], ShouldEqual, "completed")
		So(customItem["input"], ShouldEqual, "*** Begin Patch\n*** Add File: a.txt\n+x\n*** End Patch")

		nsItem := output[1].(map[string]interface{})
		So(nsItem["type"], ShouldEqual, "function_call")
		So(nsItem["name"], ShouldEqual, "spawn_agent")
		So(nsItem["namespace"], ShouldEqual, "team__")
		So(nsItem["call_id"], ShouldEqual, "call_b")
		So(nsItem["id"], ShouldEqual, "fc_call_b")
		So(nsItem["status"], ShouldEqual, "completed")
		So(nsItem["arguments"], ShouldEqual, "{}")
	})
}

func TestConvertChatResponseToResponsesWithContext_PlainFunction(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 请求里只有普通 function 工具时", t, func() {

		Convey("function.name=原名，无 namespace 字段，补 id 和 status", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"tools": [
					{"type": "function", "name": "get_weather", "description": "Get weather", "parameters": {"type": "object"}}
				]
			}`)
			chatBody := chatRespWithToolCalls("get_weather", "{\"city\":\"SF\"}", "call_100")

			result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "function_call")
			So(item["call_id"], ShouldEqual, "call_100")
			So(item["name"], ShouldEqual, "get_weather")
			So(item["arguments"], ShouldEqual, "{\"city\":\"SF\"}")
			// 新行为：与流式对齐，补 id / status
			So(item["id"], ShouldEqual, "fc_call_100")
			So(item["status"], ShouldEqual, "completed")
			// 普通 function 无 namespace
			_, hasNs := item["namespace"]
			So(hasNs, ShouldBeFalse)
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_NamespaceFunction(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 请求里有 namespace 工具时", t, func() {

		Convey("上游返回 myapp__exec → output 为 function_call.name=exec、namespace=myapp__", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"tools": [
					{
						"type": "namespace",
						"name": "myapp__",
						"tools": [
							{"type": "function", "name": "exec", "description": "run command", "parameters": {"type": "object"}}
						]
					}
				]
			}`)
			chatBody := chatRespWithToolCalls("myapp__exec", "{\"cmd\":\"ls\"}", "call_200")

			result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "function_call")
			So(item["name"], ShouldEqual, "exec")
			So(item["namespace"], ShouldEqual, "myapp__")
			So(item["call_id"], ShouldEqual, "call_200")
			So(item["arguments"], ShouldEqual, "{\"cmd\":\"ls\"}")
			So(item["id"], ShouldEqual, "fc_call_200")
			So(item["status"], ShouldEqual, "completed")
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_CustomTool(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 请求里有 custom 工具（非 apply_patch）时", t, func() {

		Convey("上游返回 custom name、arguments 含 input → 输出 custom_tool_call 且 input=raw text", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"tools": [
					{"type": "custom", "name": "my_grammar", "description": "user grammar"}
				]
			}`)
			chatBody := chatRespWithToolCalls("my_grammar", "{\"input\":\"raw text\"}", "call_300")

			result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "custom_tool_call")
			So(item["name"], ShouldEqual, "my_grammar")
			So(item["input"], ShouldEqual, "raw text")
			So(item["call_id"], ShouldEqual, "call_300")
			So(item["id"], ShouldEqual, "ctc_call_300")
			So(item["status"], ShouldEqual, "completed")
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_ApplyPatchProxy(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 请求里有 apply_patch 自定义工具时", t, func() {

		Convey("上游返回 apply_patch_add_file → 输出 custom_tool_call.name=apply_patch，input 还原为 apply_patch 语法", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"tools": [
					{"type": "custom", "name": "apply_patch", "description": "patch files"}
				]
			}`)
			chatBody := chatRespWithToolCalls("apply_patch_add_file", "{\"path\":\"a.txt\",\"content\":\"hello\"}", "call_400")

			result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "custom_tool_call")
			So(item["name"], ShouldEqual, "apply_patch")
			So(item["call_id"], ShouldEqual, "call_400")
			So(item["id"], ShouldEqual, "ctc_call_400")
			So(item["status"], ShouldEqual, "completed")
			expected := "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch"
			So(item["input"], ShouldEqual, expected)
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_NilRequest(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 传 nil originalRequestRawJSON 时", t, func() {

		Convey("退化到旧 3 参数行为：tool_call 原样输出，无 id/status/namespace/custom_tool_call", func() {
			chatBody := chatRespWithToolCalls("myapp__exec", "{\"cmd\":\"ls\"}", "call_500")

			result := convRespCtxRaw(chatBody, "gpt-test", false, nil)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "function_call")
			So(item["name"], ShouldEqual, "myapp__exec")
			So(item["call_id"], ShouldEqual, "call_500")
			// 退化：补字段也不写
			_, hasID := item["id"]
			So(hasID, ShouldBeFalse)
			_, hasStatus := item["status"]
			So(hasStatus, ShouldBeFalse)
			_, hasNs := item["namespace"]
			So(hasNs, ShouldBeFalse)
		})

		Convey("新旧入口在 nil rawJSON 场景下输出完全一致", func() {
			chatBody := chatRespWithToolCalls("apply_patch_add_file", "{\"path\":\"a.txt\",\"content\":\"x\"}", "call_501")

			oldOut := convRespRaw(chatBody, "gpt-test", false)
			newOut := convRespCtxRaw(chatBody, "gpt-test", false, nil)

			So(string(newOut), ShouldEqualJSON, string(oldOut))
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_UnknownTool(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 上游返回的 tool_call name 不在 CodexCtx 中时", t, func() {

		Convey("走 function_call fallback：name=原名，无 namespace", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"tools": [
					{"type": "function", "name": "registered_tool", "description": "x", "parameters": {"type": "object"}}
				]
			}`)
			chatBody := chatRespWithToolCalls("unknown_fn", "{\"a\":1}", "call_600")

			result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "function_call")
			So(item["name"], ShouldEqual, "unknown_fn")
			_, hasNs := item["namespace"]
			So(hasNs, ShouldBeFalse)
		})
	})
}

// -----------------------------------------------------------------------------
// #2 修复：convertInputItem 补齐 codex 历史事件类型
// -----------------------------------------------------------------------------

func TestConvertInputItem_CustomToolCall_ApplyPatch(t *testing.T) {
	Convey("convertInputItem: custom_tool_call（P0-2）历史 input 按声明 schema 编码为 {\"input\":raw} arguments", t, func() {
		patch := "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch"
		msg, err := convertInputItem(map[string]interface{}{
			"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": patch,
		}, nil)
		So(err, ShouldBeNil)
		So(msg["role"], ShouldEqual, "assistant")
		tcs, ok := msg["tool_calls"].([]interface{})
		So(ok, ShouldBeTrue)
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c1")
		So(tc["type"], ShouldEqual, "function")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "apply_patch")
		assertCustomArgumentsMatchInputSchema(fn["arguments"].(string), patch)
	})
}

// assertCustomArgumentsMatchInputSchema 校验 arguments JSON 与 custom 工具声明的
// {type:object, properties:{input:{type:string}}, required:[input]} schema 一致（P0-2）。
func assertCustomArgumentsMatchInputSchema(arguments string, wantInput string) {
	var argsMap map[string]interface{}
	So(json.Unmarshal([]byte(arguments), &argsMap), ShouldBeNil)
	inputVal, has := argsMap["input"]
	So(has, ShouldBeTrue)
	input, isStr := inputVal.(string)
	So(isStr, ShouldBeTrue)
	So(input, ShouldEqual, wantInput)
}

func TestConvertInputItem_CustomToolCall_PlainCustom(t *testing.T) {
	Convey("convertInputItem: 普通 custom 工具同样按 {\"input\":...} 包装（声明与调用同 schema）", t, func() {
		msg, err := convertInputItem(map[string]interface{}{
			"type": "custom_tool_call", "call_id": "c2", "name": "my_grammar", "input": "some raw text",
		}, nil)
		So(err, ShouldBeNil)
		tcs := msg["tool_calls"].([]interface{})
		fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "my_grammar")
		assertCustomArgumentsMatchInputSchema(fn["arguments"].(string), "some raw text")
	})
}

func TestConvertInputItem_CustomToolCallOutput_StringOutput(t *testing.T) {
	Convey("convertInputItem: type:custom_tool_call_output 字符串 output → role:tool 消息，content=output 原文", t, func() {
		item := map[string]interface{}{
			"type":    "custom_tool_call_output",
			"call_id": "c1",
			"output":  "result text",
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "tool")
		So(result["tool_call_id"], ShouldEqual, "c1")
		So(result["content"], ShouldEqual, "result text")
	})
}

func TestConvertInputItem_CustomToolCallOutput_ObjectOutput(t *testing.T) {
	Convey("convertInputItem: type:custom_tool_call_output 对象 output 含 text 字段 → 归一化抽取 text 作为 content", t, func() {
		item := map[string]interface{}{
			"type":    "custom_tool_call_output",
			"call_id": "c1",
			"output": map[string]interface{}{
				"type": "text",
				"text": "obj result",
			},
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "tool")
		So(result["tool_call_id"], ShouldEqual, "c1")
		So(result["content"], ShouldEqual, "obj result")
	})
}

func TestConvertInputItem_Reasoning(t *testing.T) {
	Convey("convertInputItem: type:reasoning → 转为 assistant reasoning_content，summary_text 用换行拼接", t, func() {
		item := map[string]interface{}{
			"type": "reasoning",
			"summary": []interface{}{
				map[string]interface{}{"type": "summary_text", "text": "thinking 1"},
				map[string]interface{}{"type": "summary_text", "text": "thinking 2"},
			},
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "assistant")
		So(result["reasoning_content"], ShouldEqual, "thinking 1\nthinking 2")
		_, hasContent := result["content"]
		So(hasContent, ShouldBeFalse)
	})
}

func TestConvertInputItem_ReasoningEmptyIgnored(t *testing.T) {
	Convey("convertInputItem: 空 reasoning 不产出空消息；能力性不可映射 block/item 丢弃并放行（P1-2）", t, func() {
		Convey("空 reasoning 无信息量 → nil 且不报错", func() {
			msg, err := convertInputItem(map[string]interface{}{"type": "reasoning"}, nil)
			So(msg, ShouldBeNil)
			So(err, ShouldBeNil)
		})

		Convey("summary block type 非 summary_text → 丢弃该 block，保留其余有效 summary", func() {
			msg, err := convertInputItem(map[string]interface{}{
				"type": "reasoning",
				"summary": []interface{}{
					map[string]interface{}{"type": "other", "text": "ignored"},
					map[string]interface{}{"type": "summary_text", "text": "kept"},
				},
			}, nil)
			So(err, ShouldBeNil)
			So(msg, ShouldNotBeNil)
			So(msg["reasoning_content"], ShouldEqual, "kept")
		})

		Convey("summary_text block 缺 text → invalid_source_shape（结构性非法维持 400）", func() {
			_, err := convertInputItem(map[string]interface{}{
				"type":    "reasoning",
				"summary": []interface{}{map[string]interface{}{"type": "summary_text"}},
			}, nil)
			soPCE(err, relaymodel.CodeInvalidSourceShape)
		})

		Convey("content block 数组非 reasoning_text 元素 → 丢弃该 block 继续（不再拒绝）", func() {
			msg, err := convertInputItem(map[string]interface{}{
				"type": "reasoning",
				"content": []interface{}{
					map[string]interface{}{"text": "ignored"},
					map[string]interface{}{"type": "reasoning_text", "text": "kept reasoning"},
				},
			}, nil)
			So(err, ShouldBeNil)
			So(msg, ShouldNotBeNil)
			So(msg["reasoning_content"], ShouldEqual, "kept reasoning")
		})

		Convey("仅 encrypted_content 的 reasoning item → 丢弃该 item 放行（不报错、无输出）", func() {
			msg, err := convertInputItem(map[string]interface{}{
				"type":              "reasoning",
				"encrypted_content": "enc_123",
			}, nil)
			So(err, ShouldBeNil)
			So(msg, ShouldBeNil)
		})
	})
}

func TestConvertResponsesToChatRequest_ReasoningInputUsesReasoningContent(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: input reasoning 不混入普通 content", t, func() {
		reqBody := []byte(`{
			"model": "deepseek-test",
			"input": [
				{"type": "message", "role": "user", "content": "continue"},
				{"type": "reasoning", "summary": [{"type":"summary_text", "text":"private reasoning"}]}
			]
		}`)

		chatReq := parseChatRequest(convReqRaw("deepseek-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 2)
		reasoningMsg := messages[1].(map[string]interface{})
		So(reasoningMsg["role"], ShouldEqual, "assistant")
		So(reasoningMsg["reasoning_content"], ShouldEqual, "private reasoning")
		_, hasContent := reasoningMsg["content"]
		So(hasContent, ShouldBeFalse)
	})
}

func TestConvertInputItem_ToolSearchCall(t *testing.T) {
	Convey("convertInputItem: type:tool_search_call → 转为 assistant message + tool_calls", t, func() {
		item := map[string]interface{}{
			"type":      "tool_search_call",
			"call_id":   "ts1",
			"name":      "tool_search",
			"arguments": map[string]interface{}{"query": "test"},
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "assistant")
		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		So(tcs[0].(map[string]interface{})["id"], ShouldEqual, "ts1")
		So(tcs[0].(map[string]interface{})["function"].(map[string]interface{})["name"], ShouldEqual, "tool_search")
	})
}

func TestConvertInputItem_ToolSearchCallOutput(t *testing.T) {
	Convey("convertInputItem: type:tool_search_call_output → 转为 role:tool 消息", t, func() {
		item := map[string]interface{}{
			"type":    "tool_search_call_output",
			"call_id": "ts1",
			"output":  []interface{}{map[string]interface{}{"result": "x"}},
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "tool")
		So(result["tool_call_id"], ShouldEqual, "ts1")
		So(result["content"], ShouldEqual, `[{"result":"x"}]`)
	})
}

func TestConvertInputItem_WebSearchCall(t *testing.T) {
	Convey("convertInputItem: type:web_search_call → 转为 assistant message + tool_calls", t, func() {
		item := map[string]interface{}{
			"type":      "web_search_call",
			"call_id":   "ws1",
			"name":      "web_search",
			"arguments": map[string]interface{}{"query": "test"},
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "assistant")
		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		So(tcs[0].(map[string]interface{})["id"], ShouldEqual, "ws1")
		So(tcs[0].(map[string]interface{})["function"].(map[string]interface{})["name"], ShouldEqual, "web_search")
	})
}

func TestConvertInputItem_WebSearchCallOutput(t *testing.T) {
	Convey("convertInputItem: type:web_search_call_output → 转为 role:tool 消息", t, func() {
		item := map[string]interface{}{
			"type":    "web_search_call_output",
			"call_id": "ws1",
			"output":  []interface{}{map[string]interface{}{"result": "x"}},
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "tool")
		So(result["tool_call_id"], ShouldEqual, "ws1")
		So(result["content"], ShouldEqual, `[{"result":"x"}]`)
	})
}

func TestConvertInputItem_UnknownType_Fallback(t *testing.T) {
	Convey("convertInputItem: 未知 type → 丢弃该 item 且不报错（不再拒绝整个请求）", t, func() {
		msg, err := convertInputItem(map[string]interface{}{"type": "future_type"}, nil)
		So(err, ShouldBeNil)
		So(msg, ShouldBeNil)
	})
}

func TestConvertInputToMessages_MixedWithNewCases(t *testing.T) {
	Convey("convertInputToMessages: 混合历史 → 连续 calls 合并（P1-5）、custom arguments 包装（P0-2）", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "hi"},
			map[string]interface{}{"type": "function_call", "call_id": "fc1", "name": "read_file", "arguments": "{\"path\":\"a.txt\"}"},
			map[string]interface{}{"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": "patch text"},
			map[string]interface{}{"type": "custom_tool_call_output", "call_id": "c1", "output": "patch applied"},
			map[string]interface{}{"type": "tool_search_call", "call_id": "ts1", "name": "search_docs", "arguments": map[string]interface{}{"query": "codex"}},
			map[string]interface{}{"type": "reasoning", "content": "thinking..."},
		}

		msgs, err := convertInputToMessages(input)
		So(err, ShouldBeNil)
		// function_call 与紧随的 custom_tool_call 合并为同一 assistant.tool_calls[]（P1-5）：6 items → 5 messages
		So(len(msgs), ShouldEqual, 5)

		m0 := msgs[0].(map[string]interface{})
		So(m0["role"], ShouldEqual, "user")
		So(m0["content"], ShouldEqual, "hi")

		m1 := msgs[1].(map[string]interface{})
		So(m1["role"], ShouldEqual, "assistant")
		_, hasContent := m1["content"]
		So(hasContent, ShouldBeFalse)
		tcs := m1["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 2)
		fc1 := tcs[0].(map[string]interface{})
		So(fc1["id"], ShouldEqual, "fc1")
		So(fc1["function"].(map[string]interface{})["name"], ShouldEqual, "read_file")
		So(fc1["function"].(map[string]interface{})["arguments"], ShouldEqual, "{\"path\":\"a.txt\"}")
		ctc := tcs[1].(map[string]interface{})
		So(ctc["id"], ShouldEqual, "c1")
		So(ctc["type"], ShouldEqual, "function")
		So(ctc["function"].(map[string]interface{})["name"], ShouldEqual, "apply_patch")
		assertCustomArgumentsMatchInputSchema(ctc["function"].(map[string]interface{})["arguments"].(string), "patch text")

		m2 := msgs[2].(map[string]interface{})
		So(m2["role"], ShouldEqual, "tool")
		So(m2["tool_call_id"], ShouldEqual, "c1")
		So(m2["content"], ShouldEqual, "patch applied")

		m3 := msgs[3].(map[string]interface{})
		So(m3["role"], ShouldEqual, "assistant")
		So(m3["tool_calls"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})["name"], ShouldEqual, "tool_search")

		m4 := msgs[4].(map[string]interface{})
		So(m4["role"], ShouldEqual, "assistant")
		So(m4["reasoning_content"], ShouldEqual, "thinking...")
		_, hasContent4 := m4["content"]
		So(hasContent4, ShouldBeFalse)
	})
}

func TestResponsesChatResponsesRoundTrip_ToolSearchCallPreserved(t *testing.T) {
	Convey("responses→chat→responses: function/custom/tool_search/web_search 保留；连续 calls 合并（P1-5）", t, func() {
		patch := "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch"
		input := []interface{}{
			map[string]interface{}{"type": "function_call", "call_id": "fc1", "name": "read_file", "arguments": "{\"path\":\"a.txt\"}"},
			map[string]interface{}{"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": patch},
			map[string]interface{}{"type": "tool_search_call", "call_id": "ts1", "name": "search_docs", "arguments": map[string]interface{}{"query": "codex", "top_k": 3}},
			map[string]interface{}{"type": "web_search_call", "call_id": "ws1", "name": "web_search", "arguments": map[string]interface{}{"query": "codex"}},
		}

		msgs, err := convertInputToMessages(input)
		So(err, ShouldBeNil)
		// 4 个连续 call items 合并为一条 assistant message（tool_calls[] 保序）
		So(len(msgs), ShouldEqual, 1)
		toolCalls, ok := msgs[0].(map[string]interface{})["tool_calls"].([]interface{})
		So(ok, ShouldBeTrue)
		So(len(toolCalls), ShouldEqual, 4)

		chatResp := map[string]interface{}{
			"id":      "chat_rt",
			"created": 1700000000,
			"model":   "gpt-test",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role":       "assistant",
						"tool_calls": toolCalls,
					},
					"finish_reason": "tool_calls",
				},
			},
		}
		chatBody, _ := json.Marshal(chatResp)
		reqBody := []byte(`{"model":"gpt-test","tools":[{"type":"function","name":"read_file","description":"x","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch","description":"patch files"},{"type":"tool_search","name":"tool_search"},{"type":"web_search","name":"web_search"}]}`)

		result, err := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
		So(err, ShouldBeNil)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 4)
		So(output[0].(map[string]interface{})["type"], ShouldEqual, "function_call")
		So(output[0].(map[string]interface{})["call_id"], ShouldEqual, "fc1")
		customItem := output[1].(map[string]interface{})
		So(customItem["type"], ShouldEqual, "custom_tool_call")
		So(customItem["call_id"], ShouldEqual, "c1")
		// P0-2 往返闭环：{\"input\":raw} 包装的 arguments 经响应侧还原回 raw input
		So(customItem["input"], ShouldEqual, patch)
		So(output[2].(map[string]interface{})["type"], ShouldEqual, "tool_search_call")
		So(output[2].(map[string]interface{})["call_id"], ShouldEqual, "ts1")
		So(output[3].(map[string]interface{})["type"], ShouldEqual, "web_search_call")
		So(output[3].(map[string]interface{})["call_id"], ShouldEqual, "ws1")
	})
}

func TestConvertFunctionCallItem_NamespaceNonString(t *testing.T) {
	Convey("convertFunctionCallItem: namespace 字段非 string（nil / 数字 / 嵌套对象）→ type assertion 失败，跳过 flatten，原 name 保持", t, func() {

		Convey("namespace=nil → name 不被改写为 ns+name", func() {
			item := map[string]interface{}{
				"type":      "function_call",
				"call_id":   "c1",
				"namespace": nil,
				"name":      "exec",
				"arguments": "{}",
			}
			result := convFnCallItem(item)
			tcs := result["tool_calls"].([]interface{})
			fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "exec")
		})

		Convey("namespace=数字 → 跳过 flatten，name 保持原值", func() {
			item := map[string]interface{}{
				"type":      "function_call",
				"call_id":   "c2",
				"namespace": 42,
				"name":      "exec",
				"arguments": "{}",
			}
			result := convFnCallItem(item)
			tcs := result["tool_calls"].([]interface{})
			fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "exec")
		})

		Convey("namespace=嵌套对象 → 跳过 flatten，name 保持原值", func() {
			item := map[string]interface{}{
				"type":      "function_call",
				"call_id":   "c3",
				"namespace": map[string]interface{}{"name": "shell"},
				"name":      "exec",
				"arguments": "{}",
			}
			result := convFnCallItem(item)
			tcs := result["tool_calls"].([]interface{})
			fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "exec")
		})
	})
}

func TestConvertFunctionCallItem_NameWithoutArguments(t *testing.T) {
	Convey("convertFunctionCallItem: arguments 字段缺失（key 不存在）→ type assertion 失败，args=\"\" → fallback \"{}\"", t, func() {
		item := map[string]interface{}{
			"type":    "function_call",
			"call_id": "c1",
			"name":    "exec",
			// 注意：故意不设置 arguments 字段
		}

		result := convFnCallItem(item)

		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "exec")
		So(fn["arguments"], ShouldEqual, "{}")
	})
}

func TestConvertChatResponseToResponsesWithContext_EmptyTools(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: 请求含 instructions 但无 tools 字段 → codexCtx 非 nil（空 ctx）→ 仍走 new path 补 id/status，无 namespace", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"instructions": "be helpful"
		}`)
		chatBody := chatRespWithToolCalls("get_weather", "{\"city\":\"SF\"}", "call_empty")

		result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "function_call")
		So(item["call_id"], ShouldEqual, "call_empty")
		So(item["name"], ShouldEqual, "get_weather")
		So(item["arguments"], ShouldEqual, "{\"city\":\"SF\"}")
		// new path: codexCtx != nil（即使是空 ctx）仍补 id/status
		So(item["id"], ShouldEqual, "fc_call_empty")
		So(item["status"], ShouldEqual, "completed")
		// 无 namespace spec → 不写 namespace 字段
		_, hasNs := item["namespace"]
		So(hasNs, ShouldBeFalse)
	})
}

func TestConvertChatResponseToResponsesWithContext_ApplyPatchProxy_DeleteFile(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: apply_patch_delete_file → custom_tool_call.name=apply_patch，input 含 Delete File 块", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "custom", "name": "apply_patch", "description": "patch files"}
			]
		}`)
		chatBody := chatRespWithToolCalls("apply_patch_delete_file", "{\"path\":\"a.txt\"}", "call_del")

		result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["name"], ShouldEqual, "apply_patch")
		So(item["call_id"], ShouldEqual, "call_del")
		So(item["id"], ShouldEqual, "ctc_call_del")
		So(item["status"], ShouldEqual, "completed")
		expected := "*** Begin Patch\n*** Delete File: a.txt\n*** End Patch"
		So(item["input"], ShouldEqual, expected)
	})
}

func TestConvertChatResponseToResponsesWithContext_ApplyPatchProxy_UpdateFile(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: apply_patch_update_file → custom_tool_call.name=apply_patch，input 含 Update File + hunks 块", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "custom", "name": "apply_patch", "description": "patch files"}
			]
		}`)
		args := `{"path":"a.txt","hunks":[{"context":"ctx line","lines":[{"op":"context","text":"x"},{"op":"remove","text":"old"},{"op":"add","text":"new"}]}]}`
		chatBody := chatRespWithToolCalls("apply_patch_update_file", args, "call_upd")

		result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["name"], ShouldEqual, "apply_patch")
		So(item["call_id"], ShouldEqual, "call_upd")
		So(item["id"], ShouldEqual, "ctc_call_upd")
		So(item["status"], ShouldEqual, "completed")
		input := item["input"].(string)
		So(strings.Contains(input, "*** Update File: a.txt"), ShouldBeTrue)
		So(strings.Contains(input, "@@ ctx line"), ShouldBeTrue)
		So(strings.Contains(input, " x"), ShouldBeTrue)
		So(strings.Contains(input, "-old"), ShouldBeTrue)
		So(strings.Contains(input, "+new"), ShouldBeTrue)
		So(strings.Contains(input, "*** End Patch"), ShouldBeTrue)
	})
}

func TestConvertChatResponseToResponsesWithContext_ApplyPatchProxy_ReplaceFile(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: apply_patch_replace_file → custom_tool_call.name=apply_patch，input 用 Delete+Add File 表达替换", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "custom", "name": "apply_patch", "description": "patch files"}
			]
		}`)
		chatBody := chatRespWithToolCalls("apply_patch_replace_file", `{"path":"a.txt","content":"new content"}`, "call_rpl")

		result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["name"], ShouldEqual, "apply_patch")
		So(item["call_id"], ShouldEqual, "call_rpl")
		So(item["id"], ShouldEqual, "ctc_call_rpl")
		So(item["status"], ShouldEqual, "completed")
		expected := "*** Begin Patch\n*** Delete File: a.txt\n*** Add File: a.txt\n+new content\n*** End Patch"
		So(item["input"], ShouldEqual, expected)
	})
}

func TestConvertChatResponseToResponsesWithContext_ApplyPatchProxy_Batch(t *testing.T) {
	Convey("ConvertChatResponseToResponsesWithContext: apply_patch_batch → custom_tool_call.name=apply_patch，input 包含多文件操作", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "custom", "name": "apply_patch", "description": "patch files"}
			]
		}`)
		args := `{"operations":[{"type":"add_file","path":"a.txt","content":"hello"},{"type":"delete_file","path":"b.txt"}]}`
		chatBody := chatRespWithToolCalls("apply_patch_batch", args, "call_batch")

		result := convRespCtxRaw(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["name"], ShouldEqual, "apply_patch")
		So(item["call_id"], ShouldEqual, "call_batch")
		So(item["id"], ShouldEqual, "ctc_call_batch")
		So(item["status"], ShouldEqual, "completed")
		input := item["input"].(string)
		So(strings.Contains(input, "*** Add File: a.txt"), ShouldBeTrue)
		So(strings.Contains(input, "+hello"), ShouldBeTrue)
		So(strings.Contains(input, "*** Delete File: b.txt"), ShouldBeTrue)
		So(strings.Contains(input, "*** Begin Patch"), ShouldBeTrue)
		So(strings.Contains(input, "*** End Patch"), ShouldBeTrue)
	})
}

func TestConvertChatResponseToResponsesWithContext_ToolCallIDMissing(t *testing.T) {
	Convey("P0-3: 上游 tool_call 缺 id → malformed_tool_call，不产出空 call_id/空 id item", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "function", "name": "get_weather", "description": "x", "parameters": {"type": "object"}}
			]
		}`)
		chatResp := map[string]interface{}{
			"id":      "chat_no_id",
			"created": 1700000000,
			"model":   "gpt-test",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role": "assistant",
						"tool_calls": []interface{}{
							map[string]interface{}{
								"type": "function",
								"function": map[string]interface{}{
									"name":      "get_weather",
									"arguments": "{\"city\":\"SF\"}",
								},
							},
						},
					},
					"finish_reason": "tool_calls",
				},
			},
		}
		chatBody, _ := json.Marshal(chatResp)

		out, err := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
		So(out, ShouldBeNil)
		soPCE(err, relaymodel.CodeMalformedToolCall)
	})
}

func TestConvertResponsesToChatRequest_ReasoningEffortMapping(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: reasoning.effort 映射表", t, func() {
		reqBase := func(effort string) []byte {
			body, _ := json.Marshal(map[string]interface{}{
				"model":     "gpt-test",
				"input":     "hi",
				"reasoning": map[string]interface{}{"effort": effort},
			})
			return body
		}

		Convey("max → reasoning_effort=max（回归：Codex 默认发 max，之前落入 default 被映射成 auto）", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBase("max"), false))
			So(chatReq["reasoning_effort"], ShouldEqual, "max")
		})

		Convey("none/minimal/low/medium/high/xhigh 正常映射（minimal 直通，不降级为 low）", func() {
			So(parseChatRequest(convReqRaw("gpt-test", reqBase("none"), false))["reasoning_effort"], ShouldEqual, "none")
			So(parseChatRequest(convReqRaw("gpt-test", reqBase("minimal"), false))["reasoning_effort"], ShouldEqual, "minimal")
			So(parseChatRequest(convReqRaw("gpt-test", reqBase("low"), false))["reasoning_effort"], ShouldEqual, "low")
			So(parseChatRequest(convReqRaw("gpt-test", reqBase("medium"), false))["reasoning_effort"], ShouldEqual, "medium")
			So(parseChatRequest(convReqRaw("gpt-test", reqBase("high"), false))["reasoning_effort"], ShouldEqual, "high")
			So(parseChatRequest(convReqRaw("gpt-test", reqBase("xhigh"), false))["reasoning_effort"], ShouldEqual, "xhigh")
		})

		Convey("auto → 映射为 high（上游枚举不含 auto，透传会 400）", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBase("auto"), false))
			So(chatReq["reasoning_effort"], ShouldEqual, "high")
		})

		Convey("未知 effort → 兜底 high（不再兜底 auto）", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBase("ultra"), false))
			So(chatReq["reasoning_effort"], ShouldEqual, "high")
		})

		Convey("无 reasoning 字段 → 不设置 reasoning_effort", func() {
			body, _ := json.Marshal(map[string]interface{}{"model": "gpt-test", "input": "hi"})
			chatReq := parseChatRequest(convReqRaw("gpt-test", body, false))
			_, has := chatReq["reasoning_effort"]
			So(has, ShouldBeFalse)
		})
	})
}

// agentMessageWarnCounter 统计 path=agent_message 的 warnDropped 次数，其余日志委托原 logger。
type agentMessageWarnCounter struct {
	logger.ILogger
	drops int
}

func (l *agentMessageWarnCounter) Warnf(format string, args ...interface{}) {
	if strings.Contains(fmt.Sprintf(format, args...), "path=agent_message") {
		l.drops++
		return
	}
	l.ILogger.Warnf(format, args...)
}

// countAgentMessageWarnDrops 在 run 执行期间收集 agent_message 块丢弃告警并返回其条数。
func countAgentMessageWarnDrops(run func()) int {
	orig := logger.Log
	cl := &agentMessageWarnCounter{ILogger: orig}
	logger.Log = cl
	defer func() { logger.Log = orig }()
	run()
	return cl.drops
}

func TestConvertResponsesToChatRequest_AgentMessageItem(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: input 中的 agent_message item 转为 assistant message（phase 按 DN-5 丢弃标记保留文本）", t, func() {

		Convey("T1: 单个 agent_message 含 2 个 text 块 → assistant 消息按换行拼接且顺序正确", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_1",
						"content": [
							{"type": "text", "text": "first part"},
							{"type": "text", "text": "second part"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "first part\nsecond part")
		})

		Convey("T2: agent_message content 为空数组 → 无信息量丢弃且不报错", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_2", "content": []},
					{"type": "message", "role": "user", "content": "hi"}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["content"], ShouldEqual, "hi")
		})

		Convey("T3: agent_message 与 message 混排 → 顺序保持、各自内容正确", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "message", "role": "user", "content": "hello"},
					{
						"type": "agent_message",
						"id": "am_3",
						"content": [
							{"type": "text", "text": "agent reply"}
						]
					},
					{"type": "message", "role": "user", "content": "again"}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 3)
			m0 := messages[0].(map[string]interface{})
			So(m0["role"], ShouldEqual, "user")
			So(m0["content"], ShouldEqual, "hello")
			m1 := messages[1].(map[string]interface{})
			So(m1["role"], ShouldEqual, "assistant")
			So(m1["content"], ShouldEqual, "agent reply")
			m2 := messages[2].(map[string]interface{})
			So(m2["role"], ShouldEqual, "user")
			So(m2["content"], ShouldEqual, "again")
		})

		Convey("T4: content 混排非文本块 → 丢弃非文本块，保留文本并放行（§3.9 文本块接受 text/input_text/output_text）", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_4",
						"content": [
							{"type": "image_url", "image_url": {"url": "http://x"}},
							{"type": "text", "text": "valid part"}
						]
					}
				]
			}`)
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "valid part")
		})

		Convey("T5: agent_message 含 input_text 块 → 文本保留并产出 assistant 消息（真实 codex CLI 流量回归锁定）", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_5",
						"content": [
							{"type": "input_text", "text": "Sender: /root/task_a\nagent done"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			content := msg["content"].(string)
			So(strings.Contains(content, "Sender: /root/task_a"), ShouldBeTrue)
			So(strings.Contains(content, "agent done"), ShouldBeTrue)
		})

		Convey("T6: input_text/encrypted_content 与非法块（图片）混排 → 文本合并，图片丢弃并计入 dropped 日志计数", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_6",
						"content": [
							{"type": "input_text", "text": "first input part"},
							{"type": "image_url", "image_url": {"url": "http://x"}},
							{"type": "encrypted_content", "encrypted_content": "payload part"}
						]
					}
				]
			}`)

			var chatReq map[string]interface{}
			drops := countAgentMessageWarnDrops(func() {
				chatReq = parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			})

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "first input part\npayload part")
			So(drops, ShouldEqual, 1)
		})

		Convey("T9: 真实流量形状 input_text 表头 + encrypted_content 明文载荷 → 表头与正文都保留且顺序正确", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_9",
						"content": [
							{"type": "input_text", "text": "Message Type: NEW_TASK\nTask name: /root/utility_make_test_file\nSender: /root\nPayload:\n"},
							{"type": "encrypted_content", "encrypted_content": "请在 /Users/rafe/work/github/myapi 当前目录下创建 test.txt，内容为一个随机数字"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			content := msg["content"].(string)
			So(strings.Contains(content, "Message Type: NEW_TASK"), ShouldBeTrue)
			So(strings.Contains(content, "Payload:"), ShouldBeTrue)
			So(strings.Contains(content, "创建 test.txt"), ShouldBeTrue)
			So(strings.Index(content, "Message Type: NEW_TASK"), ShouldBeLessThan, strings.Index(content, "创建 test.txt"))
		})

		Convey("T10a: encrypted_content 字段缺失 → 块丢弃、warnDropped 计数 +1，请求其余消息保留", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_10a", "content": [{"type": "encrypted_content"}]},
					{"type": "message", "role": "user", "content": "hi"}
				]
			}`)

			var chatReq map[string]interface{}
			drops := countAgentMessageWarnDrops(func() {
				chatReq = parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			})

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["content"], ShouldEqual, "hi")
			So(drops, ShouldEqual, 1)
		})

		Convey("T10b: encrypted_content 字段非字符串 → 块丢弃、warnDropped 计数 +1", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_10b", "content": [{"type": "encrypted_content", "encrypted_content": 123}]},
					{"type": "message", "role": "user", "content": "hi"}
				]
			}`)

			var chatReq map[string]interface{}
			drops := countAgentMessageWarnDrops(func() {
				chatReq = parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			})

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["content"], ShouldEqual, "hi")
			So(drops, ShouldEqual, 1)
		})

		Convey("T10c: encrypted_content 字段为空串 → 块丢弃、warnDropped 计数 +1", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_10c", "content": [{"type": "encrypted_content", "encrypted_content": ""}]},
					{"type": "message", "role": "user", "content": "hi"}
				]
			}`)

			var chatReq map[string]interface{}
			drops := countAgentMessageWarnDrops(func() {
				chatReq = parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			})

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["content"], ShouldEqual, "hi")
			So(drops, ShouldEqual, 1)
		})

		Convey("T10d: encrypted_content 字段为 JSON null → 块丢弃、warnDropped 计数 +1", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_10d", "content": [{"type": "encrypted_content", "encrypted_content": null}]},
					{"type": "message", "role": "user", "content": "hi"}
				]
			}`)

			var chatReq map[string]interface{}
			drops := countAgentMessageWarnDrops(func() {
				chatReq = parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			})

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["content"], ShouldEqual, "hi")
			So(drops, ShouldEqual, 1)
		})

		Convey("T11: 连续两个 encrypted_content 块 → 两段正文按块顺序拼接且都保留", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_11",
						"content": [
							{"type": "encrypted_content", "encrypted_content": "payload one"},
							{"type": "encrypted_content", "encrypted_content": "payload two"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "payload one\npayload two")
		})

		Convey("T12: 块无 type 字段但带 encrypted_content 明文 → 按载荷保留且请求不 400", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_12",
						"content": [
							{"encrypted_content": "Message Type: NEW_TASK\nPayload:\nbare payload body"}
						]
					}
				]
			}`)

			var chatReq map[string]interface{}
			drops := countAgentMessageWarnDrops(func() {
				chatReq = parseChatRequest(convReqRaw("gpt-test", reqBody, false))
			})

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "Message Type: NEW_TASK\nPayload:\nbare payload body")
			So(drops, ShouldEqual, 0)
		})

		Convey("T7: 块无 type 字段但有 text → 归一为 input_text 保留文本（与 message 路径口径对齐）", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_7",
						"content": [
							{"text": "no type text part"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "no type text part")
		})

		Convey("T8: content 含非对象块 → invalid_source_shape", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_7",
						"content": ["not-a-map"]
					}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", reqBody, false)
			So(out, ShouldBeNil)
			soPCE(err, relaymodel.CodeInvalidSourceShape)
		})
	})
}

func TestConvertResponsesToChatRequest_TextFormat(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: text.format → response_format", t, func() {

		Convey("T1: json_schema → 嵌套 json_schema 子键，name/schema/strict 直通且不携带内层 type", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": "hi",
				"text": {
					"format": {
						"type": "json_schema",
						"name": "fruit_schema",
						"description": "fruit list",
						"schema": {"type": "object", "properties": {"name": {"type": "string"}}},
						"strict": true
					}
				}
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			rf, ok := chatReq["response_format"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(rf["type"], ShouldEqual, "json_schema")
			inner, ok := rf["json_schema"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(inner["name"], ShouldEqual, "fruit_schema")
			So(inner["description"], ShouldEqual, "fruit list")
			So(inner["strict"], ShouldEqual, true)
			_, hasType := inner["type"]
			So(hasType, ShouldBeFalse)
			schema, ok := inner["schema"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(schema["type"], ShouldEqual, "object")
		})

		Convey("T2: json_object → 原样直通", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": "hi",
				"text": {"format": {"type": "json_object"}}
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			rf, ok := chatReq["response_format"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(rf["type"], ShouldEqual, "json_object")
		})

		Convey("T3: text → 原样直通", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": "hi",
				"text": {"format": {"type": "text"}}
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			rf, ok := chatReq["response_format"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(rf["type"], ShouldEqual, "text")
		})

		Convey("T4: text.format 缺失 → 不设置 response_format", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": "hi",
				"text": {"verbosity": "high"}
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			_, has := chatReq["response_format"]
			So(has, ShouldBeFalse)
		})

		Convey("T5: 未识别 format type → 原样直通", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": "hi",
				"text": {"format": {"type": "future_format", "extra": 1}}
			}`)

			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

			rf, ok := chatReq["response_format"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(rf["type"], ShouldEqual, "future_format")
			So(rf["extra"], ShouldEqual, float64(1))
		})
	})
}

func TestConvertResponsesToChatRequest_RefusalPart(t *testing.T) {
	reqWithAssistantContent := func(contentJSON string) string {
		return `{
			"model": "gpt-test",
			"input": [
				{
					"type": "message",
					"role": "__ROLE__",
					"content": ` + contentJSON + `
				}
			]
		}`
	}

	Convey("ConvertResponsesToChatRequest: role×part 矩阵（P1-3）—— refusal 仅 assistant 且与 text/media 互斥，违规 part 丢弃并放行", t, func() {

		Convey("T1: assistant text+refusal 并存 → 保留文本、丢弃 refusal 并放行", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", []byte(strings.Replace(reqWithAssistantContent(
				`[{"type": "output_text", "text": "I can do this part"},{"type": "refusal", "refusal": "but I cannot do that"}]`), `__ROLE__`, "assistant", 1)), false))
			msg := chatReq["messages"].([]interface{})[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "I can do this part")
		})

		Convey("T2: 仅 refusal part → content 数组只含 refusal part", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", []byte(strings.Replace(reqWithAssistantContent(
				`[{"type": "refusal", "refusal": "I refuse"}]`), `__ROLE__`, "assistant", 1)), false))
			messages := chatReq["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			content, ok := msg["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(content[0].(map[string]interface{})["refusal"], ShouldEqual, "I refuse")
		})

		Convey("T3: refusal part 文本为空 → 无信息量跳过，纯文本消息拼成字符串（锁定为预期降级）", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", []byte(strings.Replace(reqWithAssistantContent(
				`[{"type": "refusal", "refusal": ""},{"type": "output_text", "text": "normal reply"}]`), `__ROLE__`, "assistant", 1)), false))
			messages := chatReq["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			content, ok := msg["content"].(string)
			So(ok, ShouldBeTrue)
			So(content, ShouldEqual, "normal reply")
		})

		Convey("T4: user 消息含 refusal part → 丢弃该 refusal，保留文本并放行", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", []byte(strings.Replace(reqWithAssistantContent(
				`[{"type": "input_text", "text": "please help"},{"type": "refusal", "refusal": "stray refusal"}]`), `__ROLE__`, "user", 1)), false))
			msg := chatReq["messages"].([]interface{})[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "user")
			So(msg["content"], ShouldEqual, "please help")
		})

		Convey("T5: system 消息仅 refusal part → refusal 丢弃、整条消息无剩余内容 → 丢弃该 item 且不报错", func() {
			msg, err := convertMessageItem(map[string]interface{}{
				"type":    "message",
				"role":    "system",
				"content": []interface{}{map[string]interface{}{"type": "refusal", "refusal": "should not appear"}},
			})
			So(err, ShouldBeNil)
			So(msg, ShouldBeNil)
		})

		Convey("T6: [refusal, text] 乱序 → 保留文本、丢弃 refusal 并放行（互斥与顺序无关）", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", []byte(strings.Replace(reqWithAssistantContent(
				`[{"type": "refusal", "refusal": "but I cannot do that"},{"type": "output_text", "text": "I can do this part"}]`), `__ROLE__`, "assistant", 1)), false))
			msg := chatReq["messages"].([]interface{})[0].(map[string]interface{})
			So(msg["content"], ShouldEqual, "I can do this part")
		})

		Convey("T7: assistant refusal+image（[refusal, media] 乱序）→ 丢弃 assistant 不允许的 image，保留 refusal", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", []byte(strings.Replace(reqWithAssistantContent(
				`[{"type": "refusal", "refusal": "cannot show"},{"type": "input_image", "image_url": {"url": "http://x/img.png"}}]`), `__ROLE__`, "assistant", 1)), false))
			msg := chatReq["messages"].([]interface{})[0].(map[string]interface{})
			content, ok := msg["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "refusal")
		})

		Convey("T8: developer 消息含 image part → image 丢弃、消息无剩余内容 → 丢弃该 item 且不报错", func() {
			msg, err := convertMessageItem(map[string]interface{}{
				"type":    "message",
				"role":    "developer",
				"content": []interface{}{map[string]interface{}{"type": "input_image", "image_url": "http://img"}},
			})
			So(err, ShouldBeNil)
			So(msg, ShouldBeNil)
		})
	})
}

func parseResponsesMap(respBytes []byte) map[string]interface{} {
	var resp map[string]interface{}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil
	}
	return resp
}

func TestConvertChatResponseToResponses_Refusal(t *testing.T) {
	Convey("ConvertChatResponseToResponses: assistant message.refusal → refusal message item", t, func() {

		Convey("T1: 仅 refusal 无 content → output 含 refusal message item", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_ref",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"refusal": "I cannot help with that",
						},
						"finish_reason": "stop",
					},
				},
			}
			chatBody, _ := json.Marshal(chatResp)

			result := convRespRaw(chatBody, "gpt-test", false)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			msg := output[0].(map[string]interface{})
			So(msg["type"], ShouldEqual, "message")
			So(msg["role"], ShouldEqual, "assistant")
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 1)
			part := content[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "refusal")
			So(part["refusal"], ShouldEqual, "I cannot help with that")
		})

		Convey("T2: refusal + content → 合并进同一 message item 的两个 part", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_ref2",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "partial answer",
							"refusal": "rest refused",
						},
						"finish_reason": "stop",
					},
				},
			}
			chatBody, _ := json.Marshal(chatResp)

			result := convRespRaw(chatBody, "gpt-test", false)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 1)
			msg := output[0].(map[string]interface{})
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "output_text")
			So(content[0].(map[string]interface{})["text"], ShouldEqual, "partial answer")
			So(content[1].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(content[1].(map[string]interface{})["refusal"], ShouldEqual, "rest refused")
		})

		Convey("T3: refusal 为空字符串 → 行为与现状一致（无 message item）", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_ref3",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"refusal": "",
						},
						"finish_reason": "stop",
					},
				},
			}
			chatBody, _ := json.Marshal(chatResp)

			result := convRespRaw(chatBody, "gpt-test", false)
			output := parseOutputArray(result)

			So(len(output), ShouldEqual, 0)
		})
	})
}

func TestConvertChatResponseToResponses_FinishReasonStatus(t *testing.T) {
	Convey("ConvertChatResponseToResponses: finish_reason → status / incomplete_details", t, func() {

		chatBodyWithFinish := func(fr string) []byte {
			chatResp := map[string]interface{}{
				"id":      "chat_fr",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "partial",
						},
						"finish_reason": fr,
					},
				},
			}
			b, _ := json.Marshal(chatResp)
			return b
		}

		Convey("T1: length → status=incomplete + incomplete_details.reason=max_output_tokens + truncated", func() {
			resp := parseResponsesMap(convRespRaw(chatBodyWithFinish("length"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "incomplete")
			details := resp["incomplete_details"].(map[string]interface{})
			So(details["reason"], ShouldEqual, "max_output_tokens")
			So(resp["truncated"], ShouldEqual, true)
		})

		Convey("T2: stop → status=completed，incomplete_details 按 §4 输出 null（键恒存在）", func() {
			resp := parseResponsesMap(convRespRaw(chatBodyWithFinish("stop"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "completed")
			_, has := resp["incomplete_details"]
			So(has, ShouldBeTrue)
			So(resp["incomplete_details"], ShouldBeNil)
			_, hasErr := resp["error"]
			So(hasErr, ShouldBeTrue)
			So(resp["error"], ShouldBeNil)
			So(resp["truncated"], ShouldEqual, false)
		})

		Convey("T3: tool_calls → status=completed", func() {
			resp := parseResponsesMap(convRespRaw(chatBodyWithFinish("tool_calls"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "completed")
		})

		Convey("T4: content_filter → status=incomplete + incomplete_details.reason=content_filter + truncated", func() {
			resp := parseResponsesMap(convRespRaw(chatBodyWithFinish("content_filter"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "incomplete")
			details := resp["incomplete_details"].(map[string]interface{})
			So(details["reason"], ShouldEqual, "content_filter")
			So(resp["truncated"], ShouldEqual, true)
		})

		Convey("T5: choices 为空 → status=completed", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_nochoice",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{},
			}
			b, _ := json.Marshal(chatResp)
			resp := parseResponsesMap(convRespRaw(b, "gpt-test", false))
			So(resp["status"], ShouldEqual, "completed")
		})
	})
}

func TestConvertResponsesToChatRequest_ToolChoice(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: tool_choice 形状转换（responses §2 → chat §5.2）", t, func() {

		reqBody := func(toolChoice interface{}) []byte {
			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-test",
				"input": "go",
				"tools": []interface{}{
					map[string]interface{}{
						"type": "function",
						"name": "get_weather",
						"parameters": map[string]interface{}{
							"type": "object",
						},
					},
				},
				"tool_choice": toolChoice,
			})
			return body
		}

		Convey("T1: responses 的 {type:function, name} 对象 → chat 的 {type:function, function:{name}} 嵌套形状", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody(map[string]interface{}{
				"type": "function",
				"name": "get_weather",
			}), false))
			tc := chatReq["tool_choice"].(map[string]interface{})
			So(tc["type"], ShouldEqual, "function")
			fn := tc["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "get_weather")
		})

		Convey("T2: 已是 chat 嵌套形状 {type:function, function:{name}} → 原样透传（幂等）", func() {
			chatShape := map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": "get_weather",
				},
			}
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody(chatShape), false))
			So(chatReq["tool_choice"], ShouldResemble, chatShape)
		})

		Convey("T3: 字符串 auto/none/required → 原样直通", func() {
			for _, s := range []string{"auto", "none", "required"} {
				chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody(s), false))
				So(chatReq["tool_choice"], ShouldEqual, s)
			}
		})

		Convey("T4: 其他对象形式（如 chat 形状 custom）→ 原样直通", func() {
			custom := map[string]interface{}{
				"type":   "custom",
				"custom": map[string]interface{}{"name": "x"},
			}
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody(custom), false))
			So(chatReq["tool_choice"], ShouldResemble, custom)
		})

		Convey("T7: 字符串 function:<name> → chat {type:function, function:{name}}（chat §5.2 强制调用形状）", func() {
			chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody("function:get_weather"), false))
			tc := chatReq["tool_choice"].(map[string]interface{})
			So(tc["type"], ShouldEqual, "function")
			So(tc["function"].(map[string]interface{})["name"], ShouldEqual, "get_weather")
		})

		Convey("T8: 指向工具 id 等 chat 不可表达的字符串 → 丢弃整个 tool_choice 并放行（等效缺省 auto）", func() {
			for _, s := range []string{"call_abc123", "custom:apply_patch", "function:", "mcp:server_x"} {
				out, err := ConvertResponsesToChatRequest("gpt-test", reqBody(s), false)
				So(err, ShouldBeNil)
				So(out, ShouldNotBeNil)
				_, has := parseChatRequest(out)["tool_choice"]
				So(has, ShouldBeFalse)
			}
		})

		Convey("T9: 未知 tool_choice 对象 type → 丢弃整个 tool_choice 并放行", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", reqBody(map[string]interface{}{"type": "weird_shape"}), false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
			_, has := parseChatRequest(out)["tool_choice"]
			So(has, ShouldBeFalse)
		})

		Convey("T10: tool_choice 指向被 DN-6 丢弃的工具 → 连同 tool_choice 一并丢弃并放行", func() {
			body, _ := json.Marshal(map[string]interface{}{
				"model":       "gpt-test",
				"input":       "go",
				"tools":       []interface{}{map[string]interface{}{"type": "web_search"}},
				"tool_choice": "function:web_search",
			})
			out, err := ConvertResponsesToChatRequest("gpt-test", body, false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
			chatReq := parseChatRequest(out)
			_, hasTools := chatReq["tools"]
			So(hasTools, ShouldBeFalse)
			_, hasChoice := chatReq["tool_choice"]
			So(hasChoice, ShouldBeFalse)
		})

		Convey("T5: tool_choice 缺失 → 不设置该字段", func() {
			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-test",
				"input": "go",
				"tools": []interface{}{
					map[string]interface{}{
						"type": "function",
						"name": "get_weather",
					},
				},
			})
			chatReq := parseChatRequest(convReqRaw("gpt-test", body, false))
			_, has := chatReq["tool_choice"]
			So(has, ShouldBeFalse)
		})

		Convey("T6: 无 tools 时 tool_choice 不写入（沿用既有门槛）", func() {
			body, _ := json.Marshal(map[string]interface{}{
				"model":       "gpt-test",
				"input":       "go",
				"tool_choice": "auto",
			})
			chatReq := parseChatRequest(convReqRaw("gpt-test", body, false))
			_, has := chatReq["tool_choice"]
			So(has, ShouldBeFalse)
		})
	})
}

func TestConvertChatResponseToResponses_CreatedAtKey(t *testing.T) {
	Convey("ConvertChatResponseToResponses: chat created → responses created_at（responses §4）", t, func() {

		chatBody := map[string]interface{}{
			"id":      "chat_ts",
			"created": 1700000000,
			"model":   "gpt-test",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "hi",
					},
					"finish_reason": "stop",
				},
			},
		}
		b, _ := json.Marshal(chatBody)

		Convey("T1: 非流式响应写 created_at 键，值为 chat created 原值（Unix 秒）", func() {
			resp := parseResponsesMap(convRespRaw(b, "gpt-test", false))
			So(resp["created_at"], ShouldEqual, int64(1700000000))
		})

		Convey("T2: 不写 chat 风格的 created 键", func() {
			resp := parseResponsesMap(convRespRaw(b, "gpt-test", false))
			_, hasCreated := resp["created"]
			So(hasCreated, ShouldBeFalse)
		})

		Convey("T3: chat 响应缺 created → created_at 仍必含（responses §4，网关时间兜底，不伪装字段缺失）", func() {
			noCreated := map[string]interface{}{
				"id":    "chat_nots",
				"model": "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "hi",
						},
					},
				},
			}
			nb, _ := json.Marshal(noCreated)
			resp := parseResponsesMap(convRespRaw(nb, "gpt-test", false))
			ca, has := resp["created_at"]
			So(has, ShouldBeTrue)
			// JSON 数字解码为 float64（wire 层），网关时间兜底必为正 Unix 秒
			caNum, ok := ca.(float64)
			So(ok, ShouldBeTrue)
			So(caNum > 0, ShouldBeTrue)
		})
	})
}

func TestConvertContentArray_InputAudioPart(t *testing.T) {
	Convey("convertContentArray: responses input_audio part → chat input_audio part", t, func() {

		Convey("input_audio 子对象形状（data/format）→ 原样直通", func() {
			content := []interface{}{
				map[string]interface{}{
					"type": "input_audio",
					"input_audio": map[string]interface{}{
						"data":   "base64data",
						"format": "wav",
					},
				},
			}

			result := convContent(content, "user")
			parts, ok := result.([]interface{})
			So(ok, ShouldBeTrue)
			So(len(parts), ShouldEqual, 1)
			part := parts[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "input_audio")
			inner := part["input_audio"].(map[string]interface{})
			So(inner["data"], ShouldEqual, "base64data")
			So(inner["format"], ShouldEqual, "wav")
		})

		Convey("顶层 data/format 形状 → 组装为 input_audio 子对象", func() {
			content := []interface{}{
				map[string]interface{}{
					"type":   "input_audio",
					"data":   "rawdata",
					"format": "mp3",
				},
			}

			result := convContent(content, "user")
			parts := result.([]interface{})
			part := parts[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "input_audio")
			inner := part["input_audio"].(map[string]interface{})
			So(inner["data"], ShouldEqual, "rawdata")
			So(inner["format"], ShouldEqual, "mp3")
		})

		Convey("缺 data 或 format → invalid_source_shape（不再丢弃 part 回退纯文本）", func() {
			content := []interface{}{
				map[string]interface{}{
					"type":   "input_audio",
					"format": "wav",
				},
				map[string]interface{}{"type": "input_text", "text": "hello"},
			}
			_, err := convertContentArray(content, "user")
			soPCE(err, relaymodel.CodeInvalidSourceShape)
		})
	})
}

func TestConvertContentArray_InputFilePart(t *testing.T) {
	Convey("convertContentArray: responses input_file part → chat file part", t, func() {

		Convey("顶层 file_id → file 子对象", func() {
			content := []interface{}{
				map[string]interface{}{
					"type":    "input_file",
					"file_id": "file_abc",
				},
			}

			result := convContent(content, "user")
			parts, ok := result.([]interface{})
			So(ok, ShouldBeTrue)
			So(len(parts), ShouldEqual, 1)
			part := parts[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "file")
			inner := part["file"].(map[string]interface{})
			So(inner["file_id"], ShouldEqual, "file_abc")
		})

		Convey("file 子对象形状 → 原样直通", func() {
			content := []interface{}{
				map[string]interface{}{
					"type": "input_file",
					"file": map[string]interface{}{
						"filename":  "a.txt",
						"file_data": "base64",
					},
				},
			}

			result := convContent(content, "user")
			parts := result.([]interface{})
			part := parts[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "file")
			inner := part["file"].(map[string]interface{})
			So(inner["filename"], ShouldEqual, "a.txt")
			So(inner["file_data"], ShouldEqual, "base64")
		})

		Convey("空 file 对象 → invalid_source_shape（无 filename/file_data/file_id 载荷拒绝）", func() {
			content := []interface{}{
				map[string]interface{}{
					"type": "input_file",
					"file": map[string]interface{}{},
				},
			}
			_, err := convertContentArray(content, "user")
			soPCE(err, relaymodel.CodeInvalidSourceShape)
		})
	})
}

func TestConvertResponsesToChatRequest_InputAudioFileParts(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: user message content 含 input_audio/input_file → chat content parts 保真", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": [
				{
					"type": "message",
					"role": "user",
					"content": [
						{"type": "input_text", "text": "transcribe this"},
						{"type": "input_audio", "input_audio": {"data": "AAA", "format": "wav"}},
						{"type": "input_file", "file_id": "file_123"}
					]
				}
			]
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 1)
		msg := messages[0].(map[string]interface{})
		So(msg["role"], ShouldEqual, "user")
		content := msg["content"].([]interface{})
		So(len(content), ShouldEqual, 3)
		So(content[0].(map[string]interface{})["type"], ShouldEqual, "text")
		So(content[1].(map[string]interface{})["type"], ShouldEqual, "input_audio")
		So(content[2].(map[string]interface{})["type"], ShouldEqual, "file")
	})
}

// -----------------------------------------------------------------------------
// #3 修复：input item 已知不可映射类型区分 + developer role 保真
// -----------------------------------------------------------------------------

func TestConvertInputItem_KnownUnmappableTypes(t *testing.T) {
	Convey("convertInputItem: item_reference 与 §3.8 上下文 item 丢弃该 item 且不报错（保留同一请求其余历史）", t, func() {
		for _, typ := range []string{
			"file_search_call", "computer_call", "computer_call_output",
			"local_shell_call", "local_shell_call_output",
			"shell_call", "shell_call_output",
			"apply_patch_call", "apply_patch_call_output",
			"code_interpreter_call", "image_generation_call",
			"mcp_list_tools", "mcp_approval_request", "mcp_approval_response", "mcp_call",
			"additional_tools", "configuration_update",
			"compaction", "compaction_trigger", "item_reference",
			"program", "program_output",
		} {
			msg, err := convertInputItem(map[string]interface{}{"type": typ}, nil)
			So(err, ShouldBeNil)
			So(msg, ShouldBeNil)
		}
	})
}

func TestConvertMessageItem_DeveloperRole(t *testing.T) {
	Convey("convertInputItem: message role=developer → chat developer 消息（chat §3.1 已定义，不再降级 system）", t, func() {
		item := map[string]interface{}{
			"type":    "message",
			"role":    "developer",
			"content": "system rules",
		}

		result := convItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "developer")
		So(result["content"], ShouldEqual, "system rules")

		Convey("role 缺失仍默认 user，role=user/system 保持原样", func() {
			So(convItem(map[string]interface{}{"type": "message", "content": "x"}, nil)["role"], ShouldEqual, "user")
			So(convItem(map[string]interface{}{"type": "message", "role": "user", "content": "x"}, nil)["role"], ShouldEqual, "user")
			So(convItem(map[string]interface{}{"type": "message", "role": "system", "content": "x"}, nil)["role"], ShouldEqual, "system")
		})
	})
}

// -----------------------------------------------------------------------------
// #3 修复：工具类型补齐（responses §9 → chat §5.1 function 扁平化）
// -----------------------------------------------------------------------------

func TestConvertToolsToOpenAI_WebSearchPreview(t *testing.T) {
	Convey("convertToolsToOpenAI: web_search_preview（responses §9 预览名）同族丢弃且不报错（DN-6）", t, func() {
		out, err := convertToolsToOpenAI([]interface{}{map[string]interface{}{"type": "web_search_preview"}})
		So(err, ShouldBeNil)
		So(len(out), ShouldEqual, 0)
	})
}

func TestConvertToolsToOpenAI_ShellAndComputerFamily(t *testing.T) {
	Convey("convertToolsToOpenAI: shell / computer / computer_use_preview 丢弃且不报错（DN-6）", t, func() {
		for _, typ := range []string{"shell", "computer", "computer_use_preview"} {
			out, err := convertToolsToOpenAI([]interface{}{map[string]interface{}{"type": typ}})
			So(err, ShouldBeNil)
			So(len(out), ShouldEqual, 0)
		}
	})
}

func TestConvertToolsToOpenAI_ApplyPatchToolType(t *testing.T) {
	Convey("convertToolsToOpenAI: type:apply_patch（responses §9 独立类型）→ 主工具 + 5 代理子工具", t, func() {
		tools := []interface{}{
			map[string]interface{}{
				"type":        "apply_patch",
				"description": "patch files",
			},
		}

		result := convTools(tools)

		So(len(result), ShouldEqual, 6)
		fn := result[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "apply_patch")
		So(fn["description"], ShouldEqual, "patch files")
		sub := result[1].(map[string]interface{})["function"].(map[string]interface{})
		So(sub["name"], ShouldEqual, "apply_patch_add_file")
	})
}

func TestConvertToolsToOpenAI_UnmappableTypesRejected(t *testing.T) {
	Convey("convertToolsToOpenAI: file_search/code_interpreter/image_generation/mcp/programmatic_tool_calling/未知类型 → 丢弃且不报错", t, func() {
		for _, typ := range []string{
			"file_search", "code_interpreter", "image_generation",
			"mcp", "programmatic_tool_calling", "future_tool_type",
		} {
			out, err := convertToolsToOpenAI([]interface{}{map[string]interface{}{"type": typ}})
			So(err, ShouldBeNil)
			So(len(out), ShouldEqual, 0)
		}
	})
}

func TestConvertResponsesToChatRequest_TopLevelPassthrough(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: 顶层字段直通", t, func() {
		// previous_response_id / service_tier ultrafast 的丢弃路径见 TestConvertResponsesToChatRequestDropsUnmappableContext
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": "hi",
			"store": true,
			"metadata": {"k": "v"},
			"prompt_cache_key": "cache-1",
			"prompt_cache_options": {"ttl": "30m", "mode": "explicit"},
			"safety_identifier": "sid-1",
			"service_tier": "flex",
			"modalities": ["text", "audio"],
			"moderation": {"model": "omni-moderation-latest"},
			"text": {"verbosity": "high", "format": {"type": "text"}},
			"include": []
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		So(chatReq["store"], ShouldEqual, true)
		So(chatReq["metadata"], ShouldResemble, map[string]interface{}{"k": "v"})
		So(chatReq["prompt_cache_key"], ShouldEqual, "cache-1")
		So(chatReq["prompt_cache_options"], ShouldResemble, map[string]interface{}{"ttl": "30m", "mode": "explicit"})
		So(chatReq["safety_identifier"], ShouldEqual, "sid-1")
		So(chatReq["service_tier"], ShouldEqual, "flex")
		So(chatReq["modalities"], ShouldResemble, []interface{}{"text", "audio"})
		So(chatReq["moderation"], ShouldResemble, map[string]interface{}{"model": "omni-moderation-latest"})
		// text.verbosity 提升为 chat 顶层 verbosity，format 同时正常转 response_format
		So(chatReq["verbosity"], ShouldEqual, "high")
		So(chatReq["response_format"].(map[string]interface{})["type"], ShouldEqual, "text")
		// include 空数组为协议缺省形态 → 放行且不写入 chat 请求（非缺省拒绝路径见
		// TestConvertResponsesToChatRequestRejectsResponsesOnlyTopLevelFields）
		_, hasInclude := chatReq["include"]
		So(hasInclude, ShouldBeFalse)
	})

	Convey("nil 值不透传（缺省字段保持缺省）", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"input": "hi",
			"store": null,
			"metadata": null
		}`)

		chatReq := parseChatRequest(convReqRaw("gpt-test", reqBody, false))

		_, hasStore := chatReq["store"]
		So(hasStore, ShouldBeFalse)
		_, hasMeta := chatReq["metadata"]
		So(hasMeta, ShouldBeFalse)
	})
}

func TestConvertChatResponseToResponses_Annotations(t *testing.T) {
	Convey("ConvertChatResponseToResponses: message.annotations → output_text part.annotations（P0-3：annotations/logprobs 恒必含）", t, func() {
		annotations := []interface{}{
			map[string]interface{}{
				"type": "url_citation",
				"url_citation": map[string]interface{}{
					"start_index": float64(0),
					"end_index":   float64(5),
					"url":         "https://example.com",
					"title":       "Example",
				},
			},
		}

		Convey("content 为 string → output_text part 携带 annotations + logprobs 空数组", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_ann",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":        "assistant",
							"content":     "hello world",
							"annotations": annotations,
						},
						"finish_reason": "stop",
					},
				},
			}
			b, _ := json.Marshal(chatResp)

			output := parseOutputArray(convRespRaw(b, "gpt-test", false))

			So(len(output), ShouldEqual, 1)
			msg := output[0].(map[string]interface{})
			content := msg["content"].([]interface{})
			part := content[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "output_text")
			So(part["annotations"], ShouldResemble, annotations)
			So(part["logprobs"], ShouldResemble, []interface{}{})
		})

		Convey("content 为数组 → 每个文本 part 恒有 annotations/logprobs（有 message.annotations 时挂到文本 part）", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_ann2",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role": "assistant",
							"content": []interface{}{
								map[string]interface{}{"type": "text", "text": "first"},
								map[string]interface{}{"type": "output_text", "text": "second"},
							},
							"annotations": annotations,
						},
						"finish_reason": "stop",
					},
				},
			}
			b, _ := json.Marshal(chatResp)

			output := parseOutputArray(convRespRaw(b, "gpt-test", false))

			msg := output[0].(map[string]interface{})
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			first := content[0].(map[string]interface{})
			So(first["type"], ShouldEqual, "output_text")
			So(first["annotations"], ShouldResemble, annotations)
			second := content[1].(map[string]interface{})
			So(second["annotations"], ShouldResemble, annotations)
			So(second["logprobs"], ShouldResemble, []interface{}{})
		})

		Convey("无 annotations → part 仍必含空 annotations/logprobs（responses §5）", func() {
			chatResp := map[string]interface{}{
				"id":      "chat_ann3",
				"created": 1700000000,
				"model":   "gpt-test",
				"choices": []interface{}{
					map[string]interface{}{
						"index": 0,
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "plain",
						},
						"finish_reason": "stop",
					},
				},
			}
			b, _ := json.Marshal(chatResp)

			output := parseOutputArray(convRespRaw(b, "gpt-test", false))
			msg := output[0].(map[string]interface{})
			part := msg["content"].([]interface{})[0].(map[string]interface{})
			So(part["annotations"], ShouldResemble, []interface{}{})
			So(part["logprobs"], ShouldResemble, []interface{}{})
		})
	})
}

func TestConvertChatResponseToResponses_Audio(t *testing.T) {
	Convey("ConvertChatResponseToResponses: message.audio → output_audio item", t, func() {
		audio := map[string]interface{}{
			"id":         "a_1",
			"expires_at": float64(1700003600),
			"data":       "base64audio",
			"transcript": "spoken words",
		}

		chatResp := map[string]interface{}{
			"id":      "chat_audio",
			"created": 1700000000,
			"model":   "gpt-test",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "transcript text",
						"audio":   audio,
					},
					"finish_reason": "stop",
				},
			},
		}
		b, _ := json.Marshal(chatResp)

		output := parseOutputArray(convRespRaw(b, "gpt-test", false))

		So(len(output), ShouldEqual, 2)
		So(output[0].(map[string]interface{})["type"], ShouldEqual, "message")
		audioItem := output[1].(map[string]interface{})
		So(audioItem["type"], ShouldEqual, "output_audio")
		So(audioItem["id"], ShouldEqual, "a_1")
		So(audioItem["output_audio"], ShouldResemble, audio)
	})

	Convey("audio 缺 id → item 无 id 字段，output_audio 原样直通", t, func() {
		audio := map[string]interface{}{
			"data":       "base64audio",
			"transcript": "x",
		}
		chatResp := map[string]interface{}{
			"id":      "chat_audio2",
			"created": 1700000000,
			"model":   "gpt-test",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "t",
						"audio":   audio,
					},
					"finish_reason": "stop",
				},
			},
		}
		b, _ := json.Marshal(chatResp)

		output := parseOutputArray(convRespRaw(b, "gpt-test", false))

		So(len(output), ShouldEqual, 2)
		audioItem := output[1].(map[string]interface{})
		_, hasID := audioItem["id"]
		So(hasID, ShouldBeFalse)
		So(audioItem["output_audio"], ShouldResemble, audio)
	})
}

func TestParseUsage_InputTokensKeepsCached(t *testing.T) {
	Convey("parseUsage：input_tokens 保持总输入口径（含 cached），cached 仅经 details 表达（docs §6 与流式对齐）", t, func() {
		Convey("OpenAI 上游含 cached：input_tokens 不扣除，cached 落入 input_tokens_details", func() {
			usage := parseUsage(map[string]interface{}{
				"prompt_tokens":     float64(100),
				"completion_tokens": float64(50),
				"total_tokens":      float64(150),
				"prompt_tokens_details": map[string]interface{}{
					"cached_tokens": float64(60),
				},
			})
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["output_tokens"], ShouldEqual, float64(50))
			So(usage["total_tokens"], ShouldEqual, float64(150))
			details, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(details["cached_tokens"], ShouldEqual, float64(60))
		})

		Convey("chat prompt_tokens_details.cache_write_tokens → responses input_tokens_details.cache_write_tokens（chat §8.1 → responses §6）", func() {
			usage := parseUsage(map[string]interface{}{
				"prompt_tokens":     float64(100),
				"completion_tokens": float64(50),
				"total_tokens":      float64(150),
				"prompt_tokens_details": map[string]interface{}{
					"cached_tokens":      float64(60),
					"cache_write_tokens": float64(5),
				},
			})
			details, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(details["cached_tokens"], ShouldEqual, float64(60))
			So(details["cache_write_tokens"], ShouldEqual, float64(5))
		})

		Convey("completion_tokens_details 透传保留子字段，缺 reasoning_tokens 补 0（chat §8.2 → responses §6）", func() {
			usage := parseUsage(map[string]interface{}{
				"prompt_tokens":     float64(10),
				"completion_tokens": float64(8),
				"total_tokens":      float64(18),
				"completion_tokens_details": map[string]interface{}{
					"text_tokens": float64(8),
				},
			})
			outDetails, ok := usage["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails["text_tokens"], ShouldEqual, float64(8))
			So(outDetails["reasoning_tokens"], ShouldEqual, float64(0))

			usage2 := parseUsage(map[string]interface{}{
				"prompt_tokens":     float64(10),
				"completion_tokens": float64(8),
				"total_tokens":      float64(18),
				"completion_tokens_details": map[string]interface{}{
					"reasoning_tokens": float64(7),
				},
			})
			outDetails2, ok := usage2["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails2["reasoning_tokens"], ShouldEqual, float64(7))
		})

		Convey("OpenAI 上游无 cached：details 恒存在（responses §6 全部必填口径，值为 0 也输出子字段）", func() {
			usage := parseUsage(map[string]interface{}{
				"prompt_tokens":     float64(100),
				"completion_tokens": float64(50),
				"total_tokens":      float64(150),
			})
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["total_tokens"], ShouldEqual, float64(150))
			inDetails, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(inDetails["cached_tokens"], ShouldEqual, float64(0))
			So(inDetails["cache_write_tokens"], ShouldEqual, float64(0))
			outDetails, ok := usage["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails["reasoning_tokens"], ShouldEqual, float64(0))
		})

		Convey("Claude 上游：input_tokens 保持上游原值，cache_read 单独透传", func() {
			usage := parseUsage(map[string]interface{}{
				"input_tokens":            float64(100),
				"output_tokens":           float64(50),
				"total_tokens":            float64(210),
				"cache_read_input_tokens": float64(60),
			})
			So(usage["input_tokens"], ShouldEqual, float64(100))
			So(usage["output_tokens"], ShouldEqual, float64(50))
			So(usage["total_tokens"], ShouldEqual, float64(210))
			So(usage["cache_read_input_tokens"], ShouldEqual, float64(60))
		})

		Convey("usage 全 0（无任何 details 源）：details 恒存在且子字段补 0", func() {
			usage := parseUsage(map[string]interface{}{})
			inDetails, ok := usage["input_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(inDetails["cached_tokens"], ShouldEqual, float64(0))
			So(inDetails["cache_write_tokens"], ShouldEqual, float64(0))
			outDetails, ok := usage["output_tokens_details"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(outDetails["reasoning_tokens"], ShouldEqual, float64(0))
		})
	})
}

func TestConvertChatResponseToResponsesWithContext_BuiltinToolShape(t *testing.T) {
	Convey("非流式 chat→responses：builtin 工具输出与流式路径对齐（ts_/ws_ 前缀 + tool_search 对象 arguments / web_search 字符串 arguments + execution 仅 tool_search）", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "tool_search", "name": "tool_search"},
				{"type": "web_search", "name": "web_search"}
			]
		}`)
		chatBody := `{
			"id": "chat_builtin",
			"created": 1700000000,
			"model": "gpt-test",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"tool_calls": [
						{
							"id": "call_ts_b1",
							"type": "function",
							"function": {"name": "tool_search", "arguments": "{\"query\":\"codex\"}"}
						},
						{
							"id": "call_ws_b1",
							"type": "function",
							"function": {"name": "web_search", "arguments": "{\"query\":\"golang\"}"}
						}
					]
				},
				"finish_reason": "tool_calls"
			}]
		}`

		result := convRespCtxRaw([]byte(chatBody), "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 2)

		ts := output[0].(map[string]interface{})
		So(ts["type"], ShouldEqual, "tool_search_call")
		So(ts["id"], ShouldEqual, "ts_call_ts_b1")
		So(ts["call_id"], ShouldEqual, "call_ts_b1")
		So(ts["name"], ShouldEqual, "tool_search")
		So(ts["arguments"], ShouldResemble, map[string]interface{}{"query": "codex"})
		So(ts["execution"], ShouldEqual, "client")
		So(ts["status"], ShouldEqual, "completed")

		ws := output[1].(map[string]interface{})
		So(ws["type"], ShouldEqual, "web_search_call")
		So(ws["id"], ShouldEqual, "ws_call_ws_b1")
		So(ws["call_id"], ShouldEqual, "call_ws_b1")
		So(ws["name"], ShouldEqual, "web_search")
		So(ws["arguments"], ShouldEqual, `{"query":"golang"}`)
		So(ws["status"], ShouldEqual, "completed")
		_, hasExecution := ws["execution"]
		So(hasExecution, ShouldBeFalse)
	})
}

func TestConvertChatResponseToResponsesWithContext_ToolSearchCallArgumentsShape(t *testing.T) {
	Convey("非流式 HTTP chat→responses：tool_search_call 的 arguments 是内嵌 JSON 对象，非法 JSON 回落字符串", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "tool_search", "name": "tool_search"}
			]
		}`)

		Convey("arguments 为合法 JSON 对象 → 输出对象且 query 可取值", func() {
			chatBody := `{
				"id": "chat_ts_obj_ns",
				"created": 1700000000,
				"model": "gpt-test",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"tool_calls": [
							{
								"id": "call_ts_obj_ns",
								"type": "function",
								"function": {"name": "tool_search", "arguments": "{\"query\":\"x\"}"}
							}
						]
					},
					"finish_reason": "tool_calls"
				}]
			}`

			result := convRespCtxRaw([]byte(chatBody), "gpt-test", false, reqBody)
			output := parseOutputArray(result)
			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "tool_search_call")
			So(item["arguments"], ShouldResemble, map[string]interface{}{"query": "x"})
			// 外层对象必须仍是合法 JSON（内嵌而非破坏）
			So(gjson.Valid(string(result)), ShouldBeTrue)
		})

		Convey("arguments 为非 JSON → 回落字符串且外层 JSON 仍可解析", func() {
			chatBody := `{
				"id": "chat_ts_raw_ns",
				"created": 1700000000,
				"model": "gpt-test",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"tool_calls": [
							{
								"id": "call_ts_raw_ns",
								"type": "function",
								"function": {"name": "tool_search", "arguments": "not json"}
							}
						]
					},
					"finish_reason": "tool_calls"
				}]
			}`

			result := convRespCtxRaw([]byte(chatBody), "gpt-test", false, reqBody)
			output := parseOutputArray(result)
			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "tool_search_call")
			So(item["arguments"], ShouldEqual, "not json")
			So(gjson.Valid(string(result)), ShouldBeTrue)
		})

		Convey("arguments 为 null → 回落字符串 \"null\" 而非 JSON null", func() {
			chatBody := `{
				"id": "chat_ts_null_ns",
				"created": 1700000000,
				"model": "gpt-test",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"tool_calls": [
							{
								"id": "call_ts_null_ns",
								"type": "function",
								"function": {"name": "tool_search", "arguments": "null"}
							}
						]
					},
					"finish_reason": "tool_calls"
				}]
			}`

			result := convRespCtxRaw([]byte(chatBody), "gpt-test", false, reqBody)
			output := parseOutputArray(result)
			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "tool_search_call")
			So(item["arguments"], ShouldEqual, "null")
			So(gjson.Valid(string(result)), ShouldBeTrue)
		})

		Convey("arguments 为截断对象前缀 → 回落字符串且外层 JSON 仍可解析", func() {
			chatBody := `{
				"id": "chat_ts_trunc_ns",
				"created": 1700000000,
				"model": "gpt-test",
				"choices": [{
					"index": 0,
					"message": {
						"role": "assistant",
						"tool_calls": [
							{
								"id": "call_ts_trunc_ns",
								"type": "function",
								"function": {"name": "tool_search", "arguments": "{\"query\":\"x"}
							}
						]
					},
					"finish_reason": "tool_calls"
				}]
			}`

			result := convRespCtxRaw([]byte(chatBody), "gpt-test", false, reqBody)
			output := parseOutputArray(result)
			So(len(output), ShouldEqual, 1)
			item := output[0].(map[string]interface{})
			So(item["type"], ShouldEqual, "tool_search_call")
			So(item["arguments"], ShouldEqual, `{"query":"x`)
			So(gjson.Valid(string(result)), ShouldBeTrue)
		})
	})
}

func TestConvertInputToMessages_BuiltinFallbackCallIDPairing(t *testing.T) {
	conveyCallID := func(msgs []interface{}, i int) string {
		m := msgs[i].(map[string]interface{})
		return m["tool_calls"].([]interface{})[0].(map[string]interface{})["id"].(string)
	}
	conveyOutID := func(msgs []interface{}, i int) string {
		return msgs[i].(map[string]interface{})["tool_call_id"].(string)
	}

	Convey("builtin call/output 均缺 call_id → 确定性配对：连续两组同类两两配对且互不串对", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call", "arguments": `{"query":"q1"}`},
			map[string]interface{}{"type": "tool_search_call_output", "output": "o1"},
			map[string]interface{}{"type": "web_search_call", "arguments": `{"query":"q2"}`},
			map[string]interface{}{"type": "web_search_call_output", "output": "o2"},
			map[string]interface{}{"type": "tool_search_call", "arguments": `{"query":"q3"}`},
			map[string]interface{}{"type": "tool_search_call_output", "output": "o3"},
			map[string]interface{}{"type": "web_search_call", "arguments": `{"query":"q4"}`},
			map[string]interface{}{"type": "web_search_call_output", "output": "o4"},
		}
		msgs, err := convertInputToMessages(input)
		So(err, ShouldBeNil)
		// call 与 output 交替，无相邻 call，可全部保持独立消息
		So(len(msgs), ShouldEqual, 8)

		So(conveyOutID(msgs, 1), ShouldEqual, conveyCallID(msgs, 0))
		So(conveyOutID(msgs, 3), ShouldEqual, conveyCallID(msgs, 2))
		So(conveyOutID(msgs, 5), ShouldEqual, conveyCallID(msgs, 4))
		So(conveyOutID(msgs, 7), ShouldEqual, conveyCallID(msgs, 6))
		So(conveyCallID(msgs, 4), ShouldNotEqual, conveyCallID(msgs, 0))
		So(conveyCallID(msgs, 6), ShouldNotEqual, conveyCallID(msgs, 2))
		So(strings.HasPrefix(conveyCallID(msgs, 0), "ts_"), ShouldBeTrue)
		So(strings.HasPrefix(conveyCallID(msgs, 2), "ws_"), ShouldBeTrue)
		So(strings.HasPrefix(conveyOutID(msgs, 1), "ts_"), ShouldBeTrue)
	})

	Convey("连续两个 call 后跟两个 output → calls 合并进同一 assistant.tool_calls[]（P1-5），output 仍与最近未配对 call LIFO", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "tool_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "a"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "b"},
		}
		msgs, err := convertInputToMessages(input)
		So(err, ShouldBeNil)
		So(len(msgs), ShouldEqual, 3)
		tcs := msgs[0].(map[string]interface{})["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 2)
		id0 := tcs[0].(map[string]interface{})["id"].(string)
		id1 := tcs[1].(map[string]interface{})["id"].(string)
		So(id0, ShouldEqual, "ts_fb1")
		So(id1, ShouldEqual, "ts_fb2")
		So(msgs[1].(map[string]interface{})["tool_call_id"], ShouldEqual, id1)
		So(msgs[2].(map[string]interface{})["tool_call_id"], ShouldEqual, id0)
	})

	Convey("孤立 output（无先行 call）→ 丢弃，不产出悬空 role:tool（P2-2）", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call_output", "output": "a"},
			map[string]interface{}{"type": "web_search_call_output", "output": "b"},
		}
		msgs, err := convertInputToMessages(input)
		So(err, ShouldBeNil)
		So(len(msgs), ShouldEqual, 0)
	})

	Convey("两次独立转换结果逐字节一致 → 状态仅存活于单次转换，无跨请求残留", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "a"},
			map[string]interface{}{"type": "web_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "web_search_call_output", "output": "b"},
		}
		msgsA, errA := convertInputToMessages(input)
		msgsB, errB := convertInputToMessages(input)
		So(errA, ShouldBeNil)
		So(errB, ShouldBeNil)
		So(msgsA, ShouldResemble, msgsB)
	})
}

func TestConvertResponsesToChatRequest_BuiltinFallbackPairingEndToEnd(t *testing.T) {
	Convey("端到端：缺 call_id 的 builtin 历史 → chat 请求内 tool_calls.id 与 tool_call_id 配对且转换确定", t, func() {
		body, _ := json.Marshal(map[string]interface{}{
			"model": "gpt-test",
			"input": []interface{}{
				map[string]interface{}{"type": "tool_search_call", "arguments": `{"query":"x"}`},
				map[string]interface{}{"type": "tool_search_call_output", "output": "res"},
			},
		})
		out1 := convReqRaw("gpt-test", body, false)
		out2 := convReqRaw("gpt-test", body, false)
		So(string(out1), ShouldEqual, string(out2))

		chatReq := parseChatRequest(out1)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 2)
		callID := messages[0].(map[string]interface{})["tool_calls"].([]interface{})[0].(map[string]interface{})["id"]
		toolCallID := messages[1].(map[string]interface{})["tool_call_id"]
		So(toolCallID, ShouldEqual, callID)
	})
}

func TestConvertContentArray_UnknownBlockType(t *testing.T) {
	Convey("未知 content block type / role 矩阵违规 → 丢弃违规 part 并放行，保留可保留文本", t, func() {
		content := []interface{}{
			map[string]interface{}{"type": "input_text", "text": "hello"},
			map[string]interface{}{"type": "input_video", "video_url": "https://example.com/v.mp4"},
		}
		out, err := convertContentArray(content, "user")
		So(err, ShouldBeNil)
		So(out, ShouldEqual, "hello")

		// assistant 携带 image part 违反 chat §4 矩阵 → 丢弃，无剩余内容
		content3 := []interface{}{map[string]interface{}{"type": "input_image", "image_url": "http://img"}}
		out3, err3 := convertContentArray(content3, "assistant")
		So(err3, ShouldBeNil)
		So(out3, ShouldEqual, "")

		// 正例：system/developer 允许纯 text part 数组（chat §4 矩阵正例）
		content5 := []interface{}{map[string]interface{}{"type": "input_text", "text": "rules"}}
		out5, err5 := convertContentArray(content5, "system")
		So(err5, ShouldBeNil)
		So(out5, ShouldEqual, "rules")
		out6, err6 := convertContentArray(content5, "developer")
		So(err6, ShouldBeNil)
		So(out6, ShouldEqual, "rules")

		// 正例：user 合法多模态组合完整保留
		content4 := []interface{}{
			map[string]interface{}{"type": "input_text", "text": "look"},
			map[string]interface{}{"type": "input_image", "image_url": "http://img"},
		}
		out4, err4 := convertContentArray(content4, "user")
		So(err4, ShouldBeNil)
		parts, ok := out4.([]interface{})
		So(ok, ShouldBeTrue)
		So(len(parts), ShouldEqual, 2)
		So(parts[1].(map[string]interface{})["type"], ShouldEqual, "image_url")
	})
}

func TestConvertChatMessageToOutput_CustomToolCallVariant(t *testing.T) {
	Convey("chat §6.1.1 type:custom tool call（custom:{name,input}）→ responses custom_tool_call 无损映射", t, func() {
		message := map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":     "call_cx1",
					"type":   "custom",
					"custom": map[string]interface{}{"name": "apply_patch", "input": "*** Begin Patch"},
				},
			},
		}
		output, err := convertChatMessageToOutput(message, nil, "resp_test")
		So(err, ShouldBeNil)
		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["id"], ShouldEqual, "ctc_call_cx1")
		So(item["call_id"], ShouldEqual, "call_cx1")
		So(item["name"], ShouldEqual, "apply_patch")
		So(item["input"], ShouldEqual, "*** Begin Patch")
		So(item["status"], ShouldEqual, "completed")
	})

	Convey("P0-3: 畸形 custom（缺 name/input）与无 function/custom 载荷 → malformed_tool_call（不再静默丢弃）", t, func() {
		badCases := []interface{}{
			map[string]interface{}{"id": "call_bad1", "type": "custom", "custom": map[string]interface{}{"name": "only_name"}},
			map[string]interface{}{"id": "call_bad2", "type": "custom"},
			map[string]interface{}{"id": "call_bad3", "type": "mystery"},
		}
		for _, bad := range badCases {
			message := map[string]interface{}{
				"role":       "assistant",
				"tool_calls": []interface{}{bad},
			}
			out, err := convertChatMessageToOutput(message, nil, "resp_test")
			So(out, ShouldBeNil)
			soPCE(err, relaymodel.CodeMalformedToolCall)
		}
	})
}

// =============================================================================
// T6 契约测试（fix-plan-protocol-20260909.md Task T6 Test Contract）
// =============================================================================

// chatReqToolCall / chatReqMessage / chatReqWire 是转换产物（Chat 请求）的严格 wire 形状，
// 配合 strictDecode（DisallowUnknownFields）锁定输出不含协议外字段（如已弃用 max_tokens）。
type chatReqToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatReqMessage struct {
	Role             string            `json:"role"`
	Content          interface{}       `json:"content"`
	ReasoningContent interface{}       `json:"reasoning_content"`
	ToolCalls        []chatReqToolCall `json:"tool_calls"`
	ToolCallID       string            `json:"tool_call_id"`
}

type chatReqWire struct {
	Model               string           `json:"model"`
	Messages            []chatReqMessage `json:"messages"`
	Stream              bool             `json:"stream"`
	MaxCompletionTokens int              `json:"max_completion_tokens"`
	MaxTokens           *int             `json:"max_tokens"`
	Temperature         *float64         `json:"temperature"`
	TopP                *float64         `json:"top_p"`
	User                string           `json:"user"`
	Tools               []interface{}    `json:"tools"`
	ToolChoice          interface{}      `json:"tool_choice"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls"`
	ReasoningEffort     string           `json:"reasoning_effort"`
	ResponseFormat      interface{}      `json:"response_format"`
	Verbosity           string           `json:"verbosity"`
	Store               interface{}      `json:"store"`
	Metadata            interface{}      `json:"metadata"`
	Modalities          interface{}      `json:"modalities"`
	ServiceTier         string           `json:"service_tier"`
	Moderation          interface{}      `json:"moderation"`
	PromptCacheKey      string           `json:"prompt_cache_key"`
	PromptCacheOptions  interface{}      `json:"prompt_cache_options"`
	SafetyIdentifier    string           `json:"safety_identifier"`
	StreamOptions       interface{}      `json:"stream_options"`
}

func TestConvertResponsesToChatRequestCanonicalHistory(t *testing.T) {
	Convey("G: 合法 instructions/developer/user multimodal/连续 function/custom call+output 请求 | W: ConvertResponsesToChatRequest | T: role 矩阵合法、并行 calls 合并、custom arguments 匹配 schema、max_completion_tokens", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"instructions": "be terse",
			"max_output_tokens": 4096,
			"temperature": 0.3,
			"input": [
				{"type": "message", "role": "developer", "content": "coding rules"},
				{"type": "message", "role": "user", "content": [
					{"type": "input_text", "text": "look"},
					{"type": "input_image", "image_url": {"url": "http://x/i.png"}}
				]},
				{"type": "message", "role": "assistant", "content": "sure"},
				{"type": "function_call", "call_id": "fc1", "name": "get_weather", "arguments": "{\"city\":\"SF\"}"},
				{"type": "function_call", "call_id": "fc2", "name": "lookup", "arguments": "{\"q\":\"x\"}"},
				{"type": "function_call_output", "call_id": "fc1", "output": "sunny"},
				{"type": "function_call_output", "call_id": "fc2", "output": "ok"},
				{"type": "custom_tool_call", "call_id": "ct1", "name": "my_grammar", "input": "raw text"},
				{"type": "custom_tool_call_output", "call_id": "ct1", "output": "applied"}
			],
			"tools": [
				{"type": "function", "name": "get_weather", "description": "weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}},
				{"type": "custom", "name": "my_grammar", "description": "grammar"},
				{"type": "apply_patch", "description": "patch files"}
			],
			"parallel_tool_calls": true
		}`)

		out, err := ConvertResponsesToChatRequest("gpt-test", reqBody, false)
		So(err, ShouldBeNil)

		var wire chatReqWire
		So(strictDecode(string(out), &wire), ShouldBeNil)

		// P2-1：max_output_tokens → max_completion_tokens，不写已弃用 max_tokens
		So(wire.MaxCompletionTokens, ShouldEqual, 4096)
		So(wire.MaxTokens, ShouldBeNil)
		So(wire.Stream, ShouldBeFalse)
		So(wire.Model, ShouldEqual, "gpt-test")
		So(*wire.ParallelToolCalls, ShouldBeTrue)

		// role 矩阵：instructions→system；developer 保持 developer（§1.2 保护项）
		So(len(wire.Messages), ShouldEqual, 9)
		So(wire.Messages[0].Role, ShouldEqual, "system")
		So(wire.Messages[0].Content, ShouldEqual, "be terse")
		So(wire.Messages[1].Role, ShouldEqual, "developer")
		So(wire.Messages[1].Content, ShouldEqual, "coding rules")
		// user 多模态 → chat content parts 数组（text + image_url）
		userParts, ok := wire.Messages[2].Content.([]interface{})
		So(ok, ShouldBeTrue)
		So(len(userParts), ShouldEqual, 2)
		So(userParts[0].(map[string]interface{})["type"], ShouldEqual, "text")
		So(userParts[1].(map[string]interface{})["type"], ShouldEqual, "image_url")
		// assistant 文本
		So(wire.Messages[3].Role, ShouldEqual, "assistant")
		So(wire.Messages[3].Content, ShouldEqual, "sure")

		// P1-5：连续 function_call 合并为同一 assistant message 的 tool_calls[]（顺序与 call_id 保持）
		So(wire.Messages[4].Role, ShouldEqual, "assistant")
		So(len(wire.Messages[4].ToolCalls), ShouldEqual, 2)
		So(wire.Messages[4].ToolCalls[0].ID, ShouldEqual, "fc1")
		So(wire.Messages[4].ToolCalls[0].Function.Name, ShouldEqual, "get_weather")
		So(wire.Messages[4].ToolCalls[0].Function.Arguments, ShouldEqual, "{\"city\":\"SF\"}")
		So(wire.Messages[4].ToolCalls[1].ID, ShouldEqual, "fc2")
		// tool 输出边界后不再合并
		So(wire.Messages[5].Role, ShouldEqual, "tool")
		So(wire.Messages[5].ToolCallID, ShouldEqual, "fc1")
		So(wire.Messages[6].Role, ShouldEqual, "tool")
		So(wire.Messages[6].ToolCallID, ShouldEqual, "fc2")
		// custom call 单独成条（前一条被 tool 边界隔开）
		So(len(wire.Messages[7].ToolCalls), ShouldEqual, 1)
		customArgs := wire.Messages[7].ToolCalls[0].Function.Arguments
		So(wire.Messages[8].Role, ShouldEqual, "tool")
		So(wire.Messages[8].ToolCallID, ShouldEqual, "ct1")

		// P0-2：custom 历史 arguments 与声明的 {input:string} schema 可用同一校验通过
		toolsByName := map[string]map[string]interface{}{}
		for _, raw := range wire.Tools {
			tm := raw.(map[string]interface{})
			fn := tm["function"].(map[string]interface{})
			toolsByName[fn["name"].(string)] = fn
		}
		grammarFn, has := toolsByName["my_grammar"]
		So(has, ShouldBeTrue)
		grammarParams := grammarFn["parameters"].(map[string]interface{})
		So(grammarParams["type"], ShouldEqual, "object")
		So(grammarParams["properties"].(map[string]interface{})["input"].(map[string]interface{})["type"], ShouldEqual, "string")
		So(grammarParams["required"], ShouldResemble, []interface{}{"input"})
		assertCustomArgumentsMatchInputSchema(customArgs, "raw text")
		// apply_patch 类（DN-6 白名单）：主工具 + 5 代理子工具
		for _, name := range []string{"apply_patch", "apply_patch_add_file", "apply_patch_delete_file", "apply_patch_update_file", "apply_patch_replace_file", "apply_patch_batch"} {
			_, has := toolsByName[name]
			So(has, ShouldBeTrue)
		}
	})
}

func TestConvertResponsesToChatRequestDropsUnmappableContext(t *testing.T) {
	Convey("能力性不可映射上下文：字段/item/工具丢弃后仍完成转换；仅空 messages 维持 400", t, func() {

		Convey("previous_response_id 非空 → 丢弃该字段，转换成功且输出不含该字段", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":"hi","previous_response_id":"resp_prev"}`), false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
			_, has := parseChatRequest(out)["previous_response_id"]
			So(has, ShouldBeFalse)
		})

		Convey("service_tier ultrafast（DN-7）→ 丢弃该字段，转换成功且输出不含 service_tier", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":"hi","service_tier":"ultrafast"}`), false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
			_, has := parseChatRequest(out)["service_tier"]
			So(has, ShouldBeFalse)
		})

		Convey("DN-6 白名单外 builtin 工具 → 丢弃工具，转换成功且输出无 tools", func() {
			for _, body := range []string{
				`{"model":"gpt-test","input":"hi","tools":[{"type":"web_search"}]}`,
				`{"model":"gpt-test","input":"hi","tools":[{"type":"computer_use","display_width":1024}]}`,
				`{"model":"gpt-test","input":"hi","tools":[{"type":"shell"}]}`,
				`{"model":"gpt-test","input":"hi","tools":[{"type":"local_shell"}]}`,
				`{"model":"gpt-test","input":"hi","tools":[{"type":"code_interpreter"}]}`,
				`{"model":"gpt-test","input":"hi","tools":[{"type":"file_search","vector_store_ids":["vs_1"]}]}`,
			} {
				out, err := ConvertResponsesToChatRequest("gpt-test", []byte(body), false)
				So(err, ShouldBeNil)
				So(out, ShouldNotBeNil)
				_, has := parseChatRequest(out)["tools"]
				So(has, ShouldBeFalse)
			}
		})

		Convey("item_reference / shell_call / computer_call 历史 item 丢弃；无其余上下文时 messages 为空仍 400", func() {
			emptyCases := []string{
				`{"model":"gpt-test","input":{"type":"item_reference","id":"itm_1"}}`,
				`{"model":"gpt-test","input":[{"type":"item_reference","id":"itm_2"}]}`,
				`{"model":"gpt-test","input":[{"type":"shell_call","call_id":"sh1","action":{"commands":["ls"]}}]}`,
				`{"model":"gpt-test","input":[{"type":"computer_call","call_id":"cc1","action":{"type":"click"},"pending_safety_checks":[]}]}`,
			}
			for _, body := range emptyCases {
				out, err := ConvertResponsesToChatRequest("gpt-test", []byte(body), false)
				// 不是能力性拒绝，而是转换后无任何可发送消息（空 messages 无法执行）
				So(out, ShouldBeNil)
				soPCE(err, relaymodel.CodeUnsupportedMapping)
			}
		})

		Convey("item_reference 与合法消息混排 → 仅丢弃引用 item，转换成功且合法消息保留", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":[{"type":"message","role":"user","content":"hi"},{"type":"item_reference","id":"itm_2"}]}`), false)
			So(err, ShouldBeNil)
			messages := parseChatRequest(out)["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["content"], ShouldEqual, "hi")
		})

		Convey("DN-7 其余交集枚举原样透传", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":"hi","service_tier":"priority"}`), false)
			So(err, ShouldBeNil)
			So(parseChatRequest(out)["service_tier"], ShouldEqual, "priority")
		})

		Convey("invalid_source_json：请求体非合法 JSON", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model": `), false)
			So(out, ShouldBeNil)
			soPCE(err, relaymodel.CodeInvalidSourceJSON)
		})
	})

	Convey("降级保留路径（DN-5）：reasoning content block 数组进 assistant.reasoning_content；phase 丢标记留文本", t, func() {
		Convey("reasoning content block 数组读出文本进 reasoning_content（修复 content 数组读不出即整项丢弃 bug）", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "message", "role": "user", "content": "continue"},
					{"type": "reasoning", "summary": [], "content": [
						{"type": "reasoning_text", "text": "step one"},
						{"type": "reasoning_text", "text": "step two"}
					]}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", reqBody, false)
			So(err, ShouldBeNil)
			messages := parseChatRequest(out)["messages"].([]interface{})
			So(len(messages), ShouldEqual, 2)
			reasoningMsg := messages[1].(map[string]interface{})
			So(reasoningMsg["role"], ShouldEqual, "assistant")
			So(reasoningMsg["reasoning_content"], ShouldEqual, "step one\nstep two")
			_, hasContent := reasoningMsg["content"]
			So(hasContent, ShouldBeFalse)
		})

		Convey("summary 与 content 同时存在 → 两者完整读取按序拼接", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "reasoning",
					 "summary": [{"type":"summary_text","text":"sum line"}],
					 "content": [{"type":"reasoning_text","text":"body line"}]}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", reqBody, false)
			So(err, ShouldBeNil)
			messages := parseChatRequest(out)["messages"].([]interface{})
			So(messages[0].(map[string]interface{})["reasoning_content"], ShouldEqual, "sum line\nbody line")
		})

		Convey("assistant message phase → 丢标记保留文本，不拒绝请求", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "message", "role": "assistant", "content": [{"type":"output_text","text":"interim note"}], "phase": "commentary"}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", reqBody, false)
			So(err, ShouldBeNil)
			messages := parseChatRequest(out)["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "interim note")
			_, hasPhase := msg["phase"]
			So(hasPhase, ShouldBeFalse)
		})

		Convey("agent_message phase → 同样丢标记留文本", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_9", "phase": "finalAnswer", "content": [{"type":"text","text":"final answer text"}]}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", reqBody, false)
			So(err, ShouldBeNil)
			messages := parseChatRequest(out)["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			So(msg["content"], ShouldEqual, "final answer text")
			_, hasPhase := msg["phase"]
			So(hasPhase, ShouldBeFalse)
		})
	})
}

func TestConvertChatResponseToResponsesProducesCompleteResponseAndItems(t *testing.T) {
	Convey("G: 合法 Chat response 含 text/refusal/reasoning/tool 和 usage | W: converter | T: Response object、message/reasoning required fields、output_text arrays 全完整且 SDK-shape 可解码", t, func() {
		originalReq := []byte(`{
			"model": "gpt-test",
			"instructions": "be terse",
			"temperature": 0.5,
			"store": false,
			"modalities": ["text"],
			"text": {"format": {"type": "text"}},
			"user": "u-1",
			"service_tier": "default",
			"max_output_tokens": 512,
			"parallel_tool_calls": true,
			"tool_choice": "auto",
			"reasoning": {"effort": "low"},
			"tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
		}`)
		chatResp := `{
			"id": "chatcmpl_t6",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-test-x",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": "hello",
					"refusal": "not that",
					"reasoning_content": "deep thought",
					"tool_calls": [
						{"id": "call_t6", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}
					]
				},
				"finish_reason": "tool_calls",
				"logprobs": null
			}],
			"usage": {
				"prompt_tokens": 11,
				"completion_tokens": 7,
				"total_tokens": 18,
				"prompt_tokens_details": {"cached_tokens": 3, "cache_write_tokens": 2},
				"completion_tokens_details": {"reasoning_tokens": 4}
			}
		}`

		out, err := ConvertChatResponseToResponsesWithContext([]byte(chatResp), "gpt-test", false, originalReq)
		So(err, ShouldBeNil)

		// 顶层对象必须能按 Responses §4 wire model 严格解码（无协议外键），object 恒为 response
		var wire relaymodel.ResponsesResponse
		So(strictDecode(string(out), &wire), ShouldBeNil)
		So(wire.Object, ShouldEqual, "response")
		So(wire.ID, ShouldEqual, "chatcmpl_t6")
		So(wire.Created, ShouldEqual, int64(1700000000))
		So(wire.Status, ShouldEqual, "completed")
		So(wire.Error, ShouldBeNil)
		So(wire.IncompleteDetails, ShouldBeNil)
		// model 优先使用调用方传入模型（既有口径）
		So(wire.Model, ShouldEqual, "gpt-test")
		// 回显原请求字段
		So(wire.Instructions, ShouldEqual, "be terse")
		So(wire.ServiceTier, ShouldEqual, "default")
		So(wire.User, ShouldEqual, "u-1")
		So(wire.MaxOutputTokens != nil && *wire.MaxOutputTokens == 512, ShouldBeTrue)
		So(wire.ParallelToolCalls, ShouldBeTrue)
		So(wire.PreviousID == nil, ShouldBeTrue)

		// usage details 必填恒存在（§6，§1.2 已确认映射）
		So(wire.Usage.InputTokens, ShouldEqual, 11)
		So(wire.Usage.OutputTokens, ShouldEqual, 7)
		So(wire.Usage.TotalTokens, ShouldEqual, 18)
		So(wire.Usage.InputTokensDetails != nil && wire.Usage.InputTokensDetails.CachedTokens == 3, ShouldBeTrue)
		So(wire.Usage.InputTokensDetails.CacheWriteTokens == 2, ShouldBeTrue)
		So(wire.Usage.OutputTokensDetails != nil && wire.Usage.OutputTokensDetails.ReasoningTokens == 4, ShouldBeTrue)

		var top map[string]interface{}
		So(json.Unmarshal(out, &top), ShouldBeNil)
		_, hasErr := top["error"]
		So(hasErr, ShouldBeTrue)
		_, hasIncomplete := top["incomplete_details"]
		So(hasIncomplete, ShouldBeTrue)

		output := top["output"].([]interface{})
		// 顺序：reasoning → message → function_call（既有顺序保护）
		So(len(output), ShouldEqual, 3)
		reasoning := output[0].(map[string]interface{})
		So(reasoning["type"], ShouldEqual, "reasoning")
		So(reasoning["id"], ShouldEqual, "rs_chatcmpl_t6_0")
		So(reasoning["status"], ShouldEqual, "completed")
		summary := reasoning["summary"].([]interface{})
		So(summary[0].(map[string]interface{})["type"], ShouldEqual, "summary_text")
		So(summary[0].(map[string]interface{})["text"], ShouldEqual, "deep thought")

		msg := output[1].(map[string]interface{})
		So(msg["type"], ShouldEqual, "message")
		So(msg["id"], ShouldEqual, "msg_chatcmpl_t6_1")
		So(msg["role"], ShouldEqual, "assistant")
		So(msg["status"], ShouldEqual, "completed")
		parts := msg["content"].([]interface{})
		So(len(parts), ShouldEqual, 2)
		textPart := parts[0].(map[string]interface{})
		So(textPart["type"], ShouldEqual, "output_text")
		So(textPart["text"], ShouldEqual, "hello")
		// P0-3：output_text 必含 annotations 与 logprobs 数组
		So(textPart["annotations"], ShouldResemble, []interface{}{})
		So(textPart["logprobs"], ShouldResemble, []interface{}{})
		refusalPart := parts[1].(map[string]interface{})
		So(refusalPart["type"], ShouldEqual, "refusal")
		So(refusalPart["refusal"], ShouldEqual, "not that")

		fc := output[2].(map[string]interface{})
		So(fc["type"], ShouldEqual, "function_call")
		So(fc["id"], ShouldEqual, "fc_call_t6")
		So(fc["call_id"], ShouldEqual, "call_t6")
		So(fc["name"], ShouldEqual, "get_weather")
		So(fc["arguments"], ShouldEqual, "{\"city\":\"SF\"}")
		So(fc["status"], ShouldEqual, "completed")
	})
}

func TestConvertChatResponseToResponsesRejectsMalformedJSONAndToolCall(t *testing.T) {
	Convey("G: 畸形 JSON 或缺 id/name 的 tool call | W: converter | T: 返回 error，不返回源载荷或畸形 output_item", t, func() {
		Convey("非法 JSON → invalid_source_json，不原样返回源载荷", func() {
			out, err := ConvertChatResponseToResponses([]byte(`{"id": `), "gpt-test", false)
			So(out, ShouldBeNil)
			soPCE(err, relaymodel.CodeInvalidSourceJSON)
		})

		Convey("顶层 JSON 非 object（数组）→ invalid_source_json", func() {
			out, err := ConvertChatResponseToResponses([]byte(`["chat"]`), "gpt-test", false)
			So(out, ShouldBeNil)
			soPCE(err, relaymodel.CodeInvalidSourceJSON)
		})

		malformed := []struct {
			name string
			call map[string]interface{}
		}{
			{"missing id", map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "f", "arguments": "{}"}}},
			{"missing name", map[string]interface{}{"id": "c1", "type": "function", "function": map[string]interface{}{"arguments": "{}"}}},
			{"missing arguments", map[string]interface{}{"id": "c1", "type": "function", "function": map[string]interface{}{"name": "f"}}},
			{"empty-string arguments", map[string]interface{}{"id": "c1", "type": "function", "function": map[string]interface{}{"name": "f", "arguments": ""}}},
			{"custom missing input", map[string]interface{}{"id": "c1", "type": "custom", "custom": map[string]interface{}{"name": "n"}}},
			{"no function/custom payload", map[string]interface{}{"id": "c1", "type": "mystery"}},
		}
		for _, m := range malformed {
			Convey("tool call "+m.name+" → malformed_tool_call", func() {
				chatResp := map[string]interface{}{
					"id":      "chat_bad",
					"created": 1700000000,
					"model":   "gpt-test",
					"choices": []interface{}{
						map[string]interface{}{
							"index": 0,
							"message": map[string]interface{}{
								"role":       "assistant",
								"tool_calls": []interface{}{m.call},
							},
							"finish_reason": "tool_calls",
						},
					},
				}
				body, _ := json.Marshal(chatResp)
				out, err := ConvertChatResponseToResponses(body, "gpt-test", false)
				// 不得返回源载荷，也不得产出畸形 output_item
				So(out, ShouldBeNil)
				soPCE(err, relaymodel.CodeMalformedToolCall)
			})
		}
	})
}

// TestConvertResponsesToChatRequestDropsResponsesOnlyTopLevelFields 锁定能力性丢弃策略：
// responses §2 合法专有顶层字段（include/background/max_tool_calls/prompt/truncation/
// conversation/context_management）在本网关 Chat 上游无表达 —— 非缺省出现时丢弃该字段并记
// warn 日志，请求继续完成转换（不再 unsupported_mapping 400）。
func TestConvertResponsesToChatRequestDropsResponsesOnlyTopLevelFields(t *testing.T) {
	Convey("7 个 Responses 专有顶层字段非缺省 → 逐字段丢弃且转换成功，输出不含该字段", t, func() {
		dropCases := []struct {
			name  string
			key   string
			value string
		}{
			{"include 返回内容策略", "include", `["file_search_call.results"]`},
			{"background true（启用后台执行）", "background", "true"},
			{"background object 配置", "background", `{"mode":"async"}`},
			{"max_tool_calls 正数", "max_tool_calls", "5"},
			{"max_tool_calls 0（显式配置意图）", "max_tool_calls", "0"},
			{"prompt", "prompt", `"sys prompt"`},
			{"truncation disabled（偏离默认 auto）", "truncation", `"disabled"`},
			{"truncation object 配置", "truncation", `{"max_tokens":100}`},
			{"conversation 会话上下文组装", "conversation", `{"id":"conv_1"}`},
			{"context_management", "context_management", `{"strategy":"compact"}`},
		}
		for _, tc := range dropCases {
			Convey(tc.name, func() {
				body := `{"model":"gpt-test","input":"hi","` + tc.key + `":` + tc.value + `}`
				out, err := ConvertResponsesToChatRequest("gpt-test", []byte(body), false)
				So(err, ShouldBeNil)
				So(out, ShouldNotBeNil)
				chatReq := parseChatRequest(out)
				_, has := chatReq[tc.key]
				So(has, ShouldBeFalse)
				// 其余请求内容不受影响
				So(chatReq["model"], ShouldEqual, "gpt-test")
			})
		}
	})

	Convey("缺省/协议默认形态放行（不误杀未携带字段）", t, func() {
		Convey("完全未携带 7 字段", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":"hi"}`), false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
		})

		Convey("显式协议默认值与 null：truncation=auto（§2 默认截断策略）、background=false（默认前台执行）、空 include/prompt、其余 null", func() {
			out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":"hi",
				"truncation":"auto","background":false,"include":[],"prompt":"",
				"conversation":null,"context_management":null,"max_tool_calls":null}`), false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
		})
	})
}

// TestConvertResponsesToChatRequest_CodexIncludeEncryptedContent 锁定线上事故回归：
// codex CLI 每个 /v1/responses 请求恒带 include=["reasoning.encrypted_content"]，路由到 chat 型
// 上游时必须丢弃 include 并成功产出合法 chat 请求，不得 400。
func TestConvertResponsesToChatRequest_CodexIncludeEncryptedContent(t *testing.T) {
	Convey("真实 codex CLI 形态：include=[reasoning.encrypted_content] → 丢弃 include，转换成功且产出合法 chat 请求", t, func() {
		body := []byte(`{
			"model": "gpt-test",
			"stream": true,
			"include": ["reasoning.encrypted_content"],
			"input": [
				{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]}
			],
			"tools": [{"type": "function", "name": "shell", "description": "run shell", "parameters": {"type": "object", "properties": {}}}],
			"tool_choice": "auto"
		}`)
		out, err := ConvertResponsesToChatRequest("gpt-test", body, true)
		So(err, ShouldBeNil)
		So(out, ShouldNotBeNil)

		chatReq := parseChatRequest(out)
		_, hasInclude := chatReq["include"]
		So(hasInclude, ShouldBeFalse)
		So(chatReq["model"], ShouldEqual, "gpt-test")
		So(chatReq["stream"], ShouldEqual, true)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 1)
		So(messages[0].(map[string]interface{})["role"], ShouldEqual, "user")
		tools := chatReq["tools"].([]interface{})
		So(len(tools), ShouldEqual, 1)
		So(chatReq["tool_choice"], ShouldEqual, "auto")
	})
}

// assertNoOrphanToolMessages 跨链路断言（chat §3.1/§6.1.1 配对约束）：
// 产物中每个 role:tool 消息的 tool_call_id 都必须能在某条 assistant.tool_calls[].id 中找到，
// 否则即为丢弃 call item 后遗留的悬空引用。
func assertNoOrphanToolMessages(chatReq map[string]interface{}) {
	emitted := map[string]bool{}
	messages, _ := chatReq["messages"].([]interface{})
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, _ := msg["tool_calls"].([]interface{})
		for _, rawTC := range tcs {
			tc, ok := rawTC.(map[string]interface{})
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); id != "" {
				emitted[id] = true
			}
		}
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "tool" {
			continue
		}
		id, _ := msg["tool_call_id"].(string)
		So(emitted[id], ShouldBeTrue)
	}
}

// TestConvertResponsesToChatRequest_DroppedCallPairingIntegrity 锁定 P1-1/P2-2：
// call item 因结构性缺失被丢弃后，其配对 output 不得产出无 assistant.tool_calls 配对的 role:tool。
func TestConvertResponsesToChatRequest_DroppedCallPairingIntegrity(t *testing.T) {
	Convey("P1-1: 丢弃 call item 后不得遗留悬空 role:tool 消息", t, func() {
		Convey("坏 function_call（缺 call_id）被丢弃 → 其 function_call_output 一并丢弃", func() {
			body := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "message", "role": "user", "content": "hi"},
					{"type": "function_call", "name": "get_weather", "arguments": "{}"},
					{"type": "function_call_output", "call_id": "fc_bad", "output": "sunny"}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", body, false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
			chatReq := parseChatRequest(out)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			So(messages[0].(map[string]interface{})["role"], ShouldEqual, "user")
			assertNoOrphanToolMessages(chatReq)
		})

		Convey("仅 function_call_output（call 从未产出）→ 丢弃，无孤儿 tool 消息", func() {
			body := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "message", "role": "user", "content": "hi"},
					{"type": "function_call_output", "call_id": "fc_orphan", "output": "sunny"}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", body, false)
			So(err, ShouldBeNil)
			chatReq := parseChatRequest(out)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			assertNoOrphanToolMessages(chatReq)
		})

		Convey("坏 custom_tool_call（缺 name）被丢弃 → 其 custom_tool_call_output 一并丢弃", func() {
			body := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "message", "role": "user", "content": "hi"},
					{"type": "custom_tool_call", "call_id": "cc_bad", "input": "raw"},
					{"type": "custom_tool_call_output", "call_id": "cc_bad", "output": "applied"}
				]
			}`)
			out, err := ConvertResponsesToChatRequest("gpt-test", body, false)
			So(err, ShouldBeNil)
			chatReq := parseChatRequest(out)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			assertNoOrphanToolMessages(chatReq)
		})
	})

	Convey("P2-2: 孤立 builtin output（无先行 call）→ 丢弃，无孤儿 tool 消息", t, func() {
		body := []byte(`{
			"model": "gpt-test",
			"input": [
				{"type": "message", "role": "user", "content": "hi"},
				{"type": "web_search_output", "call_id": "ws_orphan", "output": {"results": []}}
			]
		}`)
		out, err := ConvertResponsesToChatRequest("gpt-test", body, false)
		So(err, ShouldBeNil)
		So(out, ShouldNotBeNil)
		chatReq := parseChatRequest(out)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 1)
		So(messages[0].(map[string]interface{})["role"], ShouldEqual, "user")
		assertNoOrphanToolMessages(chatReq)
	})
}

// TestConvertResponsesToChatRequest_ToolFieldsWithoutSurvivingTools 锁定 P1-2/P2-1：
// tools 声明存在但转换后存活集为空（如 DN-6 白名单外工具全被丢弃）时，依赖工具集的
// tool_choice / parallel_tool_calls 不得写入，auto/none 等效缺省可保留。
func TestConvertResponsesToChatRequest_ToolFieldsWithoutSurvivingTools(t *testing.T) {
	allToolsDroppedBody := func(extra map[string]interface{}) []byte {
		req := map[string]interface{}{
			"model": "gpt-test",
			"input": "go",
			"tools": []interface{}{map[string]interface{}{"type": "web_search"}},
		}
		for k, v := range extra {
			req[k] = v
		}
		b, _ := json.Marshal(req)
		return b
	}

	Convey("P1-2: 工具全被丢弃 → 依赖工具集的 tool_choice 形态丢弃，不写入", t, func() {
		for _, tc := range []interface{}{
			"required",
			map[string]interface{}{"type": "custom", "custom": map[string]interface{}{"name": "x"}},
			map[string]interface{}{"type": "allowed_tools", "mode": "auto", "tools": []interface{}{}},
		} {
			out, err := ConvertResponsesToChatRequest("gpt-test", allToolsDroppedBody(map[string]interface{}{"tool_choice": tc}), false)
			So(err, ShouldBeNil)
			So(out, ShouldNotBeNil)
			chatReq := parseChatRequest(out)
			_, hasTools := chatReq["tools"]
			So(hasTools, ShouldBeFalse)
			_, hasChoice := chatReq["tool_choice"]
			So(hasChoice, ShouldBeFalse)
		}
	})

	Convey("P1-2: 工具全被丢弃 → auto 等效上游缺省，保留写出", t, func() {
		out, err := ConvertResponsesToChatRequest("gpt-test", allToolsDroppedBody(map[string]interface{}{"tool_choice": "auto"}), false)
		So(err, ShouldBeNil)
		chatReq := parseChatRequest(out)
		_, hasTools := chatReq["tools"]
		So(hasTools, ShouldBeFalse)
		So(chatReq["tool_choice"], ShouldEqual, "auto")
	})

	Convey("P1-2: 无 tools 声明时沿用既有门槛，tool_choice 不写入", t, func() {
		out, err := ConvertResponsesToChatRequest("gpt-test", []byte(`{"model":"gpt-test","input":"go","tool_choice":"auto"}`), false)
		So(err, ShouldBeNil)
		_, has := parseChatRequest(out)["tool_choice"]
		So(has, ShouldBeFalse)
	})

	Convey("P2-1: 工具全被丢弃 → parallel_tool_calls 不写入", t, func() {
		for _, v := range []bool{true, false} {
			out, err := ConvertResponsesToChatRequest("gpt-test", allToolsDroppedBody(map[string]interface{}{"parallel_tool_calls": v}), false)
			So(err, ShouldBeNil)
			chatReq := parseChatRequest(out)
			_, hasTools := chatReq["tools"]
			So(hasTools, ShouldBeFalse)
			_, hasPTC := chatReq["parallel_tool_calls"]
			So(hasPTC, ShouldBeFalse)
		}
	})

	Convey("正常路径回归：工具存活时 tool_choice/parallel_tool_calls 照旧写入", t, func() {
		body := []byte(`{
			"model": "gpt-test",
			"input": "go",
			"tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}],
			"tool_choice": "required",
			"parallel_tool_calls": true
		}`)
		out, err := ConvertResponsesToChatRequest("gpt-test", body, false)
		So(err, ShouldBeNil)
		chatReq := parseChatRequest(out)
		So(chatReq["tool_choice"], ShouldEqual, "required")
		So(chatReq["parallel_tool_calls"], ShouldEqual, true)
	})
}
