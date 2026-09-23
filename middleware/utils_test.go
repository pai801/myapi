package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/smartystreets/goconvey/convey"
)

// =============================================================================
// 任务 7.1 验收对照表：10 个改造点 × {AC-1 畸形 JSON 不派生值, AC-2 类型不匹配按 R2 处理}
// =============================================================================
//
// 10 个改造点取自 openspec/changes/jsonparser-hotpath/proposal.md 的 Impact 表，
// 按 A(3)/B(3)/C(3)/D(1) 分组。每行给出 AC-1（畸形 JSON 不派生任何值）与
// AC-2（类型不匹配按设计规则 R2：显式类型化读取、失败/降级、绝不产出误导值）的覆盖测试。
//
// | # | 模块 | 改造点 | AC-1 畸形 JSON 不派生值 | AC-2 类型不匹配（R2） |
// |---|------|--------|------------------------|----------------------|
// | A1 | A | middleware/utils.go getRequestModel | TestGetRequestModelMalformedJSON | TestGetRequestModelModelWrongType (+ TestGetRequestModelCaseInsensitiveKeys) |
// | A2 | A | controller/relay.go detectStreamFromBody | TestDetectStreamFromBody_Boundary (+_CacheHitAndFallbackAgree) | TestDetectStreamFromBody_Boundary（stream string/number/object/array） |
// | A3 | A | common/ctxkey 共享缓存键 + 读取辅助 | TestGetRequestBodyMetadata（cached malformed conclusion） | TestGetRequestBodyMetadata（wrong type / pointer / nil） |
// | B1 | B | openai StreamHandler + chatStreamAccumulator.addPayload | TestChatCompletionsStreamForwardsMalformedFrameWithoutValues | TestChatStreamAccumulatorTypeBoundaries / _TypeStrictnessMatchesTypedDecode |
// | B2 | B | relay/controller handleResponsesDirectStream | TestHandleResponsesDirectStream_MalformedFramesForwardedWithoutUsage | TestResponsesStreamAccumulatorAddPayloadResult（non-string type） |
// | B3 | B | codex 三处 type 探测 | TestProbeResponsesEventType（malformed） | TestProbeResponsesEventType（number/bool/object/array） |
// | C1 | C | openai Handler | TestExtractTextResponseEquivalence（malformed）+ TestOpenAIHandlerEntryLevelMalformedReturnsHTTP500 | TestExtractTextResponseEquivalence（type mismatch）+ TestOpenAIHandlerEntryLevelPassthroughAndStorage |
// | C2 | C | relay/controller handleResponsesDirectNonStream | TestResponsesNonStreamEquivalence（malformed） | TestResponsesNonStreamDEC_C2_1MismatchMatrix / _InvalidBasisNoCharge |
// | C3 | C | codex DoResponsesResponse | TestExtractResponsesUsageEquivalence（malformed）+ TestDoResponsesResponseEntryLevelMalformedReturnsHTTP500 | TestExtractResponsesUsageEquivalence（type mismatch） |
// | D  | D | relay/controller relayResponsesDirect / relayResponsesConverted / model 改写 | TestRelayResponsesDirectMalformedJSONDoesNotPanicOrDeriveMeta / TestRelayResponsesConvertedRejectsMalformedJSONWithoutDerivingMeta | TestRelayResponsesTypeMismatchDoesNotDeriveMeta（7.1 新增） |
//
// 表中全部测试均执行成功，无 panic；C2 的 mismatch 行为为有意变更 DEC-C2-1（欠费优于按部分数据扣费）。

// newGetRequestModelContext 构造带请求体 / Content-Type / 路由路径的 *gin.Context，
// 用于覆盖 getRequestModel 的 JSON 与非 JSON 两条分支。
func newGetRequestModelContext(t *testing.T, method, path, contentType, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(method, path, reader)
	if contentType != "" {
		c.Request.Header.Set("Content-Type", contentType)
	}
	return c
}

// readRemainingBody 读尽 c.Request.Body，验证下游仍能拿到完整请求体。
func readRemainingBody(t *testing.T, c *gin.Context) []byte {
	t.Helper()
	require.NotNil(t, c.Request.Body, "body 必须已被恢复，不能为 nil")
	remaining, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err, "恢复后的 body 必须可完整读取")
	return remaining
}

// TestGetRequestModelJSON 覆盖 JSON 分支：字符串 model 正确取出并写入共享元数据缓存，
// 且调用后下游仍能逐字节重读完整请求体。
func TestGetRequestModelJSON(t *testing.T) {
	body := `{"model":"gpt-4-turbo","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", body)

	modelName, err := getRequestModel(c)

	require.NoError(t, err)
	assert.Equal(t, "gpt-4-turbo", modelName)

	metadata, ok := ctxkey.GetRequestBodyMetadata(c)
	require.True(t, ok, "JSON 分支必须写入共享元数据缓存")
	assert.True(t, metadata.WellFormed)
	assert.True(t, metadata.ModelValid)
	assert.Equal(t, "gpt-4-turbo", metadata.Model)
	assert.True(t, metadata.StreamValid)
	assert.True(t, metadata.Stream)

	assert.Equal(t, []byte(body), readRemainingBody(t, c), "下游必须能逐字节重读完整 body")
}

// TestGetRequestModelJSONWithoutStream 覆盖 JSON 请求未带 stream 字段的场景：
// stream 缺失为合法零值，不影响 model 提取。
func TestGetRequestModelJSONWithoutStream(t *testing.T) {
	body := `{"model":"gpt-3.5-turbo"}`
	c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", body)

	modelName, err := getRequestModel(c)

	require.NoError(t, err)
	assert.Equal(t, "gpt-3.5-turbo", modelName)

	metadata, ok := ctxkey.GetRequestBodyMetadata(c)
	require.True(t, ok)
	assert.True(t, metadata.WellFormed)
	assert.True(t, metadata.StreamValid)
	assert.False(t, metadata.Stream)
	assert.Equal(t, []byte(body), readRemainingBody(t, c))
}

// TestGetRequestModelNonObjectRoot 覆盖根类型判定回归：JSON 文档根为数组/数字/字符串/布尔时，
// 必须视为提取失败并返回 error，恢复改造前 json.Unmarshal into ModelRequest 的 400 行为。
// json.Valid 对这些输入均返回 true，故必须由显式根类型判定拦截，不能误归为「键缺失」。
//
// 注意：null 根不在此列。encoding/json 把 null 反序列化进 struct 视为无操作（成功），
// 故 null 根属合法零值路径，由 TestGetRequestModelObjectRootUnchanged 覆盖。
//
// 这些用例是本次修复的核心回归锁定：修复前它们会返回 ("", nil) 并落入 distributor 的 "auto" 选路。
func TestGetRequestModelNonObjectRoot(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"array root", `[1,2]`},
		{"number root", `123`},
		{"string root", `"hello"`},
		{"boolean root", `true`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			modelName, err := getRequestModel(c)

			require.Error(t, err, "根非对象必须返回 error（恢复改造前 400 行为）")
			assert.Equal(t, "", modelName, "提取失败时不得产出 model 值")

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok, "JSON 分支必须写入共享元数据缓存")
			assert.True(t, metadata.WellFormed, "根非对象仍是 well-formed JSON")
			assert.False(t, metadata.ModelValid, "根非对象必须使 ModelValid=false，与键缺失区分")
			assert.False(t, metadata.StreamValid, "根非对象必须使 StreamValid=false")

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c), "下游必须能逐字节重读完整 body")
		})
	}
}

// TestGetRequestModelObjectRootUnchanged 锁定对象根行为完全不变：缺失/null 键为合法零值
// （返回 ("", nil)），字符串 model 正常取出。`{"stream":true}` 无 model 键时 ModelValid 必须
// 仍为 true，避免本次根类型判定误伤既有 stream 语义。
//
// null 根（body 字面量 `null`）亦纳入本组：encoding/json 把 null 反序列化进 struct 视为无操作，
// 与空对象等价，故必须返回 ("", nil) 而非 error。
func TestGetRequestModelObjectRootUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantModel string
		wantValid bool
	}{
		{"empty object", `{}`, "", true},
		{"stream only", `{"stream":true}`, "", true},
		{"model null", `{"model":null}`, "", true},
		{"model string", `{"model":"gpt-4-turbo"}`, "gpt-4-turbo", true},
		{"null root", `null`, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			modelName, err := getRequestModel(c)

			require.NoError(t, err, "对象根下缺失/null 为合法零值，不得报错")
			assert.Equal(t, tc.wantModel, modelName)

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok)
			assert.True(t, metadata.WellFormed)
			assert.Equal(t, tc.wantValid, metadata.ModelValid)

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c))
		})
	}
}

// TestGetRequestModelNullRoot 锁定 null 根的完整元数据契约：encoding/json 把 null 反序列化
// 进 struct 视为无操作，故 null 根等价于空对象——WellFormed/ModelValid/StreamValid 均为 true，
// Model 为零值 ""、Stream 为零值 false，getRequestModel 返回 ("", nil)（不触发 400）。
func TestGetRequestModelNullRoot(t *testing.T) {
	c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", `null`)

	modelName, err := getRequestModel(c)

	require.NoError(t, err, "null 根是合法零值路径，不得报错")
	assert.Equal(t, "", modelName)

	metadata, ok := ctxkey.GetRequestBodyMetadata(c)
	require.True(t, ok, "JSON 分支必须写入共享元数据缓存")
	assert.True(t, metadata.WellFormed)
	assert.True(t, metadata.ModelValid, "null 根不得使 ModelValid=false")
	assert.Equal(t, "", metadata.Model)
	assert.True(t, metadata.StreamValid, "null 根不得使 StreamValid=false")
	assert.False(t, metadata.Stream)

	assert.Equal(t, []byte("null"), readRemainingBody(t, c))
}

// TestGetRequestModelCaseInsensitiveKeys 锁定 model 键匹配大小写不敏感（B-1 回归）。
//
// 改造前把 body 反序列化进 struct（json.Unmarshal(body, &ModelRequest)），encoding/json 对字段名
// 先精确匹配、再按 Unicode 简单折叠（foldName）匹配，故 `Model`/`MODEL`/`mOdel`/`MoDeL` 等任意
// 大小写变体都会命中 `json:"model"`。jsonparser 的键查找是精确字节匹配，若不显式折叠会静默把
// model 丢成 ""，使 distributor 将空 model 置为 "auto" 并改变渠道选路。
//
// 值选取：encoding/json 按键在文档中的出现顺序逐键赋值，故大小写变体之间是 last-wins
// （`{"model":"a","Model":"b"}` → "b"）。字节完全相同的重复键则保持 jsonparser 的 first-wins
// （已声明的已知差异）。
//
// 注意：`stream` **不**在本测试范围内。stream 与 model 的键匹配语义刻意不对称（stream 精确匹配，
// 见 buildRequestBodyMetadata 注释与 TestGetRequestModelStreamKeyExactMatch）。
func TestGetRequestModelCaseInsensitiveKeys(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantModel string
		wantErr   bool
	}{
		// 单变体：全部命中 json:"model"。
		{"canonical model", `{"model":"x"}`, "x", false},
		{"capitalized model", `{"Model":"x"}`, "x", false},
		{"upper model", `{"MODEL":"x"}`, "x", false},
		{"mixed model", `{"mOdel":"x"}`, "x", false},
		{"alternating model", `{"MoDeL":"x"}`, "x", false},
		// 多大小写变体共存：按文档序 last-wins（与 encoding/json 一致）。
		{"canonical then capitalized", `{"model":"a","Model":"b"}`, "b", false},
		{"capitalized then canonical", `{"Model":"b","model":"a"}`, "a", false},
		{"upper then canonical", `{"MODEL":"b","model":"a"}`, "a", false},
		{"canonical then upper", `{"model":"a","MODEL":"b"}`, "b", false},
		// 字节完全相同的重复键：保持 jsonparser first-wins（已声明差异，非 encoding/json 语义）。
		{"exact duplicate first-wins", `{"model":"a","model":"b"}`, "a", false},
		// null 变体：合法零值，Valid 保持 true。
		{"null then capitalized", `{"model":null,"Model":"b"}`, "b", false},
		{"capitalized null then canonical", `{"Model":null,"model":"a"}`, "a", false},
		// 类型不符仍必须失败：大小写变体不得绕过 R2 类型校验。
		{"capitalized model number", `{"Model":123}`, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			modelName, err := getRequestModel(c)

			if tc.wantErr {
				require.Error(t, err, "类型不符的 model 必须返回 error")
				assert.Equal(t, "", modelName, "提取失败时不得产出 model 值")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantModel, modelName)
			}

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok)
			assert.True(t, metadata.WellFormed)
			assert.Equal(t, !tc.wantErr, metadata.ModelValid, "ModelValid 必须与错误路径一致")
			assert.Equal(t, tc.wantModel, metadata.Model)

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c))
		})
	}
}

// TestGetRequestModelStreamKeyExactMatch 锁定 stream 键匹配**大小写敏感**（只认精确键名 `stream`），
// 与 model 的大小写不敏感刻意不对称。
//
// 理由：改造前 stream 的消费方全部用精确键查找——detectStreamFromBody 用 bodyMap["stream"]，
// relayResponsesDirect / relayResponsesConverted 用 req["stream"]，故 `Stream`/`STREAM`/`sTream`
// 等变体（含 Unicode 折叠变体 `ſtream`）一律视为键缺失。任务 2.3 要求 detectStreamFromBody 优先读
// 共享缓存、缺失时回退 jsonparser.GetBoolean(body, "stream")（精确匹配），并验收两条路径结果一致；
// 若此处大小写不敏感，`{"Stream":true}` 缓存命中得 true、回退得 false，两路径不一致。
func TestGetRequestModelStreamKeyExactMatch(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		wantStream      bool
		wantStreamValid bool
	}{
		// 精确小写键：唯一被识别的键名。
		{"exact lowercase true", `{"stream":true}`, true, true},
		{"exact lowercase false", `{"stream":false}`, false, true},
		// 大小写/Unicode 折叠变体：一律键缺失 → 合法零值（StreamValid=true），不得命中。
		{"capitalized", `{"Stream":true}`, false, true},
		{"upper", `{"STREAM":true}`, false, true},
		{"mixed", `{"sTream":true}`, false, true},
		{"long s fold", `{"ſtream":true}`, false, true},
		// 类型不符：存在但非布尔 → StreamValid=false（区别于键缺失）。
		{"string type", `{"stream":"yes"}`, false, false},
		{"number type", `{"stream":123}`, false, false},
		// null：显式零值，StreamValid 保持 true。
		{"null value", `{"stream":null}`, false, true},
		// 字节完全相同的重复键：first-wins（已声明差异，非 encoding/json last-wins）。
		{"exact duplicate first-wins", `{"stream":true,"stream":false}`, true, true},
		// 变体在前、精确键在后：变体被忽略，精确键生效。
		{"variant then exact", `{"Stream":false,"stream":true}`, true, true},
		// 精确键在前、变体在后：变体被忽略，精确键保持 first-wins。
		{"exact then variant", `{"stream":true,"Stream":false}`, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			_, err := getRequestModel(c)
			require.NoError(t, err, "stream 键语义不影响 model 提取的成功路径")

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok)
			assert.True(t, metadata.WellFormed)
			assert.Equal(t, tc.wantStream, metadata.Stream, "stream 值必须与精确匹配预期一致")
			assert.Equal(t, tc.wantStreamValid, metadata.StreamValid, "StreamValid 必须区分类型不符与键缺失")

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c))
		})
	}
}

// TestGetRequestModelMatchesEncodingJSONCaseMatrix 用 encoding/json 反序列化到等价 struct 作为
// 改造前基线，逐行比对大小写矩阵的 model，证明新实现复现旧语义。
//
// 仅覆盖 model：改造前的 `ModelRequest` struct 只声明了 model（没有 stream 字段），且新实现的
// stream 键匹配刻意改为精确匹配（与 encoding/json 的大小写不敏感不再对齐），故 stream 不在此对照
// 范围内，由 TestGetRequestModelStreamKeyExactMatch 单独锁定。
//
// 排除项：字节完全相同的重复键（jsonparser first-wins vs encoding/json last-wins）为已声明差异，
// 由 TestGetRequestModelCaseInsensitiveKeys 的 "exact duplicate first-wins" 单独锁定。
func TestGetRequestModelMatchesEncodingJSONCaseMatrix(t *testing.T) {
	type baseline struct {
		Model string `json:"model"`
	}

	bodies := []string{
		`{"model":"x"}`,
		`{"Model":"x"}`,
		`{"MODEL":"x"}`,
		`{"mOdel":"x"}`,
		`{"MoDeL":"x"}`,
		`{"model":"a","Model":"b"}`,
		`{"Model":"b","model":"a"}`,
		`{"MODEL":"b","model":"a"}`,
		`{"model":"a","MODEL":"b"}`,
		`{"Model":"x","Stream":true}`,
		`{"model":null,"Model":"b"}`,
		`{"Model":null,"model":"a"}`,
		`{}`,
		`null`,
	}

	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			var want baseline
			require.NoError(t, json.Unmarshal([]byte(body), &want), "基线输入必须是合法 JSON")

			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", body)
			modelName, err := getRequestModel(c)
			require.NoError(t, err)

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok)

			assert.Equal(t, want.Model, modelName, "model 必须与 encoding/json 基线一致")
			assert.Equal(t, want.Model, metadata.Model)
		})
	}
}

// TestGetRequestModelForm 覆盖非 JSON 的 form-urlencoded 分支：必须继续走 ShouldBind
// （而非 jsonparser），且调用后 body 仍可被下游完整读取。
//
// 分支判定证据：form body `model=...` 不是合法 JSON，若误入 JSON 分支，json.Valid 预检会
// 直接报错。此处断言 err == nil，即证明走的是非 JSON 分支。选用无默认值的
// /v1/chat/completions，避免路由默认值掩盖分支判定。
//
// 现状说明（刻意保留，非本次引入）：common.UnmarshalBodyReusable 非 JSON 分支调用
// c.ShouldBind(&v)，其中 v 为 any，形成 **ModelRequest 间接层，gin 的反射绑定会静默跳过
// 字段，故 form 输入的 model 实取为空串。契约要求「保留非 JSON 走 ShouldBind 的行为」，
// 修复该历史缺陷属行为变更、超出 2.1 范围，故此处锁定现状，不改变控制流。
func TestGetRequestModelForm(t *testing.T) {
	body := "model=whisper-1&language=en"
	c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/x-www-form-urlencoded", body)

	modelName, err := getRequestModel(c)

	require.NoError(t, err, "非 JSON 分支不应报错（证明未走 json.Valid 预检的 JSON 分支）")
	assert.Equal(t, "", modelName, "锁定现状：form 分支经 ShouldBind 后 model 为空串")
	_, cached := ctxkey.GetRequestBodyMetadata(c)
	assert.False(t, cached, "非 JSON 分支不得写入 JSON 元数据缓存")
	assert.Equal(t, []byte(body), readRemainingBody(t, c))
}

// TestGetRequestModelMultipart 覆盖非 JSON 的 multipart/form-data 分支：必须继续走 ShouldBind，
// 且调用后 body 仍可被下游完整读取。
func TestGetRequestModelMultipart(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("model", "dall-e-3"))
	require.NoError(t, writer.Close())

	c := newGetRequestModelContext(t, http.MethodPost, "/v1/images/generations", writer.FormDataContentType(), buf.String())

	modelName, err := getRequestModel(c)

	require.NoError(t, err, "非 JSON 分支不应报错（证明未走 JSON 分支）")
	assert.Equal(t, "dall-e-2", modelName, "images 路由默认值保持既有行为")
	_, cached := ctxkey.GetRequestBodyMetadata(c)
	assert.False(t, cached, "非 JSON 分支不得写入 JSON 元数据缓存")
	assert.Equal(t, buf.Bytes(), readRemainingBody(t, c))
}

// TestGetRequestModelRouteDefaults 覆盖路由默认值：JSON 请求未带 model 时，
// moderation / embeddings 的既有默认值行为必须保持不变。
func TestGetRequestModelRouteDefaults(t *testing.T) {
	t.Run("moderation default", func(t *testing.T) {
		c := newGetRequestModelContext(t, http.MethodPost, "/v1/moderations", "application/json", `{"input":"hello"}`)
		modelName, err := getRequestModel(c)
		require.NoError(t, err)
		assert.Equal(t, "text-moderation-stable", modelName)
	})

	t.Run("embeddings default from path param", func(t *testing.T) {
		c := newGetRequestModelContext(t, http.MethodPost, "/v1/engines/text-embedding-3-small/embeddings", "application/json", `{"input":"hello"}`)
		c.Params = gin.Params{{Key: "model", Value: "text-embedding-3-small"}}
		modelName, err := getRequestModel(c)
		require.NoError(t, err)
		assert.Equal(t, "text-embedding-3-small", modelName)
	})

	t.Run("images default", func(t *testing.T) {
		c := newGetRequestModelContext(t, http.MethodPost, "/v1/images/generations", "application/json", `{"prompt":"a cat"}`)
		modelName, err := getRequestModel(c)
		require.NoError(t, err)
		assert.Equal(t, "dall-e-2", modelName)
	})
}

// TestGetRequestModelMalformedJSON 覆盖畸形 JSON 的失败路径（AC-1）：未闭合对象 / 未闭合字符串 /
// trailing garbage / 空 body / 纯空白。json.Valid 预检必须拦截，绝不从半截 JSON 取值：
// 返回 error、model 为空串、metadata 全零值（不变式：WellFormed=false 时其余字段必须为零值）。
// 无论成功或失败都必须恢复 c.Request.Body，下游仍需逐字节重读完整请求体。
func TestGetRequestModelMalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unclosed object", `{"model":"x"`},
		{"unclosed string", `{"model":"x`},
		{"trailing garbage", `{"model":"x"} extra`},
		{"empty body", ``},
		{"whitespace only", `   `},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			modelName, err := getRequestModel(c)

			require.Error(t, err, "畸形 JSON 必须返回 error，绝不从半截 JSON 取值")
			assert.ErrorIs(t, err, errRequestBodyNotWellFormed, "畸形 JSON 必须归为 not-well-formed 失败原因")
			assert.Equal(t, "", modelName, "提取失败时不得产出 model 值")

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok, "JSON 分支必须写入共享元数据缓存（含失败结论）")
			assert.False(t, metadata.WellFormed, "畸形 JSON 必须使 WellFormed=false")
			// 不变式：WellFormed=false 要求其余字段全部为零值。
			assert.False(t, metadata.ModelValid)
			assert.False(t, metadata.StreamValid)
			assert.Equal(t, "", metadata.Model)
			assert.False(t, metadata.Stream)

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c), "下游必须能逐字节重读完整 body")
		})
	}
}

// TestGetRequestModelModelWrongType 覆盖 model 存在但类型非法（设计规则 R2）：数字 / 数组 / 对象 / 布尔。
// json.Valid 只保证 well-formedness，拦不住类型不匹配（{"model":123} 是合法 JSON），故必须由类型化
// 读取显式分支：返回 error、model 为空串、ModelValid=false，绝不从原始字节派生误导值。
// WellFormed 仍为 true（JSON 本身合法），以此区分「类型不符」与「畸形 JSON」。
func TestGetRequestModelModelWrongType(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"number", `{"model":123}`},
		{"array", `{"model":[1]}`},
		{"object", `{"model":{}}`},
		{"boolean", `{"model":true}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			modelName, err := getRequestModel(c)

			require.Error(t, err, "model 类型非法必须返回 error，不产出误导值")
			assert.ErrorIs(t, err, errRequestBodyModelType, "类型不符必须归为 model-type 失败原因")
			assert.Equal(t, "", modelName, "提取失败时不得产出 model 值")

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok, "JSON 分支必须写入共享元数据缓存")
			assert.True(t, metadata.WellFormed, "类型不符的 JSON 本身仍 well-formed")
			assert.False(t, metadata.ModelValid, "类型不符必须使 ModelValid=false")
			assert.Equal(t, "", metadata.Model, "不得从非字符串值派生 model")

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c), "下游必须能逐字节重读完整 body")
		})
	}
}

// TestGetRequestModelModelEmptyString 覆盖 model 为空字符串：空串是**合法的字符串值**，
// 既非键缺失也非类型错误，故不报错、ModelValid 保持 true，model 为零值 ""。
func TestGetRequestModelModelEmptyString(t *testing.T) {
	body := `{"model":""}`
	c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", body)

	modelName, err := getRequestModel(c)

	require.NoError(t, err, "空字符串是合法字符串值，不得报错")
	assert.Equal(t, "", modelName)

	metadata, ok := ctxkey.GetRequestBodyMetadata(c)
	require.True(t, ok)
	assert.True(t, metadata.WellFormed)
	assert.True(t, metadata.ModelValid, "空串是合法字符串值，ModelValid 必须保持 true")
	assert.Equal(t, "", metadata.Model)

	assert.Equal(t, []byte(body), readRemainingBody(t, c))
}

// TestGetRequestModelDuplicateModelFirstWins 锁定字节相同的重复 model 键取**第一个**（first-wins）。
//
// 这是 jsonparser 的确定性行为，与 encoding/json 的 **last-wins** 刻意不同：
// encoding/json 按键在文档中的出现顺序逐键赋值，`{"model":"first","model":"second"}` 会得到
// "second"；jsonparser.ObjectEach 按序回调，本实现用 modelKey 记录当前胜出键的原始字节，
// 遇到键名字节完全相同的重复键时跳过（first-wins）。
//
// 注意差异边界：该 first-wins 仅限「键名字节完全相同」的重复键；model 的不同大小写变体之间
// 仍按文档序 last-wins（由 TestGetRequestModelCaseInsensitiveKeys 锁定），以对齐 encoding/json
// 的字段匹配语义。
//
// 转义拼写 `\u006dodel` 解码后与字面 `model` 相同，同样视为重复键 → first-wins。
func TestGetRequestModelDuplicateModelFirstWins(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"exact duplicate", `{"model":"first","model":"second"}`},
		{"escaped duplicate", `{"model":"first","\u006dodel":"second"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newGetRequestModelContext(t, http.MethodPost, "/v1/chat/completions", "application/json", tc.body)

			modelName, err := getRequestModel(c)

			require.NoError(t, err)
			assert.Equal(t, "first", modelName, "重复 model 键必须取第一个（first-wins，与 encoding/json last-wins 不同）")

			metadata, ok := ctxkey.GetRequestBodyMetadata(c)
			require.True(t, ok)
			assert.True(t, metadata.WellFormed)
			assert.True(t, metadata.ModelValid)
			assert.Equal(t, "first", metadata.Model)

			assert.Equal(t, []byte(tc.body), readRemainingBody(t, c), "下游必须能逐字节重读完整 body")
		})
	}
}

func TestIsModelInListAliasExact(t *testing.T) {
	Convey("isModelInList with alias exact match", t, func() {
		// modelName="gpt4turbo" (already simplified), models="gpt4turbo,gpt35turbo"
		So(isModelInList("gpt4turbo", "gpt4turbo,gpt35turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListRejectsPrefix(t *testing.T) {
	Convey("isModelInList must not match by prefix", t, func() {
		// modelName="gpt-4" simplifies to "gpt4"; "gpt4turbo" merely starts with it,
		// prefix match would let a token restricted to a cheap model reach a pricier one
		So(isModelInList("gpt-4", "gpt4turbo,gpt35turbo"), ShouldBeFalse)
	})
}

func TestIsModelInListEmptyModels(t *testing.T) {
	Convey("isModelInList with empty models returns true", t, func() {
		// empty models means no restriction
		So(isModelInList("gpt4", ""), ShouldBeTrue)
	})
}

func TestIsModelInListUnknownModel(t *testing.T) {
	Convey("isModelInList with unknown model returns false", t, func() {
		// model not in the list
		So(isModelInList("claude3", "gpt4turbo,gpt35turbo"), ShouldBeFalse)
	})
}

func TestIsModelInListAuto(t *testing.T) {
	Convey("isModelInList with auto returns true", t, func() {
		// auto models should pass through
		So(isModelInList("auto", "gpt4turbo,gpt35turbo"), ShouldBeTrue)
		So(isModelInList("auto", ""), ShouldBeTrue)
	})
}

// ─── Original (unsimplified) model names in stored list ───
// After the bugfix, Token.Models now stores the original model name as-is
// (e.g. "gpt-4-turbo" instead of "gpt4turbo"). These tests verify
// isModelInList still works with unsimplified names in the stored list.

func TestIsModelInListOriginalNameExact(t *testing.T) {
	Convey("original name in stored list matches exact", t, func() {
		// Stored models contain original names with special chars
		So(isModelInList("gpt-4-turbo", "gpt-4-turbo,gpt-3.5-turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListOriginalNamePrefix(t *testing.T) {
	Convey("simplified request must not match prefix of original stored name", t, func() {
		// Request "gpt-4" simplifies to "gpt4", stored "gpt-4-turbo" simplifies to "gpt4turbo";
		// "gpt4" being a prefix of "gpt4turbo" must NOT grant access
		So(isModelInList("gpt-4", "gpt-4-turbo,gpt-3.5-turbo"), ShouldBeFalse)
	})
}

func TestIsModelInListOriginalNameUnknown(t *testing.T) {
	Convey("unknown model returns false with original names", t, func() {
		So(isModelInList("claude-3", "gpt-4-turbo,gpt-3.5-turbo"), ShouldBeFalse)
	})
}

func TestIsModelInListOriginalNameCaseInsensitive(t *testing.T) {
	Convey("matching is case-insensitive", t, func() {
		So(isModelInList("GPT-4-Turbo", "gpt-4-turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListMixedOldNewData(t *testing.T) {
	Convey("mixed old (simplified) and new (original) data", t, func() {
		// Backward compat: old tokens stored simplified names, new tokens store original names
		// Both must work via exact match after simplification
		So(isModelInList("gpt-4-turbo", "gpt4turbo,gpt-3.5-turbo"), ShouldBeTrue)
		So(isModelInList("gpt-3.5-turbo", "gpt-4-turbo,gpt35turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListNoPrefixPrivilegeEscalation(t *testing.T) {
	Convey("token restricted to o3-mini must not access o3", t, func() {
		// 回归：请求名是允许名的真前缀时必须拒绝，防止越权到更贵模型
		So(isModelInList("o3", "o3-mini"), ShouldBeFalse)
		// 反方向同理：允许名是请求名的真前缀时也必须拒绝
		So(isModelInList("o3-mini", "o3"), ShouldBeFalse)
	})
}

// =============================================================================
// 任务 7.3：JSON 热路径基准 —— 大请求 model/stream 读取（含改造前/后对比）
// =============================================================================

// benchmarkRequestMetadataBody 生成约 300KB 的良构 chat 请求体，规模足以暴露
// 「为取 1~2 个字段而全量反序列化」的开销（对齐 proposal 中 465KB body 的实测场景）。
func benchmarkRequestMetadataBody() []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-4-turbo","stream":true,"messages":[`)
	for i := 0; i < 1500; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"role":"user","content":"`)
		sb.WriteString(strings.Repeat("x", 180))
		sb.WriteString(`"}`)
	}
	sb.WriteString(`],"temperature":0.7,"max_tokens":4096}`)
	return []byte(sb.String())
}

// BenchmarkJsonParserHotPathRequestMetadata 对比改造前后「每请求一次」的元数据提取开销：
//
//   - before_two_full_unmarshal：改造前 getRequestModel（encoding/json → ModelRequest）
//     与 detectStreamFromBody（encoding/json → map[string]any）各自全量反序列化一次，
//     同一份 body 被解码 2 次；
//   - after_one_valid_plus_on_demand：改造后共享一次 json.Valid 预检 + 一次
//     jsonparser.ObjectEach 单遍扫描，同时得出 model 与 stream 结论（共享缓存）。
//
// 契约 7.3 / AC-5：至少一项（ns/op 或 allocs/op）必须改善；两者皆退化才需记录的测量证据。
func BenchmarkJsonParserHotPathRequestMetadata(b *testing.B) {
	body := benchmarkRequestMetadataBody()

	b.Run("before_two_full_unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			var modelRequest ModelRequest
			_ = json.Unmarshal(body, &modelRequest)
			var bodyMap map[string]any
			_ = json.Unmarshal(body, &bodyMap)
			stream, _ := bodyMap["stream"].(bool)
			_ = modelRequest.Model
			_ = stream
		}
	})

	b.Run("after_one_valid_plus_on_demand", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			metadata := buildRequestBodyMetadata(body)
			_ = metadata.Model
			_ = metadata.Stream
		}
	})
}
