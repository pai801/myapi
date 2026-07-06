package middleware

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/channeltype"
)

var autoRoundRobinIndex map[string]int
var autoRoundRobinMu sync.Mutex

func nextAutoChannel(group string, channels []*model.Channel) (*model.Channel, int) {
	autoRoundRobinMu.Lock()
	defer autoRoundRobinMu.Unlock()

	if autoRoundRobinIndex == nil {
		autoRoundRobinIndex = make(map[string]int)
	}

	idx := autoRoundRobinIndex[group]
	autoRoundRobinIndex[group] = (idx + 1) % len(channels)
	ch := channels[idx]
	return ch, idx
}

func setDistributeContext(c *gin.Context, channel *model.Channel, requestModel string, suggestedModel string) error {
	c.Set(ctxkey.OriginalModel, requestModel)
	c.Set(ctxkey.RequestModel, requestModel)
	c.Set(ctxkey.SuggestedModel, suggestedModel)
	c.Set(ctxkey.ChannelId, channel.Id)
	c.Set(ctxkey.Channel, channel.Type)
	c.Set(ctxkey.ChannelName, channel.Name)
	return nil
}

func matchChannelsByAlias(requestModel string, channels []*model.Channel) ([]*model.Channel, string) {
	alias := model.SimplifyModelName(requestModel)
	if alias == "" {
		return nil, ""
	}

	// First pass: exact match
	var exactMatches []*model.Channel
	for _, ch := range channels {
		if slices.Contains(ch.GetAlias(), alias) {
			exactMatches = append(exactMatches, ch)
		}
	}
	if len(exactMatches) > 0 {
		return exactMatches, alias
	}

	// Second pass: prefix match
	var prefixMatches []*model.Channel
	for _, ch := range channels {
		for _, a := range ch.GetAlias() {
			if strings.HasPrefix(a, alias) {
				prefixMatches = append(prefixMatches, ch)
				break
			}
		}
	}
	return prefixMatches, alias
}

func selectAutoModel(channel *model.Channel) string {
	if channel.ModelsAlias == "" && channel.Models == "" {
		return ""
	}
	if channel.Models != "" {
		parts := channel.GetModels()
		return strings.TrimSpace(parts[rand.Intn(len(parts))])
	}
	return ""
}

func weightedRandomSelect(channels []*model.Channel) *model.Channel {
	if len(channels) == 0 {
		return nil
	}
	if len(channels) == 1 {
		return channels[0]
	}

	var totalWeight int64
	weights := make([]int64, len(channels))
	for i, ch := range channels {
		if priority := ch.GetPriority(); priority > 0 {
			weights[i] = priority
			totalWeight += priority
		} else {
			weights[i] = 0
		}
	}

	if totalWeight == 0 {
		return channels[rand.Intn(len(channels))]
	}

	r := rand.Int63n(totalWeight)
	cumulative := int64(0)
	for i, w := range weights {
		cumulative += w
		if r < cumulative {
			return channels[i]
		}
	}
	return channels[len(channels)-1]
}

func autoDistribute(ctx context.Context, group string, channels []*model.Channel) (*model.Channel, string, error) {
	if len(channels) == 0 {
		return nil, "", fmt.Errorf("当前分组 %s 下无可用渠道", group)
	}
	ch, _ := nextAutoChannel(group, channels)
	selectedModel := selectAutoModel(ch)
	logger.Log.Debugf("autoDistribute: round-robin selected channel #%d model %s for group %s", ch.Id, selectedModel, group)
	return ch, selectedModel, nil
}

func nonAutoDistribute(ctx context.Context, userId int, requestModel string, channels []*model.Channel) (*model.Channel, string, error) {
	matched, alias := matchChannelsByAlias(requestModel, channels)
	if len(matched) == 0 {
		return nil, "", fmt.Errorf("no channel found for model %s", requestModel)
	}

	var ch *model.Channel

	// Check affinity first: prefer the last used channel for this (user, model)
	if affChId, ok := AffinityGlobal.Get(userId, requestModel); ok {
		logger.Log.Debugf("nonAutoDistribute: affinity hit for user %d model %s -> channel #%d", userId, requestModel, affChId)
		for _, c := range matched {
			if c.Id == affChId {
				ch = c
				break
			}
		}
		if ch == nil {
			logger.Log.Debugf("nonAutoDistribute: affinity channel #%d not in matched set, falling back to weighted select", affChId)
		}
	} else {
		logger.Log.Debugf("nonAutoDistribute: no affinity for user %d model %s, using weighted select", userId, requestModel)
	}

	// If no affinity or affinity channel not in matched set, pick weighted random by Priority
	if ch == nil {
		ch = weightedRandomSelect(matched)
		logger.Log.Debugf("nonAutoDistribute: weighted select chose channel #%d for user %d model %s", ch.Id, userId, requestModel)
	}
	if ch == nil {
		return nil, "", fmt.Errorf("no channel found for model %s", requestModel)
	}
	targedIdx := -1
	for idx, a := range ch.GetAlias() {
		if a == alias {
			targedIdx = idx
			break
		}
	}
	if targedIdx <= -1 {
		for idx, a := range ch.GetAlias() {
			if strings.HasPrefix(a, alias) {
				targedIdx = idx
				break
			}
		}
	}
	if targedIdx <= -1 {
		return nil, "", fmt.Errorf("no channel found for model %s", requestModel)
	}
	models := ch.GetModels()
	if targedIdx < len(models) {
		logger.Log.Debugf("nonAutoDistribute: selected channel #%d model %s for user %d request %s", ch.Id, models[targedIdx], userId, requestModel)
		return ch, models[targedIdx], nil
	}
	return nil, "", fmt.Errorf("no model found for alias %s", alias)
}

type ModelRequest struct {
	Model string `json:"model" form:"model"`
}

func Distribute() func(c *gin.Context) {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		userId := c.GetInt(ctxkey.Id)
		userGroup, _ := model.CacheGetUserGroup(userId)
		c.Set(ctxkey.Group, userGroup)

		var channel *model.Channel
		var requestModel string
		var suggestedModel string
		var err error

		channelId, ok := c.Get(ctxkey.SpecificChannelId)
		if ok {
			id, err := strconv.Atoi(channelId.(string))
			if err != nil {
				abortWithMessage(c, http.StatusBadRequest, "无效的渠道 Id")
				return
			}
			channel, err = model.GetChannelById(id, true)
			if err != nil {
				abortWithMessage(c, http.StatusBadRequest, "无效的渠道 Id")
				return
			}
			if channel.Status != model.ChannelStatusEnabled {
				abortWithMessage(c, http.StatusForbidden, "该渠道已被禁用")
				return
			}
			requestModel = c.GetString(ctxkey.RequestModel)
			suggestedModel = requestModel
		} else {
			requestModel = c.GetString(ctxkey.RequestModel)
			if requestModel == "" {
				requestModel = "auto"
				c.Set(ctxkey.RequestModel, requestModel)
			}
			channel, suggestedModel, err = SelectChannel(ctx, userGroup, requestModel, -1, userId)
			if err != nil {
				abortWithMessage(c, http.StatusServiceUnavailable, err.Error())
				return
			}
		}

		logger.Log.Debugf("user id %d, user group: %s, request model: %s, suggested model: %s, using channel #%d", userId, userGroup, requestModel, suggestedModel, channel.Id)
		setDistributeContext(c, channel, requestModel, suggestedModel)
		SetupContextForSelectedChannel(c, channel, suggestedModel)
		c.Next()
	}
}

func filterCoolingChannels(channels []*model.Channel) []*model.Channel {
	var result []*model.Channel
	for _, ch := range channels {
		if !CooldownGlobal.IsCoolingDown(ch.Id) {
			result = append(result, ch)
		}
	}
	return result
}

func filterLastFailedChannel(channels []*model.Channel, lastFailedChannelId int) []*model.Channel {
	if lastFailedChannelId <= 0 {
		return channels
	}
	var result []*model.Channel
	for _, ch := range channels {
		if ch.Id != lastFailedChannelId {
			result = append(result, ch)
		}
	}
	return result
}

func SelectChannel(ctx context.Context, group, requestModel string, lastFailedChannelId int, userId int) (*model.Channel, string, error) {
	channels := model.CacheGetGroupChannels(group)
	channels = filterCoolingChannels(channels)
	channels = filterLastFailedChannel(channels, lastFailedChannelId)
	if len(channels) == 0 {
		return nil, "", fmt.Errorf("no channels available for retry in group %s", group)
	}
	logger.Log.Debugf("SelectChannel: group=%s model=%s userId=%d candidates=%d", group, requestModel, userId, len(channels))
	if requestModel == "auto" {
		return autoDistribute(ctx, group, channels)
	} else {
		return nonAutoDistribute(ctx, userId, requestModel, channels)
	}
}

func SetupContextForSelectedChannel(c *gin.Context, channel *model.Channel, modelName string) {
	c.Set(ctxkey.Channel, channel.Type)
	c.Set(ctxkey.ChannelId, channel.Id)
	c.Set(ctxkey.ChannelName, channel.Name)
	if channel.SystemPrompt != nil && *channel.SystemPrompt != "" {
		c.Set(ctxkey.SystemPrompt, *channel.SystemPrompt)
	}
	c.Set(ctxkey.ModelMapping, channel.GetModelMapping())
	c.Set(ctxkey.SuggestedModel, modelName) // for retry
	c.Request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", channel.Key))
	c.Set(ctxkey.BaseURL, channel.GetBaseURL())
	cfg, _ := channel.LoadConfig()
	// this is for backward compatibility
	if channel.Other != nil {
		switch channel.Type {
		case channeltype.Azure:
			if cfg.APIVersion == "" {
				cfg.APIVersion = *channel.Other
			}
		case channeltype.Xunfei:
			if cfg.APIVersion == "" {
				cfg.APIVersion = *channel.Other
			}
		case channeltype.Gemini:
			if cfg.APIVersion == "" {
				cfg.APIVersion = *channel.Other
			}
		case channeltype.AIProxyLibrary:
			if cfg.LibraryID == "" {
				cfg.LibraryID = *channel.Other
			}
		case channeltype.Ali:
			if cfg.Plugin == "" {
				cfg.Plugin = *channel.Other
			}
		}
	}
	c.Set(ctxkey.Config, cfg)
}
