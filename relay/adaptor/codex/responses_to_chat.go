package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pai801/myapi/common/logger"
)

func ConvertResponsesToChatRequest(modelName string, inputRawJSON []byte, stream bool) []byte {
	var req map[string]interface{}
	if err := json.Unmarshal(inputRawJSON, &req); err != nil {
		return inputRawJSON
	}

	chatReq := map[string]interface{}{
		"model":    modelName,
		"messages": []interface{}{},
		"stream":   stream,
	}

	if stream {
		chatReq["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
	}

	if v, ok := req["max_output_tokens"].(float64); ok {
		chatReq["max_tokens"] = int(v)
	}
	if v, ok := req["temperature"].(float64); ok {
		chatReq["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok {
		chatReq["top_p"] = v
	}
	if v, ok := req["user"].(string); ok {
		chatReq["user"] = v
	}

	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		messages := chatReq["messages"].([]interface{})
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": instructions,
		})
		chatReq["messages"] = messages
	}

	if input, ok := req["input"]; ok {
		messages := chatReq["messages"].([]interface{})
		inputMessages := convertInputToMessages(input)
		chatReq["messages"] = append(messages, inputMessages...)
	}

	mergedTools := mergeResponseTools(req)
	if len(mergedTools) > 0 {
		chatReq["tools"] = convertToolsToOpenAI(mergedTools)
	}

	if v, ok := req["tool_choice"]; ok {
		if len(mergedTools) > 0 {
			chatReq["tool_choice"] = v
		}
	}

	if v, ok := req["parallel_tool_calls"].(bool); ok {
		if len(mergedTools) > 0 {
			chatReq["parallel_tool_calls"] = v
		}
	}

	if reasoning, ok := req["reasoning"].(map[string]interface{}); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			switch effort {
			case "none":
				chatReq["reasoning_effort"] = "none"
			case "auto":
				chatReq["reasoning_effort"] = "auto"
			case "minimal":
				chatReq["reasoning_effort"] = "low"
			case "low":
				chatReq["reasoning_effort"] = "low"
			case "medium":
				chatReq["reasoning_effort"] = "medium"
			case "high":
				chatReq["reasoning_effort"] = "high"
			case "xhigh":
				chatReq["reasoning_effort"] = "xhigh"
			default:
				chatReq["reasoning_effort"] = "auto"
			}
		}
	}

	result, _ := json.Marshal(chatReq)
	return result
}

func convertInputToMessages(input interface{}) []interface{} {
	var messages []interface{}

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": v,
		})
	case []interface{}:
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if msg := convertInputItem(itemMap); msg != nil {
					messages = append(messages, msg)
				}
			}
		}
	}

	return messages
}

func convertInputItem(item map[string]interface{}) map[string]interface{} {
	itemType, _ := item["type"].(string)
	if itemType == "" {
		if _, hasRole := item["role"]; hasRole {
			itemType = "message"
		}
	}

	switch itemType {
	case "message":
		return convertMessageItem(item)
	case "function_call":
		return convertFunctionCallItem(item)
	case "function_call_output":
		return convertFunctionCallOutputItem(item)
	case "custom_tool_call":
		return convertCustomToolCallItem(item)
	case "custom_tool_call_output":
		return convertCustomToolCallOutputItem(item)
	case "reasoning":
		return convertReasoningItem(item)
	case "tool_search_call":
		return convertToolSearchCallItem(item)
	case "tool_search_call_output", "tool_search_output":
		return convertToolSearchCallOutputItem(item)
	case "web_search_call":
		return convertWebSearchCallItem(item)
	case "web_search_call_output", "web_search_output":
		return convertWebSearchCallOutputItem(item)
	default:
		logger.Log.Errorf("unknown codex input item type: " + itemType)
		return nil
	}
}

func convertReasoningItem(item map[string]interface{}) map[string]interface{} {
	summary, _ := item["summary"].([]interface{})
	var parts []string
	for _, raw := range summary {
		part, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if partType, _ := part["type"].(string); partType != "summary_text" {
			continue
		}
		if text, ok := part["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		if content, ok := item["content"].(string); ok && content != "" {
			parts = append(parts, content)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]interface{}{
		"role":              "assistant",
		"reasoning_content": strings.Join(parts, "\n"),
	}
}

func convertToolSearchCallItem(item map[string]interface{}) map[string]interface{} {
	return convertBuiltinToolCallItem(item, "tool_search", "ts_")
}

func convertToolSearchCallOutputItem(item map[string]interface{}) map[string]interface{} {
	return convertBuiltinToolCallOutputItem(item, "ts_")
}

func convertWebSearchCallItem(item map[string]interface{}) map[string]interface{} {
	return convertBuiltinToolCallItem(item, "web_search", "ws_")
}

func convertWebSearchCallOutputItem(item map[string]interface{}) map[string]interface{} {
	return convertBuiltinToolCallOutputItem(item, "ws_")
}

func convertBuiltinToolCallItem(item map[string]interface{}, name, idPrefix string) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID = idPrefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	args := getStringOrJSONRaw(item["arguments"])
	if args == "" {
		args = "{}"
	}

	return map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			},
		},
	}
}

func convertBuiltinToolCallOutputItem(item map[string]interface{}, idPrefix string) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID = idPrefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	content := getStringOrJSONRaw(item["output"])
	if content == "" {
		content = getBuiltinToolOutputPayload(item)
	}
	return map[string]interface{}{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      content,
	}
}

func getBuiltinToolOutputPayload(item map[string]interface{}) string {
	payload := make(map[string]interface{})
	for key, value := range item {
		switch key {
		case "type", "call_id", "status", "execution", "output":
			continue
		default:
			payload[key] = value
		}
	}
	if len(payload) == 0 {
		return ""
	}
	return getStringOrJSONRaw(payload)
}

func convertMessageItem(item map[string]interface{}) map[string]interface{} {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}

	if role == "developer" {
		role = "system"
	}

	message := map[string]interface{}{
		"role":    role,
		"content": "",
	}

	if content, ok := item["content"]; ok {
		switch v := content.(type) {
		case string:
			message["content"] = v
		case []interface{}:
			message["content"] = convertContentArray(v)
		}
	}

	return message
}

func convertContentArray(content []interface{}) interface{} {
	var textParts []string
	var hasMedia bool
	chatContent := []interface{}{}

	for _, block := range content {
		if blockMap, ok := block.(map[string]interface{}); ok {
			blockType, _ := blockMap["type"].(string)
			if blockType == "" {
				blockType = "input_text"
			}

			switch blockType {
			case "input_text", "output_text", "text":
				if text, ok := blockMap["text"].(string); ok && text != "" {
					textParts = append(textParts, text)
					chatContent = append(chatContent, map[string]interface{}{
						"type": "text",
						"text": text,
					})
				}
			case "input_image", "image_url":
				if imgBlock := convertImageBlock(blockMap); imgBlock != nil {
					chatContent = append(chatContent, imgBlock)
					hasMedia = true
				}
			}
		}
	}

	if hasMedia {
		return chatContent
	}
	if len(textParts) > 0 {
		return strings.Join(textParts, "\n")
	}
	return ""
}

func convertImageBlock(block map[string]interface{}) interface{} {
	// 1. Try image_url format (existing code)
	if imageURL, ok := block["image_url"]; ok {
		switch v := imageURL.(type) {
		case string:
			if v != "" {
				return map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": v,
					},
				}
			}
		case map[string]interface{}:
			if url, ok := v["url"].(string); ok && url != "" {
				return map[string]interface{}{
					"type":      "image_url",
					"image_url": v,
				}
			}
		}
	}

	// 2. Try source format (base64 or url source)
	if source, ok := block["source"].(map[string]interface{}); ok {
		sourceType, _ := source["type"].(string)
		switch sourceType {
		case "base64":
			mediaType, _ := source["media_type"].(string)
			data, _ := source["data"].(string)
			if mediaType == "" || data == "" {
				return nil
			}
			return map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": "data:" + mediaType + ";base64," + data,
				},
			}
		case "url":
			url, _ := source["url"].(string)
			if url == "" {
				return nil
			}
			return map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": url,
				},
			}
		}
	}

	return nil
}

func convertFunctionCallItem(item map[string]interface{}) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	if namespace, ok := item["namespace"].(string); ok && namespace != "" {
		name = flattenNamespaceToolName(namespace, name)
	}
	args := getStringOrJSONRaw(item["arguments"])
	if args == "" {
		args = "{}"
	}

	return map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			},
		},
	}
}

func convertFunctionCallOutputItem(item map[string]interface{}) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	output := getStringOrJSONRaw(item["output"])

	return map[string]interface{}{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      output,
	}
}

// convertCustomToolCallItem 把 custom_tool_call 转成 chat 协议的 tool_calls 元素。
// 与 convertFunctionCallItem 对称：返回 assistant message + 单元素 tool_calls 数组。
// arguments 采用方案 A：直接透传 item["input"] 原始字符串（apply_patch 文本或其他 custom 工具的 raw input）。
// 真正的格式还原在响应侧由 reconstructCustomToolCallInput 负责（#4 已修）。
func convertCustomToolCallItem(item map[string]interface{}) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	input := getStringOrJSONRaw(item["input"])

	return map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": input,
				},
			},
		},
	}
}

func getStringOrJSONRaw(v interface{}) string {
	switch out := v.(type) {
	case string:
		return out
	case []byte:
		return string(out)
	case json.RawMessage:
		return string(out)
	default:
		if v == nil {
			return ""
		}
		if data, err := json.Marshal(v); err == nil {
			return string(data)
		}
		return ""
	}
}

// convertCustomToolCallOutputItem 把 custom_tool_call_output 转成 role:tool 消息。
// output 字段归一化由 normalizeCustomToolOutput 处理。
func convertCustomToolCallOutputItem(item map[string]interface{}) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	content := normalizeCustomToolOutput(item["output"])

	return map[string]interface{}{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      content,
	}
}

// normalizeCustomToolOutput 把 custom_tool_call_output.output 归一化为字符串。
// 兼容：string 原文、含 text 字段的对象、其他类型一律回退空串。
func normalizeCustomToolOutput(v interface{}) string {
	switch out := v.(type) {
	case string:
		return out
	case map[string]interface{}:
		if text, ok := out["text"].(string); ok {
			return text
		}
		return ""
	default:
		return ""
	}
}

func convertToolsToOpenAI(tools []interface{}) []interface{} {
	var result []interface{}
	seen := make(map[string]struct{})

	for _, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		toolType, _ := toolMap["type"].(string)
		switch toolType {
		case "function", "":
			if fn := buildFunctionTool(toolMap); fn != nil && appendUniqueChatTool(&result, seen, fn) {
			}
		case "custom":
			appendUniqueChatTools(&result, seen, flattenCustomTool(toolMap))
		case "namespace":
			appendUniqueChatTools(&result, seen, flattenNamespaceTool(toolMap))
		case "web_search", "local_shell", "computer_use", "tool_search":
			appendUniqueChatTools(&result, seen, flattenBuiltinTool(toolType, toolMap))
		}
	}

	return result
}

func appendUniqueChatTools(dst *[]interface{}, seen map[string]struct{}, tools []interface{}) {
	for _, tool := range tools {
		appendUniqueChatTool(dst, seen, tool)
	}
}

func appendUniqueChatTool(dst *[]interface{}, seen map[string]struct{}, tool interface{}) bool {
	toolMap, ok := tool.(map[string]interface{})
	if !ok {
		return false
	}
	fn, ok := toolMap["function"].(map[string]interface{})
	if !ok {
		return false
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return false
	}
	if _, exists := seen[name]; exists {
		return false
	}
	seen[name] = struct{}{}
	*dst = append(*dst, tool)
	return true
}

func mergeResponseTools(req map[string]interface{}) []interface{} {
	var merged []interface{}
	if tools, ok := req["tools"].([]interface{}); ok {
		merged = append(merged, tools...)
	}
	merged = append(merged, collectDiscoveredTools(req["input"])...)
	return merged
}

func collectDiscoveredTools(input interface{}) []interface{} {
	items, ok := input.([]interface{})
	if !ok {
		return nil
	}
	var tools []interface{}
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch itemType, _ := item["type"].(string); itemType {
		case "tool_search_output", "tool_search_call_output", "web_search_output", "web_search_call_output":
			if discovered, ok := item["tools"].([]interface{}); ok {
				tools = append(tools, discovered...)
			}
		}
	}
	return tools
}

// buildFunctionTool 构造单个标准 function tool，兼容 flat 与嵌套 function 结构。
func buildFunctionTool(toolMap map[string]interface{}) interface{} {
	name := getStringValue(toolMap, "name")
	description := getStringValue(toolMap, "description")
	params := getObjectValue(toolMap, "parameters")
	if fn, ok := toolMap["function"].(map[string]interface{}); ok {
		if name == "" {
			name = getStringValue(fn, "name")
		}
		if description == "" {
			description = getStringValue(fn, "description")
		}
		if params == nil {
			params = getObjectValue(fn, "parameters")
		}
	}
	if name == "" {
		return nil
	}
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": description,
			"parameters":  normalizeParameters(params),
		},
	}
}

// customToolInputParameters 返回 custom 工具的通用 parameters 模式（input 字符串透传）。
func customToolInputParameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"input": map[string]interface{}{
				"type":        "string",
				"description": "raw tool input",
			},
		},
		"required": []interface{}{"input"},
	}
}

// flattenCustomTool 把 type:custom 工具扁平化为 function 工具，apply_patch 额外注册 5 个代理子工具。
func flattenCustomTool(tool map[string]interface{}) []interface{} {
	name := getStringValue(tool, "name")
	if name == "" {
		return nil
	}
	description := getStringValue(tool, "description")
	if description == "" {
		description = "Codex custom tool"
	}
	mainTool := map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": description,
			"parameters":  customToolInputParameters(),
		},
	}
	if isApplyPatchCustomTool(tool) {
		return append([]interface{}{mainTool}, applyPatchProxyTools(name)...)
	}
	return []interface{}{mainTool}
}

func isApplyPatchCustomTool(tool map[string]interface{}) bool {
	kind, _ := detectCodexCustomToolKind(tool)
	return kind == CodexCustomToolApplyPatch
}

// applyPatchProxyTools 生成 apply_patch 的 5 个代理子工具，参数 schema 与 applyPatchInputFromParsedArgs 对齐。
func applyPatchProxyTools(baseName string) []interface{} {
	stringProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	hunkItems := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"context": stringProp("hunk context line"),
			"lines": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"op":   stringProp("line op: context|add|remove"),
						"text": stringProp("line text"),
					},
				},
			},
		},
	}
	operationItem := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"type":    stringProp("operation type"),
			"path":    stringProp("file path"),
			"move_to": stringProp("rename target path"),
			"content": stringProp("file content"),
			"hunks": map[string]interface{}{
				"type":  "array",
				"items": hunkItems,
			},
		},
	}
	specs := []struct {
		suffix string
		params map[string]interface{}
	}{
		{"_add_file", map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    stringProp("target file path"),
				"content": stringProp("new file content"),
			},
			"required": []interface{}{"path", "content"},
		}},
		{"_delete_file", map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": stringProp("target file path"),
			},
			"required": []interface{}{"path"},
		}},
		{"_update_file", map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    stringProp("target file path"),
				"move_to": stringProp("rename target path"),
				"hunks": map[string]interface{}{
					"type":        "array",
					"description": "patch hunks",
					"items":       hunkItems,
				},
			},
			"required": []interface{}{"path", "hunks"},
		}},
		{"_replace_file", map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path":    stringProp("target file path"),
				"content": stringProp("replacement content"),
			},
			"required": []interface{}{"path", "content"},
		}},
		{"_batch", map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"operations": map[string]interface{}{
					"type":        "array",
					"description": "batch patch operations",
					"items":       operationItem,
				},
			},
			"required": []interface{}{"operations"},
		}},
	}
	out := make([]interface{}, 0, len(specs))
	for _, s := range specs {
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        baseName + s.suffix,
				"description": "apply_patch " + strings.TrimPrefix(s.suffix, "_") + " proxy",
				"parameters":  s.params,
			},
		})
	}
	return out
}

// flattenNamespaceTool 把 type:namespace 工具的 child function 全部扁平化为顶层 function 工具。
func flattenNamespaceTool(tool map[string]interface{}) []interface{} {
	namespace := getStringValue(tool, "name")
	children, _ := tool["tools"].([]interface{})
	var out []interface{}
	for _, raw := range children {
		child, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if childType, _ := child["type"].(string); childType != "function" {
			continue
		}
		childName := getStringValue(child, "name")
		if childName == "" {
			continue
		}
		flatName := flattenNamespaceToolName(namespace, childName)
		description := getStringValue(child, "description")
		params := getObjectValue(child, "parameters")
		if fn, ok := child["function"].(map[string]interface{}); ok {
			if description == "" {
				description = getStringValue(fn, "description")
			}
			if params == nil {
				params = getObjectValue(fn, "parameters")
			}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        flatName,
				"description": description,
				"parameters":  normalizeParameters(params),
			},
		})
	}
	return out
}

// flattenBuiltinTool 把 web_search / local_shell / computer_use 统一扁平化为 function 工具。
func flattenBuiltinTool(toolType string, tool map[string]interface{}) []interface{} {
	if toolType == "tool_search" {
		return flattenToolSearchTool(tool)
	}
	name := getStringValue(tool, "name")
	if name == "" {
		name = toolType
	}
	return []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": "built-in tool",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"input": map[string]interface{}{"type": "string"},
					},
					"required": []interface{}{"input"},
				},
			},
		},
	}
}

func flattenToolSearchTool(tool map[string]interface{}) []interface{} {
	name := getStringValue(tool, "name")
	description := getStringValue(tool, "description")
	params := getObjectValue(tool, "parameters")
	if fn, ok := tool["function"].(map[string]interface{}); ok {
		if name == "" {
			name = getStringValue(fn, "name")
		}
		if description == "" {
			description = getStringValue(fn, "description")
		}
		if params == nil {
			params = getObjectValue(fn, "parameters")
		}
	}
	if name == "" {
		name = "tool_search"
	}
	if description == "" {
		description = "Deferred tool metadata discovery, including multi-agent/subagent tools."
	}
	if params == nil {
		params = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "tool metadata search query",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "maximum number of tools to return",
				},
			},
			"required": []interface{}{"query"},
		}
	}
	return []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": description,
				"parameters":  normalizeParameters(params),
			},
		},
	}
}

// ConvertChatResponseToResponses 把上游 chat 响应转回 codex Responses 格式。
// 保留为 3 参数旧入口，等价于 ConvertChatResponseToResponsesWithContext(..., nil)。
func ConvertChatResponseToResponses(chatResponseBody []byte, model string, fallbackReasoningToMessage bool) []byte {
	return ConvertChatResponseToResponsesWithContext(chatResponseBody, model, fallbackReasoningToMessage, nil)
}

// ConvertChatResponseToResponsesWithContext 把上游 chat 响应转回 codex Responses 格式。
// 当 originalRequestRawJSON 非 nil 时，从原始 Responses 请求里解析 CodexToolContext，
// 用于在 tool_calls 还原时识别 namespace 字段与 custom_tool_call 类型。
// 当 originalRequestRawJSON 为 nil 时，退化到旧 3 参数行为（保持 100% 向后兼容）。
func ConvertChatResponseToResponsesWithContext(chatResponseBody []byte, model string, fallbackReasoningToMessage bool, originalRequestRawJSON []byte) []byte {
	var chatResp map[string]interface{}
	if err := json.Unmarshal(chatResponseBody, &chatResp); err != nil {
		return chatResponseBody
	}

	responsesResp := map[string]interface{}{
		"output": []interface{}{},
		"status": "completed",
	}

	if id, ok := chatResp["id"].(string); ok {
		responsesResp["id"] = id
	}
	if created, ok := chatResp["created"].(float64); ok {
		responsesResp["created"] = int64(created)
	}
	if model != "" {
		responsesResp["model"] = model
	} else if m, ok := chatResp["model"].(string); ok {
		responsesResp["model"] = m
	}

	// 构建 CodexCtx：仅当调用方提供原始请求时才构建；nil 时走旧行为
	var codexCtx *CodexToolContext
	if originalRequestRawJSON != nil {
		ctx := buildCodexToolContextFromRequest(originalRequestRawJSON)
		codexCtx = &ctx
	}

	if choices, ok := chatResp["choices"].([]interface{}); ok {
		for _, choice := range choices {
			if choiceMap, ok := choice.(map[string]interface{}); ok {
				if message, ok := choiceMap["message"].(map[string]interface{}); ok {
					output := convertChatMessageToOutput(message, codexCtx)
					if outputs, ok := responsesResp["output"].([]interface{}); ok {
						responsesResp["output"] = append(outputs, output...)
					}
				}
			}
		}
	}

	// 兜底：output 中无 message item，但有 reasoning 时，复制第一个 reasoning 的 summary 文本为 message
	if fallbackReasoningToMessage {
		if outputs, ok := responsesResp["output"].([]interface{}); ok {
			hasMessage := false
			var firstReasoningText string
			for _, o := range outputs {
				if om, ok := o.(map[string]interface{}); ok {
					if t, _ := om["type"].(string); t == "message" {
						hasMessage = true
						break
					} else if t == "reasoning" && firstReasoningText == "" {
						if summary, ok := om["summary"].([]interface{}); ok && len(summary) > 0 {
							if s, ok := summary[0].(map[string]interface{}); ok {
								firstReasoningText, _ = s["text"].(string)
							}
						}
					}
				}
			}
			if !hasMessage && firstReasoningText != "" {
				responsesResp["output"] = append(outputs, map[string]interface{}{
					"type":    "message",
					"role":    "assistant",
					"content": []interface{}{map[string]interface{}{"type": "output_text", "text": firstReasoningText}},
				})
			}
		}
	}

	if usage, ok := chatResp["usage"].(map[string]interface{}); ok {
		responsesResp["usage"] = parseUsage(usage)
	}

	result, _ := json.Marshal(responsesResp)
	return result
}

func convertChatMessageToOutput(message map[string]interface{}, codexCtx *CodexToolContext) []interface{} {
	var output []interface{}

	// 处理 reasoning_content
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		output = append(output, map[string]interface{}{
			"type":    "reasoning",
			"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": reasoning}},
		})
	}

	// 处理 content，同时提取 <think> 标签
	if content, ok := message["content"]; ok && content != nil {
		// 先尝试提取 thinking 内容
		thinking, remainingContent := extractThinkingFromContent(content)
		if thinking != "" {
			output = append(output, map[string]interface{}{
				"type":    "reasoning",
				"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": thinking}},
			})
		}

		// 处理剩余的文本内容
		if remainingContent != nil {
			switch v := remainingContent.(type) {
			case string:
				if v != "" {
					output = append(output, map[string]interface{}{
						"type": "message",
						"role": "assistant",
						"content": []interface{}{
							map[string]interface{}{
								"type": "output_text",
								"text": v,
							},
						},
					})
				}
			case []interface{}:
				// 如果是数组，检查是否有实际内容
				hasContent := false
				for _, block := range v {
					if blockMap, ok := block.(map[string]interface{}); ok {
						blockType, _ := blockMap["type"].(string)
						if (blockType == "text" || blockType == "output_text") &&
							getStringValue(blockMap, "text") != "" {
							hasContent = true
							break
						}
					}
				}
				if hasContent {
					output = append(output, map[string]interface{}{
						"type":    "message",
						"role":    "assistant",
						"content": v,
					})
				}
			}
		}
	}

	// 处理 tool_calls
	if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
		for _, tc := range toolCalls {
			if tcMap, ok := tc.(map[string]interface{}); ok {
				fnVal := getObjectValue(tcMap, "function")
				fnMap, ok := fnVal.(map[string]interface{})
				if !ok {
					continue
				}
				rawName := getStringValue(fnMap, "name")
				callID := getStringValue(tcMap, "id")
				rawArgs := getStringValue(fnMap, "arguments")

				// CodexCtx 路径：识别 custom proxy 还原 custom_tool_call，其他还原 namespace 字段
				if codexCtx != nil && codexCtx.IsCustomToolProxy(rawName) {
					customInput := reconstructCustomToolCallInput(*codexCtx, rawName, rawArgs)
					originalName := codexCtx.OriginalCustomToolName(rawName)
					output = append(output, map[string]interface{}{
						"type":    "custom_tool_call",
						"id":      "ctc_" + callID,
						"call_id": callID,
						"name":    originalName,
						"input":   customInput,
						"status":  "completed",
					})
					continue
				}

				item := map[string]interface{}{
					"type":      "function_call",
					"call_id":   callID,
					"arguments": rawArgs,
				}
				if codexCtx != nil {
					if codexCtx.IsBuiltinTool(rawName, "tool_search") || rawName == "tool_search" {
						output = append(output, map[string]interface{}{
							"type":      "tool_search_call",
							"id":        "ts_" + callID,
							"call_id":   callID,
							"name":      rawName,
							"arguments": rawArgs,
							"status":    "completed",
						})
						continue
					}
					if codexCtx.IsBuiltinTool(rawName, "web_search") || rawName == "web_search" {
						output = append(output, map[string]interface{}{
							"type":      "web_search_call",
							"id":        "ws_" + callID,
							"call_id":   callID,
							"name":      rawName,
							"arguments": rawArgs,
							"status":    "completed",
						})
						continue
					}
					displayName, namespace := codexCtx.OpenAINameForFunctionTool(rawName)
					item["name"] = displayName
					if namespace != "" {
						item["namespace"] = namespace
					}
					item["id"] = "fc_" + callID
					item["status"] = "completed"
				} else {
					item["name"] = rawName
				}
				output = append(output, item)
			}
		}
	}

	return output
}

// parseUsage 解析 usage 字段，支持多种格式（OpenAI、Claude、Gemini）
func parseUsage(usage map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	var inputTokens, outputTokens, totalTokens int
	var cachedTokens int
	var hasCacheDetails bool

	// 检查 Claude 格式（优先级最高）
	if _, has := usage["input_tokens"]; has {
		inputTokens = getIntValue(usage, "input_tokens")
	} else if _, has := usage["prompt_tokens"]; has {
		// OpenAI 格式
		inputTokens = getIntValue(usage, "prompt_tokens")
	} else if _, has := usage["promptTokenCount"]; has {
		// Gemini 格式
		inputTokens = getIntValue(usage, "promptTokenCount")
	}

	if _, has := usage["output_tokens"]; has {
		outputTokens = getIntValue(usage, "output_tokens")
	} else if _, has := usage["completion_tokens"]; has {
		outputTokens = getIntValue(usage, "completion_tokens")
	} else if _, has := usage["candidatesTokenCount"]; has {
		outputTokens = getIntValue(usage, "candidatesTokenCount")
	}

	if _, has := usage["total_tokens"]; has {
		totalTokens = getIntValue(usage, "total_tokens")
	} else {
		totalTokens = inputTokens + outputTokens
	}

	// 基础字段
	result["input_tokens"] = inputTokens
	result["output_tokens"] = outputTokens
	result["total_tokens"] = totalTokens

	// 处理缓存相关字段
	if v, has := usage["prompt_tokens_details"]; has {
		if details, ok := v.(map[string]interface{}); ok {
			if _, has := details["cached_tokens"]; has {
				cachedTokens = getIntValue(details, "cached_tokens")
				hasCacheDetails = true
			}
		}
	}

	if v, has := usage["input_tokens_details"]; has {
		if details, ok := v.(map[string]interface{}); ok {
			if _, has := details["cached_tokens"]; has {
				cachedTokens = getIntValue(details, "cached_tokens")
				hasCacheDetails = true
			}
		}
	}

	if _, has := usage["cache_read_input_tokens"]; has {
		cachedTokens = getIntValue(usage, "cache_read_input_tokens")
		hasCacheDetails = true
		result["cache_read_input_tokens"] = cachedTokens
	}

	// Claude 缓存创建字段
	if val, has := usage["cache_creation_input_tokens"]; has {
		result["cache_creation_input_tokens"] = val
	}
	if val, has := usage["cache_creation_5m_input_tokens"]; has {
		result["cache_creation_5m_input_tokens"] = val
	}
	if val, has := usage["cache_creation_1h_input_tokens"]; has {
		result["cache_creation_1h_input_tokens"] = val
	}
	if val, has := usage["cache_ttl"]; has {
		result["cache_ttl"] = val
	}

	// Gemini 缓存字段
	if _, has := usage["cachedContentTokenCount"]; has {
		cachedTokens = getIntValue(usage, "cachedContentTokenCount")
		hasCacheDetails = true
		// Gemini 的 promptTokenCount 已经包含了 cachedContentTokenCount，需要扣除
		if _, has := usage["promptTokenCount"]; has {
			actualInput := inputTokens - cachedTokens
			if actualInput < 0 {
				actualInput = 0
			}
			result["input_tokens"] = actualInput
			result["cache_read_input_tokens"] = cachedTokens
			result["total_tokens"] = actualInput + outputTokens
		}
	}

	// 添加 input_tokens_details（兼容 OpenAI 格式）
	if hasCacheDetails && cachedTokens > 0 {
		result["input_tokens_details"] = map[string]interface{}{
			"cached_tokens": cachedTokens,
		}
	}

	// 处理 output_tokens_details
	if v, has := usage["completion_tokens_details"]; has {
		result["output_tokens_details"] = v
	} else if v, has := usage["output_tokens_details"]; has {
		result["output_tokens_details"] = v
	}

	return result
}
