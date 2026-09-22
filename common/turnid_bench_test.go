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
// T: 记录新旧实现（jsonparser 扫描 vs encoding/json 全量解析）的开销对比
//
// 子基准口径（逐一对应实际 b.Run 名称）：
//   - derive/steps=*：仅调用 DeriveTurnIDFromBody（jsonparser 扫描），**不含** json.Valid 预检；
//     覆盖 1 / 4 / 12 / 40 步，展示派生开销随 payload 体积的增长；
//   - baseline_parse_only：旧实现现状（encoding/json 全量解析到 map[string]any）的解析开销，作为对照；
//   - json_valid_only：json.Valid 预检（middleware 安全红线）的独立开销，展示预检占比；
//   - derive_from_body：预检 + jsonparser 派生全路径，与 middleware 生产路径等价。
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

	// 旧实现对照：encoding/json 全量解析到 map[string]any（旧 DeriveTurnIDFromBody 的第一步）。
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

	// 新实现口径：json.Valid 预检（middleware 安全红线）的独立开销。
	b.Run("json_valid_only", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(baseRaw)))
		for i := 0; i < b.N; i++ {
			if !json.Valid(baseRaw) {
				b.Fatal("代表性 payload 必须是合法 JSON")
			}
		}
	})

	// 新实现口径：预检 + jsonparser 派生全路径（与 middleware 生产路径等价）。
	b.Run("derive_from_body", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(baseRaw)))
		for i := 0; i < b.N; i++ {
			if !json.Valid(baseRaw) {
				b.Fatal("代表性 payload 必须是合法 JSON")
			}
			if got := DeriveTurnIDFromBody(baseRaw, sessionID); got == "" {
				b.Fatal("代表性 payload 必须可派生")
			}
		}
	})
}
