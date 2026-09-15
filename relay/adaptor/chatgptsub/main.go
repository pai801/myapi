package chatgptsub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/render"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

var (
	_             adaptor.Adaptor = (*Adaptor)(nil)
	stickyManager                 = DefaultStickyManager
	statsManager                  = NewStatsManager()
	channelProber                 = NewChannelProber()
)

type Adaptor struct {
	OpenAiImpl adaptor.Adaptor
	meta       *meta.Meta
}

// extractSessionHash 从请求头中提取会话标识，用于粘性绑定。
// 优先使用 conversation_id，其次 session_id。
func extractSessionHash(c *gin.Context) string {
	if v := c.GetHeader("conversation_id"); v != "" {
		return v
	}
	if v := c.GetHeader("session_id"); v != "" {
		return v
	}
	return ""
}

func (a *Adaptor) Init(meta *meta.Meta) {
	a.meta = meta
}

func (a *Adaptor) GetRequestURL(meta *meta.Meta) (string, error) {
	baseURL := strings.TrimSuffix(meta.BaseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	switch meta.Mode {
	case relaymode.ChatCompletions, relaymode.Responses:
		return baseURL + "/backend-api/codex/responses", nil
	default:
		return baseURL + meta.RequestURLPath, nil
	}
}

// allowedHeaders 是 ChatGPT Web 请求中允许透传到上游的头白名单
var allowedHeaders = map[string]bool{
	"accept-language":       true,
	"content-type":          true,
	"conversation_id":       true,
	"user-agent":            true,
	"originator":            true,
	"session_id":            true,
	"x-codex-turn-state":    true,
	"x-codex-turn-metadata": true,
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) error {
	// 粘性会话检查：如果这个会话有绑定的 channel，且绑定的不是当前 channel，尝试切换
	sessionHash := extractSessionHash(c)
	if sessionHash != "" {
		if boundID, ok := stickyManager.Get(meta.Group, sessionHash); ok && boundID != meta.ChannelId {
			if boundChannel, err := dbmodel.GetChannelById(boundID, true); err == nil && boundChannel != nil && boundChannel.Status == dbmodel.ChannelStatusEnabled {
				meta.ChannelId = boundChannel.Id
				meta.APIKey = boundChannel.Key
				meta.BaseURL = boundChannel.GetBaseURL()
				// GetRequestURL 先于本函数执行且拿不到 gin.Context，粘性解析只能在此进行；
				// 切换渠道后必须基于新 BaseURL 重建请求 URL，否则会以"旧 URL+新 key"请求导致鉴权失败。
				// URL 重建失败时请求已无法正确到达绑定渠道，显式报错优于发出注定 401 的请求
				newURL, uErr := a.GetRequestURL(meta)
				if uErr != nil {
					return fmt.Errorf("rebuild sticky url for channel %d failed: %w", meta.ChannelId, uErr)
				}
				if newURL == "" {
					return fmt.Errorf("rebuild sticky url for channel %d failed: empty url", meta.ChannelId)
				}
				parsed, pErr := url.Parse(newURL)
				if pErr != nil {
					return fmt.Errorf("parse sticky url for channel %d failed: %w", meta.ChannelId, pErr)
				}
				req.URL = parsed
				// 同步更新请求头
				req.Header.Set("Authorization", "Bearer "+meta.APIKey)
			}
		}
	}

	adaptor.SetupCommonRequestHeader(c, req, meta)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if meta.IsStream {
		req.Header.Set("Accept", "text/event-stream")
	}
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)

	// 头白名单过滤：只保留白名单内的头 + 固定头
	for key := range req.Header {
		lowerKey := strings.ToLower(key)
		// 永远保留这些头
		if lowerKey == "authorization" || lowerKey == "content-type" || lowerKey == "accept" {
			continue
		}
		if !allowedHeaders[lowerKey] {
			req.Header.Del(key)
		}
	}
	return nil
}

func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	switch relayMode {
	case relaymode.ChatCompletions:
		converted, err := convertChatToResponsesRequest(request)
		if err != nil {
			// *model.ProtocolConversionError 原样上抛，由 relay controller 转 HTTP 400
			return nil, err
		}
		// 记录原 Chat 客户端的 usage 策略：响应阶段（T5）只能读此值，
		// 禁止因上游 Responses usage 总是存在而擅自向 Chat 客户端发送 usage chunk
		c.Set(ctxkey.ChatStreamIncludeUsage, request.StreamOptions != nil && request.StreamOptions.IncludeUsage)
		return converted, nil
	case relaymode.Responses:
		// 透传原始 Responses 请求体
		rawBody, err := common.GetRequestBody(c)
		if err != nil {
			return nil, err
		}
		var bodyMap map[string]interface{}
		if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
			return nil, err
		}
		return bodyMap, nil
	default:
		return request, nil
	}
}

func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	return request, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error) {
	return adaptor.DoRequestHelper(a, c, meta, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (usage *model.Usage, respErr *model.ErrorWithStatusCode) {
	// 使用 defer 在返回前统一上报健康统计 + 粘性绑定
	defer func() {
		// 客户端请求形态类失败（如 invalid_request_error）不惩罚渠道健康度：
		// 完全跳过上报，与框架冷却「权重 0 不计数不冷却」口径对齐；禁止伪报 success=true。
		// 注意口径：谓词守卫 Type=upstream_error 且 Code 命中网关机器码全集的失败一律不豁免，
		// 覆盖两类变体——① upstreamConversionError 统一兜底包装为 invalid_upstream_response
		// （message 可能内嵌 invalid_request_error 等 marker）；② errors.As 保留转换机器码的
		// 包装（外层 Code 直接是 malformed_tool_call 等六个协议机器码）。真正保留上游原码的
		// 客户端错误（如 Code=invalid_request_error，不在机器码集）仍豁免。
		// respErr==nil 但 StatusCode>=400 的无法分类失败不在此豁免范围，仍照常计入。
		if respErr != nil && respErr.LooksLikeRequestShapeFailure() {
			return
		}
		success := respErr == nil && resp != nil && resp.StatusCode < 400
		statsManager.ReportResult(meta.ChannelId, success, nil)

		if success {
			sessionHash := extractSessionHash(c)
			if sessionHash != "" {
				stickyManager.Set(meta.Group, sessionHash, meta.ChannelId)
			}
		}
	}()

	if meta.IsStream {
		switch meta.Mode {
		case relaymode.ChatCompletions:
			// Responses SSE 流 → Chat Completions SSE 流
			return streamChatFromResponses(c, resp, meta)
		case relaymode.Responses:
			// Responses SSE 流直接透传
			err, _, usage := codex.StreamResponsesHandler(c, resp)
			return usage, err
		default:
			return a.OpenAiImpl.DoResponse(c, resp, meta)
		}
	}

	switch meta.Mode {
	case relaymode.ChatCompletions:
		return handleChatCompletionsResponse(c, resp, meta)
	case relaymode.Responses:
		return codex.DoResponsesResponse(c, resp, meta)
	default:
		return a.OpenAiImpl.DoResponse(c, resp, meta)
	}
}

func (a *Adaptor) GetModelList() []string {
	return []string{
		"gpt-5.5",
		"gpt-5.4-mini",
		"gpt-5.4",
	}
}

func (a *Adaptor) GetChannelName() string {
	return "chatgpt-sub"
}

// ==================== Chat Completions Streaming（Responses §7 → Chat §7） ====================

// toolCallState 记录流式 tool call（function_call / custom_tool_call）的累积状态。
// Arguments 是累积的完整参数串，EmittedArgs 记录已发送字节数：added 初值、delta 增量、
// done 快照三路合并后只发送未发后缀，保证每个字节恰好发送一次（报告二 4/23）。
// Added 标记 header 分片（id+name）是否已发出；Done 标记 output_item.done 生命周期是否闭合，
// 终态前必须闭合，否则按不可恢复工具状态显式失败（Advisory-28：字段全部参与决策）。
type toolCallState struct {
	Index       int
	ItemID      string
	CallID      string
	Name        string
	Custom      bool // DN-3 流式：custom_tool_call 输出 Chat §6.1.1 custom 变体，禁止伪装 function
	CustomSet   bool // P1-1：Custom 的"已确定来源"标记；后续任何事件断言与已确定类型不一致即显式失败
	Arguments   strings.Builder
	EmittedArgs int
	Added       bool
	Done        bool
}

// chatStreamMetadata 是全流共享的 Chat §7.1 chunk 公共元数据（id/model/created 必填且一致，
// 报告二 6）。唯一合法来源是 response.created 携带的完整 response 对象（Responses §7）；
// Model/CreatedAt 以 meta 与网关时间初始化，仅作 response.created 缺失前错误 chunk 的兜底，
// RoleSent 保证首个 choice chunk 携带 delta.role:"assistant" 且全流仅一次。
type chatStreamMetadata struct {
	ID        string
	Model     string
	CreatedAt int64
	RoleSent  bool
}

// streamChatFromResponses 读取上游 Responses SSE 流并转换为 Chat Completions SSE 写回。
// 事件名严格按 Responses §7 全集处理：虚构名（如 response.reasoning.delta）与未知事件、
// 不可映射 output item（DN-4 流式）一律显式终止，不得静默忽略；畸形 JSON payload 视为
// invalid_stream_event（报告二 24）。错误终止只按 Chat 合法方式：错误 chunk + 单一 [DONE]，
// 同时返回 ErrorWithStatusCode 供渠道统计记失败与预扣费回滚（§1.3，SSE header 已提交后
// 不再写任何 JSON HTTP 错误体）。
func streamChatFromResponses(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	reader := bufio.NewReaderSize(resp.Body, constant.ScannerBufferInitial)

	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()
	defer resp.Body.Close()

	metadata := chatStreamMetadata{
		Model:     meta.ActualModelName,
		CreatedAt: time.Now().Unix(),
	}

	// 报告二 21/T2 契约：usage chunk 只按原始 Chat 客户端的 stream_options.include_usage 策略发送，
	// 禁止从已转换的 Responses 请求或上游事件反推（上游 usage 恒存在不代表客户端要）。
	includeUsage, _ := c.Get(ctxkey.ChatStreamIncludeUsage)
	wantUsage, _ := includeUsage.(bool)

	toolCallStates := make(map[string]*toolCallState)
	toolCallSeq := 0

	// 终态（completed/incomplete）的客户端输出在 finalize 中已写完整（finish/usage/[DONE]）；
	// 此后再发生上游断流错误只回传统计侧（渠道失败+预扣费处理），不再向已终结的客户端流写任何内容。
	var outUsage *model.Usage
	var outErrResp *model.ErrorWithStatusCode
	var terminalDone bool

	// failStream 是 SSE 已提交后的唯一错误出口：写合法错误 chunk、按 Chat 方式补 [DONE]，
	// 并回传 502 ErrorWithStatusCode（复用 T4 映射，不生成任何成功 finish/terminal）。
	// 置位 terminalDone 保证任何后续路径不会向同一客户端流二次写入或二次 [DONE]。
	failStream := func(convErr error) (*model.Usage, *model.ErrorWithStatusCode) {
		_ = writeChatStreamProtocolError(c, metadata, convErr)
		render.Done(c)
		outUsage = nil
		outErrResp = upstreamConversionError(convErr)
		terminalDone = true
		return nil, outErrResp
	}

	// ensureRole 保证任何 choice chunk（含 delta 与 finish）之前 role 分片已发出，
	// 且公共 metadata 已从 response.created 就绪（id 缺失时禁止伪造，显式失败）。
	ensureRole := func() error {
		if metadata.RoleSent {
			return nil
		}
		if metadata.ID == "" {
			return respConvErr(model.CodeInvalidStreamEvent, "response.created",
				errors.New("stream content produced before response.created; chat chunk metadata (id) is unavailable (chat §7.1)"))
		}
		if err := writeChatRoleDelta(c, metadata); err != nil {
			return err
		}
		metadata.RoleSent = true
		return nil
	}

	// getOrCreateToolState 以 item_id 归属工具状态，并记录类型断言的"已确定来源"（P1-1）：
	// 首次断言即定值（CustomSet 置位）；任何后续事件（含 delta 族 function/custom 混用）
	// 断言与已确定类型不一致，即上游自相矛盾且不可恢复，返回 malformed_tool_call 显式失败。
	// 旧"只升不降"（if custom { st.Custom = true }）会掩盖翻转、伪装 function（违反 DN-3），已删除。
	getOrCreateToolState := func(itemID string, custom bool, srcPath string) (*toolCallState, error) {
		if st, ok := toolCallStates[itemID]; ok {
			if st.CustomSet && st.Custom != custom {
				return nil, respConvErr(model.CodeMalformedToolCall, srcPath,
					fmt.Errorf("tool call %q type conflict: item already determined as %s, event asserts %s",
						itemID, toolCallKindName(st.Custom), toolCallKindName(custom)))
			}
			st.Custom = custom
			st.CustomSet = true
			return st, nil
		}
		st := &toolCallState{Index: toolCallSeq, ItemID: itemID, Custom: custom, CustomSet: true}
		toolCallSeq++
		toolCallStates[itemID] = st
		return st, nil
	}

	// flushToolState 发送 Arguments 中未发送后缀并前移 EmittedArgs；withHeader 时同一分片
	// 携带 id+name（每个 tool call 的 header 只发一次，name 不重复下发）。
	flushToolState := func(state *toolCallState, withHeader bool) error {
		if withHeader && (state.CallID == "" || state.Name == "") {
			return respConvErr(model.CodeMalformedToolCall, "output_item."+state.ItemID,
				fmt.Errorf("tool call %q cannot be emitted: call_id or name is missing", state.ItemID))
		}
		if err := ensureRole(); err != nil {
			return err
		}
		full := state.Arguments.String()
		if state.EmittedArgs > len(full) {
			return respConvErr(model.CodeMalformedToolCall, "output_item."+state.ItemID,
				errors.New("internal state inconsistent: emitted bytes exceed buffered arguments"))
		}
		suffix := full[state.EmittedArgs:]
		if suffix == "" && !withHeader {
			return nil
		}
		if err := writeChatToolCallFragment(c, metadata, state, suffix, withHeader); err != nil {
			return err
		}
		state.EmittedArgs = len(full)
		return nil
	}

	emitDelta := func(field, value string) error {
		if err := ensureRole(); err != nil {
			return err
		}
		return writeChatContentDelta(c, metadata, field, value)
	}

	// validateToolCallStates 在终态前闭合工具生命周期：每个 state 必须 Done、id/name 完整、
	// 参数字节全部发出，否则是假成功的 tool_calls/stop（报告二 5/23，Advisory-28）。
	validateToolCallStates := func() (bool, error) {
		if len(toolCallStates) == 0 {
			return false, nil
		}
		byIndex := make([]*toolCallState, len(toolCallStates))
		for _, st := range toolCallStates {
			if st.Index < 0 || st.Index >= len(byIndex) || byIndex[st.Index] != nil {
				return false, respConvErr(model.CodeMalformedToolCall, "output_item."+st.ItemID,
					errors.New("tool call index sequence is inconsistent"))
			}
			byIndex[st.Index] = st
		}
		for _, st := range byIndex {
			if st == nil {
				return false, respConvErr(model.CodeMalformedToolCall, "toolCallStates",
					errors.New("tool call index sequence has a gap"))
			}
			if !st.Done {
				return false, respConvErr(model.CodeMalformedToolCall, "output_item."+st.ItemID,
					fmt.Errorf("tool call %q has no output_item.done before terminal event (responses §7 lifecycle)", st.ItemID))
			}
			if st.CallID == "" || st.Name == "" {
				return false, respConvErr(model.CodeMalformedToolCall, "output_item."+st.ItemID,
					fmt.Errorf("tool call %q is missing call_id or name", st.ItemID))
			}
			if st.EmittedArgs != st.Arguments.Len() {
				return false, respConvErr(model.CodeMalformedToolCall, "output_item."+st.ItemID,
					fmt.Errorf("tool call %q arguments were not fully emitted", st.ItemID))
			}
		}
		return true, nil
	}

	// finalizeTerminalResponse 以 T4 同一映射契约推导 finish_reason，写 finish（usage:null）、
	// 按策略写独立 usage chunk，最后 [DONE]。incomplete 依据原因映射 length/content_filter；
	// failed/cancelled/未知状态经 mapFinishReason 返回错误，绝不落入 stop。
	finalizeTerminalResponse := func(r *model.ResponsesResponse) {
		hasToolCalls, toolErr := validateToolCallStates()
		if toolErr != nil {
			failStream(toolErr)
			return
		}
		incompleteReason := ""
		if r.IncompleteDetails != nil {
			incompleteReason = r.IncompleteDetails.Reason
		}
		finishReason, mapErr := mapFinishReason(r.Status, incompleteReason, hasToolCalls)
		if mapErr != nil {
			failStream(mapErr)
			return
		}
		if err := ensureRole(); err != nil {
			failStream(err)
			return
		}
		if err := writeChatStreamFinish(c, metadata, finishReason); err != nil {
			failStream(err)
			return
		}
		usage := convertResponsesUsage(r.Usage)
		if wantUsage {
			if err := writeChatUsageChunk(c, metadata, usage); err != nil {
				failStream(err)
				return
			}
		}
		render.Done(c)
		outUsage = &usage
		terminalDone = true
	}

loop:
	for {
		event, err := codex.ReadSSEEvent(reader, constant.ScannerBufferMax)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				// 客户端断开：不再计渠道失败，也不追加输出
				return nil, nil
			}
			if errors.Is(err, io.EOF) {
				// EOF 但没有任何终态事件：Responses 流必须以 completed/incomplete/failed/error
				// 收尾（§7），提前截断不得合成 stop 伪装正常完成（报告二 22/24）
				break
			}
			// 非 EOF 读异常：SSE 已提交，写错误 chunk + [DONE]，回传统计侧错误
			if terminalDone {
				// 客户端流已按终态合法收尾，断流错误仅上报调用方（渠道统计/重试判断）
				return outUsage, codex.ErrorWrapper(err, "stream_read_error", http.StatusInternalServerError)
			}
			_ = writeChatStreamProtocolError(c, metadata, err)
			render.Done(c)
			return nil, codex.ErrorWrapper(err, "stream_read_error", http.StatusInternalServerError)
		}

		if event.Done {
			// Responses 流本身没有 [DONE] 哨兵；上游自发送 [DONE] 且未给出终态事件属协议违例
			return failStream(respConvErr(model.CodeInvalidStreamEvent, "data:[DONE]",
				errors.New("Responses stream terminated by [DONE] sentinel without a terminal event (responses §7)")))
		}

		payload := event.Data
		if payload == "" {
			// keepalive 等空 data 事件，无载荷可映射
			continue
		}

		var streamResp model.ResponsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &streamResp); err != nil {
			// 报告二 24：非空 payload 解析失败即 invalid_stream_event，禁止吞掉继续
			return failStream(respConvErr(model.CodeInvalidStreamEvent, "stream.event.payload",
				fmt.Errorf("malformed stream event payload: %w", err)))
		}
		eventType := event.Event
		if streamResp.Type != "" {
			eventType = streamResp.Type
		}
		if eventType == "" {
			return failStream(respConvErr(model.CodeInvalidStreamEvent, "type",
				errors.New("stream event is missing the type field (responses §7)")))
		}

		switch eventType {
		case "response.created", "response.in_progress", "response.queued":
			// 报告二 6：公共元数据的唯一来源是事件内完整 response 对象（此前代码误读顶层）
			r := streamResp.Response
			if r == nil || r.ID == "" {
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType,
					errors.New("lifecycle event must carry the full response object with id (responses §7)")))
			}
			metadata.ID = r.ID
			if r.Model != "" {
				metadata.Model = r.Model
			}
			// T1 决议：Go 字段名保持 Created（wire tag 为 created_at）
			if r.Created > 0 {
				metadata.CreatedAt = r.Created
			}

		case "response.output_text.delta", "response.reasoning_text.delta",
			"response.reasoning_summary_text.delta", "response.refusal.delta":
			// 报告二 22：真实事件名（删除虚构 response.reasoning.delta 基准）；
			// 推理摘要与推理文本都进 reasoning_content 扩展，refusal 进 delta.refusal，不得丢失
			deltaStr, deltaErr := streamEventDeltaString(&streamResp)
			if deltaErr != nil {
				return failStream(deltaErr)
			}
			if deltaStr == "" {
				continue
			}
			field := "content"
			switch eventType {
			case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
				field = "reasoning_content"
			case "response.refusal.delta":
				field = "refusal"
			}
			if err := emitDelta(field, deltaStr); err != nil {
				return failStream(err)
			}

		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			isCustom := eventType == "response.custom_tool_call_input.delta"
			itemID := streamItemID(&streamResp)
			if itemID == "" {
				// 报告二 23：参数增量无法归属即不可恢复，禁止静默丢弃
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType+".item_id",
					errors.New("arguments delta is missing item_id and cannot be attributed")))
			}
			deltaStr, deltaErr := streamEventDeltaString(&streamResp)
			if deltaErr != nil {
				return failStream(deltaErr)
			}
			if deltaStr == "" {
				continue
			}
			// P1-1：同一 item 的 function/custom delta 混用 → 显式失败，禁止静默翻转类型
			state, stateErr := getOrCreateToolState(itemID, isCustom, eventType+".type")
			if stateErr != nil {
				return failStream(stateErr)
			}
			state.Arguments.WriteString(deltaStr)
			if state.Added {
				// header 已发出：仅发送本增量；未发出时先缓存，等 added/done 补全后统一发送
				if err := flushToolState(state, false); err != nil {
					return failStream(err)
				}
			}

		case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
			// 定稿快照携带完整 arguments/input；用本地严格结构读取（ResponsesStreamEvent 无该字段）
			var snapEvt struct {
				Type      string          `json:"type"`
				ItemID    string          `json:"item_id"`
				Arguments json.RawMessage `json:"arguments"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal([]byte(payload), &snapEvt); err != nil {
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType, err))
			}
			isCustom := eventType == "response.custom_tool_call_input.done"
			itemID := snapEvt.ItemID
			if itemID == "" {
				itemID = streamItemID(&streamResp)
			}
			if itemID == "" {
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType+".item_id",
					errors.New("arguments done is missing item_id and cannot be attributed")))
			}
			var snap string
			var snapErr error
			if isCustom {
				snap, snapErr = model.ResponsesItem{Input: snapEvt.Input}.InputString()
			} else {
				snap, snapErr = model.ResponsesItem{Arguments: snapEvt.Arguments}.ArgumentsString()
			}
			if snapErr != nil {
				// T4 移交①：快照解码失败即非法字节，禁止写入缓冲区后继续
				return failStream(respConvErr(model.CodeMalformedToolCall, eventType, snapErr))
			}
			// P1-1：arguments/input 定稿事件与已确定类型冲突（如 custom delta → function done，
			// reviewer 序列 D）→ 显式失败，禁止把 custom 原始字节重包装成 function arguments
			state, stateErr := getOrCreateToolState(itemID, isCustom, eventType+".type")
			if stateErr != nil {
				return failStream(stateErr)
			}
			if err := applyToolArgumentsSnapshot(state, snap); err != nil {
				return failStream(err)
			}
			if state.Added {
				if err := flushToolState(state, false); err != nil {
					return failStream(err)
				}
			}

		case "response.output_item.added":
			if streamResp.Item == nil {
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType,
					errors.New("output_item.added is missing the item payload (responses §7)")))
			}
			item := *streamResp.Item
			switch item.Type {
			case "message", "reasoning":
				// 结构事件：文本/推理内容由 delta 事件投递，此处无客户端可见载荷
			case "function_call", "custom_tool_call":
				if item.ID == "" {
					return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType+".item.id",
						errors.New("tool call item added is missing item id")))
				}
				isCustom := item.Type == "custom_tool_call"
				// P1-1：类型与身份经"已确定来源"校验（同值重复 added 幂等，冲突显式失败）
				state, stateErr := getOrCreateToolState(item.ID, isCustom, eventType+".item.type")
				if stateErr != nil {
					return failStream(stateErr)
				}
				if idErr := applyToolCallIdentity(state, item, eventType); idErr != nil {
					return failStream(idErr)
				}
				var initArgs string
				var initErr error
				if isCustom {
					initArgs, initErr = item.InputString()
				} else {
					initArgs, initErr = item.ArgumentsString()
					if initArgs == "{}" {
						// 报告二 4：added 的 "{}" 是上游空占位，不是初值，禁止与后续 delta 重复
						initArgs = ""
					}
				}
				if initErr != nil {
					// T4 移交①：初值必须经 ArgumentsString()/InputString() 解码，
					// 解码失败的非法字节不写入缓冲区，直接流协议错误
					return failStream(respConvErr(model.CodeMalformedToolCall,
						eventType+".item.arguments", initErr))
				}
				if err := applyToolArgumentsSnapshot(state, initArgs); err != nil {
					return failStream(err)
				}
				if !state.Added && state.CallID != "" && state.Name != "" {
					state.Added = true
					if err := flushToolState(state, true); err != nil {
						return failStream(err)
					}
				}
			default:
				// DN-4 流式：不可映射 output item 显式失败，不得静默忽略后返回成功
				return failStream(respConvErr(model.CodeUnsupportedOutputItem,
					eventType+".item.type",
					fmt.Errorf("output item type %q has no Chat completion equivalent (DN-4)", item.Type)))
			}

		case "response.output_item.done":
			if streamResp.Item == nil {
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType,
					errors.New("output_item.done is missing the item payload (responses §7)")))
			}
			item := *streamResp.Item
			switch item.Type {
			case "message", "reasoning":
				// 结构事件：内容已经 delta 投递完成
			case "function_call", "custom_tool_call":
				if item.ID == "" {
					return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType+".item.id",
						errors.New("tool call item done is missing item id")))
				}
				isCustom := item.Type == "custom_tool_call"
				// added/delta 全部缺失时 done 也能补全并只发送未发内容（报告二 23）；
				// P1-1：done 携带的类型/身份与已确定来源冲突（reviewer 序列 E：custom delta →
				// function_call item done）→ 显式失败，禁止 state.Custom/Name 静默覆盖
				state, stateErr := getOrCreateToolState(item.ID, isCustom, eventType+".item.type")
				if stateErr != nil {
					return failStream(stateErr)
				}
				if idErr := applyToolCallIdentity(state, item, eventType); idErr != nil {
					return failStream(idErr)
				}
				var finalArgs string
				var snapErr error
				if isCustom {
					finalArgs, snapErr = item.InputString()
				} else {
					finalArgs, snapErr = extractToolCallArgs(item)
				}
				if snapErr != nil {
					return failStream(respConvErr(model.CodeMalformedToolCall,
						eventType+".item", snapErr))
				}
				if err := applyToolArgumentsSnapshot(state, finalArgs); err != nil {
					return failStream(err)
				}
				if !state.Added {
					state.Added = true
					if err := flushToolState(state, true); err != nil {
						return failStream(err)
					}
				} else if err := flushToolState(state, false); err != nil {
					return failStream(err)
				}
				state.Done = true
			default:
				return failStream(respConvErr(model.CodeUnsupportedOutputItem,
					eventType+".item.type",
					fmt.Errorf("output item type %q has no Chat completion equivalent (DN-4)", item.Type)))
			}

		case "response.output_text.done", "response.refusal.done",
			"response.reasoning_text.done", "response.reasoning_summary_text.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
			"response.content_part.added", "response.content_part.done":
			// 定稿/结构确认事件：载荷为已增量投递内容的原样重复，无新增客户端可见信息

		case "response.completed", "response.incomplete":
			if streamResp.Response == nil {
				return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType,
					errors.New("terminal event must carry the full response object (responses §7)")))
			}
			finalizeTerminalResponse(streamResp.Response)
			break loop

		case "response.failed":
			// 报告二 5/22：failed 转 Chat stream error 后 [DONE]，不得生成任何 finish
			r := streamResp.Response
			cause := errors.New("upstream reported response.failed")
			if r != nil && r.Error != nil {
				cause = fmt.Errorf("%s: %s", r.Error.Code, r.Error.Message)
			}
			return failStream(respConvErr(model.CodeInvalidStreamEvent, "response.failed", cause))

		case "response.error", "error":
			var errEvent model.ResponseStreamErrorEvent
			cause := errors.New("upstream reported a stream error")
			if err := json.Unmarshal([]byte(payload), &errEvent); err == nil {
				cause = fmt.Errorf("%s: %s", errEvent.Code, errEvent.Message)
			}
			return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType, cause))

		case "response.cancelled":
			// cancelled 不是自然停止：经 T4 映射契约必然返回错误，禁止落入 stop
			_, mapErr := mapFinishReason("cancelled", "", false)
			return failStream(mapErr)

		default:
			// 未知事件名不得静默忽略（DN-4 流式：unsupported/unknown → 显式终止）
			return failStream(respConvErr(model.CodeInvalidStreamEvent, eventType,
				fmt.Errorf("unrecognized Responses stream event %q (responses §7)", eventType)))
		}
	}

	if terminalDone {
		// 客户端流已按终态合法收尾，不再写入任何内容；
		// 成功终态后再探测一次读错误：终态之后的上游断链仍回传统计侧（渠道失败判断），
		// 但不影响已完成的客户端流。
		if outErrResp == nil {
			if _, dErr := codex.ReadSSEEvent(reader, constant.ScannerBufferMax); dErr != nil &&
				!errors.Is(dErr, io.EOF) && !errors.Is(dErr, context.Canceled) {
				return outUsage, codex.ErrorWrapper(dErr, "stream_read_error", http.StatusInternalServerError)
			}
		}
		return outUsage, outErrResp
	}
	// 走到这里 = EOF 且无终态事件
	return failStream(respConvErr(model.CodeInvalidStreamEvent, "stream",
		errors.New("Responses stream ended before a terminal event (response.completed/incomplete/failed) (responses §7)")))
}

// streamEventDeltaString 严格读取事件 delta 字符串；delta 必须为字符串（Responses §7），
// 非字符串载荷（对象/数组等非法字节）返回 invalid_stream_event，禁止写入任何缓冲区。
func streamEventDeltaString(event *model.ResponsesStreamEvent) (string, error) {
	switch d := event.Delta.(type) {
	case nil:
		return "", nil
	case string:
		return d, nil
	default:
		return "", respConvErr(model.CodeInvalidStreamEvent, event.Type+".delta",
			fmt.Errorf("delta payload must be a string (responses §7), got %T", event.Delta))
	}
}

// toolCallKindName 工具类型断言的可读名，仅用于错误消息。
func toolCallKindName(custom bool) string {
	if custom {
		return "custom_tool_call"
	}
	return "function_call"
}

// applyToolCallIdentity 合并 added/done 事件携带的 call_id/name 身份字段（P1-1）：
// 非空才视为"携带"（空字段=未携带，不算冲突），同值重复事件幂等；
// 与已确定来源值不一致即上游自相矛盾且不可恢复，返回 malformed_tool_call 由调用方
// 走 failStream 显式失败，禁止静默覆盖（Path 用源事件路径，如 response.output_item.done.item.name）。
func applyToolCallIdentity(state *toolCallState, item model.ResponsesItem, srcPath string) error {
	if item.CallID != "" {
		if state.CallID != "" && state.CallID != item.CallID {
			return respConvErr(model.CodeMalformedToolCall, srcPath+".item.call_id",
				fmt.Errorf("tool call %q call_id conflict: %q vs %q", state.ItemID, state.CallID, item.CallID))
		}
		state.CallID = item.CallID
	}
	if item.Name != "" {
		if state.Name != "" && state.Name != item.Name {
			return respConvErr(model.CodeMalformedToolCall, srcPath+".item.name",
				fmt.Errorf("tool call %q name conflict: %q vs %q", state.ItemID, state.Name, item.Name))
		}
		state.Name = item.Name
	}
	return nil
}

// applyToolArgumentsSnapshot 将 added/done 携带的参数快照与已缓冲 delta 序列合并：
// 快照必须与缓冲区前缀兼容（延长或等值）；冲突说明上游状态自相矛盾且不可恢复（报告二 4）。
func applyToolArgumentsSnapshot(state *toolCallState, snapshot string) error {
	if snapshot == "" {
		return nil
	}
	current := state.Arguments.String()
	switch {
	case strings.HasPrefix(snapshot, current):
		state.Arguments.Reset()
		state.Arguments.WriteString(snapshot)
	case strings.HasPrefix(current, snapshot):
		// 缓冲区已覆盖快照，快照没有新增字节
	default:
		return respConvErr(model.CodeMalformedToolCall, "item.arguments",
			fmt.Errorf("arguments snapshot %q conflicts with streamed delta %q (responses §7)", snapshot, current))
	}
	return nil
}

// chatChunkID 输出 Chat §7.1 必填 id；metadata 尚未就绪（response.created 缺失的灾难路径）
// 时保持非空占位，保证错误 chunk 仍是可解码的合法 chunk。
func chatChunkID(metadata chatStreamMetadata) string {
	if metadata.ID == "" {
		return "chatcmpl-stream-error"
	}
	return "chatcmpl-" + metadata.ID
}

// writeChatDeltaSSE 写一条携带完整公共 metadata 的 Chat delta SSE；
// usage 成员恒在（Chat §2.4：独立 usage chunk 之外一律为 null）。
func writeChatDeltaSSE(c *gin.Context, metadata chatStreamMetadata, choices []any) error {
	payload := map[string]any{
		"id":      chatChunkID(metadata),
		"object":  "chat.completion.chunk",
		"created": metadata.CreatedAt,
		"model":   metadata.Model,
		"choices": choices,
		"usage":   nil,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	render.StringData(c, string(data))
	return nil
}

// writeChatRoleDelta 写首个 choice chunk 的 role 分片（Chat §7.2，报告二 6）；
// RoleSent 已置位时幂等跳过，role 全流只出现一次。
func writeChatRoleDelta(c *gin.Context, metadata chatStreamMetadata) error {
	if metadata.RoleSent {
		return nil
	}
	choices := []any{map[string]any{
		"index":         0,
		"delta":         map[string]any{"role": "assistant"},
		"finish_reason": nil,
	}}
	return writeChatDeltaSSE(c, metadata, choices)
}

// writeChatContentDelta 写 content / reasoning_content / refusal 增量（Chat §7.2 delta 字段；
// reasoning_content 为网关扩展字段，与非流式 T4 同一约定）。
func writeChatContentDelta(c *gin.Context, metadata chatStreamMetadata, field, value string) error {
	choices := []any{map[string]any{
		"index":         0,
		"delta":         map[string]any{field: value},
		"finish_reason": nil,
	}}
	return writeChatDeltaSSE(c, metadata, choices)
}

// writeChatToolCallFragment 写一条 tool call 增量分片（Chat §7.2 ToolCallChunk）：
// withHeader 时携带 id 与 name（仅首分片），后续分片只带 arguments/input 增量；
// custom 状态输出 type:"custom"+custom:{name,input} 变体，禁止伪装 function（DN-3 流式）。
func writeChatToolCallFragment(c *gin.Context, metadata chatStreamMetadata, state *toolCallState, argsDelta string, withHeader bool) error {
	frag := map[string]any{"index": state.Index}
	var payload map[string]any
	var argsKey string
	if state.Custom {
		frag["type"] = "custom"
		payload = map[string]any{}
		frag["custom"] = payload
		argsKey = "input"
	} else {
		frag["type"] = "function"
		payload = map[string]any{}
		frag["function"] = payload
		argsKey = "arguments"
	}
	if withHeader {
		frag["id"] = state.CallID
		payload["name"] = state.Name
	}
	payload[argsKey] = argsDelta
	choices := []any{map[string]any{
		"index": 0,
		"delta": map[string]any{
			"tool_calls": []any{frag},
		},
		"finish_reason": nil,
	}}
	return writeChatDeltaSSE(c, metadata, choices)
}

// writeChatStreamFinish 写 finish chunk：delta 为空对象，finish_reason 由 T4 同一映射契约给出
// （报告二 5）；usage 恒为 null，独立 usage chunk 紧随其后（Chat §2.4/§7.3）。
func writeChatStreamFinish(c *gin.Context, metadata chatStreamMetadata, finishReason string) error {
	choices := []any{map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": finishReason,
	}}
	return writeChatDeltaSSE(c, metadata, choices)
}

// writeChatUsageChunk 写 Chat §7.3 的独立 usage chunk：choices 为空数组，仅 usage 非 null，
// 位于 finish chunk 之后、[DONE] 之前；保留 T1 内部 details（报告二 20/21）。
func writeChatUsageChunk(c *gin.Context, metadata chatStreamMetadata, usage model.Usage) error {
	payload := map[string]any{
		"id":      chatChunkID(metadata),
		"object":  "chat.completion.chunk",
		"created": metadata.CreatedAt,
		"model":   metadata.Model,
		"choices": []any{},
		"usage":   usage,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	render.StringData(c, string(data))
	return nil
}

// writeChatStreamProtocolError 写协议错误 chunk（SSE header 已提交后的合法错误形态）：
// 全量公共 metadata + error 成员（message/type/code），code 取 §1.3 稳定机器码；
// 若 role 尚未发出，错误 chunk 自身承担首个 choice chunk 的 role（Chat §7.2）。
func writeChatStreamProtocolError(c *gin.Context, metadata chatStreamMetadata, err error) error {
	delta := map[string]any{}
	if !metadata.RoleSent {
		delta["role"] = "assistant"
	}
	payload := map[string]any{
		"id":      chatChunkID(metadata),
		"object":  "chat.completion.chunk",
		"created": metadata.CreatedAt,
		"model":   metadata.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
		"usage": nil,
		"error": map[string]any{
			"message": err.Error(),
			"type":    "upstream_error",
			"code":    chatStreamErrorCode(err),
		},
	}
	data, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return marshalErr
	}
	render.StringData(c, string(data))
	return nil
}

// chatStreamErrorCode 提取错误 chunk 的稳定机器码；非协议转换错误（如上游读异常）用通用码。
func chatStreamErrorCode(err error) string {
	var convErr *model.ProtocolConversionError
	if errors.As(err, &convErr) {
		return convErr.Code
	}
	return "stream_error"
}

// streamItemID 从 ResponsesStreamEvent 中提取 item_id
func streamItemID(event *model.ResponsesStreamEvent) string {
	if event.ItemID != "" {
		return event.ItemID
	}
	if event.Item != nil && event.Item.ID != "" {
		return event.Item.ID
	}
	return ""
}

// ==================== Request Conversion ====================

// chatConvErr 构造 Chat→Responses 请求方向的协议转换错误（契约 §1.3）。
func chatConvErr(code, path string, cause error) *model.ProtocolConversionError {
	return &model.ProtocolConversionError{
		Code:      code,
		Direction: model.DirectionChatRequestToResponses,
		Path:      path,
		Cause:     cause,
	}
}

// isDefaultStopSequences 判定 stop 是否等价协议缺省：未提供、空串或空数组都不携带停止序列意图。
func isDefaultStopSequences(stop any) bool {
	switch s := stop.(type) {
	case nil:
		return true
	case string:
		return s == ""
	case []any:
		return len(s) == 0
	case []string:
		// Go 侧直接构造（不经 JSON 解码）的 []string 同为 stop 载体，空切片是缺省语义
		return len(s) == 0
	default:
		return false
	}
}

// isDefaultLogitBias 判定 logit_bias 是否等价协议缺省（未提供或空对象）。
func isDefaultLogitBias(logitBias any) bool {
	switch m := logitBias.(type) {
	case nil:
		return true
	case map[string]any:
		return len(m) == 0
	default:
		return false
	}
}

// validateUnsupportedChatRequestFields 拒绝 Responses 请求无等价表达的 Chat 顶层字段（报告二 11）：
// 非缺省值显式 400，不得静默删除；显式缺省等价值（0/空/n≤1）无损可省略。
func validateUnsupportedChatRequestFields(request *model.GeneralOpenAIRequest) error {
	if !isDefaultStopSequences(request.Stop) {
		return chatConvErr(model.CodeUnsupportedMapping, "stop", errors.New("Responses request has no stop field"))
	}
	if request.FrequencyPenalty != nil && *request.FrequencyPenalty != 0 {
		return chatConvErr(model.CodeUnsupportedMapping, "frequency_penalty", errors.New("Responses request cannot express frequency_penalty"))
	}
	if request.PresencePenalty != nil && *request.PresencePenalty != 0 {
		return chatConvErr(model.CodeUnsupportedMapping, "presence_penalty", errors.New("Responses request cannot express presence_penalty"))
	}
	if !isDefaultLogitBias(request.LogitBias) {
		return chatConvErr(model.CodeUnsupportedMapping, "logit_bias", errors.New("Responses request cannot express logit_bias"))
	}
	// n 仅 0/1 视缺省单 choice，其他值无法映射为单个 choice
	if request.N != 0 && request.N != 1 {
		return chatConvErr(model.CodeUnsupportedMapping, "n", fmt.Errorf("n=%d cannot be mapped to a single choice response", request.N))
	}
	return nil
}

// validateChatContentParts 校验消息 content 数组的必填子字段与 assistant 互斥约束（Chat §4）。
// 非对象 part 由 T3 的防御性 ParseContent/buildInputItems 显式拒绝（返回 error），此处不双写；
// 本函数同时前置拦截畸形 image_url 等必填子字段问题（报告二 8）。
func validateChatContentParts(msgPath string, msg model.Message) error {
	parts, ok := msg.Content.([]any)
	if !ok {
		return nil
	}
	textParts, refusalParts := 0, 0
	for j, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		partPath := fmt.Sprintf("%s.content[%d]", msgPath, j)
		switch ctype, _ := pm["type"].(string); ctype {
		case "image_url":
			iu, ok := pm["image_url"].(map[string]any)
			if !ok {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".image_url", errors.New("image_url part requires an object (chat §4)"))
			}
			if urlStr, ok := iu["url"].(string); !ok || urlStr == "" {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".image_url.url", errors.New("image_url.url is required (chat §4)"))
			}
		case "input_audio":
			ia, ok := pm["input_audio"].(map[string]any)
			if !ok {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".input_audio", errors.New("input_audio part requires an object (chat §4)"))
			}
			if data, ok := ia["data"].(string); !ok || data == "" {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".input_audio.data", errors.New("input_audio.data is required (chat §4)"))
			}
			if format, ok := ia["format"].(string); !ok || format == "" {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".input_audio.format", errors.New("input_audio.format is required (chat §4)"))
			}
		case "file":
			fm, ok := pm["file"].(map[string]any)
			if !ok {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".file", errors.New("file part requires an object (chat §4)"))
			}
			fileID, _ := fm["file_id"].(string)
			fileData, _ := fm["file_data"].(string)
			if fileID == "" && fileData == "" {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".file", errors.New("file part requires file_id or file_data (chat §4)"))
			}
		case "text":
			textParts++
		case "refusal":
			if r, ok := pm["refusal"].(string); !ok || r == "" {
				return chatConvErr(model.CodeInvalidSourceShape, partPath+".refusal", errors.New("refusal part requires a non-empty refusal string (chat §4)"))
			}
			refusalParts++
		}
	}
	// Chat §4：assistant 只允许 text 或恰一个 refusal，组合非法不得择一映射
	if msg.Role == "assistant" && refusalParts > 0 && (textParts > 0 || refusalParts > 1) {
		return chatConvErr(model.CodeInvalidSourceShape, msgPath+".content",
			errors.New("assistant message must contain text or exactly one refusal (chat §4)"))
	}
	return nil
}

// validateChatMessagesForResponses 是 buildInputItems（T3 拥有合法映射）前的前置校验：
// 畸形多模态必填子字段、assistant text+refusal 互斥、call_id 空/重复/悬空集合约束、
// legacy function role（§4 DN-2 决议：非空显式 400 拒绝，不得静默删除）。
func validateChatMessagesForResponses(messages []model.Message) error {
	calledIDs := make(map[string]bool)
	pairedOutputs := make(map[string]bool)

	for i, msg := range messages {
		msgPath := fmt.Sprintf("messages[%d]", i)

		if msg.Role == "function" {
			if msg.StringContent() != "" {
				return chatConvErr(model.CodeUnsupportedMapping, msgPath+".content",
					errors.New("legacy role \"function\" has no Responses input item equivalent (DN-2)"))
			}
			continue
		}

		if err := validateChatContentParts(msgPath, msg); err != nil {
			return err
		}

		for j, tc := range msg.ToolCalls {
			idPath := fmt.Sprintf("%s.tool_calls[%d].id", msgPath, j)
			if tc.Id == "" {
				return chatConvErr(model.CodeMalformedToolCall, idPath, errors.New("tool call id must not be empty"))
			}
			if calledIDs[tc.Id] {
				return chatConvErr(model.CodeMalformedToolCall, idPath, fmt.Errorf("duplicate tool call id %q", tc.Id))
			}
			calledIDs[tc.Id] = true
		}

		if msg.Role == "tool" {
			outPath := msgPath + ".tool_call_id"
			if msg.ToolCallId == "" {
				return chatConvErr(model.CodeMalformedToolCall, outPath, errors.New("tool message tool_call_id must not be empty"))
			}
			if !calledIDs[msg.ToolCallId] {
				return chatConvErr(model.CodeMalformedToolCall, outPath,
					fmt.Errorf("tool output for call_id %q has no preceding tool call", msg.ToolCallId))
			}
			if pairedOutputs[msg.ToolCallId] {
				return chatConvErr(model.CodeMalformedToolCall, outPath,
					fmt.Errorf("duplicate tool output for call_id %q", msg.ToolCallId))
			}
			pairedOutputs[msg.ToolCallId] = true
		}
	}
	return nil
}

// convertChatTool 把 Chat §5.1 嵌套 function tool 扁平化为 Responses §9 形状（去掉 function 中间层，
// 保留 strict）。返回错误的 Path 为 tool 相对路径，由调用方补 tools[i] 前缀。
func convertChatTool(tool model.Tool) (map[string]any, error) {
	if tool.Type != "" && tool.Type != "function" {
		return nil, chatConvErr(model.CodeUnsupportedMapping, "type",
			fmt.Errorf("chat tool type %q has no Responses tools[] equivalent", tool.Type))
	}
	if tool.Function.Name == "" {
		return nil, chatConvErr(model.CodeInvalidSourceShape, "function.name",
			errors.New("function.name is required (chat §5.1)"))
	}
	flat := map[string]any{
		"type": "function",
		"name": tool.Function.Name,
	}
	if tool.Function.Description != "" {
		flat["description"] = tool.Function.Description
	}
	if tool.Function.Parameters != nil {
		flat["parameters"] = tool.Function.Parameters
	}
	if tool.Function.Strict != nil {
		flat["strict"] = *tool.Function.Strict
	}
	return flat, nil
}

// convertChatToolChoice 转换 Chat §5.2 tool_choice：字符串枚举原样；
// function 对象从嵌套形状转 Responses {type,name}；其余形态无等价表达显式拒绝（报告二 2）。
func convertChatToolChoice(choice any) (any, error) {
	switch v := choice.(type) {
	case nil:
		return nil, nil
	case string:
		switch v {
		case "auto", "none", "required":
			return v, nil
		default:
			return nil, chatConvErr(model.CodeInvalidSourceShape, "tool_choice",
				fmt.Errorf("unknown tool_choice string %q (chat §5.2)", v))
		}
	case map[string]any:
		ctype, _ := v["type"].(string)
		switch ctype {
		case "function":
			fn, ok := v["function"].(map[string]any)
			if !ok {
				return nil, chatConvErr(model.CodeInvalidSourceShape, "tool_choice.function",
					errors.New("tool_choice function form requires a function object (chat §5.2)"))
			}
			name, ok := fn["name"].(string)
			if !ok || name == "" {
				return nil, chatConvErr(model.CodeInvalidSourceShape, "tool_choice.function.name",
					errors.New("tool_choice.function.name is required (chat §5.2)"))
			}
			return map[string]any{"type": "function", "name": name}, nil
		case "custom", "allowed_tools":
			return nil, chatConvErr(model.CodeUnsupportedMapping, "tool_choice."+ctype,
				fmt.Errorf("tool_choice %q form has no equivalent for function-only tools (chat §5.2)", ctype))
		default:
			return nil, chatConvErr(model.CodeInvalidSourceShape, "tool_choice.type",
				fmt.Errorf("unknown tool_choice type %q (chat §5.2)", ctype))
		}
	default:
		return nil, chatConvErr(model.CodeInvalidSourceShape, "tool_choice",
			fmt.Errorf("unsupported tool_choice shape %T (chat §5.2)", choice))
	}
}

// convertChatResponseFormat 把 Chat §2.4 response_format 转为 Responses §2 text.format 形状，
// json_schema 从 Chat 嵌套形状恢复为 Responses 扁平形状（报告二 9）。
func convertChatResponseFormat(rf *model.ResponseFormat) (map[string]any, error) {
	switch rf.Type {
	case "", "text":
		return map[string]any{"type": "text"}, nil
	case "json_object":
		return map[string]any{"type": "json_object"}, nil
	case "json_schema":
		if rf.JsonSchema == nil {
			return nil, chatConvErr(model.CodeInvalidSourceShape, "response_format.json_schema",
				errors.New("json_schema object is required (chat §2.4)"))
		}
		if rf.JsonSchema.Name == "" {
			return nil, chatConvErr(model.CodeInvalidSourceShape, "response_format.json_schema.name",
				errors.New("json_schema.name is required (chat §2.4)"))
		}
		format := map[string]any{
			"type": "json_schema",
			"name": rf.JsonSchema.Name,
		}
		if rf.JsonSchema.Description != "" {
			format["description"] = rf.JsonSchema.Description
		}
		if rf.JsonSchema.Schema != nil {
			format["schema"] = rf.JsonSchema.Schema
		}
		if rf.JsonSchema.Strict != nil {
			format["strict"] = *rf.JsonSchema.Strict
		}
		return format, nil
	default:
		return nil, chatConvErr(model.CodeInvalidSourceShape, "response_format.type",
			fmt.Errorf("unknown response_format type %q (chat §2.4)", rf.Type))
	}
}

// convertChatToResponsesRequest 把 Chat Completions 请求转为 Responses 请求。
// 所有不可无损映射（顶层不支持字段、畸形消息输入）在此前置拒绝并以
// *model.ProtocolConversionError 上抛；可映射字段按报告二 1/2/9/10/11 保真转换。
func convertChatToResponsesRequest(request *model.GeneralOpenAIRequest) (*model.ResponsesRequest, error) {
	if request == nil {
		return nil, chatConvErr(model.CodeInvalidSourceShape, "", errors.New("chat request is nil"))
	}
	if err := validateUnsupportedChatRequestFields(request); err != nil {
		return nil, err
	}
	if err := validateChatMessagesForResponses(request.Messages); err != nil {
		return nil, err
	}

	respReq := &model.ResponsesRequest{
		Model:             request.Model,
		Temperature:       request.Temperature,
		TopP:              request.TopP,
		Stream:            request.Stream,
		User:              request.User,
		Store:             request.Store,
		Metadata:          request.Metadata,
		Modalities:        request.Modalities,
		ServiceTier:       request.ServiceTier,
		ParallelToolCalls: request.ParallelTooCalls,
	}

	// max_completion_tokens 优先于旧 max_tokens（chat §2.1/§10）
	maxOut := request.MaxTokens
	if request.MaxCompletionTokens != nil && *request.MaxCompletionTokens > 0 {
		maxOut = *request.MaxCompletionTokens
	}
	respReq.MaxTokens = maxOut

	if request.StreamOptions != nil {
		respReq.StreamOptions = request.StreamOptions
	}
	if request.ReasoningEffort != nil && *request.ReasoningEffort != "" {
		respReq.Reasoning = map[string]any{"effort": *request.ReasoningEffort}
	}

	text := map[string]any{}
	if request.ResponseFormat != nil {
		format, err := convertChatResponseFormat(request.ResponseFormat)
		if err != nil {
			return nil, err
		}
		text["format"] = format
	}
	if request.Verbosity != "" {
		text["verbosity"] = request.Verbosity
	}
	if len(text) > 0 {
		respReq.Text = text
	}

	toolChoice, err := convertChatToolChoice(request.ToolChoice)
	if err != nil {
		return nil, err
	}
	respReq.ToolChoice = toolChoice

	// 构建 input items（T3 映射合法消息；error 传播覆盖绕过前置校验的防御路径）
	if len(request.Messages) > 0 {
		inputItems, itemsErr := buildInputItems(request.Messages)
		if itemsErr != nil {
			return nil, itemsErr
		}
		if len(inputItems) > 0 {
			respReq.Input = inputItems
		}
	}

	// 转换 tools 为 Responses §9 扁平形状
	if len(request.Tools) > 0 {
		rawTools := make([]any, 0, len(request.Tools))
		for i, t := range request.Tools {
			flat, toolErr := convertChatTool(t)
			if toolErr != nil {
				var pe *model.ProtocolConversionError
				if errors.As(toolErr, &pe) {
					pe.Path = fmt.Sprintf("tools[%d].%s", i, pe.Path)
				}
				return nil, toolErr
			}
			rawTools = append(rawTools, flat)
		}
		respReq.RawTools = rawTools
	}

	return respReq, nil
}

// chatRoleAllowedParts 锁定 Chat §4 的 role × content part 矩阵；
// function（legacy）只允许 text，其整体映射策略由 DN-2 单独处理。
var chatRoleAllowedParts = map[string]map[string]bool{
	"system":    {model.ContentTypeText: true},
	"developer": {model.ContentTypeText: true},
	"user": {
		model.ContentTypeText:       true,
		model.ContentTypeImageURL:   true,
		model.ContentTypeInputAudio: true,
		model.ContentTypeFile:       true,
	},
	"assistant": {model.ContentTypeText: true, model.ContentTypeRefusal: true},
	"tool":      {model.ContentTypeText: true},
	"function":  {model.ContentTypeText: true},
}

// chatContentParts 解析消息 content 并按 Chat §4 矩阵校验 part 类型。
// isString 为 true 时 parts 为空，字符串内容由调用方经 StringContent 处理；
// Content 缺失（nil）返回 (nil, false, nil)，是否必填由各 role 分支决定。
func chatContentParts(msg model.Message, msgPath string) (parts []model.MessageContent, isString bool, err error) {
	if msg.Content == nil {
		return nil, false, nil
	}
	if msg.IsStringContent() {
		return nil, true, nil
	}
	parsed, parseErr := msg.ParseContent()
	if parseErr != nil {
		return nil, false, chatConvErr(model.CodeInvalidSourceShape, msgPath+".content", parseErr)
	}
	allowed := chatRoleAllowedParts[msg.Role]
	for j, p := range parsed {
		if !allowed[p.Type] {
			return nil, false, chatConvErr(model.CodeInvalidSourceShape,
				fmt.Sprintf("%s.content[%d]", msgPath, j),
				fmt.Errorf("part type %q not allowed for role %q (chat §4)", p.Type, msg.Role))
		}
	}
	return parsed, false, nil
}

// buildInputItems 把已通过 T2 前置校验的 chat messages 转为 Responses input 数组
// （报告二 12/13/14/15 的合法映射路径）。被直接调用绕过前置校验时同样是防御边界：
// 任何不可无损映射的输入返回显式 *model.ProtocolConversionError，不 panic、不静默丢。
func buildInputItems(messages []model.Message) ([]any, error) {
	var items []any

	for i, msg := range messages {
		msgPath := fmt.Sprintf("messages[%d]", i)

		switch msg.Role {
		case "system", "developer":
			// §4 DN-1 决议：system 保留 role:"system"，developer 保持 role:"developer"，不再降级
			parts, isString, err := chatContentParts(msg, msgPath)
			if err != nil {
				return nil, err
			}
			var content string
			if isString {
				content = msg.StringContent()
			} else {
				for _, p := range parts {
					content += p.Text
				}
			}
			if content == "" {
				continue
			}
			items = append(items, map[string]any{
				"type":    "message",
				"role":    msg.Role,
				"content": content,
			})

		case "user":
			// Chat §4 → Responses §3.1：text/image_url/input_audio/file →
			// input_text/input_image/input_audio/input_file，无静默丢失（报告二 13）
			item := map[string]any{
				"type": "message",
				"role": "user",
			}
			parts, isString, err := chatContentParts(msg, msgPath)
			if err != nil {
				return nil, err
			}
			switch {
			case isString:
				item["content"] = msg.StringContent()
			case parts == nil:
				return nil, chatConvErr(model.CodeInvalidSourceShape, msgPath+".content",
					errors.New("user message content is required (chat §3.1)"))
			default:
				blocks := make([]any, 0, len(parts))
				for _, p := range parts {
					switch p.Type {
					case model.ContentTypeText:
						blocks = append(blocks, map[string]any{"type": "input_text", "text": p.Text})
					case model.ContentTypeImageURL:
						blocks = append(blocks, map[string]any{"type": "input_image", "image_url": p.ImageURL.Url})
					case model.ContentTypeInputAudio:
						blocks = append(blocks, map[string]any{"type": "input_audio", "input_audio": p.InputAudio})
					case model.ContentTypeFile:
						blocks = append(blocks, map[string]any{"type": "input_file", "file": p.File})
					}
				}
				if len(blocks) == 0 {
					return nil, chatConvErr(model.CodeInvalidSourceShape, msgPath+".content",
						errors.New("user content array must contain at least one part (chat §3.1 minItems 1)"))
				}
				item["content"] = blocks
			}
			items = append(items, item)

		case "assistant":
			// reasoning_content
			if msg.ReasoningContent != nil {
				if rc, ok := msg.ReasoningContent.(string); ok && rc != "" {
					items = append(items, map[string]any{
						"type":    "reasoning",
						"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
					})
				}
			}

			parts, isString, err := chatContentParts(msg, msgPath)
			if err != nil {
				return nil, err
			}
			var blocks []any
			if isString {
				if s := msg.StringContent(); s != "" {
					blocks = append(blocks, map[string]any{"type": "output_text", "text": s})
				}
			} else {
				for _, p := range parts {
					switch p.Type {
					case model.ContentTypeText:
						blocks = append(blocks, map[string]any{"type": "output_text", "text": p.Text})
					case model.ContentTypeRefusal:
						blocks = append(blocks, map[string]any{"type": "refusal", "refusal": p.Refusal})
					}
				}
			}

			// assistant 顶层 refusal 字段 → Responses refusal part（报告二 14）。
			// Chat §4：assistant 仅 text 或恰一个 refusal——顶层字段与任何 content block
			// （text 或 refusal part）并存都不可无损表达，整体显式拒绝（T3 reviewer P2：
			// 旧实现只拦 output_text，refusal part 并存会追加出第二个 refusal part）。
			if msg.Refusal != nil && *msg.Refusal != "" {
				if len(blocks) > 0 {
					return nil, chatConvErr(model.CodeInvalidSourceShape, msgPath+".refusal",
						errors.New("assistant refusal field cannot coexist with content parts (chat §4)"))
				}
				blocks = append(blocks, map[string]any{"type": "refusal", "refusal": *msg.Refusal})
			}
			if len(blocks) > 0 {
				items = append(items, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": blocks,
				})
			}

			// tool_calls：空/重复/悬空 call_id 验证归 T2；此处保持 arguments JSON string
			// 与 call_id 配对输出（报告二 15），并显式拒绝 T2 范围外的空 function.name（不得产出畸形 item）
			for j, tc := range msg.ToolCalls {
				tcPath := fmt.Sprintf("%s.tool_calls[%d]", msgPath, j)
				if tc.Function.Name == "" {
					return nil, chatConvErr(model.CodeMalformedToolCall, tcPath+".function.name",
						errors.New("function.name is required for function_call item (responses §3.2)"))
				}
				args, argsErr := toolCallArgumentsString(tc.Function.Arguments)
				if argsErr != nil {
					return nil, chatConvErr(model.CodeMalformedToolCall, tcPath+".function.arguments", argsErr)
				}
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   tc.Id,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}

		case "tool":
			parts, isString, err := chatContentParts(msg, msgPath)
			if err != nil {
				return nil, err
			}
			var output string
			if isString {
				output = msg.StringContent()
			} else {
				for _, p := range parts {
					output += p.Text
				}
			}
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": msg.ToolCallId,
				"output":  output,
			})

		case "function":
			// §4 DN-2 决议：legacy function role 无 Responses input item 等价物，
			// 非空载荷显式 unsupported_mapping，不得静默删除。T2 前置校验已按
			// StringContent 非空拒绝；此处同一判定扩展到全部载荷（直接调用绕过的防御）。
			if _, _, err := chatContentParts(msg, msgPath); err != nil {
				return nil, err
			}
			if msg.StringContent() != "" || len(msg.ToolCalls) > 0 || msg.ToolCallId != "" {
				return nil, chatConvErr(model.CodeUnsupportedMapping, msgPath+".content",
					errors.New("legacy role \"function\" has no Responses input item equivalent (DN-2)"))
			}

		default:
			return nil, chatConvErr(model.CodeInvalidSourceShape, msgPath+".role",
				fmt.Errorf("unknown message role %q (chat §3)", msg.Role))
		}
	}

	return items, nil
}

// toolCallArgumentsString 将 tool call arguments 转为 Responses §3.2 要求的 JSON 字符串。
// 不可编码输入返回 error，不得静默回退 "{}"。
func toolCallArgumentsString(args any) (string, error) {
	if args == nil {
		return "{}", nil
	}
	switch v := args.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("tool call arguments are not JSON-encodable: %w", err)
		}
		return string(b), nil
	}
}

// extractToolCallArgs 提取 Responses function_call item 的 arguments 实际 JSON 字符串
// （报告二 3；Responses §3.2 → Chat §6.1.1）。缺失或非法 JSON 返回 error，由调用方包装
// malformed_tool_call 并补全 output 路径，禁止静默回退 "{}"。
func extractToolCallArgs(item model.ResponsesItem) (string, error) {
	if len(item.Arguments) == 0 || string(item.Arguments) == "null" {
		return "", errors.New("function_call item is missing required arguments (responses §5)")
	}
	return item.ArgumentsString()
}

// ==================== Non-Streaming Response ====================

// copyUpstreamResponseHeader 透传上游响应头，跳过 hop-by-hop 与长度/编码相关头。
// 上游 Content-Length/Content-Encoding 描述的是原始 Responses body，而实际写回的是
// 转换后的 Chat body（body 已被 Go Transport 解压），原样透传会导致客户端按声明长度截断。
func copyUpstreamResponseHeader(dst http.Header, src http.Header) {
	for k, v := range src {
		switch strings.ToLower(k) {
		case "content-length", "transfer-encoding", "connection", "content-encoding":
			continue
		}
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}

// handleChatCompletionsResponse 处理 Chat Completions 的非流式响应
func handleChatCompletionsResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, codex.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	resp.Body.Close()

	// 检查上游是否返回了错误
	{
		var errResp struct {
			Error *model.Error `json:"error"`
		}
		if err := json.Unmarshal(responseBody, &errResp); err == nil && errResp.Error != nil && errResp.Error.Message != "" {
			// 尝试把 Responses 错误格式映射为 Chat 格式
			mappedBody := mapResponsesErrorToChatFormat(responseBody)
			resp.Body = io.NopCloser(bytes.NewBuffer(mappedBody))
			copyUpstreamResponseHeader(c.Writer.Header(), resp.Header)
			c.Writer.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(c.Writer, resp.Body)
			resp.Body.Close()
			return nil, nil
		}
	}

	// 把上游 Responses 格式转为 Chat 格式；畸形 JSON/不可映射输出显式 502（报告二 25，契约 §1.3）：
	// 不写原 body、不设置成功 capture，ErrorWithStatusCode 交由 controller 统一输出与渠道统计。
	chatResp, convErr := convertResponsesToChat(responseBody, meta.ActualModelName)
	if convErr != nil {
		return nil, upstreamConversionError(convErr)
	}

	// 提取 usage：details 全量保留供日志/未来策略，quota 公式不变
	usage := extractUsageFromResponses(responseBody)

	// 写回客户端
	copyUpstreamResponseHeader(c.Writer.Header(), resp.Header)
	if _, err := c.Writer.Write(chatResp); err != nil {
		return nil, codex.ErrorWrapper(err, "write_response_body_failed", http.StatusInternalServerError)
	}

	c.Set(ctxkey.ResponseBody, string(chatResp))
	return usage, nil
}

// upstreamConversionError 把响应侧协议转换错误映射为 HTTP 502（契约 §1.3：上游非流式响应
// 畸形/不可转换 → model.Error.Type=upstream_error，code=invalid_upstream_response）。
func upstreamConversionError(err error) *model.ErrorWithStatusCode {
	return &model.ErrorWithStatusCode{
		Error: model.Error{
			Message: err.Error(),
			Type:    "upstream_error",
			Param:   "",
			Code:    "invalid_upstream_response",
		},
		StatusCode: http.StatusBadGateway,
	}
}

// respConvErr 构造 Responses 响应→Chat 方向的协议转换错误（契约 §1.3）。
func respConvErr(code, path string, cause error) *model.ProtocolConversionError {
	return &model.ProtocolConversionError{
		Code:      code,
		Direction: model.DirectionResponsesResponseToChat,
		Path:      path,
		Cause:     cause,
	}
}

// chatCompletionResponseWire 等结构锁定 Chat §6/§6.1/§6.1.1 的必填与 null 语义（报告二 16）：
// content/refusal/logprobs 必须序列化输出，无值时为 null——不能用 omitempty 丢键。
type chatCompletionResponseWire struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []chatChoiceWire `json:"choices"`
	Usage   model.Usage      `json:"usage"`
}

type chatChoiceWire struct {
	Index        int             `json:"index"`
	Message      chatMessageWire `json:"message"`
	FinishReason string          `json:"finish_reason"`
	// Logprobs 非流式路径不产生 logprobs，Chat §6 Choice.logprobs* 必填 → 恒为 null
	Logprobs any `json:"logprobs"`
}

type chatMessageWire struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Refusal any    `json:"refusal"`
	// ReasoningContent 是网关扩展字段（非 Chat 标准键），仅在有值时输出
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCallWire `json:"tool_calls,omitempty"`
}

type chatToolCallWire struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function *model.Function `json:"function,omitempty"`
	// Custom 仅 type="custom" 变体使用（Chat §6.1.1，§4 DN-3）：{"name","input"}
	Custom map[string]any `json:"custom,omitempty"`
}

// buildChatToolCallWires 把内部 Tool 载体渲染为 Chat §6.1.1 wire：
// custom 条目输出 {id,type:"custom",custom:{name,input}}，禁止伪装 function（DN-3）。
func buildChatToolCallWires(tools []model.Tool) []chatToolCallWire {
	if len(tools) == 0 {
		return nil
	}
	wires := make([]chatToolCallWire, 0, len(tools))
	for _, tc := range tools {
		if tc.Type == "custom" {
			wires = append(wires, chatToolCallWire{
				ID:   tc.Id,
				Type: "custom",
				Custom: map[string]any{
					"name":  tc.Function.Name,
					"input": tc.Function.Arguments,
				},
			})
			continue
		}
		fn := tc.Function
		wires = append(wires, chatToolCallWire{
			ID:       tc.Id,
			Type:     "function",
			Function: &fn,
		})
	}
	return wires
}

// responsesToChatSummary 是 output 转换的驱动数据：finish_reason 与 incomplete 判定依赖它。
type responsesToChatSummary struct {
	HasToolCalls     bool
	IncompleteReason string
}

// convertResponsesToChat 把 Responses API 非流式 JSON 响应转为 Chat Completions 格式
// （报告二 3/5/7/16/17/18/19/20/25/26/27）。畸形 JSON 与不可映射输出返回
// *model.ProtocolConversionError，不再原样返回上游 body。
func convertResponsesToChat(respBody []byte, fallbackModel string) ([]byte, error) {
	var responsesResp model.ResponsesResponse
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		return nil, respConvErr(model.CodeInvalidSourceJSON, "",
			fmt.Errorf("upstream responses body is not valid JSON: %w", err))
	}
	// Responses §4 必填：id/object 缺失则无法生成 Chat §6 必填字段（报告二 25）
	if responsesResp.ID == "" {
		return nil, respConvErr(model.CodeInvalidSourceShape, "id",
			errors.New("response id is required (responses §4)"))
	}
	if responsesResp.Object != "response" {
		return nil, respConvErr(model.CodeInvalidSourceShape, "object",
			fmt.Errorf("response object must be \"response\", got %q (responses §4)", responsesResp.Object))
	}

	msg, summary, err := convertResponsesOutput(responsesResp.Output)
	if err != nil {
		return nil, err
	}
	if responsesResp.IncompleteDetails != nil {
		summary.IncompleteReason = responsesResp.IncompleteDetails.Reason
	}
	finishReason, err := mapFinishReason(responsesResp.Status, summary.IncompleteReason, summary.HasToolCalls)
	if err != nil {
		return nil, err
	}

	// 报告二 27：model 优先上游 response.model，fallback 仅在为空时使用
	upstreamModel := responsesResp.Model
	if upstreamModel == "" {
		upstreamModel = fallbackModel
	}

	wire := chatCompletionResponseWire{
		// 报告二 26：wire key created_at 经 T1 修正映射进 Chat created
		ID:      fmt.Sprintf("chatcmpl-%s", responsesResp.ID),
		Object:  "chat.completion",
		Created: responsesResp.Created,
		Model:   upstreamModel,
		Choices: []chatChoiceWire{{
			Index: 0,
			Message: chatMessageWire{
				Role:             "assistant",
				Content:          msg.Content, // nil → null（Chat §6.1 必填可空）
				Refusal:          refusalWireValue(msg.Refusal),
				ReasoningContent: reasoningWireValue(msg.ReasoningContent),
				ToolCalls:        buildChatToolCallWires(msg.ToolCalls),
			},
			FinishReason: finishReason,
			Logprobs:     nil,
		}},
		Usage: convertResponsesUsage(responsesResp.Usage),
	}
	result, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completion response failed: %w", err)
	}
	return result, nil
}

// refusalWireValue 区分「无拒绝」（Chat §6.1 必填 → JSON null）与「有拒绝」。
func refusalWireValue(refusal *string) any {
	if refusal == nil {
		return nil
	}
	return *refusal
}

// reasoningWireValue 内部扩展字段以 string 承载；非字符串视为无值不输出。
func reasoningWireValue(rc any) string {
	if s, ok := rc.(string); ok {
		return s
	}
	return ""
}

// convertResponsesOutput 把 Responses output 数组（Responses §5）转换为单条 Chat assistant
// 消息与摘要（报告二 7/17/18/19）：
//   - 多个 message item 的文本按 output 顺序聚合，不互相覆盖；
//   - refusal 写 message.refusal 且保持 content:null；文本×refusal 并存或多条 refusal
//     在 Chat §6.1 不可表达，显式错误而非择一覆盖；
//   - reasoning 同时读取 summary[] 与 content[reasoning_text][]，按 item 内 summary→content、
//     item 间 output 顺序聚合到 reasoning_content 扩展；encrypted_content 无 Chat 对应物，
//     整字段丢弃属显式降级（推理文本已经 summary/content 双数组承载）；
//   - §4 DN-3：custom_tool_call 以 Type="custom" 无损承载，wire 层渲染 Chat §6.1.1 custom 变体；
//   - §4 DN-4：其余任何 output item 类型使整个响应转换失败，不得忽略后返回成功。
func convertResponsesOutput(output []model.ResponsesItem) (model.Message, responsesToChatSummary, error) {
	msg := model.Message{Role: "assistant"}
	var summary responsesToChatSummary
	var textBuf strings.Builder
	var refusalParts []string
	var reasoningBuf strings.Builder
	var toolCalls []model.Tool

	for i, item := range output {
		itemPath := fmt.Sprintf("output[%d]", i)
		switch item.Type {
		case "message":
			if item.Role != "" && item.Role != "assistant" {
				return msg, summary, respConvErr(model.CodeInvalidSourceShape, itemPath+".role",
					fmt.Errorf("output message role must be \"assistant\", got %q (responses §5)", item.Role))
			}
			text, refusals, err := extractMessageTextAndRefusals(item, itemPath)
			if err != nil {
				return msg, summary, err
			}
			textBuf.WriteString(text)
			refusalParts = append(refusalParts, refusals...)
		case "reasoning":
			text, err := extractReasoningText(item, itemPath)
			if err != nil {
				return msg, summary, err
			}
			reasoningBuf.WriteString(text)
		case "function_call", "custom_tool_call":
			tc, err := convertResponsesToolCall(item, itemPath)
			if err != nil {
				return msg, summary, err
			}
			toolCalls = append(toolCalls, tc)
			summary.HasToolCalls = true
		default:
			return msg, summary, respConvErr(model.CodeUnsupportedOutputItem, itemPath+".type",
				fmt.Errorf("output item type %q has no Chat Completions representation (responses §5, DN-4)", item.Type))
		}
	}

	if len(refusalParts) > 1 {
		return msg, summary, respConvErr(model.CodeInvalidSourceShape, "output",
			errors.New("multiple refusal parts cannot be expressed in a single chat.completion message (chat §6.1)"))
	}
	if len(refusalParts) == 1 {
		if textBuf.Len() > 0 {
			return msg, summary, respConvErr(model.CodeInvalidSourceShape, "output",
				errors.New("refusal and text content cannot be expressed simultaneously in a chat.completion message (chat §6.1)"))
		}
		refusal := refusalParts[0]
		msg.Refusal = &refusal
	}
	if textBuf.Len() > 0 {
		msg.Content = textBuf.String()
	}
	if reasoningBuf.Len() > 0 {
		msg.ReasoningContent = reasoningBuf.String()
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return msg, summary, nil
}

// extractMessageTextAndRefusals 读取 message item 的 output_text/refusal content 块
// （Responses §5）。未知块类型与非对象块显式拒绝，不静默丢弃。
func extractMessageTextAndRefusals(item model.ResponsesItem, itemPath string) (string, []string, error) {
	var text string
	var refusals []string
	switch content := item.Content.(type) {
	case nil:
		return "", nil, nil
	case string:
		return content, nil, nil
	case []any:
		for j, b := range content {
			blockPath := fmt.Sprintf("%s.content[%d]", itemPath, j)
			bm, ok := b.(map[string]any)
			if !ok {
				return "", nil, respConvErr(model.CodeInvalidSourceShape, blockPath,
					errors.New("message content block must be a JSON object (responses §5)"))
			}
			bt, _ := bm["type"].(string)
			switch bt {
			case "output_text", "text":
				t, ok := bm["text"].(string)
				if !ok {
					return "", nil, respConvErr(model.CodeInvalidSourceShape, blockPath+".text",
						errors.New("output_text.text is required string (responses §5)"))
				}
				text += t
			case "refusal":
				r, ok := bm["refusal"].(string)
				if !ok {
					return "", nil, respConvErr(model.CodeInvalidSourceShape, blockPath+".refusal",
						errors.New("refusal.refusal is required string (responses §5)"))
				}
				refusals = append(refusals, r)
			default:
				return "", nil, respConvErr(model.CodeInvalidSourceShape, blockPath+".type",
					fmt.Errorf("unknown output content block type %q (responses §5)", bt))
			}
		}
		return text, refusals, nil
	default:
		return "", nil, respConvErr(model.CodeInvalidSourceShape, itemPath+".content",
			fmt.Errorf("message content must be string or array, got %T (responses §5)", item.Content))
	}
}

// extractReasoningText 聚合 reasoning item 的 summary_text 与 reasoning_text 块（报告二 17；
// Responses §3.4/§5）。两个数组都完整读取，缺失数组视为空、非法块显式拒绝。
func extractReasoningText(item model.ResponsesItem, itemPath string) (string, error) {
	var out strings.Builder
	if err := appendReasoningBlocks(&out, item.Summary, itemPath, "summary", "summary_text"); err != nil {
		return "", err
	}
	if err := appendReasoningBlocks(&out, item.Content, itemPath, "content", "reasoning_text"); err != nil {
		return "", err
	}
	return out.String(), nil
}

func appendReasoningBlocks(buf *strings.Builder, field any, itemPath, fieldKey, blockType string) error {
	if field == nil {
		return nil
	}
	blocks, ok := field.([]any)
	if !ok {
		return respConvErr(model.CodeInvalidSourceShape, itemPath+"."+fieldKey,
			fmt.Errorf("reasoning %s must be a block array (responses §3.4)", fieldKey))
	}
	for j, b := range blocks {
		blockPath := fmt.Sprintf("%s.%s[%d]", itemPath, fieldKey, j)
		bm, ok := b.(map[string]any)
		if !ok {
			return respConvErr(model.CodeInvalidSourceShape, blockPath,
				errors.New("reasoning block must be a JSON object (responses §3.4)"))
		}
		bt, _ := bm["type"].(string)
		if bt != blockType {
			return respConvErr(model.CodeInvalidSourceShape, blockPath+".type",
				fmt.Errorf("reasoning %s expects %q block, got %q (responses §3.4/§5)", fieldKey, blockType, bt))
		}
		t, ok := bm["text"].(string)
		if !ok {
			return respConvErr(model.CodeInvalidSourceShape, blockPath+".text",
				fmt.Errorf("%s.text is required string (responses §3.4)", blockType))
		}
		buf.WriteString(t)
	}
	return nil
}

// convertResponsesToolCall 把 function_call/custom_tool_call 转为内部 model.Tool 载体
// （报告二 3/18，§4 DN-3）。custom 用 Type="custom" 标记、Function.Name/Arguments 仅作
// 无损内部承载，wire 层由 buildChatToolCallWires 渲染为 Chat §6.1.1 custom 变体。
// Responses §5 必填 call_id/name/arguments(input) 缺失返回 malformed_tool_call。
func convertResponsesToolCall(item model.ResponsesItem, itemPath string) (model.Tool, error) {
	if item.CallID == "" {
		return model.Tool{}, respConvErr(model.CodeMalformedToolCall, itemPath+".call_id",
			errors.New("tool call item is missing required call_id (responses §5)"))
	}
	if item.Name == "" {
		return model.Tool{}, respConvErr(model.CodeMalformedToolCall, itemPath+".name",
			errors.New("tool call item is missing required name (responses §5)"))
	}
	switch item.Type {
	case "function_call":
		args, err := extractToolCallArgs(item)
		if err != nil {
			return model.Tool{}, respConvErr(model.CodeMalformedToolCall, itemPath+".arguments", err)
		}
		return model.Tool{Id: item.CallID, Type: "function", Function: model.Function{Name: item.Name, Arguments: args}}, nil
	case "custom_tool_call":
		if len(item.Input) == 0 || string(item.Input) == "null" {
			return model.Tool{}, respConvErr(model.CodeMalformedToolCall, itemPath+".input",
				errors.New("custom_tool_call item is missing required input (responses §5)"))
		}
		input, err := item.InputString()
		if err != nil {
			return model.Tool{}, respConvErr(model.CodeMalformedToolCall, itemPath+".input", err)
		}
		return model.Tool{Id: item.CallID, Type: "custom", Function: model.Function{Name: item.Name, Arguments: input}}, nil
	default:
		// convertResponsesOutput 的 switch 已保证类型集合，此处防御兜底
		return model.Tool{}, respConvErr(model.CodeUnsupportedOutputItem, itemPath+".type",
			fmt.Errorf("tool call type %q is not supported", item.Type))
	}
}

// mapResponsesErrorToChatFormat 尝试把 Responses 格式的 error 响应映射为 OpenAI Chat 通用错误格式
func mapResponsesErrorToChatFormat(respBody []byte) []byte {
	var responsesResp struct {
		Error *model.ResponseError `json:"error"`
	}
	if err := json.Unmarshal(respBody, &responsesResp); err != nil || responsesResp.Error == nil {
		return respBody
	}
	chatErr := map[string]interface{}{
		"error": map[string]interface{}{
			"message": responsesResp.Error.Message,
			"type":    responsesResp.Error.Type,
			"code":    responsesResp.Error.Code,
		},
	}
	if errMap, ok := chatErr["error"].(map[string]interface{}); ok {
		if typeVal, exists := errMap["type"]; !exists || typeVal == nil || typeVal == "" {
			errMap["type"] = "invalid_request_error"
		}
	}
	if b, err := json.Marshal(chatErr); err == nil {
		return b
	}
	return respBody
}

// extractUsageFromResponses 从 Responses 响应体中提取内部 model.Usage（报告二 20）。
// 畸形 JSON 返回 nil 是本函数的既有容错边界（handler 已在转换失败时 502，不会走到这）；
// details 缺失保持 nil，不合成零值细节。
func extractUsageFromResponses(respBody []byte) *model.Usage {
	var responsesResp model.ResponsesResponse
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		return nil
	}
	usage := convertResponsesUsage(responsesResp.Usage)
	return &usage
}

// convertResponsesUsage 把 Responses §6 usage 映射到内部 model.Usage（报告二 20）：
// 三个总数之外，input 保留 cached/cache_write，output 保留 reasoning/accepted/rejected/audio/text
// details（Chat §8）；上游缺失 details 对象时保持 nil，供调用方识别畸形/兼容来源。
func convertResponsesUsage(usage model.ResponsesUsage) model.Usage {
	out := model.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if usage.InputTokensDetails != nil {
		out.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens:     usage.InputTokensDetails.CachedTokens,
			CacheWriteTokens: usage.InputTokensDetails.CacheWriteTokens,
		}
	}
	if usage.OutputTokensDetails != nil {
		out.CompletionTokensDetails = &model.CompletionTokensDetails{
			ReasoningTokens:          usage.OutputTokensDetails.ReasoningTokens,
			AcceptedPredictionTokens: usage.OutputTokensDetails.AcceptedPredictionTokens,
			RejectedPredictionTokens: usage.OutputTokensDetails.RejectedPredictionTokens,
			AudioTokens:              usage.OutputTokensDetails.AudioTokens,
			TextTokens:               usage.OutputTokensDetails.TextTokens,
		}
	}
	return out
}

// mapFinishReason 把 Responses status + incomplete_details.reason 推导为 Chat finish_reason
// （报告二 5；Responses §4，Chat §6.2/§10）。有工具调用优先 tool_calls；incomplete 仅
// max_output_tokens→length、content_filter→content_filter 可映射；failed/cancelled/queued/
// in_progress 与未知状态不得伪装 stop，一律显式 error（§1.2：response.incomplete 不得被
// 合成为 completed 的姊妹约束）。
func mapFinishReason(status, incompleteReason string, hasToolCalls bool) (string, error) {
	switch status {
	case "completed":
		if hasToolCalls {
			return "tool_calls", nil
		}
		return "stop", nil
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return "length", nil
		case "content_filter":
			return "content_filter", nil
		default:
			return "", respConvErr(model.CodeUnsupportedMapping, "incomplete_details.reason",
				fmt.Errorf("incomplete reason %q has no Chat finish_reason equivalent (chat §6.2)", incompleteReason))
		}
	case "failed", "cancelled", "queued", "in_progress":
		return "", respConvErr(model.CodeUnsupportedMapping, "status",
			fmt.Errorf("response status %q is not a natural stop and must not be faked (responses §4)", status))
	default:
		return "", respConvErr(model.CodeInvalidSourceShape, "status",
			fmt.Errorf("unknown response status %q (responses §4)", status))
	}
}
