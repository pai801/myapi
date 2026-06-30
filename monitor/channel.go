package monitor

import (
	"fmt"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/message"
	"github.com/songquanpeng/one-api/model"
)

func notifyRootUser(subject string, content string) {
	if config.MessagePusherAddress != "" {
		err := message.SendMessage(subject, content, content)
		if err != nil {
			logger.Log.Errorf("failed to send message: %s", err.Error())
		}
	}
}

// DisableChannel disable & notify
func DisableChannel(channelId int, channelName string, reason string) {
	model.UpdateChannelStatusById(channelId, model.ChannelStatusAutoDisabled)
	logger.Log.Infof("channel #%d (%s) has been disabled by rule match: %s", channelId, channelName, reason)
	subject := fmt.Sprintf("渠道状态变更提醒")
	content := fmt.Sprintf(`渠道「%s」（#%d）已被禁用。
禁用原因：%s`, channelName, channelId, reason)
	notifyRootUser(subject, content)
}

func MetricDisableChannel(channelId int, successRate float64) {
	model.UpdateChannelStatusById(channelId, model.ChannelStatusAutoDisabled)
	logger.Log.Infof("channel #%d has been disabled due to low success rate: %.2f", channelId, successRate*100)
	subject := fmt.Sprintf("渠道状态变更提醒")
	content := fmt.Sprintf(`渠道 #%d 已被系统自动禁用。
禁用原因：该渠道在最近 %d 次调用中成功率为 %.2f%%，低于系统阈值 %.2f%%。`,
		channelId, config.MetricQueueSize, successRate*100, config.MetricSuccessRateThreshold*100)
	notifyRootUser(subject, content)
}

// EnableChannel enable & notify
func EnableChannel(channelId int, channelName string) {
	model.UpdateChannelStatusById(channelId, model.ChannelStatusEnabled)
	logger.Log.Infof("channel #%d has been enabled", channelId)
	subject := fmt.Sprintf("渠道状态变更提醒")
	content := fmt.Sprintf(`渠道「%s」（#%d）已被重新启用。
您现在可以继续使用该渠道了。`, channelName, channelId)
	notifyRootUser(subject, content)
}
