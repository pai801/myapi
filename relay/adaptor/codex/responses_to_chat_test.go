package codex

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

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

			result := ConvertChatResponseToResponses(chatBody, "deepseek-flash", true)
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

			result := ConvertChatResponseToResponses(chatBody, "deepseek-flash", false)
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

			result := ConvertChatResponseToResponses(chatBody, "deepseek-flash", true)
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

			result := convertToolsToOpenAI(tools)

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

			result := convertToolsToOpenAI(tools)

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

			result := convertToolsToOpenAI(tools)

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

			result := convertToolsToOpenAI(tools)

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

		result := convertToolsToOpenAI(tools)

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

		result := convertToolsToOpenAI(tools)

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
	Convey("convertToolsToOpenAI: 内建工具 web_search / local_shell / computer_use 扁平化", t, func() {

		Convey("web_search 无 name 字段 → 名字等于 type，description 固定为 built-in tool", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type": "web_search",
				},
			}

			result := convertToolsToOpenAI(tools)

			So(len(result), ShouldEqual, 1)
			out := result[0].(map[string]interface{})
			So(out["type"], ShouldEqual, "function")
			fn := out["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "web_search")
			So(fn["description"], ShouldEqual, "built-in tool")
			params := fn["parameters"].(map[string]interface{})
			So(params["properties"].(map[string]interface{})["input"].(map[string]interface{})["type"], ShouldEqual, "string")
			req := params["required"].([]interface{})
			So(req[0], ShouldEqual, "input")
		})

		Convey("local_shell 带 name 字段时优先使用 name", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type": "local_shell",
					"name": "shell_run",
				},
			}

			result := convertToolsToOpenAI(tools)

			So(len(result), ShouldEqual, 1)
			fn := result[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "shell_run")
		})

		Convey("computer_use 同样被保留", func() {
			tools := []interface{}{
				map[string]interface{}{
					"type": "computer_use",
				},
			}

			result := convertToolsToOpenAI(tools)

			So(len(result), ShouldEqual, 1)
			fn := result[0].(map[string]interface{})["function"].(map[string]interface{})
			So(fn["name"], ShouldEqual, "computer_use")
		})
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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		messages, ok := chatReq["messages"].([]interface{})
		So(ok, ShouldBeTrue)
		So(len(messages), ShouldEqual, 2)

		toolSearchOutput := messages[0].(map[string]interface{})
		So(toolSearchOutput["role"], ShouldEqual, "tool")
		So(toolSearchOutput["tool_call_id"], ShouldEqual, "ts_call_1")
		toolSearchContent, ok := toolSearchOutput["content"].(string)
		So(ok, ShouldBeTrue)
		So(toolSearchContent, ShouldContainSubstring, `"query":"agent tool"`)
		So(toolSearchContent, ShouldContainSubstring, `"tools":[{"description":"utility tool","name":"utility"}]`)

		webSearchOutput := messages[1].(map[string]interface{})
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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

		So(chatReq, ShouldNotBeNil)
		messages := chatReq["messages"].([]interface{})
		So(len(messages), ShouldEqual, 2)

		toolSearchOutput := messages[0].(map[string]interface{})
		So(toolSearchOutput["role"], ShouldEqual, "tool")
		toolSearchContent, ok := toolSearchOutput["content"].(string)
		So(ok, ShouldBeTrue)
		So(toolSearchContent, ShouldNotEqual, "")
		So(toolSearchContent, ShouldContainSubstring, `"multi_agent_v1"`)
		So(toolSearchContent, ShouldContainSubstring, `"spawn_agent"`)

		webSearchOutput := messages[1].(map[string]interface{})
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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

		result := convertToolsToOpenAI(tools)

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
			result := convertToolsToOpenAI(nil)
			So(len(result), ShouldEqual, 0)
		})

		Convey("空切片", func() {
			result := convertToolsToOpenAI([]interface{}{})
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

		result := convertFunctionCallItem(item)

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

		result := convertFunctionCallItem(item)

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

		result := convertFunctionCallItem(item)

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

		result := convertFunctionCallItem(item)

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

		result := convertFunctionCallItem(item)

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

		result := convertFunctionCallItem(item)

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

			result := ConvertChatResponseToResponses(chatBody, "gpt-test", false)
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

			result := ConvertChatResponseToResponses(chatBody, "gpt-test", false)
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

			result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

			result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

			result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

			result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

			result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, nil)
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

			oldOut := ConvertChatResponseToResponses(chatBody, "gpt-test", false)
			newOut := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, nil)

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

			result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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
	Convey("convertInputItem: type:custom_tool_call apply_patch → assistant message + tool_calls，input 字符串透传为 arguments", t, func() {
		patch := "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch"
		item := map[string]interface{}{
			"type":    "custom_tool_call",
			"call_id": "c1",
			"name":    "apply_patch",
			"input":   patch,
		}

		result := convertInputItem(item)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "assistant")
		tcs, ok := result["tool_calls"].([]interface{})
		So(ok, ShouldBeTrue)
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c1")
		So(tc["type"], ShouldEqual, "function")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "apply_patch")
		So(fn["arguments"], ShouldEqual, patch)
	})
}

func TestConvertInputItem_CustomToolCall_PlainCustom(t *testing.T) {
	Convey("convertInputItem: type:custom_tool_call 普通 custom 工具 → tool_calls.arguments=raw input 字符串", t, func() {
		item := map[string]interface{}{
			"type":    "custom_tool_call",
			"call_id": "c2",
			"name":    "my_grammar",
			"input":   "some raw text",
		}

		result := convertInputItem(item)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "assistant")
		tcs := result["tool_calls"].([]interface{})
		So(len(tcs), ShouldEqual, 1)
		tc := tcs[0].(map[string]interface{})
		So(tc["id"], ShouldEqual, "c2")
		fn := tc["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "my_grammar")
		So(fn["arguments"], ShouldEqual, "some raw text")
	})
}

func TestConvertInputItem_CustomToolCallOutput_StringOutput(t *testing.T) {
	Convey("convertInputItem: type:custom_tool_call_output 字符串 output → role:tool 消息，content=output 原文", t, func() {
		item := map[string]interface{}{
			"type":    "custom_tool_call_output",
			"call_id": "c1",
			"output":  "result text",
		}

		result := convertInputItem(item)

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

		result := convertInputItem(item)

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

		result := convertInputItem(item)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "assistant")
		So(result["reasoning_content"], ShouldEqual, "thinking 1\nthinking 2")
		_, hasContent := result["content"]
		So(hasContent, ShouldBeFalse)
	})
}

func TestConvertInputItem_ReasoningEmptyIgnored(t *testing.T) {
	Convey("convertInputItem: type:reasoning 无可用文本时不生成空 assistant message", t, func() {
		Convey("空 reasoning", func() {
			result := convertInputItem(map[string]interface{}{"type": "reasoning"})

			So(result, ShouldBeNil)
		})

		Convey("summary item 非 summary_text 或缺 text", func() {
			item := map[string]interface{}{
				"type": "reasoning",
				"summary": []interface{}{
					map[string]interface{}{"type": "other", "text": "ignored"},
					map[string]interface{}{"text": "ignored without type"},
					map[string]interface{}{"type": "summary_text"},
				},
			}

			result := convertInputItem(item)

			So(result, ShouldBeNil)
		})

		Convey("content 非 string", func() {
			item := map[string]interface{}{
				"type":    "reasoning",
				"content": []interface{}{map[string]interface{}{"text": "ignored"}},
			}

			result := convertInputItem(item)

			So(result, ShouldBeNil)
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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("deepseek-test", reqBody, false))

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

		result := convertInputItem(item)

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

		result := convertInputItem(item)

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

		result := convertInputItem(item)

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

		result := convertInputItem(item)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "tool")
		So(result["tool_call_id"], ShouldEqual, "ws1")
		So(result["content"], ShouldEqual, `[{"result":"x"}]`)
	})
}

func TestConvertInputItem_UnknownType_Fallback(t *testing.T) {
	Convey("convertInputItem: 未知 type → 显式丢弃，不再误判为 message", t, func() {
		item := map[string]interface{}{
			"type": "future_type",
		}

		result := convertInputItem(item)

		So(result, ShouldBeNil)
	})
}

func TestConvertInputToMessages_MixedWithNewCases(t *testing.T) {
	Convey("convertInputToMessages: 混合 message + function_call + custom_tool_call + custom_tool_call_output + tool_search_call + reasoning → 全部按新规则转换", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "hi"},
			map[string]interface{}{"type": "function_call", "call_id": "fc1", "name": "read_file", "arguments": "{\"path\":\"a.txt\"}"},
			map[string]interface{}{"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": "patch text"},
			map[string]interface{}{"type": "custom_tool_call_output", "call_id": "c1", "output": "patch applied"},
			map[string]interface{}{"type": "tool_search_call", "call_id": "ts1", "name": "search_docs", "arguments": map[string]interface{}{"query": "codex"}},
			map[string]interface{}{"type": "reasoning", "content": "thinking..."},
		}

		msgs := convertInputToMessages(input)

		// 期望 6 条：message + function_call + custom_tool_call + custom_tool_call_output + tool_search_call + reasoning
		So(len(msgs), ShouldEqual, 6)

		// 1. message
		m0 := msgs[0].(map[string]interface{})
		So(m0["role"], ShouldEqual, "user")
		So(m0["content"], ShouldEqual, "hi")

		// 2. function_call（保留原行为）
		m1 := msgs[1].(map[string]interface{})
		So(m1["role"], ShouldEqual, "assistant")
		fc1Tcs := m1["tool_calls"].([]interface{})
		So(len(fc1Tcs), ShouldEqual, 1)
		So(fc1Tcs[0].(map[string]interface{})["id"], ShouldEqual, "fc1")
		So(fc1Tcs[0].(map[string]interface{})["function"].(map[string]interface{})["name"], ShouldEqual, "read_file")

		// 3. custom_tool_call（新行为）
		m2 := msgs[2].(map[string]interface{})
		So(m2["role"], ShouldEqual, "assistant")
		ctcTcs := m2["tool_calls"].([]interface{})
		So(len(ctcTcs), ShouldEqual, 1)
		ctcTc := ctcTcs[0].(map[string]interface{})
		So(ctcTc["id"], ShouldEqual, "c1")
		So(ctcTc["type"], ShouldEqual, "function")
		ctcFn := ctcTc["function"].(map[string]interface{})
		So(ctcFn["name"], ShouldEqual, "apply_patch")
		So(ctcFn["arguments"], ShouldEqual, "patch text")

		// 4. custom_tool_call_output（新行为）
		m3 := msgs[3].(map[string]interface{})
		So(m3["role"], ShouldEqual, "tool")
		So(m3["tool_call_id"], ShouldEqual, "c1")
		So(m3["content"], ShouldEqual, "patch applied")

		// 5. tool_search_call（新行为）
		m4 := msgs[4].(map[string]interface{})
		So(m4["role"], ShouldEqual, "assistant")
		So(m4["tool_calls"].([]interface{})[0].(map[string]interface{})["function"].(map[string]interface{})["name"], ShouldEqual, "tool_search")

		// 6. reasoning（新行为）
		m5 := msgs[5].(map[string]interface{})
		So(m5["role"], ShouldEqual, "assistant")
		So(m5["reasoning_content"], ShouldEqual, "thinking...")
		_, hasContent := m5["content"]
		So(hasContent, ShouldBeFalse)
	})
}

func TestResponsesChatResponsesRoundTrip_ToolSearchCallPreserved(t *testing.T) {
	Convey("responses→chat→responses: function_call/custom_tool_call/tool_search_call/web_search_call 保留", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "function_call", "call_id": "fc1", "name": "read_file", "arguments": "{\"path\":\"a.txt\"}"},
			map[string]interface{}{"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch", "input": "*** Begin Patch\n*** Add File: a.txt\n+hello\n*** End Patch"},
			map[string]interface{}{"type": "tool_search_call", "call_id": "ts1", "name": "search_docs", "arguments": map[string]interface{}{"query": "codex", "top_k": 3}},
			map[string]interface{}{"type": "web_search_call", "call_id": "ws1", "name": "web_search", "arguments": map[string]interface{}{"query": "codex"}},
		}

		msgs := convertInputToMessages(input)
		So(len(msgs), ShouldEqual, 4)

		var toolCalls []interface{}
		for _, msg := range msgs {
			m := msg.(map[string]interface{})
			if tcs, ok := m["tool_calls"].([]interface{}); ok {
				toolCalls = append(toolCalls, tcs...)
			}
		}
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 4)
		So(output[0].(map[string]interface{})["type"], ShouldEqual, "function_call")
		So(output[0].(map[string]interface{})["call_id"], ShouldEqual, "fc1")
		So(output[1].(map[string]interface{})["type"], ShouldEqual, "custom_tool_call")
		So(output[1].(map[string]interface{})["call_id"], ShouldEqual, "c1")
		So(output[2].(map[string]interface{})["type"], ShouldEqual, "tool_search_call")
		So(output[2].(map[string]interface{})["call_id"], ShouldEqual, "ts1")
		So(output[3].(map[string]interface{})["type"], ShouldEqual, "web_search_call")
		So(output[3].(map[string]interface{})["call_id"], ShouldEqual, "ws1")
	})
}

// -----------------------------------------------------------------------------
// 8 个遗留 advisory 的补测：锁死边界行为（TDD 退化为补 GREEN）
// -----------------------------------------------------------------------------

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
			result := convertFunctionCallItem(item)
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
			result := convertFunctionCallItem(item)
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
			result := convertFunctionCallItem(item)
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

		result := convertFunctionCallItem(item)

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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
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
	Convey("ConvertChatResponseToResponsesWithContext: upstream tool_call id 字段缺失 → id/call_id 均为 \"\"，不 panic，补字段仍为 fc_+空字符串", t, func() {
		reqBody := []byte(`{
			"model": "gpt-test",
			"tools": [
				{"type": "function", "name": "get_weather", "description": "x", "parameters": {"type": "object"}}
			]
		}`)
		// 自构 chat body，tool_call 故意不带 id 字段
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

		result := ConvertChatResponseToResponsesWithContext(chatBody, "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "function_call")
		// id 缺失 → call_id 为 ""，new path 补的 id 为 "fc_"
		So(item["call_id"], ShouldEqual, "")
		So(item["id"], ShouldEqual, "fc_")
		So(item["status"], ShouldEqual, "completed")
		So(item["name"], ShouldEqual, "get_weather")
	})
}

func TestConvertResponsesToChatRequest_ReasoningEffortMapping(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: reasoning.effort 映射表", t, func() {
		reqBase := func(effort string) []byte {
			body, _ := json.Marshal(map[string]interface{}{
				"model":     "gpt-test",
				"reasoning": map[string]interface{}{"effort": effort},
			})
			return body
		}

		Convey("max → reasoning_effort=max（回归：Codex 默认发 max，之前落入 default 被映射成 auto）", func() {
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("max"), false))
			So(chatReq["reasoning_effort"], ShouldEqual, "max")
		})

		Convey("none/low/medium/high/xhigh 正常映射", func() {
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("none"), false))["reasoning_effort"], ShouldEqual, "none")
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("low"), false))["reasoning_effort"], ShouldEqual, "low")
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("medium"), false))["reasoning_effort"], ShouldEqual, "medium")
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("high"), false))["reasoning_effort"], ShouldEqual, "high")
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("xhigh"), false))["reasoning_effort"], ShouldEqual, "xhigh")
		})

		Convey("auto → 映射为 high（上游枚举不含 auto，透传会 400）", func() {
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("auto"), false))
			So(chatReq["reasoning_effort"], ShouldEqual, "high")
		})

		Convey("未知 effort → 兜底 high（不再兜底 auto）", func() {
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("ultra"), false))
			So(chatReq["reasoning_effort"], ShouldEqual, "high")
		})

		Convey("无 reasoning 字段 → 不设置 reasoning_effort", func() {
			body, _ := json.Marshal(map[string]interface{}{"model": "gpt-test"})
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", body, false))
			_, has := chatReq["reasoning_effort"]
			So(has, ShouldBeFalse)
		})
	})
}
