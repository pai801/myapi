package chatgptsub

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/pai801/myapi/model"
)

// TestProbeOnceInvalidURL 验证传入 nil channel 时 ProbeOnce 返回 false
func TestProbeOnceInvalidURL(t *testing.T) {
	p := NewChannelProber()

	result := p.ProbeOnce(nil)
	require.False(t, result, "ProbeOnce with nil channel should return false")
}

// TestProbeOnceWithBadChannel 验证传入无法连接的基础 URL 时 ProbeOnce 返回 false
func TestProbeOnceWithBadChannel(t *testing.T) {
	p := NewChannelProber()

	baseURL := "http://127.0.0.1:1"
	ch := &model.Channel{
		Key:     "sk-test-key",
		BaseURL: &baseURL,
	}

	result := p.ProbeOnce(ch)
	require.False(t, result, "ProbeOnce with unreachable URL should return false")
}
