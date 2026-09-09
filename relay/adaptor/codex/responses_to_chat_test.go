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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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
			result := convertInputItem(map[string]interface{}{"type": "reasoning"}, nil)

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

			result := convertInputItem(item, nil)

			So(result, ShouldBeNil)
		})

		Convey("content 非 string", func() {
			item := map[string]interface{}{
				"type":    "reasoning",
				"content": []interface{}{map[string]interface{}{"text": "ignored"}},
			}

			result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		result := convertInputItem(item, nil)

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

		Convey("none/minimal/low/medium/high/xhigh 正常映射（minimal 直通，不降级为 low）", func() {
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("none"), false))["reasoning_effort"], ShouldEqual, "none")
			So(parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBase("minimal"), false))["reasoning_effort"], ShouldEqual, "minimal")
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

func TestConvertResponsesToChatRequest_AgentMessageItem(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: input 中的 agent_message item 转为 assistant message", t, func() {

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

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "first part\nsecond part")
		})

		Convey("T2: agent_message content 为空数组 → 丢弃且不报错", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{"type": "agent_message", "id": "am_2", "content": []}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 0)
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

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

		Convey("T4: content 混排非 text 块/空文本/有效文本 → 只保留有效文本块", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "agent_message",
						"id": "am_4",
						"content": [
							{"type": "image_url", "image_url": {"url": "http://x"}},
							{"type": "text", "text": ""},
							{"type": "text", "text": "valid part"},
							"not-a-map"
						]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			So(chatReq, ShouldNotBeNil)
			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			So(msg["content"], ShouldEqual, "valid part")
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

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			_, has := chatReq["response_format"]
			So(has, ShouldBeFalse)
		})

		Convey("T5: 未识别 format type → 原样直通", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": "hi",
				"text": {"format": {"type": "future_format", "extra": 1}}
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			rf, ok := chatReq["response_format"].(map[string]interface{})
			So(ok, ShouldBeTrue)
			So(rf["type"], ShouldEqual, "future_format")
			So(rf["extra"], ShouldEqual, float64(1))
		})
	})
}

func TestConvertResponsesToChatRequest_RefusalPart(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: input message 的 refusal part → chat content part", t, func() {

		Convey("T1: assistant text+refusal 并存 → 按 chat §4 互斥规则保留 refusal 丢弃 text", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "output_text", "text": "I can do this part"},
							{"type": "refusal", "refusal": "but I cannot do that"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			content, ok := msg["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			refusalPart := content[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "but I cannot do that")
		})

		Convey("T2: 仅 refusal part → content 数组只含 refusal part", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "assistant",
						"content": [{"type": "refusal", "refusal": "I refuse"}]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			content, ok := msg["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			So(content[0].(map[string]interface{})["type"], ShouldEqual, "refusal")
			So(content[0].(map[string]interface{})["refusal"], ShouldEqual, "I refuse")
		})

		Convey("T3: refusal part 文本为空 → 忽略，行为与现状一致（纯文本消息拼成字符串）", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "refusal", "refusal": ""},
							{"type": "output_text", "text": "normal reply"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			content, ok := msg["content"].(string)
			So(ok, ShouldBeTrue)
			So(content, ShouldEqual, "normal reply")
		})

		Convey("T4: user 消息含 refusal part → 丢弃（chat §4 refusal 仅 assistant），text 保留", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "user",
						"content": [
							{"type": "input_text", "text": "please help"},
							{"type": "refusal", "refusal": "stray refusal"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "user")
			content := msg["content"]
			// 仅剩 text，且按纯文本消息拼成字符串
			So(content, ShouldEqual, "please help")
		})

		Convey("T5: system 消息仅 refusal part → 丢弃后 content 为空串", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "system",
						"content": [{"type": "refusal", "refusal": "should not appear"}]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "system")
			So(msg["content"], ShouldEqual, "")
		})

		Convey("T6: refusal 先行 + output_text 后到（[refusal, text] 乱序）→ 互斥保留 refusal，与 [text, refusal] 顺序结果等价", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "refusal", "refusal": "but I cannot do that"},
							{"type": "output_text", "text": "I can do this part"}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			So(msg["role"], ShouldEqual, "assistant")
			content, ok := msg["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			refusalPart := content[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "but I cannot do that")
		})

		Convey("T7: refusal 先行 + image 后到（[refusal, media] 乱序）→ 互斥丢弃 media，只保留 refusal", func() {
			reqBody := []byte(`{
				"model": "gpt-test",
				"input": [
					{
						"type": "message",
						"role": "assistant",
						"content": [
							{"type": "refusal", "refusal": "cannot show"},
							{"type": "input_image", "image_url": {"url": "http://x/img.png"}}
						]
					}
				]
			}`)

			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

			messages := chatReq["messages"].([]interface{})
			So(len(messages), ShouldEqual, 1)
			msg := messages[0].(map[string]interface{})
			content, ok := msg["content"].([]interface{})
			So(ok, ShouldBeTrue)
			So(len(content), ShouldEqual, 1)
			refusalPart := content[0].(map[string]interface{})
			So(refusalPart["type"], ShouldEqual, "refusal")
			So(refusalPart["refusal"], ShouldEqual, "cannot show")
		})
	})
}

// parseResponsesMap 解析 ConvertChatResponseToResponsesWithContext 的完整响应对象（含 status 等顶层字段）。
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

			result := ConvertChatResponseToResponses(chatBody, "gpt-test", false)
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

			result := ConvertChatResponseToResponses(chatBody, "gpt-test", false)
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

			result := ConvertChatResponseToResponses(chatBody, "gpt-test", false)
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
			resp := parseResponsesMap(ConvertChatResponseToResponses(chatBodyWithFinish("length"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "incomplete")
			details := resp["incomplete_details"].(map[string]interface{})
			So(details["reason"], ShouldEqual, "max_output_tokens")
			So(resp["truncated"], ShouldEqual, true)
		})

		Convey("T2: stop → status=completed 且无 incomplete_details", func() {
			resp := parseResponsesMap(ConvertChatResponseToResponses(chatBodyWithFinish("stop"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "completed")
			_, has := resp["incomplete_details"]
			So(has, ShouldBeFalse)
		})

		Convey("T3: tool_calls → status=completed", func() {
			resp := parseResponsesMap(ConvertChatResponseToResponses(chatBodyWithFinish("tool_calls"), "gpt-test", false))
			So(resp["status"], ShouldEqual, "completed")
		})

		Convey("T4: content_filter → status=incomplete + incomplete_details.reason=content_filter + truncated", func() {
			resp := parseResponsesMap(ConvertChatResponseToResponses(chatBodyWithFinish("content_filter"), "gpt-test", false))
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
			resp := parseResponsesMap(ConvertChatResponseToResponses(b, "gpt-test", false))
			So(resp["status"], ShouldEqual, "completed")
		})
	})
}

func TestConvertResponsesToChatRequest_ToolChoice(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: tool_choice 形状转换（responses §2 → chat §5.2）", t, func() {

		reqBody := func(toolChoice interface{}) []byte {
			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-test",
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
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody(map[string]interface{}{
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
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody(chatShape), false))
			So(chatReq["tool_choice"], ShouldResemble, chatShape)
		})

		Convey("T3: 字符串 auto/none/required → 原样直通", func() {
			for _, s := range []string{"auto", "none", "required"} {
				chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody(s), false))
				So(chatReq["tool_choice"], ShouldEqual, s)
			}
		})

		Convey("T4: 其他对象形式（如 chat 形状 custom）→ 原样直通", func() {
			custom := map[string]interface{}{
				"type":   "custom",
				"custom": map[string]interface{}{"name": "x"},
			}
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody(custom), false))
			So(chatReq["tool_choice"], ShouldResemble, custom)
		})

		Convey("T7: 字符串 function:<name> → chat {type:function, function:{name}}（chat §5.2 强制调用形状）", func() {
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody("function:get_weather"), false))
			tc := chatReq["tool_choice"].(map[string]interface{})
			So(tc["type"], ShouldEqual, "function")
			So(tc["function"].(map[string]interface{})["name"], ShouldEqual, "get_weather")
		})

		Convey("T8: 指向工具 id 等 chat 不可表达的字符串 → 显式降级 auto，不透传非法形状", func() {
			for _, s := range []string{"call_abc123", "custom:apply_patch", "function:", "mcp:server_x"} {
				chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody(s), false))
				So(chatReq["tool_choice"], ShouldEqual, "auto")
			}
		})

		Convey("T5: tool_choice 缺失 → 不设置该字段", func() {
			body, _ := json.Marshal(map[string]interface{}{
				"model": "gpt-test",
				"tools": []interface{}{
					map[string]interface{}{
						"type": "function",
						"name": "get_weather",
					},
				},
			})
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", body, false))
			_, has := chatReq["tool_choice"]
			So(has, ShouldBeFalse)
		})

		Convey("T6: 无 tools 时 tool_choice 不写入（沿用既有门槛）", func() {
			body, _ := json.Marshal(map[string]interface{}{
				"model":       "gpt-test",
				"tool_choice": "auto",
			})
			chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", body, false))
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
			resp := parseResponsesMap(ConvertChatResponseToResponses(b, "gpt-test", false))
			So(resp["created_at"], ShouldEqual, int64(1700000000))
		})

		Convey("T2: 不写 chat 风格的 created 键", func() {
			resp := parseResponsesMap(ConvertChatResponseToResponses(b, "gpt-test", false))
			_, hasCreated := resp["created"]
			So(hasCreated, ShouldBeFalse)
		})

		Convey("T3: chat 响应无 created → 不写 created_at", func() {
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
			resp := parseResponsesMap(ConvertChatResponseToResponses(nb, "gpt-test", false))
			_, has := resp["created_at"]
			So(has, ShouldBeFalse)
		})
	})
}

// -----------------------------------------------------------------------------
// #3 修复：content parts 补 input_audio / input_file（responses §3.1 → chat §4）
// -----------------------------------------------------------------------------

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

			result := convertContentArray(content, "user")
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

			result := convertContentArray(content, "user")
			parts := result.([]interface{})
			part := parts[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "input_audio")
			inner := part["input_audio"].(map[string]interface{})
			So(inner["data"], ShouldEqual, "rawdata")
			So(inner["format"], ShouldEqual, "mp3")
		})

		Convey("缺 data 或 format → 丢弃，剩余文本仍拼接为字符串", func() {
			content := []interface{}{
				map[string]interface{}{
					"type":   "input_audio",
					"format": "wav",
				},
				map[string]interface{}{"type": "input_text", "text": "hello"},
			}

			result := convertContentArray(content, "user")
			So(result, ShouldEqual, "hello")
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

			result := convertContentArray(content, "user")
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

			result := convertContentArray(content, "user")
			parts := result.([]interface{})
			part := parts[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "file")
			inner := part["file"].(map[string]interface{})
			So(inner["filename"], ShouldEqual, "a.txt")
			So(inner["file_data"], ShouldEqual, "base64")
		})

		Convey("空 file 对象 → 丢弃，无 media 时结果为默认空串", func() {
			content := []interface{}{
				map[string]interface{}{
					"type": "input_file",
					"file": map[string]interface{}{},
				},
			}

			result := convertContentArray(content, "user")
			So(result, ShouldEqual, "")
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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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
	Convey("convertInputItem: responses §3.8 已知不可映射类型 → 丢弃且不 panic（Info 级，不再 Errorf 误报）", t, func() {
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
			result := convertInputItem(map[string]interface{}{"type": typ}, nil)
			So(result, ShouldBeNil)
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

		result := convertInputItem(item, nil)

		So(result, ShouldNotBeNil)
		So(result["role"], ShouldEqual, "developer")
		So(result["content"], ShouldEqual, "system rules")

		Convey("role 缺失仍默认 user，role=user/system 保持原样", func() {
			So(convertInputItem(map[string]interface{}{"type": "message", "content": "x"}, nil)["role"], ShouldEqual, "user")
			So(convertInputItem(map[string]interface{}{"type": "message", "role": "user", "content": "x"}, nil)["role"], ShouldEqual, "user")
			So(convertInputItem(map[string]interface{}{"type": "message", "role": "system", "content": "x"}, nil)["role"], ShouldEqual, "system")
		})
	})
}

// -----------------------------------------------------------------------------
// #3 修复：工具类型补齐（responses §9 → chat §5.1 function 扁平化）
// -----------------------------------------------------------------------------

func TestConvertToolsToOpenAI_WebSearchPreview(t *testing.T) {
	Convey("convertToolsToOpenAI: web_search_preview（responses §9 预览名）→ function 工具", t, func() {
		tools := []interface{}{
			map[string]interface{}{"type": "web_search_preview"},
		}

		result := convertToolsToOpenAI(tools)

		So(len(result), ShouldEqual, 1)
		out := result[0].(map[string]interface{})
		So(out["type"], ShouldEqual, "function")
		fn := out["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "web_search_preview")
		So(fn["description"], ShouldEqual, "built-in tool")
	})
}

func TestConvertToolsToOpenAI_ShellAndComputerFamily(t *testing.T) {
	Convey("convertToolsToOpenAI: shell / computer / computer_use_preview 与既有内建工具同族扁平化", t, func() {
		tools := []interface{}{
			map[string]interface{}{"type": "shell"},
			map[string]interface{}{"type": "computer"},
			map[string]interface{}{"type": "computer_use_preview"},
		}

		result := convertToolsToOpenAI(tools)

		So(len(result), ShouldEqual, 3)
		names := make([]string, 0, 3)
		for _, r := range result {
			fn := r.(map[string]interface{})["function"].(map[string]interface{})
			names = append(names, fn["name"].(string))
		}
		So(names, ShouldResemble, []string{"shell", "computer", "computer_use_preview"})
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

		result := convertToolsToOpenAI(tools)

		So(len(result), ShouldEqual, 6)
		fn := result[0].(map[string]interface{})["function"].(map[string]interface{})
		So(fn["name"], ShouldEqual, "apply_patch")
		So(fn["description"], ShouldEqual, "patch files")
		sub := result[1].(map[string]interface{})["function"].(map[string]interface{})
		So(sub["name"], ShouldEqual, "apply_patch_add_file")
	})
}

func TestConvertToolsToOpenAI_UnmappableTypesDropped(t *testing.T) {
	Convey("convertToolsToOpenAI: 不可映射工具类型 → Info 丢弃，不 panic", t, func() {
		for _, typ := range []string{
			"file_search", "code_interpreter", "image_generation",
			"mcp", "programmatic_tool_calling", "future_tool_type",
		} {
			result := convertToolsToOpenAI([]interface{}{
				map[string]interface{}{"type": typ},
			})
			So(len(result), ShouldEqual, 0)
		}
	})
}

// -----------------------------------------------------------------------------
// #3 修复：顶层字段直通（responses §2 → chat §2）
// -----------------------------------------------------------------------------

func TestConvertResponsesToChatRequest_TopLevelPassthrough(t *testing.T) {
	Convey("ConvertResponsesToChatRequest: 顶层字段直通", t, func() {
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
			"previous_response_id": "resp_1",
			"include": ["file_search_call.results"]
		}`)

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

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
		// 无 chat 对应的字段不写入
		_, hasPrev := chatReq["previous_response_id"]
		So(hasPrev, ShouldBeFalse)
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

		chatReq := parseChatRequest(ConvertResponsesToChatRequest("gpt-test", reqBody, false))

		_, hasStore := chatReq["store"]
		So(hasStore, ShouldBeFalse)
		_, hasMeta := chatReq["metadata"]
		So(hasMeta, ShouldBeFalse)
	})
}

// -----------------------------------------------------------------------------
// #3 修复：输出方向 annotations / audio（chat §6.1 → responses）
// -----------------------------------------------------------------------------

func TestConvertChatResponseToResponses_Annotations(t *testing.T) {
	Convey("ConvertChatResponseToResponses: message.annotations → output_text part.annotations", t, func() {
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

		Convey("content 为 string → output_text part 携带 annotations", func() {
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

			output := parseOutputArray(ConvertChatResponseToResponses(b, "gpt-test", false))

			So(len(output), ShouldEqual, 1)
			msg := output[0].(map[string]interface{})
			content := msg["content"].([]interface{})
			part := content[0].(map[string]interface{})
			So(part["type"], ShouldEqual, "output_text")
			So(part["annotations"], ShouldResemble, annotations)
		})

		Convey("content 为数组 → annotations 附加到首个文本 part", func() {
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

			output := parseOutputArray(ConvertChatResponseToResponses(b, "gpt-test", false))

			msg := output[0].(map[string]interface{})
			content := msg["content"].([]interface{})
			So(len(content), ShouldEqual, 2)
			first := content[0].(map[string]interface{})
			So(first["type"], ShouldEqual, "text")
			So(first["annotations"], ShouldResemble, annotations)
			_, hasSecond := content[1].(map[string]interface{})["annotations"]
			So(hasSecond, ShouldBeFalse)
		})

		Convey("无 annotations → 行为不变（part 无 annotations 字段）", func() {
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

			output := parseOutputArray(ConvertChatResponseToResponses(b, "gpt-test", false))
			msg := output[0].(map[string]interface{})
			part := msg["content"].([]interface{})[0].(map[string]interface{})
			_, has := part["annotations"]
			So(has, ShouldBeFalse)
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

		output := parseOutputArray(ConvertChatResponseToResponses(b, "gpt-test", false))

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

		output := parseOutputArray(ConvertChatResponseToResponses(b, "gpt-test", false))

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
	Convey("非流式 chat→responses：builtin 工具输出与流式路径对齐（ts_/ws_ 前缀 + arguments 字符串 + execution 仅 tool_search）", t, func() {
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

		result := ConvertChatResponseToResponsesWithContext([]byte(chatBody), "gpt-test", false, reqBody)
		output := parseOutputArray(result)

		So(len(output), ShouldEqual, 2)

		ts := output[0].(map[string]interface{})
		So(ts["type"], ShouldEqual, "tool_search_call")
		So(ts["id"], ShouldEqual, "ts_call_ts_b1")
		So(ts["call_id"], ShouldEqual, "call_ts_b1")
		So(ts["name"], ShouldEqual, "tool_search")
		So(ts["arguments"], ShouldEqual, `{"query":"codex"}`)
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

func TestConvertInputToMessages_BuiltinFallbackCallIDPairing(t *testing.T) {
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
		msgs := convertInputToMessages(input)
		So(len(msgs), ShouldEqual, 8)

		callID := func(i int) string {
			m := msgs[i].(map[string]interface{})
			return m["tool_calls"].([]interface{})[0].(map[string]interface{})["id"].(string)
		}
		outID := func(i int) string {
			return msgs[i].(map[string]interface{})["tool_call_id"].(string)
		}

		// 每组 call/output 精确配对
		So(outID(1), ShouldEqual, callID(0))
		So(outID(3), ShouldEqual, callID(2))
		So(outID(5), ShouldEqual, callID(4))
		So(outID(7), ShouldEqual, callID(6))
		// 同类型两组互不串对
		So(callID(4), ShouldNotEqual, callID(0))
		So(callID(6), ShouldNotEqual, callID(2))
		// 类型前缀隔离（ts_/ws_），跨类型不可能串对
		So(strings.HasPrefix(callID(0), "ts_"), ShouldBeTrue)
		So(strings.HasPrefix(callID(2), "ws_"), ShouldBeTrue)
		So(strings.HasPrefix(outID(1), "ts_"), ShouldBeTrue)
	})

	Convey("连续两个 call 后跟两个 output → 与最近一个未配对 call 配对（LIFO）", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "tool_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "a"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "b"},
		}
		msgs := convertInputToMessages(input)
		So(len(msgs), ShouldEqual, 4)
		id0 := msgs[0].(map[string]interface{})["tool_calls"].([]interface{})[0].(map[string]interface{})["id"].(string)
		id1 := msgs[1].(map[string]interface{})["tool_calls"].([]interface{})[0].(map[string]interface{})["id"].(string)
		id2 := msgs[2].(map[string]interface{})["tool_call_id"].(string)
		id3 := msgs[3].(map[string]interface{})["tool_call_id"].(string)
		So(id2, ShouldEqual, id1)
		So(id3, ShouldEqual, id0)
	})

	Convey("孤立 output（无先行 fallback call）→ 确定性独立 id，不与后续 output 串对", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call_output", "output": "a"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "b"},
		}
		msgs := convertInputToMessages(input)
		So(len(msgs), ShouldEqual, 2)
		So(msgs[0].(map[string]interface{})["tool_call_id"], ShouldEqual, "ts_fb1")
		So(msgs[1].(map[string]interface{})["tool_call_id"], ShouldEqual, "ts_fb2")
	})

	Convey("两次独立转换结果逐字节一致 → 状态仅存活于单次转换，无跨请求残留", t, func() {
		input := []interface{}{
			map[string]interface{}{"type": "tool_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "tool_search_call_output", "output": "a"},
			map[string]interface{}{"type": "web_search_call", "arguments": "{}"},
			map[string]interface{}{"type": "web_search_call_output", "output": "b"},
		}
		msgsA := convertInputToMessages(input)
		msgsB := convertInputToMessages(input)
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
		out1 := ConvertResponsesToChatRequest("gpt-test", body, false)
		out2 := ConvertResponsesToChatRequest("gpt-test", body, false)
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
	Convey("未知 content block type → Warn 后跳过，不 panic 不产畸形条目", t, func() {
		content := []interface{}{
			map[string]interface{}{"type": "input_text", "text": "hello"},
			map[string]interface{}{"type": "input_video", "video_url": "https://example.com/v.mp4"},
		}
		result := convertContentArray(content, "user")
		s, ok := result.(string)
		So(ok, ShouldBeTrue)
		So(s, ShouldEqual, "hello")

		// 未知 type 与合法媒体共存：数组结果仅含已知合法 part
		content2 := []interface{}{
			map[string]interface{}{"type": "mystery_block", "payload": "x"},
			map[string]interface{}{"type": "input_image", "image_url": "http://img"},
		}
		arr, ok := convertContentArray(content2, "user").([]interface{})
		So(ok, ShouldBeTrue)
		So(len(arr), ShouldEqual, 1)
		So(arr[0].(map[string]interface{})["type"], ShouldEqual, "image_url")
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
		output := convertChatMessageToOutput(message, nil)
		So(len(output), ShouldEqual, 1)
		item := output[0].(map[string]interface{})
		So(item["type"], ShouldEqual, "custom_tool_call")
		So(item["id"], ShouldEqual, "ctc_call_cx1")
		So(item["call_id"], ShouldEqual, "call_cx1")
		So(item["name"], ShouldEqual, "apply_patch")
		So(item["input"], ShouldEqual, "*** Begin Patch")
		So(item["status"], ShouldEqual, "completed")
	})

	Convey("畸形 custom（缺 name/input 或整段缺失）与无 function/custom 载荷 → 显式丢弃不 panic", t, func() {
		message := map[string]interface{}{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "call_bad1", "type": "custom", "custom": map[string]interface{}{"name": "only_name"}},
				map[string]interface{}{"id": "call_bad2", "type": "custom"},
				map[string]interface{}{"id": "call_bad3", "type": "mystery"},
			},
		}
		output := convertChatMessageToOutput(message, nil)
		So(output, ShouldHaveLength, 0)
	})
}
