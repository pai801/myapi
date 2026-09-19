package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/chanregistry"
)

// unregisteredCT54/55/56 是三个「未注册 adaptor」的渠道类型：本仓不注册任何渠道，
// 故它们与任意未知渠道类型等价，用于验证未注册渠道的安全降级。
// 本文件用数值字面量而非常量标识符，避免在源码里出现渠道实现标识。
const (
	unregisteredCT54 = 54
	unregisteredCT55 = 55
	unregisteredCT56 = 56
)

// fakeCompressor 是 controller 测试用的假压缩器。
type fakeCompressor struct {
	supports map[int]bool
	out      string
	err      error
}

func (f fakeCompressor) Supports(channelType int) bool { return f.supports[channelType] }

func (f fakeCompressor) CompactKey(raw string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

// swapCompressorFor 临时替换查压缩能力的接缝，测试结束自动还原。
func swapCompressorFor(t *testing.T, fn func(int) chanregistry.CredentialCompressor) {
	t.Helper()
	orig := credentialCompressorFor
	credentialCompressorFor = fn
	t.Cleanup(func() { credentialCompressorFor = orig })
}

// prettyNestedCredential 复现生产事故里插件 OAuth 输出的多行 pretty 嵌套凭证：
// 带缩进换行，被 AddChannel 按 '\n' 拆分会逐行落成十几条渠道记录。
const prettyNestedCredential = `{
  "auth": {
    "accessToken": "eyJhbGciOiJIUzI1NiJ9.payload.sig",
    "refreshToken": "rt-abc123",
    "expiresAt": 1790000000,
    "domain": "api.example.com",
    "realm": "global"
  },
  "account": {
    "uid": "u_8f3c12345678",
    "enterpriseId": "",
    "nickname": "rafe"
  },
  "device_token": ""
}`

// TestKeysForChannelCollapsesMultilineWithCompressor 锁定事故路径（有压缩器语义）：
// 当渠道类型有压缩器时，多行凭证经 keysForChannel 后只产出 **1 条**紧凑 key，
// 不再被逐行拆成十几条记录。
//
// 默认无压缩器（见 TestKeysForChannelNoCompressor），故此处的压缩器
// 经 credentialCompressorFor 接缝注入——用 json.Compact 复现真实压缩器「整段 JSON
// 归一化为紧凑单行」的行为，从而在不 import 具体渠道实现的前提下守住 keysForChannel 的契约。
func TestKeysForChannelCollapsesMultilineWithCompressor(t *testing.T) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(prettyNestedCredential)); err != nil {
		t.Fatalf("test setup: json.Compact: %v", err)
	}
	compacted := buf.String()
	swapCompressorFor(t, func(int) chanregistry.CredentialCompressor {
		return fakeCompressor{supports: map[int]bool{unregisteredCT54: true, unregisteredCT55: true}, out: compacted}
	})

	for _, ct := range []int{unregisteredCT54, unregisteredCT55} {
		got := keysForChannel(model.Channel{Type: ct, Key: prettyNestedCredential})
		if len(got) != 1 {
			t.Fatalf("type %d: must yield exactly 1 key, got %d: %q", ct, len(got), got)
		}
		if strings.Contains(got[0], "\n") {
			t.Fatalf("type %d: key must be compact single line, got %q", ct, got[0])
		}
		var doc struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
			} `json:"auth"`
		}
		if err := json.Unmarshal([]byte(got[0]), &doc); err != nil {
			t.Fatalf("type %d: key must stay valid JSON: %v (%q)", ct, err, got[0])
		}
		if doc.Auth.AccessToken != "eyJhbGciOiJIUzI1NiJ9.payload.sig" {
			t.Errorf("type %d: accessToken changed after compaction: %q", ct, doc.Auth.AccessToken)
		}
	}
}

// TestKeysForChannelNoCompressor 模拟无压缩器注册（主仓不注册任何压缩器）：
// 54/55/56 退化为普通渠道，按 '\n' 拆分批量录入，且空 key 不 panic。
func TestKeysForChannelNoCompressor(t *testing.T) {
	swapCompressorFor(t, func(int) chanregistry.CredentialCompressor { return nil })
	for _, ct := range []int{unregisteredCT54, unregisteredCT55, unregisteredCT56} {
		got := keysForChannel(model.Channel{Type: ct, Key: "a\nb"})
		if !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Errorf("keysForChannel(type %d) = %q, want [a b] (no compressor -> split)", ct, got)
		}
	}
	if got := keysForChannel(model.Channel{Type: unregisteredCT55, Key: ""}); len(got) != 0 {
		t.Errorf("keysForChannel(empty, no compressor) = %q, want empty", got)
	}
}

// TestKeysForChannel 表驱动守护非压缩渠道的语义：
// 保持「一个 key 一行」批量录入（空行跳过，顺序不变），空 key 返回空切片。
func TestKeysForChannel(t *testing.T) {
	cases := []struct {
		name    string
		channel model.Channel
		want    []string
	}{
		{
			name:    "openai-batch-lines",
			channel: model.Channel{Type: channeltype.OpenAI, Key: "sk-a\nsk-b\n\nsk-c"},
			want:    []string{"sk-a", "sk-b", "sk-c"},
		},
		{
			name:    "openai-empty-key",
			channel: model.Channel{Type: channeltype.OpenAI, Key: ""},
			want:    []string{},
		},
		{
			name:    "openai-single-key",
			channel: model.Channel{Type: channeltype.OpenAI, Key: `{"accessToken":"at"}`},
			want:    []string{`{"accessToken":"at"}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapCompressorFor(t, func(int) chanregistry.CredentialCompressor { return nil })
			got := keysForChannel(tc.channel)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("keysForChannel(%+v) = %q, want %q", tc.channel, got, tc.want)
			}
		})
	}
}

// TestCompactChannelKey 覆盖接缝语义：无压缩器 → ok=false（走原路径）；
// 有压缩器 → 应用压缩；压缩报错 → 保留原值单条、绝不回退拆分。
func TestCompactChannelKey(t *testing.T) {
	swapCompressorFor(t, func(int) chanregistry.CredentialCompressor { return nil })
	if _, ok := compactChannelKey(unregisteredCT55, "x"); ok {
		t.Error("compactChannelKey with no compressor ok = true, want false")
	}

	swapCompressorFor(t, func(int) chanregistry.CredentialCompressor {
		return fakeCompressor{supports: map[int]bool{unregisteredCT55: true}, out: "compact"}
	})
	if got, ok := compactChannelKey(unregisteredCT55, "raw"); !ok || got != "compact" {
		t.Errorf("compactChannelKey = (%q,%v), want (compact,true)", got, ok)
	}

	swapCompressorFor(t, func(int) chanregistry.CredentialCompressor {
		return fakeCompressor{supports: map[int]bool{unregisteredCT55: true}, err: errors.New("boom")}
	})
	if got, ok := compactChannelKey(unregisteredCT55, "raw"); !ok || got != "raw" {
		t.Errorf("compactChannelKey on error = (%q,%v), want (raw,true) (never split)", got, ok)
	}
}
