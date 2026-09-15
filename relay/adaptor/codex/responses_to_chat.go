package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pai801/myapi/common/logger"
	relaymodel "github.com/pai801/myapi/relay/model"
)

// errRequest/errResponse 构造共享协议错误（契约 §1.3）：请求侧方向 responses_request_to_chat，
// 响应侧方向 chat_response_to_responses；Code 只取稳定机器码。
func errRequest(code, path, message string) *relaymodel.ProtocolConversionError {
	var cause error
	if message != "" {
		cause = errors.New(message)
	}
	return &relaymodel.ProtocolConversionError{Code: code, Direction: relaymodel.DirectionResponsesRequestToChat, Path: path, Cause: cause}
}

func errResponse(code, path string, cause error) *relaymodel.ProtocolConversionError {
	return &relaymodel.ProtocolConversionError{Code: code, Direction: relaymodel.DirectionChatResponseToResponses, Path: path, Cause: cause}
}

// warnDropped 记录 responses→chat 转换中"能力性不可映射"字段/item 的丢弃。
// 策略：确实无法映射的字段/item 丢弃并继续转换，不再以 unsupported_mapping 400 拒绝整个请求。
func warnDropped(path, reason string) {
	logger.Log.Warnf("[responses→chat dropped] path=%s reason=%s", path, reason)
}

// infoDropped 记录良性降级字段的丢弃日志，格式与 warnDropped 一致但走 Info 级。
// 仅用于丢弃后不产生模型行为偏移的字段（如 include：只影响上游多回传字段），
// 避免 codex CLI 每请求恒带的 include=["reasoning.encrypted_content"] 刷屏；真隐患字段仍 warnDropped。
func infoDropped(path, reason string) {
	logger.Log.Infof("[responses→chat dropped] path=%s reason=%s", path, reason)
}

// withRequestPath 在 convertInputToMessages 已知数组下标时把泛化路径 "input" 细化为 "input[i]"。
func withRequestPath(err error, path string) error {
	pce, ok := err.(*relaymodel.ProtocolConversionError)
	if !ok {
		return err
	}
	if pce.Path == "" || pce.Path == "input" {
		return &relaymodel.ProtocolConversionError{Code: pce.Code, Direction: pce.Direction, Path: path, Cause: pce.Cause}
	}
	return err
}

// ConvertResponsesToChatRequest 把 Responses 请求转换为 Chat Completions 请求。
// 能力性不可映射的字段/item（responses 专有顶层字段、未知/上下文 item、仅 encrypted_content 的
// reasoning item、role×part 矩阵违规、DN-6 白名单外内置工具、DN-7 ultrafast 档位等）一律丢弃
// 对应字段/item 并记 warn 日志后继续转换，不再 unsupported_mapping 400 —— 目标是让
// responses→chat 转换完成，确实转不了的字段接受丢弃。
// 仅三类结构性/不可执行情形维持错误：非法 JSON（invalid_source_json）、source shape 非法
// （invalid_source_shape）、转换后 messages 为空（无法执行，仍 unsupported_mapping 400）。
func ConvertResponsesToChatRequest(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(inputRawJSON, &req); err != nil {
		return nil, errRequest(relaymodel.CodeInvalidSourceJSON, "", "responses request body is not valid JSON")
	}
	if req == nil {
		return nil, errRequest(relaymodel.CodeInvalidSourceShape, "", "responses request body must be a JSON object")
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

	// P2-1：max_output_tokens → max_completion_tokens（chat §2.1 推荐字段），不再写已弃用的 max_tokens
	if v, ok := req["max_output_tokens"].(float64); ok {
		chatReq["max_completion_tokens"] = int(v)
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

	// DN-7：responses §2 service_tier 含 ultrafast 而 chat §2.2 枚举不含，且该档位在本网关 Chat 上游
	// 无落地路径 —— 丢弃该字段并告警（不再 400），其余交集枚举原样透传。
	droppedUltrafastTier := false
	if tier, ok := req["service_tier"].(string); ok && tier == "ultrafast" {
		warnDropped("service_tier", `service_tier "ultrafast" is not supported by this gateway's Chat upstream; field dropped`)
		droppedUltrafastTier = true
	}

	// previous_response_id 指向网关侧不可解析的历史 response 上下文：无法映射到 chat，丢弃并告警
	//（不再拒绝整个请求）。
	if prev, ok := req["previous_response_id"].(string); ok && prev != "" {
		warnDropped("previous_response_id", "previous_response_id context cannot be resolved by this gateway; field dropped")
	}

	// include/background/max_tool_calls/prompt/truncation/conversation/context_management 均为
	// responses §2 合法专有字段，但本网关 Chat 上游无对应表达 —— 非缺省出现一律丢弃并告警，
	// 缺省/未携带不告警（无信息丢失）。字段本身不在下方透传白名单内，故"丢弃"即不写入 chat 请求。
	for _, key := range []string{
		"include", "background", "max_tool_calls", "prompt",
		"truncation", "conversation", "context_management",
	} {
		if v, ok := req[key]; ok && !responsesOnlyFieldIsDefault(key, v) {
			reason := fmt.Sprintf("responses field %q is not at its protocol default and cannot be expressed on this gateway's Chat upstream; field dropped", key)
			if key == "include" {
				// 日志降噪：include 只影响上游多回传字段，丢弃无模型行为偏移，按良性降级记 Info。
				infoDropped(key, reason)
				continue
			}
			warnDropped(key, reason)
		}
	}

	if raw, ok := req["instructions"]; ok {
		instructions, ok2 := raw.(string)
		if !ok2 {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "instructions", "instructions must be a string")
		}
		if instructions != "" {
			messages := chatReq["messages"].([]interface{})
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": instructions,
			})
			chatReq["messages"] = messages
		}
	}

	if input, ok := req["input"]; ok {
		messages := chatReq["messages"].([]interface{})
		inputMessages, err := convertInputToMessages(input)
		if err != nil {
			return nil, err
		}
		chatReq["messages"] = append(messages, inputMessages...)
	}

	// chat §2 要求 messages ≥1：解析不出任何消息（input 为空或全部无信息）时不得继续发送空上下文请求。
	// 注意：这不是能力性字段丢弃，而是请求根本无法执行（无任何可发送的对话上下文），故仍显式拒绝。
	if msgs, _ := chatReq["messages"].([]interface{}); len(msgs) == 0 {
		return nil, errRequest(relaymodel.CodeUnsupportedMapping, "input", "responses request resolves to an empty chat messages list")
	}

	mergedTools := mergeResponseTools(req)
	var survivingToolNames map[string]struct{}
	survivingToolsNonEmpty := false
	if len(mergedTools) > 0 {
		tools, err := convertToolsToOpenAI(mergedTools)
		if err != nil {
			return nil, err
		}
		// 即使全部工具被丢弃也记录（空集合）：tool_choice 不得指向已不存在的工具。
		survivingToolNames = collectChatToolNames(tools)
		if len(tools) > 0 {
			chatReq["tools"] = tools
			survivingToolsNonEmpty = true
		}
	}

	if v, ok := req["tool_choice"]; ok {
		switch {
		case survivingToolsNonEmpty:
			tc, err := convertToolChoice(v, survivingToolNames)
			if err != nil {
				return nil, err
			}
			// tc == nil 表示 tool_choice 形态在 chat 不可表达或其目标工具已被丢弃：
			// 丢弃整个 tool_choice（等效上游缺省 auto），不写入 chat 请求。
			if tc != nil {
				chatReq["tool_choice"] = tc
			}
		case len(mergedTools) > 0:
			// P1-2：声明了工具但转换后全部被丢弃（存活集为空）时，required/custom/allowed_tools
			// 必须绑定工具集，写入即悬空引用 —— 丢弃并告警；auto/none 等效上游缺省，可保留写出。
			// 未声明任何工具（mergedTools 为空）时沿用既有门槛：不写 tool_choice。
			if toolChoiceRequiresTools(v) {
				warnDropped("tool_choice", "no surviving tools to bind tool_choice; tool_choice dropped")
			} else {
				tc, err := convertToolChoice(v, survivingToolNames)
				if err != nil {
					return nil, err
				}
				if tc != nil {
					chatReq["tool_choice"] = tc
				}
			}
		}
	}

	if v, ok := req["parallel_tool_calls"].(bool); ok {
		// P2-1：parallel_tool_calls 仅在有存活工具时有语义；存活集为空时不写入，
		// 避免与缺失的 tools 形成悬空声明。
		if survivingToolsNonEmpty {
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
	// service_tier：ultrafast 已在上方按 DN-7 丢弃，其余交集枚举透传。
	// moderation 结构两份文档均未展开，原值直通（结构不兼容时上游会显式报错，优于静默丢弃）。
	// previous_response_id 与 include/background/max_tool_calls/prompt/truncation/conversation/
	// context_management 无 chat 对应：非缺省值已在上方按丢弃策略告警，字段本体不写入。
	for _, key := range []string{
		"store", "metadata", "modalities", "service_tier", "moderation",
		"prompt_cache_key", "prompt_cache_options", "safety_identifier",
	} {
		if key == "service_tier" && droppedUltrafastTier {
			continue
		}
		if v, ok := req[key]; ok && v != nil {
			chatReq[key] = v
		}
	}

	result, err := json.Marshal(chatReq)
	if err != nil {
		return nil, errRequest(relaymodel.CodeInvalidSourceShape, "", "marshal converted chat request failed")
	}
	return result, nil
}

// responsesOnlyFieldIsDefault 判定 Responses §2 专有顶层字段（本网关 Chat 上游无表达）的取值
// 是否等同协议缺省；缺省形态无信息丢失、不告警，非缺省形态由调用方告警丢弃：
//   - truncation：§2 上下文截断配置，协议默认 "auto"（自动截断保窗口）。显式 "auto" 等同缺省；
//     其余形态（如 "disabled" 或对象配置）改变截断策略 → 非缺省。
//   - background：§2 后台处理配置，协议默认 false（前台执行）。显式 false 不改变执行模型，
//     按缺省处理；true/对象形态启用后台执行 → 非缺省。
//   - include/prompt：空数组/空串等同未携带（include 无额外返回字段、prompt 无提示词引用）。
//   - max_tool_calls/conversation/context_management：null 等同未携带；任何非 null 值
//     （含 max_tool_calls=0 的显式配置意图）均为非缺省。
func responsesOnlyFieldIsDefault(key string, v interface{}) bool {
	if v == nil {
		return true
	}
	switch key {
	case "truncation":
		s, ok := v.(string)
		return ok && s == "auto"
	case "background":
		b, ok := v.(bool)
		return ok && !b
	case "include":
		arr, ok := v.([]interface{})
		return ok && len(arr) == 0
	case "prompt":
		str, ok := v.(string)
		return ok && str == ""
	default: // max_tool_calls/conversation/context_management：非 nil 即显式配置
		return false
	}
}

// convertToolChoice 把 responses 协议 tool_choice 转成 chat 协议形状（responses §2 → chat §5.2）：
// "auto"/"none"/"required" 字符串两协议同形，直通；
// responses 的 {"type":"function","name":"xxx"} 需嵌套为 {"type":"function","function":{"name":"xxx"}}；
// 已是 chat 嵌套形状的 {type:function, function:{name}} 原样直通（幂等）；
// 形态在 chat §5.2 不可表达（tool id 字符串、未知对象 type、function 缺 name），或目标 function
// 工具已被 DN-6 丢弃时返回 (nil, nil) 表示丢弃整个 tool_choice（等效上游缺省 auto）并记 warn 日志。
// survivingToolNames 为已转换成功的 chat function 名集合；nil 表示不校验目标存在性。
// 仅 tool_choice 既非字符串也非对象时返回 invalid_source_shape。
func convertToolChoice(v interface{}, survivingToolNames map[string]struct{}) (interface{}, error) {
	if s, ok := v.(string); ok {
		switch s {
		case "auto", "none", "required":
			return v, nil
		}
		if name := strings.TrimPrefix(s, "function:"); name != s && name != "" {
			if !toolNameSurvives(survivingToolNames, name) {
				warnDropped("tool_choice", fmt.Sprintf("tool_choice targets dropped tool %q; tool_choice dropped", name))
				return nil, nil
			}
			return map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": name,
				},
			}, nil
		}
		warnDropped("tool_choice", fmt.Sprintf("tool_choice %q (tool id reference) has no chat §5.2 equivalent; tool_choice dropped", s))
		return nil, nil
	}
	if m, ok := v.(map[string]interface{}); ok {
		t, _ := m["type"].(string)
		switch t {
		case "function":
			if name, ok := m["name"].(string); ok && name != "" {
				if !toolNameSurvives(survivingToolNames, name) {
					warnDropped("tool_choice", fmt.Sprintf("tool_choice targets dropped tool %q; tool_choice dropped", name))
					return nil, nil
				}
				return map[string]interface{}{
					"type": "function",
					"function": map[string]interface{}{
						"name": name,
					},
				}, nil
			}
			if fn, ok := m["function"].(map[string]interface{}); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					if !toolNameSurvives(survivingToolNames, name) {
						warnDropped("tool_choice", fmt.Sprintf("tool_choice targets dropped tool %q; tool_choice dropped", name))
						return nil, nil
					}
					// 已是 chat 嵌套形状，原样直通
					return v, nil
				}
			}
			warnDropped("tool_choice", "function tool_choice missing required name; tool_choice dropped")
			return nil, nil
		case "custom", "allowed_tools":
			// chat §5.2 同形变体，直通
			return v, nil
		default:
			warnDropped("tool_choice", fmt.Sprintf("tool_choice type %q has no chat §5.2 equivalent; tool_choice dropped", t))
			return nil, nil
		}
	}
	return nil, errRequest(relaymodel.CodeInvalidSourceShape, "tool_choice", "tool_choice must be a string or an object")
}

// toolChoiceRequiresTools 判定 tool_choice 形态是否必须绑定现存工具集才可写入 chat 请求：
// "required"（强制调用某工具）与 custom/allowed_tools 对象都指向工具集合，无存活工具时为悬空引用。
// auto/none 及 function 目标形态不在此列：auto/none 等同上游缺省，function 目标由 convertToolChoice
// 按存活工具名集合校验并丢弃。
func toolChoiceRequiresTools(v interface{}) bool {
	if s, ok := v.(string); ok {
		return s == "required"
	}
	if m, ok := v.(map[string]interface{}); ok {
		t, _ := m["type"].(string)
		return t == "custom" || t == "allowed_tools"
	}
	return false
}

// toolNameSurvives 判定 tool_choice 指向的 function 名是否仍存在于已转换的 chat tools；
// survivingToolNames 为 nil 表示不校验（直接调用/无 tools 场景）。
func toolNameSurvives(survivingToolNames map[string]struct{}, name string) bool {
	if survivingToolNames == nil {
		return true
	}
	_, ok := survivingToolNames[name]
	return ok
}

// collectChatToolNames 收集 chat tools 的 function 名，供 tool_choice 目标存在性校验。
func collectChatToolNames(tools []interface{}) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := toolMap["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

func convertInputToMessages(input interface{}) ([]interface{}, error) {
	var messages []interface{}
	// 本次转换（= 单次请求）内的 call_id 配对状态，见 inputConversionState 注释
	state := newInputConversionState()

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": v,
		})
	case []interface{}:
		for i, item := range v {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, fmt.Sprintf("input[%d]", i), "input item must be a JSON object")
			}
			msg, err := convertInputItem(itemMap, state)
			if err != nil {
				return nil, withRequestPath(err, fmt.Sprintf("input[%d]", i))
			}
			if msg != nil {
				messages = append(messages, msg)
			}
		}
	default:
		// 顶层 input 为 object（ItemReference，responses §2/§3.8）等非 string/array 形态时，
		// 网关无法解析其引用的历史上下文 —— 丢弃并告警，不再拒绝整个请求。
		warnDropped("input", "top-level input object/ItemReference cannot be resolved by this gateway; input dropped")
	}

	// P1-5：同一阶段连续的 function call items 合并为一个 assistant message 的 tool_calls[]
	return mergeAdjacentFunctionCalls(messages)
}

// mergeAdjacentFunctionCalls 把相邻的纯函数调用 assistant 消息合并（responses §3.2 → chat §3.1/§6.1.1）：
// chat 协议要求同一输出阶段的并行调用落在同一 assistant message 的 tool_calls[] 内，保持顺序与 call_id；
// 一旦遇到 tool 输出消息、普通内容消息或 reasoning 消息即视为阶段边界，之后的 call 开新消息。
func mergeAdjacentFunctionCalls(messages []interface{}) ([]interface{}, error) {
	out := make([]interface{}, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "converted message must be a JSON object")
		}
		tcs, isCall := pureToolCallContent(msg)
		if !isCall {
			out = append(out, msg)
			continue
		}
		if len(out) > 0 {
			if prev, ok := out[len(out)-1].(map[string]interface{}); ok {
				if prevTcs, prevIsCall := pureToolCallContent(prev); prevIsCall {
					prev["tool_calls"] = append(prevTcs, tcs...)
					continue
				}
			}
		}
		merged := make([]interface{}, len(tcs))
		copy(merged, tcs)
		out = append(out, map[string]interface{}{
			"role":       "assistant",
			"tool_calls": merged,
		})
	}
	return out, nil
}

// pureToolCallContent 判定纯函数调用 assistant 消息（仅 role+tool_calls，无 content/reasoning_content）。
func pureToolCallContent(msg map[string]interface{}) ([]interface{}, bool) {
	if role, _ := msg["role"].(string); role != "assistant" {
		return nil, false
	}
	if _, hasContent := msg["content"]; hasContent {
		return nil, false
	}
	if _, hasReasoning := msg["reasoning_content"]; hasReasoning {
		return nil, false
	}
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) == 0 {
		return nil, false
	}
	return tcs, true
}

func convertInputItem(item map[string]interface{}, state *inputConversionState) (map[string]interface{}, error) {
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
		msg, err := convertFunctionCallItem(item)
		if err != nil {
			return nil, err
		}
		state.markToolCallsEmitted(msg)
		return msg, nil
	case "function_call_output":
		return convertFunctionCallOutputItem(item, state)
	case "custom_tool_call":
		msg, err := convertCustomToolCallItem(item)
		if err != nil {
			return nil, err
		}
		state.markToolCallsEmitted(msg)
		return msg, nil
	case "custom_tool_call_output":
		return convertCustomToolCallOutputItem(item, state)
	case "agent_message":
		return convertAgentMessageItem(item)
	case "reasoning":
		return convertReasoningItem(item)
	case "tool_search_call":
		msg := convertToolSearchCallItem(item, state.pairerOrNew())
		state.markToolCallsEmitted(msg)
		return msg, nil
	case "tool_search_call_output", "tool_search_output":
		return convertToolSearchCallOutputItem(item, state), nil
	case "web_search_call":
		msg := convertWebSearchCallItem(item, state.pairerOrNew())
		state.markToolCallsEmitted(msg)
		return msg, nil
	case "web_search_call_output", "web_search_output":
		return convertWebSearchCallOutputItem(item, state), nil
	default:
		// item_reference、§3.8 其余内置调用型 item 及未知类型均无 chat 表达：丢弃该 item 并告警，
		// 保留同一请求中其余可转换历史，不再拒绝整个请求。
		warnDropped(itemType, "input item type has no chat representation; item dropped")
		return nil, nil
	}
}

// convertReasoningItem 转换 reasoning item（responses §3.4）。
// P1-2/DN-5：summary 与 content 都是 block 数组，两者必须完整读取；推理文本经 Chat 扩展字段
// assistant.reasoning_content 继续传递（显式降级策略，非静默 —— 由契约测试锁定为预期行为）。
func convertReasoningItem(item map[string]interface{}) (map[string]interface{}, error) {
	var parts []string

	if raw, ok := item["summary"]; ok && raw != nil {
		blocks, ok := raw.([]interface{})
		if !ok {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "reasoning.summary must be a content block array (responses §3.4)")
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]interface{})
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "reasoning summary block must be a JSON object")
			}
			bt, _ := bm["type"].(string)
			if bt != "summary_text" {
				warnDropped("reasoning", fmt.Sprintf("reasoning summary block type %q is not summary_text (responses §3.4); block dropped", bt))
				continue
			}
			text, ok := bm["text"].(string)
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "reasoning summary_text block missing string field text")
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
	}

	if raw, ok := item["content"]; ok && raw != nil {
		switch v := raw.(type) {
		case string:
			// 兼容 codex 客户端把 content 作为原始字符串的既有形态（回归锁定），协议标准形态为 block 数组
			if v != "" {
				parts = append(parts, v)
			}
		case []interface{}:
			for _, b := range v {
				bm, ok := b.(map[string]interface{})
				if !ok {
					return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "reasoning content block must be a JSON object")
				}
				bt, _ := bm["type"].(string)
				if bt != "reasoning_text" {
					warnDropped("reasoning", fmt.Sprintf("reasoning content block type %q is not reasoning_text (responses §3.4); block dropped", bt))
					continue
				}
				text, ok := bm["text"].(string)
				if !ok {
					return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "reasoning_text block missing string field text")
				}
				if text != "" {
					parts = append(parts, text)
				}
			}
		default:
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "reasoning.content must be a content block array (responses §3.4)")
		}
	}

	if len(parts) == 0 {
		// 仅剩 encrypted_content 的 reasoning item 在 Chat 协议无任何表达 —— 丢弃该 item 并告警
		//（保留同一请求其余历史），不再拒绝整个请求。
		if enc, ok := item["encrypted_content"].(string); ok && enc != "" {
			warnDropped("reasoning", "encrypted reasoning item (encrypted_content only) cannot be expressed on the Chat side; item dropped")
			return nil, nil
		}
		// summary/content 均无文本的空 reasoning：无信息可丢失，跳过不产出空 assistant 消息
		return nil, nil
	}

	return map[string]interface{}{
		"role":              "assistant",
		"reasoning_content": strings.Join(parts, "\n"),
	}, nil
}

// convertAgentMessageItem 转换 codex 扩展 agent_message（responses §3.9）：语义固定 assistant 撰写，
// 文本块接受 text/input_text/output_text（真实 codex CLI 流量块类型为 input_text，接受口径与
// convertMessageItem 的 message 转换对齐）；另外 encrypted_content 块在多 agent 派发链路上承载
// NEW_TASK/FINAL_ANSWER 的明文载荷（表头在 input_text 块、正文在此块），对其 encrypted_content
// 字符串字段按文本保留，否则子 agent 只剩 Payload: 空表头、看不到任务正文。其余未知类型块丢弃。
// phase/memory_citation/delivery/questions 在 chat 无表达 —— DN-5 决议：丢弃标记、保留文本，
// 不得拒绝整个请求（显式降级由测试锁定）。
func convertAgentMessageItem(item map[string]interface{}) (map[string]interface{}, error) {
	var parts []string
	if raw, ok := item["content"]; ok && raw != nil {
		blocks, ok := raw.([]interface{})
		if !ok {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "agent_message.content must be a text block array (responses §3.9)")
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]interface{})
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "agent_message content block must be a JSON object")
			}
			bt, _ := bm["type"].(string)
			if bt == "" {
				// 无 type 的块若携带非空 string encrypted_content，说明是裸载荷形态（真实链路存在），
				// 直接按 encrypted_content 保留；否则归一为 input_text，维持既有归一/丢弃/硬失败路径。
				if text, ok := bm["encrypted_content"].(string); ok && text != "" {
					parts = append(parts, text)
					continue
				}
				bt = "input_text"
			}
			if bt == "encrypted_content" {
				// 真实 codex CLI 多 agent 派发把任务正文明文放在该字段（见函数注释）；缺失/非字符串/空串
				// 时无信息可保留，维持丢弃口径。
				text, ok := bm["encrypted_content"].(string)
				if !ok || text == "" {
					warnDropped("agent_message", "agent_message encrypted_content block missing non-empty string field encrypted_content; block dropped")
					continue
				}
				parts = append(parts, text)
				continue
			}
			if bt != "text" && bt != "input_text" && bt != "output_text" {
				warnDropped("agent_message", fmt.Sprintf("agent_message content block type %q is not a text block (responses §3.9); block dropped", bt))
				continue
			}
			text, ok := bm["text"].(string)
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "agent_message text block missing string field text")
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		// 无文本的 agent_message 不携带信息，跳过（既有回归锁定：空 content 丢弃且不报错）
		return nil, nil
	}
	return map[string]interface{}{
		"role":    "assistant",
		"content": strings.Join(parts, "\n"),
	}, nil
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

// standaloneCallID 只生成确定性递增 id、不登记配对：供 nextCallID 构造 fallback id，
// 孤立 output 不再使用（无配对即丢弃，见 inputConversionState）。
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

// inputConversionState 承载单次 convertInputToMessages 调用的配对状态（单请求转换，禁止跨请求残留）：
//   - pairer：缺 call_id 的 builtin 工具 call/output 的确定性 fallback 配对；
//   - emittedCallIDs：本次转换已成功产出为 assistant.tool_calls[].id 的 call_id 全集。
//
// emittedCallIDs 用于消除悬空引用（P1-1/P2-2）：chat §3.1/§6.1.1 要求 role:tool 消息的 tool_call_id
// 必须紧跟对应 assistant.tool_calls[].id；call item 因缺 call_id/name 等被丢弃后，若其配对的
// *_call_output 仍产出 role:tool，转换产物即为非法 chat 请求。故所有 output item 写入前校验
// call_id 已被登记，未登记则丢弃并告警。
type inputConversionState struct {
	pairer         *builtinFallbackCallIDPairer
	emittedCallIDs map[string]struct{}
}

func newInputConversionState() *inputConversionState {
	return &inputConversionState{
		pairer:         newBuiltinFallbackCallIDPairer(),
		emittedCallIDs: make(map[string]struct{}),
	}
}

// pairerOrNew 返回 builtin fallback 配对器；state 为 nil（防御直接调用/独立单测）时临时新建。
func (s *inputConversionState) pairerOrNew() *builtinFallbackCallIDPairer {
	if s == nil || s.pairer == nil {
		return newBuiltinFallbackCallIDPairer()
	}
	return s.pairer
}

// markToolCallsEmitted 登记 call item 产出的 assistant.tool_calls[].id（msg 为 nil 时不动作）。
func (s *inputConversionState) markToolCallsEmitted(msg map[string]interface{}) {
	if s == nil || msg == nil {
		return
	}
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok {
		return
	}
	if s.emittedCallIDs == nil {
		s.emittedCallIDs = make(map[string]struct{})
	}
	for _, raw := range tcs {
		tc, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if id, _ := tc["id"].(string); id != "" {
			s.emittedCallIDs[id] = struct{}{}
		}
	}
}

// callEmitted 判定 call_id 是否已在本次转换中作为 assistant.tool_calls[].id 产出。
// state 为 nil（防御直接调用/独立单测）时视为不校验，返回 true。
func (s *inputConversionState) callEmitted(callID string) bool {
	if s == nil {
		return true
	}
	_, ok := s.emittedCallIDs[callID]
	return ok
}

func convertToolSearchCallItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	return convertBuiltinToolCallItem(item, "tool_search", "ts_", pairer)
}

func convertToolSearchCallOutputItem(item map[string]interface{}, state *inputConversionState) map[string]interface{} {
	return convertBuiltinToolCallOutputItem(item, "ts_", state)
}

func convertWebSearchCallItem(item map[string]interface{}, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	return convertBuiltinToolCallItem(item, "web_search", "ws_", pairer)
}

func convertWebSearchCallOutputItem(item map[string]interface{}, state *inputConversionState) map[string]interface{} {
	return convertBuiltinToolCallOutputItem(item, "ws_", state)
}

// convertBuiltinToolCallItem 转换 builtin 工具 call item。
// 契约：pairer 由 convertInputToMessages 构造并沿调用链透传；缺 call_id 且 pairer 为 nil 时
// 兜底新建（局部配对，不 panic），仅防御直接调用。
func convertBuiltinToolCallItem(item map[string]interface{}, name, idPrefix string, pairer *builtinFallbackCallIDPairer) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		if pairer == nil {
			pairer = newBuiltinFallbackCallIDPairer()
		}
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
// 契约：state 与 convertBuiltinToolCallItem 共享同一实例才能配对；nil 时兜底新建（防御直接调用）。
// P2-2：call_id 未在本次转换中登记（无先行 call，或 call 侧被丢弃）时产出 role:tool 即悬空引用，
// 丢弃该 item 并告警，不再生成孤立 standalone id。
func convertBuiltinToolCallOutputItem(item map[string]interface{}, idPrefix string, state *inputConversionState) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		if pending, ok := state.pairerOrNew().takePending(idPrefix); ok {
			callID = pending
		} else {
			warnDropped("input", "tool call output has no emitted call to pair with; item dropped")
			return nil
		}
	} else if !state.callEmitted(callID) {
		warnDropped("input", "tool call output has no emitted call to pair with; item dropped")
		return nil
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

// convertMessageItem 转换 message item，role 先过 chat §3 六种请求角色白名单（tool/function 角色
// 不经由 message item 产生），phase 按 DN-5 丢标记保留文本。
// role 无法映射时丢弃整个 item 并告警；content 数组内全部 part 因能力性不可映射被丢弃（转换结果
// 为空）时同样丢弃该 item，不再拒绝整个请求。
func convertMessageItem(item map[string]interface{}) (map[string]interface{}, error) {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	switch role {
	case "user", "system", "developer", "assistant":
	default:
		warnDropped("message", fmt.Sprintf("message role %q has no chat request message equivalent (chat §3); item dropped", role))
		return nil, nil
	}

	// chat §3.1 消息联合已含 developer 类型（system/developer/user/assistant/tool/function），
	// responses §3.1 message.role 的 developer 原样直通，不再降级为 system（内容保真）。

	message := map[string]interface{}{
		"role":    role,
		"content": "",
	}

	if content, ok := item["content"]; ok {
		switch v := content.(type) {
		case string:
			message["content"] = v
		case []interface{}:
			cc, err := convertContentArray(v, role)
			if err != nil {
				return nil, err
			}
			if isEmptyConvertedContent(cc) {
				warnDropped("message", fmt.Sprintf("message role %q content resolved to empty after dropping unmappable parts; item dropped", role))
				return nil, nil
			}
			message["content"] = cc
		default:
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "message content must be a string or a content part array")
		}
	}

	return message, nil
}

// isEmptyConvertedContent 判定 convertContentArray 结果是否为空（无任何可发送 part）。
func isEmptyConvertedContent(cc interface{}) bool {
	switch v := cc.(type) {
	case string:
		return v == ""
	case []interface{}:
		return len(v) == 0
	case nil:
		return true
	default:
		return false
	}
}

// convertContentArray 按 chat §4 消息×部件矩阵转换 content parts（P1-3）：
// system/developer 仅 text；user 可 text/image/audio/file；assistant 仅 text 或恰一个 refusal。
// 能力性不可映射的违规 part（角色不允许的媒体/refusal、与 text/media 互斥的 refusal、多余 refusal、
// 未知 block 类型）一律丢弃并记 warn 日志，保留可保留的文本/媒体 —— 不再拒绝整个请求；
// 整条消息 part 全丢完时由 convertMessageItem 丢弃该 item。
// 仅结构性非法（block 非对象、文本/媒体必填字段缺失或载荷不全）仍返回 invalid_source_shape。
func convertContentArray(content []interface{}, role string) (interface{}, error) {
	var textParts []string
	var refusalPart map[string]interface{}
	hasMedia := false
	chatContent := []interface{}{}

	for _, block := range content {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "message content part must be a JSON object")
		}
		blockType, _ := blockMap["type"].(string)
		if blockType == "" {
			blockType = "input_text"
		}

		switch blockType {
		case "input_text", "output_text", "text":
			text, ok := blockMap["text"].(string)
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "text content part missing string field text")
			}
			if text == "" {
				// 空文本 part 无信息量，跳过不产出
				continue
			}
			textParts = append(textParts, text)
			chatContent = append(chatContent, map[string]interface{}{
				"type": "text",
				"text": text,
			})
		case "input_image", "image_url":
			if role != "user" {
				warnDropped("input", fmt.Sprintf("image content part is only allowed on user messages (chat §4), got role %q; part dropped", role))
				continue
			}
			imgBlock := convertImageBlock(blockMap)
			if imgBlock == nil {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "input_image part has no usable image_url/source payload (chat §4 image_url.url required)")
			}
			chatContent = append(chatContent, imgBlock)
			hasMedia = true
		case "input_audio":
			if role != "user" {
				warnDropped("input", fmt.Sprintf("input_audio part is only allowed on user messages (chat §4), got role %q; part dropped", role))
				continue
			}
			audioBlock := convertInputAudioBlock(blockMap)
			if audioBlock == nil {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "input_audio part missing required data/format (chat §4)")
			}
			chatContent = append(chatContent, audioBlock)
			hasMedia = true
		case "input_file":
			if role != "user" {
				warnDropped("input", fmt.Sprintf("input_file part is only allowed on user messages (chat §4), got role %q; part dropped", role))
				continue
			}
			fileBlock := convertInputFileBlock(blockMap)
			if fileBlock == nil {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "input_file part has no filename/file_data/file_id payload (chat §4)")
			}
			chatContent = append(chatContent, fileBlock)
			hasMedia = true
		case "refusal":
			// responses §5 refusal part → chat content part。chat §4 矩阵：refusal 仅 assistant，
			// 与 text/media 互斥且恰一个；违规时丢弃该 refusal part，保留可保留的文本/媒体。
			if role != "assistant" {
				warnDropped("input", fmt.Sprintf("refusal part is only allowed on assistant messages (chat §4), got role %q; part dropped", role))
				continue
			}
			refusalText, ok := blockMap["refusal"].(string)
			if !ok {
				return nil, errRequest(relaymodel.CodeInvalidSourceShape, "input", "refusal part missing string field refusal")
			}
			if refusalText == "" {
				// 空 refusal 无信息量，跳过（既有回归锁定），不构成与 text 的并存冲突
				continue
			}
			if refusalPart != nil {
				warnDropped("input", "assistant message must contain exactly one refusal part (chat §4); extra refusal dropped")
				continue
			}
			if len(textParts) > 0 || hasMedia {
				warnDropped("input", "assistant message mixes text/media parts with refusal, which are mutually exclusive (chat §4); refusal dropped")
				continue
			}
			refusalPart = map[string]interface{}{
				"type":    "refusal",
				"refusal": refusalText,
			}
		default:
			// 未知 content block type 无 chat §4 部件对应：丢弃该 part 并告警
			warnDropped("input", fmt.Sprintf("content part type %q has no chat §4 content part equivalent; part dropped", blockType))
			continue
		}
	}

	// refusal 先于 text/media 出现的乱序形态：此时拒绝保留 refusal，保留文本/媒体（互斥时文本优先）
	if refusalPart != nil && (len(textParts) > 0 || hasMedia) {
		warnDropped("input", "assistant message mixes text/media parts with refusal, which are mutually exclusive (chat §4); refusal dropped")
		refusalPart = nil
	}
	if refusalPart != nil {
		return []interface{}{refusalPart}, nil
	}
	if hasMedia {
		return chatContent, nil
	}
	if len(textParts) > 0 {
		return strings.Join(textParts, "\n"), nil
	}
	return "", nil
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
// input_audio 子对象形状（data/format）直通；顶层 data/format 形状组装为子对象；载荷不全返回 nil 由调用方报错。
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
// file 子对象形状直通；顶层 filename/file_data/file_id 形状组装为 file 子对象（三选一）；无有效载荷返回 nil 由调用方报错。
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

// convertFunctionCallItem 转换 function_call 历史 item（responses §3.2 → chat §3.1/§6.1.1）。
// call_id/name 是 chat tool_calls[].id / function.name 必填项，缺失即无法配对/无法执行 ——
// 丢弃该 item 并告警（不再拒绝整个请求）。
func convertFunctionCallItem(item map[string]interface{}) (map[string]interface{}, error) {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		warnDropped("function_call", "function_call item missing call_id: chat tool_calls[].id pairing cannot be established; item dropped")
		return nil, nil
	}
	name, _ := item["name"].(string)
	if namespace, ok := item["namespace"].(string); ok && namespace != "" {
		name = flattenNamespaceToolName(namespace, name)
	}
	if name == "" {
		warnDropped("function_call", "function_call item missing required name; item dropped")
		return nil, nil
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
	}, nil
}

func convertFunctionCallOutputItem(item map[string]interface{}, state *inputConversionState) (map[string]interface{}, error) {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		warnDropped("function_call_output", "function_call_output item missing call_id: chat tool_call_id is required (chat §3.1); item dropped")
		return nil, nil
	}
	// P1-1：配对的 function_call 因缺 call_id/name 被丢弃（或本请求从未产出该 call）时，
	// 产出 role:tool 即悬空引用，丢弃并告警。
	if !state.callEmitted(callID) {
		warnDropped("input", "tool call output has no emitted call to pair with; item dropped")
		return nil, nil
	}
	output := getStringOrJSONRaw(item["output"])

	return map[string]interface{}{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      output,
	}, nil
}

// convertCustomToolCallItem 把 custom_tool_call 转成 chat 协议的 tool_calls 元素。
// P0-2：custom 工具声明为 {input:string} function 后，历史 raw input 必须按同一 schema 编码为
// {"input":"raw"} 的 arguments JSON 字符串，否则声明与调用形态漂移、上游拒绝。
// 响应侧由 reconstructCustomToolCallInput 反向还原（#4 已修）。
func convertCustomToolCallItem(item map[string]interface{}) (map[string]interface{}, error) {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		warnDropped("custom_tool_call", "custom_tool_call item missing call_id: chat tool_calls[].id pairing cannot be established; item dropped")
		return nil, nil
	}
	name, _ := item["name"].(string)
	if name == "" {
		warnDropped("custom_tool_call", "custom_tool_call item missing required name; item dropped")
		return nil, nil
	}
	if item["input"] == nil {
		warnDropped("custom_tool_call", "custom_tool_call item missing required input (responses §3.5); item dropped")
		return nil, nil
	}
	raw := getStringOrJSONRaw(item["input"])
	wrapped, err := json.Marshal(map[string]interface{}{"input": raw})
	if err != nil {
		warnDropped("custom_tool_call", `custom_tool_call input cannot be encoded as {"input":...} arguments; item dropped`)
		return nil, nil
	}

	return map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": string(wrapped),
				},
			},
		},
	}, nil
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
// P1-1：配对的 custom_tool_call 被丢弃（或从未产出）时 role:tool 为悬空引用，丢弃并告警。
func convertCustomToolCallOutputItem(item map[string]interface{}, state *inputConversionState) (map[string]interface{}, error) {
	callID, _ := item["call_id"].(string)
	if callID == "" {
		warnDropped("custom_tool_call_output", "custom_tool_call_output item missing call_id: chat tool_call_id is required (chat §3.1); item dropped")
		return nil, nil
	}
	if !state.callEmitted(callID) {
		warnDropped("input", "tool call output has no emitted call to pair with; item dropped")
		return nil, nil
	}
	content := normalizeCustomToolOutput(item["output"])

	return map[string]interface{}{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      content,
	}, nil
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

// convertToolsToOpenAI 转换工具声明（responses §9 → chat §5.1）。
// DN-6 决议：默认拒绝 + 白名单。白名单以现有通过转换回归的类别为事实边界：
//   - function（含 "" 兼容形态）：chat §5.1 原生同族
//   - custom / apply_patch（apply_patch 类）：{input:string} function 代理 + 子工具，执行闭环在网关+codex 客户端
//   - namespace 展平路径
//   - tool_search：客户端代理执行（execution=client），call/output 配对回归完整
//
// 白名单外（web_search/web_search_preview/computer/computer_use/computer_use_preview/shell/local_shell/
// file_search/code_interpreter/image_generation/mcp/programmatic_tool_calling 及未知类型）丢弃该 tool
// 并记 warn 日志，保留同一请求其余可转换工具，不再 400（P1-7/Advisory-1 拒绝策略已按能力性丢弃调整）。
func convertToolsToOpenAI(tools []interface{}) ([]interface{}, error) {
	var result []interface{}
	seen := make(map[string]struct{})

	for i, tool := range tools {
		toolMap, ok := tool.(map[string]interface{})
		if !ok {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, fmt.Sprintf("tools[%d]", i), "tool entry must be a JSON object")
		}
		path := fmt.Sprintf("tools[%d]", i)
		toolType, _ := toolMap["type"].(string)
		switch toolType {
		case "function", "":
			fn := buildFunctionTool(toolMap)
			if fn == nil {
				warnDropped(path, "function tool missing required name; tool dropped")
				continue
			}
			appendUniqueChatTool(&result, seen, fn)
		case "custom":
			flat := flattenCustomTool(toolMap)
			if len(flat) == 0 {
				warnDropped(path, "custom tool missing required name; tool dropped")
				continue
			}
			appendUniqueChatTools(&result, seen, flat)
		case "namespace":
			flat, err := flattenNamespaceTool(toolMap, path)
			if err != nil {
				return nil, err
			}
			appendUniqueChatTools(&result, seen, flat)
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
		case "tool_search":
			appendUniqueChatTools(&result, seen, flattenToolSearchTool(toolMap))
		default:
			warnDropped(path, fmt.Sprintf("tool type %q has no execution closed loop on this gateway's Chat upstream (DN-6 whitelist: function/custom/apply_patch/namespace/tool_search); tool dropped", toolType))
			continue
		}
	}

	return result, nil
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

// buildFunctionTool 构造单个标准 function tool，兼容 flat 与嵌套 function 结构；缺 name 返回 nil 由调用方报错。
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
// 请求侧 convertCustomToolCallItem 的历史 arguments 必须与该 schema 一致（P0-2）。
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

// flattenNamespaceTool 把 type:namespace 工具的 child function 全部扁平化为顶层 function 工具（DN-6 白名单路径）。
// namespace 缺 name 丢弃整个工具、child 非 function 或缺 name 丢弃该 child，均记 warn 日志；
// 仅 namespace.tools 缺失/元素非对象等结构性非法返回 invalid_source_shape。
func flattenNamespaceTool(tool map[string]interface{}, path string) ([]interface{}, error) {
	namespace := getStringValue(tool, "name")
	if namespace == "" {
		warnDropped(path, "namespace tool missing required name; tool dropped")
		return nil, nil
	}
	children, ok := tool["tools"].([]interface{})
	if !ok {
		return nil, errRequest(relaymodel.CodeInvalidSourceShape, path, "namespace tool missing tools array")
	}
	var out []interface{}
	for i, raw := range children {
		childPath := fmt.Sprintf("%s.tools[%d]", path, i)
		child, ok := raw.(map[string]interface{})
		if !ok {
			return nil, errRequest(relaymodel.CodeInvalidSourceShape, childPath, "namespace child tool must be a JSON object")
		}
		if childType, _ := child["type"].(string); childType != "function" {
			warnDropped(childPath, fmt.Sprintf("namespace child type %q is not function; child dropped", childType))
			continue
		}
		childName := getStringValue(child, "name")
		if childName == "" {
			warnDropped(childPath, "namespace child function missing required name; child dropped")
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
	return out, nil
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
func ConvertChatResponseToResponses(chatResponseBody []byte, model string, fallbackReasoningToMessage bool) ([]byte, error) {
	return ConvertChatResponseToResponsesWithContext(chatResponseBody, model, fallbackReasoningToMessage, nil)
}

// ConvertChatResponseToResponsesWithContext 把上游 Chat 非流式响应转回 Responses Response 对象。
// P0-1：chatResponseBody 非法 JSON 返回 invalid_source_json（绝不原样透传 chat.completion）。
// P0-3：顶层对象与 message/reasoning item 输出 Responses §4/§5 全部必填字段，output_text part
// 恒含 annotations/logprobs；回显原请求可回显字段。
// 当 originalRequestRawJSON 非 nil 时，从原始 Responses 请求里解析 CodexToolContext，
// 用于在 tool_calls 还原时识别 namespace 字段与 custom_tool_call 类型。
func ConvertChatResponseToResponsesWithContext(chatResponseBody []byte, model string, fallbackReasoningToMessage bool, originalRequestRawJSON []byte) ([]byte, error) {
	var chatResp map[string]interface{}
	if err := json.Unmarshal(chatResponseBody, &chatResp); err != nil {
		return nil, errResponse(relaymodel.CodeInvalidSourceJSON, "", fmt.Errorf("upstream chat response is not valid JSON: %w", err))
	}
	if chatResp == nil {
		return nil, errResponse(relaymodel.CodeInvalidSourceJSON, "", errors.New("upstream chat response must be a JSON object"))
	}

	choices, _ := chatResp["choices"].([]interface{})

	// status 按首个 choice 的 finish_reason 推导（chat §6.2 → responses §4）：
	// length → incomplete + reason=max_output_tokens；content_filter → incomplete + reason=content_filter；
	// 其余（stop/tool_calls 等）保持 completed。与流式路径（chat_to_responses.go）对称。
	status := "completed"
	var incompleteDetails map[string]interface{}
	if len(choices) > 0 {
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

	// 顶层必填字段恒存在（responses §4）：id/created_at 缺失时由网关兜底生成，不伪装上游值缺失
	responseID := getStringValue(chatResp, "id")
	if responseID == "" {
		responseID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	createdAt := time.Now().Unix()
	if created, ok := chatResp["created"].(float64); ok {
		// responses 协议 §4 主对象字段名为 created_at（与流式路径 response.created_at 保持一致），
		// chat 的 created（Unix 秒）直接映射，不做单位换算。
		createdAt = int64(created)
	}
	resolvedModel := model
	if resolvedModel == "" {
		resolvedModel = getStringValue(chatResp, "model")
	}

	responsesResp := map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt,
		"status":     status,
		"error":      nil,
		"model":      resolvedModel,
		"output":     []interface{}{},
		"usage":      parseUsage(getObjectMap(chatResp, "usage")),
		"user":       nil,
		"truncated":  incompleteDetails != nil,
	}
	// incomplete_details 恒输出（null 字段按 responses §4 输出，与流式终态快照对齐）
	responsesResp["incomplete_details"] = incompleteDetails
	if _, has := responsesResp["usage"]; !has {
		responsesResp["usage"] = parseUsage(map[string]interface{}{})
	}

	// 回显原 Responses 请求可回显字段（responses §4）；非法原请求 JSON 显式拒绝，不静默降级
	if originalRequestRawJSON != nil {
		var req map[string]interface{}
		if err := json.Unmarshal(originalRequestRawJSON, &req); err != nil {
			return nil, errResponse(relaymodel.CodeInvalidSourceJSON, "original_request", fmt.Errorf("original responses request is not valid JSON: %w", err))
		}
		for _, key := range []string{
			"instructions", "max_output_tokens", "parallel_tool_calls", "reasoning",
			"temperature", "tool_choice", "tools", "top_p", "metadata", "text",
			"modalities", "store", "service_tier", "previous_response_id",
		} {
			if v, ok := req[key]; ok {
				responsesResp[key] = v
			}
		}
		if v, ok := req["user"]; ok && v != nil {
			responsesResp["user"] = v
		}
		if pr, ok := req["previous_response_id"].(string); ok && pr != "" {
			// 请求侧已按 P1-1 拒绝该字段；直接调用转换器时仍如实回显，不吞
			responsesResp["previous_response_id"] = pr
		}
	}
	if _, has := responsesResp["previous_response_id"]; !has {
		responsesResp["previous_response_id"] = nil
	}

	// 构建 CodexCtx：仅当调用方提供原始请求时才构建；nil 时走 3 参数旧退化行为
	var codexCtx *CodexToolContext
	if originalRequestRawJSON != nil {
		ctx := buildCodexToolContextFromRequest(originalRequestRawJSON)
		codexCtx = &ctx
	}

	output := responsesResp["output"].([]interface{})
	for ci, choice := range choices {
		choiceMap, ok := choice.(map[string]interface{})
		if !ok {
			continue
		}
		message, ok := choiceMap["message"].(map[string]interface{})
		if !ok {
			continue
		}
		// 多 choice 时以 choice 后缀复合 responseID，保证 item id 在本 response 内唯一
		respIDForChoice := responseID
		if len(choices) > 1 {
			respIDForChoice = fmt.Sprintf("%s_c%d", responseID, ci)
		}
		choiceOutput, err := convertChatMessageToOutput(message, codexCtx, respIDForChoice)
		if err != nil {
			return nil, err
		}
		output = append(output, choiceOutput...)
	}
	responsesResp["output"] = output

	// 兜底：output 中无 message item，但有 reasoning 时，复制第一个 reasoning 的 summary 文本为 message。
	// fallback message item 与常规 message item 同等完整（id/status/annotations/logprobs 必填，P0-3）。
	if fallbackReasoningToMessage {
		outputs, _ := responsesResp["output"].([]interface{})
		hasMessage := false
		firstReasoningText := ""
		ordinal := 0
		for _, o := range outputs {
			om, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			switch t, _ := om["type"].(string); t {
			case "message":
				hasMessage = true
				ordinal++
			case "reasoning":
				ordinal++
				if firstReasoningText == "" {
					if summary, ok := om["summary"].([]interface{}); ok && len(summary) > 0 {
						if s, ok := summary[0].(map[string]interface{}); ok {
							firstReasoningText, _ = s["text"].(string)
						}
					}
				}
			}
		}
		if !hasMessage && firstReasoningText != "" {
			parts := []interface{}{buildResponsesOutputTextPart(firstReasoningText, nil)}
			outputs = append(outputs, buildResponsesMessageItem(responseID, ordinal, parts))
			responsesResp["output"] = outputs
		}
	}

	result, err := json.Marshal(responsesResp)
	if err != nil {
		return nil, errResponse(relaymodel.CodeInvalidSourceShape, "", fmt.Errorf("marshal responses output failed: %w", err))
	}
	return result, nil
}

// getObjectMap 安全提取 map 字段，缺失/类型不符返回空 map（usage 兜底 parseUsage 零值用）。
func getObjectMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return map[string]interface{}{}
}

// buildResponsesMessageItem 构造 Responses §5 message item（id*/role*/content*/status* 必填）。
// item id 形如 msg_<responseID>_<ordinal>，与流式路径命名规则一致，保证本 response 内唯一。
func buildResponsesMessageItem(responseID string, ordinal int, parts []interface{}) map[string]interface{} {
	if parts == nil {
		parts = []interface{}{}
	}
	return map[string]interface{}{
		"id":      fmt.Sprintf("msg_%s_%d", responseID, ordinal),
		"type":    "message",
		"role":    "assistant",
		"content": parts,
		"status":  "completed",
	}
}

// buildResponsesReasoningItem 构造 Responses §5 reasoning item（id*/summary*/status* 必填）。
// content 为可选的 reasoning_text block 数组，仅在非空时输出。
func buildResponsesReasoningItem(responseID string, ordinal int, summary []interface{}, content []interface{}) map[string]interface{} {
	if summary == nil {
		summary = []interface{}{}
	}
	item := map[string]interface{}{
		"id":      fmt.Sprintf("rs_%s_%d", responseID, ordinal),
		"type":    "reasoning",
		"summary": summary,
		"status":  "completed",
	}
	if len(content) > 0 {
		item["content"] = content
	}
	return item
}

// buildResponsesOutputTextPart 构造 output_text part：annotations/logprobs 协议必填（P0-3），
// 上游 message.annotations 存在时挂载到每个文本 part，否则恒输出空数组。
func buildResponsesOutputTextPart(text string, annotations []interface{}) map[string]interface{} {
	anns := annotations
	if anns == nil {
		anns = []interface{}{}
	}
	return map[string]interface{}{
		"type":        "output_text",
		"text":        text,
		"annotations": anns,
		"logprobs":    []interface{}{},
	}
}

func buildResponsesRefusalPart(refusal string) map[string]interface{} {
	return map[string]interface{}{
		"type":    "refusal",
		"refusal": refusal,
	}
}

// convertChatMessageToOutput 把 Chat 响应 message 转为 Responses output items。
// responseID 用于生成 item 唯一 id（多 choice 时由调用方复合 choice 后缀保证全 response 唯一）。
// P0-3：message/reasoning item 补 id/status；output_text part 补 annotations/logprobs。
// 上游 tool call 缺 id/name/arguments 返回 malformed_tool_call，绝不产出空 id item。
func convertChatMessageToOutput(message map[string]interface{}, codexCtx *CodexToolContext, responseID string) ([]interface{}, error) {
	var output []interface{}
	ordinal := 0

	// 处理 reasoning_content（上游扩展推理文本 → summary 承载的 reasoning item）
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		output = append(output, buildResponsesReasoningItem(responseID, ordinal,
			[]interface{}{map[string]interface{}{"type": "summary_text", "text": reasoning}}, nil))
		ordinal++
	}

	// 处理 refusal（chat §3.1/§6.1 message.refusal → responses §5 refusal content part）。
	// 有 content 时合并进同一 message item；无 content 时单独生成 refusal message item。
	refusalText, _ := message["refusal"].(string)

	// annotations（chat §6.1.3 message.annotations → responses §5 output_text part.annotations）：
	// 引用标注附加到文本 part；无文本 part 时随消息丢弃（与缺失时行为一致）。
	annotations, _ := message["annotations"].([]interface{})

	// 处理 content，同时提取  thinking 标签
	if content, ok := message["content"]; ok && content != nil {
		// 先尝试提取 thinking 内容
		thinking, remainingContent := extractThinkingFromContent(content)
		if thinking != "" {
			output = append(output, buildResponsesReasoningItem(responseID, ordinal,
				[]interface{}{map[string]interface{}{"type": "summary_text", "text": thinking}}, nil))
			ordinal++
		}

		// 处理剩余的文本内容
		if remainingContent != nil {
			switch v := remainingContent.(type) {
			case string:
				if v != "" || refusalText != "" {
					parts := []interface{}{}
					if v != "" {
						parts = append(parts, buildResponsesOutputTextPart(v, annotations))
					}
					if refusalText != "" {
						parts = append(parts, buildResponsesRefusalPart(refusalText))
						refusalText = ""
					}
					output = append(output, buildResponsesMessageItem(responseID, ordinal, parts))
					ordinal++
				}
			case []interface{}:
				parts := []interface{}{}
				for _, block := range v {
					bm, ok := block.(map[string]interface{})
					if !ok {
						return nil, errResponse(relaymodel.CodeInvalidSourceShape, "message.content", errors.New("upstream content block must be a JSON object"))
					}
					blockType, _ := bm["type"].(string)
					switch blockType {
					case "text", "output_text":
						text := getStringValue(bm, "text")
						if text == "" {
							// 空文本 block 无信息量，跳过
							continue
						}
						parts = append(parts, buildResponsesOutputTextPart(text, annotations))
					case "refusal":
						r := getStringValue(bm, "refusal")
						if r == "" {
							continue
						}
						parts = append(parts, buildResponsesRefusalPart(r))
					default:
						return nil, errResponse(relaymodel.CodeUnsupportedOutputItem, "message.content", fmt.Errorf("upstream content part type %q cannot be represented as a Responses output part", blockType))
					}
				}
				if refusalText != "" {
					parts = append(parts, buildResponsesRefusalPart(refusalText))
					refusalText = ""
				}
				if len(parts) > 0 {
					output = append(output, buildResponsesMessageItem(responseID, ordinal, parts))
					ordinal++
				}
			default:
				return nil, errResponse(relaymodel.CodeInvalidSourceShape, "message.content", errors.New("upstream message.content must be a string or an array of content parts (chat §6.1)"))
			}
		}
	}

	// 无 content 时的纯 refusal message item（同样具备 id/status 必填字段，P0-3）
	if refusalText != "" {
		output = append(output, buildResponsesMessageItem(responseID, ordinal, []interface{}{buildResponsesRefusalPart(refusalText)}))
		ordinal++
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
		for j, tc := range toolCalls {
			tcMap, ok := tc.(map[string]interface{})
			if !ok {
				return nil, errResponse(relaymodel.CodeInvalidSourceShape, fmt.Sprintf("message.tool_calls[%d]", j), errors.New("tool call entry must be a JSON object"))
			}
			callID := getStringValue(tcMap, "id")
			fnVal := getObjectValue(tcMap, "function")
			fnMap, fnOK := fnVal.(map[string]interface{})
			if !fnOK {
				// chat §6.1.1 除 function 外还定义 type:"custom" 变体（ChatCompletionMessageCustomToolCall：
				// id*, custom:{name*,input*}），与 responses §5 custom_tool_call（id*,call_id*,name*,input*）
				// 字段一一对应，可无损映射；必填子键缺失视为上游畸形 —— 显式 502，不再 Warn 后跳过。
				if tcType, _ := tcMap["type"].(string); tcType == "custom" {
					customMap, _ := getObjectValue(tcMap, "custom").(map[string]interface{})
					customName := getStringValue(customMap, "name")
					customInput, hasCustomInput := customMap["input"].(string)
					if callID == "" || customName == "" || !hasCustomInput {
						return nil, errResponse(relaymodel.CodeMalformedToolCall, fmt.Sprintf("message.tool_calls[%d]", j), errors.New("chat custom tool call missing id/name/input (chat §6.1.1)"))
					}
					output = append(output, map[string]interface{}{
						"type":    "custom_tool_call",
						"id":      "ctc_" + callID,
						"call_id": callID,
						"name":    customName,
						"input":   customInput,
						"status":  "completed",
					})
					continue
				}
				return nil, errResponse(relaymodel.CodeMalformedToolCall, fmt.Sprintf("message.tool_calls[%d]", j), errors.New("chat tool call has neither function nor custom payload (chat §6.1.1)"))
			}
			rawName := getStringValue(fnMap, "name")
			rawArgs, argsOK := fnMap["arguments"].(string)
			// chat §6.1.1 function 变体 id/name/arguments 必填；缺失即上游畸形（P0-3），
			// 不能生成空 id / 空 name item。
			if callID == "" || rawName == "" || !argsOK || rawArgs == "" {
				return nil, errResponse(relaymodel.CodeMalformedToolCall, fmt.Sprintf("message.tool_calls[%d]", j), errors.New("chat function tool call missing id/name/arguments (chat §6.1.1)"))
			}

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
					// tool_search_call 的 arguments 是内嵌 JSON 对象，与 function_call.arguments 字符串口径不同；
					// 取值口径见 toolSearchArgumentsValue（统一 helper，三条输出路径结构一致）。
					output = append(output, map[string]interface{}{
						"type":      "tool_search_call",
						"id":        "ts_" + callID,
						"call_id":   callID,
						"name":      rawName,
						"arguments": toolSearchArgumentsValue(rawArgs),
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

	return output, nil
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
