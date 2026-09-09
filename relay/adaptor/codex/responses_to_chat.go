package codex

import (
	"encoding/json"
	"fmt"
	"strings"

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
			chatReq["tool_choice"] = convertToolChoice(v)
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
				// auto 不是上游 chat 端点的合法枚举（如 DeepSeek: none/minimal/low/medium/high/xhigh/max），
				// 映射到 high（各上游普遍支持的默认档位）而非透传 auto 导致 400。
				chatReq["reasoning_effort"] = "high"
			case "minimal":
				// chat 协议 §2.2 reasoning_effort 枚举含 minimal（none/minimal/low/medium/high/xhigh/max），直通
				chatReq["reasoning_effort"] = "minimal"
			case "low":
				chatReq["reasoning_effort"] = "low"
			case "medium":
				chatReq["reasoning_effort"] = "medium"
			case "high":
				chatReq["reasoning_effort"] = "high"
			case "xhigh":
				chatReq["reasoning_effort"] = "xhigh"
			case "max":
				chatReq["reasoning_effort"] = "max"
			default:
				// 未识别值同样兜底 high（原先兜底 auto 会导致 400，Codex 默认 effort=max 曾落入此分支）
				chatReq["reasoning_effort"] = "high"
			}
		}
	}

	// text.format → chat response_format（responses §2 text 对象 → chat §2.4 response_format）：
	// json_schema 按 chat 协议嵌套到 json_schema 子键（name/description/schema/strict 等直通，type 除外）；
	// text / json_object / 未识别类型原样直通（chat 侧同样按 type 判别）。
	if text, ok := req["text"].(map[string]interface{}); ok {
		if format, ok := text["format"].(map[string]interface{}); ok {
			if formatType, _ := format["type"].(string); formatType == "json_schema" {
				inner := make(map[string]interface{}, len(format)-1)
				for k, v := range format {
					if k != "type" {
						inner[k] = v
					}
				}
				chatReq["response_format"] = map[string]interface{}{
					"type":        "json_schema",
					"json_schema": inner,
				}
			} else {
				chatReq["response_format"] = format
			}
		}
		// text.verbosity（responses §2 text 对象内嵌）→ chat §2.2 顶层 verbosity
		if verbosity, ok := text["verbosity"].(string); ok && verbosity != "" {
			chatReq["verbosity"] = verbosity
		}
	}

	// 顶层字段直通（responses §2 → chat §2 同名字段，保持请求原值）：
	// store/metadata/modalities/service_tier/moderation 见 chat §2.2；prompt_cache_key/prompt_cache_options/safety_identifier 见 chat §2.4。
	// service_tier 枚举差异：responses §2 含 ultrafast 而 chat §2.2 枚举不含，仍原值直通（是否支持由上游裁决，不臆造映射）。
	// moderation 结构两份文档均未展开，原值直通（结构不兼容时上游会显式报错，优于静默丢弃）。
	// 无 chat 对应、维持丢弃的字段：previous_response_id/include/background/max_tool_calls/prompt/truncation/conversation/context_management。
	for _, key := range []string{
		"store", "metadata", "modalities", "service_tier", "moderation",
		"prompt_cache_key", "prompt_cache_options", "safety_identifier",
	} {
		if v, ok := req[key]; ok && v != nil {
			chatReq[key] = v
		}
	}

	result, _ := json.Marshal(chatReq)
	return result
}

// convertToolChoice 把 responses 协议 tool_choice 转成 chat 协议形状（responses §2 → chat §5.2）：
// "auto"/"none"/"required" 字符串两协议同形，直通；
// responses 的 {"type":"function","name":"xxx"} 需嵌套为 {"type":"function","function":{"name":"xxx"}}；
// "指定 tool id" 字符串形态（responses §2）：function:<name> 可映射为 chat function 强制调用形状，
// 其余字符串（指向 custom/mcp/web_search 等工具）chat §5.2 无对应表达 → 显式降级 auto + Warn（与本文件
// reasoning_effort 未识别值兜底同一处置模式），禁止静默透传上游必然 400 的非法形状；
// 其他对象形式（custom/allowed_tools 等）默认已是 chat 形状，原样直通。
func convertToolChoice(v interface{}) interface{} {
	if s, ok := v.(string); ok {
		switch s {
		case "auto", "none", "required":
			return v
		}
		if name := strings.TrimPrefix(s, "function:"); name != s && name != "" {
			return map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": name,
				},
			}
		}
		logger.Log.Warnf("downgrade responses tool_choice string with no chat tool_choice equivalent to auto: %q", s)
		return "auto"
	}
	if m, ok := v.(map[string]interface{}); ok {
		if t, _ := m["type"].(string); t == "function" {
			if name, ok := m["name"].(string); ok && name != "" {
				return map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}
			}
		}
	}
	return v
}

func convertInputToMessages(input interface{}) []interface{} {
	var messages []interface{}
	// 本次转换（= 单次请求）内的 builtin 工具 fallback call_id 配对状态，见 builtinFallbackCallIDPairer 注释
	pairer := newBuiltinFallbackCallIDPairer()

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": v,
		})
	case []interface{}:
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if msg := convertInputItem(itemMap, pairer); msg != nil {
					messages = append(messages, msg)
				}
			}
		}
	}

	return messages
}

func convertInputItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
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
	case "agent_message":
		return convertAgentMessageItem(item)
	case "reasoning":
		return convertReasoningItem(item)
	case "tool_search_call":
		return convertToolSearchCallItem(item, pairer)
	case "tool_search_call_output", "tool_search_output":
		return convertToolSearchCallOutputItem(item, pairer)
	case "web_search_call":
		return convertWebSearchCallItem(item, pairer)
	case "web_search_call_output", "web_search_output":
		return convertWebSearchCallOutputItem(item, pairer)
	case "file_search_call", "computer_call", "computer_call_output",
		"local_shell_call", "local_shell_call_output",
		"shell_call", "shell_call_output",
		"apply_patch_call", "apply_patch_call_output",
		"code_interpreter_call", "image_generation_call",
		"mcp_list_tools", "mcp_approval_request", "mcp_approval_response", "mcp_call",
		"additional_tools", "configuration_update",
		"compaction", "compaction_trigger", "item_reference",
		"program", "program_output":
		// responses §3.8 已知但不可映射的 item 类型：chat 协议 §3 消息仅
		// system/developer/user/assistant/tool/function 六种角色，调用型/会话管理型/MCP 链路 item 均无对应表达。
		// 已知不可映射 → Info 记录后丢弃，不 Errorf 误报（codex 历史上下文带回这些 item 属正常情况）。
		logger.Log.Infof("drop known unmappable codex input item type (no chat message equivalent): %q", itemType)
		return nil
	default:
		logger.Log.Errorf("unknown codex input item type: %q", itemType)
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

func convertAgentMessageItem(item map[string]interface{}) map[string]interface{} {
	content, _ := item["content"].([]interface{})
	var parts []string
	for _, raw := range content {
		block, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if blockType, _ := block["type"].(string); blockType != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]interface{}{
		"role":    "assistant",
		"content": strings.Join(parts, "\n"),
	}
}

// builtinFallbackCallIDPairer 为缺 call_id 的 builtin 工具 call/output item（tool_search/web_search）
// 提供确定性 fallback call_id 配对。chat 协议要求 assistant.tool_calls[].id 与 role:tool 的 tool_call_id
// 精确配对，旧实现两侧各自 UnixNano 随机生成永远配不上（上游必然 400）。此处改为 call 生成确定性 id
// 并按类型压栈登记，同类型 output 缺 call_id 时取栈顶（= 该类型最近一个未配对 id）复用并出栈；
// 用栈而非单槽是为兼容并行调用形状（call,call,out,out），避免产生无 call 可配的孤儿 tool 消息。
// 状态仅存活于单次 convertInputToMessages 调用（单请求转换），禁止跨请求残留。
type builtinFallbackCallIDPairer struct {
	counters map[string]int
	pending  map[string][]string
}

func newBuiltinFallbackCallIDPairer() *builtinFallbackCallIDPairer {
	return &builtinFallbackCallIDPairer{
		counters: make(map[string]int),
		pending:  make(map[string][]string),
	}
}

// nextCallID 生成确定性 fallback id（前缀+"fb"+递增计数）并压入该前缀未配对栈顶。
func (p *builtinFallbackCallIDPairer) nextCallID(idPrefix string) string {
	id := p.standaloneCallID(idPrefix)
	p.pending[idPrefix] = append(p.pending[idPrefix], id)
	return id
}

// standaloneCallID 只生成确定性 id、不登记配对：孤立 output 用之，避免被后续 output 错配。
func (p *builtinFallbackCallIDPairer) standaloneCallID(idPrefix string) string {
	p.counters[idPrefix]++
	return idPrefix + "fb" + fmt.Sprintf("%d", p.counters[idPrefix])
}

// takePending 弹出该前缀最近一个未配对的 fallback call id（栈顶）。
func (p *builtinFallbackCallIDPairer) takePending(idPrefix string) (string, bool) {
	stack := p.pending[idPrefix]
	if len(stack) == 0 {
		return "", false
	}
	id := stack[len(stack)-1]
	stack = stack[:len(stack)-1]
	if len(stack) == 0 {
		delete(p.pending, idPrefix)
	} else {
		p.pending[idPrefix] = stack
	}
	return id, true
}

func convertToolSearchCallItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	return convertBuiltinToolCallItem(item, "tool_search", "ts_", pairer)
}

func convertToolSearchCallOutputItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	return convertBuiltinToolCallOutputItem(item, "ts_", pairer)
}

func convertWebSearchCallItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	return convertBuiltinToolCallItem(item, "web_search", "ws_", pairer)
}

func convertWebSearchCallOutputItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	return convertBuiltinToolCallOutputItem(item, "ws_", pairer)
}

// convertBuiltinToolCallItem 转换 builtin 工具 call item。
// 契约：pairer 由 convertInputToMessages 构造并沿调用链透传，必须非 nil——call/output 共享同一实例才能配对。
func convertBuiltinToolCallItem(item map[string]interface{}, name, idPrefix string, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID = pairer.nextCallID(idPrefix)
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

// convertBuiltinToolCallOutputItem 转换 builtin 工具 output item。
// 契约：pairer 非 nil（与 convertBuiltinToolCallItem 同一实例，见其注释）。
func convertBuiltinToolCallOutputItem(item map[string]interface{}, idPrefix string, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		if pending, ok := pairer.takePending(idPrefix); ok {
			callID = pending
		} else {
			// 孤立 output：无先行未配对 fallback call 可复用，生成独立确定性 id 并显式告警
			callID = pairer.standaloneCallID(idPrefix)
			logger.Log.Warnf("builtin tool output has no preceding fallback call to pair with, emitted unpaired id: %q", callID)
		}
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

	// chat §3.1 消息联合已含 developer 类型（system/developer/user/assistant/tool/function），
	// responses §3.1 message.role 的 developer 原样直通，不再降级为 system（内容保真）。
	// 个别旧上游模型不识别 developer role 属上游能力差异，由上游裁决，转换层不预判。

	message := map[string]interface{}{
		"role":    role,
		"content": "",
	}

	if content, ok := item["content"]; ok {
		switch v := content.(type) {
		case string:
			message["content"] = v
		case []interface{}:
			// role 传入供 refusal part 按 chat §4 消息×部件矩阵校验（refusal 仅 assistant）
			message["content"] = convertContentArray(v, role)
		}
	}

	return message
}

func convertContentArray(content []interface{}, role string) interface{} {
	var textParts []string
	var hasMedia bool
	var hasRefusal bool
	chatContent := []interface{}{}

	for _, block := range content {
		if blockMap, ok := block.(map[string]interface{}); ok {
			blockType, _ := blockMap["type"].(string)
			if blockType == "" {
				blockType = "input_text"
			}

			switch blockType {
			case "input_text", "output_text", "text":
				if hasRefusal {
					// refusal 已先行（[refusal, text] 乱序），chat §4 互斥：丢弃后到的 text
					logger.Log.Infof("drop text part after refusal part in assistant message (chat §4 mutual exclusion)")
					continue
				}
				if text, ok := blockMap["text"].(string); ok && text != "" {
					textParts = append(textParts, text)
					chatContent = append(chatContent, map[string]interface{}{
						"type": "text",
						"text": text,
					})
				}
			case "input_image", "image_url":
				if hasRefusal {
					// refusal 已先行（[refusal, media] 乱序），chat §4 互斥：丢弃后到的 image
					logger.Log.Infof("drop image part after refusal part in assistant message (chat §4 mutual exclusion)")
					continue
				}
				if imgBlock := convertImageBlock(blockMap); imgBlock != nil {
					chatContent = append(chatContent, imgBlock)
					hasMedia = true
				}
			case "input_audio":
				if hasRefusal {
					// refusal 已先行（[refusal, media] 乱序），chat §4 互斥：丢弃后到的 audio
					logger.Log.Infof("drop audio part after refusal part in assistant message (chat §4 mutual exclusion)")
					continue
				}
				if audioBlock := convertInputAudioBlock(blockMap); audioBlock != nil {
					chatContent = append(chatContent, audioBlock)
					hasMedia = true
				}
			case "input_file":
				if hasRefusal {
					// refusal 已先行（[refusal, media] 乱序），chat §4 互斥：丢弃后到的 file
					logger.Log.Infof("drop file part after refusal part in assistant message (chat §4 mutual exclusion)")
					continue
				}
				if fileBlock := convertInputFileBlock(blockMap); fileBlock != nil {
					chatContent = append(chatContent, fileBlock)
					hasMedia = true
				}
			case "refusal":
				// responses §5 refusal part → chat content part。chat §4 矩阵：refusal 仅 assistant 消息可用，
				// 且与 text 互斥（恰一个）。
				refusalText, ok := blockMap["refusal"].(string)
				if !ok || refusalText == "" {
					continue
				}
				if role != "assistant" {
					logger.Log.Infof("drop refusal part for non-assistant message role: %q", role)
					continue
				}
				// assistant 消息已含 text 等 part 时按互斥规则取舍：保留 refusal 丢弃已收集的
				// text/media（refusal 是该消息的语义终态），不把两者同时放入（chat §4 矩阵）。
				if len(chatContent) > 0 {
					logger.Log.Infof("drop coexisting text/media parts in assistant message with refusal part (chat §4 mutual exclusion)")
					chatContent = []interface{}{}
					textParts = nil
					hasMedia = false
				}
				hasRefusal = true
				chatContent = append(chatContent, map[string]interface{}{
					"type":    "refusal",
					"refusal": refusalText,
				})
			default:
				// 未知 content block type 无 chat §4 部件对应：Warn 后跳过，静默丢弃改显式说明
				logger.Log.Warnf("drop unknown codex content block type (no chat content part equivalent): %q", blockType)
			}
		}
	}

	if hasMedia || hasRefusal {
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

// convertInputAudioBlock 把 responses input_audio part（responses §3.1）转为 chat input_audio part（chat §4）：
// input_audio 子对象形状（data/format）直通；顶层 data/format 形状组装为子对象；缺 data 或 format 则丢弃。
// 流式差异：chat 协议文档（§4 部件矩阵按消息角色约束）未对流式请求限制 input_audio，
// 转换层不做流式/非流式区分；流式不支持属上游模型执行层限制。
func convertInputAudioBlock(block map[string]interface{}) interface{} {
	if inner, ok := block["input_audio"].(map[string]interface{}); ok {
		data, _ := inner["data"].(string)
		format, _ := inner["format"].(string)
		if data == "" || format == "" {
			return nil
		}
		return map[string]interface{}{
			"type":        "input_audio",
			"input_audio": inner,
		}
	}
	data, _ := block["data"].(string)
	format, _ := block["format"].(string)
	if data == "" || format == "" {
		return nil
	}
	return map[string]interface{}{
		"type": "input_audio",
		"input_audio": map[string]interface{}{
			"data":   data,
			"format": format,
		},
	}
}

// convertInputFileBlock 把 responses input_file part（responses §3.1）转为 chat file part（chat §4）：
// file 子对象形状直通；顶层 filename/file_data/file_id 形状组装为 file 子对象（三选一）；无有效载荷则丢弃。
func convertInputFileBlock(block map[string]interface{}) interface{} {
	if inner, ok := block["file"].(map[string]interface{}); ok && hasFilePayload(inner) {
		return map[string]interface{}{
			"type": "file",
			"file": inner,
		}
	}
	fileInner := make(map[string]interface{})
	for _, key := range []string{"filename", "file_data", "file_id"} {
		if v, ok := block[key]; ok && v != nil && v != "" {
			fileInner[key] = v
		}
	}
	if len(fileInner) == 0 {
		return nil
	}
	return map[string]interface{}{
		"type": "file",
		"file": fileInner,
	}
}

// hasFilePayload 判断 file 对象是否含 filename/file_data/file_id 任一有效载荷。
func hasFilePayload(inner map[string]interface{}) bool {
	for _, key := range []string{"filename", "file_data", "file_id"} {
		if v, ok := inner[key]; ok && v != nil && v != "" {
			return true
		}
	}
	return false
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
		case "web_search", "web_search_preview", "local_shell", "shell",
			"computer", "computer_use", "computer_use_preview", "tool_search":
			// responses §9 内建工具族：web_search_preview/computer_use_preview 为对应工具预览名，
			// shell 为 codex 函数 shell，均与既有 web_search/computer_use/local_shell 同族，统一扁平化为 function。
			appendUniqueChatTools(&result, seen, flattenBuiltinTool(toolType, toolMap))
		case "apply_patch":
			// responses §9 apply_patch 独立工具类型（codex 文件编辑）与 type:custom name:apply_patch 等价：
			// 统一按 custom 扁平化（主工具 + 5 个代理子工具），name 缺失时回退 "apply_patch"。
			name := getStringValue(toolMap, "name")
			if name == "" {
				name = "apply_patch"
			}
			apTool := map[string]interface{}{
				"type":        "custom",
				"name":        name,
				"description": getStringValue(toolMap, "description"),
			}
			appendUniqueChatTools(&result, seen, flattenCustomTool(apTool))
		default:
			// responses §9 其余工具（file_search/code_interpreter/image_generation/mcp/programmatic_tool_calling 等）
			// 在 chat §5.1 仅 function 类型、无对应表达：Info 记录后丢弃（不 Errorf 避免噪音，静默丢弃改显式说明）。
			logger.Log.Infof("drop unmappable codex tool type (no chat tool equivalent): %q", toolType)
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

	// status 按首个 choice 的 finish_reason 推导（chat §6.2 → responses §4）：
	// length → incomplete + reason=max_output_tokens；content_filter → incomplete + reason=content_filter；
	// 其余（stop/tool_calls 等）保持 completed。与流式路径（chat_to_responses.go）对称。
	status := "completed"
	var incompleteDetails map[string]interface{}
	if choices, ok := chatResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choiceMap, ok := choices[0].(map[string]interface{}); ok {
			if fr, ok := choiceMap["finish_reason"].(string); ok {
				switch fr {
				case "length":
					status = "incomplete"
					incompleteDetails = map[string]interface{}{"reason": "max_output_tokens"}
				case "content_filter":
					status = "incomplete"
					incompleteDetails = map[string]interface{}{"reason": "content_filter"}
				}
			}
		}
	}

	responsesResp := map[string]interface{}{
		"output": []interface{}{},
		"status": status,
	}
	if incompleteDetails != nil {
		responsesResp["incomplete_details"] = incompleteDetails
		// 与流式路径对齐（chat_to_responses.go）：incomplete 终态同时标记 truncated（responses §4）
		responsesResp["truncated"] = true
	}

	if id, ok := chatResp["id"].(string); ok {
		responsesResp["id"] = id
	}
	if created, ok := chatResp["created"].(float64); ok {
		// responses 协议 §4 主对象字段名为 created_at（与流式路径 response.created_at 保持一致），
		// chat 的 created（Unix 秒）直接映射，不做单位换算。
		responsesResp["created_at"] = int64(created)
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

	// 处理 refusal（chat §3.1/§6.1 message.refusal → responses §5 refusal content part）。
	// 有 content 时合并进同一 message item；无 content 时单独生成 refusal message item。
	refusalText, _ := message["refusal"].(string)

	// annotations（chat §6.1.3 message.annotations → responses §5 output_text part.annotations）：
	// 引用标注（url_citation 等）附加到文本 part；无文本 part 可附着时随消息丢弃（与缺失时行为一致）。
	annotations, _ := message["annotations"].([]interface{})
	hasAnnotations := len(annotations) > 0

	// 处理 content，同时提取  thinking 标签
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
				if v != "" || refusalText != "" {
					parts := []interface{}{}
					if v != "" {
						textPart := map[string]interface{}{
							"type": "output_text",
							"text": v,
						}
						if hasAnnotations {
							textPart["annotations"] = annotations
						}
						parts = append(parts, textPart)
					}
					if refusalText != "" {
						parts = append(parts, map[string]interface{}{
							"type":    "refusal",
							"refusal": refusalText,
						})
						refusalText = ""
					}
					output = append(output, map[string]interface{}{
						"type":    "message",
						"role":    "assistant",
						"content": parts,
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
				if hasContent || refusalText != "" {
					parts := v
					// annotations 附加到首个文本 part（已有 annotations 字段时不覆盖）
					if hasAnnotations {
						for _, block := range parts {
							if bm, ok := block.(map[string]interface{}); ok {
								if bt, _ := bm["type"].(string); (bt == "text" || bt == "output_text") && getStringValue(bm, "text") != "" {
									if _, exists := bm["annotations"]; !exists {
										bm["annotations"] = annotations
									}
									break
								}
							}
						}
					}
					if refusalText != "" {
						parts = append(parts, map[string]interface{}{
							"type":    "refusal",
							"refusal": refusalText,
						})
						refusalText = ""
					}
					output = append(output, map[string]interface{}{
						"type":    "message",
						"role":    "assistant",
						"content": parts,
					})
				}
			}
		}
	}

	// 无 content 时的纯 refusal message item
	if refusalText != "" {
		output = append(output, map[string]interface{}{
			"type": "message",
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type":    "refusal",
					"refusal": refusalText,
				},
			},
		})
	}

	// audio（chat §6.1.2 message.audio → responses output_audio item）：
	// chat §10 差异速查标注 responses 侧对应 output_audio item；audio 对象（id/expires_at/data/transcript）原样直通，
	// item id 复用 audio.id（缺失时不写 id 字段）。
	if audio, ok := message["audio"].(map[string]interface{}); ok {
		audioItem := map[string]interface{}{
			"type":         "output_audio",
			"output_audio": audio,
		}
		if audioID, ok := audio["id"].(string); ok && audioID != "" {
			audioItem["id"] = audioID
		}
		output = append(output, audioItem)
	}

	// 处理 tool_calls
	if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
		for _, tc := range toolCalls {
			if tcMap, ok := tc.(map[string]interface{}); ok {
				fnVal := getObjectValue(tcMap, "function")
				fnMap, ok := fnVal.(map[string]interface{})
				if !ok {
					// chat §6.1.1 除 function 外还定义 type:"custom" 变体（custom:{name*,input*}，
					// 字段形状以 openai-go ChatCompletionMessageCustomToolCall 为准），与 responses §5
					// custom_tool_call（id*,call_id*,name*,input*,status）字段一一对应，可无损映射；
					// 必填子键缺失视为上游畸形，Warn 后跳过，不产出畸形 item。
					if tcType, _ := tcMap["type"].(string); tcType == "custom" {
						customMap, _ := getObjectValue(tcMap, "custom").(map[string]interface{})
						customCallID := getStringValue(tcMap, "id")
						customName := getStringValue(customMap, "name")
						customInput, hasCustomInput := customMap["input"].(string)
						if customCallID == "" || customName == "" || !hasCustomInput {
							logger.Log.Warnf("drop malformed chat custom tool call in responses output (missing id/name/input)")
						} else {
							output = append(output, map[string]interface{}{
								"type":    "custom_tool_call",
								"id":      "ctc_" + customCallID,
								"call_id": customCallID,
								"name":    customName,
								"input":   customInput,
								"status":  "completed",
							})
						}
						continue
					}
					logger.Log.Warnf("drop chat tool call with no function/custom payload in responses output (chat §6.1.1): %q", getStringValue(tcMap, "type"))
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
						// execution:"client"：本中转为客户端侧代理执行，与流式路径输出对齐（docs §3.7）
						output = append(output, map[string]interface{}{
							"type":      "tool_search_call",
							"id":        "ts_" + callID,
							"call_id":   callID,
							"name":      rawName,
							"arguments": rawArgs,
							"execution": "client",
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
	var cacheWriteTokens int

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

	// 处理缓存相关字段。cache_write_tokens 语义对照 chat §8.1（写入缓存的 prompt token 数）
	// → responses §6 input_tokens_details.cache_write_tokens，prompt/input 两种源命名同读（与 cached_tokens 口径一致）。
	if v, has := usage["prompt_tokens_details"]; has {
		if details, ok := v.(map[string]interface{}); ok {
			if _, has := details["cached_tokens"]; has {
				cachedTokens = getIntValue(details, "cached_tokens")
			}
			if _, has := details["cache_write_tokens"]; has {
				cacheWriteTokens = getIntValue(details, "cache_write_tokens")
			}
		}
	}

	if v, has := usage["input_tokens_details"]; has {
		if details, ok := v.(map[string]interface{}); ok {
			if _, has := details["cached_tokens"]; has {
				cachedTokens = getIntValue(details, "cached_tokens")
			}
			if _, has := details["cache_write_tokens"]; has {
				cacheWriteTokens = getIntValue(details, "cache_write_tokens")
			}
		}
	}

	if _, has := usage["cache_read_input_tokens"]; has {
		cachedTokens = getIntValue(usage, "cache_read_input_tokens")
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

	// input_tokens_details 恒存在（responses §6 usage「全部必填」口径，与流式路径对称）：
	// 值为 0 也输出 cached_tokens/cache_write_tokens 子字段，不再条件性省略 details 对象。
	result["input_tokens_details"] = map[string]interface{}{
		"cached_tokens":      cachedTokens,
		"cache_write_tokens": cacheWriteTokens,
	}

	// output_tokens_details 恒存在：源 details（chat §8.2）透传，缺 reasoning_tokens 时补 0
	if v, has := usage["completion_tokens_details"]; has {
		result["output_tokens_details"] = normalizeOutputDetails(v)
	} else if v, has := usage["output_tokens_details"]; has {
		result["output_tokens_details"] = normalizeOutputDetails(v)
	} else {
		result["output_tokens_details"] = map[string]interface{}{"reasoning_tokens": 0}
	}

	return result
}

// normalizeOutputDetails 透传上游输出细分对象，兜底保证 reasoning_tokens 子字段恒存在。
func normalizeOutputDetails(v interface{}) interface{} {
	details, ok := v.(map[string]interface{})
	if !ok {
		return map[string]interface{}{"reasoning_tokens": 0}
	}
	if _, has := details["reasoning_tokens"]; !has {
		details["reasoning_tokens"] = 0
	}
	return details
}
