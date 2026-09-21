package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 固定向量：sessionID="sess-fixed-001"、最后真实 user 消息下标=1、文本指纹="hello"，
// 期望 sha256(sessionID+"\x00"+"1"+"\x00"+"hello") 前 32 位十六进制。
const (
	fixedSession   = "sess-fixed-001"
	fixedBody      = `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`
	fixedTurnID    = "35cbe8553bdcf9c60bb0ac23bbdcc1b1"
	bodyBase       = `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"}]}`
	bodyAppendAsst = `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"},{"role":"assistant","content":"a"}]}`
	bodyAppendTool = `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"},{"role":"assistant","content":"a"},{"role":"tool","content":"t"}]}`
	bodyNewUser    = `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"},{"role":"assistant","content":"a"},{"role":"user","content":"next"}]}`
	bodyPureTool   = `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"}]}]}`
)

// isLowerHex32 判定字符串是否为 32 位小写十六进制。
func isLowerHex32(s string) bool {
	if len(s) != 32 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// TestDeriveTurnIDFromBody_DeterministicVector
// G: 固定 session 与含真实 user 消息的固定 JSON | W: 重复调用 DeriveTurnIDFromBody |
// T: 两次结果相同、长度 32、为小写十六进制且符合 OpenSpec 公式
func TestDeriveTurnIDFromBody_DeterministicVector(t *testing.T) {
	first := DeriveTurnIDFromBody([]byte(fixedBody), fixedSession)
	second := DeriveTurnIDFromBody([]byte(fixedBody), fixedSession)

	assert.Equal(t, first, second, "同输入重复调用必须得到相同派生值")
	assert.Len(t, first, 32, "派生值长度必须为 32")
	assert.True(t, isLowerHex32(first), "派生值必须为小写十六进制")
	assert.Equal(t, fixedTurnID, first, "派生值必须与 OpenSpec 公式逐字节一致")
}

// TestDeriveTurnIDFromBody
// G: 表驱动覆盖 tool/assistant 追加、新 user、纯 tool_result、缺失/空/错误类型 messages、非法 JSON、无 text block、空 session |
// W: 调用纯函数 | T: 稳定、轮换、跳过或空串降级符合各 case 且不 panic
func TestDeriveTurnIDFromBody(t *testing.T) {
	base := DeriveTurnIDFromBody([]byte(bodyBase), fixedSession)
	require.Len(t, base, 32, "基线用例必须能派生出有效值")

	t.Run("tool 或 assistant 追加时派生值恒定", func(t *testing.T) {
		assert.Equal(t, base, DeriveTurnIDFromBody([]byte(bodyAppendAsst), fixedSession), "追加 assistant 消息不得改变派生值")
		assert.Equal(t, base, DeriveTurnIDFromBody([]byte(bodyAppendTool), fixedSession), "追加 tool 消息不得改变派生值")
	})

	t.Run("追加新 user 消息时换值", func(t *testing.T) {
		rotated := DeriveTurnIDFromBody([]byte(bodyNewUser), fixedSession)
		assert.Len(t, rotated, 32)
		assert.NotEqual(t, base, rotated, "新 user 消息必须使派生值轮换")
	})

	t.Run("纯 tool_result user 消息被跳过并回退到上一条真实 user", func(t *testing.T) {
		assert.Equal(t, base, DeriveTurnIDFromBody([]byte(bodyPureTool), fixedSession), "纯 tool_result 的 user 消息不得改变所选真实 user 消息")
	})

	t.Run("数组 content 按序拼接 text block", func(t *testing.T) {
		stringForm := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), fixedSession)
		arrayForm := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"he"},{"type":"text","text":"llo"}]}]}`), fixedSession)
		assert.Equal(t, stringForm, arrayForm, "数组 text block 拼接结果必须与字符串 content 一致")
	})

	t.Run("messages 缺失或空或非数组时返回空串", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"messages 缺失", `{"model":"m"}`},
			{"messages 为空数组", `{"messages":[]}`},
			{"messages 非数组", `{"messages":"oops"}`},
			{"messages 为对象", `{"messages":{"role":"user"}}`},
			{"无真实 user 消息", `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"a"}]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, "", DeriveTurnIDFromBody([]byte(tc.body), fixedSession), "无效 messages 必须降级为空串")
			})
		}
	})

	t.Run("非法 JSON 返回空串且不 panic", func(t *testing.T) {
		require.NotPanics(t, func() {
			assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[`), fixedSession))
		})
	})

	t.Run("真实 user 消息但无法提取文本时降级为空串", func(t *testing.T) {
		require.NotPanics(t, func() {
			assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`), fixedSession), "纯 image block 数组无法提取文本，必须降级为空串")
		})
		require.NotPanics(t, func() {
			assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"text"}]}]}`), fixedSession), "text block 缺失 text 字段无法提取文本，必须降级为空串")
		})
		require.NotPanics(t, func() {
			assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":123}]}]}`), fixedSession), "text 字段非字符串无法提取文本，必须降级为空串")
		})
		require.NotPanics(t, func() {
			assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"r"}]}]}`), fixedSession), "纯 tool_result 数组不得被当作真实 user 消息")
		})
	})

	t.Run("最后一条真实 user 无法提取文本时立即失败且不回溯", func(t *testing.T) {
		single := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":"real"}]}`), fixedSession)
		require.True(t, isLowerHex32(single), "仅含第一条真实 user 时必须能正常派生，作为对照基线")
		got := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":"real"},{"role":"user","content":[{"type":"image","source":{}}]}]}`), fixedSession)
		assert.Equal(t, "", got, "最后一条真实 user 无 text block 时必须立即失败返回空串")
		assert.NotEqual(t, single, got, "不得回溯到更早的有文本真实 user 消息")
	})

	t.Run("末条 user 消息形态畸形时被跳过并回溯到上一条真实 user", func(t *testing.T) {
		// 期望值用 OpenSpec 公式独立计算（sha256(sessionID+"\x00"+idx+"\x00"+text)[:32]，idx=0、text="real"），
		// 不调用被测实现，避免把实现逻辑抄进期望值。
		sum := sha256.Sum256([]byte(fixedSession + "\x00" + "0" + "\x00" + "real"))
		want := hex.EncodeToString(sum[:])[:32]

		// 前置一条真实 user（下标 0）+ 末条畸形 user：content 为 null / [] / 数字 / 对象。
		// 四种形态均被判为非真实 user 消息 → 跳过 → 回溯到下标 0 的真实 user。
		cases := []struct {
			name string
			body string
		}{
			{"content 为 null", `{"messages":[{"role":"user","content":"real"},{"role":"user","content":null}]}`},
			{"content 为空数组", `{"messages":[{"role":"user","content":"real"},{"role":"user","content":[]}]}`},
			{"content 为数字", `{"messages":[{"role":"user","content":"real"},{"role":"user","content":123}]}`},
			{"content 为对象", `{"messages":[{"role":"user","content":"real"},{"role":"user","content":{"k":"v"}}]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, want, DeriveTurnIDFromBody([]byte(tc.body), fixedSession),
					"末条 user 消息畸形时须跳过并回溯到上一条真实 user 的派生值")
			})
		}
	})

	t.Run("数组 content 混合 text 与 tool_result 时正常派生", func(t *testing.T) {
		stringForm := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), fixedSession)
		mixedForm := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_result","tool_use_id":"x","content":"r"}]}]}`), fixedSession)
		assert.True(t, isLowerHex32(mixedForm), "含 text block 的混合数组必须正常派生")
		assert.Equal(t, stringForm, mixedForm, "混合数组的文本指纹必须等于纯 text block 拼接结果")
	})

	t.Run("content 为空数组或 null 时返回空串", func(t *testing.T) {
		assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[]}]}`), fixedSession), "空数组无法提取文本，必须降级为空串")
		assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":null}]}`), fixedSession), "null content 不是真实 user 消息，必须降级为空串")
	})

	t.Run("role 大小写不匹配时返回空串", func(t *testing.T) {
		assert.Equal(t, "", DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"User","content":"hello"}]}`), fixedSession), "role 判定必须严格等于小写 user")
	})

	t.Run("content 为 string 空串时视为成功提取空文本并正常派生", func(t *testing.T) {
		got := DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":""}]}`), fixedSession)
		assert.True(t, isLowerHex32(got), "字符串 content 的原值即为文本，空串属成功提取，必须正常派生")
		assert.Equal(t, DeriveTurnIDFromBody([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`), fixedSession), got, "字符串空串与空 text block 的文本指纹必须一致")
	})

	t.Run("sessionID 为空串时返回空串", func(t *testing.T) {
		assert.Equal(t, "", DeriveTurnIDFromBody([]byte(bodyBase), ""), "空 session 锚点必须降级为空串")
	})

	t.Run("payload 入口与 body 入口语义一致", func(t *testing.T) {
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(bodyAppendTool), &payload))
		assert.Equal(t, DeriveTurnIDFromBody([]byte(bodyAppendTool), fixedSession), DeriveTurnIDFromPayload(payload, fixedSession), "两个入口对同一输入必须产出相同结果")
		assert.Equal(t, "", DeriveTurnIDFromPayload(payload, ""), "payload 入口同样要求非空 session")
	})
}
