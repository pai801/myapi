package ctxkey

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
)
