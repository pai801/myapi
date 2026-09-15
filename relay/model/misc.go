package model

import (
	"fmt"
	"strings"
)

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails 是网关内部 usage 的输入细分（chat-completions-protocol.md §8.1）。
// CacheWriteTokens 只作为 detail 承载供日志与后续计费策略使用，不参与现有 quota 公式。
type PromptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"` // 缓存命中的token数
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	AudioTokens      int `json:"audio_tokens,omitempty"`
	TextTokens       int `json:"text_tokens,omitempty"`
	ImageTokens      int `json:"image_tokens,omitempty"`
}

// CompletionTokensDetails 是网关内部 usage 的输出细分（chat-completions-protocol.md §8.2）。
type CompletionTokensDetails struct {
	ReasoningTokens          int `json:"reasoning_tokens"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
	AudioTokens              int `json:"audio_tokens,omitempty"`
	TextTokens               int `json:"text_tokens,omitempty"`
}

type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    any    `json:"code"`
}

type ErrorWithStatusCode struct {
	Error
	StatusCode int `json:"status_code"`
}

// SemanticText 返回错误的语义拼接文本（小写 type | code | message），
// 供跨层基于文本 marker 的错误分类复用；nil 接收者返回空串。
func (e *ErrorWithStatusCode) SemanticText() string {
	if e == nil {
		return ""
	}
	parts := []string{
		strings.ToLower(strings.TrimSpace(e.Type)),
		strings.ToLower(strings.TrimSpace(fmt.Sprint(e.Code))),
		strings.ToLower(strings.TrimSpace(e.Message)),
	}
	return strings.Join(parts, " | ")
}

// LooksLikeRequestShapeFailure 判定错误是否属于客户端请求形态类失败（请求体/参数/格式非法等）。
// 此类失败来自客户端输入而非渠道健康问题，不应用于渠道健康惩罚（不计数、不冷却）；
// marker 列表与渠道冷却口径保持一致。nil 接收者返回 false。
func (e *ErrorWithStatusCode) LooksLikeRequestShapeFailure() bool {
	if e == nil {
		return false
	}
	// 网关机器码失败集（Type=upstream_error 且 Code 命中 gatewayFailureCodes）代表失败源自
	// 渠道/上游侧，一律不豁免，覆盖两类变体：
	//   ① 统一兜底包装：Code=invalid_upstream_response，Message 内嵌 malformed_tool_call 等
	//      机器码或 cause 自由文本（upstreamConversionError / responsesStreamFailureError 路径）；
	//   ② errors.As 保留转换机器码的包装：Code 直接是六个协议转换机器码之一
	//      （如 codex chat→responses 的 conversionModelError 保留 malformed_tool_call），
	//      Message 仅承载转换细节；此类失败同样源于上游数据完整性故障。
	// 此处是对 client-400 中途上报路径的刻意取舍——该场景（上游流内声明请求非法）经转换包装后
	// 同样计一次渠道失败，换取上游数据完整性故障（畸形 tool-call 流等）可被检出。
	// marker 列表不动；真正的客户端请求形态错误以 Type=invalid_request_error 表达，不在守卫范围。
	if e.Type == "upstream_error" && isGatewayFailureCode(fmt.Sprint(e.Code)) {
		return false
	}
	text := e.SemanticText()
	requestShapeMarkers := []string{
		"invalid_request_error",
		"invalid request",
		"invalid input",
		"invalid parameter",
		"invalid schema",
		"invalid format",
		"malformed",
		"unsupported_request",
		"request body",
		"request schema",
	}
	for _, marker := range requestShapeMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// 协议转换稳定机器码枚举（全局约束 §1.3）：值一旦发布不得改名，客户端与日志按此分派。
const (
	CodeInvalidSourceJSON     = "invalid_source_json"
	CodeInvalidSourceShape    = "invalid_source_shape"
	CodeUnsupportedMapping    = "unsupported_mapping"
	CodeMalformedToolCall     = "malformed_tool_call"
	CodeInvalidStreamEvent    = "invalid_stream_event"
	CodeUnsupportedOutputItem = "unsupported_output_item"
)

// gatewayFailureCodes 是触发「渠道/上游侧」守卫的网关机器码全集：统一兜底码
// invalid_upstream_response 与上方六个协议转换机器码常量。与常量块同文件，防枚举漂移。
var gatewayFailureCodes = map[string]struct{}{
	"invalid_upstream_response": {},
	CodeInvalidSourceJSON:       {},
	CodeInvalidSourceShape:      {},
	CodeUnsupportedMapping:      {},
	CodeMalformedToolCall:       {},
	CodeInvalidStreamEvent:      {},
	CodeUnsupportedOutputItem:   {},
}

// isGatewayFailureCode 判定 code 是否属于网关机器码失败集。
func isGatewayFailureCode(code string) bool {
	_, ok := gatewayFailureCodes[code]
	return ok
}

// 协议转换方向枚举（全局约束 §1.3），Path 使用源协议 JSON path。
const (
	DirectionChatRequestToResponses  = "chat_request_to_responses"
	DirectionResponsesRequestToChat  = "responses_request_to_chat"
	DirectionResponsesResponseToChat = "responses_response_to_chat"
	DirectionChatResponseToResponses = "chat_response_to_responses"
)

// ProtocolConversionError 是跨协议转换的显式错误边界（全局约束 §1.3）。
// 未来自各 adaptor 的不可无损映射必须归入此类型，禁止记日志后静默继续。
type ProtocolConversionError struct {
	Code      string
	Direction string
	Path      string
	Cause     error
}

// Error 输出稳定机器码 + 方向 + 源协议 JSON path，Cause 存在时追加其文本。
func (e *ProtocolConversionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := "protocol conversion failed: code=" + e.Code + " direction=" + e.Direction + " path=" + e.Path
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap 暴露底层原因，使 errors.Is/errors.As 可继续追溯。
func (e *ProtocolConversionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
