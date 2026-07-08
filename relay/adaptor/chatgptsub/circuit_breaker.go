package chatgptsub

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/channeltype"
)

// 熔断配置
const (
	// errorRateThreshold: EWMA 错误率超过此值时触发熔断
	errorRateThreshold = 0.5 // 50% 错误率

	// consecutiveFailThreshold: 连续失败次数超过此值时触发熔断（作为 errorRate 的补充）
	consecutiveFailThreshold = 5

	// probeInterval: 探活周期
	probeInterval = 5 * time.Minute

	// probeRequestTimeout: 探活请求超时
	probeRequestTimeout = 10 * time.Second

	// recoverySuccessThreshold: 连续成功探活次数达到此值时恢复 channel 状态
	recoverySuccessThreshold = 3
)

// CheckAndDisable 检查 channel 健康状态，EWMA 错误率超阈值则自动禁用
func CheckAndDisable(channelID int) {
	errRate := statsManager.GetErrorRate(channelID)
	if errRate < errorRateThreshold {
		return
	}

	// 先检查当前 status，避免重复写库
	ch, err := model.GetChannelById(channelID, true)
	if err != nil || ch == nil {
		return
	}
	if ch.Status == model.ChannelStatusAutoDisabled {
		return // 已禁用，不重复写
	}

	channelProber.ensureRunning()
	model.UpdateChannelStatusById(channelID, model.ChannelStatusAutoDisabled)
	logger.Log.Warnf("channel %d auto-disabled due to high error rate: %.2f", channelID, errRate)
}

// ChannelProber 定时探活 auto-disabled 的 channel
type ChannelProber struct {
	recoveryCounts map[int]int // channelID → 连续成功探活次数
	client         *http.Client
	startOnce      sync.Once
}

func NewChannelProber() *ChannelProber {
	return &ChannelProber{
		recoveryCounts: make(map[int]int),
		client: &http.Client{
			Timeout: probeRequestTimeout,
		},
	}
}

// ensureRunning 按需启动探活 goroutine（仅首次调用有效）
func (p *ChannelProber) ensureRunning() {
	p.startOnce.Do(func() {
		go p.RunProbeLoop(context.Background())
	})
}

// ProbeOnce 对单个 channel 执行探活：尝试请求 chatgpt.com 的轻量端点
// 使用 channel 的 Key 作为 bearer token，请求 GET https://chatgpt.com/backend-api/models
func (p *ChannelProber) ProbeOnce(ch *model.Channel) bool {
	if ch == nil {
		return false
	}

	// 构建探活请求
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		strings.TrimRight(ch.GetBaseURL(), "/")+"/backend-api/models",
		nil,
	)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+ch.Key)
	req.Header.Set("User-Agent", "myapi/1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// HTTP 200 视为 token 有效
	return resp.StatusCode == http.StatusOK
}

// RunProbeLoop 周期性地遍历 auto-disabled 的 ChatGPTSub channel，执行探活
// 连续 recoverySuccessThreshold 次成功则恢复启用
func (p *ChannelProber) RunProbeLoop(ctx context.Context) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeAll()
		}
	}
}

func (p *ChannelProber) probeAll() {
	p.ensureRunning()

	// 获取所有 ChatGPTSub 类型且状态为 auto-disabled 的 channel
	channels, err := model.GetAllChannels(0, 0, "disabled")
	if err != nil {
		logger.Log.Errorf("failed to get disabled channels: %v", err)
		return
	}

	for _, ch := range channels {
		if ch.Type != channeltype.ChatGPTSub {
			continue
		}
		if ch.Status != model.ChannelStatusAutoDisabled {
			continue
		}

		if p.ProbeOnce(ch) {
			p.recoveryCounts[ch.Id]++
			if p.recoveryCounts[ch.Id] >= recoverySuccessThreshold {
				// 恢复启用
				model.UpdateChannelStatusById(ch.Id, model.ChannelStatusEnabled)
				logger.Log.Warnf("channel %d recovered after %d successful probes", ch.Id, p.recoveryCounts[ch.Id])
				delete(p.recoveryCounts, ch.Id)
				// 重置健康统计
				statsManager.Reset(ch.Id)
			}
		} else {
			// 探活失败，重置计数
			p.recoveryCounts[ch.Id] = 0
		}
	}
}
