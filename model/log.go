package model

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
)

type Log struct {
	Id                int    `json:"id"`
	UserId            int    `json:"user_id" gorm:"index"`
	CreatedAt         int64  `json:"created_at" gorm:"bigint;index:idx_created_at_type"`
	Type              int    `json:"type" gorm:"index:idx_created_at_type"`
	Content           string `json:"content"`
	Username          string `json:"username" gorm:"index:index_username_model_name,priority:2;default:''"`
	TokenName         string `json:"token_name" gorm:"index;default:''"`
	ModelName         string `json:"model_name" gorm:"index;index:index_username_model_name,priority:1;default:''"`
	Quota             int    `json:"quota" gorm:"default:0"`
	PromptTokens      int    `json:"prompt_tokens" gorm:"default:0"`
	CompletionTokens  int    `json:"completion_tokens" gorm:"default:0"`
	CachedTokens      int    `json:"cached_tokens" gorm:"default:0"` // 缓存命中的token数
	ChannelId         int    `json:"channel" gorm:"index"`
	RequestId         string `json:"request_id" gorm:"default:''"`
	ElapsedTime       int64  `json:"elapsed_time" gorm:"default:0"` // unit is ms
	IsStream          bool   `json:"is_stream" gorm:"default:false"`
	SystemPromptReset bool   `json:"system_prompt_reset" gorm:"default:false"`
	ChannelName       string `json:"channel_name" gorm:"default:''"`
	RequestBody   string `json:"request_body" gorm:"type:text"`
	ResponseBody  string `json:"response_body" gorm:"type:text"`
	RequestHeader string `json:"request_header" gorm:"type:text"`
}

// LogListItem 用于日志列表查询，包含三个 bool 字段用于标识大字段是否有内容
// 这个结构体不会参与数据库迁移，只用于查询结果的映射
type LogListItem struct {
	Id                int    `json:"id"`
	UserId            int    `json:"user_id"`
	CreatedAt         int64  `json:"created_at"`
	Type              int    `json:"type"`
	Content           string `json:"content"`
	Username          string `json:"username"`
	TokenName         string `json:"token_name"`
	ModelName         string `json:"model_name"`
	Quota             int    `json:"quota"`
	PromptTokens      int    `json:"prompt_tokens"`
	CompletionTokens  int    `json:"completion_tokens"`
	CachedTokens      int    `json:"cached_tokens"`
	ChannelId         int    `json:"channel"`
	RequestId         string `json:"request_id"`
	ElapsedTime       int64  `json:"elapsed_time"`
	IsStream          bool   `json:"is_stream"`
	SystemPromptReset bool   `json:"system_prompt_reset"`
	ChannelName       string `json:"channel_name"`
	HasRequestBody    bool   `json:"has_request_body"`
	HasResponseBody   bool   `json:"has_response_body"`
	HasRequestHeader  bool   `json:"has_request_header"`
}

// TableName 指定 LogListItem 查询时使用的表名
func (LogListItem) TableName() string {
	return "logs"
}

const (
	LogTypeUnknown = iota
	// LogTypeTopup 充值/额度变更记录。原充值功能已移除，保留该常量以保持数据库旧数据一致。
	LogTypeTopup
	LogTypeConsume
	LogTypeManage
	LogTypeSystem
	LogTypeTest
)

func recordLogHelper(ctx context.Context, log *Log) {
	requestId := helper.GetRequestID(ctx)
	log.RequestId = requestId
	err := LOG_DB.Create(log).Error
	if err != nil {
		logger.Log.Errorf("failed to record log: " + err.Error())
		return
	}
	logger.Log.Infof("record log userId:%v, userName:%v, channelName:%v, modelName:%v, isStream:%v", log.UserId, log.Username, log.ChannelName, log.ModelName, log.IsStream)
}

func RecordLog(ctx context.Context, userId int, logType int, content string) {
	log := &Log{
		UserId:    userId,
		Username:  GetUsernameById(userId),
		CreatedAt: helper.GetTimestamp(),
		Type:      logType,
		Content:   content,
	}
	recordLogHelper(ctx, log)
}

func RecordConsumeLog(ctx context.Context, log *Log) {
	log.Username = GetUsernameById(log.UserId)
	log.CreatedAt = helper.GetTimestamp()
	log.Type = LogTypeConsume
	recordLogHelper(ctx, log)
}

func RecordTestLog(ctx context.Context, log *Log) {
	log.CreatedAt = helper.GetTimestamp()
	log.Type = LogTypeTest
	recordLogHelper(ctx, log)
}

func buildAllLogsQuery(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, channel int) *gorm.DB {
	var tx *gorm.DB
	if logType == LogTypeUnknown {
		tx = LOG_DB
	} else {
		tx = LOG_DB.Where("type = ?", logType)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	if channel != 0 {
		tx = tx.Where("channel_id = ?", channel)
	}
	return tx
}

func GetAllLogs(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, startIdx int, num int, channel int) (logs []*LogListItem, err error) {
	tx := buildAllLogsQuery(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel)
	err = tx.Select(
		"id, user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, is_stream, system_prompt_reset, channel_name, " +
			"request_body != '' as has_request_body, response_body != '' as has_response_body, request_header != '' as has_request_header",
	).Order("id desc").Limit(num).Offset(startIdx).Find(&logs).Error
	return logs, err
}

func GetAllLogsCount(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, channel int) (total int64, err error) {
	tx := buildAllLogsQuery(logType, startTimestamp, endTimestamp, modelName, username, tokenName, channel)
	err = tx.Model(&Log{}).Count(&total).Error
	return total, err
}

func buildUserLogsQuery(userId int, logType int, startTimestamp int64, endTimestamp int64, modelName string, tokenName string) *gorm.DB {
	var tx *gorm.DB
	if logType == LogTypeUnknown {
		tx = LOG_DB.Where("user_id = ?", userId)
	} else {
		tx = LOG_DB.Where("user_id = ? and type = ?", userId, logType)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	return tx
}

func GetUserLogs(userId int, logType int, startTimestamp int64, endTimestamp int64, modelName string, tokenName string, startIdx int, num int) (logs []*LogListItem, err error) {
	tx := buildUserLogsQuery(userId, logType, startTimestamp, endTimestamp, modelName, tokenName)
	err = tx.Select(
		"user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, is_stream, system_prompt_reset, channel_name, " +
			"request_body != '' as has_request_body, response_body != '' as has_response_body, request_header != '' as has_request_header",
	).Order("id desc").Limit(num).Offset(startIdx).Find(&logs).Error
	return logs, err
}

func GetUserLogsCount(userId int, logType int, startTimestamp int64, endTimestamp int64, modelName string, tokenName string) (total int64, err error) {
	tx := buildUserLogsQuery(userId, logType, startTimestamp, endTimestamp, modelName, tokenName)
	err = tx.Model(&Log{}).Count(&total).Error
	return total, err
}

func SearchAllLogs(keyword string) (logs []*LogListItem, err error) {
	err = LOG_DB.Where("type = ? or content LIKE ?", keyword, keyword+"%").
		Select(
			"id, user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, is_stream, system_prompt_reset, channel_name, "+
				"request_body != '' as has_request_body, response_body != '' as has_response_body, request_header != '' as has_request_header",
		).
		Order("id desc").Limit(config.MaxRecentItems).Find(&logs).Error
	return logs, err
}

func SearchUserLogs(userId int, keyword string) (logs []*LogListItem, err error) {
	err = LOG_DB.Where("user_id = ? and type = ?", userId, keyword).
		Select(
			"user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, is_stream, system_prompt_reset, channel_name, "+
				"request_body != '' as has_request_body, response_body != '' as has_response_body, request_header != '' as has_request_header",
		).
		Order("id desc").Limit(config.MaxRecentItems).Find(&logs).Error
	return logs, err
}

func GetLogById(id int) (*Log, error) {
	var log Log
	err := LOG_DB.Where("id = ?", id).First(&log).Error
	return &log, err
}

func SumUsedQuota(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string, channel int) (quota int64) {
	ifnull := "ifnull"
	if common.UsingPostgreSQL {
		ifnull = "COALESCE"
	}
	db := LOG_DB
	if db == nil {
		db = DB
	}
	tx := db.Table("logs").Select(fmt.Sprintf("%s(sum(quota),0)", ifnull))
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	if channel != 0 {
		tx = tx.Where("channel_id = ?", channel)
	}
	tx.Where("type = ?", LogTypeConsume).Scan(&quota)
	return quota
}

// SumUserQuotaTotal sums the total quota consumed by a user from logs.
func SumUserQuotaTotal(userId int) (token int) {
	ifnull := "ifnull"
	if common.UsingPostgreSQL {
		ifnull = "COALESCE"
	}
	db := LOG_DB
	if db == nil {
		db = DB
	}
	db.Table("logs").Select(fmt.Sprintf("%s(sum(quota),0)", ifnull)).
		Where("type = ? and user_id = ?", LogTypeConsume, userId).
		Scan(&token)
	return token
}

func SumUsedToken(logType int, startTimestamp int64, endTimestamp int64, modelName string, username string, tokenName string) (token int) {
	ifnull := "ifnull"
	if common.UsingPostgreSQL {
		ifnull = "COALESCE"
	}
	db := LOG_DB
	if db == nil {
		db = DB
	}
	tx := db.Table("logs").Select(fmt.Sprintf("%s(sum(prompt_tokens),0) + %s(sum(completion_tokens),0)", ifnull, ifnull))
	if username != "" {
		tx = tx.Where("username = ?", username)
	}
	if tokenName != "" {
		tx = tx.Where("token_name = ?", tokenName)
	}
	if startTimestamp != 0 {
		tx = tx.Where("created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("created_at <= ?", endTimestamp)
	}
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	tx.Where("type = ?", LogTypeConsume).Scan(&token)
	return token
}

func DeleteOldLog(targetTimestamp int64) (int64, error) {
	result := LOG_DB.Where("created_at < ?", targetTimestamp).Delete(&Log{})
	return result.RowsAffected, result.Error
}

func ClearOldLogBodies(targetTimestamp int64) (int64, error) {
	startTime := targetTimestamp - int64(time.Hour.Seconds()*2)
	result := LOG_DB.Model(&Log{}).Where("created_at < ? and created_at > ?", targetTimestamp, startTime).Updates(map[string]interface{}{
		"request_body":   "",
		"response_body":  "",
		"request_header": "",
	})
	return result.RowsAffected, result.Error
}

type LogStatistic struct {
	Day              string `gorm:"column:day"`
	ModelName        string `gorm:"column:model_name"`
	RequestCount     int    `gorm:"column:request_count"`
	Quota            int    `gorm:"column:quota"`
	PromptTokens     int    `gorm:"column:prompt_tokens"`
	CompletionTokens int    `gorm:"column:completion_tokens"`
}

func SearchLogsByDayAndModel(userId, start, end int, username string) (LogStatistics []*LogStatistic, err error) {
	groupSelect := "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d') as day"

	if common.UsingPostgreSQL {
		groupSelect = "TO_CHAR(date_trunc('day', to_timestamp(created_at)), 'YYYY-MM-DD') as day"
	}

	if common.UsingSQLite {
		groupSelect = "strftime('%Y-%m-%d', datetime(created_at, 'unixepoch')) as day"
	}

	query := `
		SELECT ` + groupSelect + `,
		model_name, count(1) as request_count,
		sum(quota) as quota,
		sum(prompt_tokens) as prompt_tokens,
		sum(completion_tokens) as completion_tokens
		FROM logs
		WHERE type=2`
	var args []interface{}
	if userId > 0 {
		query += " AND user_id = ?"
		args = append(args, userId)
	}
	if username != "" {
		query += " AND username = ?"
		args = append(args, username)
	}
	query += ` AND created_at BETWEEN ? AND ?
		GROUP BY day, model_name
		ORDER BY day, model_name`
	args = append(args, start, end)

	err = LOG_DB.Raw(query, args...).Scan(&LogStatistics).Error

	return LogStatistics, err
}
