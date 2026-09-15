package model

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessageDecodesRefusalField 锁定 Message.refusal 承载（报告二 14 / Chat §3.1）：
// assistant 历史消息的顶层 refusal 字符串必须可解码，不得静默丢失。
func TestMessageDecodesRefusalField(t *testing.T) {
	raw := `{"role":"assistant","content":null,"refusal":"I can't help with that"}`
	var msg Message
	require.NoError(t, json.Unmarshal([]byte(raw), &msg))
	require.NotNil(t, msg.Refusal)
	assert.Equal(t, "I can't help with that", *msg.Refusal)

	// 未提供 refusal 时保持 nil，序列化不得混入 refusal key（omitempty 语义）
	out, err := json.Marshal(msg)
	require.NoError(t, err)
	msg2 := Message{Role: "user", Content: "hi"}
	out2, err := json.Marshal(msg2)
	require.NoError(t, err)
	assert.NotContains(t, string(out2), "refusal")
	assert.Contains(t, string(out), "refusal")
}

// TestMessageParseContentDefensive 锁定 ParseContent 的防御性安全解析（报告二 8 的 parser 侧）：
// 合法 5 种 part 全保真（Chat §4 → MessageContent），任何畸形输入返回 error 且不 panic。
func TestMessageParseContentDefensive(t *testing.T) {
	t.Run("string_content", func(t *testing.T) {
		parts, err := Message{Content: "hello"}.ParseContent()
		require.NoError(t, err)
		require.Len(t, parts, 1)
		assert.Equal(t, ContentTypeText, parts[0].Type)
		assert.Equal(t, "hello", parts[0].Text)
	})

	t.Run("nil_content_no_error", func(t *testing.T) {
		parts, err := Message{}.ParseContent()
		require.NoError(t, err)
		assert.Nil(t, parts)
	})

	t.Run("valid_all_five_part_types", func(t *testing.T) {
		var msg Message
		require.NoError(t, json.Unmarshal([]byte(`{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"https://x/1.png","detail":"low"}},
			{"type":"input_audio","input_audio":{"data":"aGVsbG8=","format":"wav"}},
			{"type":"file","file":{"filename":"a.txt","file_data":"data:,"}},
			{"type":"refusal","refusal":"no"}
		]}`), &msg))
		parts, err := msg.ParseContent()
		require.NoError(t, err)
		require.Len(t, parts, 5)

		assert.Equal(t, ContentTypeText, parts[0].Type)
		assert.Equal(t, "look", parts[0].Text)

		assert.Equal(t, ContentTypeImageURL, parts[1].Type)
		require.NotNil(t, parts[1].ImageURL)
		assert.Equal(t, "https://x/1.png", parts[1].ImageURL.Url)
		assert.Equal(t, "low", parts[1].ImageURL.Detail)

		assert.Equal(t, ContentTypeInputAudio, parts[2].Type)
		assert.Equal(t, map[string]any{"data": "aGVsbG8=", "format": "wav"}, parts[2].InputAudio)

		assert.Equal(t, ContentTypeFile, parts[3].Type)
		assert.Equal(t, map[string]any{"filename": "a.txt", "file_data": "data:,"}, parts[3].File)

		assert.Equal(t, ContentTypeRefusal, parts[4].Type)
		assert.Equal(t, "no", parts[4].Refusal)
	})

	// 每条畸形样本都必须：不 panic + 返回 error（旧实现在 url 缺失处裸断言直接 panic）
	malformed := []struct {
		name    string
		content string
	}{
		{"part_not_object", `["plain string element"]`},
		{"type_missing", `[{"text":"x"}]`},
		{"type_not_string", `[{"type":42}]`},
		{"unknown_type", `[{"type":"video_url","video_url":{}}]`},
		{"text_not_string", `[{"type":"text","text":42}]`},
		{"text_missing", `[{"type":"text"}]`},
		{"image_url_not_object", `[{"type":"image_url","image_url":"https://x/1.png"}]`},
		{"image_url_missing_url", `[{"type":"image_url","image_url":{"detail":"low"}}]`},
		{"image_url_url_not_string", `[{"type":"image_url","image_url":{"url":42}}]`},
		{"input_audio_not_object", `[{"type":"input_audio","input_audio":"raw"}]`},
		{"file_not_object", `[{"type":"file","file":"a.txt"}]`},
		{"refusal_not_string", `[{"type":"refusal","refusal":42}]`},
	}
	for _, tt := range malformed {
		t.Run(tt.name, func(t *testing.T) {
			var msg Message
			require.NoError(t, json.Unmarshal([]byte(`{"role":"user","content":`+tt.content+`}`), &msg))
			var parts []MessageContent
			var err error
			require.NotPanics(t, func() { parts, err = msg.ParseContent() }, "defensive parse must not panic")
			require.Error(t, err)
			assert.Nil(t, parts, "失败路径不得返回部分结果")
		})
	}
}
