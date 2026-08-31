package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/random"
)

var (
	TokenCacheSeconds         = config.SyncFrequency
	UserId2QuotaCacheSeconds  = config.SyncFrequency
	UserId2StatusCacheSeconds = config.SyncFrequency
	GroupModelsCacheSeconds   = config.SyncFrequency
)

// tokenCacheKey 统一构造令牌缓存键：读侧与失效侧必须引用同一构造，
// 防止两侧键前缀各自演化导致失效落空
func tokenCacheKey(key string) string {
	return fmt.Sprintf("token:%s", key)
}

// userEnabledCacheKey 同上，收敛用户状态缓存键的构造
func userEnabledCacheKey(id int) string {
	return fmt.Sprintf("user_enabled:%d", id)
}

// userQuotaCacheKey 同上，收敛用户额度缓存键的构造（读侧/增量侧/回写侧共用）
func userQuotaCacheKey(id int) string {
	return fmt.Sprintf("user_quota:%d", id)
}

// userQuotaDeltaScript 在缓存键存在时对其原子增量并续期 TTL：
// - 键不存在时跳过：DECRBY/INCRBY 对不存在的键会以 0 为基数凭空建键（且无 TTL），
//   预扣会创建 -quota 的假负值、退回会创建 +quota 的假余额，均可造成 TTL 内持续误判；
//   跳过则交由下次 CacheGetUserQuota 回源重建真实值。
// - 键存在但余额不足时允许扣成负数：缓存值只是近似，预扣额超过缓存值时扣负后
//   额度检查会拒绝后续请求，待结算/回源修正，属于预期语义。
// - 续期 TTL：额度每条写路径都同步缓存，活跃用户的常驻缓存值始终准确。
var userQuotaDeltaScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('INCRBY', KEYS[1], ARGV[1])
  redis.call('EXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`)

func CacheGetTokenByKey(key string) (*Token, error) {
	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}
	var token Token
	if !common.RedisEnabled {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		return &token, err
	}
	tokenObjectString, err := common.RedisGet(tokenCacheKey(key))
	if err != nil {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		if err != nil {
			return nil, err
		}
		jsonBytes, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		err = common.RedisSet(tokenCacheKey(key), string(jsonBytes), time.Duration(TokenCacheSeconds)*time.Second)
		if err != nil {
			logger.Log.Errorf("Redis set token error: " + err.Error())
		}
		return &token, nil
	}
	err = json.Unmarshal([]byte(tokenObjectString), &token)
	return &token, err
}

func fetchAndUpdateUserQuota(ctx context.Context, id int) (quota int64, err error) {
	quota, err = GetUserQuota(id)
	if err != nil {
		return 0, err
	}
	// 批量模式下 DB 是陈旧值（预扣/退回仍在缓冲区），回写缓存前必须叠加未落盘差值，
	// 否则回源会把缓存拉回高估的陈旧值，使批量间隔内的额度检查失效
	if config.BatchUpdateEnabled {
		quota += getPendingUserQuotaDelta(id)
	}
	if !common.RedisEnabled {
		// 无 Redis 形态下无需回写缓存，仅返回叠加差值后的额度（便于无 Redis 单测覆盖）
		return quota, nil
	}
	err = common.RedisSet(userQuotaCacheKey(id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	if err != nil {
		logger.Log.Errorf("Redis set user quota error: " + err.Error())
	}
	return
}

func CacheGetUserQuota(ctx context.Context, id int) (quota int64, err error) {
	if !common.RedisEnabled {
		return GetUserQuota(id)
	}
	quotaString, err := common.RedisGet(userQuotaCacheKey(id))
	if err != nil {
		return fetchAndUpdateUserQuota(ctx, id)
	}
	quota, err = strconv.ParseInt(quotaString, 10, 64)
	if err != nil {
		// 缓存值损坏时不能吞错返回 0：该用户所有请求会被"额度不足"误拒直到 TTL 过期，
		// 记日志并回源 DB 重建缓存后返回真实值
		logger.Log.Errorf("parse user quota cache value %q for user %d failed: %s", quotaString, id, err.Error())
		return fetchAndUpdateUserQuota(ctx, id)
	}
	return quota, nil
}

// CacheUpdateUserQuota 以"读 DB(叠加差值) → SET 回写"覆盖缓存绝对值，该读改写并非原子，
// 与并发增量 Lua 脚本（DecreaseUserQuota 预扣 / IncreaseUserQuota 退回的 cacheIncrUserQuota）交织时：
// T1 读到旧值后 T2 的增量先落盘，再被 T1 的 SET 覆盖，T2 的增量丢失——
// T2 为扣减则缓存高估一个预扣额（有界透支），T2 为退回则缓存低估（保守拒绝）。
// 窗口为读（含一次 DB 查询，远程 DB 下可达毫秒级）与 SET 之间的间隙，且有自愈路径：后续 PostConsumeReset 回写、
// 下次 CacheGetUserQuota 回源、TTL 过期均会重建真实值，不会持久漂移，
// 故接受该有界窗口而不引入分布式锁开销。
func CacheUpdateUserQuota(ctx context.Context, id int) error {
	if !common.RedisEnabled {
		return nil
	}
	quota, err := GetUserQuota(id)
	if err != nil {
		return err
	}
	// 批量模式下 DB 是陈旧值（消费结算的补扣/退回仍在缓冲区），
	// 直接回写会把缓存拉回高估的陈旧值，必须叠加未落盘差值
	if config.BatchUpdateEnabled {
		quota += getPendingUserQuotaDelta(id)
	}
	err = common.RedisSet(userQuotaCacheKey(id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	return err
}

// CacheDecreaseUserQuota 对缓存额度做增量扣减（预扣/扣费路径调用），仅键存在时生效
func CacheDecreaseUserQuota(id int, quota int64) error {
	return cacheIncrUserQuota(id, -quota)
}

// CacheIncreaseUserQuota 对缓存额度做增量退回（回滚/退款路径调用），仅键存在时生效
func CacheIncreaseUserQuota(id int, quota int64) error {
	return cacheIncrUserQuota(id, quota)
}

func cacheIncrUserQuota(id int, delta int64) error {
	if !common.RedisEnabled {
		return nil
	}
	return userQuotaDeltaScript.Run(context.Background(), common.RDB,
		[]string{userQuotaCacheKey(id)}, delta, UserId2QuotaCacheSeconds).Err()
}

// CacheInvalidateTokenByKey 在令牌写路径成功后失效 token:<key> 缓存，
// 消除"删除/禁用后旧凭证在 TTL 窗口内仍可调用"的安全窗口。
// 删除缓存失败仅记日志不回滚业务：缓存最终一致可接受，
// 最坏情况退化为既有 TTL 窗口，业务数据（DB）始终是权威状态。
func CacheInvalidateTokenByKey(key string) error {
	if !common.RedisEnabled {
		// 未启用 Redis 时 CacheGetTokenByKey 直查 DB，无缓存可失效
		return nil
	}
	if key == "" {
		// 正常写路径的 key 均非空，出现空 key 说明调用方状态异常，需留痕排查
		logger.Log.Warnf("CacheInvalidateTokenByKey: empty token key, skip invalidation")
		return nil
	}
	err := common.RedisDel(tokenCacheKey(key))
	if err != nil {
		logger.Log.Errorf("Redis del token error: " + err.Error())
	}
	return err
}

// CacheInvalidateUserEnabled 在用户状态写路径成功后失效 user_enabled:<id> 缓存。
// 调用方无法预判本次 Update 是否变更了 status，故统一失效，
// 代价仅为低频管理操作后的一次缓存回源。
func CacheInvalidateUserEnabled(userId int) error {
	if !common.RedisEnabled {
		// 未启用 Redis 时 CacheIsUserEnabled 直查 DB，无缓存可失效
		return nil
	}
	if userId == 0 {
		logger.Log.Warnf("CacheInvalidateUserEnabled: empty user id, skip invalidation")
		return nil
	}
	err := common.RedisDel(userEnabledCacheKey(userId))
	if err != nil {
		logger.Log.Errorf("Redis del user enabled error: " + err.Error())
	}
	return err
}

func CacheIsUserEnabled(userId int) (bool, error) {
	if !common.RedisEnabled {
		return IsUserEnabled(userId)
	}
	enabled, err := common.RedisGet(userEnabledCacheKey(userId))
	if err == nil {
		return enabled == "1", nil
	}

	userEnabled, err := IsUserEnabled(userId)
	if err != nil {
		return false, err
	}
	enabled = "0"
	if userEnabled {
		enabled = "1"
	}
	err = common.RedisSet(userEnabledCacheKey(userId), enabled, time.Duration(UserId2StatusCacheSeconds)*time.Second)
	if err != nil {
		logger.Log.Errorf("Redis set user enabled error: " + err.Error())
	}
	return userEnabled, err
}

func CacheGetGroupModels(ctx context.Context, group string) ([]string, error) {
	if !common.RedisEnabled {
		return GetGroupModels(ctx, group)
	}
	modelsStr, err := common.RedisGet(fmt.Sprintf("group_models:%s", group))
	if err == nil {
		return strings.Split(modelsStr, ","), nil
	}
	models, err := GetGroupModels(ctx, group)
	if err != nil {
		return nil, err
	}
	err = common.RedisSet(fmt.Sprintf("group_models:%s", group), strings.Join(models, ","), time.Duration(GroupModelsCacheSeconds)*time.Second)
	if err != nil {
		logger.Log.Errorf("Redis set group models error: " + err.Error())
	}
	return models, nil
}

var group2model2channels map[string]map[string][]*Channel
var channelSyncLock sync.RWMutex

func InitChannelCache() {
	newChannelId2channel := make(map[int]*Channel)
	var channels []*Channel
	DB.Where("status = ?", ChannelStatusEnabled).Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
	}
	var abilities []*Ability
	DB.Where("enabled = ?", true).Find(&abilities)
	groups := make(map[string]bool)
	for _, ability := range abilities {
		groups[ability.Group] = true
	}
	newGroup2model2channels := make(map[string]map[string][]*Channel)
	for group := range groups {
		newGroup2model2channels[group] = make(map[string][]*Channel)
	}
	for _, channel := range channels {
		groups := strings.Split(channel.Group, ",")
		for _, group := range groups {
			models := channel.GetModels()
			for _, model := range models {
				if _, ok := newGroup2model2channels[group][model]; !ok {
					newGroup2model2channels[group][model] = make([]*Channel, 0)
				}
				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel)
			}
		}
	}

	// sort by priority
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return channels[i].GetPriority() > channels[j].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	channelSyncLock.Lock()
	group2model2channels = newGroup2model2channels
	channelSyncLock.Unlock()
	logger.Log.Debugf("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.Log.Debugf("syncing channels from database")
		InitChannelCache()
	}
}

func CacheGetGroupChannels(group string) []*Channel {
	var channels []*Channel
	var err error
	if config.MemoryCacheEnabled {
		channelSyncLock.RLock()
		defer channelSyncLock.RUnlock()
		groupModels, ok := group2model2channels[group]
		if !ok {
			return nil
		}
		seen := make(map[int]bool)
		for _, chs := range groupModels {
			for _, ch := range chs {
				if !seen[ch.Id] {
					seen[ch.Id] = true
					channels = append(channels, ch)
				}
			}
		}
		sort.Slice(channels, func(i, j int) bool {
			return int64(channels[i].Id) < int64(channels[j].Id)
		})
	} else {
		channels, err = GetChannelsByGroup(group)
		if err != nil {
			return nil
		}
	}
	return channels
}

func CacheGetRandomSatisfiedChannel(group string, model string, ignoreFirstPriority bool) (*Channel, error) {
	if !config.MemoryCacheEnabled {
		return GetRandomSatisfiedChannel(group, model, ignoreFirstPriority)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	channels := group2model2channels[group][model]
	if len(channels) == 0 {
		return nil, errors.New("channel not found")
	}
	endIdx := len(channels)
	// choose by priority
	firstChannel := channels[0]
	if firstChannel.GetPriority() > 0 {
		for i := range channels {
			if channels[i].GetPriority() != firstChannel.GetPriority() {
				endIdx = i
				break
			}
		}
	}
	idx := rand.Intn(endIdx)
	if ignoreFirstPriority {
		if endIdx < len(channels) { // which means there are more than one priority
			idx = random.RandRange(endIdx, len(channels))
		}
	}
	return channels[idx], nil
}
