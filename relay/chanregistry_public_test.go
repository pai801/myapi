package relay

import (
	"testing"

	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/chanregistry"
)

// TestChanRegistryEmptyByDefault 锁定默认注册表的初始状态：主仓不登记任何凭证压缩器。
//
// controller 查不到压缩器即回退到「按 '\n' 拆分」路径（controller/channel.go 的
// compactChannelKey）；本测试证明该回退必然触发，而非依赖偶然。
func TestChanRegistryEmptyByDefault(t *testing.T) {
	if ids := chanregistry.RegisteredIDs(); len(ids) != 0 {
		t.Fatalf("chanregistry registered ids = %v, want empty", ids)
	}
	for _, ct := range []int{54, 55, 56, channeltype.OpenAI} {
		if c := chanregistry.GetCompressor(ct); c != nil {
			t.Errorf("GetCompressor(%d) = %v, want nil (no compressor registered)", ct, c)
		}
	}
}
