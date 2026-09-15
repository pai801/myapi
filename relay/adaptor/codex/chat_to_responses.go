package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/model"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// chatToResponsesState 流式转换状态
type chatToResponsesState struct {
	Seq            int
	ResponseID     string
	CreatedAt      int64
	TerminalEvent  responsesTerminalEvent
	CurrentMsgID   string
	CurrentFCID    string
	InTextBlock    bool
	InFuncBlock    bool
	FuncArgsBuf    map[int]*strings.Builder // index -> args
	FuncNames      map[int]string           // index -> function name
	FuncCallIDs    map[int]string           // index -> call id
	FuncItemAdded  map[int]bool             // index -> whether output_item.added has been emitted
	TextBuf        strings.Builder
	PendingTextBuf strings.Builder // 延迟首段纯空白 content，避免 reasoning fallback 场景 streaming/completed 不一致
	// tool_calls 上游畸形回退：delta.tool_calls 缺 index 时按到达顺序分配的匿名 key
	// （多个 call 都回落到 0 会让 FuncArgsBuf/FuncNames/FuncCallIDs 互相覆盖，arguments 串包）
	NextAnonToolIdx    int          // 下一个候选匿名 key（跳过已被显式 index 或先前匿名 call 占用的 key）
	AnonToolCallIdx    int          // 最近一个匿名 call 的 key，供后续仅带 arguments 的 delta 续用
	AnonToolCallActive bool         // 是否已有可续用的匿名 call
	WarnedAnonToolDrop bool         // 无法关联 / key 争用的匿名 delta 丢弃日志一次性标记
	AnonToolKeys       map[int]bool // 匿名分配出去的 key 集合，显式 index 落在其上时按 call id 判归属，禁止接管
	// refusal state（拒绝内容增量，与 text 互斥，chat §4/§7.2）
	RefusalBuf                strings.Builder
	InRefusalBlock            bool
	CurrentRefusalMsgID       string // refusal message item 的 id（msg_<respID>_<outputIndex>）
	CurrentRefusalOutputIndex int    // refusal 块打开时记录的 output_index，close 复用不按 ReasoningPartAdded 重算
	RefusalItemInOutput       bool   // refusal 已作为独立 message item 占据 output（tool/reasoning 偏移用）
	RefusalAppendedToMsg      bool   // 防御路径：refusal part 追加到已关闭的 text message item
	CurrentTextOutputIndex    int    // text 块打开时记录的 output_index，close/delta/终态排序复用（与 refusal 快照同构，不随 reasoning 后到重算）
	// text 分段：一个 message item 可含多个 output_text part（refusal→text→tool→text 等
	// 正常重开场景按 part 追加，不重发 output_item.added，不丢内容）
	TextParts               []string        // 各已完成 text 段全文（按 content_index 顺序，供 done/终态数组还原）
	TextPartBuf             strings.Builder // 当前 text 段增量缓冲（done 事件按段而非全文）
	TextPartCount           int             // 已开启的 text 段数 = 下一段的 content_index = 追加模式下 refusal part 的下标
	CurrentTextContentIndex int             // 当前打开 text 段的 content_index
	// 违规交错防御：仅当「refusal 增量正在进行中」或「该 message item 已挂上 refusal part」时来了
	// 新 text 段才丢弃（chat §4 互斥违反，与非流式侧 drop text after refusal part 对齐）。
	// 不可用粘滞的 RefusalItemInOutput 判定，否则 refusal→text→tool→text 的后续正常 text 段会被永久丢弃
	WarnedTextDropAfterRefusal bool // 丢弃日志一次性标记，避免畸形上游逐 chunk 刷日志
	// RefusalViolationSeen：「refusal 刚关闭」一次性违规窗口。closeRefusalBlock 关闭时置位；
	// tool/reasoning/refusal 重开等非 text 内容块到达即复位；置位期间来的新 text 段按违规丢弃。
	// 必须是一次性标志而非粘滞位，否则 refusal→text→tool→text 的后续正常 text 段会被永久丢弃
	RefusalViolationSeen bool
	// 最后一个非空 finish_reason（length → response.incomplete 语义，chat §6.2）
	FinishReason string
	// reasoning state
	ReasoningActive    bool
	ReasoningItemID    string
	ReasoningBuf       strings.Builder
	ReasoningPartAdded bool
	ReasoningIndex     int
	// <think> 标签状态机（用于将正文里的 <think>...</think> 提取为 reasoning_content）
	Think thinkTagStateMachine
	// usage（完整支持详细字段）
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CachedTokens     int64 // input_tokens_details.cached_tokens / cache_read_input_tokens
	CacheWriteTokens int64 // prompt_tokens_details.cache_write_tokens → input_tokens_details.cache_write_tokens
	ReasoningTokens  int64 // output_tokens_details.reasoning_tokens（仅来自上游 usage，缺失为 0，禁止文本估算 P0-4）
	UsageSeen        bool
	// CompletionDetails 承载 Chat completion_tokens_details 全字段（chat §8.2）：
	// accepted/rejected/audio/reasoning/text 与非流式 normalizeOutputDetails 同源映射（P1-9）。
	CompletionDetails     model.CompletionTokensDetails
	completionDetailsSeen bool                   // 上游是否提供过 completion/output_tokens_details 对象
	completionDetailsRaw  map[string]interface{} // details 原始对象（透传零值子字段，字段集合与非流式一致）
	// ConversionError 记录本流首个转换错误（invalid_stream_event / malformed_tool_call 等）。
	// 置位后 generateFailedEvents 产出唯一 failed 终态，[DONE] 及后续 chunk 一律不得合成 completed（P0-1）。
	ConversionError error
	// Claude 缓存 TTL 细分
	CacheCreationTokens   int64  // cache_creation_input_tokens
	CacheCreation5mTokens int64  // cache_creation_5m_input_tokens
	CacheCreation1hTokens int64  // cache_creation_1h_input_tokens
	CacheTTL              string // "5m" | "1h" | "mixed"
	HasClaudeCacheFields  bool
	// 首次消息标记
	FirstChunk                 bool
	CodexToolCompatEnabled     bool
	CodexCtx                   CodexToolContext
	CodexCtxInitialized        bool
	FallbackReasoningToMessage bool // 兜底：无 content 仅 reasoning 时，将 reasoning 文本复制为 message
	// 已完成响应体的 JSON 缓存，避免 generateCompletedEvents 被重复调用时因 TerminalEvent 已设置而无法获取
	CompletedBodyJSON []byte
}

type responsesTerminalEvent string

const (
	responsesTerminalCompleted  responsesTerminalEvent = "response.completed"
	responsesTerminalFailed     responsesTerminalEvent = "response.failed"
	responsesTerminalIncomplete responsesTerminalEvent = "response.incomplete"
)

// isCustomProxy 返回给定索引的工具调用是否为 Codex 自定义工具代理
func (st *chatToResponsesState) isCustomProxy(idx int) bool {
	name := st.FuncNames[idx]
	if name == "" || !st.CodexCtxInitialized {
		return false
	}
	return st.CodexCtx.IsCustomToolProxy(name)
}

func (st *chatToResponsesState) customToolOutputIndex(idx int) int {
	outputIndex := idx
	if st.ReasoningPartAdded {
		outputIndex++
	}
	if st.CurrentMsgID != "" || st.shouldFallbackReasoning() {
		outputIndex++
	}
	// refusal 独立 message item 已占据 output：refusal→text→tool 时该 item 仍位于 tool 之前需偏移，
	// 无条件加（RefusalItemInOutput 仅在独立 refusal 打开时置位，append 模式置位的是
	// RefusalAppendedToMsg 不置此项，与 CurrentMsgID 分支天然互斥，不会重复加）。
	if st.RefusalItemInOutput {
		outputIndex++
	}
	return outputIndex
}

func (st *chatToResponsesState) builtinToolKind(idx int) string {
	name := st.FuncNames[idx]
	if name == "" || !st.CodexCtxInitialized {
		return ""
	}
	if st.CodexCtx.IsBuiltinTool(name, "tool_search") {
		return "tool_search"
	}
	if st.CodexCtx.IsBuiltinTool(name, "web_search") {
		return "web_search"
	}
	return ""
}

// builtinToolItemID 返回 builtin 工具 item id。前缀 ts_/ws_ 与
// docs/responses-protocol.md §3.6/3.7 及非流式侧（responses_to_chat.go）保持一致。
func (st *chatToResponsesState) builtinToolItemID(idx int) string {
	callID := st.FuncCallIDs[idx]
	switch st.builtinToolKind(idx) {
	case "tool_search":
		return fmt.Sprintf("ts_%s", callID)
	case "web_search":
		return fmt.Sprintf("ws_%s", callID)
	default:
		return ""
	}
}

func (st *chatToResponsesState) emitBuiltinLifecycleEvent(idx int, nextSeq func() int, suffix string) string {
	kind := st.builtinToolKind(idx)
	if kind == "" {
		return ""
	}
	msg := `{"type":"","sequence_number":0,"item_id":"","output_index":0}`
	msg, _ = sjson.Set(msg, "type", fmt.Sprintf("response.%s_call.%s", kind, suffix))
	msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
	msg, _ = sjson.Set(msg, "item_id", st.builtinToolItemID(idx))
	msg, _ = sjson.Set(msg, "output_index", st.customToolOutputIndex(idx))
	return emitResponsesEvent(fmt.Sprintf("response.%s_call.%s", kind, suffix), msg)
}

func (st *chatToResponsesState) emitBuiltinSearchQueryDelta(idx int, delta string, nextSeq func() int) string {
	kind := st.builtinToolKind(idx)
	if kind == "" {
		return ""
	}
	msg := `{"type":"","sequence_number":0,"item_id":"","output_index":0,"delta":""}`
	msg, _ = sjson.Set(msg, "type", fmt.Sprintf("response.%s_call.search_query.delta", kind))
	msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
	msg, _ = sjson.Set(msg, "item_id", st.builtinToolItemID(idx))
	msg, _ = sjson.Set(msg, "output_index", st.customToolOutputIndex(idx))
	msg, _ = sjson.Set(msg, "delta", delta)
	return emitResponsesEvent(fmt.Sprintf("response.%s_call.search_query.delta", kind), msg)
}

func (st *chatToResponsesState) emitBuiltinSearchQueryDone(idx int, query string, nextSeq func() int) string {
	kind := st.builtinToolKind(idx)
	if kind == "" {
		return ""
	}
	queryValue := query
	if parsed := gjson.Parse(query); parsed.IsObject() {
		if v := parsed.Get("query"); v.Exists() && v.Type != gjson.Null {
			queryValue = v.String()
		}
	}
	msg := `{"type":"","sequence_number":0,"item_id":"","output_index":0,"query":""}`
	msg, _ = sjson.Set(msg, "type", fmt.Sprintf("response.%s_call.search_query.done", kind))
	msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
	msg, _ = sjson.Set(msg, "item_id", st.builtinToolItemID(idx))
	msg, _ = sjson.Set(msg, "output_index", st.customToolOutputIndex(idx))
	msg, _ = sjson.Set(msg, "query", queryValue)
	return emitResponsesEvent(fmt.Sprintf("response.%s_call.search_query.done", kind), msg)
}

func (st *chatToResponsesState) shouldFallbackReasoning() bool {
	text := st.TextBuf.String() + st.PendingTextBuf.String()
	if st.RefusalBuf.Len() > 0 {
		// refusal 已有独立 message item（refusal part），不再兜底复制 reasoning 文本
		return false
	}
	return st.FallbackReasoningToMessage && strings.TrimSpace(text) == "" && st.ReasoningBuf.Len() > 0
}

func (st *chatToResponsesState) addToolCallItemIfNeeded(idx int, nextSeq func() int) []string {
	if st.FuncItemAdded[idx] || st.FuncCallIDs[idx] == "" || st.FuncNames[idx] == "" {
		return nil
	}

	callID := st.FuncCallIDs[idx]
	name := st.FuncNames[idx]
	outputIndex := st.customToolOutputIndex(idx)

	var item string
	if st.isCustomProxy(idx) {
		itemID := fmt.Sprintf("ctc_%s", callID)
		originalName := st.CodexCtx.OriginalCustomToolName(name)
		item = `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"in_progress","call_id":"","name":"","input":""}}`
		item, _ = sjson.Set(item, "item.id", itemID)
		item, _ = sjson.Set(item, "item.name", originalName)
	} else if st.CodexCtxInitialized && st.CodexCtx.IsBuiltinTool(name, "tool_search") {
		itemID := fmt.Sprintf("ts_%s", callID)
		item = `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"tool_search_call","status":"in_progress","arguments":"","call_id":"","name":"tool_search","execution":"client"}}`
		item, _ = sjson.Set(item, "item.id", itemID)
		item, _ = sjson.Set(item, "item.name", name)
		item, _ = sjson.Set(item, "item.call_id", callID)
	} else if st.CodexCtxInitialized && st.CodexCtx.IsBuiltinTool(name, "web_search") {
		// web_search_call 无 execution 字段（docs §3.6 与上游格式一致）
		itemID := fmt.Sprintf("ws_%s", callID)
		item = `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"web_search_call","status":"in_progress","arguments":"","call_id":"","name":"web_search"}}`
		item, _ = sjson.Set(item, "item.id", itemID)
		item, _ = sjson.Set(item, "item.name", name)
		item, _ = sjson.Set(item, "item.call_id", callID)
	} else {
		itemID := fmt.Sprintf("fc_%s", callID)
		item = `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","arguments":"","call_id":"","name":""}}`
		item, _ = sjson.Set(item, "item.id", itemID)
		if buf := st.FuncArgsBuf[idx]; buf != nil && buf.Len() > 0 {
			item, _ = sjson.Set(item, "item.arguments", buf.String())
		}
		displayName, namespace := st.CodexCtx.OpenAINameForFunctionTool(name)
		item, _ = sjson.Set(item, "item.name", displayName)
		if namespace != "" {
			item, _ = sjson.Set(item, "item.namespace", namespace)
		}
	}
	item, _ = sjson.Set(item, "sequence_number", nextSeq())
	item, _ = sjson.Set(item, "output_index", outputIndex)
	item, _ = sjson.Set(item, "item.call_id", callID)
	st.FuncItemAdded[idx] = true
	out := []string{emitResponsesEvent("response.output_item.added", item)}
	if st.builtinToolKind(idx) != "" {
		out = append(out, st.emitBuiltinLifecycleEvent(idx, nextSeq, "in_progress"))
		out = append(out, st.emitBuiltinLifecycleEvent(idx, nextSeq, "searching"))
	}
	return out
}

// findFuncIdxByCallID 在已登记的 tool_calls 里按 call id 反查 key（部分上游每个 chunk
// 重发完整 tool_call，若按 id 重新分配 key 会把同一个 call 拆成多个 item）。
func (st *chatToResponsesState) findFuncIdxByCallID(callID string) (int, bool) {
	if callID == "" {
		return 0, false
	}
	for k, v := range st.FuncCallIDs {
		if v == callID {
			return k, true
		}
	}
	return 0, false
}

// anonToolCallIdx 为缺 index 的畸形 tool_calls delta 给出跨 chunk 稳定的 key。
// 规则（OpenAI 流式语义里 index 是同一 call 的 delta 归并唯一依据，缺它只能按到达顺序推断）：
//  1. 带 id：先按 id 复用既有 key，否则分配一个未被显式 index 或先前匿名 call 占用的新 key，并记为当前 call；
//  2. 不带 id：续用最近的匿名 call（纯 arguments delta）；
//  3. 二者皆不可用：无法可靠关联，返回 false 由调用方跳过并 Warn，而非静默覆盖 0 号 key。
//
// 分配出去的 key 记入 AnonToolKeys：显式 index 落到这些 key 上时要按 call id 判归属
// （见 ConvertOpenAIChatToResponsesWithContext 的 tool_calls 分支），否则匿名 call 与
// 显式 call 会共用同一 FuncArgsBuf 并互相覆盖 FuncCallIDs。
func (st *chatToResponsesState) anonToolCallIdx(callID string) (int, bool) {
	if callID != "" {
		if k, ok := st.findFuncIdxByCallID(callID); ok {
			st.AnonToolCallIdx = k
			st.AnonToolCallActive = true
			return k, true
		}
		// FuncNames/FuncCallIDs/FuncArgsBuf 任一非空都视为 key 已占用：
		// 显式 index 的 delta 可以只带 name（id 在前一 chunk 已到），漏判 FuncNames 会撞 key
		for st.FuncArgsBuf[st.NextAnonToolIdx] != nil || st.FuncCallIDs[st.NextAnonToolIdx] != "" ||
			st.FuncNames[st.NextAnonToolIdx] != "" {
			st.NextAnonToolIdx++
		}
		st.AnonToolCallIdx = st.NextAnonToolIdx
		st.NextAnonToolIdx++
		st.AnonToolCallActive = true
		if st.AnonToolKeys == nil {
			st.AnonToolKeys = make(map[int]bool)
		}
		st.AnonToolKeys[st.AnonToolCallIdx] = true
		return st.AnonToolCallIdx, true
	}
	if st.AnonToolCallActive {
		return st.AnonToolCallIdx, true
	}
	return 0, false
}

var chatDataTag = []byte("data:")

func emitResponsesEvent(event string, payload string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, payload)
}

func GetStreamUsage(param interface{}) (promptTokens, completionTokens, totalTokens, cachedTokens int) {
	st, ok := param.(*chatToResponsesState)
	if !ok || st == nil {
		return 0, 0, 0, 0
	}
	return int(st.InputTokens), int(st.OutputTokens), int(st.TotalTokens), int(st.CachedTokens)
}

func GetStreamCompletedBody(param interface{}, originalRequestRawJSON []byte) []byte {
	st, ok := param.(*chatToResponsesState)
	if !ok || st == nil {
		return nil
	}
	// 优先返回缓存的 completed body，避免 generateCompletedEvents 因 TerminalEvent 已设置而返回 nil
	if st.CompletedBodyJSON != nil {
		return st.CompletedBodyJSON
	}
	// 兜底：尚未生成过 completed 事件时，按原有逻辑生成（错误显式记日志：终态链已由 forwarder 消费，
	// 此处仅用于日志响应体提取，失败即无 completed body）
	events, genErr := st.generateCompletedEvents(originalRequestRawJSON)
	if genErr != nil {
		logger.Log.Warnf("GetStreamCompletedBody: terminal generation failed: %v", genErr)
	}
	for _, event := range events {
		if strings.Contains(event, "response.completed") || strings.Contains(event, "response.incomplete") {
			dataPrefix := "data: "
			idx := strings.Index(event, dataPrefix)
			if idx >= 0 {
				dataStr := event[idx+len(dataPrefix):]
				dataStr = strings.TrimSpace(dataStr)
				return []byte(dataStr)
			}
		}
	}
	return nil
}

func effectiveCacheCreationTokens(cacheCreation, cacheCreation5m, cacheCreation1h int64) int64 {
	if cacheCreation > 0 {
		return cacheCreation
	}
	return cacheCreation5m + cacheCreation1h
}

func calculateClaudeTotalTokens(inputTokens, outputTokens, cacheReadTokens, cacheCreation, cacheCreation5m, cacheCreation1h int64) int64 {
	return inputTokens + outputTokens + cacheReadTokens + effectiveCacheCreationTokens(cacheCreation, cacheCreation5m, cacheCreation1h)
}

// ensureCodexToolContext 初始化 Codex 工具上下文
func (st *chatToResponsesState) ensureCodexToolContext(originalRequestRawJSON []byte) {
	if st.CodexCtxInitialized {
		return
	}
	st.CodexCtx = buildCodexToolContextFromRequest(originalRequestRawJSON)
	st.CodexCtxInitialized = true
}

// conversionModelError 把协议转换错误映射为 response.failed 终态的 error 对象（responses §4 error 结构）。
// Code 保留稳定机器码（invalid_stream_event / malformed_tool_call 等），供客户端与日志分派。
func conversionModelError(err error) *model.Error {
	e := &model.Error{Message: err.Error(), Type: "upstream_error", Code: "invalid_upstream_response"}
	var pce *model.ProtocolConversionError
	if errors.As(err, &pce) {
		e.Code = pce.Code
	}
	return e
}

// failConversion 接管任何转换错误（契约 §1.3）：partial 事件照常写流，随后产出唯一
// response.failed 终态并向上返回错误；TerminalEvent 置位后后续 chunk（含 [DONE]）
// 全部丢弃，禁止合成 completed/incomplete 假成功（P0-1 stream）。SSE 已提交，
// 错误只经事件终态表达，不得再附加 JSON error body。
func (st *chatToResponsesState) failConversion(originalRequestRawJSON []byte, partial []string, convErr error) ([]string, error) {
	if st.ConversionError == nil {
		st.ConversionError = convErr
	}
	events, genErr := st.generateFailedEvents(originalRequestRawJSON, conversionModelError(convErr))
	out := append(append([]string{}, partial...), events...)
	if genErr != nil {
		return out, errors.Join(convErr, genErr)
	}
	return out, convErr
}

// checkToolCallLifecycle 校验 tool item 的 added/done 生命周期不变量（responses §7，P1-10）：
// 任何有内容（id / name / arguments 至少其一非空）却未成功 add（add 需 id+name 齐备）的
// call 都属上游畸形，close 时不得为其发 output_item.done，整流经 malformed_tool_call 走失败终态。
func (st *chatToResponsesState) checkToolCallLifecycle() error {
	idxs := make([]int, 0, len(st.FuncArgsBuf))
	for idx := range st.FuncArgsBuf {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		if st.FuncItemAdded[idx] {
			continue
		}
		hasContent := st.FuncCallIDs[idx] != "" || st.FuncNames[idx] != ""
		if buf := st.FuncArgsBuf[idx]; buf != nil && buf.Len() > 0 {
			hasContent = true
		}
		if hasContent {
			return errResponse(model.CodeMalformedToolCall, fmt.Sprintf("choices[0].delta.tool_calls[%d]", idx),
				fmt.Errorf("tool call delta registered without id or name: output_item.added was never emitted, closing it as done would orphan the item"))
		}
	}
	return nil
}

// captureCompletionTokensDetails 读取 usage 输出细分对象（Chat §8.2 completion_tokens_details，
// 兼容 Responses 命名 output_tokens_details）全字段进 state，并留存原始对象供终态与非流式
// normalizeOutputDetails 同源透传（零值子字段同样保留，保证流式/非流式字段集合一致）。
// 返回是否存在该对象。
func captureCompletionTokensDetails(details gjson.Result, st *chatToResponsesState) bool {
	if !details.Exists() || !details.IsObject() {
		return false
	}
	raw := make(map[string]interface{})
	details.ForEach(func(k, v gjson.Result) bool {
		raw[k.String()] = v.Value()
		return true
	})
	st.completionDetailsRaw = raw
	if v := details.Get("reasoning_tokens"); v.Exists() {
		st.CompletionDetails.ReasoningTokens = int(v.Int())
	}
	if v := details.Get("accepted_prediction_tokens"); v.Exists() {
		st.CompletionDetails.AcceptedPredictionTokens = int(v.Int())
	}
	if v := details.Get("rejected_prediction_tokens"); v.Exists() {
		st.CompletionDetails.RejectedPredictionTokens = int(v.Int())
	}
	if v := details.Get("audio_tokens"); v.Exists() {
		st.CompletionDetails.AudioTokens = int(v.Int())
	}
	if v := details.Get("text_tokens"); v.Exists() {
		st.CompletionDetails.TextTokens = int(v.Int())
	}
	return true
}

// terminalRequestEchoKeys 是终态快照回显的原请求字段清单，与 T6 非流式
// ConvertChatResponseToResponsesWithContext 的回显清单逐项一致（P2-3 流式/非流式统一）。
var terminalRequestEchoKeys = []string{
	"instructions", "max_output_tokens", "parallel_tool_calls", "reasoning",
	"temperature", "tool_choice", "tools", "top_p", "metadata", "text",
	"modalities", "store", "service_tier", "previous_response_id",
}

// buildTerminalResponseSnapshot 构建终态事件（completed/incomplete/failed）response 对象的基础快照
// （responses §4）：网关身份字段 + 原请求可回显字段（含 text/modalities/store/service_tier/user，
// 修复 P2-3 只回显部分字段）+ usage details（与非流式同源映射）。status/error/output/
// incomplete_details/truncated 由具体终态生成方按语义填充。
// originalRequestRawJSON 非法 JSON 显式返回错误，不静默丢回显（与 T6 同口径）。
func buildTerminalResponseSnapshot(originalRequestRawJSON []byte, st *chatToResponsesState) (map[string]interface{}, error) {
	snapshot := map[string]interface{}{
		"id":                   st.ResponseID,
		"object":               "response",
		"created_at":           st.CreatedAt,
		"background":           false,
		"error":                nil,
		"incomplete_details":   nil,
		"previous_response_id": nil,
		"user":                 nil,
		"truncated":            false,
		"usage":                buildStreamUsage(st),
	}
	if originalRequestRawJSON == nil {
		return snapshot, nil
	}
	var req map[string]interface{}
	if err := json.Unmarshal(originalRequestRawJSON, &req); err != nil || req == nil {
		return nil, errResponse(model.CodeInvalidSourceJSON, "original_request",
			errors.New("original responses request is not a valid JSON object"))
	}
	for _, key := range terminalRequestEchoKeys {
		if v, ok := req[key]; ok {
			snapshot[key] = v
		}
	}
	if v, ok := req["model"]; ok {
		snapshot["model"] = v
	}
	if v, ok := req["user"]; ok && v != nil {
		snapshot["user"] = v
	}
	return snapshot, nil
}

// buildStreamUsage 把流式累积状态映射为 responses §6 usage 对象。
// completion details 走与非流式 normalizeOutputDetails 相同的字段映射（P1-9）：
// 上游提供过 completion_tokens_details / output_tokens_details 时透传原始对象并保证
// reasoning_tokens 存在；reasoning_tokens 只来自上游统计，缺失为 0，无字节估算（P0-4）。
func buildStreamUsage(st *chatToResponsesState) map[string]interface{} {
	inputTokens := st.InputTokens
	total := st.TotalTokens
	needTotalRecalc := st.HasClaudeCacheFields && (st.CachedTokens > 0 || effectiveCacheCreationTokens(st.CacheCreationTokens, st.CacheCreation5mTokens, st.CacheCreation1hTokens) > 0)
	if total == 0 || needTotalRecalc {
		if st.HasClaudeCacheFields {
			// Claude 上游：input_tokens 不含 cache，total 需汇集 cache 三件套（与上游 total 口径一致）
			total = calculateClaudeTotalTokens(
				inputTokens,
				st.OutputTokens,
				st.CachedTokens,
				st.CacheCreationTokens,
				st.CacheCreation5mTokens,
				st.CacheCreation1hTokens,
			)
		} else {
			// OpenAI 上游：input_tokens 已含 cached，total = input + output（responses §6）
			total = inputTokens + st.OutputTokens
		}
	}

	// 输入口径与 docs/responses-protocol.md §6 及非流式 parseUsage 对齐：
	// input_tokens 为总输入（含 cached），cached 命中数仅经 input_tokens_details.cached_tokens 单独表达。
	// details 对象恒存在（responses §6 必填），值为 0 也输出。
	outputDetails := map[string]interface{}{"reasoning_tokens": st.ReasoningTokens}
	if st.completionDetailsSeen && st.completionDetailsRaw != nil {
		merged := make(map[string]interface{}, len(st.completionDetailsRaw)+1)
		for k, v := range st.completionDetailsRaw {
			merged[k] = v
		}
		if _, ok := merged["reasoning_tokens"]; !ok {
			merged["reasoning_tokens"] = st.ReasoningTokens
		}
		// 非流式同源函数保证返回 map[string]interface{}（reasoning_tokens 恒存在）
		if normalized, ok := normalizeOutputDetails(merged).(map[string]interface{}); ok {
			outputDetails = normalized
		} else {
			outputDetails = merged
		}
	}

	usage := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": st.OutputTokens,
		"total_tokens":  total,
		"input_tokens_details": map[string]interface{}{
			"cached_tokens":      st.CachedTokens,
			"cache_write_tokens": st.CacheWriteTokens,
		},
		"output_tokens_details": outputDetails,
	}
	// Claude 缓存 TTL 细分字段（与非流式 parseUsage 同口径：有值才输出）
	if st.CacheCreationTokens > 0 {
		usage["cache_creation_input_tokens"] = st.CacheCreationTokens
	}
	if st.CacheCreation5mTokens > 0 {
		usage["cache_creation_5m_input_tokens"] = st.CacheCreation5mTokens
	}
	if st.CacheCreation1hTokens > 0 {
		usage["cache_creation_1h_input_tokens"] = st.CacheCreation1hTokens
	}
	if st.HasClaudeCacheFields && st.CachedTokens > 0 {
		usage["cache_read_input_tokens"] = st.CachedTokens
	}
	if st.CacheTTL != "" {
		usage["cache_ttl"] = st.CacheTTL
	}
	return usage
}

// marshalNoEscapeJSON 序列化快照为紧凑 JSON，不转义 <>&（保持与既有 sjson 路径字节级内容一致，
// 避免 instructions/tools 里的 URL 查询符被改写）。
func marshalNoEscapeJSON(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ConvertOpenAIChatToResponses 将 OpenAI Chat Completions SSE 转换为 Responses SSE 事件。
// 旧 5 参数入口，作为 thin wrapper 调用 ConvertOpenAIChatToResponsesWithContext 并传 nil 作为
// originalRequestRawJSON，保持对历史调用方的 100% 行为兼容。
// originalRequestRawJSON: 原始的 Responses API 请求 JSON（用于回显字段）
// requestRawJSON: 转换后的 Chat Completions 请求 JSON
// rawJSON: OpenAI Chat Completions SSE 行
// param: 状态指针（*any，在多次调用间保持状态）
// 返回的 error 为协议转换显式边界（契约 §1.3）：畸形 chunk / 畸形工具调用等，调用方必须消费，
// 经错误终态链处理，禁止 swallow。
func ConvertOpenAIChatToResponses(originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any, fallbackReasoningToMessage bool) ([]string, error) {
	return ConvertOpenAIChatToResponsesWithContext(originalRequestRawJSON, requestRawJSON, rawJSON, param, fallbackReasoningToMessage)
}

// ConvertOpenAIChatToResponsesWithContext 将 OpenAI Chat Completions SSE 转换为 Responses SSE 事件。
// 当 originalRequestRawJSON 非 nil 时，从原始 Responses 请求里解析 CodexToolContext，
// 用于在 tool_calls 流式 output_item.added 事件中携带正确的 name/namespace/custom_tool_call 类型。
// 当 originalRequestRawJSON 为 nil 时，退化到无 CodexCtx 行为（name 保留原上游名，无 namespace 字段）。
// #5 修复：流式 output_item.added 事件必须在 first chunk 时就带 name/namespace/input，与 #4 非流式对称。
// originalRequestRawJSON: 原始的 Responses API 请求 JSON
// requestRawJSON: 转换后的 Chat Completions 请求 JSON
// rawJSON: OpenAI Chat Completions SSE 行
// param: 状态指针（*any，在多次调用间保持状态）
// fallbackReasoningToMessage: 兜底无 content 仅 reasoning 时复制文本为 message
//
// 返回 ([]string, error)：error 为协议转换显式边界（契约 §1.3）——非 [DONE] chunk 未通过
// JSON validity/object 校验返回 invalid_stream_event；工具 delta 缺 id/name 未成功 added
// 却在 close 时返回 malformed_tool_call；两者都已产出生成唯一 response.failed 终态并置
// TerminalEvent，后续 [DONE] 不得合成 completed。调用方（forwarder）必须消费该 error。
func ConvertOpenAIChatToResponsesWithContext(originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any, fallbackReasoningToMessage bool) ([]string, error) {
	var st *chatToResponsesState
	if param == nil {
		st = &chatToResponsesState{
			FuncArgsBuf:   make(map[int]*strings.Builder),
			FuncNames:     make(map[int]string),
			FuncCallIDs:   make(map[int]string),
			FuncItemAdded: make(map[int]bool),
			FirstChunk:    true,
		}
	} else if *param == nil {
		st = &chatToResponsesState{
			FuncArgsBuf:   make(map[int]*strings.Builder),
			FuncNames:     make(map[int]string),
			FuncCallIDs:   make(map[int]string),
			FuncItemAdded: make(map[int]bool),
			FirstChunk:    true,
		}
		*param = st
	} else {
		var ok bool
		st, ok = (*param).(*chatToResponsesState)
		if !ok {
			st = &chatToResponsesState{
				FuncArgsBuf:   make(map[int]*strings.Builder),
				FuncNames:     make(map[int]string),
				FuncCallIDs:   make(map[int]string),
				FuncItemAdded: make(map[int]bool),
				FirstChunk:    true,
			}
			*param = st
		}
	}

	st.FallbackReasoningToMessage = fallbackReasoningToMessage

	// 期望 `data: {..}` 格式
	if !bytes.HasPrefix(rawJSON, chatDataTag) {
		return []string{}, nil
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	// 一旦已经产生终态事件，后续普通 chunk 和迟到终态都直接丢弃，避免破坏终态状态机。
	if st.TerminalEvent != "" {
		return []string{}, nil
	}

	// 检查 [DONE] 标记
	if string(rawJSON) == "[DONE]" {
		// 生成完成事件（存在 ConversionError 时产出 failed 终态，不得合成 completed，P0-1）
		return st.generateCompletedEvents(originalRequestRawJSON)
	}

	// 空 data 载荷属保活帧而非 Chat chunk（chat §7.1 每 chunk 必为 JSON object），跳过不初始化响应
	if len(rawJSON) == 0 {
		return []string{}, nil
	}

	// 非 [DONE] chunk 必须先通过 JSON validity + object 形状校验（chat §7.1，P0-1 stream）：
	// gjson 对无效载荷静默返回空结果会让畸形 chunk 触发 created/in_progress 初始化并在 [DONE]
	// 时合成成功终态，显式拒绝并转唯一 response.failed。
	if !json.Valid(rawJSON) {
		return st.failConversion(originalRequestRawJSON, nil,
			errResponse(model.CodeInvalidStreamEvent, "data", fmt.Errorf("upstream chat stream chunk is not valid JSON")))
	}
	root := gjson.ParseBytes(rawJSON)
	if !root.IsObject() {
		return st.failConversion(originalRequestRawJSON, nil,
			errResponse(model.CodeInvalidStreamEvent, "data", fmt.Errorf("upstream chat stream chunk is not a JSON object")))
	}
	var out []string

	nextSeq := func() int { st.Seq++; return st.Seq }

	if errResp := parseChatStreamError(root); errResp != nil {
		return st.generateFailedEvents(originalRequestRawJSON, errResp)
	}

	// 处理首次 chunk - 初始化并生成 response.created 和 response.in_progress
	if st.FirstChunk {
		st.FirstChunk = false
		// 从 chunk 中提取 id
		if id := root.Get("id"); id.Exists() {
			st.ResponseID = id.String()
		} else {
			st.ResponseID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
		}
		// P2-2：首个合法 Chat chunk 的 created 优先作为 Responses created_at（chat §7.1 必填、
		// responses §4），缺失/非正数才回落网关时间
		st.CreatedAt = time.Now().Unix()
		if v := root.Get("created"); v.Exists() && v.Type == gjson.Number && v.Int() > 0 {
			st.CreatedAt = v.Int()
		}

		// 重置状态
		st.TextBuf.Reset()
		st.RefusalBuf.Reset()
		st.InRefusalBlock = false
		st.CurrentRefusalMsgID = ""
		st.CurrentRefusalOutputIndex = 0
		st.CurrentTextOutputIndex = 0
		st.RefusalItemInOutput = false
		st.RefusalAppendedToMsg = false
		st.WarnedTextDropAfterRefusal = false
		st.RefusalViolationSeen = false
		st.FinishReason = ""
		st.ReasoningBuf.Reset()
		st.ReasoningActive = false
		st.InTextBlock = false
		st.InFuncBlock = false
		st.CurrentMsgID = ""
		st.CurrentFCID = ""
		st.ReasoningItemID = ""
		st.ReasoningIndex = 0
		st.ReasoningPartAdded = false
		st.Think.Reset()
		st.FuncArgsBuf = make(map[int]*strings.Builder)
		st.FuncNames = make(map[int]string)
		st.FuncCallIDs = make(map[int]string)
		st.FuncItemAdded = make(map[int]bool)
		st.NextAnonToolIdx = 0
		st.AnonToolCallIdx = 0
		st.AnonToolCallActive = false
		st.WarnedAnonToolDrop = false
		st.AnonToolKeys = make(map[int]bool)
		st.TextParts = nil
		st.TextPartBuf.Reset()
		st.TextPartCount = 0
		st.CurrentTextContentIndex = 0
		st.InputTokens = 0
		st.OutputTokens = 0
		st.TerminalEvent = ""
		// 与 TerminalEvent 对称重置：state 复用跑第二段流时，残留的上一段转换错误不得
		// 强制新流 [DONE] 走 failed（生产路径每请求新建 state，此处锁状态不变量）
		st.ConversionError = nil
		st.CachedTokens = 0
		st.CacheWriteTokens = 0
		st.ReasoningTokens = 0
		st.CacheCreationTokens = 0
		st.CacheCreation5mTokens = 0
		st.CacheCreation1hTokens = 0
		st.CacheTTL = ""
		st.UsageSeen = false
		st.CompletionDetails = model.CompletionTokensDetails{}
		st.completionDetailsSeen = false
		st.completionDetailsRaw = nil

		st.ensureCodexToolContext(originalRequestRawJSON)

		// 发送 response.created
		created := `{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"instructions":""}}`
		created, _ = sjson.Set(created, "sequence_number", nextSeq())
		created, _ = sjson.Set(created, "response.id", st.ResponseID)
		created, _ = sjson.Set(created, "response.created_at", st.CreatedAt)
		out = append(out, emitResponsesEvent("response.created", created))

		// 发送 response.in_progress
		inprog := `{"type":"response.in_progress","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress"}}`
		inprog, _ = sjson.Set(inprog, "sequence_number", nextSeq())
		inprog, _ = sjson.Set(inprog, "response.id", st.ResponseID)
		inprog, _ = sjson.Set(inprog, "response.created_at", st.CreatedAt)
		out = append(out, emitResponsesEvent("response.in_progress", inprog))
	}

	// 处理 usage（完整支持多格式详细字段）
	// 必须先于 choices 早退判断执行：部分 OpenAI 兼容上游收尾 chunk 只带 usage 不带 choices，
	// 放在早退之后会漏解析导致网关记 0 token（资损方向）
	if usage := root.Get("usage"); usage.Exists() {
		st.UsageSeen = true

		// OpenAI 格式基础字段
		if v := usage.Get("prompt_tokens"); v.Exists() {
			st.InputTokens = v.Int()
		}
		if v := usage.Get("completion_tokens"); v.Exists() {
			st.OutputTokens = v.Int()
		}
		if v := usage.Get("total_tokens"); v.Exists() {
			st.TotalTokens = v.Int()
		}

		// OpenAI 格式详细字段（chat §8.1 cache_write_tokens → responses §6 input_tokens_details.cache_write_tokens）
		if v := usage.Get("prompt_tokens_details.cached_tokens"); v.Exists() {
			st.CachedTokens = v.Int()
		}
		if v := usage.Get("prompt_tokens_details.cache_write_tokens"); v.Exists() {
			st.CacheWriteTokens = v.Int()
		}
		// completion details 全字段采集（chat §8.2 → responses §6 output_tokens_details，P1-9）：
		// reasoning/accepted/rejected/audio/text 与非流式 normalizeOutputDetails 同源映射；
		// reasoning_tokens 仅取上游值，缺失即 0，绝不由 ReasoningBuf 长度估算（P0-4）。
		st.completionDetailsSeen = captureCompletionTokensDetails(usage.Get("completion_tokens_details"), st) ||
			st.completionDetailsSeen
		if !st.completionDetailsSeen && usage.Get("output_tokens_details").Exists() {
			st.completionDetailsSeen = captureCompletionTokensDetails(usage.Get("output_tokens_details"), st)
		}
		if st.CompletionDetails.ReasoningTokens != 0 {
			st.ReasoningTokens = int64(st.CompletionDetails.ReasoningTokens)
		}

		// Claude 格式基础字段（优先级高于 OpenAI）
		if v := usage.Get("input_tokens"); v.Exists() {
			st.InputTokens = v.Int()
		}
		if v := usage.Get("output_tokens"); v.Exists() {
			st.OutputTokens = v.Int()
		}

		// Claude 格式缓存字段
		if v := usage.Get("cache_read_input_tokens"); v.Exists() {
			st.CachedTokens = v.Int()
			st.HasClaudeCacheFields = true
		}
		if v := usage.Get("cache_creation_input_tokens"); v.Exists() {
			st.CacheCreationTokens = v.Int()
			st.HasClaudeCacheFields = true
		}
		if v := usage.Get("cache_creation_5m_input_tokens"); v.Exists() {
			st.CacheCreation5mTokens = v.Int()
			st.HasClaudeCacheFields = true
		}
		if v := usage.Get("cache_creation_1h_input_tokens"); v.Exists() {
			st.CacheCreation1hTokens = v.Int()
			st.HasClaudeCacheFields = true
		}

		// 设置缓存 TTL 标识
		has5m := st.CacheCreation5mTokens > 0
		has1h := st.CacheCreation1hTokens > 0
		if has5m && has1h {
			st.CacheTTL = "mixed"
		} else if has1h {
			st.CacheTTL = "1h"
		} else if has5m {
			st.CacheTTL = "5m"
		}
	}

	// 解析 choices
	choices := root.Get("choices")
	if !choices.Exists() || !choices.IsArray() {
		return out, nil
	}

	for _, choice := range choices.Array() {
		finishReason := choice.Get("finish_reason").String()

		// 持久化最后一个非空 finish_reason：length → response.incomplete 语义（chat §6.2 → responses §4）。
		// 必须在 delta 存在性检查之前读取：部分上游的收尾 chunk 只带 finish_reason 不带 delta，
		// 提前跳过会丢失 length 语义导致终态误判为 completed。
		if finishReason != "" && finishReason != "null" {
			st.FinishReason = finishReason
		}

		delta := choice.Get("delta")
		if !delta.Exists() {
			continue
		}

		// 处理 reasoning_content（OpenAI o1 模型的原生 reasoning 字段）
		if reasoning := delta.Get("reasoning_content"); reasoning.Exists() && reasoning.String() != "" {
			out = append(out, st.handleReasoningPart(reasoning.String(), nextSeq)...)
		}

		// 处理 content（文本内容）：先经过 <think> 状态机分流到 reasoning / content
		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			reasoningParts, contentParts := st.Think.Feed(content.String())
			for _, rp := range reasoningParts {
				out = append(out, st.handleReasoningPart(rp, nextSeq)...)
			}
			for _, cp := range contentParts {
				out = append(out, st.handleContentPart(cp, nextSeq)...)
			}
		}

		// 处理 refusal（chat §7.2 delta.refusal → responses §7 response.refusal.delta/done）。
		// 按 chat §4 refusal 与 text 互斥，handleRefusalPart 在需要时会先关闭 text/reasoning 块。
		if refusal := delta.Get("refusal"); refusal.Exists() && refusal.String() != "" {
			out = append(out, st.handleRefusalPart(refusal.String(), nextSeq)...)
		}

		// 处理 tool_calls
		if toolCalls := delta.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
			out = append(out, st.flushThinkTagBuf(nextSeq)...)
			if st.PendingTextBuf.Len() > 0 && !st.shouldFallbackReasoning() {
				out = append(out, st.flushPendingWhitespace(nextSeq)...)
			}
			for _, tc := range toolCalls.Array() {
				idxNode := tc.Get("index")
				tcCallID := tc.Get("id").String()
				idx := int(idxNode.Int())
				if !idxNode.Exists() || idxNode.Type == gjson.Null {
					// 上游畸形：delta.tool_calls 缺 index。禁止回落到 0 —— 同流多个 call 会
					// 互相覆盖 FuncArgsBuf/FuncNames/FuncCallIDs 导致 arguments 串包。
					key, ok := st.anonToolCallIdx(tcCallID)
					if !ok {
						if !st.WarnedAnonToolDrop {
							st.WarnedAnonToolDrop = true
							logger.Log.Warnf("drop tool_calls delta without index and with no anonymous call to correlate: response_id=%s", st.ResponseID)
						}
						continue
					}
					idx = key
				} else if st.AnonToolKeys[idx] && st.FuncCallIDs[idx] != tcCallID {
					// 反向争用：显式 index 落在匿名分配的 key 上，且 call id 与本 item 归属的匿名 call 不同。
					// 直接写入会让两个不同 call 共用 FuncArgsBuf（arguments 混流）并覆盖 FuncCallIDs（静默串包），
					// 故丢弃该 delta：畸形流丢一段优于两 call 混污。匿名 key 归属不被接管，
					// 该匿名 call 后续的无 index delta 仍落到自己那个 item。
					if !st.WarnedAnonToolDrop {
						st.WarnedAnonToolDrop = true
						logger.Log.Warnf("drop tool_calls delta whose explicit index collides with an anonymous call key: response_id=%s index=%d anon_call_id=%s incoming_call_id=%s",
							st.ResponseID, idx, st.FuncCallIDs[idx], tcCallID)
					}
					continue
				}

				// 如果 reasoning 还在活跃状态，先关闭它
				if st.ReasoningActive {
					out = append(out, st.closeReasoningBlock(nextSeq)...)
				}

				// 如果 text block 还在活跃状态，先关闭它
				if st.InTextBlock {
					out = append(out, st.closeTextBlock(nextSeq)...)
				}

				// 如果 refusal block 还在活跃状态，先关闭它（与 text 互斥）
				if st.InRefusalBlock {
					out = append(out, st.closeRefusalBlock(nextSeq)...)
				}

				// 初始化 tool call 状态
				if st.FuncArgsBuf[idx] == nil {
					st.FuncArgsBuf[idx] = &strings.Builder{}
				}

				// 处理 tool call ID
				if tcID := tc.Get("id"); tcID.Exists() && tcID.String() != "" {
					st.FuncCallIDs[idx] = tcID.String()
					st.CurrentFCID = tcID.String()

					// 开始新的 tool call item。
					// #5 修复：output_item.added 事件延迟到 function.name 到达时由
					// addToolCallItemIfNeeded 统一发射，确保 item.name / item.namespace /
					// custom_tool_call 类型在事件发出时已就位，与 #4 非流式对称。
					// 原 350 路径在收到 id 时立即发 name="" 的 output_item.added，会让
					// codex 客户端在收到事件时缺 name 而报错或挂起。
					st.InFuncBlock = true
				}

				// 处理 function
				if function := tc.Get("function"); function.Exists() {
					// 处理函数名
					if name := function.Get("name"); name.Exists() && name.String() != "" {
						st.FuncNames[idx] = name.String()
					}

					// 处理参数
					if args := function.Get("arguments"); args.Exists() && args.String() != "" {
						st.FuncArgsBuf[idx].WriteString(args.String())
						out = append(out, st.addToolCallItemIfNeeded(idx, nextSeq)...)

						if st.isCustomProxy(idx) {
							continue
						}
						if st.builtinToolKind(idx) != "" {
							out = append(out, st.emitBuiltinSearchQueryDelta(idx, args.String(), nextSeq))
							continue
						}

						// 计算 output_index
						outputIndex := st.customToolOutputIndex(idx)

						msg := `{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`
						msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
						msg, _ = sjson.Set(msg, "item_id", fmt.Sprintf("fc_%s", st.FuncCallIDs[idx]))
						msg, _ = sjson.Set(msg, "output_index", outputIndex)
						msg, _ = sjson.Set(msg, "delta", args.String())
						out = append(out, emitResponsesEvent("response.function_call_arguments.delta", msg))
					} else {
						out = append(out, st.addToolCallItemIfNeeded(idx, nextSeq)...)
					}
				}
				// tool 属非 text 内容块：处理完毕后复位「refusal 刚关闭」违规窗口。
				// 必须放在本分支 closeRefusalBlock 之后——refusal→tool→text 中 refusal 由 tool
				// 关闭触发的置位会被立即复位，后续 text 属「中间隔了其他块」的正常续写
				st.RefusalViolationSeen = false
			}
		}

		// 处理 finish_reason
		if finishReason != "" && finishReason != "null" {
			// 先把 think 状态机剩余 buffer 兜底刷出
			out = append(out, st.flushThinkTagBuf(nextSeq)...)
			if st.PendingTextBuf.Len() > 0 && !st.shouldFallbackReasoning() {
				out = append(out, st.flushPendingWhitespace(nextSeq)...)
			}
			// 关闭所有打开的 blocks
			if st.ReasoningActive {
				out = append(out, st.closeReasoningBlock(nextSeq)...)
			}
			if st.InTextBlock {
				out = append(out, st.closeTextBlock(nextSeq)...)
			}
			if st.InRefusalBlock {
				out = append(out, st.closeRefusalBlock(nextSeq)...)
			}
			if st.InFuncBlock {
				closed, cerr := st.closeFuncBlocks(nextSeq)
				if cerr != nil {
					// P1-10：未成功 added 的工具禁止在 close 时 done → 唯一 response.failed 终态
					return st.failConversion(originalRequestRawJSON, out, cerr)
				}
				out = append(out, closed...)
			}
		}
	}

	return out, nil
}

func parseChatStreamError(root gjson.Result) *model.Error {
	errNode := root.Get("error")
	if !errNode.Exists() || errNode.Type == gjson.Null {
		return nil
	}

	errResp := &model.Error{}
	if v := errNode.Get("message"); v.Exists() {
		errResp.Message = v.String()
	}
	if v := errNode.Get("type"); v.Exists() {
		errResp.Type = v.String()
	}
	if v := errNode.Get("param"); v.Exists() {
		errResp.Param = v.String()
	}
	if v := errNode.Get("code"); v.Exists() {
		errResp.Code = v.Value()
	}

	if errResp.Message == "" && errResp.Type == "" && errResp.Param == "" && errResp.Code == nil {
		return nil
	}

	return errResp
}

// handleReasoningPart 发射 reasoning 块相关事件，并维护 ReasoningActive/ReasoningBuf 等状态
func (st *chatToResponsesState) handleReasoningPart(reasoningText string, nextSeq func() int) []string {
	if reasoningText == "" {
		return nil
	}
	// reasoning 属非 text 内容块：到达即复位「refusal 刚关闭」违规窗口，
	// 其后的 refusal→reasoning→text 不再按「紧随 refusal 的违规 text」处理
	st.RefusalViolationSeen = false
	var out []string

	// 开始 reasoning block
	if !st.ReasoningActive {
		st.ReasoningActive = true
		st.ReasoningIndex = 0
		// 已占独立 item 数推导顺延：refusal 独立 message item、text message item（打开中或已关闭）
		// 均需让位，保证 refusal→text→reasoning 三重乱序时 reasoning 不撞 text 的 index，
		// 与终态数组还原（各 item 用各自快照 index 排序）一致
		if st.RefusalItemInOutput {
			st.ReasoningIndex++
		}
		if st.CurrentMsgID != "" {
			st.ReasoningIndex++
		}
		st.ReasoningBuf.Reset()
		st.ReasoningItemID = fmt.Sprintf("rs_%s_%d", st.ResponseID, st.ReasoningIndex)

		// response.output_item.added for reasoning
		item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","summary":[]}}`
		item, _ = sjson.Set(item, "sequence_number", nextSeq())
		item, _ = sjson.Set(item, "output_index", st.ReasoningIndex)
		item, _ = sjson.Set(item, "item.id", st.ReasoningItemID)
		out = append(out, emitResponsesEvent("response.output_item.added", item))

		// response.reasoning_summary_part.added
		part := `{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`
		part, _ = sjson.Set(part, "sequence_number", nextSeq())
		part, _ = sjson.Set(part, "item_id", st.ReasoningItemID)
		part, _ = sjson.Set(part, "output_index", st.ReasoningIndex)
		out = append(out, emitResponsesEvent("response.reasoning_summary_part.added", part))
		st.ReasoningPartAdded = true
	}

	// 发送 reasoning delta
	st.ReasoningBuf.WriteString(reasoningText)
	msg := `{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`
	msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
	msg, _ = sjson.Set(msg, "item_id", st.ReasoningItemID)
	msg, _ = sjson.Set(msg, "output_index", st.ReasoningIndex)
	msg, _ = sjson.Set(msg, "text", reasoningText)
	out = append(out, emitResponsesEvent("response.reasoning_summary_text.delta", msg))
	return out
}

func (st *chatToResponsesState) shouldDelayLeadingWhitespace(contentText string) bool {
	return !st.InTextBlock &&
		st.TextBuf.Len() == 0 &&
		st.CurrentMsgID == "" &&
		strings.TrimSpace(contentText) == "" &&
		st.FallbackReasoningToMessage &&
		(st.ReasoningPartAdded || st.ReasoningActive || st.ReasoningBuf.Len() > 0)
}

// textOutputIndex 返回 text 块的 output_index：reasoning/refusal 独立 item 已占据
// 更小 index 时顺延（与 handleReasoningPart 的 refusal 顺延同构），保证跨 item 唯一。
func (st *chatToResponsesState) textOutputIndex() int {
	idx := 0
	if st.ReasoningPartAdded {
		idx = 1
	}
	if st.RefusalItemInOutput {
		idx++
	}
	return idx
}

func (st *chatToResponsesState) emitContentPart(contentText string, nextSeq func() int) []string {
	if contentText == "" {
		return nil
	}
	var out []string

	// 违规交错防御，最终判定条件 —— 新 text 段（text 块未打开且本流已建过 message item）满足任一即丢弃：
	//  1. InRefusalBlock：refusal 增量仍在进行中来了 text（chat §4 互斥已被上游破坏，无法给出无矛盾事件序）；
	//  2. RefusalAppendedToMsg：该 message item 已挂 refusal part（追加模式），再追加 text part 会违反
	//     「output_text 在前、refusal 在后」的还原顺序（防御路径，粘滞判定是有意为之）；
	//  3. RefusalViolationSeen：refusal 刚关闭的一次性窗口内直接命中 text（如 finish 关闭 refusal 后
	//     仍来的迟到 text 段）；中途有任何非 text 内容块到达即复位，不算「紧随 refusal 关闭」。
	// 三种情况都与非流式侧「drop text part after refusal part」（responses_to_chat.go）同构：丢弃并一次性 Warn。
	// refusal→text 首段属正常顺延：此时 text item 尚不存在（CurrentMsgID==""，判定不成立）或违规标志
	// 已被 tool/refusal 重开复位，故 refusal→text→tool→text 的第二段 text 保留为同 item 的新 output_text part。
	// 放在关闭 refusal 块之前：丢弃时保留 refusal 累积。
	if !st.InTextBlock && st.CurrentMsgID != "" &&
		(st.InRefusalBlock || st.RefusalAppendedToMsg || st.RefusalViolationSeen) {
		if !st.WarnedTextDropAfterRefusal {
			st.WarnedTextDropAfterRefusal = true
			logger.Log.Warnf("drop text content directly following refusal delta (chat §4 mutual exclusion violated by upstream): response_id=%s dropped_len=%d", st.ResponseID, len(contentText))
		}
		return out
	}

	// 如果 reasoning 还在活跃状态，先关闭它
	if st.ReasoningActive {
		out = append(out, st.closeReasoningBlock(nextSeq)...)
	}

	// 如果 refusal 块还在活跃状态，先关闭它（refusal→text 乱序，chat §4 互斥）
	if st.InRefusalBlock {
		out = append(out, st.closeRefusalBlock(nextSeq)...)
	}

	// 开始 text block
	if !st.InTextBlock {
		st.InTextBlock = true
		st.TextPartBuf.Reset()
		if st.CurrentMsgID == "" {
			outputIndex := st.textOutputIndex()
			st.CurrentTextOutputIndex = outputIndex // 快照：此后 delta/done/终态排序复用，不随 reasoning 后到重算
			st.CurrentMsgID = fmt.Sprintf("msg_%s_%d", st.ResponseID, outputIndex)

			// response.output_item.added for message（仅首段建 item；重开时复用同一 item 不再发 added）
			item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`
			item, _ = sjson.Set(item, "sequence_number", nextSeq())
			item, _ = sjson.Set(item, "output_index", st.CurrentTextOutputIndex)
			item, _ = sjson.Set(item, "item.id", st.CurrentMsgID)
			out = append(out, emitResponsesEvent("response.output_item.added", item))
		}
		// 每一段 text 都是该 message item 上一个独立 output_text part：content_index 递增、
		// item id 与 output_index 沿用首段快照，避免重开时同 ID 再发 output_item.added
		st.CurrentTextContentIndex = st.TextPartCount
		st.TextPartCount++

		// response.content_part.added
		part := `{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`
		part, _ = sjson.Set(part, "sequence_number", nextSeq())
		part, _ = sjson.Set(part, "item_id", st.CurrentMsgID)
		part, _ = sjson.Set(part, "output_index", st.CurrentTextOutputIndex)
		part, _ = sjson.Set(part, "content_index", st.CurrentTextContentIndex)
		out = append(out, emitResponsesEvent("response.content_part.added", part))
	}

	// 发送 text delta
	st.TextBuf.WriteString(contentText)
	st.TextPartBuf.WriteString(contentText)
	outputIndex := st.CurrentTextOutputIndex // 复用打开时快照，保证同一 item 事件 index 恒定
	msg := `{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`
	msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
	msg, _ = sjson.Set(msg, "item_id", st.CurrentMsgID)
	msg, _ = sjson.Set(msg, "output_index", outputIndex)
	msg, _ = sjson.Set(msg, "content_index", st.CurrentTextContentIndex)
	msg, _ = sjson.Set(msg, "delta", contentText)
	out = append(out, emitResponsesEvent("response.output_text.delta", msg))
	return out
}

func (st *chatToResponsesState) flushPendingWhitespace(nextSeq func() int) []string {
	if st.PendingTextBuf.Len() == 0 {
		return nil
	}
	pending := st.PendingTextBuf.String()
	st.PendingTextBuf.Reset()
	return st.emitContentPart(pending, nextSeq)
}

// handleContentPart 发射 text 块相关事件，并维护 InTextBlock/TextBuf 等状态
func (st *chatToResponsesState) handleContentPart(contentText string, nextSeq func() int) []string {
	if contentText == "" {
		return nil
	}
	if st.shouldDelayLeadingWhitespace(contentText) {
		st.PendingTextBuf.WriteString(contentText)
		return nil
	}
	var out []string
	out = append(out, st.flushPendingWhitespace(nextSeq)...)
	out = append(out, st.emitContentPart(contentText, nextSeq)...)
	return out
}

// flushThinkTagBuf 刷新 <think> 标签状态机的尾部缓冲（用于流结束兜底）。
// 把残留文本按状态归到 reasoning 或 content 通道并发送对应事件。
func (st *chatToResponsesState) flushThinkTagBuf(nextSeq func() int) []string {
	remaining, toReasoning := st.Think.Drain()
	if remaining == "" {
		return nil
	}
	if toReasoning {
		return st.handleReasoningPart(remaining, nextSeq)
	}
	return st.handleContentPart(remaining, nextSeq)
}

// closeReasoningBlock 关闭 reasoning block
func (st *chatToResponsesState) closeReasoningBlock(nextSeq func() int) []string {
	if !st.ReasoningActive {
		return nil
	}

	var out []string
	full := st.ReasoningBuf.String()

	// response.reasoning_summary_text.done
	textDone := `{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`
	textDone, _ = sjson.Set(textDone, "sequence_number", nextSeq())
	textDone, _ = sjson.Set(textDone, "item_id", st.ReasoningItemID)
	textDone, _ = sjson.Set(textDone, "output_index", st.ReasoningIndex)
	textDone, _ = sjson.Set(textDone, "text", full)
	out = append(out, emitResponsesEvent("response.reasoning_summary_text.done", textDone))

	// response.reasoning_summary_part.done
	partDone := `{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`
	partDone, _ = sjson.Set(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.Set(partDone, "item_id", st.ReasoningItemID)
	partDone, _ = sjson.Set(partDone, "output_index", st.ReasoningIndex)
	partDone, _ = sjson.Set(partDone, "part.text", full)
	out = append(out, emitResponsesEvent("response.reasoning_summary_part.done", partDone))

	// response.output_item.done for reasoning
	itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"completed","summary":[]}}`
	itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
	itemDone, _ = sjson.Set(itemDone, "output_index", st.ReasoningIndex)
	itemDone, _ = sjson.Set(itemDone, "item.id", st.ReasoningItemID)
	itemDone, _ = sjson.Set(itemDone, "item.summary", []interface{}{map[string]interface{}{"type": "summary_text", "text": full}})
	out = append(out, emitResponsesEvent("response.output_item.done", itemDone))

	st.ReasoningActive = false
	return out
}

// closeTextBlock 关闭 text block
func (st *chatToResponsesState) closeTextBlock(nextSeq func() int) []string {
	if !st.InTextBlock {
		return nil
	}

	var out []string
	// 复用打开时记录的 index 快照：reasoning 后到（refusal→text→reasoning 乱序）时按
	// ReasoningPartAdded 重算会与打开时的 index 矛盾（added/delta 与 done 不一致），
	// 快照保证同一 item 的事件 output_index 恒定
	outputIndex := st.CurrentTextOutputIndex
	contentIndex := st.CurrentTextContentIndex
	// done 系列按「当前段」而非全文：多段 text（refusal→text→tool→text）各自对应一个
	// output_text part，全文塞进每段的 done 会让各 part 内容重复
	partText := st.TextPartBuf.String()

	// response.output_text.done
	done := `{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`
	done, _ = sjson.Set(done, "sequence_number", nextSeq())
	done, _ = sjson.Set(done, "item_id", st.CurrentMsgID)
	done, _ = sjson.Set(done, "output_index", outputIndex)
	done, _ = sjson.Set(done, "content_index", contentIndex)
	done, _ = sjson.Set(done, "text", partText)
	out = append(out, emitResponsesEvent("response.output_text.done", done))

	// response.content_part.done
	partDone := `{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`
	partDone, _ = sjson.Set(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.Set(partDone, "item_id", st.CurrentMsgID)
	partDone, _ = sjson.Set(partDone, "output_index", outputIndex)
	partDone, _ = sjson.Set(partDone, "content_index", contentIndex)
	partDone, _ = sjson.Set(partDone, "part.text", partText)
	out = append(out, emitResponsesEvent("response.content_part.done", partDone))

	// 归档本段，供后续段与终态数组按 content part 顺序还原
	st.TextParts = append(st.TextParts, partText)

	// 段关闭不发 item 级 output_item.done：协议 docs/responses-protocol.md §7 output item
	// 事件语义是每个 item 恰好一条 added/done（ResponseOutputItemDoneEvent 的 marked done
	// 为一次性状态转换，done 携带该 item 最终完整 content）。text 段关闭不代表 message item
	// 定稿——后续 text 段/追加 refusal 仍会增补内容，唯一 done 由统一终态补发点
	// emitMessageItemDoneEvent 在终态事件前按与 response.output 同源快照发出。
	// part 级 output_text.done + content_part.done 每段各一条，与 item 级 done 无关，保留。

	st.InTextBlock = false
	return out
}

// messageTextContentParts 按 content_index 顺序构造 message item 的完整 content part 列表：
// 已归档 text 段 + text 块仍打开时缓冲中的当前段（InTextBlock 门控，防止与「先归档再取」双计）
// + 追加模式的 refusal part（恒排在全部 output_text part 之后，下标即 refusalContentIndex）。
// 唯一 output_item.done（统一终态补发点 emitMessageItemDoneEvent）与终态 response.output 一律复用本函数现取，
// 保证 done 恒为该 item 的最终完整内容（协议 docs/responses-protocol.md §7 output item
// 事件：done 携带最终完整 content）。
func (st *chatToResponsesState) messageTextContentParts() []interface{} {
	parts := make([]interface{}, 0, len(st.TextParts)+2)
	for _, text := range st.TextParts {
		parts = append(parts, map[string]interface{}{
			"type":        "output_text",
			"annotations": []interface{}{},
			"logprobs":    []interface{}{},
			"text":        text,
		})
	}
	if st.InTextBlock && st.TextPartBuf.Len() > 0 {
		parts = append(parts, map[string]interface{}{
			"type":        "output_text",
			"annotations": []interface{}{},
			"logprobs":    []interface{}{},
			"text":        st.TextPartBuf.String(),
		})
	}
	if st.RefusalAppendedToMsg && st.RefusalBuf.Len() > 0 {
		parts = append(parts, map[string]interface{}{
			"type":    "refusal",
			"refusal": st.RefusalBuf.String(),
		})
	}
	return parts
}

// emitMessageItemDoneEvent 为单个 message item 生成 output_item.done 事件。
// item 快照（type/id/role/status/content）必须传入终态 response.output 数组的同一构建
// 产物，保证每 item 恒且仅一条 done 且与终态天然同源一致。
// 协议 docs/responses-protocol.md §7：output item 事件按 added/done 成对标记生命周期，
// ResponseOutputItemDoneEvent 的 marked done 是一次性状态转换，done 携带该 item 最终
// 完整 content——故一切 message item done 仅在终态补发点发出，流中途 close* 不发。
func emitMessageItemDoneEvent(item map[string]interface{}, outputIndex int, nextSeq func() int) string {
	payload := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{}}`
	payload, _ = sjson.Set(payload, "sequence_number", nextSeq())
	payload, _ = sjson.Set(payload, "output_index", outputIndex)
	payload, _ = sjson.Set(payload, "item", item)
	return emitResponsesEvent("response.output_item.done", payload)
}

// handleRefusalPart 发射 refusal 块相关事件，并维护 InRefusalBlock/RefusalBuf 状态。
// 事件序列与文本块对称（responses §5/§7）：output_item.added(message) →
// content_part.added(refusal part) → response.refusal.delta。
// refusal 与 text 互斥（chat §4），若 text/reasoning 块尚在活跃则先关闭。
// 所有 refusal 事件使用打开时记录的 CurrentRefusalOutputIndex，close 时不按 ReasoningPartAdded 重算，
// 保证同一 item 的事件 output_index 恒定、跨 item 唯一。
func (st *chatToResponsesState) handleRefusalPart(refusalText string, nextSeq func() int) []string {
	if refusalText == "" {
		return nil
	}
	// refusal 到达（续写或重开）本身是非 text 内容块：复位上一轮「refusal 刚关闭」窗口；
	// 紧随重开 refusal 的违规 text 由 emitContentPart 守卫的 InRefusalBlock 分支兜住
	st.RefusalViolationSeen = false
	var out []string

	// 若 reasoning 或 text 块仍活跃，先关闭（chat §4 refusal 与 text 互斥）
	if st.ReasoningActive {
		out = append(out, st.closeReasoningBlock(nextSeq)...)
	}
	if st.InTextBlock {
		out = append(out, st.closeTextBlock(nextSeq)...)
	}

	// 开始 refusal block
	if !st.InRefusalBlock {
		st.InRefusalBlock = true
		switch {
		case st.RefusalItemInOutput:
			// 复用先前已定稿的独立 refusal item（refusal→text→refusal 交错）：
			// id 与 output_index 全部沿用其打开时快照（CurrentRefusalMsgID / CurrentRefusalOutputIndex
			// 在此之前不会被改写），既不重复发 output_item.added，也不按 ReasoningPartAdded 重算 index
			//（重算会与该 refusal item 已发事件矛盾，或撞 text item 的 index）。
			// 视为同一 refusal part 的续写：不发 content_part.added，文本并入 RefusalBuf，
			// close 时的 refusal.done / 终态 item 均以合并后全文为准。
		case st.CurrentMsgID != "":
			// 防御路径：text block 已关闭（互斥违反）后仍到 refusal —— 复用该 message item 追加 refusal part，
			// 对齐非流式 convertChatMessageToOutput 的合并行为（responses §5 允许 message content 含 refusal part），
			// 不新建同 ID 的 message item 导致 item id 重复。
			st.RefusalAppendedToMsg = true
			st.CurrentRefusalMsgID = st.CurrentMsgID
			// index 必须继承被复用 item 的快照：按 ReasoningPartAdded 重算会让同一 item_id 的
			// text 事件与 refusal 事件 output_index 互相矛盾
			st.CurrentRefusalOutputIndex = st.CurrentTextOutputIndex

			// response.content_part.added for refusal part（追加到既有 message item）
			part := `{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`
			part, _ = sjson.Set(part, "sequence_number", nextSeq())
			part, _ = sjson.Set(part, "item_id", st.CurrentRefusalMsgID)
			part, _ = sjson.Set(part, "output_index", st.CurrentRefusalOutputIndex)
			part, _ = sjson.Set(part, "content_index", st.refusalContentIndex())
			out = append(out, emitResponsesEvent("response.content_part.added", part))
		default:
			// 新建独立 refusal message item：reasoning 已占 0 时顺延 1
			st.CurrentRefusalOutputIndex = 0
			if st.ReasoningPartAdded {
				st.CurrentRefusalOutputIndex = 1
			}
			st.RefusalItemInOutput = true
			st.CurrentRefusalMsgID = fmt.Sprintf("msg_%s_%d", st.ResponseID, st.CurrentRefusalOutputIndex)

			// response.output_item.added for message
			item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`
			item, _ = sjson.Set(item, "sequence_number", nextSeq())
			item, _ = sjson.Set(item, "output_index", st.CurrentRefusalOutputIndex)
			item, _ = sjson.Set(item, "item.id", st.CurrentRefusalMsgID)
			out = append(out, emitResponsesEvent("response.output_item.added", item))

			// response.content_part.added for refusal part
			part := `{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`
			part, _ = sjson.Set(part, "sequence_number", nextSeq())
			part, _ = sjson.Set(part, "item_id", st.CurrentRefusalMsgID)
			part, _ = sjson.Set(part, "output_index", st.CurrentRefusalOutputIndex)
			out = append(out, emitResponsesEvent("response.content_part.added", part))
		}
	}

	// 发送 refusal delta
	st.RefusalBuf.WriteString(refusalText)
	msg := `{"type":"response.refusal.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`
	msg, _ = sjson.Set(msg, "sequence_number", nextSeq())
	msg, _ = sjson.Set(msg, "item_id", st.CurrentRefusalMsgID)
	msg, _ = sjson.Set(msg, "output_index", st.CurrentRefusalOutputIndex)
	msg, _ = sjson.Set(msg, "content_index", st.refusalContentIndex())
	msg, _ = sjson.Set(msg, "delta", refusalText)
	out = append(out, emitResponsesEvent("response.refusal.delta", msg))
	return out
}

// refusalContentIndex 返回 refusal part 的 content_index：追加模式（text→refusal 防御路径）
// 时 refusal 排在消息已有的全部 output_text part 之后（单段 text 即第二个 part），
// 其余场景（独立 refusal item）为 0。
func (st *chatToResponsesState) refusalContentIndex() int {
	if st.RefusalAppendedToMsg {
		return st.TextPartCount
	}
	return 0
}

// closeRefusalBlock 关闭 refusal block：仅发 part 级事件（refusal.done / content_part.done），
// item 级 output_item.done 统一由终态补发点 emitMessageItemDoneEvent 发出
func (st *chatToResponsesState) closeRefusalBlock(nextSeq func() int) []string {
	if !st.InRefusalBlock {
		return nil
	}

	var out []string
	// 复用打开时记录的 index，不按当前 ReasoningPartAdded 重算：
	// refusal→reasoning 乱序场景下拒绝块打开时尚未见 reasoning，重算会让同一 item 的
	// delta/done 事件 output_index 前后矛盾（终态数组按流式已定 index 还原顺序见 generateCompletedEvents）。
	outputIndex := st.CurrentRefusalOutputIndex
	contentIndex := st.refusalContentIndex()
	full := st.RefusalBuf.String()

	// response.refusal.done
	done := `{"type":"response.refusal.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"refusal":"","logprobs":[]}`
	done, _ = sjson.Set(done, "sequence_number", nextSeq())
	done, _ = sjson.Set(done, "item_id", st.CurrentRefusalMsgID)
	done, _ = sjson.Set(done, "output_index", outputIndex)
	done, _ = sjson.Set(done, "content_index", contentIndex)
	done, _ = sjson.Set(done, "refusal", full)
	out = append(out, emitResponsesEvent("response.refusal.done", done))

	// response.content_part.done
	partDone := `{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`
	partDone, _ = sjson.Set(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.Set(partDone, "item_id", st.CurrentRefusalMsgID)
	partDone, _ = sjson.Set(partDone, "output_index", outputIndex)
	partDone, _ = sjson.Set(partDone, "content_index", contentIndex)
	partDone, _ = sjson.Set(partDone, "part.refusal", full)
	out = append(out, emitResponsesEvent("response.content_part.done", partDone))

	// refusal 块关闭不发 item 级 output_item.done（含独立 item 复用续写与追加模式两类）：
	// refusal 可重开续写（RefusalBuf 合并全文持续增长），块关闭 ≠ item 定稿。message item
	// 唯一 done 统一由终态补发点发出（generateCompletedEvents / generateFailedEvents），
	// 与终态 response.output 同源（协议 docs/responses-protocol.md §7：每个 output item
	// 恰好一条 added/done，marked done 为一次性状态转换，done 携带最终完整 content）。

	st.InRefusalBlock = false
	// refusal 刚关闭：开启一次性违规窗口，下一个内容块若直接是新 text 段按违规丢弃
	// （由随后到达的 tool/reasoning/refusal 复位，见各 handle* 与 tool_calls 分支）
	st.RefusalViolationSeen = true
	return out
}

// closeFuncBlocks 关闭所有 function call blocks
func (st *chatToResponsesState) closeFuncBlocks(nextSeq func() int) ([]string, error) {
	if !st.InFuncBlock || len(st.FuncArgsBuf) == 0 {
		return nil, nil
	}

	// 事务性预检（P1-10）：任何有内容但未成功 added（缺 id 或 name）的 call 使整个 close 失败，
	// 不得为其产出 output_item.done；调用方经 failConversion 走唯一 failed 终态。
	if err := st.checkToolCallLifecycle(); err != nil {
		st.InFuncBlock = false
		return nil, err
	}

	var out []string

	// 收集并排序索引
	idxs := make([]int, 0, len(st.FuncArgsBuf))
	for idx := range st.FuncArgsBuf {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)

	for _, idx := range idxs {
		// 预检通过后仍未 added 的只可能是无任何内容的空登记（不发事件，避免凭空造 item）
		if !st.FuncItemAdded[idx] {
			continue
		}
		args := "{}"
		if buf := st.FuncArgsBuf[idx]; buf != nil && buf.Len() > 0 {
			args = buf.String()
		}
		callID := st.FuncCallIDs[idx]
		name := st.FuncNames[idx]

		// 计算 output_index
		outputIndex := st.customToolOutputIndex(idx)

		if st.isCustomProxy(idx) {
			customInput := reconstructCustomToolCallInput(st.CodexCtx, name, args)
			originalName := st.CodexCtx.OriginalCustomToolName(name)
			itemID := fmt.Sprintf("ctc_%s", callID)

			ctcDelta := `{"type":"response.custom_tool_call_input.delta","sequence_number":0,"item_id":"","call_id":"","output_index":0,"delta":""}`
			ctcDelta, _ = sjson.Set(ctcDelta, "sequence_number", nextSeq())
			ctcDelta, _ = sjson.Set(ctcDelta, "item_id", itemID)
			ctcDelta, _ = sjson.Set(ctcDelta, "call_id", callID)
			ctcDelta, _ = sjson.Set(ctcDelta, "output_index", outputIndex)
			ctcDelta, _ = sjson.Set(ctcDelta, "delta", customInput)
			out = append(out, emitResponsesEvent("response.custom_tool_call_input.delta", ctcDelta))

			// P1-8：custom 关闭序列必须含 response.custom_tool_call_input.done（delta/done 成对，
			// responses §7），其 final input 与紧随的 output_item.done.item.input 完全一致，
			// 否则依赖 .done 定稿的客户端永久等待。
			ctcDone := `{"type":"response.custom_tool_call_input.done","sequence_number":0,"item_id":"","call_id":"","output_index":0,"input":""}`
			ctcDone, _ = sjson.Set(ctcDone, "sequence_number", nextSeq())
			ctcDone, _ = sjson.Set(ctcDone, "item_id", itemID)
			ctcDone, _ = sjson.Set(ctcDone, "call_id", callID)
			ctcDone, _ = sjson.Set(ctcDone, "output_index", outputIndex)
			ctcDone, _ = sjson.Set(ctcDone, "input", customInput)
			out = append(out, emitResponsesEvent("response.custom_tool_call_input.done", ctcDone))

			itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"completed","call_id":"","name":"","input":""}}`
			itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
			itemDone, _ = sjson.Set(itemDone, "output_index", outputIndex)
			itemDone, _ = sjson.Set(itemDone, "item.id", itemID)
			itemDone, _ = sjson.Set(itemDone, "item.call_id", callID)
			itemDone, _ = sjson.Set(itemDone, "item.name", originalName)
			itemDone, _ = sjson.Set(itemDone, "item.input", customInput)
			out = append(out, emitResponsesEvent("response.output_item.done", itemDone))
			continue
		} else if st.CodexCtxInitialized && st.CodexCtx.IsBuiltinTool(name, "tool_search") {
			out = append(out, st.emitBuiltinSearchQueryDone(idx, args, nextSeq))
			out = append(out, st.emitBuiltinLifecycleEvent(idx, nextSeq, "completed"))
			itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"tool_search_call","status":"completed","arguments":"","call_id":"","name":"tool_search","execution":"client"}}`
			itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
			itemDone, _ = sjson.Set(itemDone, "output_index", outputIndex)
			itemDone, _ = sjson.Set(itemDone, "item.id", fmt.Sprintf("ts_%s", callID))
			itemDone, _ = sjson.Set(itemDone, "item.name", name)
			itemDone, _ = sjson.Set(itemDone, "item.call_id", callID)
			// tool_search_call 的 arguments 是内嵌 JSON 对象，与 function_call.arguments 字符串口径不同；
			// 取值口径见 toolSearchArgumentsValue（统一 helper，三条输出路径结构一致）。
			itemDone, _ = sjson.Set(itemDone, "item.arguments", toolSearchArgumentsValue(args))
			out = append(out, emitResponsesEvent("response.output_item.done", itemDone))
			continue
		} else if st.CodexCtxInitialized && st.CodexCtx.IsBuiltinTool(name, "web_search") {
			out = append(out, st.emitBuiltinSearchQueryDone(idx, args, nextSeq))
			out = append(out, st.emitBuiltinLifecycleEvent(idx, nextSeq, "completed"))
			itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"web_search_call","status":"completed","arguments":"","call_id":"","name":"web_search"}}`
			itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
			itemDone, _ = sjson.Set(itemDone, "output_index", outputIndex)
			itemDone, _ = sjson.Set(itemDone, "item.id", fmt.Sprintf("ws_%s", callID))
			itemDone, _ = sjson.Set(itemDone, "item.name", name)
			itemDone, _ = sjson.Set(itemDone, "item.call_id", callID)
			itemDone, _ = sjson.Set(itemDone, "item.arguments", args)
			out = append(out, emitResponsesEvent("response.output_item.done", itemDone))
			continue
		}

		// response.function_call_arguments.done
		fcDone := `{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`
		fcDone, _ = sjson.Set(fcDone, "sequence_number", nextSeq())
		fcDone, _ = sjson.Set(fcDone, "item_id", fmt.Sprintf("fc_%s", callID))
		fcDone, _ = sjson.Set(fcDone, "output_index", outputIndex)
		fcDone, _ = sjson.Set(fcDone, "arguments", args)
		out = append(out, emitResponsesEvent("response.function_call_arguments.done", fcDone))

		// response.output_item.done for function_call
		itemDone := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`
		itemDone, _ = sjson.Set(itemDone, "sequence_number", nextSeq())
		itemDone, _ = sjson.Set(itemDone, "output_index", outputIndex)
		itemDone, _ = sjson.Set(itemDone, "item.id", fmt.Sprintf("fc_%s", callID))
		itemDone, _ = sjson.Set(itemDone, "item.arguments", args)
		itemDone, _ = sjson.Set(itemDone, "item.call_id", callID)
		displayName, namespace := st.CodexCtx.OpenAINameForFunctionTool(name)
		itemDone, _ = sjson.Set(itemDone, "item.name", displayName)
		if namespace != "" {
			itemDone, _ = sjson.Set(itemDone, "item.namespace", namespace)
		}
		out = append(out, emitResponsesEvent("response.output_item.done", itemDone))
	}

	st.InFuncBlock = false
	return out, nil
}

// toolSearchArgumentsValue 返回 tool_search_call.arguments 在输出中的取值：
// 整串为合法 JSON 对象时返回其 map 形态（内嵌对象；codex core 侧该字段 serde 目标是结构体
// SearchToolCallParams，传字符串会报 invalid type: string ...）；其余情况（被截断的对象前缀、null、
// 标量、非法 JSON、空串）返回原字符串，交由 core 显式报错。
// 判定必须用严格的 json.Unmarshal，不可用 gjson 前缀解析（截断的 `{"query":"x` 也会被判为 object）；
// 渲染必须走 sjson.Set / json.Marshal 让换行被转义，严禁 SetRaw 原样注入——未闭合前缀或含裸换行的
// 对象都会破坏外层 SSE 帧。
func toolSearchArgumentsValue(args string) interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(args), &m); err != nil || m == nil {
		return args
	}
	return m
}

// generateCompletedEvents 生成完成事件。返回 error 为协议转换显式边界（契约 §1.3）：
// 工具 added/done 生命周期违规（malformed_tool_call）时产出唯一 failed 终态并返回错误，
// 不得合成 completed/incomplete 假成功（P0-1/P1-10）。
func (st *chatToResponsesState) generateCompletedEvents(originalRequestRawJSON []byte) ([]string, error) {
	if st.TerminalEvent != "" {
		return nil, nil
	}
	// 转换错误已记录（畸形 chunk/畸形工具）：只允许 failed 终态，禁止 [DONE] 合成 completed（P0-1）
	if st.ConversionError != nil {
		events, genErr := st.generateFailedEvents(originalRequestRawJSON, conversionModelError(st.ConversionError))
		if genErr != nil {
			// 合并返回（与 failConversion errors.Join 同口径）：主转换错误必须保持可经
			// errors.As 提取稳定机器码，genErr（终态快照诊断）不得丢弃也不得遮蔽主错误
			return events, errors.Join(st.ConversionError, genErr)
		}
		return events, st.ConversionError
	}
	// 未显式 close（无 finish_reason 直接 [DONE]）的畸形工具调用同样在终态前拦截（P1-10）
	if len(st.FuncArgsBuf) > 0 {
		if cerr := st.checkToolCallLifecycle(); cerr != nil {
			return st.failConversion(originalRequestRawJSON, nil, cerr)
		}
	}

	var out []string
	nextSeq := func() int { st.Seq++; return st.Seq }

	// 兜底：刷出 think 状态机的尾部缓冲（如未闭合的 <think> 或 "<thi" 之类边界片段）
	out = append(out, st.flushThinkTagBuf(nextSeq)...)
	if st.PendingTextBuf.Len() > 0 && !st.shouldFallbackReasoning() {
		out = append(out, st.flushPendingWhitespace(nextSeq)...)
	}

	// 先关闭所有打开的 blocks
	if st.ReasoningActive {
		out = append(out, st.closeReasoningBlock(nextSeq)...)
	}
	if st.InTextBlock {
		out = append(out, st.closeTextBlock(nextSeq)...)
	}
	if st.InRefusalBlock {
		out = append(out, st.closeRefusalBlock(nextSeq)...)
	}
	if st.InFuncBlock {
		closed, cerr := st.closeFuncBlocks(nextSeq)
		if cerr != nil {
			return st.failConversion(originalRequestRawJSON, out, cerr)
		}
		out = append(out, closed...)
	}

	// 兜底：整轮流无有效 content，仅 reasoning，则将 reasoning 文本复制为 message 渲染。
	// 流式阶段仍完整保留纯空白 content；仅在 completed output 阶段避免空白 message 污染 fallback 场景。
	hasTextBlock := st.TextBuf.Len() > 0 || st.CurrentMsgID != ""
	shouldFallbackReasoning := st.shouldFallbackReasoning()
	if shouldFallbackReasoning {

		full := st.ReasoningBuf.String()
		outputIndex := 1 // reasoning 占 0，兜底 message 占 1
		msgID := fmt.Sprintf("msg_%s_1", st.ResponseID)

		// 1. response.output_item.added
		item := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`
		item, _ = sjson.Set(item, "sequence_number", nextSeq())
		item, _ = sjson.Set(item, "output_index", outputIndex)
		item, _ = sjson.Set(item, "item.id", msgID)
		out = append(out, emitResponsesEvent("response.output_item.added", item))

		// 2. response.content_part.added
		part := `{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`
		part, _ = sjson.Set(part, "sequence_number", nextSeq())
		part, _ = sjson.Set(part, "item_id", msgID)
		part, _ = sjson.Set(part, "output_index", outputIndex)
		out = append(out, emitResponsesEvent("response.content_part.added", part))

		// 3. response.output_text.delta
		delta := `{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`
		delta, _ = sjson.Set(delta, "sequence_number", nextSeq())
		delta, _ = sjson.Set(delta, "item_id", msgID)
		delta, _ = sjson.Set(delta, "output_index", outputIndex)
		delta, _ = sjson.Set(delta, "delta", full)
		out = append(out, emitResponsesEvent("response.output_text.delta", delta))

		// 4. response.output_text.done
		done := `{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`
		done, _ = sjson.Set(done, "sequence_number", nextSeq())
		done, _ = sjson.Set(done, "item_id", msgID)
		done, _ = sjson.Set(done, "output_index", outputIndex)
		done, _ = sjson.Set(done, "text", full)
		out = append(out, emitResponsesEvent("response.output_text.done", done))

		// 5. response.content_part.done
		partDone := `{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`
		partDone, _ = sjson.Set(partDone, "sequence_number", nextSeq())
		partDone, _ = sjson.Set(partDone, "item_id", msgID)
		partDone, _ = sjson.Set(partDone, "output_index", outputIndex)
		partDone, _ = sjson.Set(partDone, "part.text", full)
		out = append(out, emitResponsesEvent("response.content_part.done", partDone))

		// fallback 合成 item 的 output_item.done 不在此发：统一由下方终态补发点
		// 从终态 outputs 数组同源生成（每 message item 恒且仅一条 done）
	}

	// 构建终态事件：finish_reason=length/content_filter 时为 response.incomplete（chat §6.2 → responses §4 status/incomplete_details）。
	// length → max_output_tokens；content_filter 原样映射（与响应侧 responses_to_chat.go 非流式路径对称，chat §6.2）。
	// responses §5 message item 未定义 truncated 字段，truncated 只标记在 response 主对象上（§4 示例字段）。
	terminal := responsesTerminalCompleted
	statusValue := "completed"
	incompleteReason := ""
	switch st.FinishReason {
	case "length":
		terminal = responsesTerminalIncomplete
		statusValue = "incomplete"
		incompleteReason = "max_output_tokens"
	case "content_filter":
		terminal = responsesTerminalIncomplete
		statusValue = "incomplete"
		incompleteReason = "content_filter"
	}
	// 统一终态快照（P2-3）：身份字段 + 原请求回显（含 text/modalities/store/service_tier/user）
	// + usage，与非流式 T6 同一清单；status/incomplete_details 按终态种类填充。
	snapshot, snapErr := buildTerminalResponseSnapshot(originalRequestRawJSON, st)
	if snapErr != nil {
		return st.failConversion(originalRequestRawJSON, out, snapErr)
	}
	snapshot["status"] = statusValue
	if terminal == responsesTerminalIncomplete {
		snapshot["incomplete_details"] = map[string]interface{}{"reason": incompleteReason}
		snapshot["truncated"] = true
	}
	snapshotBody, marshalErr := marshalNoEscapeJSON(snapshot)
	if marshalErr != nil {
		return st.failConversion(originalRequestRawJSON, out, errResponse(model.CodeInvalidSourceShape, "response", marshalErr))
	}
	completed := `{"type":"","sequence_number":0,"response":` + string(snapshotBody) + `}`
	completed, _ = sjson.Set(completed, "type", string(terminal))

	// 构建 output 数组
	var outputs []interface{}

	hasReasoning := st.ReasoningBuf.Len() > 0 || st.ReasoningPartAdded
	// 追加模式（text→refusal 防御路径）下 refusal part 已并入 text message item，不产出独立 item
	hasStandaloneRefusal := st.RefusalBuf.Len() > 0 && !st.RefusalAppendedToMsg

	// reasoning item（如果有）
	var reasoningItem map[string]interface{}
	if hasReasoning {
		reasoningItem = map[string]interface{}{
			"id":     st.ReasoningItemID,
			"type":   "reasoning",
			"status": "completed",
			"summary": []interface{}{map[string]interface{}{
				"type": "summary_text",
				"text": st.ReasoningBuf.String(),
			}},
		}
	}

	// message item（如果有文本块）。触发 reasoning fallback 时不额外输出纯空白 message。
	var messageItem map[string]interface{}
	if hasTextBlock && !shouldFallbackReasoning {
		// content 按流式已定的 content_index 逐段还原（多段 text 各占一个 output_text part）；
		// 防御路径的 refusal part 合并（responses §5 允许 message content 含 refusal part，
		// 与流式事件序列及非流式 convertChatMessageToOutput 合并行为对齐）已由
		// messageTextContentParts 统一并入，与流式唯一 output_item.done 同源现取、逐项一致
		messageItem = map[string]interface{}{
			"id":      st.CurrentMsgID,
			"type":    "message",
			"status":  "completed",
			"content": st.messageTextContentParts(),
			"role":    "assistant",
		}
	}

	// 兜底 message item（无 content 仅 reasoning 时，把 reasoning 文本复制为 message 渲染）
	if shouldFallbackReasoning {
		messageItem = map[string]interface{}{
			"id":     fmt.Sprintf("msg_%s_1", st.ResponseID),
			"type":   "message",
			"status": "completed",
			"content": []interface{}{map[string]interface{}{
				"type":        "output_text",
				"annotations": []interface{}{},
				"logprobs":    []interface{}{},
				"text":        st.ReasoningBuf.String(),
			}},
			"role": "assistant",
		}
	}

	// refusal message item（chat §6.1 message.refusal → responses §5 refusal content part）。
	// closeRefusalBlock 已在此前关闭块阶段把完整文本写入 RefusalBuf；流式期间已有独立 refusal 事件序列。
	var refusalItem map[string]interface{}
	if hasStandaloneRefusal {
		refusalItem = map[string]interface{}{
			"id":     st.CurrentRefusalMsgID,
			"type":   "message",
			"status": "completed",
			"content": []interface{}{map[string]interface{}{
				"type":    "refusal",
				"refusal": st.RefusalBuf.String(),
			}},
			"role": "assistant",
		}
	}

	// 按流式已定的 output_index 还原终态数组顺序：refusal/reasoning/text 存在乱序到达
	// （refusal→reasoning→text）时各自占据的 index 已在流式阶段顺延，此处按 index 升序
	// 排列三项，保证终态数组与各事件的 output_index 一致（item 不重复占用同一 index）。
	type orderedOutputItem struct {
		index int
		item  interface{}
	}
	orderedOutputs := make([]orderedOutputItem, 0, 3)
	if reasoningItem != nil {
		orderedOutputs = append(orderedOutputs, orderedOutputItem{st.ReasoningIndex, reasoningItem})
	}
	if messageItem != nil {
		messageIndex := st.CurrentTextOutputIndex // 流式快照 index 还原，与 id 后缀/流式事件一致
		if shouldFallbackReasoning {
			// 兜底 message 固定占 index 1（id 为 msg_<respID>_1，text 块从未打开，无快照可言）
			messageIndex = 1
		}
		orderedOutputs = append(orderedOutputs, orderedOutputItem{messageIndex, messageItem})
	}
	if refusalItem != nil {
		orderedOutputs = append(orderedOutputs, orderedOutputItem{st.CurrentRefusalOutputIndex, refusalItem})
	}
	sort.Slice(orderedOutputs, func(i, j int) bool { return orderedOutputs[i].index < orderedOutputs[j].index })
	for _, o := range orderedOutputs {
		outputs = append(outputs, o.item)
	}

	// function_call items（仅输出已成功 added 的 call；生命周期违规在函数头部已拦截走 failed 终态，
	// 无任何内容的空登记不得凭空造 terminal item，P1-10）
	if len(st.FuncArgsBuf) > 0 {
		idxs := make([]int, 0, len(st.FuncArgsBuf))
		for idx := range st.FuncArgsBuf {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			if !st.FuncItemAdded[idx] {
				continue
			}
			args := ""
			if b := st.FuncArgsBuf[idx]; b != nil {
				args = b.String()
			}
			if args == "" {
				args = "{}"
			}
			callID := st.FuncCallIDs[idx]
			name := st.FuncNames[idx]
			if st.isCustomProxy(idx) {
				customInput := reconstructCustomToolCallInput(st.CodexCtx, name, args)
				originalName := st.CodexCtx.OriginalCustomToolName(name)
				item := map[string]interface{}{
					"id":      fmt.Sprintf("ctc_%s", callID),
					"type":    "custom_tool_call",
					"status":  "completed",
					"call_id": callID,
					"name":    originalName,
					"input":   customInput,
				}
				outputs = append(outputs, item)
				continue
			}
			if st.CodexCtxInitialized && st.CodexCtx.IsBuiltinTool(name, "tool_search") {
				// tool_search_call 的 arguments 是内嵌 JSON 对象，与 function_call.arguments 字符串口径不同；
				// 取值口径见 toolSearchArgumentsValue（统一 helper，三条输出路径结构一致）。
				item := map[string]interface{}{
					"id":        fmt.Sprintf("ts_%s", callID),
					"type":      "tool_search_call",
					"status":    "completed",
					"arguments": toolSearchArgumentsValue(args),
					"call_id":   callID,
					"name":      name,
					"execution": "client",
				}
				outputs = append(outputs, item)
				continue
			}
			if st.CodexCtxInitialized && st.CodexCtx.IsBuiltinTool(name, "web_search") {
				// web_search_call 无 execution 字段（docs §3.6 与上游格式一致）
				item := map[string]interface{}{
					"id":        fmt.Sprintf("ws_%s", callID),
					"type":      "web_search_call",
					"status":    "completed",
					"arguments": args,
					"call_id":   callID,
					"name":      name,
				}
				outputs = append(outputs, item)
				continue
			}
			displayName, namespace := st.CodexCtx.OpenAINameForFunctionTool(name)
			item := map[string]interface{}{
				"id":        fmt.Sprintf("fc_%s", callID),
				"type":      "function_call",
				"status":    "completed",
				"arguments": args,
				"call_id":   callID,
				"name":      displayName,
			}
			if namespace != "" {
				item["namespace"] = namespace
			}
			outputs = append(outputs, item)
		}
	}

	if len(outputs) > 0 {
		completed, _ = sjson.Set(completed, "response.output", outputs)
	}

	// usage 已随统一终态快照注入（buildTerminalResponseSnapshot → buildStreamUsage，responses §6：
	// 五个顶层字段 + 两个 details 对象恒存在）。completion details 与非流式 normalizeOutputDetails
	// 同源映射（P1-9）；reasoning_tokens 仅来自上游 usage，缺失为 0——旧实现按 reasoning 文本字节长度
	// 除以常数估算 token 伪造计费数据的通道已彻底移除（P0-4，报告一 P0-4）。

	// message item done 终态统一补发点（reviewer v2 裁决：放弃「定稿点」概念）：
	// 所有 message item（text item 与独立 refusal item 一体适用）的唯一 output_item.done
	// 只在终态事件前发出，item 快照直接复用上方 outputs 数组构建产物——done 与终态
	// response.output 天然同源一致，每 item 恒且仅一条，refusal 续写/复用、多段 text
	// 追加等一切增长路径都不再产生过期快照或重复 done（协议 docs/responses-protocol.md
	// §7：added/done 成对标记 item 生命周期，marked done 为一次性状态转换，done 携带
	// 最终完整 content）。发射顺序沿用 outputs 数组（流式 output_index 升序）。
	// 防御性兜底分支（当前状态机下不可达）：前导空白被 Think 状态机吞掉或走延迟缓冲，
	// 无法同时满足「已 added（CurrentMsgID != ""）」与「TrimSpace 为空」，仅守
	// added/done 成对不变量（快照 messageTextContentParts 现取）
	if shouldFallbackReasoning && st.CurrentMsgID != "" {
		blankItem := map[string]interface{}{
			"id":      st.CurrentMsgID,
			"type":    "message",
			"status":  "completed",
			"content": st.messageTextContentParts(),
			"role":    "assistant",
		}
		out = append(out, emitMessageItemDoneEvent(blankItem, st.CurrentTextOutputIndex, nextSeq))
	}
	for _, o := range outputs {
		item, ok := o.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := item["type"].(string); t != "message" {
			continue
		}
		id, _ := item["id"].(string)
		doneIndex := st.CurrentTextOutputIndex
		switch {
		case st.RefusalItemInOutput && id == st.CurrentRefusalMsgID:
			// 独立 refusal item：打开时快照 index
			doneIndex = st.CurrentRefusalOutputIndex
		case shouldFallbackReasoning && id == fmt.Sprintf("msg_%s_1", st.ResponseID):
			// reasoning fallback 合成 item：固定占 1（reasoning 占 0）
			doneIndex = 1
		}
		out = append(out, emitMessageItemDoneEvent(item, doneIndex, nextSeq))
	}
	// completed/incomplete 自身 sequence_number 在所有补发事件之后取号，保证全流严格递增
	completed, _ = sjson.Set(completed, "sequence_number", nextSeq())

	st.CompletedBodyJSON = []byte(completed)
	st.TerminalEvent = terminal
	out = append(out, emitResponsesEvent(string(terminal), completed))
	return out, nil
}

// generateFailedEvents 生成唯一失败终态事件（response.failed），并阻断后续成功终态发射。
// 终态 response 对象使用与 completed 同一 buildTerminalResponseSnapshot（P2-3 统一快照：
// 回显 text/modalities/store/service_tier/user 等原请求字段 + usage details）。
// 返回的 error 仅在统一快照构建失败时非 nil（events 已用最小合法快照兜底产出，仍为唯一 failed 终态）。
func (st *chatToResponsesState) generateFailedEvents(originalRequestRawJSON []byte, errResp *model.Error) ([]string, error) {
	if st.TerminalEvent != "" || errResp == nil {
		return nil, nil
	}

	var out []string
	nextSeq := func() int { st.Seq++; return st.Seq }

	// 关闭所有打开的 blocks（与 generateCompletedEvents 一致）
	out = append(out, st.flushThinkTagBuf(nextSeq)...)
	if st.PendingTextBuf.Len() > 0 && !st.shouldFallbackReasoning() {
		out = append(out, st.flushPendingWhitespace(nextSeq)...)
	}
	if st.ReasoningActive {
		out = append(out, st.closeReasoningBlock(nextSeq)...)
	}
	if st.InTextBlock {
		out = append(out, st.closeTextBlock(nextSeq)...)
	}
	if st.InRefusalBlock {
		out = append(out, st.closeRefusalBlock(nextSeq)...)
	}
	// 工具 blocks：仅当 added/done 生命周期合法时才 close 补发 done；畸形工具直接放弃 done
	// （不得为未 added 的 call 发 output_item.done，P1-10），failed 终态本身即错误边界。
	if len(st.FuncArgsBuf) > 0 {
		closed, cerr := st.closeFuncBlocks(nextSeq)
		if cerr == nil {
			out = append(out, closed...)
		} else if st.ConversionError == nil {
			st.ConversionError = cerr
		}
	}

	// failed 终态统一补发点：为流式已 added 的全部 message item（独立 refusal item +
	// text item）按 output_index 升序、在 response.failed 前补发唯一 done（added/done
	// 成对不变量，同 generateCompletedEvents 补发点）。failed 终态无 output 数组，快照
	// 与 completed 终态构建同源：refusal item = [RefusalBuf 合并全文]，
	// text item = messageTextContentParts（TextParts+追加 refusal 合并）
	type failedMsgDone struct {
		index int
		item  map[string]interface{}
	}
	var failedPendings []failedMsgDone
	if st.RefusalItemInOutput && st.RefusalBuf.Len() > 0 {
		failedPendings = append(failedPendings, failedMsgDone{st.CurrentRefusalOutputIndex, map[string]interface{}{
			"id":      st.CurrentRefusalMsgID,
			"type":    "message",
			"status":  "completed",
			"content": []interface{}{map[string]interface{}{"type": "refusal", "refusal": st.RefusalBuf.String()}},
			"role":    "assistant",
		}})
	}
	if st.CurrentMsgID != "" {
		failedPendings = append(failedPendings, failedMsgDone{st.CurrentTextOutputIndex, map[string]interface{}{
			"id":      st.CurrentMsgID,
			"type":    "message",
			"status":  "completed",
			"content": st.messageTextContentParts(),
			"role":    "assistant",
		}})
	}
	sort.Slice(failedPendings, func(i, j int) bool { return failedPendings[i].index < failedPendings[j].index })
	for _, p := range failedPendings {
		out = append(out, emitMessageItemDoneEvent(p.item, p.index, nextSeq))
	}

	if st.ResponseID == "" {
		st.ResponseID = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	if st.CreatedAt == 0 {
		st.CreatedAt = time.Now().Unix()
	}

	// 统一终态快照（与 completed 同一构建，P2-3）；原请求非法导致快照失败时退化为无回显的
	// 最小合法 failed 快照——终态事件必须产出（SSE 已提交），快照错误仍随 events 向上暴露。
	snapshot, snapErr := buildTerminalResponseSnapshot(originalRequestRawJSON, st)
	if snapErr != nil {
		snapshot, _ = buildTerminalResponseSnapshot(nil, st)
	}
	snapshot["status"] = "failed"
	snapshot["error"] = errResp
	snapshot["output"] = []interface{}{}
	snapshotBody, marshalErr := marshalNoEscapeJSON(snapshot)
	if marshalErr != nil {
		return out, errors.Join(snapErr, marshalErr)
	}
	failed := `{"type":"response.failed","sequence_number":0,"response":` + string(snapshotBody) + `}`
	failed, _ = sjson.Set(failed, "sequence_number", nextSeq())

	st.TerminalEvent = responsesTerminalFailed
	out = append(out, emitResponsesEvent(string(st.TerminalEvent), failed))
	return out, snapErr
}
