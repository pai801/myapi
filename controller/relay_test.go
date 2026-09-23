package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRetry(t *testing.T) {
	Convey("shouldRetry decisions", t, func() {
		newBizErr := func(statusCode int, errType string, code any, message string) *model.ErrorWithStatusCode {
			return &model.ErrorWithStatusCode{
				StatusCode: statusCode,
				Error: model.Error{
					Type:    errType,
					Code:    code,
					Message: message,
				},
			}
		}

		Convey("returns false when SpecificChannelId is set", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.SpecificChannelId, "42")
			So(shouldRetry(c, newBizErr(http.StatusInternalServerError, "server_error", nil, "upstream timeout")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusTooManyRequests, "rate_limit_error", nil, "quota exceeded")), ShouldBeFalse)
		})

		Convey("returns true for 429 TooManyRequests", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusTooManyRequests, "rate_limit_error", nil, "quota exceeded")), ShouldBeTrue)
		})

		Convey("returns true for 5xx errors", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusInternalServerError, "server_error", nil, "upstream timeout")), ShouldBeTrue)
			So(shouldRetry(c, newBizErr(http.StatusBadGateway, "server_error", nil, "bad gateway")), ShouldBeTrue)
			So(shouldRetry(c, newBizErr(http.StatusServiceUnavailable, "server_error", nil, "service unavailable")), ShouldBeTrue)
		})

		Convey("returns false for 400 request-shape failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "malformed_request", "Malformed request body")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "unsupported_request", "unsupported request schema")), ShouldBeFalse)
		})

		Convey("returns true for 400 provider-specific compatibility failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "model_not_supported", "model is not supported by this provider channel")), ShouldBeTrue)
		})

		Convey("returns false for 2xx business-success responses", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusOK, "", nil, "business success but no retry")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusCreated, "", nil, "created")), ShouldBeFalse)
		})

		Convey("returns false for 2xx generic upstream errors without adapter-failure evidence", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusOK, "upstream_error", "upstream_error", "upstream rejected business request")), ShouldBeFalse)
		})

		Convey("returns true for adapter parse or bad-response failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusOK, "upstream_error", "bad_response", "upstream returned malformed response payload")), ShouldBeTrue)
			So(shouldRetry(c, newBizErr(http.StatusOK, "upstream_error", "bad_response_status_code", "adapter failed to parse upstream response")), ShouldBeTrue)
		})

		Convey("returns false for contract errors without alternate-channel signal", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "generic_client_error", "client contract error")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid api key")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusForbidden, "permission_error", "forbidden", "forbidden")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusProxyAuthRequired, "proxy_auth_error", "proxy_auth_required", "proxy authentication required")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported_media_type", "unsupported media type")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusUnprocessableEntity, "invalid_request_error", "invalid_schema", "schema validation failed")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusConflict, "conflict_error", "conflict", "provider rejected current request state")), ShouldBeFalse)
		})

		Convey("returns true for contract errors with alternate-channel compatibility signal", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusUnprocessableEntity, "invalid_request_error", "unsupported_model", "model is unsupported by this provider")), ShouldBeTrue)
		})

		Convey("returns false for unknown errors that look like request-shape failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(499, "", nil, "invalid request format")), ShouldBeFalse)
		})
	})
}

func TestProcessChannelRelayErrorLogDecision(t *testing.T) {
	// This test verifies the log output of processChannelRelayError
	// without triggering DB side effects.
	//
	// processChannelRelayError calls ShouldDisableChannel (which depends on
	// config.AutomaticDisableChannelEnabled), then either DisableChannel
	// (DB write) or Emit (goroutine) + CooldownGlobal.ReportFailure (in-memory).
	//
	// Since DisableChannel requires DB access, we test the path where
	// AutomaticDisableChannelEnabled is false, which triggers the cooldown path.
	Convey("processChannelRelayError with disable disabled goes to cooldown path", t, func() {
		// DisableChannel is disabled by default, so ShouldDisableChannel returns false
		// and we hit the cooldown path (Emit + CooldownGlobal.ReportFailure)
		err := model.ErrorWithStatusCode{
			Error: model.Error{
				Message: "test error",
				Type:    "test_error",
				Code:    "test_code",
			},
			StatusCode: 500,
		}

		// This should not panic — goes to cooldown path
		So(func() {
			processChannelRelayError(context.Background(), 1, 1, "test-ch", "test-model", err)
		}, ShouldNotPanic)
	})
}

// TestShouldRetry_CommittedResponseSuppressesRetry 锁定契约 §1.3：SSE 响应已提交后不得重试，
// 否则会向同一响应追加第二段流。
func TestShouldRetry_CommittedResponseSuppressesRetry(t *testing.T) {
	Convey("响应已提交后 shouldRetry 必须为 false", t, func() {
		bizErr := &model.ErrorWithStatusCode{
			StatusCode: http.StatusBadGateway,
			Error:      model.Error{Message: "responses stream failed", Type: "upstream_error", Code: "invalid_upstream_response"},
		}

		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		// 未提交：5xx 语义允许重试（保持既有行为）
		So(shouldRetry(c, bizErr), ShouldBeTrue)

		// 一旦写出 body（SSE 已提交），不得再重试
		_, _ = c.Writer.WriteString("data: {\"error\":\"x\"}\n\n")
		So(c.Writer.Written(), ShouldBeTrue)
		So(shouldRetry(c, bizErr), ShouldBeFalse)
	})
}

// TestResponseCommittedDetection_WriteHeaderAloneIsNotCommitted 记录 gin Written() 语义（本修复的判定依据）：
// 仅 WriteHeader 不置位（非流式只写状态码时不得据此抑制重试/JSON），写出 body 或 Flush 后才算已提交。
// 两条失败路径（codex StreamResponsesHandler handler.go:163、chatgptsub streamChatFromResponses main.go:261）
// 均在 WriteHeader 后立即调用 c.Writer.Flush()，Flush 内部 WriteHeaderNow 会置位 Written()，故返回错误时可靠。
func TestResponseCommittedDetection_WriteHeaderAloneIsNotCommitted(t *testing.T) {
	Convey("gin Written() 判定", t, func() {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		So(c.Writer.Written(), ShouldBeFalse)

		c.Writer.WriteHeader(http.StatusOK)
		So(c.Writer.Written(), ShouldBeFalse)

		_, _ = c.Writer.WriteString("data: {\"error\":\"x\"}\n\n")
		So(c.Writer.Written(), ShouldBeTrue)
	})
}

// ─── detectStreamFromBody（任务 2.3 / 2.4）───

// newDetectStreamContext 构造带请求体的 *gin.Context，用于覆盖 detectStreamFromBody 的回退路径。
// body 为空串时 reader 为 nil（httptest 会置为 http.NoBody），用于覆盖「空 body」场景。
func newDetectStreamContext(t *testing.T, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", reader)
	return c
}

// newDetectStreamCacheContext 构造「缓存命中」上下文：写入一个具体非指针的 RequestBodyMetadata。
// wellFormed 与 stream 由用例给出，模拟 middleware 阶段（getRequestModel）的写入结论。
func newDetectStreamCacheContext(t *testing.T, body string, stream bool, wellFormed bool) *gin.Context {
	t.Helper()
	c := newDetectStreamContext(t, body)
	c.Set(ctxkey.KeyRequestBodyMetadata, ctxkey.RequestBodyMetadata{
		WellFormed:  wellFormed,
		ModelValid:  wellFormed,
		Stream:      stream,
		StreamValid: wellFormed,
	})
	return c
}

// referenceStreamFromBody 复刻改造前 detectStreamFromBody 的 encoding/json 语义，作为独立对照基线。
// 仅用于锁定非重复键用例的等价性；重复键是已声明的 first-wins 差异，不适用本基线。
func referenceStreamFromBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var bodyMap map[string]any
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		return false
	}
	if stream, ok := bodyMap["stream"]; ok {
		if b, ok := stream.(bool); ok {
			return b
		}
	}
	return false
}

// TestDetectStreamFromBody_CacheHitAndFallbackAgree 锁定任务 2.3 的核心验收：缓存命中与回退
// 两条路径对同一 body 结果一致，且与改造前 encoding/json 语义等价（非重复键用例）。
//
// 独立基线 referenceStreamFromBody 直接复刻改造前实现，故本测试同时验证：
//   - 回退路径（GetRequestBody + json.Valid + jsonparser.GetBoolean）复现旧语义；
//   - 缓存命中路径直接返回缓存结论（缓存由 middleware 按同一不变式写入）；
//   - 大小写变体 `{"Stream":true}`、Unicode 折叠变体 `{"ſtream":true}` 在两条路径下均为 false。
func TestDetectStreamFromBody_CacheHitAndFallbackAgree(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"exact true", `{"stream":true}`},
		{"exact false", `{"stream":false}`},
		{"with model true", `{"model":"gpt-4o","stream":true}`},
		{"with model false", `{"model":"gpt-4o","stream":false}`},
		{"empty object", `{}`},
		{"stream null", `{"stream":null}`},
		{"stream string", `{"stream":"true"}`},
		{"stream number", `{"stream":1}`},
		{"stream object", `{"stream":{}}`},
		{"stream array", `{"stream":[]}`},
		{"capitalized key", `{"Stream":true}`},
		{"upper key", `{"STREAM":true}`},
		{"long s fold key", `{"ſtream":true}`},
		{"malformed unclosed object", `{"stream":true`},
		{"malformed unclosed string", `{"stream":"x`},
		{"trailing garbage", `{"stream":true} extra`},
		{"empty body", ``},
		{"whitespace only", `   `},
		{"array root", `[1,2]`},
		{"string root", `"hello"`},
		{"number root", `123`},
		{"boolean root", `true`},
		{"null root", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 独立基线：改造前 encoding/json 语义。
			want := referenceStreamFromBody([]byte(tc.body))

			fallbackCtx := newDetectStreamContext(t, tc.body)
			gotFallback := detectStreamFromBody(fallbackCtx)
			require.Equal(t, want, gotFallback, "回退路径必须复现改造前 encoding/json 语义")

			cacheCtx := newDetectStreamCacheContext(t, tc.body, want, json.Valid([]byte(tc.body)))
			gotCache := detectStreamFromBody(cacheCtx)
			require.Equal(t, want, gotCache, "缓存命中路径必须返回缓存结论")

			assert.Equal(t, gotFallback, gotCache, "缓存命中与回退两条路径结果必须一致")

			// body 可读性：回退路径只读不恢复，但字节已缓存于 ctxkey.KeyRequestBody，
			// 再次调用必须幂等（命中缓存，不再消费 c.Request.Body）。
			assert.Equal(t, gotFallback, detectStreamFromBody(fallbackCtx), "重复调用必须幂等")
		})
	}
}

// TestDetectStreamFromBody_CacheHitIgnoresBodyAvailability 锁定任务 2.3 的「缓存命中不依赖
// body 可用性」：缓存 ok=true 时直接返回缓存结论，绝不重新校验 body。
// 用一个解析结果为 false 的 body（甚至空 body）配合 Stream=true 的缓存，验证缓存权威性。
func TestDetectStreamFromBody_CacheHitIgnoresBodyAvailability(t *testing.T) {
	t.Run("cache true with empty-object body", func(t *testing.T) {
		c := newDetectStreamCacheContext(t, `{}`, true, true)
		assert.True(t, detectStreamFromBody(c), "缓存命中必须直接返回缓存结论，不重新校验 body")
	})

	t.Run("cache true with empty body", func(t *testing.T) {
		c := newDetectStreamCacheContext(t, ``, true, true)
		assert.True(t, detectStreamFromBody(c), "body 不可用时缓存结论仍然权威")
	})

	t.Run("cache false with stream-true body", func(t *testing.T) {
		// 缓存携带权威的 WellFormed=false 结论（如畸形 body），即使 body 字面像 true 也必须为 false。
		c := newDetectStreamCacheContext(t, `{"stream":true}`, false, false)
		assert.False(t, detectStreamFromBody(c), "缓存携带失败结论时不得回退重解析 body")
	})
}

// TestDetectStreamFromBody_StaleCacheFallsBack 锁定任务 2.4 的「stale-shaped 缓存值触发回退而非
// panic」：缺失键、字符串值、指针值、异构 struct 值均使 GetRequestBodyMetadata 返回 ok=false，
// detectStreamFromBody 必须回退自解析，不得 panic。
func TestDetectStreamFromBody_StaleCacheFallsBack(t *testing.T) {
	// staleMetadata 与 ctxkey.RequestBodyMetadata 形状不同，类型断言必失败。
	type staleMetadata struct {
		Stream bool
	}

	t.Run("string value", func(t *testing.T) {
		c := newDetectStreamContext(t, `{"stream":true}`)
		c.Set(ctxkey.KeyRequestBodyMetadata, "not-a-metadata")
		assert.True(t, detectStreamFromBody(c), "类型不符必须回退自解析")
	})

	t.Run("pointer value", func(t *testing.T) {
		c := newDetectStreamContext(t, `{"stream":true}`)
		metadata := ctxkey.RequestBodyMetadata{WellFormed: true, Stream: false}
		c.Set(ctxkey.KeyRequestBodyMetadata, &metadata)
		assert.True(t, detectStreamFromBody(c), "指针值必须回退自解析（缓存只接受具体值）")
	})

	t.Run("stale-shaped struct value", func(t *testing.T) {
		c := newDetectStreamContext(t, `{"stream":false}`)
		c.Set(ctxkey.KeyRequestBodyMetadata, staleMetadata{Stream: true})
		assert.False(t, detectStreamFromBody(c), "异构 struct 必须回退自解析，不得误用其字段")
	})

	t.Run("nil context does not panic", func(t *testing.T) {
		// GetRequestBodyMetadata(nil) 返回 ok=false，随后 GetRequestBody(nil) 会 panic；
		// 故此处只验证不因 nil 上下文在缓存读取阶段 panic —— 通过 recover 观察。
		// 契约只要求缓存读取失败不 panic，不要求对 nil 上下文做业务兜底。
		require.NotPanics(t, func() {
			_, ok := ctxkey.GetRequestBodyMetadata(nil)
			assert.False(t, ok)
		})
	})
}

// TestDetectStreamFromBody_DoesNotConsumeBodyWhenBodyCached 锁定 body 可读性：生产链路中
// middleware.TokenAuth 的 getRequestModel 已调用 common.GetRequestBody（读后显式 restore）并把
// 字节缓存进 ctxkey.KeyRequestBody，故 detectStreamFromBody 的回退路径命中该缓存、根本不触碰
// c.Request.Body，下游直读 body 的路由（proxy/audio/text/image）仍可完整读取。
//
// 本测试手工复刻 middleware 的缓存步骤（common.GetRequestBody 为导出函数，缓存键同一），
// 以验证 detectStreamFromBody 在「body 已缓存」时对 c.Request.Body 零消费。
func TestDetectStreamFromBody_DoesNotConsumeBodyWhenBodyCached(t *testing.T) {
	body := `{"model":"gpt-4o","stream":true}`
	c := newDetectStreamContext(t, body)

	// 复刻 middleware 阶段：GetRequestBody 缓存字节，并按 getRequestModel 的做法 restore body。
	cached, err := common.GetRequestBody(c)
	require.NoError(t, err)
	require.Equal(t, []byte(body), cached)
	c.Request.Body = io.NopCloser(bytes.NewReader(cached))

	// 缓存缺失（未写元数据）→ 走回退路径，但 body 字节已缓存，不消费 c.Request.Body。
	require.True(t, detectStreamFromBody(c), "回退路径应命中已缓存的 body 字节")

	remaining, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err, "下游必须仍能完整读取 body")
	assert.Equal(t, []byte(body), remaining, "detectStreamFromBody 不得消费 c.Request.Body")
}

// TestDetectStreamFromBody_Boundary 锁定任务 2.4：畸形 JSON、stream 缺失、stream 为字符串/数字/
// 对象/数组、空 body、非对象根均返回 false（与现状 `.(bool)` 类型断言行为一致），且缓存命中与
// 回退两条路径结果一致。
func TestDetectStreamFromBody_Boundary(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"malformed unclosed object", `{"stream":true`},
		{"malformed unclosed string", `{"stream":"x`},
		{"trailing garbage", `{"stream":true} extra`},
		{"empty body", ``},
		{"whitespace only", `   `},
		{"stream missing", `{"model":"gpt-4o"}`},
		{"stream null", `{"stream":null}`},
		{"stream string", `{"stream":"true"}`},
		{"stream number", `{"stream":1}`},
		{"stream object", `{"stream":{}}`},
		{"stream array", `{"stream":[]}`},
		{"array root", `[1,2]`},
		{"string root", `"hello"`},
		{"number root", `123`},
		{"boolean root", `true`},
		{"null root", `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fallbackCtx := newDetectStreamContext(t, tc.body)
			gotFallback := detectStreamFromBody(fallbackCtx)
			require.False(t, gotFallback, "所有边界输入必须返回 false")

			// 缓存命中路径：按 middleware 不变式，非法/缺失/畸形均折叠为 Stream=false。
			cacheCtx := newDetectStreamCacheContext(t, tc.body, false, json.Valid([]byte(tc.body)))
			gotCache := detectStreamFromBody(cacheCtx)
			require.False(t, gotCache, "缓存命中路径对边界输入同样必须返回 false")

			assert.Equal(t, gotFallback, gotCache, "两条路径结果必须一致")
		})
	}
}

// TestDetectStreamFromBody_DuplicateStreamFirstWins 锁定重复 stream 键取第一个（first-wins）。
//
// 这是 jsonparser 的确定性行为，与改造前 encoding/json 的 **last-wins** 刻意不同：
// encoding/json 逐键赋值取最后一个，故 `{"stream":true,"stream":false}` 旧实现得 false；
// 本实现（含 middleware 缓存的 buildRequestBodyMetadata）取第一个，得 true。
// 本测试同时断言 referenceStreamFromBody 得到相反值，以显式记录该已声明差异。
func TestDetectStreamFromBody_DuplicateStreamFirstWins(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"true then false", `{"stream":true,"stream":false}`, true},
		{"false then true", `{"stream":false,"stream":true}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fallbackCtx := newDetectStreamContext(t, tc.body)
			gotFallback := detectStreamFromBody(fallbackCtx)
			require.Equal(t, tc.want, gotFallback, "重复 stream 键必须取第一个（first-wins）")

			cacheCtx := newDetectStreamCacheContext(t, tc.body, tc.want, true)
			gotCache := detectStreamFromBody(cacheCtx)
			require.Equal(t, tc.want, gotCache, "缓存命中路径同为 first-wins")
			assert.Equal(t, gotFallback, gotCache, "两条路径结果必须一致")

			// 显式记录与 encoding/json（last-wins）的差异：基线取最后一个布尔值。
			assert.NotEqual(t, referenceStreamFromBody([]byte(tc.body)), tc.want,
				"encoding/json last-wins 结果必须与 first-wins 相反，以证明差异被锁定")
		})
	}

	t.Run("first non-boolean duplicate stays false", func(t *testing.T) {
		// 首个 stream 为字符串、第二个为 true：first-wins 下取首个 → 类型不符 → false。
		body := `{"stream":"x","stream":true}`
		fallbackCtx := newDetectStreamContext(t, body)
		require.False(t, detectStreamFromBody(fallbackCtx))

		// middleware 缓存对首个 stream 类型不符亦置 StreamValid=false、Stream=false，两路径一致。
		cacheCtx := newDetectStreamCacheContext(t, body, false, true)
		assert.False(t, detectStreamFromBody(cacheCtx))
	})
}

// TestRenderFinalRelayError_SkipsCommittedResponse 锁定契约 §1.3：最终 JSON 错误体只在响应
// 未提交时写出；已提交时不得向 SSE 响应追加任何内容。
func TestRenderFinalRelayError_SkipsCommittedResponse(t *testing.T) {
	bizErr := &model.ErrorWithStatusCode{
		StatusCode: http.StatusBadGateway,
		Error:      model.Error{Message: "responses stream failed", Type: "upstream_error", Code: "invalid_upstream_response"},
	}

	Convey("最终错误体渲染", t, func() {
		Convey("未提交：写出 502 JSON 错误体", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			renderFinalRelayError(c, bizErr)
			So(recorder.Code, ShouldEqual, http.StatusBadGateway)
			So(recorder.Body.String(), ShouldContainSubstring, `"error"`)
			So(recorder.Body.String(), ShouldContainSubstring, "invalid_upstream_response")
		})

		Convey("已提交：不追加任何内容", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			_, _ = c.Writer.WriteString("data: {\"error\":\"x\"}\n\n")
			committed := recorder.Body.String()
			renderFinalRelayError(c, bizErr)
			So(recorder.Body.String(), ShouldEqual, committed)
		})
	})
}
