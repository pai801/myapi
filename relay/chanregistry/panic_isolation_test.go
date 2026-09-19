package chanregistry_test

import (
	"testing"

	"github.com/pai801/myapi/relay/chanregistry"
)

// panickingCompressor 的 Supports 故意 panic，用于验证 PRD §5.10 的故障隔离：
// 扩展压缩器实现 panic 时不得带崩主进程，应降级为「不支持该渠道」。
type panickingCompressor struct{}

func (panickingCompressor) Supports(int) bool { panic("injected Supports panic") }

func (panickingCompressor) CompactKey(raw string) (string, error) { return raw, nil }

// TestGetCompressorIsolatesSupportsPanic 锁定：唯一压缩器的 Supports panic 时，
// GetCompressor 不 panic、返回 nil（调用方回退到按 '\n' 拆分路径）。
func TestGetCompressorIsolatesSupportsPanic(t *testing.T) {
	r := chanregistry.New()
	r.Register("boom", panickingCompressor{})

	// 若 recover 缺失，此处会 panic 带崩测试进程。
	got := r.GetCompressor(54)
	if got != nil {
		t.Errorf("GetCompressor with panicking Supports = %v, want nil (degraded)", got)
	}
}

// TestGetCompressorSkipsPanickingAndFindsNext 锁定降级不阻断后续压缩器：
// 坏压缩器排在前、好压缩器排在后时，坏的被跳过、好的仍命中（保序语义不变）。
func TestGetCompressorSkipsPanickingAndFindsNext(t *testing.T) {
	r := chanregistry.New()
	r.Register("boom", panickingCompressor{})
	r.Register("good", fakeCompressor{supports: map[int]bool{54: true}, out: "good"})

	c := r.GetCompressor(54)
	if c == nil {
		t.Fatal("GetCompressor = nil, want the good compressor (panicking one must be skipped)")
	}
	if got, err := c.CompactKey("x"); err != nil || got != "good" {
		t.Errorf("CompactKey = (%q,%v), want (good,nil)", got, err)
	}
}
