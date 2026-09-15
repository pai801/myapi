package chatgptsub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestMain 为包级测试初始化内存 DB：流式失败用例经生产 defer 上报后会异步触发
// CheckAndDisable → model.GetChannelById，无 DB 时 gorm 全局为 nil 会 panic；
// 内存库查无该渠道即优雅返回，与 model 包既有测试做法一致
func TestMain(m *testing.M) {
	common.UsingSQLite = true
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to open in-memory test db: " + err.Error())
	}
	dbmodel.DB = db
	os.Exit(m.Run())
}

func setupGin() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	return c, w
}

func testMeta() *meta.Meta {
	return &meta.Meta{
		ChannelType: 53,
		ChannelId:   1,
		BaseURL:     "https://chatgpt.com",
		APIKey:      "test-session-token",
		Mode:        relaymode.ChatCompletions,
		APIType:     20,
	}
}

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

func ptrFloat(v float64) *float64 { return &v }

// ==================== ConvertRequest — Chat Completions → Responses ====================

// decodeStrict 以 DisallowUnknownFields 级别解码，保证测试样本与转换输出都符合协议文档的 wire 形状（契约 §1.1：
// 禁止为保留旧行为而使用协议非法样本）。
func decodeStrict(t *testing.T, src string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(src))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(v))
}

// strictResponsesRequestWire 枚举 Responses §2 请求体全部合法键（含未使用的可选键），
// 转换结果若混入 Chat 残留键（stop/frequency_penalty 等）会因 unknown field 直接失败。
type strictResponsesRequestWire struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       *string         `json:"instructions"`
	PreviousResponseID *string         `json:"previous_response_id"`
	MaxOutputTokens    *int            `json:"max_output_tokens"`
	Temperature        *float64        `json:"temperature"`
	TopP               *float64        `json:"top_p"`
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	Reasoning          json.RawMessage `json:"reasoning"`
	Stream             *bool           `json:"stream"`
	StreamOptions      json.RawMessage `json:"stream_options"`
	Text               json.RawMessage `json:"text"`
	Modalities         []string        `json:"modalities"`
	Store              *bool           `json:"store"`
	Metadata           json.RawMessage `json:"metadata"`
	User               *string         `json:"user"`
	ServiceTier        *string         `json:"service_tier"`
}

// strictResponsesToolWire 是 Responses §9 function tool 的扁平形状：任何 function 中间层键都会 strict 解码失败。
type strictResponsesToolWire struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description *string         `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// strictResponsesInputItemWire 枚举 §3.1/§3.2/§3.3 message/function_call/function_call_output item 的合法键。
type strictResponsesInputItemWire struct {
	Type      string          `json:"type"`
	ID        *string         `json:"id"`
	Role      *string         `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    *string         `json:"call_id"`
	Name      *string         `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Summary   json.RawMessage `json:"summary"`
	Status    *string         `json:"status"`
	Phase     *string         `json:"phase"`
}

func marshalConverted(t *testing.T, result any) string {
	t.Helper()
	data, err := json.Marshal(result)
	require.NoError(t, err)
	return string(data)
}

func TestConvertRequest_ChatToResponses(t *testing.T) {
	// 消息集自带合法的 call_id 配对（tool 输出必须能对上先前 assistant.tool_calls 的 id），
	// 否则会被 T2 前置校验以 malformed_tool_call 拒绝，无法进入 buildInputItems 断言。
	req := &model.GeneralOpenAIRequest{
		Model: "gpt-4o",
		Messages: []model.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello!"},
			{
				Role:             "assistant",
				Content:          "Hi there!",
				ReasoningContent: "thinking step by step...",
			},
			{
				Role: "assistant",
				ToolCalls: []model.Tool{
					{
						Id:       "call_tool_1",
						Type:     "function",
						Function: model.Function{Name: "get_weather", Arguments: `{}`},
					},
				},
			},
			{Role: "tool", Content: "weather result", ToolCallId: "call_tool_1"},
			{
				Role: "assistant",
				ToolCalls: []model.Tool{
					{
						Id:       "call_func_1",
						Type:     "function",
						Function: model.Function{Name: "get_weather", Arguments: `{"location":"NYC"}`},
					},
				},
			},
		},
		MaxTokens: 1000,
		Tools: []model.Tool{
			{
				Type: "function",
				Function: model.Function{
					Name:        "get_weather",
					Description: "Get current weather",
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"location": map[string]interface{}{"type": "string"},
						},
					},
					Strict: boolPtr(true),
				},
			},
		},
		Temperature:      ptrFloat(0.7),
		TopP:             ptrFloat(1.0),
		Stream:           false,
		ToolChoice:       "auto",
		User:             "user_abc",
		ParallelTooCalls: boolPtr(true),
	}

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	adpt := &Adaptor{}
	result, err := adpt.ConvertRequest(c, relaymode.ChatCompletions, req)
	require.NoError(t, err)
	require.NotNil(t, result)

	respReq, ok := result.(*model.ResponsesRequest)
	require.True(t, ok, "expected *model.ResponsesRequest")

	// 顶层字段验证
	assert.Equal(t, "gpt-4o", respReq.Model)
	assert.Equal(t, 1000, respReq.MaxTokens)
	assert.False(t, respReq.Stream)
	assert.Equal(t, "auto", respReq.ToolChoice)
	assert.Equal(t, "user_abc", respReq.User)
	require.NotNil(t, respReq.ParallelToolCalls)
	assert.True(t, *respReq.ParallelToolCalls)

	// 显式采样值必须出现在序列化结果中（指针语义，finding 10）
	wireStr := marshalConverted(t, result)
	var wireMap map[string]interface{}
	decodeStrict(t, wireStr, &wireMap)
	assert.Equal(t, 0.7, wireMap["temperature"])
	assert.Equal(t, 1.0, wireMap["top_p"])

	// input 数组验证
	// 6 条消息 → 7 个 item: system, user, assistant-reasoning, assistant-message,
	// assistant-function_call, tool(function_call_output), assistant-function_call
	input, ok := respReq.Input.([]interface{})
	require.True(t, ok, "input should be []interface{}")
	require.Len(t, input, 7, "expected 7 input items")

	// item[0]: system 保持 system（§4 DN-1 决议：system 不再降级为 developer，developer 保持 developer）
	item0 := input[0].(map[string]interface{})
	assert.Equal(t, "message", item0["type"])
	assert.Equal(t, "system", item0["role"])
	assert.Equal(t, "You are a helpful assistant.", item0["content"])

	// item[1]: user
	item1 := input[1].(map[string]interface{})
	assert.Equal(t, "message", item1["type"])
	assert.Equal(t, "user", item1["role"])
	assert.Equal(t, "Hello!", item1["content"])

	// item[2]: assistant reasoning (reasoning_content 在 content 前添加)
	item2 := input[2].(map[string]interface{})
	assert.Equal(t, "reasoning", item2["type"])
	summary, ok := item2["summary"].([]interface{})
	require.True(t, ok)
	require.Len(t, summary, 1)
	sumBlock := summary[0].(map[string]interface{})
	assert.Equal(t, "summary_text", sumBlock["type"])
	assert.Equal(t, "thinking step by step...", sumBlock["text"])

	// item[3]: assistant content (message)
	item3 := input[3].(map[string]interface{})
	assert.Equal(t, "message", item3["type"])
	assert.Equal(t, "assistant", item3["role"])
	contentArr, ok := item3["content"].([]interface{})
	require.True(t, ok)
	require.Len(t, contentArr, 1)
	block := contentArr[0].(map[string]interface{})
	assert.Equal(t, "output_text", block["type"])
	assert.Equal(t, "Hi there!", block["text"])

	// item[4]: assistant function_call（call_tool_1，为 item[5] 的 tool 输出提供配对来源）
	item4 := input[4].(map[string]interface{})
	assert.Equal(t, "function_call", item4["type"])
	assert.Equal(t, "call_tool_1", item4["call_id"])
	assert.Equal(t, "get_weather", item4["name"])
	assert.Equal(t, "{}", item4["arguments"])

	// item[5]: tool (function_call_output)
	item5 := input[5].(map[string]interface{})
	assert.Equal(t, "function_call_output", item5["type"])
	assert.Equal(t, "call_tool_1", item5["call_id"])
	assert.Equal(t, "weather result", item5["output"])

	// item[6]: assistant function_call
	item6 := input[6].(map[string]interface{})
	assert.Equal(t, "function_call", item6["type"])
	assert.Equal(t, "call_func_1", item6["call_id"])
	assert.Equal(t, "get_weather", item6["name"])
	assert.Equal(t, `{"location":"NYC"}`, item6["arguments"])

	// tools 验证：Responses §9 扁平形状，不得出现 Chat §5.1 的 function 中间层，strict 保留（finding 1）
	require.Len(t, respReq.RawTools, 1)
	tool0 := respReq.RawTools[0].(map[string]interface{})
	assert.Equal(t, "function", tool0["type"])
	assert.Equal(t, "get_weather", tool0["name"])
	assert.Equal(t, "Get current weather", tool0["description"])
	assert.Equal(t, true, tool0["strict"])
	assert.NotContains(t, tool0, "function")
}

// TestConvertChatToResponsesRequestCanonicalToolsAndFields 锁定 Chat→Responses 请求的
// 全部可映射字段保真（报告二 1/2/9/10/11 的应有行为），样本按 Chat §2/§4/§5 严格解码。
func TestConvertChatToResponsesRequestCanonicalToolsAndFields(t *testing.T) {
	const canonicalChatRequest = `{
  "model": "gpt-5.4-mini",
  "messages": [
    {"role": "user", "content": "Hello!"},
    {"role": "assistant", "tool_calls": [
      {"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}
    ]},
    {"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
  ],
  "store": false,
  "reasoning_effort": "medium",
  "metadata": {"order": "test"},
  "max_tokens": 999,
  "max_completion_tokens": 1234,
  "modalities": ["text"],
  "response_format": {"type": "json_schema", "json_schema": {"name": "weather_schema", "description": "weather output", "schema": {"type": "object", "properties": {"unit": {"type": "string"}}}, "strict": true}},
  "service_tier": "auto",
  "stream": true,
  "stream_options": {"include_usage": true},
  "temperature": 0,
  "top_p": 0,
  "verbosity": "low",
  "tools": [
    {"type": "function", "function": {"name": "get_weather", "description": "Get current weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}, "strict": true}}
  ],
  "tool_choice": {"type": "function", "function": {"name": "get_weather"}},
  "parallel_tool_calls": false,
  "user": "user_abc"
}`

	req := &model.GeneralOpenAIRequest{}
	decodeStrict(t, canonicalChatRequest, req)

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	adpt := &Adaptor{}
	result, err := adpt.ConvertRequest(c, relaymode.ChatCompletions, req)
	require.NoError(t, err)
	require.NotNil(t, result)

	respReq, ok := result.(*model.ResponsesRequest)
	require.True(t, ok, "expected *model.ResponsesRequest")
	assert.Equal(t, "gpt-5.4-mini", respReq.Model)
	assert.Equal(t, 1234, respReq.MaxTokens, "max_completion_tokens 必须优先于 max_tokens（finding 9）")
	assert.True(t, respReq.Stream)
	assert.Equal(t, "user_abc", respReq.User)
	require.NotNil(t, respReq.Store)
	assert.False(t, *respReq.Store, "显式 store:false 必须用指针保留")
	require.NotNil(t, respReq.ParallelToolCalls)
	assert.False(t, *respReq.ParallelToolCalls, "显式 parallel_tool_calls:false 必须用指针保留")

	// 整包按 Responses §2 严格解码：混入任何协议外键都会失败
	var wire strictResponsesRequestWire
	decodeStrict(t, marshalConverted(t, respReq), &wire)

	// 显式零采样值序列化存在（finding 10）
	require.NotNil(t, wire.Temperature)
	assert.Equal(t, 0.0, *wire.Temperature)
	require.NotNil(t, wire.TopP)
	assert.Equal(t, 0.0, *wire.TopP)

	require.NotNil(t, wire.MaxOutputTokens)
	assert.Equal(t, 1234, *wire.MaxOutputTokens)
	require.NotNil(t, wire.ServiceTier)
	assert.Equal(t, "auto", *wire.ServiceTier)
	require.NotNil(t, wire.User)
	assert.Equal(t, "user_abc", *wire.User)
	assert.Equal(t, []string{"text"}, wire.Modalities)
	assert.JSONEq(t, `{"order":"test"}`, string(wire.Metadata))
	assert.JSONEq(t, `{"include_usage":true}`, string(wire.StreamOptions))

	// reasoning_effort -> reasoning.effort（finding 9）
	var reasoning struct {
		Effort string `json:"effort"`
	}
	decodeStrict(t, string(wire.Reasoning), &reasoning)
	assert.Equal(t, "medium", reasoning.Effort)

	// response_format -> text.format：json_schema 从 Chat 嵌套恢复 Responses 扁平；verbosity -> text.verbosity
	var text struct {
		Format    json.RawMessage `json:"format"`
		Verbosity string          `json:"verbosity"`
	}
	decodeStrict(t, string(wire.Text), &text)
	assert.Equal(t, "low", text.Verbosity)
	var format struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description *string         `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Strict      *bool           `json:"strict"`
	}
	decodeStrict(t, string(text.Format), &format)
	assert.Equal(t, "json_schema", format.Type)
	assert.Equal(t, "weather_schema", format.Name)
	require.NotNil(t, format.Description)
	assert.Equal(t, "weather output", *format.Description)
	assert.JSONEq(t, `{"type":"object","properties":{"unit":{"type":"string"}}}`, string(format.Schema))
	require.NotNil(t, format.Strict)
	assert.True(t, *format.Strict)

	// tools[]：扁平 function tool，strict 经 model.Function 保留（finding 1）
	var tools []strictResponsesToolWire
	decodeStrict(t, string(wire.Tools), &tools)
	require.Len(t, tools, 1)
	assert.Equal(t, "function", tools[0].Type)
	assert.Equal(t, "get_weather", tools[0].Name)
	require.NotNil(t, tools[0].Description)
	assert.Equal(t, "Get current weather", *tools[0].Description)
	require.NotNil(t, tools[0].Strict)
	assert.True(t, *tools[0].Strict)
	assert.JSONEq(t, `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`, string(tools[0].Parameters))

	// tool_choice 对象扁平化为 {type,name}，不原样透传 Chat 嵌套形状（finding 2）
	var toolChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	decodeStrict(t, string(wire.ToolChoice), &toolChoice)
	assert.Equal(t, "function", toolChoice.Type)
	assert.Equal(t, "get_weather", toolChoice.Name)

	// input items：§3 严格形状 + call_id 配对保真（基础行为，映射细节归 T3）
	var items []strictResponsesInputItemWire
	decodeStrict(t, string(wire.Input), &items)
	require.Len(t, items, 3)
	assert.Equal(t, "message", items[0].Type)
	require.NotNil(t, items[0].Role)
	assert.Equal(t, "user", *items[0].Role)
	assert.Equal(t, "function_call", items[1].Type)
	require.NotNil(t, items[1].CallID)
	assert.Equal(t, "call_1", *items[1].CallID)
	var argsStr string
	decodeStrict(t, string(items[1].Arguments), &argsStr)
	assert.Equal(t, `{"city":"SF"}`, argsStr)
	assert.Equal(t, "function_call_output", items[2].Type)
	require.NotNil(t, items[2].CallID)
	assert.Equal(t, "call_1", *items[2].CallID)

	// include_usage 原始客户端策略写入 context，供响应阶段（T5）读取
	v, exists := c.Get(ctxkey.ChatStreamIncludeUsage)
	require.True(t, exists, "stream_options.include_usage 策略必须写入 context")
	assert.Equal(t, true, v)
}

// TestConvertChatToResponsesRequestRejectsUnsupportedOrMalformedInput 锁定 T2 前置校验边界：
// 不可映射顶层字段与畸形消息输入必须在转换处显式拒绝（报告二 8/9/10/11/15 + DN-2），
// 返回 *model.ProtocolConversionError（controller 侧转 HTTP 400），且不产出上游请求对象。
func TestConvertChatToResponsesRequestRejectsUnsupportedOrMalformedInput(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		code       string
		pathPrefix string
	}{
		{
			name:       "stop",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"stop":["EOF"]}`,
			code:       model.CodeUnsupportedMapping,
			pathPrefix: "stop",
		},
		{
			name:       "frequency_penalty",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5}`,
			code:       model.CodeUnsupportedMapping,
			pathPrefix: "frequency_penalty",
		},
		{
			name:       "presence_penalty",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"presence_penalty":-0.5}`,
			code:       model.CodeUnsupportedMapping,
			pathPrefix: "presence_penalty",
		},
		{
			name:       "logit_bias",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"logit_bias":{"0":0.5}}`,
			code:       model.CodeUnsupportedMapping,
			pathPrefix: "logit_bias",
		},
		{
			name:       "n_greater_than_one",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"n":2}`,
			code:       model.CodeUnsupportedMapping,
			pathPrefix: "n",
		},
		{
			name:       "legacy_function_role",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"},{"role":"function","name":"get_weather","content":"legacy result"}]}`,
			code:       model.CodeUnsupportedMapping,
			pathPrefix: "messages[1].content",
		},
		{
			name:       "assistant_text_with_refusal",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"x"},{"type":"refusal","refusal":"no"}]}]}`,
			code:       model.CodeInvalidSourceShape,
			pathPrefix: "messages[1].content",
		},
		{
			name:       "empty_tool_call_id",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			code:       model.CodeMalformedToolCall,
			pathPrefix: "messages[1].tool_calls[0].id",
		},
		{
			name:       "duplicate_tool_call_id",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"call_dup","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"call_dup","type":"function","function":{"name":"g","arguments":"{}"}}]}]}`,
			code:       model.CodeMalformedToolCall,
			pathPrefix: "messages[1].tool_calls[1].id",
		},
		{
			name:       "dangling_tool_output",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"call_missing","content":"out"}]}`,
			code:       model.CodeMalformedToolCall,
			pathPrefix: "messages[1].tool_call_id",
		},
		{
			name:       "duplicate_tool_output",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"a"},{"role":"tool","tool_call_id":"call_1","content":"b"}]}`,
			code:       model.CodeMalformedToolCall,
			pathPrefix: "messages[3].tool_call_id",
		},
		// 畸形多模态必填子字段（旧实现在此对缺失 url 强制断言直接 panic，finding 8）
		{
			name:       "image_url_missing_url",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"detail":"low"}}]}]}`,
			code:       model.CodeInvalidSourceShape,
			pathPrefix: "messages[0].content[0].image_url.url",
		},
		{
			name:       "input_audio_missing_format",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"aGVsbG8="}}]}]}`,
			code:       model.CodeInvalidSourceShape,
			pathPrefix: "messages[0].content[0].input_audio.format",
		},
		{
			name:       "file_missing_fields",
			body:       `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.txt"}}]}]}`,
			code:       model.CodeInvalidSourceShape,
			pathPrefix: "messages[0].content[0].file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &model.GeneralOpenAIRequest{}
			decodeStrict(t, tt.body, req)

			c, _ := setupGin()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			adpt := &Adaptor{}
			result, err := adpt.ConvertRequest(c, relaymode.ChatCompletions, req)
			require.Error(t, err)
			var convErr *model.ProtocolConversionError
			require.ErrorAs(t, err, &convErr)
			assert.Equal(t, tt.code, convErr.Code)
			assert.Equal(t, model.DirectionChatRequestToResponses, convErr.Direction)
			assert.Contains(t, convErr.Path, tt.pathPrefix)
			assert.Nil(t, result, "拒绝路径不得产出上游请求对象")
		})
	}

	// 缺省等价值不得误伤：n:1 与 frequency_penalty:0 均视缺省（契约：仅非缺省拒绝）
	t.Run("defaults_pass_through", func(t *testing.T) {
		body := `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"n":1,"frequency_penalty":0,"presence_penalty":0,"stop":null,"logit_bias":{}}`
		req := &model.GeneralOpenAIRequest{}
		decodeStrict(t, body, req)

		c, _ := setupGin()
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

		adpt := &Adaptor{}
		result, err := adpt.ConvertRequest(c, relaymode.ChatCompletions, req)
		require.NoError(t, err)
		require.NotNil(t, result)
	})
}

// ==================== T3: buildInputItems 消息映射 ====================

// strictResponsesInputContentBlockWire 枚举 Responses §3.1/§5 message content block 的合法键，
// 转换输出混入 Chat 残留键（如 type:"text"/"image_url"）会因 unknown field 直接失败。
type strictResponsesInputContentBlockWire struct {
	Type       string          `json:"type"`
	Text       *string         `json:"text"`
	ImageURL   *string         `json:"image_url"`
	InputAudio json.RawMessage `json:"input_audio"`
	File       json.RawMessage `json:"file"`
	Refusal    *string         `json:"refusal"`
}

func decodeInputItems(t *testing.T, items []any) []strictResponsesInputItemWire {
	t.Helper()
	var wire []strictResponsesInputItemWire
	decodeStrict(t, marshalConverted(t, items), &wire)
	return wire
}

// TestBuildInputItemsPreservesRolesModalitiesAndRefusal 锁定 T3 合法映射路径（报告二 12/13/14/15）：
// §4 DN-1 system/developer 区分、user 4 种模态保真、assistant refusal 双形态、并行 tool call 的
// call_id 配对与 arguments JSON string 保真。样本按 Chat §3/§4 严格 schema 解码。
func TestBuildInputItemsPreservesRolesModalitiesAndRefusal(t *testing.T) {
	const chatMessages = `[
  {"role":"system","content":"sys prompt"},
  {"role":"developer","content":"dev prompt"},
  {"role":"user","content":[
    {"type":"text","text":"look at this"},
    {"type":"image_url","image_url":{"url":"https://img.example/1.png"}},
    {"type":"input_audio","input_audio":{"data":"aGVsbG8=","format":"wav"}},
    {"type":"file","file":{"filename":"a.txt","file_data":"data:text/plain;base64,AAA="}}
  ]},
  {"role":"assistant","content":[{"type":"text","text":"sure"}]},
  {"role":"assistant","content":[{"type":"refusal","refusal":"part refusal"}]},
  {"role":"assistant","content":null,"refusal":"field refusal"},
  {"role":"assistant","tool_calls":[
    {"id":"call_a","type":"function","function":{"name":"f1","arguments":"{\"x\":1}"}},
    {"id":"call_b","type":"function","function":{"name":"f2","arguments":"{}"}}
  ]},
  {"role":"tool","tool_call_id":"call_a","content":"outA"},
  {"role":"tool","tool_call_id":"call_b","content":"outB"}
]`
	var msgs []model.Message
	decodeStrict(t, chatMessages, &msgs)

	items, err := buildInputItems(msgs)
	require.NoError(t, err)

	wire := decodeInputItems(t, items)
	require.Len(t, wire, 10, "9 条消息 → 10 个 item（并行 tool_calls 展开为 2 个 function_call）")

	// DN-1：system 保留 system，developer 保持 developer，不得互相升降级
	require.NotNil(t, wire[0].Role)
	assert.Equal(t, "message", wire[0].Type)
	assert.Equal(t, "system", *wire[0].Role)
	var sysContent string
	decodeStrict(t, string(wire[0].Content), &sysContent)
	assert.Equal(t, "sys prompt", sysContent)

	require.NotNil(t, wire[1].Role)
	assert.Equal(t, "developer", *wire[1].Role)

	// user 多模态：text/image_url/input_audio/file → input_text/input_image/input_audio/input_file，
	// 无静默丢失（报告二 13）
	assert.Equal(t, "message", wire[2].Type)
	var userBlocks []strictResponsesInputContentBlockWire
	decodeStrict(t, string(wire[2].Content), &userBlocks)
	require.Len(t, userBlocks, 4)

	assert.Equal(t, "input_text", userBlocks[0].Type)
	require.NotNil(t, userBlocks[0].Text)
	assert.Equal(t, "look at this", *userBlocks[0].Text)

	assert.Equal(t, "input_image", userBlocks[1].Type)
	require.NotNil(t, userBlocks[1].ImageURL)
	assert.Equal(t, "https://img.example/1.png", *userBlocks[1].ImageURL)

	assert.Equal(t, "input_audio", userBlocks[2].Type)
	assert.JSONEq(t, `{"data":"aGVsbG8=","format":"wav"}`, string(userBlocks[2].InputAudio))

	assert.Equal(t, "input_file", userBlocks[3].Type)
	assert.JSONEq(t, `{"filename":"a.txt","file_data":"data:text/plain;base64,AAA="}`, string(userBlocks[3].File))

	// assistant text → output_text（保持既有 §1.2 保护行为）
	var assistantTextBlocks []strictResponsesInputContentBlockWire
	decodeStrict(t, string(wire[3].Content), &assistantTextBlocks)
	require.Len(t, assistantTextBlocks, 1)
	assert.Equal(t, "output_text", assistantTextBlocks[0].Type)
	require.NotNil(t, assistantTextBlocks[0].Text)
	assert.Equal(t, "sure", *assistantTextBlocks[0].Text)

	// assistant refusal content part → Responses refusal part（报告二 14）
	var partRefusalBlocks []strictResponsesInputContentBlockWire
	decodeStrict(t, string(wire[4].Content), &partRefusalBlocks)
	require.Len(t, partRefusalBlocks, 1)
	assert.Equal(t, "refusal", partRefusalBlocks[0].Type)
	require.NotNil(t, partRefusalBlocks[0].Refusal)
	assert.Equal(t, "part refusal", *partRefusalBlocks[0].Refusal)

	// assistant 顶层 refusal 字段 → Responses refusal part（报告二 14）
	var fieldRefusalBlocks []strictResponsesInputContentBlockWire
	decodeStrict(t, string(wire[5].Content), &fieldRefusalBlocks)
	require.Len(t, fieldRefusalBlocks, 1)
	assert.Equal(t, "refusal", fieldRefusalBlocks[0].Type)
	require.NotNil(t, fieldRefusalBlocks[0].Refusal)
	assert.Equal(t, "field refusal", *fieldRefusalBlocks[0].Refusal)

	// 合法并行工具调用：call_id 配对输出，arguments 保持 JSON string（报告二 15）
	assert.Equal(t, "function_call", wire[6].Type)
	require.NotNil(t, wire[6].CallID)
	assert.Equal(t, "call_a", *wire[6].CallID)
	require.NotNil(t, wire[6].Name)
	assert.Equal(t, "f1", *wire[6].Name)
	var argsA string
	decodeStrict(t, string(wire[6].Arguments), &argsA)
	assert.Equal(t, `{"x":1}`, argsA)

	assert.Equal(t, "function_call", wire[7].Type)
	require.NotNil(t, wire[7].CallID)
	assert.Equal(t, "call_b", *wire[7].CallID)

	assert.Equal(t, "function_call_output", wire[8].Type)
	require.NotNil(t, wire[8].CallID)
	assert.Equal(t, "call_a", *wire[8].CallID)
	var outA string
	decodeStrict(t, string(wire[8].Output), &outA)
	assert.Equal(t, "outA", outA)

	assert.Equal(t, "function_call_output", wire[9].Type)
	require.NotNil(t, wire[9].CallID)
	assert.Equal(t, "call_b", *wire[9].CallID)
}

// TestBuildInputItemsParsesContentDefensively 锁定 parser/build 的防御边界（报告二 8 的 parser 侧 +
// T2 advisory-3/4）：绕过 T2 前置校验的直接调用，任何畸形输入返回 error、不 panic、不产出 item；
// 已验证的合法多模态输入保真通过。
func TestBuildInputItemsParsesContentDefensively(t *testing.T) {
	// 合法基线：T2 校验通过的多模态消息在此保真（拒绝路径已由 T2 锁定，不双写）
	t.Run("valid_multimodal_no_error", func(t *testing.T) {
		var msgs []model.Message
		decodeStrict(t, `[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"https://x/1.png"}}
		]}]`, &msgs)
		items, err := buildInputItems(msgs)
		require.NoError(t, err)
		require.Len(t, items, 1)
	})

	// 绕过 T2 的畸形输入：必须 error 且不得 panic（旧实现在此裸断言 panic，报告二 8）
	cases := []struct {
		name       string
		msgs       []model.Message
		wantCode   string
		wantPathIn string
	}{
		{
			name: "malformed_image_url_missing_url",
			msgs: []model.Message{{Role: "user", Content: []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"detail": "low"}},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name: "malformed_image_url_wrong_type",
			msgs: []model.Message{{Role: "user", Content: []any{
				map[string]any{"type": "image_url", "image_url": "not-an-object"},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name:       "content_part_not_object",
			msgs:       []model.Message{{Role: "user", Content: []any{"bare string element"}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name: "unknown_content_part_type",
			msgs: []model.Message{{Role: "user", Content: []any{
				map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://x"}},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name:       "empty_content_array",
			msgs:       []model.Message{{Role: "user", Content: []any{}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name:       "user_content_missing",
			msgs:       []model.Message{{Role: "user"}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name: "role_part_matrix_user_refusal",
			msgs: []model.Message{{Role: "user", Content: []any{
				map[string]any{"type": "refusal", "refusal": "no"},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name: "role_part_matrix_system_image",
			msgs: []model.Message{{Role: "system", Content: []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/1.png"}},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			name: "role_part_matrix_tool_audio",
			msgs: []model.Message{{Role: "tool", ToolCallId: "call_1", Content: []any{
				map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "QQ==", "format": "wav"}},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].content",
		},
		{
			// T2 advisory-3：function.name 空值不在 T2 前置校验范围，不得静默产出无 name 的 function_call item
			name: "tool_call_missing_function_name",
			msgs: []model.Message{{Role: "assistant", ToolCalls: []model.Tool{
				{Id: "call_1", Type: "function", Function: model.Function{Arguments: "{}"}},
			}}},
			wantCode:   model.CodeMalformedToolCall,
			wantPathIn: "messages[0].tool_calls[0].function.name",
		},
		{
			name: "tool_call_arguments_unmarshalable",
			msgs: []model.Message{{Role: "assistant", ToolCalls: []model.Tool{
				{Id: "call_1", Type: "function", Function: model.Function{Name: "f", Arguments: make(chan int)}},
			}}},
			wantCode:   model.CodeMalformedToolCall,
			wantPathIn: "messages[0].tool_calls[0].function.arguments",
		},
		{
			name:       "unknown_role",
			msgs:       []model.Message{{Role: "wizard", Content: "hi"}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].role",
		},
		{
			// T3 reviewer P2：顶层 refusal × content refusal part 并存时旧实现追加出
			// 两个 refusal part 且无 error，违反 Chat §4 assistant 仅 text 或恰一个 refusal
			name: "assistant_refusal_field_with_refusal_part",
			msgs: []model.Message{{Role: "assistant", Refusal: strPtr("field refusal"), Content: []any{
				map[string]any{"type": "refusal", "refusal": "part refusal"},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].refusal",
		},
		{
			// 同一互斥边界覆盖 text part 并存情形
			name: "assistant_refusal_field_with_text_part",
			msgs: []model.Message{{Role: "assistant", Refusal: strPtr("field refusal"), Content: []any{
				map[string]any{"type": "text", "text": "hi"},
			}}},
			wantCode:   model.CodeInvalidSourceShape,
			wantPathIn: "messages[0].refusal",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var items []any
			var err error
			require.NotPanics(t, func() { items, err = buildInputItems(tt.msgs) }, "defensive build must not panic")
			require.Error(t, err)
			var convErr *model.ProtocolConversionError
			require.ErrorAs(t, err, &convErr)
			assert.Equal(t, tt.wantCode, convErr.Code)
			assert.Equal(t, model.DirectionChatRequestToResponses, convErr.Direction)
			assert.Contains(t, convErr.Path, tt.wantPathIn)
			assert.Nil(t, items, "失败路径不得产出部分 input items")
		})
	}
}

// TestBuildInputItemsLegacyFunctionRoleDecision 锁定 §4 DN-2 决议在 buildInputItems
// 被直接调用（绕过 T2 前置校验）时的行为：非空 role:"function" 显式 unsupported_mapping
// 错误而非静默删除；空 content function 消息与 T2 判定一致跳过且不产 item。
func TestBuildInputItemsLegacyFunctionRoleDecision(t *testing.T) {
	t.Run("nonempty_function_rejected_not_dropped", func(t *testing.T) {
		var msgs []model.Message
		decodeStrict(t, `[
			{"role":"user","content":"hi"},
			{"role":"function","name":"get_weather","content":"legacy result"}
		]`, &msgs)

		items, err := buildInputItems(msgs)
		require.Error(t, err)
		var convErr *model.ProtocolConversionError
		require.ErrorAs(t, err, &convErr)
		assert.Equal(t, model.CodeUnsupportedMapping, convErr.Code)
		assert.Equal(t, model.DirectionChatRequestToResponses, convErr.Direction)
		assert.Contains(t, convErr.Path, "messages[1].content")
		assert.Nil(t, items, "拒绝路径不得产出部分 input items")
	})

	t.Run("function_with_payload_rejected", func(t *testing.T) {
		// content 为空但携带 tool_calls 的 function 消息同样不得静默丢弃载荷
		msgs := []model.Message{{Role: "function", Name: strPtr("f"), ToolCalls: []model.Tool{
			{Id: "call_1", Type: "function", Function: model.Function{Name: "f", Arguments: "{}"}},
		}}}
		items, err := buildInputItems(msgs)
		require.Error(t, err)
		var convErr *model.ProtocolConversionError
		require.ErrorAs(t, err, &convErr)
		assert.Equal(t, model.CodeUnsupportedMapping, convErr.Code)
		assert.Nil(t, items)
	})

	t.Run("empty_content_function_skipped", func(t *testing.T) {
		// 与 T2 前置校验同一判定：空 content function 跳过，其余消息正常映射，不产 function item
		var msgs []model.Message
		decodeStrict(t, `[
			{"role":"function","name":"get_weather","content":""},
			{"role":"user","content":"real question"}
		]`, &msgs)

		items, err := buildInputItems(msgs)
		require.NoError(t, err)
		wire := decodeInputItems(t, items)
		require.Len(t, wire, 1, "空 function 消息不产 item，其余消息照常映射")
		require.NotNil(t, wire[0].Role)
		assert.Equal(t, "user", *wire[0].Role)
	})
}

// ==================== ConvertRequest — Responses 透传 ====================

func TestConvertRequest_ResponsesPassthrough(t *testing.T) {
	rawBody := `{"model":"gpt-4o","input":"hello world","max_output_tokens":100}`

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(rawBody))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.KeyRequestBody, []byte(rawBody))

	adpt := &Adaptor{}
	result, err := adpt.ConvertRequest(c, relaymode.Responses, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	bodyMap, ok := result.(map[string]interface{})
	require.True(t, ok, "expected map[string]interface{} for Responses passthrough")
	assert.Equal(t, "gpt-4o", bodyMap["model"])
	assert.Equal(t, "hello world", bodyMap["input"])
	assert.Equal(t, float64(100), bodyMap["max_output_tokens"])
}

// ==================== SetupRequestHeader 白名单过滤 ====================

func TestSetupRequestHeader_Whitelist(t *testing.T) {
	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	upstreamReq := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	// 白名单头（注意 conversation_id/session_id 使用下划线，与 allowedHeaders map 匹配）
	upstreamReq.Header.Set("Accept-Language", "en-US")
	upstreamReq.Header.Set("User-Agent", "test-agent")
	upstreamReq.Header.Set("Conversation_id", "conv-123")
	upstreamReq.Header.Set("Session_id", "sess-456")
	upstreamReq.Header.Set("X-Codex-Turn-State", "state-value")
	upstreamReq.Header.Set("X-Codex-Turn-Metadata", "meta-value")
	upstreamReq.Header.Set("Originator", "test-origin")
	// 非白名单头
	upstreamReq.Header.Set("X-Custom-Header", "should-be-filtered")
	upstreamReq.Header.Set("Sensitive-Header", "should-be-filtered")

	metaVal := testMeta()

	adpt := &Adaptor{}
	err := adpt.SetupRequestHeader(c, upstreamReq, metaVal)
	require.NoError(t, err)

	// 白名单头应保留
	assert.Equal(t, "en-US", upstreamReq.Header.Get("Accept-Language"))
	assert.Equal(t, "test-agent", upstreamReq.Header.Get("User-Agent"))
	assert.Equal(t, "conv-123", upstreamReq.Header.Get("Conversation_id"))
	assert.Equal(t, "sess-456", upstreamReq.Header.Get("Session_id"))
	assert.Equal(t, "state-value", upstreamReq.Header.Get("X-Codex-Turn-State"))
	assert.Equal(t, "meta-value", upstreamReq.Header.Get("X-Codex-Turn-Metadata"))
	assert.Equal(t, "test-origin", upstreamReq.Header.Get("Originator"))

	// 非白名单头应被过滤
	assert.Equal(t, "", upstreamReq.Header.Get("X-Custom-Header"))
	assert.Equal(t, "", upstreamReq.Header.Get("Sensitive-Header"))

	// 固定头被自动设置
	assert.Equal(t, "Bearer test-session-token", upstreamReq.Header.Get("Authorization"))
	assert.Equal(t, "application/json", upstreamReq.Header.Get("Content-Type"))
}

// ==================== 非流式 Chat Completions 响应 ====================

// strictChatResponseWire 枚举 Chat §6 CreateChatCompletionResponse 的必填键集，
// 转换输出缺 id/object/created/model/choices 或混入未知键都会 strict 解码失败（报告二 25/16）。
// choice.logprobs 与 message.content/refusal 用 json.RawMessage：键缺失时解码结果为 nil，
// 断言 "null" 即同时锁定「必含且为 null」语义。
type strictChatResponseWire struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []strictChatChoiceWire `json:"choices"`
	Usage   strictChatUsageWire    `json:"usage"`
}

type strictChatChoiceWire struct {
	Index        int                   `json:"index"`
	Message      strictChatMessageWire `json:"message"`
	FinishReason string                `json:"finish_reason"`
	Logprobs     json.RawMessage       `json:"logprobs"`
}

type strictChatMessageWire struct {
	Role             string                   `json:"role"`
	Content          json.RawMessage          `json:"content"`
	Refusal          json.RawMessage          `json:"refusal"`
	ReasoningContent *string                  `json:"reasoning_content"`
	ToolCalls        []strictChatToolCallWire `json:"tool_calls"`
}

type strictChatToolCallWire struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function *model.Function `json:"function"`
	Custom   *struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"custom"`
}

type strictChatUsageWire struct {
	PromptTokens            int                          `json:"prompt_tokens"`
	CompletionTokens        int                          `json:"completion_tokens"`
	TotalTokens             int                          `json:"total_tokens"`
	PromptTokensDetails     *strictPromptDetailsWire     `json:"prompt_tokens_details"`
	CompletionTokensDetails *strictCompletionDetailsWire `json:"completion_tokens_details"`
}

type strictPromptDetailsWire struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type strictCompletionDetailsWire struct {
	ReasoningTokens          int `json:"reasoning_tokens"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
	AudioTokens              int `json:"audio_tokens"`
	TextTokens               int `json:"text_tokens"`
}

func TestNonStreamDoResponse_Chat(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	responsesBody := `{
		"id": "resp_test_123",
		"object": "response",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"id": "msg_1",
				"role": "assistant",
				"status": "completed",
				"content": [{"type": "output_text", "text": "Hello, world!", "annotations": [], "logprobs": []}]
			}
		],
		"status": "completed",
		"usage": {
			"input_tokens": 10,
			"input_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 0},
			"output_tokens": 20,
			"output_tokens_details": {"reasoning_tokens": 0},
			"total_tokens": 30
		},
		"created_at": 1234567890
	}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responsesBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode, "expected no error for successful response")
	require.NotNil(t, usage, "expected usage from completed response")

	// 验证 usage
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	assert.Equal(t, 30, usage.TotalTokens)

	// 转换输出按 Chat §6 严格 schema 解码（报告二 25：合法 Chat wire，而非原样透传）
	var wire strictChatResponseWire
	decodeStrict(t, w.Body.String(), &wire)
	assert.Equal(t, "chat.completion", wire.Object)
	assert.Contains(t, wire.ID, "chatcmpl-")
	assert.Equal(t, "gpt-4o", wire.Model)
	// 报告二 26：created_at → created
	assert.Equal(t, int64(1234567890), wire.Created)

	require.Len(t, wire.Choices, 1)
	assert.Equal(t, "stop", wire.Choices[0].FinishReason)
	// 报告二 16：choice 必含 logprobs:null；message 必含 refusal:null
	assert.Equal(t, "null", string(wire.Choices[0].Logprobs))
	assert.Equal(t, "null", string(wire.Choices[0].Message.Refusal))
	assert.JSONEq(t, `"Hello, world!"`, string(wire.Choices[0].Message.Content))

	assert.Equal(t, 10, wire.Usage.PromptTokens)
	assert.Equal(t, 20, wire.Usage.CompletionTokens)
	assert.Equal(t, 30, wire.Usage.TotalTokens)
}

func TestNonStreamDoResponse_ChatWithReasoningAndToolCalls(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	responsesBody := `{
		"id": "resp_reasoning_tool",
		"object": "response",
		"model": "gpt-4o",
		"output": [
			{
				"type": "reasoning",
				"id": "rs_1",
				"status": "completed",
				"summary": [{"type": "summary_text", "text": "I need to think about this..."}],
				"content": [{"type": "reasoning_text", "text": "raw reasoning"}]
			},
			{
				"type": "message",
				"id": "msg_1",
				"role": "assistant",
				"status": "completed",
				"content": [{"type": "output_text", "text": "Let me check the weather.", "annotations": [], "logprobs": []}]
			},
			{
				"type": "function_call",
				"id": "fc_1",
				"call_id": "call_abc",
				"name": "get_weather",
				"arguments": "{\"location\":\"NYC\"}",
				"status": "completed"
			}
		],
		"status": "completed",
		"usage": {
			"input_tokens": 5,
			"input_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 0},
			"output_tokens": 15,
			"output_tokens_details": {"reasoning_tokens": 3},
			"total_tokens": 20
		},
		"created_at": 1234567890
	}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responsesBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	assert.Equal(t, 5, usage.PromptTokens)
	assert.Equal(t, 15, usage.CompletionTokens)
	assert.Equal(t, 20, usage.TotalTokens)
	// 报告二 20：output reasoning_tokens 进入内部 completion details
	require.NotNil(t, usage.CompletionTokensDetails)
	assert.Equal(t, 3, usage.CompletionTokensDetails.ReasoningTokens)

	var wire strictChatResponseWire
	decodeStrict(t, w.Body.String(), &wire)
	msg := wire.Choices[0].Message

	assert.JSONEq(t, `"Let me check the weather."`, string(msg.Content))
	// 报告二 17：reasoning 的 summary[] 与 content[reasoning_text][] 都聚合进 reasoning_content
	require.NotNil(t, msg.ReasoningContent)
	assert.Equal(t, "I need to think about this...raw reasoning", *msg.ReasoningContent)
	// 报告二 5：completed + tool calls → finish_reason=tool_calls，不再是 stop
	assert.Equal(t, "tool_calls", wire.Choices[0].FinishReason)
	assert.Equal(t, "null", string(msg.Refusal))

	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "call_abc", msg.ToolCalls[0].ID)
	assert.Equal(t, "function", msg.ToolCalls[0].Type)
	require.NotNil(t, msg.ToolCalls[0].Function)
	assert.Equal(t, "get_weather", msg.ToolCalls[0].Function.Name)
	// 报告二 3：arguments 是 JSON 字符串原文，禁止二次编码（外层不再带引号转义）
	assert.Equal(t, `{"location":"NYC"}`, msg.ToolCalls[0].Function.Arguments)
}

func TestNonStreamDoResponse_ChatWithCachedTokens(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	responsesBody := `{
		"id": "resp_cached",
		"object": "response",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"id": "msg_1",
				"role": "assistant",
				"status": "completed",
				"content": [{"type": "output_text", "text": "Cached response", "annotations": [], "logprobs": []}]
			}
		],
		"status": "completed",
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"total_tokens": 15,
			"input_tokens_details": {"cached_tokens": 8, "cache_write_tokens": 2},
			"output_tokens_details": {"reasoning_tokens": 1}
		},
		"created_at": 1234567890
	}`

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(responsesBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 5, usage.CompletionTokens)
	assert.Equal(t, 15, usage.TotalTokens)

	// 报告二 20：cached/cache_write 与 reasoning details 全量到达内部 usage（计费公式不变）
	require.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 2, usage.PromptTokensDetails.CacheWriteTokens)
	require.NotNil(t, usage.CompletionTokensDetails)
	assert.Equal(t, 1, usage.CompletionTokensDetails.ReasoningTokens)

	var wire strictChatResponseWire
	decodeStrict(t, w.Body.String(), &wire)
	require.NotNil(t, wire.Usage.PromptTokensDetails)
	assert.Equal(t, 8, wire.Usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 2, wire.Usage.PromptTokensDetails.CacheWriteTokens)
	require.NotNil(t, wire.Usage.CompletionTokensDetails)
	assert.Equal(t, 1, wire.Usage.CompletionTokensDetails.ReasoningTokens)
}

func TestNonStreamDoResponse_ChatErrorMapping(t *testing.T) {
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := testMeta()
	m.ActualModelName = "gpt-4o"

	errBody := `{"error":{"code":"rate_limit_exceeded","message":"Rate limit hit","type":"rate_limit_error"}}`

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(errBody)),
	}

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.Nil(t, usage)
	require.Nil(t, errWithCode)

	var errResp map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &errResp)
	require.NoError(t, err)

	errObj, ok := errResp["error"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "Rate limit hit", errObj["message"])
	assert.Equal(t, "rate_limit_exceeded", errObj["code"])
	assert.Equal(t, "rate_limit_error", errObj["type"])
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// ==================== stop 缺省判定（T2 P2-1） ====================

// TestIsDefaultStopSequences 表驱动锁定 stop 缺省判定（契约 §1.2 禁回归清单的姊妹行为）：
// P2-1 修复点——Go 侧直接构造的 []string（不经 JSON 解码，如渠道测试代码/内部拼装请求）
// 空切片是缺省语义，不得因未覆盖分支被误判非缺省而 400。
func TestIsDefaultStopSequences(t *testing.T) {
	tests := []struct {
		name string
		stop any
		want bool
	}{
		{"nil_default", nil, true},
		{"empty_string_default", "", true},
		{"nonempty_string", "EOF", false},
		{"empty_any_array_default", []any{}, true},
		{"nonempty_any_array", []any{"EOF"}, false},
		{"empty_go_string_slice_default", []string{}, true},
		{"nonempty_go_string_slice", []string{"EOF"}, false},
		{"unsupported_type_not_default", 42, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isDefaultStopSequences(tt.stop))
		})
	}

	// 端到端：空 []string 不得被 validateUnsupportedChatRequestFields 以 unsupported_mapping 拒绝
	t.Run("empty_go_string_slice_passes_validation", func(t *testing.T) {
		require.NoError(t, validateUnsupportedChatRequestFields(&model.GeneralOpenAIRequest{
			Model: "gpt-5.4-mini",
			Stop:  []string{},
			Messages: []model.Message{
				{Role: "user", Content: "hi"},
			},
		}))
	})
}

// ==================== response_format text/json_object 形状（T2 P2-2） ====================

// TestConvertChatResponseFormatTextAndJsonObjectWireShapes 补齐 text/json_object 两分支的 wire 断言，
// 与 canonical 测试的 json_schema 分支共同锁定 §1.2 保护项「text.format 三种基本形状」。
func TestConvertChatResponseFormatTextAndJsonObjectWireShapes(t *testing.T) {
	tests := []struct {
		name         string
		responseJSON string
		wantFormat   string
	}{
		{
			name:         "text",
			responseJSON: `{"type":"text"}`,
			wantFormat:   `{"type":"text"}`,
		},
		{
			name:         "json_object",
			responseJSON: `{"type":"json_object"}`,
			wantFormat:   `{"type":"json_object"}`,
		},
		{
			name:         "json_schema_flat",
			responseJSON: `{"type":"json_schema","json_schema":{"name":"n1","schema":{"type":"object"},"strict":true}}`,
			wantFormat:   `{"type":"json_schema","name":"n1","schema":{"type":"object"},"strict":true}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}],"response_format":` + tt.responseJSON + `}`
			req := &model.GeneralOpenAIRequest{}
			decodeStrict(t, body, req)

			respReq, err := convertChatToResponsesRequest(req)
			require.NoError(t, err)
			require.NotNil(t, respReq.Text)

			var text struct {
				Format json.RawMessage `json:"format"`
			}
			decodeStrict(t, marshalConverted(t, respReq.Text), &text)
			assert.JSONEq(t, tt.wantFormat, string(text.Format))
		})
	}
}

// ==================== toolCallArgumentsString 单元测试 ====================

func TestToolCallArgumentsString(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{"nil", nil, "{}"},
		{"string", `{"a":1}`, `{"a":1}`},
		{"bytes", []byte(`{"b":2}`), `{"b":2}`},
		{"map", map[string]interface{}{"c": 3}, `{"c":3}`},
		{"int", 42, `42`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toolCallArgumentsString(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}

	// 不可 JSON 编码的 arguments 必须显式失败，旧实现静默回退 "{}"（swallow）
	t.Run("unmarshalable_returns_error", func(t *testing.T) {
		got, err := toolCallArgumentsString(make(chan int))
		require.Error(t, err)
		assert.Equal(t, "", got)
	})
}

// ==================== extractUsageFromResponses 单元测试 ====================

func TestExtractUsageFromResponses(t *testing.T) {
	// details 对象缺失 = 畸形/兼容来源（T1 允许解析为 nil），不得被合成为零值细节
	body := `{
		"id":"resp_usage_test",
		"object":"response",
		"model":"gpt-4o",
		"output":[],
		"status":"completed",
		"created_at":1700000000,
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}
	}`
	usage := extractUsageFromResponses([]byte(body))
	require.NotNil(t, usage)
	assert.Equal(t, 1, usage.PromptTokens)
	assert.Equal(t, 2, usage.CompletionTokens)
	assert.Equal(t, 3, usage.TotalTokens)
	assert.Nil(t, usage.PromptTokensDetails)
	assert.Nil(t, usage.CompletionTokensDetails)
}

func TestExtractUsageFromResponses_WithCached(t *testing.T) {
	// 报告二 20：cached/cache_write/reasoning details 全量到达内部 usage
	body := `{
		"id":"resp_cached_usage",
		"object":"response",
		"model":"gpt-4o",
		"output":[],
		"status":"completed",
		"created_at":1700000000,
		"usage":{
			"input_tokens":10,"output_tokens":5,"total_tokens":15,
			"input_tokens_details":{"cached_tokens":7,"cache_write_tokens":3},
			"output_tokens_details":{"reasoning_tokens":4,"accepted_prediction_tokens":1,"rejected_prediction_tokens":2,"audio_tokens":0,"text_tokens":1}
		}
	}`
	usage := extractUsageFromResponses([]byte(body))
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 5, usage.CompletionTokens)
	assert.Equal(t, 15, usage.TotalTokens)
	require.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 7, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 3, usage.PromptTokensDetails.CacheWriteTokens)
	require.NotNil(t, usage.CompletionTokensDetails)
	assert.Equal(t, 4, usage.CompletionTokensDetails.ReasoningTokens)
	assert.Equal(t, 1, usage.CompletionTokensDetails.AcceptedPredictionTokens)
	assert.Equal(t, 2, usage.CompletionTokensDetails.RejectedPredictionTokens)
	assert.Equal(t, 1, usage.CompletionTokensDetails.TextTokens)
}

func TestExtractUsageFromResponses_InvalidJSON(t *testing.T) {
	usage := extractUsageFromResponses([]byte("not json"))
	assert.Nil(t, usage)
}

// ==================== mapFinishReason 单元测试 ====================

// TestMapFinishReason 锁定报告二 5 的完整映射契约（Responses §4，Chat §6.2/§10）。
// 旧断言 failed/unknown/"" → stop 属协议违约伪装，已按契约重写为显式 error。
func TestMapFinishReason(t *testing.T) {
	tests := []struct {
		name             string
		status           string
		incompleteReason string
		hasToolCalls     bool
		want             string
		wantCode         string // 非空表示期望 *model.ProtocolConversionError 的 Code
	}{
		{name: "completed_no_tools", status: "completed", want: "stop"},
		{name: "completed_with_tools", status: "completed", hasToolCalls: true, want: "tool_calls"},
		{name: "incomplete_max_output_tokens", status: "incomplete", incompleteReason: "max_output_tokens", want: "length"},
		{name: "incomplete_content_filter", status: "incomplete", incompleteReason: "content_filter", want: "content_filter"},
		{name: "incomplete_other_reason", status: "incomplete", incompleteReason: "steered", wantCode: model.CodeUnsupportedMapping},
		{name: "incomplete_missing_reason", status: "incomplete", wantCode: model.CodeUnsupportedMapping},
		{name: "failed_not_faked", status: "failed", wantCode: model.CodeUnsupportedMapping},
		{name: "cancelled_not_faked", status: "cancelled", wantCode: model.CodeUnsupportedMapping},
		{name: "queued_not_faked", status: "queued", wantCode: model.CodeUnsupportedMapping},
		{name: "in_progress_not_faked", status: "in_progress", wantCode: model.CodeUnsupportedMapping},
		{name: "unknown_status_shape", status: "bogus", wantCode: model.CodeInvalidSourceShape},
		{name: "empty_status_shape", status: "", wantCode: model.CodeInvalidSourceShape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapFinishReason(tt.status, tt.incompleteReason, tt.hasToolCalls)
			if tt.wantCode != "" {
				require.Error(t, err, "unmappable status must not fake a finish reason")
				var convErr *model.ProtocolConversionError
				require.ErrorAs(t, err, &convErr)
				assert.Equal(t, tt.wantCode, convErr.Code)
				assert.Equal(t, model.DirectionResponsesResponseToChat, convErr.Direction)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ==================== extractSessionHash 单元测试 ====================

func TestExtractSessionHash(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("conversation_id优先", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set("conversation_id", "conv-001")
		c.Request.Header.Set("session_id", "sess-001")
		assert.Equal(t, "conv-001", extractSessionHash(c))
	})

	t.Run("仅session_id", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set("session_id", "sess-002")
		assert.Equal(t, "sess-002", extractSessionHash(c))
	})

	t.Run("无任何标识", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		assert.Equal(t, "", extractSessionHash(c))
	})
}

// ==================== mapResponsesErrorToChatFormat 单元测试 ====================

func TestMapResponsesErrorToChatFormat(t *testing.T) {
	errBody := `{"error":{"code":"server_error","message":"Internal error","type":"server_error"}}`
	mapped := mapResponsesErrorToChatFormat([]byte(errBody))
	require.NotNil(t, mapped)

	var errResp map[string]interface{}
	err := json.Unmarshal(mapped, &errResp)
	require.NoError(t, err)

	errObj := errResp["error"].(map[string]interface{})
	assert.Equal(t, "Internal error", errObj["message"])
	assert.Equal(t, "server_error", errObj["code"])
	assert.Equal(t, "server_error", errObj["type"])
}

func TestMapResponsesErrorToChatFormat_NoError(t *testing.T) {
	normalBody := `{"id":"resp_123","status":"completed"}`
	mapped := mapResponsesErrorToChatFormat([]byte(normalBody))
	assert.Equal(t, normalBody, string(mapped), "non-error body should be returned as-is")
}

func TestMapResponsesErrorToChatFormat_EmptyType(t *testing.T) {
	// 注意：现有代码中 chatErr["type"] 检查的是顶层 map 的 type key，
	// 而 type 实际在 chatErr["error"] 嵌套 map 中，因此空 type 不会被默认填充。
	// 此测试验证当前的实际行为。
	errBody := `{"error":{"code":"rate_limit","message":"too many requests"}}`
	mapped := mapResponsesErrorToChatFormat([]byte(errBody))
	require.NotNil(t, mapped)

	// 验证空 type 不会被自动填充为 invalid_request_error（现有代码行为）
	var errResp map[string]interface{}
	err := json.Unmarshal(mapped, &errResp)
	require.NoError(t, err)
	// 修复后，空 type 应被填充为 invalid_request_error
	assert.Equal(t, "invalid_request_error", errResp["error"].(map[string]interface{})["type"])
}

// ==================== convertResponsesToChat 集成验证 ====================

func TestConvertResponsesToChat_Basic(t *testing.T) {
	responsesBody := `{
		"id": "resp_conv_test",
		"object": "response",
		"model": "gpt-4o",
		"output": [
			{
				"type": "message",
				"id": "msg_1",
				"role": "assistant",
				"status": "completed",
				"content": [{"type": "output_text", "text": "Hello!", "annotations": [], "logprobs": []}]
			}
		],
		"status": "completed",
		"usage": {
			"input_tokens": 3,
			"input_tokens_details": {"cached_tokens": 0, "cache_write_tokens": 0},
			"output_tokens": 5,
			"output_tokens_details": {"reasoning_tokens": 0},
			"total_tokens": 8
		},
		"created_at": 999999
	}`

	chatBytes, err := convertResponsesToChat([]byte(responsesBody), "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, chatBytes)

	var chatResp map[string]interface{}
	err = json.Unmarshal(chatBytes, &chatResp)
	require.NoError(t, err)

	assert.Equal(t, "chatcmpl-resp_conv_test", chatResp["id"])
	assert.Equal(t, "chat.completion", chatResp["object"])
	assert.Equal(t, "gpt-4o", chatResp["model"])
	assert.Equal(t, float64(999999), chatResp["created"])
}

// ==================== T4: 非流式 Responses → Chat 契约测试 ====================

// canonicalResponsesBody 是一条协议合法（Responses §4/§5/§6）的完整非流式响应：
// created_at、多 message item、reasoning summary+content、function/custom call、全量 usage details。
const canonicalResponsesBody = `{
	"id": "resp_canon",
	"object": "response",
	"created_at": 1710000000,
	"model": "gpt-5.4",
	"status": "completed",
	"output": [
		{"type": "reasoning", "id": "rs_1", "status": "completed",
			"summary": [{"type": "summary_text", "text": "S1"}],
			"content": [{"type": "reasoning_text", "text": "C1"}, {"type": "reasoning_text", "text": "C2"}]},
		{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
			"content": [{"type": "output_text", "text": "part one ", "annotations": [], "logprobs": []}]},
		{"type": "message", "id": "msg_2", "role": "assistant", "status": "completed",
			"content": [{"type": "output_text", "text": "part two", "annotations": [], "logprobs": []}]},
		{"type": "function_call", "id": "fc_1", "call_id": "call_fn", "name": "get_weather",
			"arguments": "{\"location\":\"NYC\"}", "status": "completed"},
		{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_ctc", "name": "freeform",
			"input": "raw <<delta>>", "status": "completed"}
	],
	"usage": {
		"input_tokens": 100,
		"input_tokens_details": {"cached_tokens": 20, "cache_write_tokens": 5},
		"output_tokens": 50,
		"output_tokens_details": {"reasoning_tokens": 10, "accepted_prediction_tokens": 2,
			"rejected_prediction_tokens": 1, "audio_tokens": 3, "text_tokens": 30},
		"total_tokens": 150
	}
}`

// wrapResponsesOutput 用最小合法 §4 信封包裹给定的 output 数组与 status/incomplete_details。
func wrapResponsesOutput(status string, incompleteDetails string, outputJSON string) string {
	body := `{"id":"resp_wrap","object":"response","created_at":1700000000,"model":"gpt-5.4","status":` + status
	if incompleteDetails != "" {
		body += `,` + incompleteDetails
	}
	body += `,"output":` + outputJSON +
		`,"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},` +
		`"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}`
	return body
}

// requireConversionError 断言 convertResponsesToChat 的失败路径产出契约 §1.3 稳定机器码。
func requireConversionError(t *testing.T, body string, wantCode string) {
	t.Helper()
	out, err := convertResponsesToChat([]byte(body), "fallback-model")
	require.Error(t, err)
	var convErr *model.ProtocolConversionError
	require.ErrorAs(t, err, &convErr)
	assert.Equal(t, wantCode, convErr.Code)
	assert.Equal(t, model.DirectionResponsesResponseToChat, convErr.Direction)
	assert.Empty(t, out, "失败路径不得产出部分转换结果")
}

// TestConvertResponsesToChatCanonicalNonStream 锁定合法非流式响应的完整 Chat 输出
// （报告二 3/5/7/16/17/18/20/25/26/27）：必填键与 null、聚合顺序、arguments 原文、
// finish_reason、全量 usage details；并逐项锁定 refusal 互斥与不可映射输出边界。
func TestConvertResponsesToChatCanonicalNonStream(t *testing.T) {
	// 样本本身必须能由共享 Responses wire model 严格解码（契约 §1.1）
	decodeStrict(t, canonicalResponsesBody, &model.ResponsesResponse{})

	out, err := convertResponsesToChat([]byte(canonicalResponsesBody), "fallback-model")
	require.NoError(t, err)

	var wire strictChatResponseWire
	decodeStrict(t, string(out), &wire)

	// Chat §6 必填 + 报告二 26/27：created_at→created，model 优先上游实际值
	assert.Equal(t, "chatcmpl-resp_canon", wire.ID)
	assert.Equal(t, "chat.completion", wire.Object)
	assert.Equal(t, int64(1710000000), wire.Created)
	assert.Equal(t, "gpt-5.4", wire.Model)

	require.Len(t, wire.Choices, 1)
	choice := wire.Choices[0]
	// 报告二 16：choice 必含 logprobs:null；message 必含 refusal:null
	assert.Equal(t, "null", string(choice.Logprobs))
	assert.Equal(t, "null", string(choice.Message.Refusal))
	// 报告二 5：completed + tool calls → tool_calls
	assert.Equal(t, "tool_calls", choice.FinishReason)

	// 报告二 7：多 message 按 output 顺序聚合，不互相覆盖
	assert.JSONEq(t, `"part one part two"`, string(choice.Message.Content))
	// 报告二 17：reasoning summary[] 与 content[reasoning_text][] 按 item/content 顺序聚合
	require.NotNil(t, choice.Message.ReasoningContent)
	assert.Equal(t, "S1C1C2", *choice.Message.ReasoningContent)

	// 工具调用保序：function 在前、custom 在后
	require.Len(t, choice.Message.ToolCalls, 2)
	fn := choice.Message.ToolCalls[0]
	assert.Equal(t, "call_fn", fn.ID)
	assert.Equal(t, "function", fn.Type)
	require.NotNil(t, fn.Function)
	assert.Equal(t, "get_weather", fn.Function.Name)
	// 报告二 3：arguments 解码为原 JSON 字符串，无二次编码
	assert.Equal(t, `{"location":"NYC"}`, fn.Function.Arguments)

	// §4 DN-3（报告二 18）：custom tool call 走 Chat §6.1.1 custom 变体，不伪装 function
	custom := choice.Message.ToolCalls[1]
	assert.Equal(t, "call_ctc", custom.ID)
	assert.Equal(t, "custom", custom.Type)
	assert.Nil(t, custom.Function, "custom call 不得输出 function 载荷")
	require.NotNil(t, custom.Custom)
	assert.Equal(t, "freeform", custom.Custom.Name)
	assert.Equal(t, "raw <<delta>>", custom.Custom.Input)

	// 报告二 20：usage 全量 details 到达 Chat §8 输出
	assert.Equal(t, 100, wire.Usage.PromptTokens)
	assert.Equal(t, 50, wire.Usage.CompletionTokens)
	assert.Equal(t, 150, wire.Usage.TotalTokens)
	require.NotNil(t, wire.Usage.PromptTokensDetails)
	assert.Equal(t, 20, wire.Usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 5, wire.Usage.PromptTokensDetails.CacheWriteTokens)
	require.NotNil(t, wire.Usage.CompletionTokensDetails)
	assert.Equal(t, 10, wire.Usage.CompletionTokensDetails.ReasoningTokens)
	assert.Equal(t, 2, wire.Usage.CompletionTokensDetails.AcceptedPredictionTokens)
	assert.Equal(t, 1, wire.Usage.CompletionTokensDetails.RejectedPredictionTokens)
	assert.Equal(t, 3, wire.Usage.CompletionTokensDetails.AudioTokens)
	assert.Equal(t, 30, wire.Usage.CompletionTokensDetails.TextTokens)

	t.Run("pure_tool_call_content_null", func(t *testing.T) {
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"function_call","id":"fc_1","call_id":"call_x","name":"f","arguments":"{}","status":"completed"}]`)
		out, err := convertResponsesToChat([]byte(body), "fb")
		require.NoError(t, err)
		var w2 strictChatResponseWire
		decodeStrict(t, string(out), &w2)
		assert.Equal(t, "null", string(w2.Choices[0].Message.Content), "纯工具调用 content 必须为 null（chat §6.1）")
		assert.Equal(t, "tool_calls", w2.Choices[0].FinishReason)
	})

	t.Run("pure_refusal_keeps_content_null", func(t *testing.T) {
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"message","id":"msg_1","role":"assistant","status":"completed",
			   "content":[{"type":"refusal","refusal":"I can't help with that"}]}]`)
		out, err := convertResponsesToChat([]byte(body), "fb")
		require.NoError(t, err)
		var w2 strictChatResponseWire
		decodeStrict(t, string(out), &w2)
		assert.Equal(t, "null", string(w2.Choices[0].Message.Content))
		assert.JSONEq(t, `"I can't help with that"`, string(w2.Choices[0].Message.Refusal))
		assert.Equal(t, "stop", w2.Choices[0].FinishReason)
	})

	t.Run("refusal_with_text_is_explicit_error", func(t *testing.T) {
		// 报告二 7：文本与 refusal 同时不可表达时显式错误，不覆盖
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[
				{"type":"output_text","text":"hi","annotations":[],"logprobs":[]},
				{"type":"refusal","refusal":"no"}]}]`)
		requireConversionError(t, body, model.CodeInvalidSourceShape)
	})

	t.Run("multiple_refusals_are_explicit_error", func(t *testing.T) {
		// Chat §6.1 message.refusal 只能承载恰一条拒绝
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[
				{"type":"refusal","refusal":"r1"}]},
			  {"type":"message","id":"msg_2","role":"assistant","status":"completed","content":[
				{"type":"refusal","refusal":"r2"}]}]`)
		requireConversionError(t, body, model.CodeInvalidSourceShape)
	})

	t.Run("unsupported_output_item_rejects_whole_response", func(t *testing.T) {
		// §4 DN-4（报告二 19）：不可映射 item 使整体失败，不得忽略后返回成功
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"image_generation_call","id":"ig_1","status":"completed","result":"Zm9v"}]`)
		requireConversionError(t, body, model.CodeUnsupportedOutputItem)
	})

	t.Run("malformed_arguments_rejected_not_swallowed", func(t *testing.T) {
		// 报告二 3：arguments 缺失/非法是畸形工具调用，不得静默回退 "{}"
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"function_call","id":"fc_1","call_id":"call_x","name":"f","status":"completed"}]`)
		requireConversionError(t, body, model.CodeMalformedToolCall)
	})

	t.Run("malformed_json_is_explicit_error", func(t *testing.T) {
		requireConversionError(t, `{"id": "resp_x", "object": "response", "status": "compl`, model.CodeInvalidSourceJSON)
	})

	t.Run("missing_required_top_fields", func(t *testing.T) {
		requireConversionError(t,
			`{"object":"response","status":"completed","created_at":1,"model":"m","output":[]}`,
			model.CodeInvalidSourceShape)
		requireConversionError(t,
			`{"id":"resp_1","status":"completed","created_at":1,"model":"m","output":[]}`,
			model.CodeInvalidSourceShape)
	})

	t.Run("failed_status_not_faked_as_stop", func(t *testing.T) {
		// §1.2 保护项延伸：failed 不得被合成为任何成功 finish
		body := wrapResponsesOutput(`"failed"`, ``, `[]`)
		requireConversionError(t, body, model.CodeUnsupportedMapping)
	})

	t.Run("model_fallback_only_when_upstream_empty", func(t *testing.T) {
		// 报告二 27：fallback 仅在上游 model 为空时使用
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[]}]`)
		body = strings.Replace(body, `"model":"gpt-5.4"`, `"model":""`, 1)
		out, err := convertResponsesToChat([]byte(body), "fallback-model")
		require.NoError(t, err)
		var w2 strictChatResponseWire
		decodeStrict(t, string(out), &w2)
		assert.Equal(t, "fallback-model", w2.Model)
	})
}

// TestConvertResponsesToChatMapsIncompleteReasons 按契约三例直验 mapFinishReason，
// 并端到端锁定 incomplete body 的 Chat 输出（报告二 5；§1.2：incomplete 不得被合成为 completed）。
func TestConvertResponsesToChatMapsIncompleteReasons(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		reason   string
		tools    bool
		expected string
	}{
		{"incomplete_max_output_tokens", "incomplete", "max_output_tokens", false, "length"},
		{"incomplete_content_filter", "incomplete", "content_filter", false, "content_filter"},
		{"completed_tool_call", "completed", "", true, "tool_calls"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mapFinishReason(tt.status, tt.reason, tt.tools)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}

	// 端到端：incomplete(max_output_tokens) body → Chat finish_reason=length，而非 stop
	body := wrapResponsesOutput(`"incomplete"`, `"incomplete_details":{"reason":"max_output_tokens"}`,
		`[{"type":"message","id":"msg_1","role":"assistant","status":"incomplete","content":[
			{"type":"output_text","text":"truncated","annotations":[],"logprobs":[]}]}]`)
	out, err := convertResponsesToChat([]byte(body), "fb")
	require.NoError(t, err)
	var w2 strictChatResponseWire
	decodeStrict(t, string(out), &w2)
	assert.Equal(t, "length", w2.Choices[0].FinishReason)
}

// TestHandleChatCompletionsResponseRejectsMalformedOrUnsupportedUpstream 锁定 handler 的
// 上游错误边界（报告二 25，契约 §1.3）：畸形 JSON 或 unsupported output item → HTTP 502
// upstream_error/invalid_upstream_response；不写原 body、不设置成功 capture、返回零 usage。
func TestHandleChatCompletionsResponseRejectsMalformedOrUnsupportedUpstream(t *testing.T) {
	runHandler := func(t *testing.T, body string) (*httptest.ResponseRecorder, *gin.Context, *model.Usage, *model.ErrorWithStatusCode) {
		t.Helper()
		c, w := setupGin()
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		m := testMeta()
		m.ActualModelName = "gpt-4o"
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		usage, errWithCode := handleChatCompletionsResponse(c, resp, m)
		return w, c, usage, errWithCode
	}

	assertRejected := func(t *testing.T, w *httptest.ResponseRecorder, c *gin.Context, usage *model.Usage, errWithCode *model.ErrorWithStatusCode, wantMsgFragment string) {
		t.Helper()
		require.Nil(t, usage, "拒绝路径不得返回计费 usage")
		require.NotNil(t, errWithCode)
		assert.Equal(t, http.StatusBadGateway, errWithCode.StatusCode)
		assert.Equal(t, "upstream_error", errWithCode.Error.Type)
		assert.Equal(t, "invalid_upstream_response", errWithCode.Error.Code)
		assert.Contains(t, errWithCode.Error.Message, wantMsgFragment)
		assert.Empty(t, w.Body.String(), "502 路径不得写回任何上游原 body")
		_, captured := c.Get(ctxkey.ResponseBody)
		assert.False(t, captured, "502 路径不得设置成功 capture")
	}

	t.Run("malformed_json", func(t *testing.T) {
		w, c, usage, errWithCode := runHandler(t, `{"id": "resp_x", "object": "response", "broken`)
		assertRejected(t, w, c, usage, errWithCode, model.CodeInvalidSourceJSON)
	})

	t.Run("unsupported_output_item", func(t *testing.T) {
		body := wrapResponsesOutput(`"completed"`, ``,
			`[{"type":"mcp_call","server_label":"s","name":"n","arguments":"{}","status":"completed"}]`)
		w, c, usage, errWithCode := runHandler(t, body)
		assertRejected(t, w, c, usage, errWithCode, model.CodeUnsupportedOutputItem)
	})

	t.Run("valid_response_still_succeeds", func(t *testing.T) {
		// 负向对照：合法上游不得被误拦，成功 capture 语义不变
		w, c, usage, errWithCode := runHandler(t, canonicalResponsesBody)
		require.Nil(t, errWithCode)
		require.NotNil(t, usage)
		assert.NotEmpty(t, w.Body.String())
		captured, ok := c.Get(ctxkey.ResponseBody)
		require.True(t, ok)
		assert.Contains(t, captured, "chat.completion")
	})
}

// ==================== SSE 流式 Chat Completions（Responses §7 真实事件 → Chat §7） ====================
//
// T5 测试基准（报告二 4/5/6/20/21/22/23/24/Advisory-28）：
//   - 上游样本全部按 Responses §7 事件全集构造：公共元数据挂在 response.created 的完整 response 对象上
//     （id/model/created_at），delta 事件使用真实事件名（response.output_text.delta /
//     response.reasoning_text.delta / response.reasoning_summary_text.delta / response.refusal.delta /
//     response.function_call_arguments.delta / response.custom_tool_call_input.delta）。
//   - Responses 流本身没有 [DONE] 哨兵：正常与异常路径都以 completed/incomplete/failed/error/cancelled
//     终态收尾；上游自发送 [DONE] 属于协议违例（负向锁死子用例）。
//   - 虚构事件名 response.reasoning.delta 只出现在负向子用例，断言其被当作未知事件显式终止。

// sseEvt 一条上游 SSE 事件：event: 行名（空则省略）+ data: 行载荷。
type sseEvt struct {
	event string
	data  string
}

func responsesSSE(events ...sseEvt) string {
	var b strings.Builder
	for _, e := range events {
		if e.event != "" {
			b.WriteString("event: ")
			b.WriteString(e.event)
			b.WriteString("\n")
		}
		b.WriteString("data: ")
		b.WriteString(e.data)
		b.WriteString("\n\n")
	}
	return b.String()
}

const (
	// 公共元数据唯一合法来源：response.created.response（Responses §7）。
	createdDataFmt = `{"type":"response.created","sequence_number":0,"response":{"id":"%s","object":"response","created_at":1725000000,"status":"in_progress","model":"gpt-5-codex","output":[]}}`

	// completed 终态携带完整 usage details（Responses §6，T1 wire）。
	completedUsageJSON = `{"input_tokens":11,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3,"accepted_prediction_tokens":1,"text_tokens":4},"total_tokens":18}`
	completedDataFmt   = `{"type":"response.completed","sequence_number":99,"response":{"id":"%s","object":"response","created_at":1725000000,"status":"completed","model":"gpt-5-codex","output":%s,"usage":%s,"error":null,"incomplete_details":null}}`

	incompleteDataFmt = `{"type":"response.incomplete","sequence_number":98,"response":{"id":"%s","object":"response","created_at":1725000000,"status":"incomplete","model":"gpt-5-codex","output":[],"usage":%s,"error":null,"incomplete_details":{"reason":"%s"}}}`

	cancelledDataFmt = `{"type":"response.cancelled","sequence_number":97,"response":{"id":"%s","object":"response","created_at":1725000000,"status":"cancelled","model":"gpt-5-codex","output":[],"usage":%s,"error":null,"incomplete_details":null}}`

	// function arguments 在 wire 上是 JSON 字符串（Responses §3.2）；added 的 "{}" 是上游空占位
	// （报告二 4：合法 "{}" 不得被当作非空初值发出）。
	toolAddedDataFmt    = `{"type":"response.output_item.added","sequence_number":10,"output_index":1,"item":{"id":"%s","type":"function_call","call_id":"%s","name":"%s","arguments":"{}","status":"in_progress"}}`
	toolArgsDeltaFmt    = `{"type":"response.function_call_arguments.delta","sequence_number":11,"item_id":"%s","output_index":1,"delta":%s}`
	toolArgsDoneFmt     = `{"type":"response.function_call_arguments.done","sequence_number":12,"item_id":"%s","output_index":1,"arguments":%s}`
	toolItemDoneFmt     = `{"type":"response.output_item.done","sequence_number":13,"output_index":1,"item":{"id":"%s","type":"function_call","call_id":"%s","name":"%s","arguments":%s,"status":"completed"}}`
	emptyOutputArray    = `[]`
	messageItemAdded    = `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`
	contentPartAdded    = `{"type":"response.content_part.added","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`
	textDeltaFmt        = `{"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"delta":%s}`
	textDoneEvt         = `{"type":"response.output_text.done","sequence_number":5,"item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello World"}`
	contentPartDoneEvt  = `{"type":"response.content_part.done","sequence_number":6,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello World","annotations":[]}}`
	messageItemDoneEvt  = `{"type":"response.output_item.done","sequence_number":7,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello World","annotations":[]}]}}`
	refusalPartAddedFmt = `{"type":"response.content_part.added","sequence_number":20,"item_id":"msg_1","output_index":0,"content_index":1,"part":{"type":"refusal","refusal":""}}`
)

func evCreated(respID string) sseEvt {
	return sseEvt{event: "response.created", data: fmt.Sprintf(createdDataFmt, respID)}
}

func evCompleted(respID, outputJSON string) sseEvt {
	return sseEvt{event: "response.completed", data: fmt.Sprintf(completedDataFmt, respID, outputJSON, completedUsageJSON)}
}

// ---------- Chat chunk 协议走查（Chat §7.1/§7.2/§2.4） ----------

var (
	// Chat §7.1 合法顶层键 + 网关错误 chunk 的 error 成员。
	chatChunkTopKeys = map[string]bool{
		"id": true, "object": true, "created": true, "model": true,
		"choices": true, "usage": true, "error": true,
		"system_fingerprint": true, "service_tier": true, "obfuscation": true, "moderation": true,
	}
	chatChoiceKeys = map[string]bool{"index": true, "delta": true, "finish_reason": true, "logprobs": true}
	// reasoning_content 是网关扩展键（T4 wire 同规则），其余为 Chat §7.2 delta 字段。
	chatDeltaKeys             = map[string]bool{"role": true, "content": true, "refusal": true, "reasoning_content": true, "tool_calls": true}
	chatToolCallFragmentKeys  = map[string]bool{"index": true, "id": true, "type": true, "function": true, "custom": true}
	chatFunctionFragmentKeys  = map[string]bool{"name": true, "arguments": true}
	chatCustomFragmentKeys    = map[string]bool{"name": true, "input": true}
	chatUsageChunkAllowedKeys = map[string]bool{"prompt_tokens": true, "completion_tokens": true, "total_tokens": true, "prompt_tokens_details": true, "completion_tokens_details": true}
	chatPromptDetailsKeys     = map[string]bool{"cached_tokens": true, "cache_write_tokens": true, "audio_tokens": true, "text_tokens": true, "image_tokens": true}
	chatCompletionDetailsKeys = map[string]bool{"reasoning_tokens": true, "accepted_prediction_tokens": true, "rejected_prediction_tokens": true, "audio_tokens": true, "text_tokens": true}
)

func toolCallEntries(delta map[string]any) []map[string]any {
	arr, _ := delta["tool_calls"].([]any)
	var out []map[string]any
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// parseChatSSEChunks 走查网关 SSE 输出：每条 data 行必须是 Chat §7.1 合法 chunk
// （id/model/created 必填非空、object 固定、choices 存在、usage 键恒在、无未知键），
// 并单独统计 [DONE] 出现次数。
func parseChatSSEChunks(t *testing.T, body string) ([]map[string]any, int) {
	t.Helper()
	var chunks []map[string]any
	doneCount := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			doneCount++
			continue
		}
		var chunk map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(payload), &chunk), "gateway emitted non-JSON chunk: %s", payload)
		for k := range chunk {
			assert.Truef(t, chatChunkTopKeys[k], "unknown chunk top key %q (chat §7.1)", k)
		}
		id, ok := chunk["id"].(string)
		require.Truef(t, ok && id != "", "chat chunk requires non-empty id (chat §7.1): %s", payload)
		require.Equal(t, "chat.completion.chunk", chunk["object"])
		created, ok := chunk["created"].(float64)
		require.Truef(t, ok && created > 0, "chat chunk requires non-zero created (chat §7.1): %s", payload)
		modelName, ok := chunk["model"].(string)
		require.Truef(t, ok && modelName != "", "chat chunk requires non-empty model (chat §7.1): %s", payload)
		choices, ok := chunk["choices"].([]any)
		require.Truef(t, ok, "chat chunk requires choices array (chat §7.1): %s", payload)
		_, hasUsage := chunk["usage"]
		require.Truef(t, hasUsage, "usage member must be present (null or object, chat §2.4): %s", payload)
		for _, ch := range choices {
			cm, ok := ch.(map[string]any)
			require.True(t, ok)
			for k := range cm {
				assert.Truef(t, chatChoiceKeys[k], "unknown choice key %q (chat §7.2)", k)
			}
			dm, ok := cm["delta"].(map[string]any)
			require.Truef(t, ok, "choice.delta required (chat §7.2)")
			for k := range dm {
				assert.Truef(t, chatDeltaKeys[k], "unknown delta key %q (chat §7.2)", k)
			}
			for _, frag := range toolCallEntries(dm) {
				for k := range frag {
					assert.Truef(t, chatToolCallFragmentKeys[k], "unknown tool_call fragment key %q (chat §7.2)", k)
				}
				if fn, ok := frag["function"].(map[string]any); ok {
					for k := range fn {
						assert.Truef(t, chatFunctionFragmentKeys[k], "unknown function fragment key %q", k)
					}
				}
				if cu, ok := frag["custom"].(map[string]any); ok {
					for k := range cu {
						assert.Truef(t, chatCustomFragmentKeys[k], "unknown custom fragment key %q", k)
					}
				}
			}
		}
		chunks = append(chunks, chunk)
	}
	return chunks, doneCount
}

// requireChatChunkOrderMetadata 锁定 Chat §7.1：全流 chunk 共用同一非空 id/model/created。
func requireUniformChatChunkMetadata(t *testing.T, chunks []map[string]any, wantID, wantModel string, wantCreated float64) {
	t.Helper()
	require.NotEmpty(t, chunks)
	for i, ch := range chunks {
		assert.Equalf(t, wantID, ch["id"], "chunk[%d] id", i)
		assert.Equalf(t, wantModel, ch["model"], "chunk[%d] model", i)
		assert.InDeltaf(t, wantCreated, ch["created"], 0.001, "chunk[%d] created", i)
	}
}

func firstDeltaOf(chunk map[string]any) map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	cm, _ := choices[0].(map[string]any)
	dm, _ := cm["delta"].(map[string]any)
	return dm
}

func finishReasonOf(chunk map[string]any) any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	cm, _ := choices[0].(map[string]any)
	return cm["finish_reason"]
}

func chunkHasError(chunk map[string]any) (map[string]any, bool) {
	e, ok := chunk["error"].(map[string]any)
	return e, ok
}

func toolFragmentsByIndex(chunks []map[string]any) map[int][]map[string]any {
	out := map[int][]map[string]any{}
	for _, ch := range chunks {
		choices, _ := ch["choices"].([]any)
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			dm, _ := cm["delta"].(map[string]any)
			if dm == nil {
				continue
			}
			for _, frag := range toolCallEntries(dm) {
				idx, _ := frag["index"].(float64)
				out[int(idx)] = append(out[int(idx)], frag)
			}
		}
	}
	return out
}

// concatFragmentField 拼接某 tool call 各分片中 function.arguments 或 custom.input。
func concatFragmentField(frags []map[string]any, objectKey, field string) string {
	var b strings.Builder
	for _, f := range frags {
		obj, _ := f[objectKey].(map[string]any)
		if obj == nil {
			continue
		}
		if s, ok := obj[field].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func countNamedFragments(frags []map[string]any, objectKey, field string) int {
	n := 0
	for _, f := range frags {
		obj, _ := f[objectKey].(map[string]any)
		if obj == nil {
			continue
		}
		if s, ok := obj[field].(string); ok && s != "" {
			n++
		}
	}
	return n
}

// runSSEViaDoResponse 以适配器完整 DoResponse 路径驱动流转换（覆盖 TestSSEStream_* 回归）。
func runSSEViaDoResponse(t *testing.T, sse string) (*gin.Context, *httptest.ResponseRecorder, *model.Usage, *model.ErrorWithStatusCode) {
	t.Helper()
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}
	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o-sub"
	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	return c, w, usage, errWithCode
}

// chatStreamFixture 直接以契约入口 streamChatFromResponses 驱动（T5 契约 W 侧）。
func chatStreamFixture(t *testing.T, sse string, includeUsage *bool) (*gin.Context, *httptest.ResponseRecorder, *model.Usage, *model.ErrorWithStatusCode) {
	t.Helper()
	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if includeUsage != nil {
		c.Set(ctxkey.ChatStreamIncludeUsage, *includeUsage)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}
	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o-sub"
	usage, errWithCode := streamChatFromResponses(c, resp, m)
	return c, w, usage, errWithCode
}

// assertChatStreamErrorTermination 断言：流以错误 chunk + 单一 [DONE] 终止，
// 无 finish_reason=stop/tool_calls/length 假成功，函数回传失败 ErrorWithStatusCode。
func assertChatStreamErrorTermination(t *testing.T, sse string, wantChunkCode string, wantErrCode string) {
	t.Helper()
	_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
	require.NotNil(t, errWithCode, "错误/畸形终止必须回传 ErrorWithStatusCode 供渠道统计与预扣费回滚")
	assert.Nil(t, usage)
	assert.Equal(t, http.StatusBadGateway, errWithCode.StatusCode, "转换错误回传统计侧必须为 502（契约 §1.3）")
	if wantErrCode != "" {
		assert.Equal(t, wantErrCode, errWithCode.Error.Code)
	}
	body := w.Body.String()
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount, "错误后仍按 Chat 合法方式以单一 [DONE] 收尾")
	require.NotEmpty(t, chunks)
	errObj, ok := chunkHasError(chunks[len(chunks)-1])
	require.True(t, ok, "最后一条 chunk 必须是错误 chunk")
	if wantChunkCode != "" {
		assert.Equal(t, wantChunkCode, errObj["code"])
	}
	assert.NotContains(t, body, `"finish_reason":"stop"`)
	assert.NotContains(t, body, `"finish_reason":"tool_calls"`)
	assert.NotContains(t, body, `"finish_reason":"length"`)
}

func TestSSEStream_ChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_stream_1"),
		sseEvt{event: "response.in_progress", data: `{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_stream_1","object":"response","created_at":1725000000,"status":"in_progress","model":"gpt-5-codex","output":[]}}`},
		sseEvt{event: "response.output_item.added", data: messageItemAdded},
		sseEvt{event: "response.content_part.added", data: contentPartAdded},
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"Hello "`)},
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"World"`)},
		sseEvt{event: "response.output_text.done", data: textDoneEvt},
		sseEvt{event: "response.content_part.done", data: contentPartDoneEvt},
		sseEvt{event: "response.output_item.done", data: messageItemDoneEvt},
		evCompleted("resp_stream_1", `[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello World","annotations":[]}]}]`),
	)

	_, w, usage, errWithCode := runSSEViaDoResponse(t, sse)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)
	assert.Equal(t, 11, usage.PromptTokens)
	assert.Equal(t, 7, usage.CompletionTokens)
	assert.Equal(t, 18, usage.TotalTokens)
	// 报告二 20：流式 usage 必须保留 details（不再只有三个总数字）
	require.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 4, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 2, usage.PromptTokensDetails.CacheWriteTokens)
	require.NotNil(t, usage.CompletionTokensDetails)
	assert.Equal(t, 3, usage.CompletionTokensDetails.ReasoningTokens)

	chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
	assert.Equal(t, 1, doneCount, "网关输出 [DONE] 恰一次")
	requireUniformChatChunkMetadata(t, chunks, "chatcmpl-resp_stream_1", "gpt-5-codex", 1725000000)

	// 报告二 6：首个 choice chunk 必须携带 delta.role:"assistant"，且全流只出现一次
	require.NotEmpty(t, chunks)
	require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])
	roleChunks := 0
	for _, ch := range chunks {
		if d := firstDeltaOf(ch); d != nil && d["role"] != nil {
			roleChunks++
		}
	}
	assert.Equal(t, 1, roleChunks)

	var text strings.Builder
	for _, ch := range chunks {
		if d := firstDeltaOf(ch); d != nil {
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
		}
	}
	assert.Equal(t, "Hello World", text.String())

	last := chunks[len(chunks)-1]
	assert.Equal(t, "stop", finishReasonOf(last))

	// 报告二 21：include_usage 未请求（context 未写入策略）时不得出现独立 usage chunk
	body := w.Body.String()
	assert.NotContains(t, body, "prompt_tokens")
}

func TestSSEStream_ChatCompletions_WithToolCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_tool_stream"),
		sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_1", "call_weather_1", "get_weather")},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_1", `"{\"city\":"`)},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_1", `"\"Paris\"}"`)},
		sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_1", `"{\"city\":\"Paris\"}"`)},
		sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_1", "call_weather_1", "get_weather", `"{\"city\":\"Paris\"}"`)},
		evCompleted("resp_tool_stream", `[{"id":"fc_1","type":"function_call","call_id":"call_weather_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`),
	)

	_, w, usage, errWithCode := runSSEViaDoResponse(t, sse)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
	assert.Equal(t, 1, doneCount)
	requireUniformChatChunkMetadata(t, chunks, "chatcmpl-resp_tool_stream", "gpt-5-codex", 1725000000)
	require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])

	frags := toolFragmentsByIndex(chunks)[0]
	require.NotEmpty(t, frags)
	// 首分片携带 id+name+type，name 只发一次（避免客户端重复拼接）
	assert.Equal(t, "call_weather_1", frags[0]["id"])
	assert.Equal(t, "function", frags[0]["type"])
	require.Equal(t, 1, countNamedFragments(frags, "function", "name"))
	// 报告二 4：added 的 "{}" 空占位不得成为初值与 delta 重复；拼接结果可 JSON decode
	joined := concatFragmentField(frags, "function", "arguments")
	assert.Equal(t, `{"city":"Paris"}`, joined)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(joined), &decoded))
	assert.Equal(t, "Paris", decoded["city"])

	last := chunks[len(chunks)-1]
	assert.Equal(t, "tool_calls", finishReasonOf(last), "报告二 5：completed+工具调用 → tool_calls，不得固定 stop")
	assert.NotContains(t, w.Body.String(), `"finish_reason":"stop"`)
}

// 报告二 22：只处理真实事件名。合法 reasoning/refusal 事件内容不得丢失。
func TestSSEStream_ChatCompletions_ReasoningRefusalRealEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_rea"),
		sseEvt{event: "response.output_item.added", data: `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[],"content":[]}}`},
		sseEvt{event: "response.reasoning_summary_part.added", data: `{"type":"response.reasoning_summary_part.added","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`},
		sseEvt{event: "response.reasoning_summary_text.delta", data: `{"type":"response.reasoning_summary_text.delta","sequence_number":4,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"summary of thinking"}`},
		sseEvt{event: "response.reasoning_summary_text.done", data: `{"type":"response.reasoning_summary_text.done","sequence_number":5,"item_id":"rs_1","output_index":0,"summary_index":0,"text":"summary of thinking"}`},
		sseEvt{event: "response.reasoning_text.delta", data: `{"type":"response.reasoning_text.delta","sequence_number":6,"item_id":"rs_1","output_index":0,"content_index":0,"delta":"raw reasoning text"}`},
		sseEvt{event: "response.reasoning_text.done", data: `{"type":"response.reasoning_text.done","sequence_number":7,"item_id":"rs_1","output_index":0,"content_index":0,"text":"raw reasoning text"}`},
		sseEvt{event: "response.output_item.done", data: `{"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"summary of thinking"}],"content":[{"type":"reasoning_text","text":"raw reasoning text"}]}}`},
		sseEvt{event: "response.refusal.delta", data: `{"type":"response.refusal.delta","sequence_number":9,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"cannot comply"}`},
		sseEvt{event: "response.refusal.done", data: `{"type":"response.refusal.done","sequence_number":10,"item_id":"msg_1","output_index":1,"content_index":0,"refusal":"cannot comply"}`},
		evCompleted("resp_rea", emptyOutputArray),
	)

	_, w, _, errWithCode := runSSEViaDoResponse(t, sse)
	require.Nil(t, errWithCode)

	chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
	assert.Equal(t, 1, doneCount)
	require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])

	var reasoning, refusal strings.Builder
	for _, ch := range chunks {
		d := firstDeltaOf(ch)
		if d == nil {
			continue
		}
		if s, ok := d["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}
		if s, ok := d["refusal"].(string); ok {
			refusal.WriteString(s)
		}
	}
	assert.Equal(t, "summary of thinkingraw reasoning text", reasoning.String())
	assert.Equal(t, "cannot comply", refusal.String())
	assert.Equal(t, "stop", finishReasonOf(chunks[len(chunks)-1]))
}

// 报告二 22：response.reasoning.delta 是虚构事件名，不得再被识别为推理事件，
// 未知事件必须显式终止而非静默忽略（DN-4 流式）。
func TestSSEStream_ChatCompletions_FictionalReasoningDeltaTerminates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_fake"),
		sseEvt{event: "response.reasoning.delta", data: `{"type":"response.reasoning.delta","delta":"thinking..."}`},
	)

	_, w, usage, errWithCode := runSSEViaDoResponse(t, sse)
	require.NotNil(t, errWithCode, "虚构事件名必须作为未知事件显式终止")
	assert.Nil(t, usage)

	body := w.Body.String()
	assert.NotContains(t, body, `"reasoning_content"`)
	assert.NotContains(t, body, `"finish_reason":"stop"`)
	assert.Contains(t, body, `"error"`)
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"))
}

// 报告二 22 + §1.3：response.error/failed 转 Chat stream error 后 [DONE]，
// 且返回 ErrorWithStatusCode 供渠道统计记失败。
func TestSSEStream_ChatCompletions_ErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_err"),
		sseEvt{event: "error", data: `{"type":"error","code":"internal_error","message":"Something went wrong"}`},
	)

	_, w, usage, errWithCode := runSSEViaDoResponse(t, sse)
	require.NotNil(t, errWithCode, "错误终态必须回传渠道统计（预扣费回滚依赖此返回）")
	assert.Nil(t, usage)

	body := w.Body.String()
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount)
	require.NotEmpty(t, chunks)
	errObj, ok := chunkHasError(chunks[len(chunks)-1])
	require.True(t, ok, "错误 chunk 必须携带 error 成员")
	assert.Contains(t, errObj["message"], "Something went wrong")
	assert.Contains(t, errObj["message"], "internal_error")
	assert.NotContains(t, body, `"finish_reason":"stop"`)
}

// 报告二：Responses 流本身无 [DONE]；正常路径以 response.completed 终止，网关 [DONE] 恰一次。
func TestSSEStream_ChatCompletions_CompletedNoDone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_no_done"),
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"hi"`)},
		evCompleted("resp_no_done", emptyOutputArray),
	)

	_, w, usage, errWithCode := runSSEViaDoResponse(t, sse)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	body := w.Body.String()
	assert.Contains(t, body, `"finish_reason":"stop"`)
	assert.Equal(t, 1, strings.Count(body, "data: [DONE]"), "expected exactly one [DONE]")
}

// errReader 模拟上游流式传输中途异常断开（非 EOF，如连接重置）
type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// 上游流读取异常（非 EOF）且未产出任何内容：已提交的 SSE 必须以错误 chunk + [DONE] 收尾，
// 并回传 ErrorWithStatusCode 供渠道统计与预扣费回滚。
func TestSSEStream_ChatCompletions_StreamReadError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(errReader{err: errors.New("connection reset by peer")}),
	}
	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.NotNil(t, errWithCode, "stream read error should be reported to caller")
	assert.Nil(t, usage)
	assert.Equal(t, "stream_read_error", errWithCode.Error.Code)

	body := w.Body.String()
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount, "expected exactly one [DONE]")
	require.NotEmpty(t, chunks)
	_, ok := chunkHasError(chunks[len(chunks)-1])
	assert.True(t, ok)
	assert.Contains(t, body, `"message":"connection reset by peer"`)
	assert.NotContains(t, body, `"finish_reason":"stop"`)
}

// 部分内容下发后上游异常断流（非 EOF）：不得写 finish_reason:"stop" 伪装正常完成。
func TestSSEStream_ChatCompletions_PartialThenError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_partial"),
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"partial "`)},
	)

	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(io.MultiReader(
			strings.NewReader(sse),
			errReader{err: errors.New("connection reset by peer")},
		)),
	}
	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.NotNil(t, errWithCode)
	assert.Nil(t, usage)

	body := w.Body.String()
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount)
	assert.Contains(t, body, `"content":"partial "`)
	_, ok := chunkHasError(chunks[len(chunks)-1])
	assert.True(t, ok)
	assert.NotContains(t, body, `"finish_reason":"stop"`)
}

// response.completed 处理完毕后连接才异常断开：响应已事实完成，
// 不得补发误导性错误 chunk，仅保证 [DONE] 恰一次；上游断流错误仍回传统计侧。
func TestSSEStream_ChatCompletions_CompletedThenError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_done_err"),
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"hi"`)},
		evCompleted("resp_done_err", emptyOutputArray),
	)

	c, w := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(io.MultiReader(
			strings.NewReader(sse),
			errReader{err: errors.New("connection reset by peer")},
		)),
	}
	m := testMeta()
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	usage, errWithCode := adpt.DoResponse(c, resp, m)
	require.NotNil(t, errWithCode, "上游断流错误仍上报调用方（渠道统计/重试）")
	require.NotNil(t, usage)
	assert.Equal(t, 11, usage.PromptTokens)
	assert.Equal(t, 7, usage.CompletionTokens)

	body := w.Body.String()
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount)
	assert.Equal(t, "stop", finishReasonOf(chunks[len(chunks)-1]))
	for _, ch := range chunks {
		_, hasErr := chunkHasError(ch)
		assert.False(t, hasErr, "已事实完成的流不得补发错误 chunk")
	}
}

// ---------- T5 契约测试 ----------

// TestStreamChatFromResponsesCanonicalSequence 契约测试一：
// 无 [DONE] 的合法 created→role/text/refusal/tool→completed 流且 include_usage=true，
// 公共 metadata、首 role、finish（usage:null）、独立 choices:[] usage chunk、单一 [DONE] 顺序合法。
func TestStreamChatFromResponsesCanonicalSequence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_canon"),
		sseEvt{event: "response.in_progress", data: `{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_canon","object":"response","created_at":1725000000,"status":"in_progress","model":"gpt-5-codex","output":[]}}`},
		sseEvt{event: "response.output_item.added", data: messageItemAdded},
		sseEvt{event: "response.content_part.added", data: contentPartAdded},
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"Hello "`)},
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"World"`)},
		sseEvt{event: "response.output_text.done", data: textDoneEvt},
		sseEvt{event: "response.content_part.done", data: contentPartDoneEvt},
		sseEvt{event: "response.output_item.done", data: messageItemDoneEvt},
		sseEvt{event: "response.content_part.added", data: refusalPartAddedFmt},
		sseEvt{event: "response.refusal.delta", data: `{"type":"response.refusal.delta","sequence_number":30,"item_id":"msg_1","output_index":0,"content_index":1,"delta":"but not everything"}`},
		sseEvt{event: "response.refusal.done", data: `{"type":"response.refusal.done","sequence_number":31,"item_id":"msg_1","output_index":0,"content_index":1,"refusal":"but not everything"}`},
		sseEvt{event: "response.content_part.done", data: `{"type":"response.content_part.done","sequence_number":32,"item_id":"msg_1","output_index":0,"content_index":1,"part":{"type":"refusal","refusal":"but not everything"}}`},
		sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_1", "call_weather_1", "get_weather")},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_1", `"{\"city\":"`)},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_1", `"\"Paris\"}"`)},
		sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_1", `"{\"city\":\"Paris\"}"`)},
		sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_1", "call_weather_1", "get_weather", `"{\"city\":\"Paris\"}"`)},
		evCompleted("resp_canon", `[{"id":"fc_1","type":"function_call","call_id":"call_weather_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`),
	)

	usagePtr := true
	_, w, usage, errWithCode := chatStreamFixture(t, sse, &usagePtr)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)
	assert.Equal(t, 11, usage.PromptTokens)
	assert.Equal(t, 18, usage.TotalTokens)
	require.NotNil(t, usage.PromptTokensDetails)
	assert.Equal(t, 4, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 2, usage.PromptTokensDetails.CacheWriteTokens)
	require.NotNil(t, usage.CompletionTokensDetails)
	assert.Equal(t, 3, usage.CompletionTokensDetails.ReasoningTokens)

	body := w.Body.String()
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount, "[DONE] 恰一次且位于最末")
	assert.True(t, strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]"), "流必须以 [DONE] 收尾")

	// ① 公共 metadata 从 response.created 的 response 对象读取，全 chunk 一致且必填（报告二 6）
	requireUniformChatChunkMetadata(t, chunks, "chatcmpl-resp_canon", "gpt-5-codex", 1725000000)

	// ② 首个 choice chunk 携带 assistant role，且全流仅一次
	require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])
	for _, ch := range chunks[1:] {
		d := firstDeltaOf(ch)
		if d != nil {
			assert.NotContains(t, d, "role", "role delta 只能出现在首个 choice chunk")
		}
	}

	// ③ 顺序：... finish(tool_calls) → 独立 usage chunk → [DONE]（报告二 21）
	require.GreaterOrEqual(t, len(chunks), 3)
	usageChunk := chunks[len(chunks)-1]
	finishChunk := chunks[len(chunks)-2]
	choices, _ := usageChunk["choices"].([]any)
	require.Empty(t, choices, "usage chunk 必须 choices:[]")
	usageObj, ok := usageChunk["usage"].(map[string]any)
	require.True(t, ok, "usage chunk 必须携带 usage 对象")
	for k := range usageObj {
		assert.Truef(t, chatUsageChunkAllowedKeys[k], "unknown usage key %q (chat §8)", k)
	}
	assert.EqualValues(t, 11, usageObj["prompt_tokens"])
	assert.EqualValues(t, 7, usageObj["completion_tokens"])
	assert.EqualValues(t, 18, usageObj["total_tokens"])
	// T1 内部 details 保留（报告二 20）
	pd, ok := usageObj["prompt_tokens_details"].(map[string]any)
	require.True(t, ok)
	for k := range pd {
		assert.Truef(t, chatPromptDetailsKeys[k], "unknown prompt_tokens_details key %q", k)
	}
	assert.EqualValues(t, 4, pd["cached_tokens"])
	assert.EqualValues(t, 2, pd["cache_write_tokens"])
	cd, ok := usageObj["completion_tokens_details"].(map[string]any)
	require.True(t, ok)
	for k := range cd {
		assert.Truef(t, chatCompletionDetailsKeys[k], "unknown completion_tokens_details key %q", k)
	}
	assert.EqualValues(t, 3, cd["reasoning_tokens"])
	assert.EqualValues(t, 1, cd["accepted_prediction_tokens"])

	// finish chunk usage 恒为 null（Chat §2.4：其他 chunk 的 usage 为 null）
	assert.Nil(t, finishChunk["usage"])
	assert.Equal(t, "tool_calls", finishReasonOf(finishChunk))
	assert.Equal(t, 1, strings.Count(body, `"prompt_tokens"`),
		"usage 对象只允许出现在独立 usage chunk（Chat §2.4）")

	// ④ 文本/拒绝/工具参数内容完整
	var content, refusal strings.Builder
	for _, ch := range chunks {
		d := firstDeltaOf(ch)
		if d == nil {
			continue
		}
		if s, ok := d["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := d["refusal"].(string); ok {
			refusal.WriteString(s)
		}
	}
	assert.Equal(t, "Hello World", content.String())
	assert.Equal(t, "but not everything", refusal.String())

	frags := toolFragmentsByIndex(chunks)[0]
	require.NotEmpty(t, frags)
	assert.Equal(t, "call_weather_1", frags[0]["id"])
	require.Equal(t, 1, countNamedFragments(frags, "function", "name"))
	joined := concatFragmentField(frags, "function", "arguments")
	assert.Equal(t, `{"city":"Paris"}`, joined, "参数每个字节恰好发送一次（无重复前缀）")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(joined), &decoded))
}

// TestStreamChatFromResponsesRecoversOutOfOrderToolArguments 契约测试二：
// arguments delta 先于 added（报告二 23），done 含完整 function item；
// id/name 补全且参数每个字节只发送一次，finish_reason=tool_calls。
func TestStreamChatFromResponsesRecoversOutOfOrderToolArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 两个并行调用的 delta 都先于各自 added 到达，且互不串扰
	sse := responsesSSE(
		evCreated("resp_ooo"),
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_A", `"{\"q\":"`)},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_B", `"{\"n\":"`)},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_A", `"\"sunny\"}"`)},
		sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_A", "call_a", "get_weather")},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_B", `"42}"`)},
		sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_B", "call_b", "get_count")},
		sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_A", `"{\"q\":\"sunny\"}"`)},
		sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_B", `"{\"n\":42}"`)},
		sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_A", "call_a", "get_weather", `"{\"q\":\"sunny\"}"`)},
		sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_B", "call_b", "get_count", `"{\"n\":42}"`)},
		evCompleted("resp_ooo", `[{"id":"fc_A","type":"function_call","call_id":"call_a","name":"get_weather","arguments":"{\"q\":\"sunny\"}","status":"completed"},{"id":"fc_B","type":"function_call","call_id":"call_b","name":"get_count","arguments":"{\"n\":42}","status":"completed"}]`),
	)

	_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
	assert.Equal(t, 1, doneCount)
	requireUniformChatChunkMetadata(t, chunks, "chatcmpl-resp_ooo", "gpt-5-codex", 1725000000)
	require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])

	byIndex := toolFragmentsByIndex(chunks)
	require.Len(t, byIndex, 2, "两个并行调用按 index 隔离")

	// 每个 index 恰有一个携带 id+name 的首分片；拼接结果与最终 arguments 逐字节相等（每个字节只发一次）
	wantArgs := map[interface{}]string{0: `{"q":"sunny"}`, 1: `{"n":42}`}
	wantCallIDs := map[interface{}]string{0: "call_a", 1: "call_b"}
	for idx, frags := range byIndex {
		require.NotEmpty(t, frags)
		assert.Equal(t, 1, countNamedFragments(frags, "function", "name"), "index %d: name 只随首分片发送", idx)
		assert.Equal(t, wantCallIDs[idx], frags[0]["id"])
		joined := concatFragmentField(frags, "function", "arguments")
		assert.Equal(t, wantArgs[idx], joined, "index %d: 参数无重复前缀且完整", idx)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(joined), &decoded))
	}

	last := chunks[len(chunks)-1]
	assert.Equal(t, "tool_calls", finishReasonOf(last))
}

// TestStreamChatFromResponsesRejectsMalformedAndTerminalErrorEvents 契约测试三：
// 畸形 JSON、response.error、incomplete/cancelled 子流（报告二 22/23/24）：
// 无 stop 假成功，错误或对应 finish 后单一 [DONE]。
func TestStreamChatFromResponsesRejectsMalformedAndTerminalErrorEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 断言：流以错误 chunk + 单一 [DONE] 终止，无 finish_reason=stop/tool_calls，函数回传失败
	assertErrorTermination := assertChatStreamErrorTermination

	// 断言：终态生成合法 finish（非 stop 假成功）+ 单一 [DONE]，且不算渠道失败
	assertFinishTermination := func(t *testing.T, sse string, wantFinish string) {
		t.Helper()
		_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
		require.Nil(t, errWithCode, "incomplete 的合法 finish 映射不是失败")
		require.NotNil(t, usage, "incomplete 终态同样携带 usage（Responses §6）")
		body := w.Body.String()
		chunks, doneCount := parseChatSSEChunks(t, body)
		assert.Equal(t, 1, doneCount)
		require.NotEmpty(t, chunks)
		assert.Equal(t, wantFinish, finishReasonOf(chunks[len(chunks)-1]))
	}

	t.Run("malformed_json_midstream", func(t *testing.T) {
		// 报告二 24：非空 payload JSON 解析失败 → invalid_stream_event，不吞掉
		sse := responsesSSE(
			evCreated("resp_bad"),
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"Hi"`)},
		) + "data: {\"delta\": \"oops\n\n"
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
		_, w, _, _ := chatStreamFixture(t, sse, nil)
		body := w.Body.String()
		assert.Contains(t, body, `"content":"Hi"`, "畸形事件前已合法下发的内容不回滚")
		assert.Contains(t, body, model.CodeInvalidStreamEvent)
	})

	t.Run("malformed_first_event_no_created", func(t *testing.T) {
		sse := "data: this-is-not-json\n\n"
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})

	t.Run("response_error_event", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_rerr"),
			sseEvt{event: "response.error", data: `{"type":"response.error","sequence_number":5,"code":"rate_limit_exceeded","message":"slow down"}`},
		)
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})

	t.Run("response_failed_event", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_fail"),
			sseEvt{event: "response.failed", data: `{"type":"response.failed","sequence_number":6,"response":{"id":"resp_fail","object":"response","created_at":1725000000,"status":"failed","model":"gpt-5-codex","output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0},"error":{"code":"server_error","message":"upstream exploded"}}}`},
		)
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
		_, w, _, _ := chatStreamFixture(t, sse, nil)
		assert.Contains(t, w.Body.String(), "upstream exploded")
	})

	t.Run("response_cancelled_no_stop", func(t *testing.T) {
		// cancelled 不得伪装 stop（T4 mapFinishReason 契约：显式错误）
		sse := responsesSSE(
			evCreated("resp_cancel"),
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"half"`)},
			sseEvt{event: "response.cancelled", data: fmt.Sprintf(cancelledDataFmt, "resp_cancel", completedUsageJSON)},
		)
		assertErrorTermination(t, sse, model.CodeUnsupportedMapping, "invalid_upstream_response")
	})

	t.Run("incomplete_max_output_tokens", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_inc_len"),
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"trunc"`)},
			sseEvt{event: "response.incomplete", data: fmt.Sprintf(incompleteDataFmt, "resp_inc_len", completedUsageJSON, "max_output_tokens")},
		)
		assertFinishTermination(t, sse, "length")
	})

	t.Run("incomplete_content_filter", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_inc_cf"),
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"no"`)},
			sseEvt{event: "response.incomplete", data: fmt.Sprintf(incompleteDataFmt, "resp_inc_cf", completedUsageJSON, "content_filter")},
		)
		assertFinishTermination(t, sse, "content_filter")
	})

	t.Run("incomplete_unknown_reason_is_error", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_inc_unk"),
			sseEvt{event: "response.incomplete", data: fmt.Sprintf(incompleteDataFmt, "resp_inc_unk", completedUsageJSON, "steered")},
		)
		assertErrorTermination(t, sse, model.CodeUnsupportedMapping, "invalid_upstream_response")
	})

	t.Run("tool_delta_without_item_id_is_unrecoverable", func(t *testing.T) {
		// 报告二 23：delta 无 item_id 无法归属 → 不可恢复，禁止静默丢弃
		sse := responsesSSE(
			evCreated("resp_orphan"),
			sseEvt{event: "response.function_call_arguments.delta", data: `{"type":"response.function_call_arguments.delta","sequence_number":3,"delta":"{\"a\":1}"}`},
			evCompleted("resp_orphan", emptyOutputArray),
		)
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})

	t.Run("tool_never_completed_at_terminal", func(t *testing.T) {
		// added 后无 done：completed 时状态机不完整 → 不得以 tool_calls 假成功收尾
		sse := responsesSSE(
			evCreated("resp_unfin"),
			sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_1", "call_x", "get_weather")},
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_1", `"{\"a\":1}"`)},
			evCompleted("resp_unfin", emptyOutputArray),
		)
		assertErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
	})

	t.Run("tool_done_missing_call_id", func(t *testing.T) {
		// delta 先到、added 缺失、done 的 item 又没有 call_id：id 永远无法补全 → 不可恢复
		sse := responsesSSE(
			evCreated("resp_nocall"),
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_1", `"{}"`)},
			sseEvt{event: "response.output_item.done", data: `{"type":"response.output_item.done","sequence_number":13,"output_index":1,"item":{"id":"fc_1","type":"function_call","name":"get_weather","arguments":"{}","status":"completed"}}`},
			evCompleted("resp_nocall", emptyOutputArray),
		)
		assertErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
	})

	t.Run("non_string_arguments_delta_not_buffered", func(t *testing.T) {
		// T4 移交①：非法载荷字节不得写入缓冲区——delta 为对象即畸形事件，直接协议错误终止
		sse := responsesSSE(
			evCreated("resp_baddelta"),
			sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_1", "call_z", "get_weather")},
			sseEvt{event: "response.function_call_arguments.delta", data: `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":{"nested":true}}`},
			evCompleted("resp_baddelta", emptyOutputArray),
		)
		_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
		require.NotNil(t, errWithCode)
		assert.Nil(t, usage)
		body := w.Body.String()
		chunks, doneCount := parseChatSSEChunks(t, body)
		assert.Equal(t, 1, doneCount)
		// 错误发生前 header 分片可以已发出，但非法 delta 的字节绝不出现在任何 arguments 中
		for _, ch := range chunks {
			for _, frags := range toolFragmentsByIndex([]map[string]any{ch}) {
				for _, f := range frags {
					if fn, ok := f["function"].(map[string]any); ok {
						args, _ := fn["arguments"].(string)
						assert.NotContains(t, args, "nested")
					}
				}
			}
		}
		assert.NotContains(t, body, `"finish_reason":"stop"`)
	})

	t.Run("unsupported_output_item_event_fails", func(t *testing.T) {
		// DN-4 流式：unsupported output event 不得静默忽略
		sse := responsesSSE(
			evCreated("resp_unsup"),
			sseEvt{event: "response.output_item.added", data: `{"type":"response.output_item.added","sequence_number":4,"output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress","action":{"type":"search","query":"q"}}}`},
		)
		assertErrorTermination(t, sse, model.CodeUnsupportedOutputItem, "invalid_upstream_response")
	})

	t.Run("unknown_event_name_fails", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_unknown"),
			sseEvt{event: "response.made_up_thing", data: `{"type":"response.made_up_thing","foo":"bar"}`},
		)
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})

	t.Run("upstream_done_sentinel_without_terminal_fails", func(t *testing.T) {
		// Responses 流本身无 [DONE]：上游自发送 [DONE] 且无终态 → 协议违例
		sse := responsesSSE(
			evCreated("resp_fakedone"),
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"text"`)},
		) + "data: [DONE]\n\n"
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})

	t.Run("eof_without_terminal_fails", func(t *testing.T) {
		// 无终态的提前 EOF 不得合成 stop
		sse := responsesSSE(
			evCreated("resp_trunc"),
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"text"`)},
		)
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})

	t.Run("content_before_created_fails", func(t *testing.T) {
		// 缺失 response.created → 无法产出合法公共 metadata，禁止伪造
		sse := responsesSSE(
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"no meta"`)},
		)
		assertErrorTermination(t, sse, model.CodeInvalidStreamEvent, "invalid_upstream_response")
	})
}

// TestStreamChatFromResponsesCustomToolCallEmitsCustomVariant 流式 custom call 必须输出
// Chat §6.1.1 custom 变体（type:"custom", custom:{name,input}），禁止伪装 function（§4 DN-3 流式）。
func TestStreamChatFromResponsesCustomToolCallEmitsCustomVariant(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const finalInput = "*** Begin Patch\n*** Update File: a.txt\n*** End Patch"
	finalInputJSON, err := json.Marshal(finalInput)
	require.NoError(t, err)

	sse := responsesSSE(
		evCreated("resp_custom"),
		sseEvt{event: "response.output_item.added", data: `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_c1","name":"apply_patch","input":"","status":"in_progress"}}`},
		sseEvt{event: "response.custom_tool_call_input.delta", data: `{"type":"response.custom_tool_call_input.delta","sequence_number":3,"item_id":"ctc_1","output_index":0,"delta":"*** Begin Patch\n*** Upda"}`},
		sseEvt{event: "response.custom_tool_call_input.delta", data: `{"type":"response.custom_tool_call_input.delta","sequence_number":4,"item_id":"ctc_1","output_index":0,"delta":"te File: a.txt\n*** End Patch"}`},
		sseEvt{event: "response.custom_tool_call_input.done", data: `{"type":"response.custom_tool_call_input.done","sequence_number":5,"item_id":"ctc_1","output_index":0,"input":` + string(finalInputJSON) + `}`},
		sseEvt{event: "response.output_item.done", data: `{"type":"response.output_item.done","sequence_number":6,"output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_c1","name":"apply_patch","input":` + string(finalInputJSON) + `,"status":"completed"}}`},
		evCompleted("resp_custom", `[{"id":"ctc_1","type":"custom_tool_call","call_id":"call_c1","name":"apply_patch","input":`+string(finalInputJSON)+`,"status":"completed"}]`),
	)

	require.NoError(t, err)
	_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)

	chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
	assert.Equal(t, 1, doneCount)
	require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])

	frags := toolFragmentsByIndex(chunks)[0]
	require.NotEmpty(t, frags)
	for _, f := range frags {
		assert.Equal(t, "custom", f["type"], "custom 变体不得伪装 function（DN-3）")
		_, hasFunction := f["function"]
		assert.False(t, hasFunction)
	}
	assert.Equal(t, "call_c1", frags[0]["id"])
	require.Equal(t, 1, countNamedFragments(frags, "custom", "name"))
	joined := concatFragmentField(frags, "custom", "input")
	assert.Equal(t, finalInput, joined, "custom input 每个字节恰好发送一次")
	assert.Equal(t, "tool_calls", finishReasonOf(chunks[len(chunks)-1]))
}

// TestStreamChatFromResponsesRejectsToolCallIdentityFlip 锁定 reviewer P1-1：
// 同一 item_id 的事件类型（function_call/custom_tool_call）或身份（call_id/name）
// 与已确定来源不一致（上游自相矛盾且不可恢复）必须经 failStream 以
// malformed_tool_call 显式失败（错误 chunk + 单一 [DONE] + 502 ErrorWithStatusCode），
// 禁止静默覆盖伪装 function（DN-3）；同值重复事件必须幂等不误杀。
func TestStreamChatFromResponsesRejectsToolCallIdentityFlip(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("seqD_custom_delta_to_function_arguments_done", func(t *testing.T) {
		// reviewer 实验序列 D：custom input delta →（字节兼容）→ function_call_arguments.done
		// 同一 item id → 旧代码只升不降掩盖翻转，最终假成功 tool_calls；修复后显式失败。
		sse := responsesSSE(
			evCreated("resp_flip_d"),
			sseEvt{event: "response.custom_tool_call_input.delta", data: `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"item_id":"ctc_d","output_index":0,"delta":"*** Begin Patch"}`},
			sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "ctc_d", `"*** Begin Patch"`)},
			sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "ctc_d", "call_f", "f_e", `"*** Begin Patch"`)},
			evCompleted("resp_flip_d", `[{"id":"ctc_d","type":"function_call","call_id":"call_f","name":"f_e","arguments":"*** Begin Patch","status":"completed"}]`),
		)
		assertChatStreamErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
		_, w, _, _ := chatStreamFixture(t, sse, nil)
		assert.NotContains(t, w.Body.String(), `"tool_calls"`, "翻转被检测到前不得发出任何（含 mixed-type）工具分片")
	})

	t.Run("seqE_custom_delta_to_function_item_done", func(t *testing.T) {
		// reviewer 实验序列 E 变体：custom delta → output_item.done 携带 function_call item
		// （同 id、name 不同）→ 同样 malformed_tool_call 显式失败。
		sse := responsesSSE(
			evCreated("resp_flip_e"),
			sseEvt{event: "response.custom_tool_call_input.delta", data: `{"type":"response.custom_tool_call_input.delta","sequence_number":2,"item_id":"ctc_e","output_index":0,"delta":"*** Begin Patch"}`},
			sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "ctc_e", "call_e", "f_e", `"*** Begin Patch"`)},
			evCompleted("resp_flip_e", `[{"id":"ctc_e","type":"function_call","call_id":"call_e","name":"f_e","arguments":"*** Begin Patch","status":"completed"}]`),
		)
		assertChatStreamErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
		_, w, _, _ := chatStreamFixture(t, sse, nil)
		assert.NotContains(t, w.Body.String(), `"tool_calls"`, "custom 字节被重包装成 function arguments 的假成功路径必须被锁死")
	})

	t.Run("reverse_function_delta_to_custom_input_done", func(t *testing.T) {
		// 反向：function_call_arguments.delta → custom_tool_call_input.done 同 item 混用。
		sse := responsesSSE(
			evCreated("resp_flip_r"),
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_r", `"{\"a\":1"`)},
			sseEvt{event: "response.custom_tool_call_input.done", data: `{"type":"response.custom_tool_call_input.done","sequence_number":12,"item_id":"fc_r","output_index":1,"input":"{\"a\":1}"}`},
			sseEvt{event: "response.output_item.done", data: `{"type":"response.output_item.done","sequence_number":13,"output_index":1,"item":{"id":"fc_r","type":"custom_tool_call","call_id":"call_r","name":"echo","input":"{\"a\":1}","status":"completed"}}`},
			evCompleted("resp_flip_r", `[{"id":"fc_r","type":"custom_tool_call","call_id":"call_r","name":"echo","input":"{\"a\":1}","status":"completed"}]`),
		)
		assertChatStreamErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
	})

	t.Run("mixed_delta_families_same_item", func(t *testing.T) {
		// 要求 2：delta 族收口——function delta 与 custom delta 对同一 item 混用（旧代码
		// getOrCreateToolState 只升不降掩盖翻转，走完生命周期后假成功）→ 必须显式失败。
		sse := responsesSSE(
			evCreated("resp_flip_m"),
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_m", `"{\"a"`)},
			sseEvt{event: "response.custom_tool_call_input.delta", data: `{"type":"response.custom_tool_call_input.delta","sequence_number":3,"item_id":"fc_m","output_index":1,"delta":"\":1}"}`},
			sseEvt{event: "response.custom_tool_call_input.done", data: `{"type":"response.custom_tool_call_input.done","sequence_number":12,"item_id":"fc_m","output_index":1,"input":"{\"a\":1}"}`},
			sseEvt{event: "response.output_item.done", data: `{"type":"response.output_item.done","sequence_number":13,"output_index":1,"item":{"id":"fc_m","type":"custom_tool_call","call_id":"call_m","name":"echo","input":"{\"a\":1}","status":"completed"}}`},
			evCompleted("resp_flip_m", `[{"id":"fc_m","type":"custom_tool_call","call_id":"call_m","name":"echo","input":"{\"a\":1}","status":"completed"}]`),
		)
		assertChatStreamErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
	})

	t.Run("name_conflict_same_type", func(t *testing.T) {
		// 类型一致但 done 的 name 与 added 已确定来源不同 → 禁止静默覆盖。
		sse := responsesSSE(
			evCreated("resp_name_flip"),
			sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_n", "call_n", "get_weather")},
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_n", `"{\"a\":1}"`)},
			sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_n", `"{\"a\":1}"`)},
			sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_n", "call_n", "evil_name", `"{\"a\":1}"`)},
			evCompleted("resp_name_flip", emptyOutputArray),
		)
		assertChatStreamErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
		_, w, _, _ := chatStreamFixture(t, sse, nil)
		chunks, _ := parseChatSSEChunks(t, w.Body.String())
		for _, frags := range toolFragmentsByIndex(chunks) {
			for _, f := range frags {
				assert.NotEqual(t, "evil_name", f["id"], "被拒绝的身份值不得进入 tool_call 分片（错误消息内披露除外）")
				if fn, ok := f["function"].(map[string]any); ok {
					assert.NotEqual(t, "evil_name", fn["name"], "被拒绝的身份值不得覆盖已确定 name")
				}
			}
		}
	})

	t.Run("call_id_conflict_same_type", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_callid_flip"),
			sseEvt{event: "response.output_item.added", data: fmt.Sprintf(toolAddedDataFmt, "fc_c", "call_c", "get_weather")},
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_c", `"{\"a\":1}"`)},
			sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_c", "call_evil", "get_weather", `"{\"a\":1}"`)},
			evCompleted("resp_callid_flip", emptyOutputArray),
		)
		assertChatStreamErrorTermination(t, sse, model.CodeMalformedToolCall, "invalid_upstream_response")
		_, w, _, _ := chatStreamFixture(t, sse, nil)
		chunks, _ := parseChatSSEChunks(t, w.Body.String())
		for _, frags := range toolFragmentsByIndex(chunks) {
			for _, f := range frags {
				assert.NotEqual(t, "call_evil", f["id"], "被拒绝的 call_id 不得进入 tool_call 分片（错误消息内披露除外）")
			}
		}
	})

	t.Run("idempotent_same_values_not_killed", func(t *testing.T) {
		// 要求 3（幂等正样本）：同值 added 重发 + done 与 added 同值 → 正常完成，header 恰一次。
		added := fmt.Sprintf(toolAddedDataFmt, "fc_idem", "call_idem", "get_weather")
		sse := responsesSSE(
			evCreated("resp_idem"),
			sseEvt{event: "response.output_item.added", data: added},
			sseEvt{event: "response.output_item.added", data: added},
			sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_idem", `"{\"a\":1}"`)},
			sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_idem", `"{\"a\":1}"`)},
			sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_idem", "call_idem", "get_weather", `"{\"a\":1}"`)},
			evCompleted("resp_idem", `[{"id":"fc_idem","type":"function_call","call_id":"call_idem","name":"get_weather","arguments":"{\"a\":1}","status":"completed"}]`),
		)
		_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
		require.Nil(t, errWithCode, "同值重复事件不得误杀")
		require.NotNil(t, usage)
		chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
		assert.Equal(t, 1, doneCount)
		requireUniformChatChunkMetadata(t, chunks, "chatcmpl-resp_idem", "gpt-5-codex", 1725000000)
		frags := toolFragmentsByIndex(chunks)[0]
		require.NotEmpty(t, frags)
		idCount := 0
		for _, f := range frags {
			if _, ok := f["id"]; ok {
				idCount++
			}
		}
		assert.Equal(t, 1, idCount, "header（id+name）分片恰一次")
		assert.Equal(t, 1, countNamedFragments(frags, "function", "name"))
		joined := concatFragmentField(frags, "function", "arguments")
		assert.Equal(t, `{"a":1}`, joined)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(joined), &decoded))
		assert.Equal(t, "tool_calls", finishReasonOf(chunks[len(chunks)-1]))
	})
}

// TestStreamChatFromResponsesQueuedAndSummaryPartPositives P2-1 正样本补全：
// response.queued（与 response.created 同 metadata 来源语义）与
// response.reasoning_summary_part.done（合法 §7 事件不得误杀、推理文本不得重复投递）。
func TestStreamChatFromResponsesQueuedAndSummaryPartPositives(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("response_queued_carries_metadata", func(t *testing.T) {
		sse := responsesSSE(
			sseEvt{event: "response.queued", data: `{"type":"response.queued","sequence_number":0,"response":{"id":"resp_q","object":"response","created_at":1725000000,"status":"queued","model":"gpt-5-codex","output":[]}}`},
			sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"ok"`)},
			evCompleted("resp_q", `[{"id":"msg_q","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}]`),
		)
		_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
		require.Nil(t, errWithCode, "response.queued 携带完整 response 对象，与 created 同 metadata 来源语义，不得判畸形")
		require.NotNil(t, usage)
		chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
		assert.Equal(t, 1, doneCount)
		requireUniformChatChunkMetadata(t, chunks, "chatcmpl-resp_q", "gpt-5-codex", 1725000000)
		require.Equal(t, "assistant", firstDeltaOf(chunks[0])["role"])
		assert.Equal(t, "stop", finishReasonOf(chunks[len(chunks)-1]))
	})

	t.Run("reasoning_summary_part_done_not_killed", func(t *testing.T) {
		sse := responsesSSE(
			evCreated("resp_sum"),
			sseEvt{event: "response.output_item.added", data: `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"in_progress","summary":[]}}`},
			sseEvt{event: "response.reasoning_summary_part.added", data: `{"type":"response.reasoning_summary_part.added","sequence_number":2,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`},
			sseEvt{event: "response.reasoning_summary_text.delta", data: `{"type":"response.reasoning_summary_text.delta","sequence_number":3,"item_id":"rs_1","output_index":0,"summary_index":0,"delta":"thinking hard"}`},
			sseEvt{event: "response.reasoning_summary_part.done", data: `{"type":"response.reasoning_summary_part.done","sequence_number":4,"item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"thinking hard"}}`},
			sseEvt{event: "response.output_item.done", data: `{"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"thinking hard"}]}}`},
			evCompleted("resp_sum", `[{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"thinking hard"}]}]`),
		)
		_, w, usage, errWithCode := chatStreamFixture(t, sse, nil)
		require.Nil(t, errWithCode, "reasoning_summary_part.done 为 §7 合法事件，不得误杀")
		require.NotNil(t, usage)
		body := w.Body.String()
		chunks, doneCount := parseChatSSEChunks(t, body)
		assert.Equal(t, 1, doneCount)
		assert.Equal(t, 1, strings.Count(body, `"reasoning_content":"thinking hard"`), "定稿载荷不得导致推理文本重复投递")
		assert.Equal(t, "stop", finishReasonOf(chunks[len(chunks)-1]))
	})
}

// TestStreamChatFromResponsesAddedArgumentsDecodedNotRaw 锁定 T4 移交①：
// added 初值必须经 ArgumentsString() 解码为实际 JSON 字符串（报告二 4 的二次编码回归），
// 且解码初值与后续 delta 拼接结果保持合法 JSON、无前缀重复。
func TestStreamChatFromResponsesAddedArgumentsDecodedNotRaw(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// added 携带 wire JSON 字符串初值 "{\"partial\":true"（解码后为未闭合前缀 {"partial\":true），
	// delta 追加 `,"n":1}` 补全；若初值以带外层引号的二次编码原文写入缓冲区，
	// 拼接结果必然不可 decode。
	sse := responsesSSE(
		evCreated("resp_init"),
		sseEvt{event: "response.output_item.added", data: `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"fc_i","type":"function_call","call_id":"call_i","name":"do_thing","arguments":"{\"partial\":true","status":"in_progress"}}`},
		sseEvt{event: "response.function_call_arguments.delta", data: fmt.Sprintf(toolArgsDeltaFmt, "fc_i", `",\"n\":1}"`)},
		sseEvt{event: "response.function_call_arguments.done", data: fmt.Sprintf(toolArgsDoneFmt, "fc_i", `"{\"partial\":true,\"n\":1}"`)},
		sseEvt{event: "response.output_item.done", data: fmt.Sprintf(toolItemDoneFmt, "fc_i", "call_i", "do_thing", `"{\"partial\":true,\"n\":1}"`)},
		evCompleted("resp_init", `[{"id":"fc_i","type":"function_call","call_id":"call_i","name":"do_thing","arguments":"{\"partial\":true,\"n\":1}","status":"completed"}]`),
	)

	_, w, _, errWithCode := chatStreamFixture(t, sse, nil)
	require.Nil(t, errWithCode)

	chunks, doneCount := parseChatSSEChunks(t, w.Body.String())
	assert.Equal(t, 1, doneCount)
	frags := toolFragmentsByIndex(chunks)[0]
	require.NotEmpty(t, frags)
	joined := concatFragmentField(frags, "function", "arguments")
	assert.Equal(t, `{"partial":true,"n":1}`, joined)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(joined), &decoded), "解码后初值 + delta 拼接必须是合法 JSON")
	assert.Equal(t, true, decoded["partial"])
	assert.EqualValues(t, 1, decoded["n"])
}

// TestStreamChatFromResponsesUsageChunkFollowsIncludeUsagePolicy 锁定 Chat §2.4 与 T2 策略：
// usage chunk 是否发送只取决于 ctxkey.ChatStreamIncludeUsage（原始客户端策略），
// 与上游 Responses usage 恒存在无关；false/缺失时不得发送 usage chunk。
func TestStreamChatFromResponsesUsageChunkFollowsIncludeUsagePolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sse := responsesSSE(
		evCreated("resp_pol"),
		sseEvt{event: "response.output_text.delta", data: fmt.Sprintf(textDeltaFmt, `"x"`)},
		evCompleted("resp_pol", emptyOutputArray),
	)

	// include_usage=false：usage 不回显但计费 usage 仍返回
	_, w, usage, errWithCode := chatStreamFixture(t, sse, boolPtr(false))
	require.Nil(t, errWithCode)
	require.NotNil(t, usage)
	body := w.Body.String()
	assert.NotContains(t, body, "prompt_tokens")
	chunks, doneCount := parseChatSSEChunks(t, body)
	assert.Equal(t, 1, doneCount)
	assert.Equal(t, "stop", finishReasonOf(chunks[len(chunks)-1]))

	// include_usage=true：finish 之后、[DONE] 之前出现独立 usage chunk
	_, w2, usage2, errWithCode2 := chatStreamFixture(t, sse, boolPtr(true))
	require.Nil(t, errWithCode2)
	require.NotNil(t, usage2)
	chunks2, doneCount2 := parseChatSSEChunks(t, w2.Body.String())
	require.Equal(t, 1, doneCount2)
	require.GreaterOrEqual(t, len(chunks2), 2)
	usageChunk := chunks2[len(chunks2)-1]
	choices2, _ := usageChunk["choices"].([]any)
	require.Empty(t, choices2)
	usageObj, ok := usageChunk["usage"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 11, usageObj["prompt_tokens"])
	// finish chunk 位于 usage chunk 之前且 usage 为 null
	finishChunk := chunks2[len(chunks2)-2]
	assert.Equal(t, "stop", finishReasonOf(finishChunk))
	assert.Nil(t, finishChunk["usage"])
}

// ==================== P2-4: 请求形态类失败豁免渠道健康统计 ====================

// TestDoResponseStatsExemptionForRequestShapeFailure 锁定客户端请求形态类失败
// 不计入 chatgptsub 本地 EWMA 熔断（与框架冷却「权重 0 不计数」口径对齐）。
// 使用测试专属 channelID 避免全局单例串扰；豁免仅作用于渠道健康统计，失败仍须如实回传。
//
// 真实路径（方案 A 改造）：Responses 模式流式走 codex.StreamResponsesHandler；上游 error 事件
// code=invalid_request_error 经 parseStreamErrorEvent（handler.go:550-567）保留原机器码，
// streamFailureError（handler.go:587-598）仅在 Code 缺失时才兜底 invalid_upstream_response，
// 故回传 {Type:upstream_error, Code:invalid_request_error}，守卫（Type=upstream_error 且 Code
// 命中网关机器码全集）不命中（invalid_request_error 不在全集内），仍按请求形态类豁免——
// 这正是「保留原机器码的非兜底包装」路径。
func TestDoResponseStatsExemptionForRequestShapeFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const channelID = 9601
	statsManager.Reset(channelID)

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// committed-SSE 口径：上游 error 事件声明客户端请求非法（invalid_request_error）
	sse := responsesSSE(
		evCreated("resp_reqshape"),
		sseEvt{event: "error", data: `{"type":"error","code":"invalid_request_error","message":"Invalid request body"}`},
	)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}
	m := testMeta()
	m.ChannelId = channelID
	m.IsStream = true
	m.Mode = relaymode.Responses
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	_, errWithCode := adpt.DoResponse(c, resp, m)
	require.NotNil(t, errWithCode, "请求形态类失败仍须如实回传，豁免不得吞掉失败")
	require.Equal(t, http.StatusBadGateway, errWithCode.StatusCode)
	require.Equal(t, "upstream_error", errWithCode.Error.Type)
	require.Equal(t, "invalid_request_error", errWithCode.Error.Code,
		"非兜底包装保留上游机器码，守卫不得改写分类")
	require.True(t, errWithCode.LooksLikeRequestShapeFailure(), "用例前置：错误必须被分类为请求形态类")

	assert.Equal(t, 0.0, statsManager.GetErrorRate(channelID),
		"请求形态类失败不得计入 EWMA（不 count、不触发 CheckAndDisable）")
}

// TestDoResponseStatsCountsRealUpstreamFailure 反向锁定：真实上游失败（本批 committed-SSE
// 失败口径，502 invalid_upstream_response）必须照常计入 EWMA，豁免不得扩大化。
func TestDoResponseStatsCountsRealUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const channelID = 9602
	statsManager.Reset(channelID)

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	sse := responsesSSE(
		evCreated("resp_real_fail"),
		sseEvt{event: "error", data: `{"type":"error","code":"internal_error","message":"Something went wrong"}`},
	)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}
	m := testMeta()
	m.ChannelId = channelID
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	_, errWithCode := adpt.DoResponse(c, resp, m)
	require.NotNil(t, errWithCode)
	require.False(t, errWithCode.LooksLikeRequestShapeFailure(), "用例前置：真实上游失败不得被误分类为请求形态类")
	assert.Equal(t, http.StatusBadGateway, errWithCode.StatusCode, "committed-SSE 失败口径为 502")
	assert.Equal(t, "invalid_upstream_response", errWithCode.Error.Code)

	assert.Equal(t, 1.0, statsManager.GetErrorRate(channelID),
		"真实上游失败必须计入 EWMA 并触发熔断检查")
}

// TestDoResponseStatsCountsWrappedRequestShapeFailure 锁定方案 A 的新口径：上游流内声明
// invalid_request_error，经 Chat 转换统一兜底包装为 502/invalid_upstream_response
// （message 内嵌 invalid_request_error）后，守卫使其不再被豁免——渠道计一次失败。
// 这是既定取舍：牺牲 client-400 中途上报的豁免，换取上游数据完整性故障（畸形 tool-call 流）
// 可被检出，与「统一兜底包装即渠道/上游侧问题」语义一致。
func TestDoResponseStatsCountsWrappedRequestShapeFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const channelID = 9603
	statsManager.Reset(channelID)

	c, _ := setupGin()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// Chat 模式：error 事件被 streamChatFromResponses 包成 invalid_stream_event +
	// upstreamConversionError → Type=upstream_error/Code=invalid_upstream_response，message 内嵌 marker
	sse := responsesSSE(
		evCreated("resp_wrapped_reqshape"),
		sseEvt{event: "error", data: `{"type":"error","code":"invalid_request_error","message":"Invalid request body"}`},
	)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse))}
	m := testMeta()
	m.ChannelId = channelID
	m.IsStream = true
	m.ActualModelName = "gpt-4o"

	adpt := &Adaptor{}
	_, errWithCode := adpt.DoResponse(c, resp, m)
	require.NotNil(t, errWithCode)
	require.Equal(t, http.StatusBadGateway, errWithCode.StatusCode)
	require.Equal(t, "invalid_upstream_response", errWithCode.Error.Code)
	require.Contains(t, errWithCode.Message, "invalid_request_error",
		"message 必须内嵌上游原码，使本用例在 Message 构造退化时不会空转")
	require.False(t, errWithCode.LooksLikeRequestShapeFailure(),
		"方案 A：兜底包装后不得凭 message 内嵌 marker 豁免")
	assert.Equal(t, 1.0, statsManager.GetErrorRate(channelID),
		"方案 A 取舍：该场景渠道必须计一次失败")
}
