package chatgptsub

import (
	"sync"
)

const (
	ewmaAlpha     = 0.2 // Sub2API 使用的 α 值
	statsCapacity = 256
)

// ChannelStats 维护单个 channel 的健康指标（EWMA 错误率 + TTFT）
type ChannelStats struct {
	errorRate float64
	ttft      float64
	hasTTFT   bool
	count     int64 // 总请求次数（用于标识是否初始化）
}

type StatsManager struct {
	mu    sync.RWMutex
	stats map[int]*ChannelStats // channelID → stats
}

func NewStatsManager() *StatsManager {
	return &StatsManager{
		stats: make(map[int]*ChannelStats, statsCapacity),
	}
}

// ReportResult 上报一次请求结果。success=false 时计入错误；firstTokenMs>0 时更新 TTFT。
func (m *StatsManager) ReportResult(channelID int, success bool, firstTokenMs *int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cs, ok := m.stats[channelID]
	if !ok {
		cs = &ChannelStats{}
		m.stats[channelID] = cs
	}

	// EWMA 更新错误率：sample = 1.0（失败）或 0.0（成功）
	var errSample float64 = 0.0
	if !success {
		errSample = 1.0
	}
	if cs.count == 0 {
		cs.errorRate = errSample
	} else {
		cs.errorRate = ewmaAlpha*errSample + (1-ewmaAlpha)*cs.errorRate
	}

	// TTFT
	if firstTokenMs != nil && *firstTokenMs > 0 {
		sample := float64(*firstTokenMs)
		if cs.count == 0 {
			cs.ttft = sample
			cs.hasTTFT = true
		} else {
			cs.ttft = ewmaAlpha*sample + (1-ewmaAlpha)*cs.ttft
			cs.hasTTFT = true
		}
	}
	cs.count++

	// 失败时检查是否需要熔断（异步执行，不阻塞上报）
	if !success {
		go CheckAndDisable(channelID)
	}
}

// Reset 重置指定 channel 的统计（恢复启用时调用）
func (m *StatsManager) Reset(channelID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.stats, channelID)
}

// GetErrorRate 返回指定 channel 的 EWMA 错误率
func (m *StatsManager) GetErrorRate(channelID int) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cs, ok := m.stats[channelID]; ok {
		return cs.errorRate
	}
	return 0
}

// GetTTFT 返回指定 channel 的 EWMA TTFT（ms）
func (m *StatsManager) GetTTFT(channelID int) (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cs, ok := m.stats[channelID]; ok {
		return cs.ttft, cs.hasTTFT
	}
	return 0, false
}

// GetCandidateWeight 计算候选 channel 的权重分数（低错误率+低TTFT=高权重）
// 用于后续阶段的加权选择器。
func (m *StatsManager) GetCandidateWeight(channelID int) float64 {
	// 1.0 = 基准健康分，每 10% 错误率扣 0.5，TTFT>2000ms 扣 0.3
	errRate := m.GetErrorRate(channelID)
	weight := 1.0 - errRate*5.0 // 错误率 20% 时 weight=0，100% 时 weight=-4（但仍可能被选）
	if weight < 0 {
		weight = 0
	}
	return weight
}
