package ctxkey

import "github.com/gin-gonic/gin"

const (
	Config            = "config"
	Id                = "id"
	Username          = "username"
	Role              = "role"
	Status            = "status"
	Channel           = "channel"
	ChannelId         = "channel_id"
	SpecificChannelId = "specific_channel_id"
	RequestModel      = "request_model"
	ConvertedRequest  = "converted_request"
	OriginalModel     = "original_model"
	Group             = "group"
	AffinityScope     = "affinity_scope"
	ModelMapping      = "model_mapping"
	ChannelName       = "channel_name"
	TokenId           = "token_id"
	TokenName         = "token_name"
	BaseURL           = "base_url"
	AvailableModels   = "available_models"
	KeyRequestBody    = "key_request_body"
	SystemPrompt      = "system_prompt"
	SuggestedModel    = "suggested_model"
	ResponseBody      = "response_body"
	FirstTokenTime    = "first_token_time"
	TokenModelMapping = "token_model_mapping"
	PreConsumedQuota  = "pre_consumed_quota"
	// ActualChannelId 记录请求**实际服务**的渠道 id，仅用于归因（亲和 / 冷却 / 监控）。
	// 与 ChannelId（选路渠道）可能不同：adaptor 层 sticky（chatgptsub.SetupRequestHeader）会强制
	// 改写 meta.ChannelId 并实际打到该渠道，但从不回写 ChannelId。刻意独立成键，避免影响重试剔除
	// （filterLastFailedChannel）与既有日志 / 计费语义。
	ActualChannelId = "actual_channel_id"
	// ActualChannelName 记录请求**实际服务**渠道的名字，与 ActualChannelId 同源、仅用于归因与日志。
	// 刻意不复用 ChannelName：ChannelName 语义是选路渠道名，失败日志若用它会与按实际渠道归因的
	// 渠道 id 自相矛盾（出现「channel #B（A的名字）」），故独立成键，避免影响其它消费方。
	ActualChannelName = "actual_channel_name"
	// ChatStreamIncludeUsage 保存原 Chat 请求 stream_options.include_usage 的布尔策略，
	// 由请求转换写入、响应流阶段只读；禁止用上游 Responses usage 的存在与否反推客户端意图。
	ChatStreamIncludeUsage = "chat_stream_include_usage"
	// KeyRequestBodyMetadata 保存从缓存的原始请求体推导出的结论（well-formedness / model / stream）。
	// 值必须是以具体非指针 ctxkey.RequestBodyMetadata 存储；读取统一走 GetRequestBodyMetadata。
	KeyRequestBodyMetadata = "key_request_body_metadata"
)

// RequestBodyMetadata records conclusions derived from the original cached request body.
//
// 不变式：
//   - WellFormed == false 时，Model == ""、Stream == false、ModelValid == false、StreamValid == false。
//   - WellFormed 仅表示对 ctxkey.KeyRequestBody 中同一份字节的 well-formedness 校验结果，
//     绝不因字段提取成功而推断为 true。
//   - Model 仅当 WellFormed && ModelValid 时才有意义。ModelValid 为 true 当且仅当 model 缺失、
//     为 null、或为字符串；畸形 JSON 或存在但类型不符时为 false。
//   - Stream 同理：仅当 WellFormed && StreamValid 时才有意义。StreamValid 为 true 当且仅当 stream
//     缺失、为 null、或为布尔；消费者仍将非法 stream 值降级为 false。
//   - 缺失与显式 null 均为合法零值（与 encoding/json 一致），不是提取错误；存在但类型不符可区分于
//     缺失/null，且永不产出派生值。
//   - 重复键采用 jsonparser 的确定性 first-wins，与 encoding/json 的 last-wins 不同；
//     每个提取点必须声明该差异并用测试锁定。
type RequestBodyMetadata struct {
	WellFormed  bool
	Model       string
	ModelValid  bool
	Stream      bool
	StreamValid bool
}

// GetRequestBodyMetadata returns only an exact RequestBodyMetadata value; nil context, missing keys, pointers, and all other stored types return ok=false without panicking.
//
// ok=false 始终指示调用方自行提取（self-extract）；ok=true 表示缓存结论权威，包括缓存的 WellFormed=false 结果。
func GetRequestBodyMetadata(c *gin.Context) (metadata RequestBodyMetadata, ok bool) {
	if c == nil {
		return RequestBodyMetadata{}, false
	}
	value, exists := c.Get(KeyRequestBodyMetadata)
	if !exists {
		return RequestBodyMetadata{}, false
	}
	metadata, ok = value.(RequestBodyMetadata)
	if !ok {
		return RequestBodyMetadata{}, false
	}
	return metadata, true
}
