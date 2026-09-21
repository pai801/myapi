package middleware

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// affinityToolLoopBody 构造 opencode / Anthropic 形态的 tool-loop 请求体：
// 首条为真实 user 文本消息，其后逐步追加 assistant tool_use + 纯 tool_result user 消息。
// 纯 tool_result 是工具结果载体，不改变「最后真实 user 消息」，故派生值应在轮内恒定；
// toolSteps 增大时 payload 体积线性增长，用来观测「轮内 key 数量不随 tool-loop 次数增长」。
// followUp 非空时在末尾追加一条新的真实 user 消息，用于模拟进入下一轮（派生值应轮换）。
func affinityToolLoopBody(sessionID, userText string, toolSteps int, followUp string) string {
	var b strings.Builder
	b.WriteString(`{"model":"`)
	b.WriteString(affinityE2EModel)
	b.WriteString(`","session_id":"`)
	b.WriteString(sessionID)
	b.WriteString(`","messages":[{"role":"user","content":"`)
	b.WriteString(userText)
	b.WriteString(`"}`)
	for i := 0; i < toolSteps; i++ {
		id := fmt.Sprintf("toolu_%d", i)
		b.WriteString(fmt.Sprintf(`,{"role":"assistant","content":[{"type":"tool_use","id":"%s","name":"bash","input":{"command":"ls"}}]}`, id))
		b.WriteString(fmt.Sprintf(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"%s","content":"file_%d.txt"}]}`, id, i))
	}
	if followUp != "" {
		b.WriteString(`,{"role":"user","content":"`)
		b.WriteString(followUp)
		b.WriteString(`"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestAffinitySmoke_ToolLoopKeyReuse
// G: mock 上游、同 session 的 tool-loop 序列及下一轮 user 消息
// W: 重放请求并观察汇总
// T: 轮内 key 不增长、turn_derived 随命中增长、下一轮 key 轮换
//
// 「mock 上游」采用进程内路径：候选渠道由 affinityE2EChannels() 提供（与 affinity_e2e_test.go
// 同一装置），选路走真实 nonAutoDistribute，亲和写入按生产写法 recordAffinityE2E 复刻。
// 选择进程内而非改造 scripts/affinity_smoke.sh 的理由：本契约的 Test Contract 是 Go 测试函数名，
// 必须能被 `go test` 直接执行；脚本的可用性属 Additional Verification，单独执行确认，不改脚本。
//
// 关键断言非恒真：
//   - 「轮内 key 不增长」统计的是**派生 turn 键值集合的基数**（map 去重后 len），不是命中计数；
//     若派生值每次变化，基数随请求数增长，断言必然失败。
//   - 「下一轮 key 轮换」要求追加新 user 后的派生键不在轮内键集合中；若派生值恒定（忽略 body），
//     断言必然失败。
func TestAffinitySmoke_ToolLoopKeyReuse(t *testing.T) {
	originalDerive := config.AffinityDeriveTurnID
	defer func() { config.AffinityDeriveTurnID = originalDerive }()
	config.AffinityDeriveTurnID = "true"
	withStatsInterval(t, 3600)
	resetGlobalAffinityStats()
	defer resetGlobalAffinityStats()

	h := newAffinityE2E(t)
	const userID = 920007
	const session = "sess-tool-loop"
	headers := map[string]string{"X-Session-Id": session}
	channels := affinityE2EChannels()

	// 统计派生 turn 键的**去重基数**：轮内应恒为 1，跨轮应增至 2。
	distinctTurnKeys := map[string]int{}
	turnKeyOrder := make([]string, 0, 8)
	requests := 0
	payloadBytes := 0
	var turnIDs []string
	var turnDerivedDeltas []int64

	// replay 走真实链路：真实 JSON body + 真实 session 头 → ResolveAffinityScope → nonAutoDistribute
	// → 按生产写法写亲和。返回该请求**写入前**观测到的命中层级（写入后 turn 键必然存在，
	// 若在写入后取层级会恒真命中 turn 层，无法证明跨轮回落 session）。
	replay := func(body string) (AffinityScope, *model.Channel, AffinityLevel, bool) {
		h.t.Helper()
		payloadBytes += len(body)
		requests++

		scope := h.resolveBody(userID, headers, body)
		require.True(t, scope.turnDerived, "tool-loop 请求必须走派生路径")
		require.NotEmpty(t, scope.TurnID, "tool-loop 请求必须派生出非空 turn id")
		keys := scope.Keys(affinityE2EModel)
		require.Equal(t, AffinityLevelTurn, keys[0].Level, "派生成功时首个候选键必须是 turn 层")

		turnKey := keys[0].Value
		if _, seen := distinctTurnKeys[turnKey]; !seen {
			turnKeyOrder = append(turnKeyOrder, scope.TurnID)
		}
		distinctTurnKeys[turnKey]++
		turnIDs = append(turnIDs, scope.TurnID)

		// 写入前观测命中层级：这才是该请求真实生效的亲和层。
		_, preLevel, preHit := AffinityGlobal.Get(keys)

		before := readAffinityTierSnapshot()
		ch, _, err := nonAutoDistribute(context.Background(), scope, affinityE2EModel, channels)
		require.NoError(t, err, "nonAutoDistribute 选路失败")
		require.NotNil(t, ch)
		after := readAffinityTierSnapshot()
		turnDerivedDeltas = append(turnDerivedDeltas, after.turnDerived-before.turnDerived)

		// 按生产写法写亲和：后续请求应命中 turn 层（派生档）。
		recordAffinityE2E(scope, ch)
		return scope, ch, preLevel, preHit
	}

	// --- 第一轮：同 session，逐步追加 tool_result（tool-loop 增长）---
	round1Steps := []int{0, 1, 2, 3, 4}
	for i, steps := range round1Steps {
		body := affinityToolLoopBody(session, "list the files and summarize", steps, "")
		scope, _, preLevel, preHit := replay(body)
		assert.Equal(t, session, scope.SessionID, "session 头必须被解析进 scope")
		if i == 0 {
			assert.False(t, preHit, "首个请求尚无任何亲和键，写入前不得命中")
		} else {
			require.True(t, preHit, "第 %d 个请求应命中上一请求写入的亲和键", i)
			assert.Equal(t, AffinityLevelTurn, preLevel,
				"第 %d 个请求应命中 turn 层（派生键轮内复用）", i)
		}
	}

	// 轮内派生值恒定：所有请求的派生 turn id 两两相同。
	require.Len(t, turnIDs, len(round1Steps))
	for i := 1; i < len(round1Steps); i++ {
		assert.Equal(t, turnIDs[0], turnIDs[i],
			"轮内追加 tool_result 后派生 turn id 必须恒定（第 %d 个请求）", i)
	}
	// 轮内 key 不增长：去重基数必须恰为 1（真正统计 key 数量，而非命中计数）。
	assert.Len(t, distinctTurnKeys, 1,
		"轮内派生 turn 键的去重基数必须为 1（tool-loop 次数增长不得产生新 key）")
	// turn_derived 随命中增长：首个请求 miss，其后每个请求各命中一次派生档。
	require.Len(t, turnDerivedDeltas, len(round1Steps))
	assert.Equal(t, int64(0), turnDerivedDeltas[0], "首个请求尚无亲和键，不得计入 turn_derived 命中")
	for i := 1; i < len(turnDerivedDeltas); i++ {
		assert.Equal(t, int64(1), turnDerivedDeltas[i],
			"第 %d 个请求必须命中一次 turn_derived 档（派生键复用成功）", i)
	}
	t.Logf("第一轮: 请求=%d payloadBytes=%d 派生键去重基数=%d turn_derived 命中=%d",
		len(round1Steps), payloadBytes, len(distinctTurnKeys), turnDerivedDeltas[len(round1Steps)-1])

	// --- 第二轮：同一 session 追加新真实 user 消息 → 派生键必须轮换 ---
	round2Body := affinityToolLoopBody(session, "list the files and summarize", 4, "now summarize the findings")
	scope2, _, preLevel2, preHit2 := replay(round2Body)
	assert.Equal(t, session, scope2.SessionID, "第二轮仍为同一 session")
	assert.NotEqual(t, turnIDs[0], scope2.TurnID, "追加新 user 消息后派生 turn id 必须轮换")
	require.Len(t, distinctTurnKeys, 2, "跨轮后派生 turn 键去重基数必须增至 2（新旧各一）")
	require.Len(t, turnKeyOrder, 2, "轮内键应只有一个，跨轮新增一个")

	// 第二轮首个请求：turn 键换新 → turn miss → 回落 session 命中（不劣于变更前行为）。
	assert.Equal(t, int64(0), turnDerivedDeltas[len(turnDerivedDeltas)-1],
		"换轮后首个请求的 turn 键未映射，不得计入 turn_derived 命中")
	require.True(t, preHit2, "第二轮首个请求应回落命中 session 层")
	assert.Equal(t, AffinityLevelSession, preLevel2,
		"换轮后 turn 键未映射，应命中 session 层")

	t.Logf("第二轮: 请求=%d 累计payloadBytes=%d 派生键去重基数=%d",
		requests, payloadBytes, len(distinctTurnKeys))
}
