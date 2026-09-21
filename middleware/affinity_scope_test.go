package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAffinityTestContext 构造一个带指定请求头的 *gin.Context，供 ResolveAffinityScope 读取。
func newAffinityTestContext(t *testing.T, headers map[string]string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	return c
}

// --- A. ResolveAffinityScope ---

func TestResolveAffinityScope_TurnHeaderPriority(t *testing.T) {
	// 同时给 X-Conversation-Request-Id 与 X-Query-Id，应取优先级更高的前者。
	c := newAffinityTestContext(t, map[string]string{
		"X-Conversation-Request-Id": "conv-req-1",
		"X-Query-Id":                "query-1",
	})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "conv-req-1", scope.TurnID,
		"X-Conversation-Request-Id 应优先于 X-Query-Id")
}

func TestResolveAffinityScope_TurnAndSessionBothParsed(t *testing.T) {
	c := newAffinityTestContext(t, map[string]string{
		"X-Conversation-Request-Id": "turn-abc",
		"X-Session-Id":              "sess-xyz",
	})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "turn-abc", scope.TurnID, "TurnID 应被解析")
	assert.Equal(t, "sess-xyz", scope.SessionID, "SessionID 应被解析")
}

// 防回归核心：per-request / 遥测头绝不能当成 turn，否则亲和永不命中。
func TestResolveAffinityScope_PerRequestHeadersAreNotTurn(t *testing.T) {
	excluded := []string{
		"X-Request-Id",
		"X-Client-Request-Id",
		"x-amzn-trace-id",
		"X-Stainless-Request-Id",
		"x-ms-client-request-id",
		"x-goog-api-client",
		"x-is-human-initiated",
		"x-litellm-trace-id",
	}
	for _, name := range excluded {
		t.Run("only_"+name, func(t *testing.T) {
			c := newAffinityTestContext(t, map[string]string{
				name: "per-request-value",
			})
			scope := ResolveAffinityScope(c)
			assert.Equal(t, "", scope.TurnID, "%s 是 per-request/遥测语义，绝不能识别为 turn", name)
			assert.Equal(t, "", scope.SessionID, "%s 是 per-request/遥测语义，绝不能识别为 session", name)
		})
	}
}

func TestResolveAffinityScope_SessionOnlyLeavesTurnEmpty(t *testing.T) {
	t.Run("conversation_id style", func(t *testing.T) {
		c := newAffinityTestContext(t, map[string]string{
			"conversation_id": "sess-underscore",
		})
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.TurnID, "无 turn 头时 TurnID 必须为空（不得生成随机 id）")
		assert.Equal(t, "sess-underscore", scope.SessionID, "conversation_id 应被识别为 session")
	})

	t.Run("X-Session-Id style", func(t *testing.T) {
		c := newAffinityTestContext(t, map[string]string{
			"X-Session-Id": "sess-hyphen",
		})
		scope := ResolveAffinityScope(c)
		assert.Equal(t, "", scope.TurnID, "无 turn 头时 TurnID 必须为空")
		assert.Equal(t, "sess-hyphen", scope.SessionID, "X-Session-Id 应被识别为 session")
	})
}

// 业界调研补充的 5 个会话级头必须各自被识别为 session（且不得误判为 turn）。
func TestResolveAffinityScope_NewSessionHeaders(t *testing.T) {
	cases := map[string]string{
		"X-Claude-Code-Session-Id": "claude-sess",
		"X-Opencode-Session":       "opencode-sess",
		"X-Litellm-Session-Id":     "litellm-sess",
		"Acp-Connection-Id":        "acp-conn",
		"Acp-Session-Id":           "acp-sess",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			c := newAffinityTestContext(t, map[string]string{name: want})
			scope := ResolveAffinityScope(c)
			assert.Equal(t, want, scope.SessionID, "%s 应被识别为 session", name)
			assert.Equal(t, "", scope.TurnID, "%s 是会话级头，不得识别为 turn", name)
		})
	}
}

// HTTP 头名大小写不敏感（gin GetHeader → http.Header.Get → CanonicalMIMEHeaderKey），
// 客户端用小写形态发送同样必须识别。
func TestResolveAffinityScope_NewSessionHeadersCaseInsensitive(t *testing.T) {
	for _, name := range []string{
		"x-claude-code-session-id",
		"x-opencode-session",
		"x-litellm-session-id",
		"acp-connection-id",
		"acp-session-id",
	} {
		t.Run(name, func(t *testing.T) {
			c := newAffinityTestContext(t, map[string]string{name: "lower-value"})
			scope := ResolveAffinityScope(c)
			assert.Equal(t, "lower-value", scope.SessionID, "小写形态的 %s 也应识别为 session", name)
		})
	}
}

func TestResolveAffinityScope_NoHeaders(t *testing.T) {
	c := newAffinityTestContext(t, nil)

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "", scope.TurnID, "无相关头时 TurnID 为空")
	assert.Equal(t, "", scope.SessionID, "无相关头时 SessionID 为空")
}

func TestResolveAffinityScope_WhitespaceHeaderTreatedAsMissing(t *testing.T) {
	c := newAffinityTestContext(t, map[string]string{
		"X-Conversation-Request-Id": "   ",
		"X-Session-Id":              "  ",
	})

	scope := ResolveAffinityScope(c)

	assert.Equal(t, "", scope.TurnID, "纯空白 turn 头应视为缺失")
	assert.Equal(t, "", scope.SessionID, "纯空白 session 头应视为缺失")
}

func TestResolveAffinityScope_LongIDNormalizedTo16(t *testing.T) {
	long := strings.Repeat("a", 200)
	c := newAffinityTestContext(t, map[string]string{
		"X-Conversation-Request-Id": long,
	})

	scope := ResolveAffinityScope(c)

	assert.Len(t, scope.TurnID, 16, "超长 id 归一化后应为 sha256 前 16 位 hex")
}

func TestResolveAffinityScope_UserIDAndGroupFromContext(t *testing.T) {
	c := newAffinityTestContext(t, nil)
	c.Set(ctxkey.Id, 42)
	c.Set(ctxkey.Group, "vip")

	scope := ResolveAffinityScope(c)

	assert.Equal(t, 42, scope.UserID, "UserID 应取自 ctxkey.Id")
	assert.Equal(t, "vip", scope.Group, "Group 应取自 ctxkey.Group")
}

// --- B. Keys ---

func TestAffinityKeys_ThreeLevelsOrder(t *testing.T) {
	scope := AffinityScope{UserID: 1, Group: "g", TurnID: "t", SessionID: "s"}

	keys := scope.Keys("gpt-4")

	require.Len(t, keys, 3, "三层都可用时应返回 3 个键")
	assert.Equal(t, AffinityLevelTurn, keys[0].Level, "第 1 个键应为 turn")
	assert.Equal(t, AffinityLevelSession, keys[1].Level, "第 2 个键应为 session")
	assert.Equal(t, AffinityLevelUser, keys[2].Level, "第 3 个键应为 user")
}

func TestAffinityKeys_SessionOnlyNoTurn(t *testing.T) {
	scope := AffinityScope{UserID: 1, Group: "g", SessionID: "s"}

	keys := scope.Keys("gpt-4")

	require.Len(t, keys, 2, "只有 session 时应返回 session + user 两个键")
	assert.Equal(t, AffinityLevelSession, keys[0].Level, "第 1 个键应为 session")
	assert.Equal(t, AffinityLevelUser, keys[1].Level, "第 2 个键应为 user")
	for _, k := range keys {
		assert.NotEqual(t, AffinityLevelTurn, k.Level, "不得包含 turn 层")
	}
}

func TestAffinityKeys_UserOnly(t *testing.T) {
	scope := AffinityScope{UserID: 1, Group: "g"}

	keys := scope.Keys("gpt-4")

	require.Len(t, keys, 1, "都不可用时只返回 user 一个键")
	assert.Equal(t, AffinityLevelUser, keys[0].Level, "唯一键应为 user")
}

func TestAffinityKeys_TenantIsolation(t *testing.T) {
	base := AffinityScope{UserID: 1, Group: "g", TurnID: "t", SessionID: "s"}
	baseKeys := base.Keys("gpt-4")

	t.Run("different UserID yields different keys", func(t *testing.T) {
		other := AffinityScope{UserID: 2, Group: "g", TurnID: "t", SessionID: "s"}
		otherKeys := other.Keys("gpt-4")
		require.Len(t, otherKeys, len(baseKeys))
		for i := range baseKeys {
			assert.NotEqual(t, baseKeys[i].Value, otherKeys[i].Value,
				"UserID 不同时第 %d 层键必须不同（防跨租户串号）", i)
		}
	})

	t.Run("different Group yields different keys", func(t *testing.T) {
		other := AffinityScope{UserID: 1, Group: "g2", TurnID: "t", SessionID: "s"}
		otherKeys := other.Keys("gpt-4")
		require.Len(t, otherKeys, len(baseKeys))
		for i := range baseKeys {
			assert.NotEqual(t, baseKeys[i].Value, otherKeys[i].Value,
				"Group 不同时第 %d 层键必须不同（防跨租户串号）", i)
		}
	})
}

// --- B2. KeysToSet（成功转发后写入哪些键）---

func TestAffinityKeysToSet_TurnSessionUserWritesAll(t *testing.T) {
	scope := AffinityScope{UserID: 1, Group: "g", TurnID: "t", SessionID: "s"}

	toSet := scope.KeysToSet("gpt-4")

	require.Len(t, toSet, 3, "三层都可用时应写入全部可用层（turn + session + user）")
	assert.Equal(t, AffinityLevelTurn, toSet[0].Level, "首个键应为最细的 turn 层")
	assert.Equal(t, AffinityLevelSession, toSet[1].Level, "第二个键应为 session 层")
	assert.Equal(t, AffinityLevelUser, toSet[2].Level, "第三个键应为 user 兜底层")
}

func TestAffinityKeysToSet_SessionAndUserWritesBoth(t *testing.T) {
	scope := AffinityScope{UserID: 1, Group: "g", SessionID: "s"}

	toSet := scope.KeysToSet("gpt-4")

	require.Len(t, toSet, 2, "session + user 可用时应写入 session 与 user 两个键")
	assert.Equal(t, AffinityLevelSession, toSet[0].Level, "首个键应为 session 层")
	assert.Equal(t, AffinityLevelUser, toSet[1].Level, "第二个键应为 user 兜底层")
}

func TestAffinityKeysToSet_UserOnlyWritesUser(t *testing.T) {
	scope := AffinityScope{UserID: 1, Group: "g"}

	toSet := scope.KeysToSet("gpt-4")

	require.Len(t, toSet, 1, "都不可用时只写 user 一个键")
	assert.Equal(t, AffinityLevelUser, toSet[0].Level, "唯一键应为 user 层")
}

// 回退模式（AFFINITY_KEY_MODE=user）下 Keys 只产出 user 一个键，KeysToSet 行为不变。
func TestAffinityKeysToSet_UserFallbackModeWritesSingleKey(t *testing.T) {
	original := config.AffinityKeyMode
	defer func() { config.AffinityKeyMode = original }()
	config.AffinityKeyMode = "user"

	scope := AffinityScope{UserID: 1, Group: "g", TurnID: "t", SessionID: "s"}

	toSet := scope.KeysToSet("gpt-4")

	require.Len(t, toSet, 1, "回退模式下只写一个键")
	assert.Equal(t, AffinityLevelUser, toSet[0].Level, "唯一键应为 user 层")
}

// --- B3. ShouldRecordAffinity（成功转发是否需要写亲和）---

// P2-2：requestModel == "auto" 的请求走 autoDistribute（轮询选路，从不查亲和），
// 其亲和键在任何路径下都不会被 Get 命中 → 写入只会产生死键。必须跳过。
func TestShouldRecordAffinity_AutoIsSkipped(t *testing.T) {
	assert.False(t, ShouldRecordAffinity("auto"),
		"auto 走 autoDistribute、从不查亲和，写入只会产生永不命中的死键，必须跳过")
}

func TestShouldRecordAffinity_ConcreteModelIsRecorded(t *testing.T) {
	for _, model := range []string{"gpt-4", "deepseek-v4-flash", "claude-3-5-sonnet"} {
		assert.True(t, ShouldRecordAffinity(model),
			"具体模型 %q 会走 nonAutoDistribute 查亲和，必须记录", model)
	}
}

// 空串不在判定范围：生产调用方 controller/relay.go 在取 requestModel 时已把空串归一化为
// "auto"（relay.go:64-66），故空串永远不会到达此处。本函数只排除字面量 "auto"，
// 空串按「其它」处理返回 true，由调用方负责归一化——不在此臆造额外语义。
func TestShouldRecordAffinity_EmptyStringFollowsCaller(t *testing.T) {
	assert.True(t, ShouldRecordAffinity(""),
		"空串由调用方归一化为 auto，本函数只排除字面量 auto，空串按其它处理")
}

// 判定为字面量精确比较：大小写变体不是 auto 哨兵值（Distribute 只写小写 "auto"）。
func TestShouldRecordAffinity_OnlyLiteralAutoIsSkipped(t *testing.T) {
	for _, model := range []string{"AUTO", "Auto", " auto", "auto "} {
		assert.True(t, ShouldRecordAffinity(model),
			"%q 不是字面量 auto 哨兵，应视为具体模型记录（哨兵由 Distribute 以小写形态写入）", model)
	}
}

// --- C. AffinityManager ---

func TestAffinityManager_LevelPriority(t *testing.T) {
	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys("gpt-4")
	require.Len(t, keys, 3, "前置条件：三层键齐全")
	turnKey, sessionKey, userKey := keys[0], keys[1], keys[2]

	am := NewAffinityManager(60, 60, 60, 0)
	am.Set(turnKey, 100)
	am.Set(sessionKey, 200)
	am.Set(userKey, 300)

	ch, level, ok := am.Get(keys)
	require.True(t, ok, "应命中 turn 层")
	assert.Equal(t, 100, ch, "应返回 turn 层渠道")
	assert.Equal(t, AffinityLevelTurn, level, "命中层级应为 turn")

	am.Remove(turnKey)
	ch, level, ok = am.Get(keys)
	require.True(t, ok, "移除 turn 后应命中 session 层")
	assert.Equal(t, 200, ch, "应返回 session 层渠道")
	assert.Equal(t, AffinityLevelSession, level, "命中层级应为 session")

	am.Remove(sessionKey)
	ch, level, ok = am.Get(keys)
	require.True(t, ok, "移除 session 后应命中 user 层")
	assert.Equal(t, 300, ch, "应返回 user 层渠道")
	assert.Equal(t, AffinityLevelUser, level, "命中层级应为 user")
}

func TestAffinityManager_UserFallbackHit(t *testing.T) {
	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys("gpt-4")
	require.Len(t, keys, 3, "前置条件：三层键齐全")
	userKey := keys[2]

	am := NewAffinityManager(60, 60, 60, 0)
	am.Set(userKey, 300)

	ch, level, ok := am.Get(keys)
	require.True(t, ok, "只 Set user 层时，用含 turn 的 keys 查询应降级命中 user")
	assert.Equal(t, 300, ch, "应返回 user 层渠道")
	assert.Equal(t, AffinityLevelUser, level, "命中层级应为 user")
}

func TestAffinityManager_MissReturnsFalse(t *testing.T) {
	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys("gpt-4")

	am := NewAffinityManager(60, 60, 60, 0)

	ch, level, ok := am.Get(keys)
	assert.False(t, ok, "完全无记录时应未命中")
	assert.Equal(t, 0, ch, "未命中时渠道应为零值")
	assert.Equal(t, AffinityLevelUser, level, "未命中时 hitLevel 应为零值 AffinityLevelUser")
}

func TestAffinityManager_TieredTTLIndependent(t *testing.T) {
	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys("gpt-4")
	require.Len(t, keys, 3, "前置条件：三层键齐全")
	turnKey, userKey := keys[0], keys[2]

	// turn TTL = 1s，user TTL = 60s，验证两层 TTL 相互独立。
	am := NewAffinityManager(60, 1, 60, 0)
	am.Set(turnKey, 100)
	am.Set(userKey, 300)

	time.Sleep(1100 * time.Millisecond)

	_, level, ok := am.Get([]AffinityKey{turnKey})
	assert.False(t, ok, "turn 键 TTL=1s，1.1s 后应过期不命中")

	// map 级断言：Get 命中过期条目时必须真的把它从 map 中 delete（惰性删除），
	// 而不只是返回 miss。仅断言「不命中」无法发现 delete 分支失效。
	am.mu.Lock()
	_, stillPresent := am.entries[turnKey.Value]
	remaining := len(am.entries)
	am.mu.Unlock()
	assert.False(t, stillPresent, "过期 turn 条目应被 Get 惰性删除（map 级断言）")
	assert.Equal(t, 1, remaining, "删除过期 turn 条目后应仅剩未过期的 user 条目")

	ch, level, ok := am.Get([]AffinityKey{userKey})
	require.True(t, ok, "user 键 TTL=60s，仍应命中")
	assert.Equal(t, 300, ch, "应返回 user 层渠道")
	assert.Equal(t, AffinityLevelUser, level, "命中层级应为 user")
}

// 回归核心（P1-2）：turn 头按定义每轮换新。若成功转发只写最细可用层（turn），
// 下一轮 turn 键必然 miss，而 session / user 层从未写过也 miss → 每轮走加权随机，
// 跨轮亲和彻底断链（比改造前还有 user 级亲和更差）。本用例先以 (turn=A, session=S)
// 写入，再以 (turn=B, session=S) 读取，必须降级命中 session 层。
func TestAffinityManager_CrossTurnSessionFallback(t *testing.T) {
	model := "gpt-4"
	first := AffinityScope{UserID: 7, Group: "g", TurnID: "turn-A", SessionID: "session-S"}
	second := AffinityScope{UserID: 7, Group: "g", TurnID: "turn-B", SessionID: "session-S"}

	am := NewAffinityManager(60, 60, 60, 0)
	// 修复后生产写入路径：写入全部可用层（turn + session + user）。
	for _, k := range first.KeysToSet(model) {
		am.Set(k, 555)
	}

	ch, level, ok := am.Get(second.Keys(model))
	require.True(t, ok, "同一 session、下一轮 turn 应降级命中 session 层")
	assert.Equal(t, 555, ch, "应返回会话级写入的渠道")
	assert.Equal(t, AffinityLevelSession, level, "命中层级应为 session")
}

// turn-only 客户端跨轮亲和（无任何 session 头）：Keys=[turn, user]，KeysToSet 必须写入 user 层。
// 否则下一轮 turn id 换新后 turn 键 miss、user 层从未写过也 miss → 跨轮亲和断链，比改造前更差。
func TestAffinityManager_CrossTurnUserFallback_TurnOnlyClient(t *testing.T) {
	model := "gpt-4"
	first := AffinityScope{UserID: 7, Group: "g", TurnID: "turn-A"}  // 只有 turn 头
	second := AffinityScope{UserID: 7, Group: "g", TurnID: "turn-B"} // 下一轮：turn 换新

	keys := first.Keys(model)
	require.Len(t, keys, 2, "turn-only 客户端 Keys 应为 [turn, user]")
	require.Equal(t, AffinityLevelTurn, keys[0].Level, "keys[0] 应为 turn 层")
	require.Equal(t, AffinityLevelUser, keys[1].Level, "keys[1] 应为 user 兜底层")

	am := NewAffinityManager(60, 60, 60, 0)
	for _, k := range first.KeysToSet(model) {
		am.Set(k, 777)
	}

	ch, level, ok := am.Get(second.Keys(model))
	require.True(t, ok, "turn-only 客户端下一轮必须降级命中 user 层，否则跨轮亲和断链")
	assert.Equal(t, 777, ch, "应返回上一轮写入的渠道")
	assert.Equal(t, AffinityLevelUser, level, "命中层级应为 user")
}

func TestAffinityManager_CapacityEviction(t *testing.T) {
	am := NewAffinityManager(60, 60, 60, 4)

	for i := 0; i < 10; i++ {
		am.Set(AffinityKey{Level: AffinityLevelUser, Value: string(rune('a' + i))}, i)
	}

	am.mu.Lock()
	n := len(am.entries)
	am.mu.Unlock()
	assert.LessOrEqual(t, n, 4, "容量上限 4，塞入 10 个不同键后 entries 不应超过 4")

	// 读写不应 panic，且未淘汰的键仍可命中。
	_, _, _ = am.Get([]AffinityKey{{Level: AffinityLevelUser, Value: "j"}})
	am.Set(AffinityKey{Level: AffinityLevelUser, Value: "z"}, 999)
}

// P2-1：maxEntries<=0 必须兜底为默认容量，否则 Set 内 am.maxEntries > 0 的守卫静默失效，
// 亲和表退化为无界增长。
func TestNewAffinityManager_MaxEntriesFallback(t *testing.T) {
	for _, maxEntries := range []int{0, -1, -1000} {
		am := NewAffinityManager(60, 60, 60, maxEntries)
		assert.Equal(t, config.DefaultAffinityMaxEntries, am.maxEntries,
			"maxEntries=%d 应兜底为默认容量", maxEntries)
		assert.Greater(t, am.maxEntries, 0, "兜底后 maxEntries 必须为正，容量守卫才生效")
	}

	// 正数配置不得被改写。
	am := NewAffinityManager(60, 60, 60, 4)
	assert.Equal(t, 4, am.maxEntries, "正数 maxEntries 应原样保留")
}

func TestAffinityLevel_String(t *testing.T) {
	assert.Equal(t, "user", AffinityLevelUser.String(), "AffinityLevelUser 应返回 user")
	assert.Equal(t, "session", AffinityLevelSession.String(), "AffinityLevelSession 应返回 session")
	assert.Equal(t, "turn", AffinityLevelTurn.String(), "AffinityLevelTurn 应返回 turn")
}

// 守护设计约定：生产写入路径（controller/relay.go）遍历 scope.KeysToSet(model)，
// 即写入「全部可用层」（turn + session + user）。绝不再只写 keys[0]：turn id 每轮换新，
// 只写最细层会让跨轮亲和断链（见 TestAffinityManager_CrossTurnSessionFallback 与
// TestAffinityManager_CrossTurnUserFallback_TurnOnlyClient）。
// 本用例断言 turn / session / user 三层都被写入。
func TestAffinityManager_KeysToSetWritesAllAvailableLevels(t *testing.T) {
	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys("gpt-4")
	require.Len(t, keys, 3, "前置条件：三层键齐全")
	turnKey, sessionKey, userKey := keys[0], keys[1], keys[2]
	require.Equal(t, AffinityLevelTurn, turnKey.Level, "keys[0] 应是最细的 turn 层")

	toSet := scope.KeysToSet("gpt-4")
	require.Len(t, toSet, 3, "应写入全部可用层（turn + session + user）三个键")
	assert.Equal(t, turnKey, toSet[0], "首个键应为最细的 turn 层")
	assert.Equal(t, sessionKey, toSet[1], "第二个键应为 session 层")
	assert.Equal(t, userKey, toSet[2], "第三个键应为 user 兜底层")

	am := NewAffinityManager(60, 60, 60, 0)
	for _, k := range toSet {
		am.Set(k, 123)
	}

	ch, level, ok := am.Get(keys)
	require.True(t, ok, "写入 turn 层后应命中")
	assert.Equal(t, 123, ch, "应返回写入的渠道")
	assert.Equal(t, AffinityLevelTurn, level, "命中层级应为 turn")

	_, _, sessionOk := am.Get([]AffinityKey{sessionKey})
	assert.True(t, sessionOk, "session 层必须被写入（供下一轮 turn 换新后降级命中）")

	_, _, userOk := am.Get([]AffinityKey{userKey})
	assert.True(t, userOk, "user 层必须被写入（细层 id 轮换/过期时的最终兜底，否则亲和整体断链）")
}

// --- D. 回退开关 ---

func TestAffinityKeyModeUserFallback(t *testing.T) {
	original := config.AffinityKeyMode
	defer func() { config.AffinityKeyMode = original }()

	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}

	// 回退开关匹配应为大小写不敏感 + TrimSpace 容错：USER / User / 带空格 都应生效。
	for _, mode := range []string{"user", "USER", "User", " user "} {
		t.Run("mode="+mode, func(t *testing.T) {
			config.AffinityKeyMode = mode

			keys := scope.Keys("gpt-4")

			require.Len(t, keys, 1, "AFFINITY_KEY_MODE=%q 时应只返回 user 层一个键", mode)
			assert.Equal(t, AffinityLevelUser, keys[0].Level, "唯一键应为 user 层")
		})
	}
}

// 回退开关在 manager 读写入口上的闭合性：user 模式下写入的是 user 键，
// 用 turn 层 key 查不到，用 user 层 key 能查到。
func TestAffinityKeyModeUserFallback_RoundTrip(t *testing.T) {
	original := config.AffinityKeyMode
	defer func() { config.AffinityKeyMode = original }()

	config.AffinityKeyMode = "user"

	scope := AffinityScope{UserID: 7, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys("gpt-4")
	require.Len(t, keys, 1, "回退模式下 Keys 只产出 user 层一个键")

	am := NewAffinityManager(60, 60, 60, 0)
	am.Set(keys[0], 456)

	ch, level, ok := am.Get(keys)
	require.True(t, ok, "回退模式下 Set 后应能取回")
	assert.Equal(t, 456, ch, "应取回同一渠道")
	assert.Equal(t, AffinityLevelUser, level, "命中层级应为 user")

	// 证明回退开关在读写入口闭合：该模式不会生成 turn 键，写入也只落 user 键，
	// 因此用 turn 层 key 查询不得命中。
	turnKey := AffinityKey{
		Level: AffinityLevelTurn,
		Value: buildAffinityKeyValue(AffinityLevelTurn, scope.Group, scope.UserID, "gpt-4", scope.TurnID),
	}
	_, _, turnOk := am.Get([]AffinityKey{turnKey})
	assert.False(t, turnOk, "回退模式下不应写入 turn 层，查 turn 层不得命中")

	_, _, userOk := am.Get([]AffinityKey{keys[0]})
	assert.True(t, userOk, "user 层键应命中")
}

// P2-2：回退模式的键必须与改造前 buildKey 的 fmt.Sprintf("%d:%s", userId, model)
// 逐字节等价（不含 group），否则同一用户跨 group 不再共享亲和。
func TestAffinityKeyModeUserFallback_KeyFormatByteEqual(t *testing.T) {
	original := config.AffinityKeyMode
	defer func() { config.AffinityKeyMode = original }()
	config.AffinityKeyMode = "user"

	// group / turn / session 都不应进入键值。
	scope := AffinityScope{UserID: 42, Group: "ignored-group", TurnID: "t", SessionID: "s"}
	keys := scope.Keys("gpt-4")

	require.Len(t, keys, 1, "回退模式只产出一个键")
	assert.Equal(t, fmt.Sprintf("%d:%s", 42, "gpt-4"), keys[0].Value,
		"回退模式键必须与旧格式 %%d:%%s 逐字节等价（不含 group/level）")
	assert.NotContains(t, keys[0].Value, "ignored-group", "回退模式键不得含 group")

	// 同一用户、不同 group 必须得到完全相同的键（旧行为：跨 group 共享亲和）。
	other := AffinityScope{UserID: 42, Group: "another-group", TurnID: "t2", SessionID: "s2"}
	assert.Equal(t, keys[0].Value, other.Keys("gpt-4")[0].Value,
		"回退模式下同一用户跨 group 必须得到同一个键")
}
