package apitype

// EndpointType 定义模型支持的 API 端点类型
type EndpointType string

const (
	// EndpointTypeOpenAI 支持 OpenAI Chat Completions (/v1/chat/completions)
	EndpointTypeOpenAI EndpointType = "openai"
	// EndpointTypeOpenAIResponse 支持 OpenAI Responses (/v1/responses)
	EndpointTypeOpenAIResponse EndpointType = "openai-response"
	// EndpointTypeOpenAIResponseCompact 支持 OpenAI Responses Compact (/v1/responses/compact)
	EndpointTypeOpenAIResponseCompact EndpointType = "openai-response-compact"
)
