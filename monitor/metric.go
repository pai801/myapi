package monitor

import (
	"github.com/pai801/myapi/common/config"
)

var store = make(map[int][]bool)

// metricEvent 携带成功/失败标记，供单一 consumer 串行处理
type metricEvent struct {
	channelId int
	success   bool
}

// 单一 channel + 单一 consumer，避免两个 consumer 并发写 store 触发 concurrent map writes
var metricChan = make(chan metricEvent, config.MetricSuccessChanSize+config.MetricFailChanSize)

func consumeSuccess(channelId int) {
	if len(store[channelId]) > config.MetricQueueSize {
		store[channelId] = store[channelId][1:]
	}
	store[channelId] = append(store[channelId], true)
}

func consumeFail(channelId int) (bool, float64) {
	if len(store[channelId]) > config.MetricQueueSize {
		store[channelId] = store[channelId][1:]
	}
	store[channelId] = append(store[channelId], false)
	successCount := 0
	for _, success := range store[channelId] {
		if success {
			successCount++
		}
	}
	successRate := float64(successCount) / float64(len(store[channelId]))
	if len(store[channelId]) < config.MetricQueueSize {
		return false, successRate
	}
	if successRate < config.MetricSuccessRateThreshold {
		store[channelId] = make([]bool, 0)
		return true, successRate
	}
	return false, successRate
}

func metricConsumer() {
	for event := range metricChan {
		if event.success {
			consumeSuccess(event.channelId)
			continue
		}
		disable, successRate := consumeFail(event.channelId)
		if disable {
			go MetricDisableChannel(event.channelId, successRate)
		}
	}
}

func init() {
	if config.EnableMetric {
		go metricConsumer()
	}
}

// Emit 采用非阻塞投递：指标为尽力而为语义，channel 打满时直接丢弃事件，
// 宁可丢指标也不阻塞请求路径；若阻塞投递或按事件起 goroutine 排队，
// channel 打满时会导致 goroutine 无界堆积。丢弃属预期行为，不逐条打日志以免刷屏。
func Emit(channelId int, success bool) {
	if !config.EnableMetric {
		return
	}
	select {
	case metricChan <- metricEvent{channelId: channelId, success: success}:
	default:
	}
}
