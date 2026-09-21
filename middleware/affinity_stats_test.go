package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"github.com/stretchr/testify/require"
)

// infofCaptureLogger 替换全局 logger.Log，捕获 flush 汇总走的 Infof。
// Debugf 置为 no-op：nonAutoDistribute 在命中/未命中分支会打 debug 日志，
// 内嵌 nil 接口会 panic，故显式覆盖。
type infofCaptureLogger struct {
	logger.ILogger

	mu  sync.Mutex
	buf strings.Builder
}

func (l *infofCaptureLogger) Infof(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.WriteString(fmt.Sprintf(format, args...))
	l.buf.WriteString("\n")
}

func (l *infofCaptureLogger) Debugf(string, ...interface{}) {}

func (l *infofCaptureLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func withInfofCapture(t *testing.T) *infofCaptureLogger {
	t.Helper()
	original := logger.Log
	c := &infofCaptureLogger{}
	logger.Log = c
	t.Cleanup(func() { logger.Log = original })
	return c
}

// withStatsInterval 覆盖统计输出周期并在用例结束后还原。
func withStatsInterval(t *testing.T, seconds int) {
	t.Helper()
	original := config.AffinityStatsIntervalSeconds
	config.AffinityStatsIntervalSeconds = seconds
	t.Cleanup(func() { config.AffinityStatsIntervalSeconds = original })
}

// withStatsClock 覆盖统计时钟，测试据此推进时间而不 sleep。
func withStatsClock(t *testing.T, now func() time.Time) {
	t.Helper()
	original := affinityStatsNow
	affinityStatsNow = now
	t.Cleanup(func() { affinityStatsNow = original })
}

// resetGlobalAffinityStats 归零全局观测器，隔离用例间相互影响。
func resetGlobalAffinityStats() {
	s := affinityStatsGlobal
	s.turnHits.Store(0)
	s.turnDerivedHits.Store(0)
	s.sessionHits.Store(0)
	s.userHits.Store(0)
	s.fallbacks.Store(0)
	s.misses.Store(0)
	for i := range s.headerCounts {
		s.headerCounts[i].Store(0)
	}
	s.lastFlushNano.Store(0)
}

func candidateIndex(t *testing.T, name string) int {
	t.Helper()
	for i, n := range affinityCandidateHeaders {
		if n == name {
			return i
		}
	}
	t.Fatalf("候选头 %q 不在 affinityCandidateHeaders 中", name)
	return -1
}

// --- 1. 命中分层计数 ---

func TestAffinityStats_HitCountsPerLevelIndependent(t *testing.T) {
	// 周期设得远大于用例耗时：开启统计但不触发输出，纯计数。
	withStatsInterval(t, 3600)
	s := newAffinityStats()

	s.recordHit(AffinityLevelTurn, false)
	s.recordHit(AffinityLevelTurn, false)
	s.recordHit(AffinityLevelSession, false)
	s.recordHit(AffinityLevelUser, false)

	require.Equal(t, int64(2), s.turnHits.Load(), "turn 命中应计 2 次")
	require.Equal(t, int64(1), s.sessionHits.Load(), "session 命中应计 1 次，不受 turn 计数污染")
	require.Equal(t, int64(1), s.userHits.Load(), "user 命中应计 1 次")
	require.Equal(t, int64(0), s.misses.Load(), "命中不应计入 miss")
}

// --- 2. 未命中计数 + 头名采样 ---

func TestAffinityStats_MissCountsAndSamplesHeaderNames(t *testing.T) {
	withStatsInterval(t, 3600)
	s := newAffinityStats()

	h := http.Header{}
	h.Set("X-Session-Id", "SECRET-SESSION-VALUE")
	h.Set("X-Query-Id", "SECRET-QUERY-VALUE")
	// per-request 头不是候选头，不得被采样
	h.Set("X-Request-Id", "per-request-value")

	s.recordMiss(h)
	s.recordMiss(nil) // nil 头表不得 panic

	require.Equal(t, int64(2), s.misses.Load(), "两次未命中应计 2 次")
	require.Equal(t, int64(1), s.headerCounts[candidateIndex(t, "X-Session-Id")].Load(),
		"本次未命中带了 X-Session-Id，应计 1 次")
	require.Equal(t, int64(1), s.headerCounts[candidateIndex(t, "X-Query-Id")].Load(),
		"本次未命中带了 X-Query-Id，应计 1 次")
	require.Equal(t, int64(0), s.headerCounts[candidateIndex(t, "conversation_id")].Load(),
		"未出现的候选头计数应为 0")

	// 非候选头（X-Request-Id）不在候选清单中，天然不会被采样到
	for _, name := range affinityCandidateHeaders {
		require.NotEqual(t, "X-Request-Id", name, "X-Request-Id 是 per-request 头，不得进入候选采样清单")
	}
}

// --- 3. 并发计数（go test -race 验证）---

func TestAffinityStats_ConcurrentCounting(t *testing.T) {
	// 显式开启统计且周期远大于用例耗时：让并发 goroutine 走 maybeFlush 的原子 CAS/Load 路径；
	// 时钟不推进，不会真的 flush。不依赖环境变量默认值——否则运行环境若设了
	// AFFINITY_STATS_INTERVAL_SECONDS=0，埋点入口直接 return 会让计数全 0 而假红。
	withStatsInterval(t, 3600)
	s := newAffinityStats()
	const perLevel = 300
	var wg sync.WaitGroup
	for i := 0; i < perLevel*3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				s.recordHit(AffinityLevelTurn, false)
			case 1:
				s.recordHit(AffinityLevelSession, false)
			default:
				s.recordHit(AffinityLevelUser, false)
			}
		}(i)
	}
	for i := 0; i < perLevel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.recordMiss(http.Header{"X-Session-Id": {"v"}})
		}()
	}
	wg.Wait()

	require.Equal(t, int64(perLevel), s.turnHits.Load(), "并发下 turn 计数必须精确")
	require.Equal(t, int64(perLevel), s.sessionHits.Load(), "并发下 session 计数必须精确")
	require.Equal(t, int64(perLevel), s.userHits.Load(), "并发下 user 计数必须精确")
	require.Equal(t, int64(perLevel), s.misses.Load(), "并发下 miss 计数必须精确")
	require.Equal(t, int64(perLevel), s.headerCounts[candidateIndex(t, "X-Session-Id")].Load(),
		"并发下头名采样计数必须精确")
}

// --- 4. 周期输出后计数重置 ---

func TestAffinityStats_FlushResetsCountersAndLogs(t *testing.T) {
	captured := withInfofCapture(t)
	withStatsInterval(t, 1)
	fake := time.Unix(2000, 0)
	withStatsClock(t, func() time.Time { return fake })

	s := newAffinityStats()
	s.recordHit(AffinityLevelTurn, false)
	s.recordHit(AffinityLevelSession, false)
	s.recordMiss(nil)
	// 首个埋点只打点窗口起点，此时不应输出
	require.NotContains(t, captured.joined(), "affinity stats", "周期未到不得输出")

	fake = fake.Add(time.Second) // 到达 1s 周期
	s.recordHit(AffinityLevelUser, false)

	require.Equal(t, int64(0), s.turnHits.Load(), "输出后 turn 计数应重置为 0")
	require.Equal(t, int64(0), s.sessionHits.Load(), "输出后 session 计数应重置为 0")
	require.Equal(t, int64(0), s.userHits.Load(), "输出后 user 计数应重置为 0")
	require.Equal(t, int64(0), s.misses.Load(), "输出后 miss 计数应重置为 0")

	out := captured.joined()
	require.Contains(t, out, "affinity stats", "到点应输出一行汇总")
	require.Contains(t, out, "turn=1", "汇总应含本窗口 turn 命中数")
	require.Contains(t, out, "turn_derived=0", "汇总应含派生 turn 独立字段，未派生时为 0")
	require.Contains(t, out, "session=1", "汇总应含本窗口 session 命中数")
	require.Contains(t, out, "user=1", "汇总应含本窗口 user 命中数")
	require.Contains(t, out, "miss=1", "汇总应含本窗口 miss 数")
}

// --- 5. 红线：采样绝不记头值 ---

func TestAffinityStats_FlushDoesNotLogHeaderValues(t *testing.T) {
	captured := withInfofCapture(t)
	withStatsInterval(t, 1)
	fake := time.Unix(3000, 0)
	withStatsClock(t, func() time.Time { return fake })

	s := newAffinityStats()
	h := http.Header{}
	h.Set("X-Session-Id", "SECRET-SESSION-VALUE")
	h.Set("X-Conversation-Request-Id", "SECRET-TURN-VALUE")

	s.recordMiss(h) // 首个埋点：初始化窗口
	fake = fake.Add(2 * time.Second)
	s.recordMiss(h) // 到点：输出并重置

	out := captured.joined()
	require.Contains(t, out, "X-Session-Id", "汇总应出现头名")
	require.Contains(t, out, "X-Conversation-Request-Id", "汇总应出现头名")
	require.NotContains(t, out, "SECRET-SESSION-VALUE", "红线：汇总绝不能出现头值")
	require.NotContains(t, out, "SECRET-TURN-VALUE", "红线：汇总绝不能出现头值")
}

// --- 6. 周期 <= 0 时关闭输出，且关闭态零计数、零采样（P2：真正做到零开销）---

func TestAffinityStats_DisabledWhenIntervalNonPositive(t *testing.T) {
	captured := withInfofCapture(t)
	withStatsInterval(t, 0)

	s := newAffinityStats()
	// 关闭态：埋点在最入口直接返回，既不计数也不采样。
	s.recordHit(AffinityLevelTurn, false)
	s.recordFallback()
	h := http.Header{}
	for _, name := range affinityCandidateHeaders {
		h.Set(name, "SECRET-VALUE")
	}
	s.recordMiss(h)

	require.Equal(t, "", captured.joined(), "周期 <=0 时不得输出任何汇总")
	require.Equal(t, int64(0), s.lastFlushNano.Load(), "关闭状态下不得初始化窗口")

	// 零计数：四个计数 + 回落计数全为 0
	require.Equal(t, int64(0), s.turnHits.Load(), "关闭态 turn 不得计数")
	require.Equal(t, int64(0), s.sessionHits.Load(), "关闭态 session 不得计数")
	require.Equal(t, int64(0), s.userHits.Load(), "关闭态 user 不得计数")
	require.Equal(t, int64(0), s.fallbacks.Load(), "关闭态 fallback 不得计数")
	require.Equal(t, int64(0), s.misses.Load(), "关闭态 miss 不得计数")

	// 零采样：即便请求带满了 15 个候选头，headerCounts 也必须全为 0
	for i, name := range affinityCandidateHeaders {
		require.Equal(t, int64(0), s.headerCounts[i].Load(), "关闭态不得采样候选头 %s", name)
	}
}

// --- 7. 惰性输出：仅在周期届满后的埋点触发 ---

func TestAffinityStats_LazyFlushOnlyAfterInterval(t *testing.T) {
	captured := withInfofCapture(t)
	withStatsInterval(t, 10)
	fake := time.Unix(4000, 0)
	withStatsClock(t, func() time.Time { return fake })

	s := newAffinityStats()
	s.recordHit(AffinityLevelTurn, false) // 初始化窗口
	fake = fake.Add(5 * time.Second)
	s.recordHit(AffinityLevelTurn, false) // 未到 10s：不输出
	require.Equal(t, "", captured.joined(), "周期未满不得输出")

	fake = fake.Add(6 * time.Second) // 累计 11s >= 10s
	s.recordHit(AffinityLevelTurn, false)
	require.Contains(t, captured.joined(), "affinity stats", "周期届满后的首个埋点应输出")
	require.Contains(t, captured.joined(), "turn=3", "汇总应含窗口内全部 3 次 turn 命中")
}

// --- 8. 端到端：nonAutoDistribute 埋点接线（命中 / 未命中 / 键命中但回落）---

func TestNonAutoDistribute_RecordsAffinityStats(t *testing.T) {
	withStatsInterval(t, 3600) // 开启统计、不触发输出，避免污染
	resetGlobalAffinityStats()

	modelName := "gpt4turbostats"
	// 命中：turn 层
	hitScope := AffinityScope{UserID: 88888, Group: "g", TurnID: "t1", SessionID: "s1"}
	hitKeys := hitScope.Keys(modelName)
	require.Len(t, hitKeys, 2, "前置条件：有 session 时键序列为 turn + session")
	AffinityGlobal.Set(hitKeys[0], 1) // turn 层指向渠道 1
	defer AffinityGlobal.Remove(hitKeys[0])

	channels := []*model.Channel{{Name: "A", Id: 1, Models: modelName, ModelsAlias: modelName}}
	_, _, err := nonAutoDistribute(context.Background(), hitScope, modelName, channels)
	require.NoError(t, err)

	require.Equal(t, int64(1), affinityStatsGlobal.turnHits.Load(), "turn 层命中应被计数")
	require.Equal(t, int64(0), affinityStatsGlobal.sessionHits.Load(), "session 计数不得被 turn 命中污染")
	require.Equal(t, int64(0), affinityStatsGlobal.userHits.Load(), "user 计数不得被 turn 命中污染")
	require.Equal(t, int64(0), affinityStatsGlobal.misses.Load(), "命中不得计入 miss")

	// 未命中：全新 scope，且请求带候选头
	resetGlobalAffinityStats()
	missScope := AffinityScope{
		UserID: 88889, Group: "g", TurnID: "t2", SessionID: "s2",
		reqHeaders: http.Header{
			"X-Session-Id": {"SECRET-SESSION-VALUE"},
			"X-Query-Id":   {"SECRET-QUERY-VALUE"},
		},
	}
	channels2 := []*model.Channel{{Name: "B", Id: 2, Models: modelName, ModelsAlias: modelName}}
	_, _, err = nonAutoDistribute(context.Background(), missScope, modelName, channels2)
	require.NoError(t, err)

	require.Equal(t, int64(1), affinityStatsGlobal.misses.Load(), "未命中应被计数")
	require.Equal(t, int64(0), affinityStatsGlobal.turnHits.Load(), "未命中不得计入命中")
	require.Equal(t, int64(1), affinityStatsGlobal.headerCounts[candidateIndex(t, "X-Session-Id")].Load(),
		"未命中时应采样到 X-Session-Id（只记头名）")
	require.Equal(t, int64(1), affinityStatsGlobal.headerCounts[candidateIndex(t, "X-Query-Id")].Load(),
		"未命中时应采样到 X-Query-Id（只记头名）")

	resetGlobalAffinityStats()
}

// --- 9. 三档互斥：命中 / 回落 / 未命中各计各的（advisory-1）---

func TestAffinityStats_ThreeTiersAreIndependent(t *testing.T) {
	withStatsInterval(t, 3600)
	s := newAffinityStats()

	s.recordHit(AffinityLevelTurn, false)
	s.recordFallback()
	s.recordFallback()
	s.recordMiss(nil)

	require.Equal(t, int64(1), s.turnHits.Load(), "命中独立计数")
	require.Equal(t, int64(2), s.fallbacks.Load(), "回落独立计数，不得混入命中")
	require.Equal(t, int64(1), s.misses.Load(), "未命中独立计数")
	require.Equal(t, int64(0), s.sessionHits.Load(), "未触发的层级计数应为 0")
	require.Equal(t, int64(0), s.userHits.Load(), "未触发的层级计数应为 0")
}

// --- 10. 端到端：亲和键命中但渠道不在候选集 → 计入 fallback、不计命中（advisory-1）---

func TestNonAutoDistribute_RecordsFallbackWhenAffinityChannelNotInCandidates(t *testing.T) {
	withStatsInterval(t, 3600)
	resetGlobalAffinityStats()
	defer resetGlobalAffinityStats()

	modelName := "gpt4turbofallback"
	scope := AffinityScope{UserID: 77777, Group: "g", TurnID: "t1", SessionID: "s1"}
	keys := scope.Keys(modelName)
	require.Len(t, keys, 2, "前置条件：有 session 时键序列为 turn + session")
	// 亲和键指向渠道 999，但候选集里只有渠道 3 —— 键命中却不生效，必须回落加权随机
	AffinityGlobal.Set(keys[0], 999)
	defer AffinityGlobal.Remove(keys[0])

	channels := []*model.Channel{{Name: "C", Id: 3, Models: modelName, ModelsAlias: modelName}}
	ch, _, err := nonAutoDistribute(context.Background(), scope, modelName, channels)
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.Equal(t, 3, ch.Id, "回落加权随机应选中候选集内唯一渠道")

	require.Equal(t, int64(1), affinityStatsGlobal.fallbacks.Load(), "键命中但渠道不在候选集应计入 fallback")
	require.Equal(t, int64(0), affinityStatsGlobal.turnHits.Load(), "回落不得计入命中（否则高估生效率）")
	require.Equal(t, int64(0), affinityStatsGlobal.sessionHits.Load(), "回落不得计入 session 命中")
	require.Equal(t, int64(0), affinityStatsGlobal.userHits.Load(), "回落不得计入 user 命中")
	require.Equal(t, int64(0), affinityStatsGlobal.misses.Load(), "键命中不得计入 miss")
}

// --- 11. 汇总输出实际窗口时长，而非配置周期（advisory-3）---

func TestAffinityStats_FlushLogsActualWindowNotConfiguredInterval(t *testing.T) {
	captured := withInfofCapture(t)
	withStatsInterval(t, 5)
	fake := time.Unix(5000, 0)
	withStatsClock(t, func() time.Time { return fake })

	s := newAffinityStats()
	s.recordHit(AffinityLevelTurn, false) // 初始化窗口起点

	// 空闲 42s 后才有下一个埋点触发 flush：实际窗口 42s，远大于配置的 5s
	fake = fake.Add(42 * time.Second)
	s.recordHit(AffinityLevelTurn, false)

	out := captured.joined()
	require.Contains(t, out, "window 42s", "应输出实际窗口时长 42s")
	require.NotContains(t, out, "window 5s", "不得用配置值 5s 冒充实际窗口")
}

// --- 12. 汇总日志逐档写明含义（advisory-1）---

func TestAffinityStats_FlushLogExplainsEachTier(t *testing.T) {
	captured := withInfofCapture(t)
	withStatsInterval(t, 1)
	fake := time.Unix(6000, 0)
	withStatsClock(t, func() time.Time { return fake })

	s := newAffinityStats()
	s.recordHit(AffinityLevelTurn, false)
	s.recordFallback()
	s.recordMiss(nil)
	fake = fake.Add(2 * time.Second)
	s.recordHit(AffinityLevelUser, false) // 到点触发输出

	out := captured.joined()
	require.Contains(t, out, "hit{", "汇总应含命中档")
	require.Contains(t, out, "fallback=1", "汇总应含回落档及计数")
	require.Contains(t, out, "miss=1", "汇总应含未命中档及计数")
	require.Contains(t, out, "选路尝试次数", "汇总应写明口径为选路尝试次数")
	require.Contains(t, out, "仅头名不记值", "汇总应写明采样只记头名")
	require.Contains(t, out, "filterLastFailedChannel", "fallback 口径说明不得丢失")
	require.Contains(t, out, "仅非 auto", "统计范围说明不得丢失")
}

// --- 13. 派生 turn 独立档位：真实 turn 与派生 turn 各自计数（Task 5.1/5.2 契约）---

func TestAffinityStats_DerivedTurnBucket(t *testing.T) {
	// G: 依次记录真实 turn 与派生 turn 命中 | W: recordHit | T: turnHits 与 turnDerivedHits
	// 各自递增且 session/user/fallback/miss 不变。
	withStatsInterval(t, 3600)
	s := newAffinityStats()

	s.recordHit(AffinityLevelTurn, false) // 真实 turn 头命中
	s.recordHit(AffinityLevelTurn, true)  // body 派生 turn 命中
	s.recordHit(AffinityLevelTurn, true)

	require.Equal(t, int64(1), s.turnHits.Load(), "真实 turn 头命中应只计 1 次，不得被派生命中污染")
	require.Equal(t, int64(2), s.turnDerivedHits.Load(), "派生 turn 命中应独立计 2 次")
	require.Equal(t, int64(0), s.sessionHits.Load(), "turn 层命中不得污染 session 档")
	require.Equal(t, int64(0), s.userHits.Load(), "turn 层命中不得污染 user 档")
	require.Equal(t, int64(0), s.fallbacks.Load(), "命中不得计入 fallback")
	require.Equal(t, int64(0), s.misses.Load(), "命中不得计入 miss")

	// 非 turn 层忽略 derived：session/user 层不存在派生语义，绝不能进入派生档位。
	s.recordHit(AffinityLevelSession, true)
	s.recordHit(AffinityLevelUser, true)
	require.Equal(t, int64(1), s.sessionHits.Load(), "session 层命中应进 session 档，忽略 derived")
	require.Equal(t, int64(1), s.userHits.Load(), "user 层命中应进 user 档，忽略 derived")
	require.Equal(t, int64(2), s.turnDerivedHits.Load(), "非 turn 层不得进入派生档位（六档互斥）")
}

func TestAffinityStats_FlushReportsDerivedTurnSeparately(t *testing.T) {
	// G: 六档均有已知计数且 miss headers 含秘密值 | W: flush 固定窗口 |
	// T: 日志含 turn_derived 独立字段、六档值正确、计数清零、仅含头名不含任何头值。
	captured := withInfofCapture(t)
	withStatsInterval(t, 3600)
	s := newAffinityStats()

	s.recordHit(AffinityLevelTurn, false) // turn=1
	s.recordHit(AffinityLevelTurn, true)  // turn_derived=2
	s.recordHit(AffinityLevelTurn, true)
	s.recordHit(AffinityLevelSession, false) // session=1
	s.recordHit(AffinityLevelUser, false)    // user=1
	s.recordFallback()                       // fallback=1
	h := http.Header{}
	h.Set("X-Conversation-Request-Id", "SECRET-TURN-VALUE")
	h.Set("X-Session-Id", "SECRET-SESSION-VALUE")
	s.recordMiss(h) // miss=1，采样头名

	s.flush(7)

	out := captured.joined()
	require.Contains(t, out, "window 7s", "应输出传入的窗口时长")
	// 六档值逐一核对。
	require.Contains(t, out, "turn=1", "turn 档计数应为 1")
	require.Contains(t, out, "turn_derived=2", "派生 turn 档计数应为 2")
	require.Contains(t, out, "session=1", "session 档计数应为 1")
	require.Contains(t, out, "user=1", "user 档计数应为 1")
	require.Contains(t, out, "fallback=1", "fallback 档计数应为 1")
	require.Contains(t, out, "miss=1", "miss 档计数应为 1")
	// turn_derived 必须紧跟 turn：两字段之间不得插入其他档位。
	require.Contains(t, out, "turn=1, turn_derived=2", "turn_derived 必须固定紧跟 turn")
	// 计数清零。
	require.Equal(t, int64(0), s.turnHits.Load(), "flush 后 turn 计数应清零")
	require.Equal(t, int64(0), s.turnDerivedHits.Load(), "flush 后派生 turn 计数应清零")
	require.Equal(t, int64(0), s.sessionHits.Load(), "flush 后 session 计数应清零")
	require.Equal(t, int64(0), s.userHits.Load(), "flush 后 user 计数应清零")
	require.Equal(t, int64(0), s.fallbacks.Load(), "flush 后 fallback 计数应清零")
	require.Equal(t, int64(0), s.misses.Load(), "flush 后 miss 计数应清零")
	// 红线：只记头名，绝不记头值。
	require.Contains(t, out, "X-Conversation-Request-Id", "汇总应出现头名")
	require.Contains(t, out, "X-Session-Id", "汇总应出现头名")
	require.NotContains(t, out, "SECRET-TURN-VALUE", "红线：汇总绝不能出现头值")
	require.NotContains(t, out, "SECRET-SESSION-VALUE", "红线：汇总绝不能出现头值")
}

// --- 14. 端到端：派生 turn 命中只进派生档（Task 5.3 契约）---

func TestNonAutoDistribute_RecordsDerivedTurnHit(t *testing.T) {
	// G: 派生 turn key 映射到候选渠道且 scope.turnDerived 为 true | W: nonAutoDistribute |
	// T: 选中该渠道并只递增 turnDerivedHits；真实 turn、fallback 与 miss 均不递增。
	withStatsInterval(t, 3600)
	resetGlobalAffinityStats()
	defer resetGlobalAffinityStats()

	modelName := "gpt4turboderivedstats"
	scope := AffinityScope{
		UserID: 66666, Group: "g", TurnID: "derived-turn", SessionID: "s1",
		turnDerived: true,
	}
	keys := scope.Keys(modelName)
	require.Len(t, keys, 2, "前置条件：有 session 时键序列为 turn + session")
	require.Equal(t, AffinityLevelTurn, keys[0].Level, "前置条件：首个键为 turn 层")
	AffinityGlobal.Set(keys[0], 1) // turn 层指向候选渠道 1
	defer AffinityGlobal.Remove(keys[0])

	channels := []*model.Channel{{Name: "A", Id: 1, Models: modelName, ModelsAlias: modelName}}
	ch, _, err := nonAutoDistribute(context.Background(), scope, modelName, channels)
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.Equal(t, 1, ch.Id, "应选中亲和键映射的候选渠道")

	require.Equal(t, int64(1), affinityStatsGlobal.turnDerivedHits.Load(), "派生 turn 命中应进派生档")
	require.Equal(t, int64(0), affinityStatsGlobal.turnHits.Load(), "派生命中不得混入真实 turn 档")
	require.Equal(t, int64(0), affinityStatsGlobal.sessionHits.Load(), "派生命中不得混入 session 档")
	require.Equal(t, int64(0), affinityStatsGlobal.userHits.Load(), "派生命中不得混入 user 档")
	require.Equal(t, int64(0), affinityStatsGlobal.fallbacks.Load(), "真正选中不得计入 fallback")
	require.Equal(t, int64(0), affinityStatsGlobal.misses.Load(), "命中不得计入 miss")
}
