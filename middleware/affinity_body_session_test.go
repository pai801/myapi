package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAffinityBodyContext 构造带请求体 / Content-Type / 请求头的 *gin.Context。
func newAffinityBodyContext(t *testing.T, method, contentType, body string, headers map[string]string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(method, "/v1/chat/completions", reader)
	if contentType != "" {
		c.Request.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	return c
}

// bodyCached 判断亲和解析是否真的读了（并缓存了）body。
func bodyCached(c *gin.Context) bool {
	_, ok := c.Get(ctxkey.KeyRequestBody)
	return ok
}

// --- 1. 四个稳定字段各自能从 body 取到会话标识 ---
// （previous_response_id 已被移除：它是每轮变化的上一轮响应 id，不能作 session 键，
//   见 affinity_scope.go 的 affinityBodySessionIDFields 注释与下方专门的负向用例。）

func TestResolveAffinityScope_SessionIDFromBody_Fields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"litellm_session_id", `{"litellm_session_id":"litellm-abc"}`, "litellm-abc"},
		{"session_id", `{"session_id":"sess-abc"}`, "sess-abc"},
		{"conversation_id", `{"conversation_id":"conv-abc"}`, "conv-abc"},
		{"metadata.cline_task_id", `{"metadata":{"cline_task_id":"task-xyz"}}`, "task-xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAffinityBodyContext(t, http.MethodPost, "application/json", tc.body, nil)

			scope := ResolveAffinityScope(c)

			assert.Equal(t, tc.want, scope.SessionID, "应能从 body 的 %s 取到会话标识", tc.name)
			assert.Equal(t, "", scope.TurnID, "body 不含 turn 语义，TurnID 必须为空")
		})
	}
}

// 优先级：litellm_session_id > session_id > conversation_id > metadata.cline_task_id。
func TestResolveAffinityScope_SessionIDFromBody_Priority(t *testing.T) {
	t.Run("litellm_session_id wins over session_id", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"litellm_session_id":"litellm-first","session_id":"sess-second"}`, nil)
		assert.Equal(t, "litellm-first", ResolveAffinityScope(c).SessionID)
	})

	t.Run("session_id wins over conversation_id", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"session_id":"sess-first","conversation_id":"conv-second"}`, nil)
		assert.Equal(t, "sess-first", ResolveAffinityScope(c).SessionID)
	})

	t.Run("conversation_id wins over metadata.cline_task_id", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"conversation_id":"conv-first","metadata":{"cline_task_id":"task-second"}}`, nil)
		assert.Equal(t, "conv-first", ResolveAffinityScope(c).SessionID)
	})

	t.Run("empty higher-priority falls through to next", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"litellm_session_id":"   ","session_id":"sess-real"}`, nil)
		assert.Equal(t, "sess-real", ResolveAffinityScope(c).SessionID,
			"高优先级字段为纯空白应视为缺失，继续尝试下一字段")
	})
}

// previous_response_id 绝不能遮蔽稳定的 litellm_session_id：两者同时出现时必须取后者。
// 这正是移除 previous_response_id 要修复的核心问题（见 affinity_scope.go 红线三）。
func TestResolveAffinityScope_PreviousResponseIDDoesNotShadowLitellm(t *testing.T) {
	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"previous_response_id":"resp-every-request","litellm_session_id":"litellm-stable"}`, nil)

	assert.Equal(t, "litellm-stable", ResolveAffinityScope(c).SessionID,
		"previous_response_id 每次请求都变，必须被忽略；稳定字段 litellm_session_id 必须取到")
}

// 只有 previous_response_id 时，session 层不得产生任何 body 会话键（不得拿它当键）。
func TestResolveAffinityScope_PreviousResponseIDOnlyYieldsNoSessionKey(t *testing.T) {
	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"previous_response_id":"resp-every-request","model":"gpt-4","messages":[]}`, nil)

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "", scope.SessionID,
		"previous_response_id 是每轮变化的上一轮响应 id，绝不能作为 session 键")

	// session 层无键：Keys() 中不得出现 AffinityLevelSession（只剩 user 兜底层）。
	for _, k := range scope.Keys("gpt-4") {
		assert.NotEqual(t, AffinityLevelSession, k.Level,
			"仅带 previous_response_id 时不应产出 session 层亲和键")
	}
}

// --- 2. 头号回归：读后 c.Request.Body 仍可读且逐字节完整 ---

func TestResolveAffinityScope_BodyStillReadableAfterResolve(t *testing.T) {
	// 模拟 proxy.go:27 的下游直读：亲和解析后直接 io.ReadAll(c.Request.Body) 必须拿到完整原文。
	original := `{"model":"gpt-4","litellm_session_id":"litellm-abc","messages":[{"role":"user","content":"hi"}]}`
	c := newAffinityBodyContext(t, http.MethodPost, "application/json", original, nil)

	scope := ResolveAffinityScope(c)
	require.Equal(t, "litellm-abc", scope.SessionID, "前置条件：确实从 body 取到了会话标识")

	got, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "亲和解析读 body 后必须恢复，下游直读必须逐字节相同")
}

// 重复解析（命中缓存）也应重建 body，下游仍能读到完整内容。
func TestResolveAffinityScope_BodyReadableAfterRepeatedResolve(t *testing.T) {
	original := `{"session_id":"sess-abc"}`
	c := newAffinityBodyContext(t, http.MethodPost, "application/json", original, nil)

	_ = ResolveAffinityScope(c)
	first, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	assert.Equal(t, original, string(first), "首次读取应拿到完整 body")

	_ = ResolveAffinityScope(c)
	second, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	assert.Equal(t, original, string(second), "再次解析（命中缓存）后 body 应仍可完整读取")
}

// --- 3. nil body 不 panic ---

func TestResolveAffinityScope_NilBodyDoesNotPanic(t *testing.T) {
	c := newAffinityBodyContext(t, http.MethodPost, "application/json", "", nil)
	c.Request.Body = nil

	require.NotPanics(t, func() {
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.SessionID, "body 为 nil 时应静默降级为无 session")
	})
}

// --- 4. 非 JSON / 非 body 方法时不读 body ---

func TestResolveAffinityScope_NonJSONBodyNotRead(t *testing.T) {
	body := `{"litellm_session_id":"should-not-be-read"}`

	t.Run("multipart/form-data", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "multipart/form-data; boundary=xyz", body, nil)
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.SessionID, "multipart 不应被读取")
		assert.False(t, bodyCached(c), "multipart 时不得写 body 缓存（证明未读）")
	})

	t.Run("no content-type", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "", body, nil)
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.SessionID, "无 Content-Type 时不应被读取")
		assert.False(t, bodyCached(c), "无 Content-Type 时不得写 body 缓存（证明未读）")
	})

	t.Run("GET method", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodGet, "application/json", body, nil)
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.SessionID, "GET 不应读取 body")
		assert.False(t, bodyCached(c), "GET 时不得写 body 缓存（证明未读）")
	})

	t.Run("form-urlencoded", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/x-www-form-urlencoded", body, nil)
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.SessionID, "非 JSON 不应被读取")
		assert.False(t, bodyCached(c), "非 JSON 时不得写 body 缓存（证明未读）")
	})
}

// --- 5. JSON 解析失败 / 字段类型不符时静默降级 ---

func TestResolveAffinityScope_MalformedBodySilentlyDegrades(t *testing.T) {
	t.Run("invalid json does not affect turn parse and keeps body", func(t *testing.T) {
		body := `{"litellm_session_id":` // 截断的非法 JSON
		c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, map[string]string{
			"X-Conversation-Request-Id": "turn-1",
		})

		var scope AffinityScope
		require.NotPanics(t, func() { scope = ResolveAffinityScope(c) })
		assert.Equal(t, "", scope.SessionID, "非法 JSON 应静默降级为无 session")
		assert.Equal(t, "turn-1", scope.TurnID, "非法 body 不得影响 turn 头解析")

		got, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		assert.Equal(t, body, string(got), "即便 JSON 解析失败，body 也必须被恢复")
	})

	t.Run("non-string field is skipped", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"session_id":123,"conversation_id":"conv-real"}`, nil)
		assert.Equal(t, "conv-real", ResolveAffinityScope(c).SessionID,
			"session_id 非字符串应跳过，继续尝试 conversation_id")
	})

	t.Run("non-object metadata is skipped", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"metadata":"not-an-object"}`, nil)
		assert.Equal(t, "", ResolveAffinityScope(c).SessionID, "metadata 非对象应静默跳过")
	})

	t.Run("no session fields at all", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json",
			`{"model":"gpt-4","messages":[]}`, nil)
		assert.Equal(t, "", ResolveAffinityScope(c).SessionID, "无会话字段时应返回空")
	})
}

// --- 6. 开关关闭时完全不读 body ---

func TestResolveAffinityScope_BodySwitchOff(t *testing.T) {
	originalBody := config.AffinityBodySessionID
	originalDerive := config.AffinityDeriveTurnID
	defer func() {
		config.AffinityBodySessionID = originalBody
		config.AffinityDeriveTurnID = originalDerive
	}()
	// 显式关闭派生开关以隔离被测行为：派生默认开启时，本用例「无 turn 头」的请求会走
	// 派生分支独立读 body（design.md:154 已批准的覆盖面扩大），与「body 补取开关关闭」
	// 语义无关，必须排除其干扰，否则无法断言零读取。
	config.AffinityDeriveTurnID = "false"

	body := `{"litellm_session_id":"litellm-abc"}`
	// 关闭判断必须容错：大小写与首尾空白都应识别为关闭。
	for _, off := range []string{"false", "FALSE", "False", " false ", "\tfalse"} {
		t.Run("AFFINITY_BODY_SESSION_ID="+off, func(t *testing.T) {
			config.AffinityBodySessionID = off
			c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, nil)

			scope := ResolveAffinityScope(c)

			assert.Equal(t, "", scope.SessionID, "body 补取开关关闭时不得从 body 取会话标识")
			assert.False(t, bodyCached(c), "派生开关关闭且 body 补取开关关闭时不得读 body")
		})
	}
}

func TestResolveAffinityScope_BodySwitchOn(t *testing.T) {
	original := config.AffinityBodySessionID
	defer func() { config.AffinityBodySessionID = original }()

	// 非 "false" 的任意取值都视为开启（默认行为）。
	for _, on := range []string{"true", "TRUE", "1", "", "yes"} {
		t.Run("AFFINITY_BODY_SESSION_ID="+on, func(t *testing.T) {
			config.AffinityBodySessionID = on
			c := newAffinityBodyContext(t, http.MethodPost, "application/json",
				`{"litellm_session_id":"litellm-abc"}`, nil)
			assert.Equal(t, "litellm-abc", ResolveAffinityScope(c).SessionID,
				"开关非 false 时应保持开启")
		})
	}
}

// --- 7. 超长值走 normalizeAffinityID 截断 ---

func TestResolveAffinityScope_BodyLongIDNormalized(t *testing.T) {
	long := strings.Repeat("a", 200)
	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"litellm_session_id":"`+long+`"}`, nil)

	scope := ResolveAffinityScope(c)

	assert.Len(t, scope.SessionID, 16, "超长 body 会话 id 归一化后应为 sha256 前 16 位 hex")
}

// --- 8. turn 头与 session 头均已命中时才不读 body（零开销路径）---

func TestResolveAffinityScope_HeaderHitSkipsBody(t *testing.T) {
	// 零读取路径要求「turn 头命中 且 session 头命中」：仅有 session 头而无 turn 头时，
	// 派生分支仍会读 body（design.md:154 已批准的覆盖面扩大），故此处必须补一个真实 turn 头。
	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"litellm_session_id":"from-body"}`, map[string]string{
			"X-Session-Id":              "from-header",
			"X-Conversation-Request-Id": "turn-from-header",
		})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "from-header", scope.SessionID, "header 命中时应优先取 header")
	assert.False(t, bodyCached(c), "session 头与 turn 头均已命中时不得读 body（零开销路径）")
}

// 正向锁定新设计的覆盖面扩大：session 头已命中但 turn 头全 miss 且派生开关开启时，
// 仍必须读取并解析 body（design.md:154），保证派生分支不被「session 已命中」提前短路。
func TestResolveAffinityScope_SessionHeaderStillReadsBodyForDerive(t *testing.T) {
	original := config.AffinityDeriveTurnID
	defer func() { config.AffinityDeriveTurnID = original }()
	config.AffinityDeriveTurnID = "true"

	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"litellm_session_id":"from-body","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Session-Id": "from-header"})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "from-header", scope.SessionID, "session 仍应优先取 header")
	assert.True(t, bodyCached(c), "turn 头全 miss 且派生开启时，即便 session 头已命中也必须读 body 供派生")
}

// 内容类型带参数（charset）时仍应识别为 JSON。
func TestResolveAffinityScope_JSONWithCharset(t *testing.T) {
	c := newAffinityBodyContext(t, http.MethodPost, "application/json; charset=utf-8",
		`{"session_id":"sess-abc"}`, nil)
	assert.Equal(t, "sess-abc", ResolveAffinityScope(c).SessionID,
		"application/json; charset=utf-8 也应被识别为 JSON")
}

// Content-Type 的媒体类型按 RFC 大小写不敏感：大小写变体也必须能取到 body 会话标识。
func TestResolveAffinityScope_JSONContentTypeCaseInsensitive(t *testing.T) {
	for _, ct := range []string{
		"Application/JSON",
		"APPLICATION/JSON; charset=utf-8",
		"Application/Json",
	} {
		t.Run(ct, func(t *testing.T) {
			c := newAffinityBodyContext(t, http.MethodPost, ct,
				`{"session_id":"sess-case"}`, nil)
			assert.Equal(t, "sess-case", ResolveAffinityScope(c).SessionID,
				"Content-Type %q 的大小写变体也应被识别为 JSON 并取到 body 会话标识", ct)
		})
	}
}

// --- 9. GetRequestBodyReusable 与既有缓存键互通 ---

func TestGetRequestBodyReusable_ReusesExistingCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("drained"))
	c.Request.Header.Set("Content-Type", "application/json")
	// 模拟「上游已用 GetRequestBody 缓存但不恢复 body」：body 已被消费，仅缓存可用。
	c.Set(ctxkey.KeyRequestBody, []byte(`{"session_id":"cached-sess"}`))

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "cached-sess", scope.SessionID, "应复用既有缓存而非重读 body")
	got, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	assert.Equal(t, `{"session_id":"cached-sess"}`, string(got),
		"命中缓存时也必须重建 body，保证下游直读拿到缓存内容")
}

// errReadCloser 在首次 Read 返回已读到的字节 + 指定错误，用于构造「body 读取失败」场景
// （连接中断 / 流损坏）；后续 Read 直接返回该错误。
type errReadCloser struct {
	data []byte
	err  error
	done bool
}

func (e *errReadCloser) Read(p []byte) (int, error) {
	if e.done {
		return 0, e.err
	}
	e.done = true
	return copy(p, e.data), e.err
}

func (e *errReadCloser) Close() error { return nil }

// --- 10. GetRequestBodyReusable 读取出错：恢复 body 但不写缓存 ---

func TestGetRequestBodyReusable_ReadErrorRestoresBodyWithoutCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	boom := errors.New("connection reset by peer")
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Request.Body = &errReadCloser{data: []byte(`{"session_id":"partial"}`), err: boom}

	body, err := common.GetRequestBodyReusable(c)

	require.Error(t, err, "读取失败必须返回错误")
	assert.ErrorIs(t, err, boom)
	assert.Nil(t, body, "读取出错时按既有约定返回 nil body")

	_, cached := c.Get(ctxkey.KeyRequestBody)
	assert.False(t, cached, "读取出错时不得写缓存（避免把不完整/损坏内容当成完整 body 复用）")

	require.NotNil(t, c.Request.Body, "读取出错时也必须恢复 body，避免留下半损坏的 c.Request.Body")
	restored, rerr := io.ReadAll(c.Request.Body)
	require.NoError(t, rerr)
	assert.Equal(t, `{"session_id":"partial"}`, string(restored),
		"恢复的 body 应为已读到的部分字节")
}
