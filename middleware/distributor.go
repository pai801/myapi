package middleware

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
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
	// 渠道列表会收缩（渠道被自动禁用是常态），全局存储的索引可能越界，取值前先取模折回
	idx %= len(channels)
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

// matchChannelsByAlias 把候选渠道按匹配精度拆成两组返回：
//   - exactMatches：渠道别名经 canonical 归一化后与请求 alias 相等（exact + 等价命中）；
//   - prefixMatches：仅靠前缀命中、且不在 exactMatches 内的渠道；
// 设计意图：分发时 exact 优先、prefix 仅作亲和兜底——避免 prefix-only 弱匹配渠道在
// weightedRandomSelect 中稀释 exact 强语义（如把 deepseek-v4-flash-extended 发到上游）。
func matchChannelsByAlias(requestModel string, channels []*model.Channel) (exactMatches []*model.Channel, prefixMatches []*model.Channel, alias string) {
	alias = model.SimplifyModelName(requestModel)
	if alias == "" {
		return nil, nil, ""
	}

	// exact + canonical 等价命中：精确度最高，最先返回
	requestCanonical := model.CanonicalizeSimplifiedName(alias)
	for _, ch := range channels {
		for _, a := range ch.GetAlias() {
			if model.CanonicalizeSimplifiedName(a) == requestCanonical {
				exactMatches = append(exactMatches, ch)
				break
			}
		}
	}

	// prefix 命中：补充只靠前缀匹配的渠道（如 deepseek-v4-flash-08xx 不在等价配置里但带前缀）
	// 与 exact 命中按渠道 ID 去重，保证同一渠道只出现在一组内。
	seen := make(map[int]struct{}, len(exactMatches))
	for _, ch := range exactMatches {
		seen[ch.Id] = struct{}{}
	}
	for _, ch := range channels {
		if _, ok := seen[ch.Id]; ok {
			continue
		}
		for _, a := range ch.GetAlias() {
			if strings.HasPrefix(a, alias) {
				prefixMatches = append(prefixMatches, ch)
				seen[ch.Id] = struct{}{}
				break
			}
		}
	}
	return exactMatches, prefixMatches, alias
}

func selectAutoModel(channel *model.Channel) string {
	if channel.ModelsAlias == "" && channel.Models == "" {
		return ""
	}
	if channel.Models != "" {
		parts := channel.GetModels()
		// auto 模式选模型时跳过冷却中的模型；全部冷却时兜底随机返回
		// （全部冷却的渠道理论上已被 filterCoolingChannels 剔除，不会走到这里）
		available := make([]string, 0, len(parts))
		for _, p := range parts {
			m := strings.TrimSpace(p)
			if !CooldownGlobal.IsCoolingDown(channel.Id, m) {
				available = append(available, m)
			}
		}
		if len(available) > 0 {
			return available[rand.Intn(len(available))]
		}
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

// resolveAliasModelIndex 在渠道别名列表中定位 alias 的索引（别名与 Models 按下标一一对应）：
// 精确 → canonical 等价 → 前缀，nonAutoDistribute 与 filterCoolingChannels 共用此匹配规则。
// 未命中返回 -1。
func resolveAliasModelIndex(ch *model.Channel, alias string) int {
	aliasList := ch.GetAlias()
	for idx, a := range aliasList {
		if a == alias {
			return idx
		}
	}
	// 等价匹配必须先于前缀匹配，否则前缀可能抢先命中错误的别名索引
	canonicalAlias := model.CanonicalizeSimplifiedName(alias)
	for idx, a := range aliasList {
		if model.CanonicalizeSimplifiedName(a) == canonicalAlias {
			return idx
		}
	}
	for idx, a := range aliasList {
		if strings.HasPrefix(a, alias) {
			return idx
		}
	}
	return -1
}

// resolveSpecificChannelModel 将指定渠道路径的请求模型解析为该渠道的实际模型，
// 使失败冷却写入的 key 与 filterCoolingChannels 查询的 key 同源；
// 解析失败（渠道不支持该模型）时退化为原始请求模型
func resolveSpecificChannelModel(channel *model.Channel, requestModel string) string {
	alias := model.SimplifyModelName(requestModel)
	// 空别名会使前缀匹配 HasPrefix(a, "") 恒真而错误命中 models[0]，直接退化为原始请求模型
	if alias == "" {
		return requestModel
	}
	idx := resolveAliasModelIndex(channel, alias)
	models := channel.GetModels()
	if idx >= 0 && idx < len(models) {
		return models[idx]
	}
	return requestModel
}

func nonAutoDistribute(ctx context.Context, userId int, requestModel string, channels []*model.Channel) (*model.Channel, string, error) {
	exactMatches, prefixMatches, alias := matchChannelsByAlias(requestModel, channels)
	if len(exactMatches) == 0 && len(prefixMatches) == 0 {
		return nil, "", fmt.Errorf("no channel found for model %s", requestModel)
	}

	// exact 集是 weightedRandomSelect 的主选池；prefix 集仅在 exact 为空时兜底，避免 prefix-only
	// 弱匹配渠道稀释 exact 强语义（如 deepseek-v4-flash-extended 抢占 deepseek-v4-flash 请求）。
	primary := exactMatches
	if len(primary) == 0 {
		primary = prefixMatches
	}

	var ch *model.Channel

	// 亲和命中优先：先在 exact 集找、再在 prefix 集找；都没有才在 primary 集内 weighted 选择
	if affChId, ok := AffinityGlobal.Get(userId, requestModel); ok {
		logger.Log.Debugf("nonAutoDistribute: affinity hit for user %d model %s -> channel #%d", userId, requestModel, affChId)
		for _, c := range exactMatches {
			if c.Id == affChId {
				ch = c
				break
			}
		}
		if ch == nil {
			for _, c := range prefixMatches {
				if c.Id == affChId {
					ch = c
					break
				}
			}
		}
		if ch == nil {
			logger.Log.Debugf("nonAutoDistribute: affinity channel #%d not in matched set, falling back to weighted select", affChId)
		}
	} else {
		logger.Log.Debugf("nonAutoDistribute: no affinity for user %d model %s, using weighted select", userId, requestModel)
	}

	if ch == nil {
		ch = weightedRandomSelect(primary)
		logger.Log.Debugf("nonAutoDistribute: weighted select chose channel #%d for user %d model %s", ch.Id, userId, requestModel)
	}
	if ch == nil {
		return nil, "", fmt.Errorf("no channel found for model %s", requestModel)
	}
	targedIdx := resolveAliasModelIndex(ch, alias)
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
		tokenGroup := c.GetString(ctxkey.Group)
		if tokenGroup == "" {
			tokenGroup = "default"
		}

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
			// 解析为渠道实际模型，使冷却 key 与过滤侧同源；解析失败退化为原始请求模型
			suggestedModel = resolveSpecificChannelModel(channel, requestModel)
		} else {
			requestModel = c.GetString(ctxkey.RequestModel)
			if requestModel == "" {
				requestModel = "auto"
				c.Set(ctxkey.RequestModel, requestModel)
			}
			channel, suggestedModel, err = SelectChannel(ctx, tokenGroup, requestModel, -1, userId)
			if err != nil {
				abortWithMessage(c, http.StatusServiceUnavailable, err.Error())
				return
			}
		}

		logger.Log.Debugf("user id %d, token group: %s, request model: %s, suggested model: %s, using channel #%d", userId, tokenGroup, requestModel, suggestedModel, channel.Id)
		setDistributeContext(c, channel, requestModel, suggestedModel)
		SetupContextForSelectedChannel(c, channel, suggestedModel)
		c.Next()
	}
}

// filterCoolingChannels 按 (渠道, 实际服务模型) 粒度剔除冷却中的渠道：
//   - 非 auto：解析该渠道服务 requestModel 的实际模型，命中冷却才剔除；
//     渠道不支持该模型时不因冷却剔除（交由后续匹配自然淘汰）。
//   - auto：仅当渠道支持的全部模型都在冷却中才剔除，部分可用则保留。
func filterCoolingChannels(channels []*model.Channel, requestModel string) []*model.Channel {
	if requestModel == "auto" {
		var result []*model.Channel
		for _, ch := range channels {
			if !isChannelFullyCooling(ch) {
				result = append(result, ch)
			}
		}
		return result
	}

	alias := model.SimplifyModelName(requestModel)
	if alias == "" {
		return channels
	}
	var result []*model.Channel
	for _, ch := range channels {
		models := ch.GetModels()
		idx := resolveAliasModelIndex(ch, alias)
		// 渠道不支持该请求模型（无别名命中或别名无对应模型）时不因冷却剔除
		if idx < 0 || idx >= len(models) {
			result = append(result, ch)
			continue
		}
		if !CooldownGlobal.IsCoolingDown(ch.Id, models[idx]) {
			result = append(result, ch)
		}
	}
	return result
}

// isChannelFullyCooling 判断渠道支持的全部模型是否都在冷却中；
// 渠道级条目（Put 时不带模型）覆盖该渠道全部模型，优先命中。
func isChannelFullyCooling(ch *model.Channel) bool {
	if CooldownGlobal.IsCoolingDown(ch.Id, "") {
		return true
	}
	models := ch.GetModels()
	// 渠道未配置模型时无具体模型可查，仅有渠道级条目生效（已在上面查过）
	if len(models) == 0 {
		return false
	}
	for _, m := range models {
		if !CooldownGlobal.IsCoolingDown(ch.Id, m) {
			return false
		}
	}
	return true
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
	channels = filterCoolingChannels(channels, requestModel)
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
