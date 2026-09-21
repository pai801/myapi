package common

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// opencodeToolLoopBody 构造代表性 opencode / Anthropic tool-loop payload：
// 首条为真实 user 文本消息，其后按 steps 逐步追加 assistant tool_use + 纯 tool_result user 消息。
// 纯 tool_result 是工具结果载体，不改变「最后真实 user 消息」，故派生值应轮内恒定，
// 而 payload 体积随 steps 线性增长 —— 正是 design.md 风险项要实测的覆盖面。
func opencodeToolLoopBody(sessionID string, steps int) string {
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-4","session_id":`)
	b.WriteString(strconv.Quote(sessionID))
	b.WriteString(`,"messages":[{"role":"user","content":[{"type":"text","text":"list the files and summarize"}]}`)
	for i := 0; i < steps; i++ {
		id := "toolu_" + strconv.Itoa(i)
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":`)
		b.WriteString(strconv.Quote(id))
		b.WriteString(`,"name":"bash","input":{"command":"ls -la /workspace/project"}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":`)
		b.WriteString(strconv.Quote(id))
		b.WriteString(`,"content":"total 42\ndrwxr-xr-x 12 dev dev 4096 main.go go.mod README.md internal pkg cmd"}]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// BenchmarkDeriveTurnIDFromBody_ToolLoop
// G: 固定 session 与逐步追加 tool_result 的代表性 opencode payload
// W: 执行派生路径基准
// T: 记录相对现状的解析开销且不改变功能代码
//
// 三个同口径子基准（同一份 12 步 payload，字节数一致）：
//   - derive/...：派生开启后的完整路径（JSON 解析 + 派生），即本次改造新增的覆盖面；
//   - baseline_parse_only：现状（派生关闭、仅 session body 补取）已有的 JSON 解析开销，无派生；
//   - increment_derive_from_payload：body 已解析时派生本身的增量（middleware 复用共享 payload 的真实增量）。
func BenchmarkDeriveTurnIDFromBody_ToolLoop(b *testing.B) {
	const sessionID = "sess-bench-tool-loop"

	for _, steps := range []int{1, 4, 12, 40} {
		raw := []byte(opencodeToolLoopBody(sessionID, steps))
		b.Run(fmt.Sprintf("derive/steps=%d/bytes=%d", steps, len(raw)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				if got := DeriveTurnIDFromBody(raw, sessionID); got == "" {
					b.Fatal("代表性 payload 必须可派生")
				}
			}
		})
	}

	const baselineSteps = 12
	baseRaw := []byte(opencodeToolLoopBody(sessionID, baselineSteps))

	b.Run(fmt.Sprintf("baseline_parse_only/bytes=%d", len(baseRaw)), func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(baseRaw)))
		for i := 0; i < b.N; i++ {
			var payload map[string]any
			if err := json.Unmarshal(baseRaw, &payload); err != nil {
				b.Fatal(err)
			}
		}
	})

	var parsed map[string]any
	if err := json.Unmarshal(baseRaw, &parsed); err != nil {
		b.Fatal(err)
	}
	b.Run("increment_derive_from_payload", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if got := DeriveTurnIDFromPayload(parsed, sessionID); got == "" {
				b.Fatal("代表性 payload 必须可派生")
			}
		}
	})
}
