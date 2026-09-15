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
	// ChatStreamIncludeUsage 保存原 Chat 请求 stream_options.include_usage 的布尔策略，
	// 由请求转换写入、响应流阶段只读；禁止用上游 Responses usage 的存在与否反推客户端意图。
	ChatStreamIncludeUsage = "chat_stream_include_usage"
)
