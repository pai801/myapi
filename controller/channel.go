package controller

import (
	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/middleware"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/chanregistry"
	"net/http"
	"strconv"
	"strings"
)

// credentialCompressorFor 是 channel.go 查「凭证压缩能力」的接缝。
//
// 生产环境指向 chanregistry.GetCompressor；测试可替换为返回 nil，模拟「无任何渠道注册压缩器」
// 时 controller 回退到「按 '\n' 拆分」路径的行为。
var credentialCompressorFor = chanregistry.GetCompressor

func GetAllChannels(c *gin.Context) {
	p, _ := strconv.Atoi(c.Query("p"))
	if p < 0 {
		p = 0
	}
	channels, err := model.GetAllChannels(p*config.ItemsPerPage, config.ItemsPerPage, "limited")
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channels,
	})
	return
}

func SearchChannels(c *gin.Context) {
	keyword := c.Query("keyword")
	channels, err := model.SearchChannels(keyword)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channels,
	})
	return
}

func GetChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	channel, err := model.GetChannelById(id, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channel,
	})
	return
}

func AddChannel(c *gin.Context) {
	channel := model.Channel{}
	err := c.ShouldBindJSON(&channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	channel.CreatedTime = helper.GetTimestamp()
	keys := keysForChannel(channel)
	channels := make([]model.Channel, 0, len(keys))
	for _, key := range keys {
		localChannel := channel
		localChannel.Key = key
		channels = append(channels, localChannel)
	}
	err = model.BatchInsertChannels(channels)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}

// compactChannelKey 查询 channelType 对应的凭证压缩能力并压缩 raw。
//
//	(compacted, true)  → 该渠道有压缩能力，compacted 为归一化后的 Key；
//	(_, false)         → 无压缩能力（非压缩渠道，或该渠道未注册压缩器），调用方走原有路径。
//
// 压缩失败时**绝不回退到按行拆分**（那会把整段凭证拆碎），而是保留 raw 作为单条并记日志 ——
// 与压缩器实现自身「解析失败即原样返回」的语义一致。
//
// CompactKey 调用同样被 recover 包裹（PRD §5.10 故障隔离）：扩展方压缩器实现 panic 时
// 降级为「保留 raw 作为单条」，既不让单个渠道带崩主进程，也不把整段凭证拆碎。
func compactChannelKey(channelType int, raw string) (out string, ok bool) {
	compressor := credentialCompressorFor(channelType)
	if compressor == nil {
		return "", false
	}
	defer func() {
		if rec := recover(); rec != nil {
			logger.Log.Errorf("channel: compact credential key for channel type %d panicked: %v", channelType, rec)
			out, ok = raw, true // 降级：保留原值为单条，绝不回退按行拆分
		}
	}()
	compacted, err := compressor.CompactKey(raw)
	if err != nil {
		logger.Log.Errorf("channel: compact credential key for channel type %d failed: %v", channelType, err)
		return raw, true
	}
	return compacted, true
}

// keysForChannel 把渠道 Key 归一化成待落库的 key 列表。
//
// 既有语义（非压缩渠道）：一个 key 一行批量录入 —— 按 '\n' 拆分并跳过空行。
//
// 压缩渠道例外：Key 是整段凭证 JSON（可含缩进换行），必须作为**单条**落库，
// 否则会被逐行拆成十几条渠道记录；同时压缩成紧凑单行，避免与刷新写回的紧凑 JSON 比较不等。
func keysForChannel(channel model.Channel) []string {
	if compacted, ok := compactChannelKey(channel.Type, channel.Key); ok {
		if strings.TrimSpace(compacted) == "" {
			return nil
		}
		return []string{compacted}
	}
	keys := strings.Split(channel.Key, "\n")
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		out = append(out, key)
	}
	return out
}

func DeleteChannel(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	channel := model.Channel{Id: id}
	err := channel.Delete()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}

func DeleteDisabledChannel(c *gin.Context) {
	rows, err := model.DeleteDisabledChannel()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    rows,
	})
	return
}

func UpdateChannel(c *gin.Context) {
	channel := model.Channel{}
	err := c.ShouldBindJSON(&channel)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	// 压缩渠道：编辑保存不走按行拆分，多行凭证会整段存进单条渠道 —— 虽能被
	// json 解析，但刷新写回的是紧凑 JSON，格式不一致会导致首次刷新后仍多一次写库与无谓的 key
	// 比对失败；且列表展示/日志难看。故保存前归一化为紧凑单行。
	if compacted, ok := compactChannelKey(channel.Type, channel.Key); ok {
		channel.Key = compacted
	}
	err = channel.Update()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channel,
	})
	return
}

func ResetChannel(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	middleware.CooldownGlobal.ResetChannel(id)
	channel, err := model.GetChannelById(id, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	if channel.Status == model.ChannelStatusAutoDisabled {
		model.UpdateChannelStatusById(id, model.ChannelStatusEnabled)
		channel.Status = model.ChannelStatusEnabled
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    channel,
	})
	return
}
