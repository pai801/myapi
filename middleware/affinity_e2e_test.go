package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件是亲和改造的**端到端**回归：每条用例都从真实 HTTP 请求头出发，经真实入口
// ResolveAffinityScope 解析成亲和键，再走真实选路 nonAutoDistribute（与 Distribute 内部
// 调用的同一函数），最后按生产写法（controller/relay.go recordSuccessAttribution）写亲和。
//
// 边界（务必知悉，别误以为本文件验证了生产写入路径）：本文件只覆盖**中间件层**链路
// —— 真实头 → ResolveAffinityScope → nonAutoDistribute（读侧选路 + 键命名空间契约）。
// 它**不覆盖** controller/relay.go 的写入路径 recordSuccessAttribution（含 sticky 覆盖后按
// ActualChannelId 归因、ShouldRecordAffinity 跳过 auto 等分支）——那是 controller 层职责，
// 由 controller/relay_actual_channel_test.go 覆盖。此处写亲和只是用 recordAffinityE2E
// **手工复刻**生产写入循环（遍历 scope.KeysToSet 逐键 Set），以闭环验证「读到的键 = 写下的键」，
// 并不等于调用了 recordSuccessAttribution。这是中间件层测试的固有边界。
//
// 为什么不用 Distribute() / SelectChannel()：二者内部走 model.CacheGetGroupChannels，
// 依赖渠道缓存与 DB，单测里要额外搭内存库并改 model.DB / config.MemoryCacheEnabled 等全局状态，
// 既脆弱又存在顺序相关的偶发失败。nonAutoDistribute 接收字面量构造的渠道切片，无需任何全局缓存，
// 链路同样真实（真实头 → 真实 scope → 真实选路），且完全确定性。
//
// 背景：改造期间两次出现「测试全绿但没碰到真实路径」的虚假信心事故，故这里坚持：
//  1. 请求头用真实头名（X-Conversation-Request-Id / X-Session-Id），绝不手工拼 AffinityScope；
//  2. 候选渠道 ≥3 个，并用反向采样证明「两次相等」不是候选集过小的巧合；
//  3. 每次都重新解析 scope，绝不复用上一次的对象。
const (
	affinityE2EUserA = 920001
	affinityE2EUserB = 920002
	affinityE2EUserC = 920003 // fallback 用例专用（亲和渠道不在候选集）
	affinityE2EUserD = 920004 // 超长 session 头一致性用例专用
	affinityE2EGroup = "default"
	affinityE2EModel = "gpt-4-turbo"
	affinityE2EAlias = "gpt4turbo"
)

// affinityE2EChannels 返回 3 个支持同一模型的候选渠道：候选只有 1 个会让「两次相等」恒真，
// 必须 ≥2，这里取 3 以让反向采样有足够区分度（无亲和时每次约 1/3 命中同一渠道）。
func affinityE2EChannels() []*model.Channel {
	return []*model.Channel{
		{Id: 9201, Name: "e2e-A", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias},
		{Id: 9202, Name: "e2e-B", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias},
		{Id: 9203, Name: "e2e-C", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias},
	}
}

// affinityE2E 是单条用例的测试装置：记录解析出的 scope，用例结束时统一清理全局亲和表，
// 保证不污染同包其它用例（既有用例共享全局 AffinityGlobal）。
type affinityE2E struct {
	t      *testing.T
	scopes []AffinityScope
}

func newAffinityE2E(t *testing.T) *affinityE2E {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &affinityE2E{t: t}
	t.Cleanup(h.cleanup)
	return h
}

// cleanup 复用既有 clearAffinity()，并额外逐键删除本文件写入的 turn/session/user 三层键：
// clearAffinity 只清 user 999 的 user 层键，覆盖不到本文件带 turn/session 头的分层键，
// 因此必须按 KeysToSet 逐键补删，才能做到「零残留」。
func (h *affinityE2E) cleanup() {
	clearAffinity()
	for _, scope := range h.scopes {
		for _, key := range scope.KeysToSet(affinityE2EModel) {
			AffinityGlobal.Remove(key)
		}
	}
}

// newAffinityE2EContext 构造带真实请求头、真实 userId / group 的 gin.Context，
// 供 ResolveAffinityScope 从真实头解析 scope。
//
// 用 POST 但**不设** Content-Type：sessionIDFromBody 只在 application/json 时才读 body，
// 故此处只验证请求头路径，不会因读 body 引入额外分支。
func newAffinityE2EContext(t *testing.T, userID int, headers map[string]string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	c.Set(ctxkey.Id, userID)
	c.Set(ctxkey.Group, affinityE2EGroup)
	return c
}

// request 走真实链路：真实请求头 → ResolveAffinityScope → nonAutoDistribute。
// 每次调用都新建 context 并重新解析 scope，绝不复用上一次的 scope。
func (h *affinityE2E) request(userID int, headers map[string]string) (AffinityScope, *model.Channel) {
	h.t.Helper()
	return h.requestWithChannels(userID, headers, affinityE2EChannels())
}

// requestWithChannels 同 request，但允许指定候选渠道集，用于验证「亲和指向的渠道不在候选集内 → 回落」。
func (h *affinityE2E) requestWithChannels(userID int, headers map[string]string, channels []*model.Channel) (AffinityScope, *model.Channel) {
	h.t.Helper()

	scope := ResolveAffinityScope(newAffinityE2EContext(h.t, userID, headers))
	h.scopes = append(h.scopes, scope)

	ch, _, err := nonAutoDistribute(context.Background(), scope, affinityE2EModel, channels)
	require.NoError(h.t, err, "nonAutoDistribute 选路失败")
	require.NotNil(h.t, ch)
	return scope, ch
}

// recordAffinityE2E 按生产写法（controller/relay.go recordSuccessAttribution）写入全部可用层。
func recordAffinityE2E(scope AffinityScope, ch *model.Channel) {
	for _, key := range scope.KeysToSet(affinityE2EModel) {
		AffinityGlobal.Set(key, ch.Id)
	}
}

// TestAffinityE2E_HeaderSameSessionSticks 从真实请求头出发验证：
// 同一 turn + 同一 session 连续两次请求必须命中同一渠道（turn 层生效）。
func TestAffinityE2E_HeaderSameSessionSticks(t *testing.T) {
	h := newAffinityE2E(t)
	headers := map[string]string{
		"X-Conversation-Request-Id": "turn-1",
		"X-Session-Id":              "sess-1",
	}

	// 第 1 次请求：真实头 → 解析 scope → 选路 → 按生产写法写亲和
	scope1, ch1 := h.request(affinityE2EUserA, headers)
	require.Equal(t, "turn-1", scope1.TurnID, "真实 turn 头必须被解析进 scope")
	require.Equal(t, "sess-1", scope1.SessionID, "真实 session 头必须被解析进 scope")
	require.Equal(t, affinityE2EUserA, scope1.UserID)
	// Group 与 Distribute() 同源：生产里 Distribute 把 ctxkey.Group 空值归一化为 "default"
	// 并回写 scope.Group，此处显式设为 affinityE2EGroup，断言它确实进入了键命名空间，
	// 避免将来 group 悄悄偏离、导致亲和键串命名空间（键值形如 level|group|uid|model|id）。
	require.Equal(t, affinityE2EGroup, scope1.Group, "scope.Group 必须与选路分组同源")
	recordAffinityE2E(scope1, ch1)

	// 第 2 次请求：同样头，**重新**解析 scope（不复用 scope1），必须命中同一渠道
	scope2, ch2 := h.request(affinityE2EUserA, headers)
	require.Equal(t, scope1.TurnID, scope2.TurnID, "两次请求应解析出同一 turn id")
	assert.Equal(t, ch1.Id, ch2.Id, "同一 turn+session 的第二次请求必须命中同一渠道（turn 层亲和）")

	// 佐证命中层级确为 turn（而非退化为更粗层）
	_, level, ok := AffinityGlobal.Get(scope2.Keys(affinityE2EModel))
	require.True(t, ok, "第二次请求应能查到亲和")
	assert.Equal(t, AffinityLevelTurn, level, "应命中 turn 层")
}

// TestAffinityE2E_HeaderNewTurnFallsBackToSession 验证换 turn 后回落 session 层：
// turn 头换成新值、session 头不变，仍应命中同一渠道。
func TestAffinityE2E_HeaderNewTurnFallsBackToSession(t *testing.T) {
	h := newAffinityE2E(t)

	first := map[string]string{
		"X-Conversation-Request-Id": "turn-A",
		"X-Session-Id":              "sess-stable",
	}
	scope1, ch1 := h.request(affinityE2EUserA, first)
	recordAffinityE2E(scope1, ch1)

	// 下一轮：turn 换新，session 不变
	second := map[string]string{
		"X-Conversation-Request-Id": "turn-B",
		"X-Session-Id":              "sess-stable",
	}
	scope2, ch2 := h.request(affinityE2EUserA, second)
	require.NotEqual(t, scope1.TurnID, scope2.TurnID, "前置条件：两次 turn 必须不同")
	require.Equal(t, scope1.SessionID, scope2.SessionID, "前置条件：两次 session 相同")
	assert.Equal(t, ch1.Id, ch2.Id, "turn 换新后应回落 session 层，仍命中同一渠道")

	// 佐证确实走的是 session 层（turn 层已 miss）
	_, level, ok := AffinityGlobal.Get(scope2.Keys(affinityE2EModel))
	require.True(t, ok, "换 turn 后应能查到亲和")
	assert.Equal(t, AffinityLevelSession, level, "应回落命中 session 层")
}

// TestAffinityE2E_HeaderNoSessionFallsBackToUser 验证无任何 turn/session 头时回落 user 层：
// 同一 userId 的后续请求仍命中同一渠道。
func TestAffinityE2E_HeaderNoSessionFallsBackToUser(t *testing.T) {
	h := newAffinityE2E(t)

	withSession := map[string]string{
		"X-Conversation-Request-Id": "turn-1",
		"X-Session-Id":              "sess-1",
	}
	scope1, ch1 := h.request(affinityE2EUserA, withSession)
	recordAffinityE2E(scope1, ch1)

	// 不带任何 turn / session 头：只应产出 user 兜底键
	scope2, ch2 := h.request(affinityE2EUserA, nil)
	assert.Empty(t, scope2.TurnID, "无 turn 头时 TurnID 必须为空")
	assert.Empty(t, scope2.SessionID, "无 session 头时 SessionID 必须为空")
	require.Len(t, scope2.Keys(affinityE2EModel), 1, "无头时应只产出 user 兜底键")
	assert.Equal(t, ch1.Id, ch2.Id, "无 turn/session 头时应回落 user 层，命中同一渠道")

	// 佐证命中层级确为 user
	_, level, ok := AffinityGlobal.Get(scope2.Keys(affinityE2EModel))
	require.True(t, ok, "无头请求应能查到 user 层亲和")
	assert.Equal(t, AffinityLevelUser, level, "应命中 user 层")
}

// TestAffinityE2E_HeaderCrossUserIsolation 验证跨用户不串号：
// 不同 userId 带**相同** session 头，不应命中对方写入的亲和键。
func TestAffinityE2E_HeaderCrossUserIsolation(t *testing.T) {
	h := newAffinityE2E(t)

	session := map[string]string{
		"X-Conversation-Request-Id": "turn-shared",
		"X-Session-Id":              "sess-shared",
	}

	// 用户 A 建立亲和
	scopeA, chA := h.request(affinityE2EUserA, session)
	recordAffinityE2E(scopeA, chA)

	// 用户 B 用**完全相同**的 session 头解析 scope：键按 userId 命名空间隔离，必须查不到 A 的亲和。
	// 直接断言 Get 结果，避免「随机选路恰好分到不同渠道」带来的偶真。
	scopeB, _ := h.request(affinityE2EUserB, session)
	_, _, ok := AffinityGlobal.Get(scopeB.Keys(affinityE2EModel))
	assert.False(t, ok, "不同 userId 带相同 session 头，绝不应命中用户 A 写入的亲和键")

	// 键值本身也必须不同（命名空间含 userId），进一步锁定隔离性
	assert.NotEqual(t, scopeA.Keys(affinityE2EModel)[0].Value, scopeB.Keys(affinityE2EModel)[0].Value,
		"不同用户的亲和键值必须不同")
}

// TestAffinityE2E_HeaderReverseProof_NoAffinityIsUnstable 反向验证（永久用例）：
// 不写亲和时，同一用户同一 session 的多次选路不应稳定等于首次渠道。
// 这证明候选集足够大、选路确实是随机的，从而正向用例里的「两次相等」是亲和造成的，
// 而不是「候选集只有 1 个渠道」的恒真巧合。
func TestAffinityE2E_HeaderReverseProof_NoAffinityIsUnstable(t *testing.T) {
	h := newAffinityE2E(t)

	headers := map[string]string{
		"X-Conversation-Request-Id": "turn-1",
		"X-Session-Id":              "sess-1",
	}

	// 首次选路（不写亲和），作为比较基准
	_, base := h.request(affinityE2EUserA, headers)

	const rounds = 30
	distinct := map[int]struct{}{}
	for i := 0; i < rounds; i++ {
		_, ch := h.request(affinityE2EUserA, headers)
		distinct[ch.Id] = struct{}{}
	}

	// 3 个候选、无亲和时随机选路：P(30 次全同) = (1/3)^29 ≈ 1e-14，几乎不可能恒同。
	t.Logf("无亲和采样统计：%d 轮落在 %d 个不同渠道（基准=%d）", rounds, len(distinct), base.Id)
	assert.Greater(t, len(distinct), 1,
		"无亲和时 %d 个候选渠道的随机选路必须出现多个渠道；若恒同说明候选集过小，正向断言无意义",
		len(affinityE2EChannels()))
}

// TestAffinityE2E_HeaderFallbackWhenAffinityChannelNotInCandidates 验证「亲和指向的渠道不在
// 本次候选集」时的回落：必须回落加权随机，且**仍返回一个有效渠道**（不 nil、不报错）。
//
// 构造：先用只含 X 的候选集选路（单候选必得 X）并写亲和；再用不含 X 的 [A,B,C] 集选路。
// 第二次亲和命中 X，但 X 不在候选集 → nonAutoDistribute 走 recordFallback 分支并 weightedRandomSelect。
func TestAffinityE2E_HeaderFallbackWhenAffinityChannelNotInCandidates(t *testing.T) {
	h := newAffinityE2E(t)
	headers := map[string]string{"X-Session-Id": "sess-fallback"}

	// 第一次：候选集只含 X → 选路必得 X → 写亲和
	x := &model.Channel{Id: 9301, Name: "fallback-X", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias}
	scope1, ch1 := h.requestWithChannels(affinityE2EUserC, headers, []*model.Channel{x})
	require.Equal(t, x.Id, ch1.Id, "单候选集选路必得 X")
	recordAffinityE2E(scope1, ch1)

	// 前置：亲和确实指向 X
	affCh, _, ok := AffinityGlobal.Get(scope1.Keys(affinityE2EModel))
	require.True(t, ok, "写入后应能查到亲和")
	require.Equal(t, x.Id, affCh, "前置条件：亲和应指向 X")

	// 第二次：候选集不含 X → 必须回落且返回有效渠道
	candidates := []*model.Channel{
		{Id: 9302, Name: "fallback-A", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias},
		{Id: 9303, Name: "fallback-B", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias},
		{Id: 9304, Name: "fallback-C", Models: affinityE2EModel, ModelsAlias: affinityE2EAlias},
	}
	scope2, ch2 := h.requestWithChannels(affinityE2EUserC, headers, candidates)

	require.NotNil(t, ch2, "亲和渠道不在候选集时必须回落，且仍返回有效渠道（不能 nil）")
	assert.NotEqual(t, x.Id, ch2.Id, "亲和渠道 X 不在候选集，返回渠道绝不能是 X")
	inSet := false
	for _, c := range candidates {
		if c.Id == ch2.Id {
			inSet = true
		}
	}
	assert.True(t, inSet, "回落选出的渠道必须来自本次候选集")
	require.Equal(t, scope1.SessionID, scope2.SessionID, "两次应解析出同一 session")
}

// TestAffinityE2E_HeaderOverlongSessionIDStable 验证超长（>64 字符）session 头的一致性：
// normalizeAffinityID 把超过 affinityIDMaxLen(64) 的值截断为 16 位 sha256 hex；
// 同一个超长头两次请求必须解析出相同 SessionID，且第二次仍命中同一渠道。
func TestAffinityE2E_HeaderOverlongSessionIDStable(t *testing.T) {
	h := newAffinityE2E(t)

	long := strings.Repeat("a", 200) // > 64，必被 sha256 截断
	headers := map[string]string{"X-Session-Id": long}

	scope1, ch1 := h.request(affinityE2EUserD, headers)
	require.Len(t, scope1.SessionID, 16, "超长 session 头应被截断为 16 位 sha256 hex")
	require.NotEqual(t, long, scope1.SessionID, "截断后不应等于原始超长值")
	recordAffinityE2E(scope1, ch1)

	// 第二次：同一超长头重新解析，SessionID 必须一致 → session 键一致 → 命中同一渠道
	scope2, ch2 := h.request(affinityE2EUserD, headers)
	assert.Equal(t, scope1.SessionID, scope2.SessionID, "同一超长头两次解析出的 SessionID 必须相同")
	assert.Equal(t, ch1.Id, ch2.Id, "超长 session 头归一化后第二次仍应命中同一渠道")

	_, level, ok := AffinityGlobal.Get(scope2.Keys(affinityE2EModel))
	require.True(t, ok, "第二次应能查到亲和")
	assert.Equal(t, AffinityLevelSession, level, "应命中 session 层")
}
