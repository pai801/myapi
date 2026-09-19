package channeltype

import (
	"sync"
	"testing"

	"github.com/pai801/myapi/relay/apitype"
)

// TestToAPITypeBuiltinFallback 守护内置渠道的映射不受外部注册表影响。
func TestToAPITypeBuiltinFallback(t *testing.T) {
	cases := []struct {
		channelType int
		want        int
	}{
		{OpenAI, apitype.OpenAI},
		{Anthropic, apitype.Anthropic},
		{Gemini, apitype.Gemini},
		{Coze, apitype.Coze},
		{ChatGPTSub, apitype.ChatGPTSub},
	}
	for _, tc := range cases {
		if got := ToAPIType(tc.channelType); got != tc.want {
			t.Errorf("ToAPIType(%d) = %d, want %d", tc.channelType, got, tc.want)
		}
	}
}

// TestToAPITypeUnknownFallsBackToOpenAI 守护未注册渠道回退到 OpenAI 的既有语义。
func TestToAPITypeUnknownFallsBackToOpenAI(t *testing.T) {
	const unknown = 1 << 20 // 远离内置与常见扩展取值的任意值，且从不在本包内注册
	if got := ToAPIType(unknown); got != apitype.OpenAI {
		t.Errorf("ToAPIType(%d) = %d, want apitype.OpenAI(%d)", unknown, got, apitype.OpenAI)
	}
}

// TestToAPITypeUsesRegisteredExtension 守护核心契约：外部扩展方经 RegisterAPIType 登记后，
// 本仓无需任何改动即可把其渠道类型映射到目标 API 类型。
func TestToAPITypeUsesRegisteredExtension(t *testing.T) {
	const extChannelType = 1<<20 + 1
	RegisterAPIType(extChannelType, apitype.Coze)
	t.Cleanup(func() { UnregisterAPIType(extChannelType) })
	if got := ToAPIType(extChannelType); got != apitype.Coze {
		t.Errorf("ToAPIType(%d) = %d, want apitype.Coze(%d)", extChannelType, got, apitype.Coze)
	}
}

// TestRegisterAPITypeConcurrentSafe 在 -race 下守护注册与查找的并发安全（注册在 init 期、
// 查找在运行时，两者可能并发）。
func TestRegisterAPITypeConcurrentSafe(t *testing.T) {
	const n = 50
	t.Cleanup(func() {
		for i := 0; i < n; i++ {
			UnregisterAPIType(2<<20 + i)
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ct := 2<<20 + i
		wg.Add(2)
		go func() {
			defer wg.Done()
			RegisterAPIType(ct, apitype.Proxy)
		}()
		go func() {
			defer wg.Done()
			_ = ToAPIType(ct)
		}()
	}
	wg.Wait()
}
