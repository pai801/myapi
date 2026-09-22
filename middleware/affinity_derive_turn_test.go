package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingReadCloser 统计底层 body 的读取行为，用于证明「一次读取、共享解析」。
//
// 度量的真实语义与能力边界（务必如实理解，勿再赋予其不具备的证明力）：
//   - reads：所有 Read 调用次数（含数据耗尽后的 EOF 读取）。
//   - dataReads：返回数据的 Read 次数（n > 0）。它统计的是底层被填充的**块数**，而非
//     「逻辑读取次数」——io.ReadAll 以 512 字节初始缓冲起步并倍增，故同一份 body 在
//     1094 字节上产生 dataReads=3、5094 字节上产生 dataReads=7。因此 dataReads==1
//     仅在 body < 512 字节时成立，**与 body 体积强耦合，不可用于精确断言读取次数**。
//     本字段仅保留给「小 body 且只需判定『是否发生过读取』」的场景（如守卫零读取）。
//   - servedAll：成功返回给调用方的数据字节总量（逐次累加 n），与 io.ReadAll 的缓冲分块
//     方式无关。注意：单个 io.Reader 只服务其字节一次，故对同一底层 reader 重复 ReadAll
//     时 servedAll 恒为 len(body)、不会翻倍；它只能检测「截断/未读完」这类变异。
//   - exhaustedAt / readsAfterExhaustion：完整消费事件。当某次 Read 返回 io.EOF 且此前
//     已累计 servedAll 字节时，记 exhaustedAt = servedAll，此后任何 Read 都会把
//     readsAfterExhaustion 加一（即使返回 0, EOF）。它把「读完一次」与「读完后仍被继续
//     读取」区分开，且同样与缓冲分块方式无关。
//     边界：该机制依赖 exhaustedAt > 0，空 body（servedAll 始终为 0）不会触发，故空 body
//     场景不覆盖「耗尽后被继续读取」的检测。
type countingReadCloser struct {
	r         io.Reader
	reads     int
	dataReads int

	servedAll            int
	exhaustedAt          int
	readsAfterExhaustion int
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	c.reads++
	if c.exhaustedAt > 0 {
		// 已完整读完一次后仍被读取：无论本次是否返回数据都记录下来。
		c.readsAfterExhaustion++
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.dataReads++
		c.servedAll += n
	}
	if err == io.EOF && c.exhaustedAt == 0 && c.servedAll > 0 {
		// 首次完整消费：记录耗尽时已服务的字节总量。
		c.exhaustedAt = c.servedAll
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return nil }

// TestResolveAffinityScope_SharesParsedPayload
// G: 无 session/turn 头、两个开关开启、JSON POST 同时含 session 字段与 messages
// W: ResolveAffinityScope
// T: session 解析成功且共享一个 payload（body 只读一次；复用不再触发 IO）
//
// 阶段适配：本任务（Task 3.1）只抽出「读 body + 解析 payload」并让 session 消费者使用，
// turn 派生接线属 Task 3.2，故此处不对 TurnID 断言派生成功，只断言「payload 被共享」——
// 即 session 从共享 payload 解析成功、body 仅被读取一次、复用同一 payload 不再产生 IO。
// turn 派生相关断言留待 Task 4.1。
func TestResolveAffinityScope_SharesParsedPayload(t *testing.T) {
	original := config.AffinityDeriveTurnID
	defer func() { config.AffinityDeriveTurnID = original }()
	config.AffinityDeriveTurnID = "true" // 派生开关开启（默认即 true）

	body := `{"session_id":"sess-shared","messages":[{"role":"user","content":"hi"}]}`
	c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, nil)
	reader := &countingReadCloser{r: strings.NewReader(body)}
	c.Request.Body = reader

	scope := ResolveAffinityScope(c)

	// session 从共享 payload 解析成功（证明 payload 已被解析并被消费者使用）。
	require.Equal(t, "sess-shared", scope.SessionID, "session 应从共享 payload 解析成功")
	require.True(t, bodyCached(c), "共享读取必须写入 body 缓存，供下游复用")

	// 再次通过共享入口读取：命中缓存，不得再触碰底层 reader（证明「一次读取、共享解析」）。
	readsAfterResolve := reader.reads
	shared := readAffinityBodyPayload(c)
	require.NotNil(t, shared, "缓存存在时应能取回共享 payload")
	assert.Equal(t, body, string(shared.Raw), "共享 Raw 应为完整原始 body（不截断）")
	assert.Equal(t, "sess-shared", sessionIDFromBody(shared.Raw),
		"同一份 Raw 可被 session 消费者复用")
	assert.Equal(t, readsAfterResolve, reader.reads,
		"复用共享 payload 不得再次读取底层 body（缓存命中）")

	// 下游仍可读到完整 body（读后已恢复）。
	got, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "读 body 后必须恢复，下游直读逐字节完整")
}

// TestResolveAffinityScope_RealTurnHeaderPrecedesDerived
// G: 真实 turn 头、session 锚点与可派生 JSON 同时存在
// W: ResolveAffinityScope
// T: TurnID 等于头值、turnDerived 为 false、派生路径不运行（body 未被读取）
func TestResolveAffinityScope_RealTurnHeaderPrecedesDerived(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	// 真实 turn 头与 session 头均已命中，同时 body 也「可派生」——
	// 若实现错误地优先派生，TurnID 会变成 sha256 派生值而非头值。
	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"litellm_session_id":"from-body","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			"X-Conversation-Request-Id": "turn-from-header",
			"X-Session-Id":              "session-from-header",
		})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "turn-from-header", scope.TurnID, "真实 turn 头必须优先于派生值")
	assert.Equal(t, "session-from-header", scope.SessionID, "session 头命中时优先取头值")
	assert.False(t, scope.turnDerived, "真实 turn 头命中时 turnDerived 必须为 false")
	assert.False(t, bodyCached(c), "真实 turn 头命中且 session 头命中时派生路径不得运行（不读 body）")
}

// TestResolveAffinityScope_DerivesTurnIDFromBody
// G: 无真实 turn 头、session 锚点来自头、JSON POST、body 含可派生的 messages（真实 user 消息）、派生开关开启
// W: ResolveAffinityScope
// T: TurnID 为派生值且与纯函数独立计算结果逐字一致、turnDerived 为 true、派生路径确实读取了 body
//
// 本用例锁定 Task 3.2 的核心正向行为（派生接线 + turnDerived 置位）。断言刻意与纯函数
// common.DeriveTurnIDFromBody 的独立计算结果逐字比对：若派生接线被禁用、算法被替换或
// turnDerived 未置位，本用例必然失败（见变异 D / E）。
func TestResolveAffinityScope_DerivesTurnIDFromBody(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	const sessionID = "session-from-header"
	body := `{"model":"gpt-4","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello world"}]}`

	// 无真实 turn 头；session 锚点来自头（避免依赖 body 补取路径）。
	c := newAffinityBodyContext(t, http.MethodPost, "application/json", body,
		map[string]string{"X-Session-Id": sessionID})

	scope := ResolveAffinityScope(c)

	// 1) 派生成功，TurnID 非空。
	require.NotEmpty(t, scope.TurnID, "无真实 turn 头且可派生时必须产出派生 turn id")
	// 2) 与纯函数独立计算结果逐字一致（最强断言：直接锁定算法接线正确）。
	assert.Equal(t, common.DeriveTurnIDFromBody([]byte(body), sessionID), scope.TurnID,
		"派生 TurnID 必须与纯函数 DeriveTurnIDFromBody 的独立计算结果逐字一致")
	// 3) turnDerived 置位，供统计区分「真实头 turn」与「派生 turn」。
	assert.True(t, scope.turnDerived, "派生成功时 turnDerived 必须为 true")
	// 4) 派生路径确实读取了 body（共享缓存已写入）。
	assert.True(t, bodyCached(c), "派生路径必须读取 body 并写入共享缓存")
}

// TestResolveAffinityScope_DeriveEmptyValueDoesNotMarkDerived
// G: 无真实 turn 头、session 锚点来自头、JSON POST、body 不含可派生的 messages、派生开关开启
// W: ResolveAffinityScope
// T: TurnID 为空且 turnDerived 为 false（派生未产出值时不得置位）
//
// 补此对照的理由：turnDerived 的语义是「当前非空 TurnID 来自派生」。若派生值为空串却仍置位，
// 会让统计把「无 turn」误计为「派生 turn」。本用例与 TestResolveAffinityScope_DerivesTurnIDFromBody
// 共同为 turnDerived 建立双向语义保护。
func TestResolveAffinityScope_DeriveEmptyValueDoesNotMarkDerived(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	// session 锚点在头里（保证进入派生分支），但 body 无 messages → 纯函数返回空串。
	c := newAffinityBodyContext(t, http.MethodPost, "application/json", `{"model":"gpt-4"}`,
		map[string]string{"X-Session-Id": "session-from-header"})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "", scope.TurnID, "body 不可派生时 TurnID 必须为空")
	assert.False(t, scope.turnDerived, "派生未产出值时 turnDerived 必须为 false")
	assert.Equal(t, "", common.DeriveTurnIDFromBody([]byte(`{"model":"gpt-4"}`), "session-from-header"),
		"前置校验：纯函数对无 messages 的 body 确实返回空串")
}

// TestResolveAffinityScope_RequiresSessionAnchor
// G: 无真实 turn 头且 header/body 均无 session 锚点
// W: ResolveAffinityScope
// T: TurnID 为空、turnDerived 为 false、Keys 仅含 user 层
func TestResolveAffinityScope_RequiresSessionAnchor(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	// body 含可派生的 messages，但没有任何 session 字段；头里也无 session 锚点。
	c := newAffinityBodyContext(t, http.MethodPost, "application/json",
		`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`, nil)

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "", scope.TurnID, "无 session 锚点时必须抑制派生，TurnID 为空")
	assert.Equal(t, "", scope.SessionID, "header/body 均无 session 锚点，SessionID 为空")
	assert.False(t, scope.turnDerived, "派生未发生，turnDerived 必须为 false")

	keys := scope.Keys("gpt-4")
	require.Len(t, keys, 1, "无 turn / session 锚点时应只剩 user 兜底层")
	assert.Equal(t, AffinityLevelUser, keys[0].Level, "唯一候选键必须是 user 层")
}

// TestReadAffinityBodyPayload_FailureDegrades
// G: nil 请求、空 body、读取错误或非法 JSON
// W: readAffinityBodyPayload
// T: 返回 nil、不 panic、不污染缓存/日志且请求链路可继续降级
func TestReadAffinityBodyPayload_FailureDegrades(t *testing.T) {
	t.Run("nil context", func(t *testing.T) {
		var got *affinityBodyPayload
		require.NotPanics(t, func() { got = readAffinityBodyPayload(nil) })
		assert.Nil(t, got, "nil context 必须返回 nil 且不 panic")
	})

	t.Run("nil request", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = nil
		var got *affinityBodyPayload
		require.NotPanics(t, func() { got = readAffinityBodyPayload(c) })
		assert.Nil(t, got, "nil request 必须返回 nil 且不 panic")
	})

	t.Run("empty body", func(t *testing.T) {
		c := newAffinityBodyContext(t, http.MethodPost, "application/json", "", nil)
		var got *affinityBodyPayload
		require.NotPanics(t, func() { got = readAffinityBodyPayload(c) })
		assert.Nil(t, got, "空 body 必须返回 nil（静默降级）")
	})

	t.Run("read error degrades without faking a complete body", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		c := newAffinityBodyContext(t, http.MethodPost, "application/json", "", nil)
		c.Request.Body = &errReadCloser{data: []byte(`{"session_id":"partial"}`), err: io.ErrUnexpectedEOF}

		var got *affinityBodyPayload
		require.NotPanics(t, func() { got = readAffinityBodyPayload(c) })
		assert.Nil(t, got, "读取失败必须返回 nil（静默降级）")

		// 新语义：读取出错时绝不把「已读到的部分字节」伪装成完整 body 交给下游。
		// 底层 reader 已 done，若实现错误地把部分内容重建成 NopCloser，下游会「成功」读到
		// `{"session_id":"partial"}` 这段截断内容；此处断言读它必须失败（拿到 error）。
		restored, err := io.ReadAll(c.Request.Body)
		require.Error(t, err, "读取失败后不得让下游成功读到被截断的部分内容")
		assert.NotEqual(t, `{"session_id":"partial"}`, string(restored),
			"绝不接受截断内容被当成完整 body 转发")
	})

	t.Run("invalid json degrades and keeps body", func(t *testing.T) {
		body := `{"session_id":`
		c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, nil)

		var got *affinityBodyPayload
		require.NotPanics(t, func() { got = readAffinityBodyPayload(c) })
		assert.Nil(t, got, "非法 JSON 必须返回 nil（静默降级）")

		got2, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		assert.Equal(t, body, string(got2), "非法 JSON 也必须恢复完整 body 供下游使用")
	})
}

// affinityDeriveGuardCase 描述一条派生守卫用例：开关、请求特征与四个可观测结果。
// wantDerivedFromBody 为 true 时 TurnID 期望值由纯函数按 body + wantSessionID 独立计算，
// 避免把派生算法抄进期望值。
type affinityDeriveGuardCase struct {
	name                string
	deriveSwitch        string
	bodySessionSwitch   string
	method              string
	contentType         string
	body                string
	headers             map[string]string
	wantTurnID          string
	wantSessionID       string
	wantTurnDerived     bool
	wantDerivedFromBody bool
	wantBodyCached      bool
}

// TestResolveAffinityScope_DerivationGuards
// G: 表驱动覆盖关闭开关（含大小写/空白容错变体）、GET、非 JSON Content-Type、非法 JSON、真实 turn 头，
// 以及「派生开启 + body 补取关闭」的正交组合
// W: ResolveAffinityScope
// T: 不派生且沿 session/user 降级；每个 case 断言 TurnID / SessionID / turnDerived / body 读取的精确值
//
// 正向对照 case 与「补取关闭仍读 body」case 共同保证负向断言非恒真：前者证明同 harness 下派生确实会发生，
// 后者证明 body 确实被读取（否则「不提取 session」的断言会因「根本没读」而恒真通过）。
func TestResolveAffinityScope_DerivationGuards(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()

	const derivableBody = `{"litellm_session_id":"sess-from-body","messages":[{"role":"user","content":"hi"}]}`

	cases := []affinityDeriveGuardCase{
		// 1. 派生开关关闭：关闭判断必须容错，大小写与首尾空白变体一律视为关闭。
		// session 头命中 → needBody 为 false，故同时锁定「不派生」与「零读取」。
		{
			name: "real turn header suppresses derivation", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/json", body: derivableBody,
			headers:         map[string]string{"X-Conversation-Request-Id": "turn-from-header", "X-Session-Id": "session-from-header"},
			wantTurnID:      "turn-from-header",
			wantSessionID:   "session-from-header",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		// 正向对照：证明同 harness 下派生确实会发生，负向用例的「TurnID 为空」才有意义。
		{
			name: "positive control: derivation does happen", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/json", body: derivableBody,
			headers:             map[string]string{"X-Session-Id": "session-from-header"},
			wantSessionID:       "session-from-header",
			wantDerivedFromBody: true,
			wantTurnDerived:     true,
			wantBodyCached:      true,
		},
		// 2. 非 body 方法：GET 不读 body，即便派生开启且无 session 头。
		{
			name: "GET with session header suppresses derivation and read", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodGet, contentType: "application/json", body: derivableBody,
			headers:         map[string]string{"X-Session-Id": "session-from-header"},
			wantSessionID:   "session-from-header",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		{
			name: "GET without session anchor degrades to user", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodGet, contentType: "application/json", body: derivableBody,
			wantSessionID:   "",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		// 3. 非 JSON Content-Type：即便 body 同时含 session 字段与 messages，也不得读取。
		{
			name: "text/plain suppresses derivation and read", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "text/plain", body: derivableBody,
			wantSessionID:   "",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		{
			name: "multipart/form-data suppresses derivation and read", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "multipart/form-data; boundary=xyz", body: derivableBody,
			wantSessionID:   "",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		{
			name: "form-urlencoded suppresses derivation and read", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/x-www-form-urlencoded", body: derivableBody,
			wantSessionID:   "",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		{
			name: "missing Content-Type suppresses derivation and read", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "", body: derivableBody,
			wantSessionID:   "",
			wantBodyCached:  false,
			wantTurnDerived: false,
		},
		// 4. 非法 JSON：不得 panic、不派生、降级；body 已被 GetRequestBodyReusable 成功缓存（读成功、解析失败）。
		{
			name: "invalid JSON degrades to session without panic", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/json", body: `{"litellm_session_id":`,
			headers:         map[string]string{"X-Session-Id": "session-from-header"},
			wantSessionID:   "session-from-header",
			wantBodyCached:  true,
			wantTurnDerived: false,
		},
		{
			name: "invalid JSON degrades to user without panic", deriveSwitch: "true", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/json", body: `{"litellm_session_id":`,
			wantSessionID:   "",
			wantBodyCached:  true,
			wantTurnDerived: false,
		},
		// 正交组合（advisory 1）：派生开启 + body 补取关闭 + JSON POST + body 含 session 字段 + 无 session 头。
		// 派生需要 body → 必须读取；补取开关关闭 → 不得提取 session；无 session 锚点 → 派生前置条件不满足。
		// 删除 sessionIDFromBody 前的 affinityBodySessionIDEnabled() 判定会让 SessionID/TurnID 变非空，本 case 必然失败。
		{
			name:         "derive on + body session switch off reads body but does not extract session",
			deriveSwitch: "true", bodySessionSwitch: "false",
			method: http.MethodPost, contentType: "application/json", body: derivableBody,
			wantSessionID:   "",
			wantTurnID:      "",
			wantBodyCached:  true,
			wantTurnDerived: false,
		},
		// 派生关闭但补取开启：必须回到改造前行为——session 仍从 body 提取，turn 层不派生。
		{
			name: "derive switch off still extracts session from body", deriveSwitch: "false", bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/json", body: derivableBody,
			wantSessionID:   "sess-from-body",
			wantBodyCached:  true,
			wantTurnDerived: false,
		},
	}

	// 关闭开关的容错变体：仅 "false"（忽略大小写与首尾空白）表示关闭。
	for _, off := range []string{"false", "FALSE", "False", " false ", "\tfalse"} {
		cases = append(cases, affinityDeriveGuardCase{
			name: "derive switch off (" + off + ") suppresses derivation", deriveSwitch: off, bodySessionSwitch: "true",
			method: http.MethodPost, contentType: "application/json", body: derivableBody,
			headers:         map[string]string{"X-Session-Id": "session-from-header"},
			wantSessionID:   "session-from-header",
			wantBodyCached:  false,
			wantTurnDerived: false,
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.AffinityDeriveTurnID = tc.deriveSwitch
			config.AffinityBodySessionID = tc.bodySessionSwitch

			wantTurnID := tc.wantTurnID
			if tc.wantDerivedFromBody {
				wantTurnID = common.DeriveTurnIDFromBody([]byte(tc.body), tc.wantSessionID)
				require.NotEmpty(t, wantTurnID, "前置校验：该 case 的 body 必须可派生，否则正向对照恒真")
			}

			c := newAffinityBodyContext(t, tc.method, tc.contentType, tc.body, tc.headers)

			var scope AffinityScope
			require.NotPanics(t, func() { scope = ResolveAffinityScope(c) }, "守卫路径绝不 panic")

			assert.Equal(t, wantTurnID, scope.TurnID, "TurnID")
			assert.Equal(t, tc.wantSessionID, scope.SessionID, "SessionID")
			assert.Equal(t, tc.wantTurnDerived, scope.turnDerived, "turnDerived")
			assert.Equal(t, tc.wantBodyCached, bodyCached(c), "body 是否被读取")
		})
	}
}

// TestResolveAffinityScope_DoesNotReadBodyWhenGuardFails
// G: 可观测读取次数的 body 且方法或 Content-Type 守卫失败
// W: ResolveAffinityScope
// T: 读取次数为 0、KeyRequestBody 缓存不存在
//
// 派生开启且请求不带任何 turn / session 头 → needBody 必为 true，故「零读取」只能由
// 方法 / Content-Type 守卫造成，而非「无人需要 body」。末位 JSON POST 为正向对照：
// 同一 harness 下确实读取并派生，证明零读取断言不是因计数失效而恒真。
func TestResolveAffinityScope_DoesNotReadBodyWhenGuardFails(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	const body = `{"session_id":"sess-in-body","messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name          string
		method        string
		contentType   string
		wantZeroReads bool
	}{
		{"GET application/json", http.MethodGet, "application/json", true},
		{"POST text/plain", http.MethodPost, "text/plain", true},
		{"POST multipart/form-data", http.MethodPost, "multipart/form-data; boundary=xyz", true},
		{"POST application/x-www-form-urlencoded", http.MethodPost, "application/x-www-form-urlencoded", true},
		{"POST application/json (positive control)", http.MethodPost, "application/json", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAffinityBodyContext(t, tc.method, tc.contentType, body, nil)
			reader := &countingReadCloser{r: strings.NewReader(body)}
			c.Request.Body = reader

			scope := ResolveAffinityScope(c)

			if tc.wantZeroReads {
				assert.Equal(t, 0, reader.reads, "方法与 Content-Type 守卫失败时不得读取底层 body")
				assert.False(t, bodyCached(c), "守卫失败时不得写入 KeyRequestBody 缓存")
				assert.Equal(t, "", scope.SessionID, "未读 body 且无 session 头 → SessionID 为空（降级）")
				assert.Equal(t, "", scope.TurnID, "未读 body → 不得派生 turn id")
				assert.False(t, scope.turnDerived, "未派生时 turnDerived 必须为 false")
				return
			}

			assert.Greater(t, reader.reads, 0, "对照：JSON POST 必须读取底层 body")
			assert.True(t, bodyCached(c), "对照：JSON POST 必须写入 body 缓存")
			assert.Equal(t, "sess-in-body", scope.SessionID, "对照：session 应从 body 提取")
			assert.NotEmpty(t, scope.TurnID, "对照：存在 session 锚点时应派生出 turn id")
			assert.True(t, scope.turnDerived, "对照：派生成功时 turnDerived 为 true")
		})
	}
}

// affinityDerivedTurnCase 描述一条派生语义用例：同一 session 下的请求序列（逐步追加消息）与逐请求的精确期望。
// wantDerivedFromBody 为 true 时 TurnID 期望值由纯函数按该请求 body + wantSessionID 独立计算，避免把算法抄进期望值。
// stablePrefixLen > 1 时断言前 N 个请求的派生值两两相同（轮内恒定）；rotateAtIndex >= 0 时断言该下标派生值与下标 0 不同（跨轮轮换）。
type affinityDerivedTurnCase struct {
	name                string
	deriveSwitch        string
	method              string
	contentType         string
	headers             map[string]string
	bodies              []string
	wantSessionID       string
	wantTurnID          string
	wantTurnDerived     bool
	wantBodyCached      bool
	wantDerivedFromBody bool
	stablePrefixLen     int
	rotateAtIndex       int
}

// TestResolveAffinityScope_DerivedTurnMatrix
// G: 表驱动覆盖轮内追加 tool/assistant（派生值恒定）、追加新 user（换值）、Anthropic 纯 tool_result 被跳过、
// 历史压缩下标重复、多模态无文本降级，以及真实头 / 无 session / 关闭开关（False 与带空白 false）/ 非 JSON / 坏 JSON / GET 的必要交叉验证
// W: ResolveAffinityScope（对同一 session 的请求序列逐次调用）
// T: 每个请求的 TurnID / SessionID / turnDerived / body 读取精确符合期望，且序列内派生值轮内恒定、跨轮轮换
//
// 轮内恒定与跨轮轮换用请求序列而非单次调用验证：若派生被替换为「每次不同」（如时间戳）则恒定断言失败，
// 若被替换为「忽略 body 的恒定值」则轮换断言失败——两条核心语义因此非恒真。
func TestResolveAffinityScope_DerivedTurnMatrix(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()

	const session = "sess-matrix"

	// 轮内追加：同一 session，先发 user，再逐步追加 assistant / tool，最后一条真实 user 下标不变。
	toolLoop := []string{
		`{"messages":[{"role":"user","content":"solve it"}]}`,
		`{"messages":[{"role":"user","content":"solve it"},{"role":"assistant","content":"calling tool"}]}`,
		`{"messages":[{"role":"user","content":"solve it"},{"role":"assistant","content":"calling tool"},{"role":"tool","content":"tool output"}]}`,
		`{"messages":[{"role":"user","content":"solve it"},{"role":"assistant","content":"calling tool"},{"role":"tool","content":"tool output"},{"role":"assistant","content":"more"}]}`,
	}
	// 跨轮：前两个请求属第一轮（下标 0），追加新 user 后进入第二轮（下标 2）。
	newUserTurn := []string{
		`{"messages":[{"role":"user","content":"first question"}]}`,
		`{"messages":[{"role":"user","content":"first question"},{"role":"assistant","content":"first answer"}]}`,
		`{"messages":[{"role":"user","content":"first question"},{"role":"assistant","content":"first answer"},{"role":"user","content":"second question"}]}`,
		`{"messages":[{"role":"user","content":"first question"},{"role":"assistant","content":"first answer"},{"role":"user","content":"second question"},{"role":"assistant","content":"second answer"}]}`,
	}
	// Anthropic 工具循环：末尾的 user 消息只含 tool_result，必须跳过并回退到下标 0 的真实 user。
	toolResultSkip := []string{
		`{"messages":[{"role":"user","content":[{"type":"text","text":"run the command"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"run the command"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"cmd":"ls"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"run the command"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"cmd":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file.txt"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"run the command"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"cmd":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file.txt"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"bash","input":{"cmd":"cat"}}]}]}`,
	}
	// 历史压缩：两个不同轮次的最后真实 user 下标相同（均为 0），仅文本指纹不同。
	compacted := []string{
		`{"messages":[{"role":"user","content":"alpha question"}]}`,
		`{"messages":[{"role":"user","content":"beta question"}]}`,
	}
	// 多模态无文本：content 数组只有 image block，无 text block → 无法提取指纹 → 降级。
	imageOnly := []string{
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}`,
	}
	derivableBody := `{"messages":[{"role":"user","content":"hi"}]}`

	cases := []affinityDerivedTurnCase{
		// ① 轮内追加 tool/assistant → 派生值恒定。
		{
			name: "tool loop appends keep derived turn id constant", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies: toolLoop, wantSessionID: session, wantDerivedFromBody: true,
			wantTurnDerived: true, wantBodyCached: true, stablePrefixLen: 4, rotateAtIndex: -1,
		},
		// ② 追加新 user 消息 → 派生值轮换（前两请求恒定，第三请求起换值）。
		{
			name: "new user message rotates derived turn id", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies: newUserTurn, wantSessionID: session, wantDerivedFromBody: true,
			wantTurnDerived: true, wantBodyCached: true, stablePrefixLen: 2, rotateAtIndex: 2,
		},
		// ⑧ Anthropic 纯 tool_result user 消息被跳过 → 回退上一条真实 user，整个工具循环恒定。
		{
			name: "anthropic pure tool_result user message is skipped", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies: toolResultSkip, wantSessionID: session, wantDerivedFromBody: true,
			wantTurnDerived: true, wantBodyCached: true, stablePrefixLen: 4, rotateAtIndex: -1,
		},
		// 补充：历史压缩导致下标重复时，含文本指纹仍能区分两个 turn。
		{
			name: "compacted history with same index still rotates by text fingerprint", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies: compacted, wantSessionID: session, wantDerivedFromBody: true,
			wantTurnDerived: true, wantBodyCached: true, stablePrefixLen: 0, rotateAtIndex: 1,
		},
		// 补充：多模态无文本（image-only）→ 降级，TurnID 为空且不置 turnDerived。
		{
			name: "image-only message without text degrades", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies: imageOnly, wantSessionID: session, wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: true, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		// 交叉验证：真实 turn 头优先，不派生且不读 body。
		{
			name: "real turn header suppresses derivation", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json",
			headers:       map[string]string{"X-Conversation-Request-Id": "turn-hdr", "X-Session-Id": session},
			bodies:        []string{derivableBody},
			wantSessionID: session, wantTurnID: "turn-hdr",
			wantTurnDerived: false, wantBodyCached: false, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		// 交叉验证：无 session 锚点 → 不派生，body 已读但 session 为空。
		{
			name: "missing session anchor suppresses derivation", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: nil,
			bodies:        []string{derivableBody},
			wantSessionID: "", wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: true, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		// 交叉验证：开关容错关闭（False / 带首尾空白 false）→ 不派生、不读 body。
		{
			name: "derive switch off (False) suppresses derivation", deriveSwitch: "False",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies:        []string{derivableBody},
			wantSessionID: session, wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: false, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		{
			name: "derive switch off ( false ) suppresses derivation", deriveSwitch: " false ",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies:        []string{derivableBody},
			wantSessionID: session, wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: false, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		// 交叉验证：非 JSON Content-Type → 不读 body、不派生。
		{
			name: "non JSON content type suppresses derivation", deriveSwitch: "true",
			method: http.MethodPost, contentType: "text/plain", headers: map[string]string{"X-Session-Id": session},
			bodies:        []string{derivableBody},
			wantSessionID: session, wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: false, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		// 交叉验证：坏 JSON → 不 panic、不派生，body 已读并缓存。
		{
			name: "malformed JSON degrades without panic", deriveSwitch: "true",
			method: http.MethodPost, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies:        []string{`{"messages":[`},
			wantSessionID: session, wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: true, stablePrefixLen: 0, rotateAtIndex: -1,
		},
		// 交叉验证：GET → 不读 body、不派生。
		{
			name: "GET suppresses derivation", deriveSwitch: "true",
			method: http.MethodGet, contentType: "application/json", headers: map[string]string{"X-Session-Id": session},
			bodies:        []string{derivableBody},
			wantSessionID: session, wantTurnID: "",
			wantTurnDerived: false, wantBodyCached: false, stablePrefixLen: 0, rotateAtIndex: -1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.AffinityDeriveTurnID = tc.deriveSwitch
			config.AffinityBodySessionID = "true"

			require.NotEmpty(t, tc.bodies, "用例必须至少包含一个请求")
			scopes := make([]AffinityScope, 0, len(tc.bodies))
			for _, body := range tc.bodies {
				c := newAffinityBodyContext(t, tc.method, tc.contentType, body, tc.headers)

				var scope AffinityScope
				require.NotPanics(t, func() { scope = ResolveAffinityScope(c) }, "派生路径绝不 panic")

				wantTurnID := tc.wantTurnID
				if tc.wantDerivedFromBody {
					wantTurnID = common.DeriveTurnIDFromBody([]byte(body), tc.wantSessionID)
					require.NotEmpty(t, wantTurnID, "前置校验：该 body 必须可派生，否则正向断言恒真")
				}

				assert.Equal(t, wantTurnID, scope.TurnID, "TurnID")
				assert.Equal(t, tc.wantSessionID, scope.SessionID, "SessionID")
				assert.Equal(t, tc.wantTurnDerived, scope.turnDerived, "turnDerived")
				assert.Equal(t, tc.wantBodyCached, bodyCached(c), "body 是否被读取")
				scopes = append(scopes, scope)
			}

			if tc.stablePrefixLen > 1 {
				require.LessOrEqual(t, tc.stablePrefixLen, len(scopes), "序列长度必须覆盖轮内恒定断言范围")
				require.NotEmpty(t, scopes[0].TurnID, "轮内恒定的前提是派生出了非空值")
				for i := 1; i < tc.stablePrefixLen; i++ {
					assert.Equal(t, scopes[0].TurnID, scopes[i].TurnID,
						"轮内追加 assistant/tool 消息后派生 turn id 必须恒定")
				}
			}
			if tc.rotateAtIndex >= 0 {
				require.Less(t, tc.rotateAtIndex, len(scopes), "序列长度必须覆盖跨轮轮换断言下标")
				require.NotEmpty(t, scopes[0].TurnID, "跨轮轮换的前提是派生出了非空值")
				assert.NotEqual(t, scopes[0].TurnID, scopes[tc.rotateAtIndex].TurnID,
					"追加新 user 消息后派生 turn id 必须轮换")
			}
		})
	}
}

// TestResolveAffinityScope_RestoresBody
// G: body 为完整 JSON（含多字节文本与长字段）且派生路径读取它
// W: ResolveAffinityScope 之后下游直读 c.Request.Body
// T: 下游读到的字节与原始 body 逐字节完全一致且长度相等（无截断、无消费）
func TestResolveAffinityScope_RestoresBody(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	// body 含多字节字符与长字段：任何按字节截断都会在长度或内容比对中暴露。
	body := `{"session_id":"sess-restore","messages":[` +
		`{"role":"user","content":"请逐步分析这个问题并给出完整答案：` + strings.Repeat("数据", 40) + `"},` +
		`{"role":"assistant","content":"好的，我来分析"},` +
		`{"role":"tool","content":"` + strings.Repeat("x", 256) + `"}]}`

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"derive path reads body with header session", map[string]string{"X-Session-Id": "sess-restore"}},
		{"derive path reads body with body session", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, tc.headers)
			reader := &countingReadCloser{r: strings.NewReader(body)}
			c.Request.Body = reader

			scope := ResolveAffinityScope(c)

			require.NotEmpty(t, scope.TurnID, "派生路径必须执行，否则无法证明「读后恢复」")
			require.True(t, scope.turnDerived, "派生成功时 turnDerived 必须为 true")
			require.Greater(t, reader.reads, 0, "派生路径必须实际读取底层 body")

			got, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			assert.Equal(t, len(body), len(got), "下游读到的字节数必须与原始 body 完全一致（无截断）")
			assert.Equal(t, body, string(got), "下游读到的字节必须与原始 body 逐字节一致")
		})
	}
}

// affinityBodyOfSize 构造一个总长度恰为 size 字节的合法 JSON body：
// 含 session_id 与可派生的 messages，并用 pad 字段把长度补齐到 size。
// 要求 size >= len(head)+len(suffix)，否则 t.Fatal 而非静默产出错误长度。
func affinityBodyOfSize(t *testing.T, sessionID string, size int) string {
	t.Helper()
	head := `{"session_id":"` + sessionID + `","pad":"`
	suffix := `","messages":[{"role":"user","content":"hello once"}]}`
	padLen := size - len(head) - len(suffix)
	require.GreaterOrEqual(t, padLen, 0, "目标体积过小，无法容纳必需字段")
	body := head + strings.Repeat("x", padLen) + suffix
	require.Len(t, body, size, "body 构造必须精确命中目标字节数")
	return body
}

// TestResolveAffinityScope_ReadsBodyOnceForBothConsumers
// G: 无 session / turn 头、两个开关开启、JSON POST（body 同时含 session 字段与可派生 messages）、
// 底层 body 使用可观测「服务字节数 / 完整消费事件 / 耗尽后读取」的计数 reader；body 体积取
// 94B / 1094B / 5094B 三档
// W: ResolveAffinityScope，随后再次经共享入口 readAffinityBodyPayload
// T: reader 服务的总字节数恰为 len(body)（恰好一次完整消费，且耗尽后无任何读取尝试）；
// KeyRequestBody 缓存为与原始 body 逐字节相等的完整 []byte；SessionID 与 TurnID 均来自同一份
// body 且各自正确；下游仍能逐字节读到完整 body；ResolveAffinityScope 的分配量不超过
// 「单次读取步骤」基线（证明同一 Raw 只被解析一次）
//
// 度量口径说明（本用例刻意不绑定 io.ReadAll 的分块方式）：
// 旧实现的 dataReads（n>0 计数）统计的是底层被填充的**块数**，随 body 体积增长而增长
// （94B→1、1094B→3、5094B→7），故 `dataReads==1` 只在 body<512B 时成立，会对真实 LLM
// 请求量级（>512B）的正确实现产生假失败。本用例改用 servedAll/exhaustedAt/readsAfterExhaustion：
//   - 断言 reader 服务的总字节数恰为 len(body)（只能检测「截断/未读完」这类变异）；
//   - 断言 body 恰被完整消费一次（exhaustedAt == len(body)）且耗尽后无读取尝试。
//
// 「恰好一次完整消费」的可击杀性边界（如实声明）：
//   - servedAll / exhaustedAt：只能检测「截断/未读完」（M4 型）——即底层 reader 未被完整消费；
//   - readsAfterExhaustion：只能检测「同一底层 reader 耗尽后被继续读取」（M3b 型）；
//   - 不能击杀「绕过缓存再次完整读取 c.Request.Body」的变异（M1/M5 型）——GetRequestBodyReusable
//     读后把 c.Request.Body 替换为新的 bytes.Buffer（见 common/gin.go），该 buffer 不在计数
//     reader 上，故重读的字节对 servedAll 不可见（实测：同 reader 读两次 servedAll 未翻倍、
//     readsAfterExhaustion=1）；且单个 io.Reader 只服务其字节一次，「重复消费使字节翻倍」在
//     物理上不成立。此类 bypass-cache 重读由本用例的缓存断言（KeyRequestBody 逐字节相等）与
//     negative control 子测试捕获（与 M1 变异披露一致）；
//   - 能击杀「破坏 body 恢复」的变异（下游完整 body 断言失败）；
//   - 不能击杀「再次调用读取步骤但命中缓存」的变异——GetRequestBodyReusable 命中缓存时
//     既不触碰底层 reader，原始 reader 耗尽后再读也只返回 0/EOF，故该口径下不可见；
//     此类「读取步骤被重复调用」由同用例的 parse-once 子测试（分配量）负责捕获。
func TestResolveAffinityScope_ReadsBodyOnceForBothConsumers(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	const sessionID = "sess-once"

	t.Run("main path consumes body data exactly once across body sizes", func(t *testing.T) {
		// 三档体积：<512B（旧断言唯一成立的区间）、>512B（旧断言假失败区间）、真实 LLM 量级。
		for _, size := range []int{94, 1094, 5094} {
			t.Run(fmt.Sprintf("body=%dB", size), func(t *testing.T) {
				body := affinityBodyOfSize(t, sessionID, size)
				wantTurnID := common.DeriveTurnIDFromBody([]byte(body), sessionID)
				require.NotEmpty(t, wantTurnID, "前置校验：该 body 必须可派生，否则 TurnID 断言恒真")

				c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, nil)
				reader := &countingReadCloser{r: strings.NewReader(body)}
				c.Request.Body = reader

				scope := ResolveAffinityScope(c)

				// 1) 与体积解耦的核心断言：恰好一次完整消费。
				assert.Equal(t, len(body), reader.servedAll,
					"reader 服务的总字节数必须恰为 len(body)：不得截断或未读完（同 reader 重读不会翻倍，故本断言不负责检测 bypass-cache 重读）")
				assert.Equal(t, len(body), reader.exhaustedAt,
					"body 必须恰好被完整消费一次（耗尽事件记录的字节总量等于 body 长度）")
				assert.Equal(t, 0, reader.readsAfterExhaustion,
					"完整消费后不得再有任何读取尝试（证明读取步骤未被重复触发）")

				// 2) KeyRequestBody 保存完整 []byte（逐字节相等）。
				cached, ok := c.Get(ctxkey.KeyRequestBody)
				require.True(t, ok, "共享读取必须写入 KeyRequestBody 缓存")
				cachedBytes, ok := cached.([]byte)
				require.True(t, ok, "KeyRequestBody 必须保存 []byte")
				assert.Equal(t, body, string(cachedBytes), "缓存必须是完整原始 body（逐字节相等，不得截断）")

				// 3) 两个消费者结果均正确，且来自同一份 body。
				assert.Equal(t, sessionID, scope.SessionID, "session 消费者应从共享 payload 取到会话标识")
				assert.Equal(t, wantTurnID, scope.TurnID,
					"turn 消费者应从同一 payload 派生出与纯函数独立计算一致的 turn id")
				assert.True(t, scope.turnDerived, "派生成功时 turnDerived 必须为 true")

				// 4) 再次经共享入口读取：命中缓存，不得再触碰底层 reader。
				servedBefore := reader.servedAll
				readsBefore := reader.reads
				shared := readAffinityBodyPayload(c)
				require.NotNil(t, shared, "缓存存在时应能取回共享 payload")
				assert.Equal(t, body, string(shared.Raw), "共享 Raw 必须为完整原始 body")
				assert.Equal(t, sessionID, sessionIDFromBody(shared.Raw), "同一份 Raw 可被 session 消费者复用")
				assert.Equal(t, wantTurnID, common.DeriveTurnIDFromBody(shared.Raw, sessionID),
					"同一份 Raw 可被 turn 消费者复用")
				assert.Equal(t, servedBefore, reader.servedAll, "复用共享 payload 不得再消费底层 body（缓存命中）")
				assert.Equal(t, readsBefore, reader.reads, "复用共享 payload 不得再读取底层 reader（缓存命中）")

				// 5) 下游仍能读到完整 body（逐字节 + 长度）。
				got, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				assert.Equal(t, len(body), len(got), "下游读到的字节数必须与原始 body 相等")
				assert.Equal(t, body, string(got), "下游读到的字节必须与原始 body 逐字节一致")
			})
		}
	})

	t.Run("both consumers scan without per-key allocation", func(t *testing.T) {
		// 旧实现（json.Unmarshal → map[string]any）的分配量随 payload 键数线性增长，故可用分配量
		// 作为「是否发生全量解析」的可观测信号。新实现改为 GetRequestBodyReusable（一次全量读 + 缓存）
		// + json.Valid（零分配）+ jsonparser 只读扫描，分配量不应随键数增长。
		//
		// 断言设计（可击杀性）：
		//   - 正向对照：json.Unmarshal 全量解析同 body 的分配量必须随键数显著增长，证明本度量对
		//     「按 key 分配」敏感（否则阈值恒真、无击杀力）；
		//   - 被测：ResolveAffinityScope 在小/大键数下的分配量差值必须远小于全量解析的差值 ——
		//     若实现回退到全量解析，resolve 差值会与 unmarshal 差值同量级，本断言必然失败。
		light := affinityParseHeavyBody(100)
		heavy := affinityParseHeavyBody(1600)

		resolveLight := testing.AllocsPerRun(20, func() {
			c := newAffinityBodyContext(t, http.MethodPost, "application/json", light, nil)
			_ = ResolveAffinityScope(c)
		})
		resolveHeavy := testing.AllocsPerRun(20, func() {
			c := newAffinityBodyContext(t, http.MethodPost, "application/json", heavy, nil)
			_ = ResolveAffinityScope(c)
		})
		unmarshalLight := testing.AllocsPerRun(20, func() {
			var m map[string]any
			if err := json.Unmarshal([]byte(light), &m); err != nil {
				t.Fatal(err)
			}
		})
		unmarshalHeavy := testing.AllocsPerRun(20, func() {
			var m map[string]any
			if err := json.Unmarshal([]byte(heavy), &m); err != nil {
				t.Fatal(err)
			}
		})

		t.Logf("allocs: resolve(100)=%.0f resolve(1600)=%.0f unmarshal(100)=%.0f unmarshal(1600)=%.0f",
			resolveLight, resolveHeavy, unmarshalLight, unmarshalHeavy)

		require.Greater(t, unmarshalHeavy, unmarshalLight*2,
			"前置校验：全量 json.Unmarshal 的分配量必须随键数显著增长，否则本度量无击杀力")

		assert.Less(t, resolveHeavy-resolveLight, (unmarshalHeavy-unmarshalLight)/4,
			"ResolveAffinityScope 不得按 payload 键数分配（回退到全量解析必然超标）")
		// 放宽为比值断言（原 resolveHeavy <= resolveLight+10 在 -race 下间歇假失败）：
		// resolveLight 实测 45~46、resolveHeavy 54~57，差值 10~11 恰好骑在 +10 边界上，
		// -race -count=30 可复现失败。改用 resolveHeavy < resolveLight*1.5：
		//   - 余量：实测比值 54/45≈1.20 ~ 57/46≈1.24，距 1.5 尚有约 26 个百分点（≈10 个分配单位）；
		//   - 选比值而非固定 +N 的原因：分配量含读取缓冲随 body 体积增长的常数项，该常数项
		//     随版本/体积浮动，比值口径与量级无关、更抗同向漂移；
		//   - 击杀力：若回退到全量解析，resolveLight/Heavy 会各自叠加 unmarshal 量级
		//     （实测 unmarshal 430/6430），比值将跃至 ~13，远超 1.5 必然失败。
		assert.Less(t, resolveHeavy, resolveLight*1.5,
			"resolve 分配量应基本与键数无关（比值口径，回退到全量解析必然超标）")
	})

	t.Run("negative control: prewarmed cache performs zero data reads", func(t *testing.T) {
		body := affinityBodyOfSize(t, sessionID, 1094)
		wantTurnID := common.DeriveTurnIDFromBody([]byte(body), sessionID)
		require.NotEmpty(t, wantTurnID, "前置校验：该 body 必须可派生")

		c := newAffinityBodyContext(t, http.MethodPost, "application/json", body, nil)
		reader := &countingReadCloser{r: strings.NewReader(body)}
		c.Request.Body = reader
		// 预热缓存：读取步骤若正确复用缓存，就不得再触碰底层 reader。
		c.Set(ctxkey.KeyRequestBody, []byte(body))

		scope := ResolveAffinityScope(c)

		assert.Equal(t, 0, reader.reads,
			"缓存命中时不得读取底层 body（证明「恰好一次消费」不是恒真）")
		assert.Equal(t, 0, reader.servedAll, "缓存命中时不得从底层 reader 消费任何字节")
		assert.Equal(t, 0, reader.dataReads, "缓存命中时不得触发任何返回数据的底层 Read")
		assert.Equal(t, sessionID, scope.SessionID, "缓存路径仍须解析出会话标识")
		assert.Equal(t, wantTurnID, scope.TurnID, "缓存路径仍须派生出相同的 turn id")
	})
}

// affinityParseHeavyBody 构造键数可控的合法 JSON body：键数越多，json.Unmarshal 全量解析的分配量
// 越大 —— 该性质被用作「是否发生全量解析」的可观测信号（见 parse 子测试的正向对照）。
// 含 session_id 与可派生 messages，保证被测路径的两个消费者都实际消费该 body。
func affinityParseHeavyBody(keys int) string {
	var b strings.Builder
	b.WriteString(`{"session_id":"sess-once","messages":[{"role":"user","content":"hello once"}]`)
	for i := 0; i < keys; i++ {
		b.WriteString(`,"key`)
		for j := 0; j < 6; j++ {
			b.WriteByte(byte('a' + (i+j)%26))
		}
		b.WriteString(`":"value"`)
	}
	b.WriteString(`}`)
	return b.String()
}

// TestResolveAffinityScope_SharedBodyIsRaceFree
// G: 并发执行多个互相独立的请求 context，每个持有独立 body（不同 session 字段与不同 messages）
// 与独立 headers
// W: 并发 ResolveAffinityScope（sync.WaitGroup）
// T: 每个请求的 SessionID / TurnID 与各自输入一致，绝不串号；-race 下无数据竞争报告
//
// 本用例在 -race 下运行：若共享 payload 或配置读取被实现为跨请求可变状态，竞争检测器会报告。
func TestResolveAffinityScope_SharedBodyIsRaceFree(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	originalBody := config.AffinityBodySessionID
	defer func() {
		config.AffinityDeriveTurnID = originalDerive
		config.AffinityBodySessionID = originalBody
	}()
	config.AffinityDeriveTurnID = "true"
	config.AffinityBodySessionID = "true"

	const requests = 16
	type expectation struct {
		sessionID string
		turnID    string
		body      string
	}
	wants := make([]expectation, requests)
	for i := 0; i < requests; i++ {
		sessionID := fmt.Sprintf("sess-race-%d", i)
		// 每个请求的 messages 文本不同 → 派生值必须各不相同，串号会被立刻发现。
		body := fmt.Sprintf(`{"session_id":%q,"messages":[{"role":"user","content":"race message %d"}]}`,
			sessionID, i)
		wants[i] = expectation{
			sessionID: sessionID,
			turnID:    common.DeriveTurnIDFromBody([]byte(body), sessionID),
			body:      body,
		}
		require.NotEmpty(t, wants[i].turnID, "前置校验：每个请求的 body 必须可派生")
	}

	// context 在主 goroutine 内预构造：newAffinityBodyContext 会调用 gin.SetMode（写全局模式），
	// 并发构造会触发 gin 自身全局状态的竞态，与本任务被测的 body 共享无关，必须在并发区外完成。
	contexts := make([]*gin.Context, requests)
	for i := 0; i < requests; i++ {
		contexts[i] = newAffinityBodyContext(t, http.MethodPost, "application/json", wants[i].body, nil)
	}

	scopes := make([]AffinityScope, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := contexts[i]
			scopes[i] = ResolveAffinityScope(c)
			// 请求内只读引用：下游并发读取自身 body 不得越界到其他请求。
			got, err := io.ReadAll(c.Request.Body)
			if err != nil || !bytes.Equal(got, []byte(wants[i].body)) {
				// 并发中不使用 t.Fatalf（非测试 goroutine），用 t.Errorf 记录后继续。
				t.Errorf("request %d: downstream body mismatch", i)
			}
		}(i)
	}
	wg.Wait()

	seenTurnIDs := make(map[string]int, requests)
	for i := 0; i < requests; i++ {
		assert.Equal(t, wants[i].sessionID, scopes[i].SessionID, "请求 %d 的 SessionID 必须来自自身输入", i)
		assert.Equal(t, wants[i].turnID, scopes[i].TurnID, "请求 %d 的 TurnID 必须来自自身 body", i)
		assert.True(t, scopes[i].turnDerived, "请求 %d 派生成功时 turnDerived 必须为 true", i)
		seenTurnIDs[scopes[i].TurnID]++
	}
	assert.Len(t, seenTurnIDs, requests, "每个请求的派生 turn id 必须互不相同（证明无串号）")
}
