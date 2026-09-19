package controller

import (
	"testing"

	"github.com/pai801/myapi/relay/chanregistry"
)

// panicCompactor 的 CompactKey 故意 panic，用于验证 PRD §5.10 的故障隔离：
// 扩展压缩器 panic 时，controller 的压缩接缝不得带崩进程，应降级为「保留原值为单条」。
type panicCompactor struct{}

func (panicCompactor) Supports(int) bool { return true }

func (panicCompactor) CompactKey(string) (string, error) { panic("injected CompactKey panic") }

// TestCompactChannelKeyIsolatesPanic 锁定：压缩器 CompactKey panic 时，
// compactChannelKey 返回 (raw, true) —— 保留整段凭证为单条，绝不回退按行拆分。
func TestCompactChannelKeyIsolatesPanic(t *testing.T) {
	swapCompressorFor(t, func(int) chanregistry.CredentialCompressor { return panicCompactor{} })

	const raw = "{\n  \"accessToken\": \"at\"\n}"
	got, ok := compactChannelKey(unregisteredCT54, raw)
	if !ok {
		t.Fatalf("compactChannelKey on panic ok = false, want true (degrade keeps raw as single key)")
	}
	if got != raw {
		t.Errorf("compactChannelKey on panic = %q, want raw kept (%q)", got, raw)
	}
}
