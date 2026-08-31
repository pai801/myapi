package model

import (
	"encoding/json"
	"strings"
	"sync"
	"unicode"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"gorm.io/gorm"
)

const (
	ChannelStatusUnknown          = 0
	ChannelStatusEnabled          = 1 // don't use 0, 0 is the default value!
	ChannelStatusManuallyDisabled = 2 // also don't use 0
	ChannelStatusAutoDisabled     = 3
)

type Channel struct {
	Id                 int     `json:"id"`
	Type               int     `json:"type" gorm:"default:0"`
	Key                string  `json:"key" gorm:"type:text"`
	Status             int     `json:"status" gorm:"default:1"`
	Name               string  `json:"name" gorm:"index"`
	Weight             *uint   `json:"weight" gorm:"default:0"`
	CreatedTime        int64   `json:"created_time" gorm:"bigint"`
	TestTime           int64   `json:"test_time" gorm:"bigint"`
	ResponseTime       int     `json:"response_time"` // in milliseconds
	BaseURL            *string `json:"base_url" gorm:"column:base_url;default:''"`
	Other              *string `json:"other"`   // DEPRECATED: please save config to field Config
	Balance            float64 `json:"balance"` // in USD
	BalanceUpdatedTime int64   `json:"balance_updated_time" gorm:"bigint"`
	Models             string  `json:"models"`
	models             []string
	Group              string  `json:"group" gorm:"type:varchar(32);default:'default'"`
	UsedQuota          int64   `json:"used_quota" gorm:"bigint;default:0"`
	ModelMapping       *string `json:"model_mapping" gorm:"type:varchar(1024);default:''"`
	Priority           *int64  `json:"priority" gorm:"bigint;default:1"`
	ModelsAlias        string  `json:"models_alias" gorm:"type:text"`
	alias              []string
	Config             string  `json:"config"`
	SystemPrompt       *string `json:"system_prompt" gorm:"type:text"`
}

type ChannelConfig struct {
	Region            string `json:"region,omitempty"`
	SK                string `json:"sk,omitempty"`
	AK                string `json:"ak,omitempty"`
	UserID            string `json:"user_id,omitempty"`
	APIVersion        string `json:"api_version,omitempty"`
	LibraryID         string `json:"library_id,omitempty"`
	Plugin            string `json:"plugin,omitempty"`
	VertexAIProjectID string `json:"vertex_ai_project_id,omitempty"`
	VertexAIADC       string `json:"vertex_ai_adc,omitempty"`
}

// SimplifyModelName strips non-alphanumeric characters and lowercases the model name.
// This produces a simplified alias suitable for display or matching purposes.
func SimplifyModelName(modelName string) string {
	splits := strings.Split(modelName, "/")
	modelName = splits[len(splits)-1]
	var builder strings.Builder
	for _, r := range modelName {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(unicode.ToLower(r))
		}
	}
	return builder.String()
}

func GetAllChannels(startIdx int, num int, scope string) ([]*Channel, error) {
	var channels []*Channel
	var err error
	switch scope {
	case "all":
		err = DB.Order("id desc").Find(&channels).Error
	case "disabled":
		err = DB.Order("id desc").Where("status = ? or status = ?", ChannelStatusAutoDisabled, ChannelStatusManuallyDisabled).Find(&channels).Error
	default:
		err = DB.Order("id desc").Limit(num).Offset(startIdx).Omit("key").Find(&channels).Error
	}
	return channels, err
}

func SearchChannels(keyword string) (channels []*Channel, err error) {
	err = DB.Omit("key").Where("id = ? or name LIKE ?", helper.String2Int(keyword), keyword+"%").Find(&channels).Error
	return channels, err
}

func GetChannelById(id int, selectAll bool) (*Channel, error) {
	channel := Channel{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(&channel, "id = ?", id).Error
	} else {
		err = DB.Omit("key").First(&channel, "id = ?", id).Error
	}
	return &channel, err
}

func BatchInsertChannels(channels []Channel) error {
	var err error
	for i := range channels {
		channels[i].autoGenerateModelsAlias()
	}
	err = DB.Create(&channels).Error
	if err != nil {
		return err
	}
	for _, channel_ := range channels {
		err = channel_.AddAbilities()
		if err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) GetPriority() int64 {
	if channel.Priority == nil {
		return 0
	}
	return *channel.Priority
}

func (channel *Channel) GetBaseURL() string {
	if channel.BaseURL == nil {
		return ""
	}
	return *channel.BaseURL
}

// channelLazyMuMap 按渠道 Id 提供懒初始化锁：Channel 会被 JSON 序列化/值拷贝，
// 不能内嵌 sync.Mutex，故用包级锁表；同 Id 的实例共享一把锁，保证字段读写互斥
var channelLazyMuMap sync.Map // map[int]*sync.Mutex

func channelLazyMu(id int) *sync.Mutex {
	if v, ok := channelLazyMuMap.Load(id); ok {
		return v.(*sync.Mutex)
	}
	v, _ := channelLazyMuMap.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (channel *Channel) GetAlias() []string {
	mu := channelLazyMu(channel.Id)
	mu.Lock()
	defer mu.Unlock()
	if channel.alias == nil {
		if channel.ModelsAlias == "" {
			return nil
		}
		channel.alias = strings.Split(channel.ModelsAlias, ",")
	}
	return channel.alias
}

func (channel *Channel) GetModels() []string {
	mu := channelLazyMu(channel.Id)
	mu.Lock()
	defer mu.Unlock()
	if channel.models == nil {
		if channel.Models == "" {
			return nil
		}
		channel.models = strings.Split(channel.Models, ",")
	}
	return channel.models
}

func (channel *Channel) GetModelMapping() map[string]string {
	if channel.ModelMapping == nil || *channel.ModelMapping == "" || *channel.ModelMapping == "{}" {
		return nil
	}
	modelMapping := make(map[string]string)
	err := json.Unmarshal([]byte(*channel.ModelMapping), &modelMapping)
	if err != nil {
		logger.Log.Errorf("failed to unmarshal model mapping for channel %d, error: %s", channel.Id, err.Error())
		return nil
	}
	return modelMapping
}

func (channel *Channel) autoGenerateModelsAlias() {
	if channel.Models == "" {
		channel.ModelsAlias = ""
		return
	}
	parts := strings.Split(channel.Models, ",")
	aliases := make([]string, 0, len(parts))
	for _, part := range parts {
		aliases = append(aliases, SimplifyModelName(part))
	}
	channel.ModelsAlias = strings.Join(aliases, ",")
	channel.alias = aliases
	channel.models = parts
}

func (channel *Channel) Insert() error {
	var err error
	channel.autoGenerateModelsAlias()
	err = DB.Create(channel).Error
	if err != nil {
		return err
	}
	err = channel.AddAbilities()
	return err
}

func (channel *Channel) Update() error {
	var err error
	channel.autoGenerateModelsAlias()
	err = DB.Model(channel).Updates(channel).Error
	if err != nil {
		return err
	}
	DB.Model(channel).First(channel, "id = ?", channel.Id)
	err = channel.UpdateAbilities()
	if err != nil {
		return err
	}
	if config.MemoryCacheEnabled {
		InitChannelCache()
	}
	return nil
}

func (channel *Channel) UpdateResponseTime(responseTime int64) {
	err := DB.Model(channel).Select("response_time", "test_time").Updates(Channel{
		TestTime:     helper.GetTimestamp(),
		ResponseTime: int(responseTime),
	}).Error
	if err != nil {
		logger.Log.Errorf("failed to update response time: " + err.Error())
	}
}

func (channel *Channel) UpdateBalance(balance float64) {
	err := DB.Model(channel).Select("balance_updated_time", "balance").Updates(Channel{
		BalanceUpdatedTime: helper.GetTimestamp(),
		Balance:            balance,
	}).Error
	if err != nil {
		logger.Log.Errorf("failed to update balance: " + err.Error())
	}
}

func (channel *Channel) Delete() error {
	var err error
	err = DB.Delete(channel).Error
	if err != nil {
		return err
	}
	err = channel.DeleteAbilities()
	return err
}

func (channel *Channel) LoadConfig() (ChannelConfig, error) {
	var cfg ChannelConfig
	if channel.Config == "" {
		return cfg, nil
	}
	err := json.Unmarshal([]byte(channel.Config), &cfg)
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

func UpdateChannelStatusById(id int, status int) {
	err := UpdateAbilityStatus(id, status == ChannelStatusEnabled)
	if err != nil {
		logger.Log.Errorf("failed to update ability status: " + err.Error())
	}
	err = DB.Model(&Channel{}).Where("id = ?", id).Update("status", status).Error
	if err != nil {
		logger.Log.Errorf("failed to update channel status: " + err.Error())
	}
	if config.MemoryCacheEnabled {
		InitChannelCache()
	}
}

func UpdateChannelUsedQuota(id int, quota int64) {
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeChannelUsedQuota, id, quota)
		return
	}
	updateChannelUsedQuota(id, quota)
}

func updateChannelUsedQuota(id int, quota int64) {
	err := DB.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota)).Error
	if err != nil {
		logger.Log.Errorf("failed to update channel used quota: " + err.Error())
	}
}

func DeleteChannelByStatus(status int64) (int64, error) {
	return batchDeleteChannels(func(tx *gorm.DB) *gorm.DB {
		return tx.Where("status = ?", status)
	})
}

func DeleteDisabledChannel() (int64, error) {
	return batchDeleteChannels(func(tx *gorm.DB) *gorm.DB {
		return tx.Where("status = ? or status = ?", ChannelStatusAutoDisabled, ChannelStatusManuallyDisabled)
	})
}

// 批量删渠道必须与单删 Delete() 行为对齐：事务内先清 abilities 再删渠道，
// 否则遗留孤儿 ability 会在默认路由下被随机命中、查渠道报 ErrRecordNotFound，
// 该 (group, model) 的请求持续失败。applyWhere 在事务内基于 tx 构建条件
// （复用外部 DB 构建的 query 会绕开事务连接）。
// 旧版 SQLite 宿主参数上限为 999，Pluck 出的全量 id 整体进 IN ? 在大量渠道一次性清理
// （DeleteDisabledChannel 为周期任务，存在累积场景）时会报错，故删除按 500/批分片：
// 先一次性 Pluck 固定候选 id 集合，再每片独立事务内复用筛选条件二次确认
// （剔除 Pluck 后被重新启用等状态变化的渠道，与原单语句 DELETE 逐行评估条件的语义对齐），
// 随后该片先清 abilities 后删渠道。
// 部分成功语义：分片间事务独立提交，某片失败时前面各片已落库，返回失败前实际删除的行数，
// 调用方以 err != nil 判定整体失败
const batchDeleteChannelsChunkSize = 500

func batchDeleteChannels(applyWhere func(*gorm.DB) *gorm.DB) (int64, error) {
	var channelIds []int
	// 候选集合先一次性查出（只读 Pluck 无需事务），后续分片基于该集合二次确认
	if err := applyWhere(DB).Model(&Channel{}).Pluck("id", &channelIds).Error; err != nil {
		return 0, err
	}
	var affected int64
	for start := 0; start < len(channelIds); start += batchDeleteChannelsChunkSize {
		end := start + batchDeleteChannelsChunkSize
		if end > len(channelIds) {
			end = len(channelIds)
		}
		chunk := channelIds[start:end]
		if err := DB.Transaction(func(tx *gorm.DB) error {
			// 复用筛选条件二次确认：Pluck 与删除非同事务，渠道状态可能在间隙内变化，
			// 被重新启用的渠道不应在本次清理中删除（其 abilities 一并保留）
			var chunkIds []int
			if err := applyWhere(tx).Model(&Channel{}).Where("id IN ?", chunk).Pluck("id", &chunkIds).Error; err != nil {
				return err
			}
			if len(chunkIds) == 0 {
				return nil
			}
			// DeleteAbilities 的泛化形式：按 channel_id 集合清理
			if err := tx.Where("channel_id IN ?", chunkIds).Delete(&Ability{}).Error; err != nil {
				return err
			}
			result := tx.Where("id IN ?", chunkIds).Delete(&Channel{})
			affected += result.RowsAffected
			return result.Error
		}); err != nil {
			return affected, err
		}
	}
	return affected, nil
}
