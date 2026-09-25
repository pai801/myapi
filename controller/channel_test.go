package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

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

// copyChannelResponse 是 CopyChannel 端点的响应形状：data 用 RawMessage 以便断言「有无 data」。
type copyChannelResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// TestCopyChannelReturnsKey 锁定复制端点的核心契约：对既有渠道返回 success=true，
// 且 data.key 与库中凭证一致（这是列表 / 详情刻意省略的字段）。
func TestCopyChannelReturnsKey(t *testing.T) {
	initUnregisteredChannelTestDB(t)

	id := insertUnregisteredChannel(t, unregisteredCT55, model.ChannelStatusEnabled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/copy/"+strconv.Itoa(id), nil)
	CopyChannel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("CopyChannel code = %d, want 200", w.Code)
	}
	var resp copyChannelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal copy response: %v (body=%s)", err, w.Body.String())
	}
	if !resp.Success {
		t.Fatalf("success = false, body=%s", w.Body.String())
	}
	var ch model.Channel
	if err := json.Unmarshal(resp.Data, &ch); err != nil {
		t.Fatalf("unmarshal data: %v (data=%s)", err, resp.Data)
	}
	if ch.Key != "legacy-key" {
		t.Errorf("data.key = %q, want %q (full channel must include the credential)", ch.Key, "legacy-key")
	}
}

// TestCopyChannelInvalidOrMissingIdFails 锁定失败路径：非法 id 与不存在的 id 都必须
// 返回 HTTP 200 + success=false，且不带 data，与 GetChannel / ResetChannel 约定一致。
func TestCopyChannelInvalidOrMissingIdFails(t *testing.T) {
	initUnregisteredChannelTestDB(t)

	for _, tc := range []struct {
		name string
		id   string
	}{
		{name: "non-numeric", id: "abc"},
		{name: "not-found", id: "999999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: tc.id}}
			c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/copy/"+tc.id, nil)
			CopyChannel(c)

			if w.Code != http.StatusOK {
				t.Fatalf("CopyChannel(%q) code = %d, want 200", tc.id, w.Code)
			}
			var resp copyChannelResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal copy response: %v (body=%s)", err, w.Body.String())
			}
			if resp.Success {
				t.Errorf("CopyChannel(%q) success = true, want false", tc.id)
			}
			if resp.Message == "" {
				t.Errorf("CopyChannel(%q) message empty, want error message", tc.id)
			}
			if len(resp.Data) != 0 {
				t.Errorf("CopyChannel(%q) data = %s, want absent", tc.id, resp.Data)
			}
		})
	}
}
