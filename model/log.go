package model

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	ElapsedTime       int64  `json:"elapsed_time" gorm:"default:0"`      // unit is ms
	FirstTokenTime    int64  `json:"first_token_time" gorm:"default:0"` // 首字耗时（TTFT），unit is ms；仅流式请求有值，非流式为 0
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
	FirstTokenTime    int64  `json:"first_token_time"`
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
		"id, user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, first_token_time, is_stream, system_prompt_reset, channel_name, " +
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
		"user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, first_token_time, is_stream, system_prompt_reset, channel_name, " +
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
	tx := LOG_DB
	// keyword 为数字时才与整数 type 比较：字符串直接比较在 PostgreSQL 下报错、
	// MySQL 下隐式转型为 0 语义错误；非数字时仅按 content 前缀过滤
	if typeInt, convErr := strconv.Atoi(keyword); convErr == nil {
		tx = tx.Where("type = ? or content LIKE ?", typeInt, keyword+"%")
	} else {
		tx = tx.Where("content LIKE ?", keyword+"%")
	}
	err = tx.Select(
		"id, user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, first_token_time, is_stream, system_prompt_reset, channel_name, "+
			"request_body != '' as has_request_body, response_body != '' as has_response_body, request_header != '' as has_request_header",
	).
		Order("id desc").Limit(config.MaxRecentItems).Find(&logs).Error
	return logs, err
}

func SearchUserLogs(userId int, keyword string) (logs []*LogListItem, err error) {
	// 修正原先把 keyword 当整数 type 比较的复制粘贴错误：按 content 前缀搜索本用户日志
	err = LOG_DB.Where("user_id = ? and content LIKE ?", userId, keyword+"%").
		Select(
			"user_id, created_at, type, content, username, token_name, model_name, quota, prompt_tokens, completion_tokens, cached_tokens, channel_id, request_id, elapsed_time, first_token_time, is_stream, system_prompt_reset, channel_name, "+
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
	if err != nil {
		return LogStatistics, err
	}

	// 统计展示归一化：模型名统一小写，并按等价组主名归并；
	// 元数据加载失败时降级为仅小写化（空映射），不得让 dashboard 报错
	alias := map[string]string{}
	metadataList, metaErr := GetAllModelMetadata()
	if metaErr != nil {
		logger.Log.Warnf("failed to load model metadata for dashboard statistics, fallback to lowercase only: %v", metaErr)
	} else {
		alias = buildCanonicalDisplayAlias(metadataList)
	}
	LogStatistics = normalizeLogStatistics(LogStatistics, alias)

	return LogStatistics, nil
}

// normalizeLogStatistics 对统计行做展示归一化并按 (Day, ModelName) 合并数值字段：
// 模型名先剥离 "/" 及其前的厂商前缀只取末段（展示口径与路由侧取末段一致，厂商前缀
// 不参与统计维度），再统一小写；命中 alias（key=成员模型名的简化名，value=等价主名
// 展示名）时归并到主名。纯函数便于无 DB 单测；alias 只做一级映射，与路由侧归并行为
// 一致，不递归解析主名。
func normalizeLogStatistics(stats []*LogStatistic, alias map[string]string) []*LogStatistic {
	type logStatKey struct {
		day  string
		name string
	}
	merged := make(map[logStatKey]*LogStatistic)
	for _, stat := range stats {
		// 防御 nil 元素：纯函数可能被其他调用方复用，避免空指针 panic
		if stat == nil {
			continue
		}
		// 剥离厂商前缀只保留末段；不用 SimplifyModelName 做展示名，它会把连字符等
		// 展示字符也去掉
		display := stat.ModelName
		if idx := strings.LastIndex(display, "/"); idx >= 0 {
			display = display[idx+1:]
		}
		if display == "" {
			// 病理输入（如 "vendor/"）剥离后为空，回退原名小写，避免产生空模型名行
			display = stat.ModelName
		}
		name := strings.ToLower(display)
		// 等价匹配按简化名（去分隔符）查表，与路由侧 canonicalAliasMap 的 key 口径一致；
		// SimplifyModelName 本就只取末段，前缀剥离不影响该匹配
		if canonical, ok := alias[SimplifyModelName(name)]; ok {
			name = canonical
		}
		key := logStatKey{day: stat.Day, name: name}
		if exist, ok := merged[key]; ok {
			exist.RequestCount += stat.RequestCount
			exist.Quota += stat.Quota
			exist.PromptTokens += stat.PromptTokens
			exist.CompletionTokens += stat.CompletionTokens
			continue
		}
		merged[key] = &LogStatistic{
			Day:              stat.Day,
			ModelName:        name,
			RequestCount:     stat.RequestCount,
			Quota:            stat.Quota,
			PromptTokens:     stat.PromptTokens,
			CompletionTokens: stat.CompletionTokens,
		}
	}

	result := make([]*LogStatistic, 0, len(merged))
	for _, stat := range merged {
		result = append(result, stat)
	}
	// 归并打乱了 SQL 的 ORDER BY 结果，这里恢复原输出顺序语义：按天升序、同天按模型名升序
	sort.Slice(result, func(i, j int) bool {
		if result[i].Day != result[j].Day {
			return result[i].Day < result[j].Day
		}
		return result[i].ModelName < result[j].ModelName
	})
	return result
}

// buildCanonicalDisplayAlias 从模型元数据构建统计展示用的等价映射：
// key=成员模型名的简化名，value=小写等价主名。路由侧全局 canonicalAliasMap 的值是
// SimplifyModelName 的去符号形式，不适合做展示名，故单独构建而非复用。
func buildCanonicalDisplayAlias(metadataList []*ModelMetadata) map[string]string {
	alias := make(map[string]string)
	for _, metadata := range metadataList {
		if metadata == nil || metadata.CanonicalName == "" {
			continue
		}
		alias[SimplifyModelName(metadata.Name)] = strings.ToLower(metadata.CanonicalName)
	}
	return alias
}

// dashboardBucketExpression 把白名单粒度解析为当前数据库方言下可信的时间分桶表达式。
// 仅接受 hour/day/week：hour 产出 `YYYY-MM-DD HH:00`，day/week 产出 `YYYY-MM-DD`，
// 其中 week 统一取该周周一。调用方输入只用于查表、绝不拼接进 SQL，未知粒度直接报错。
// 未识别方言时与 SearchLogsByDayAndModel 保持一致，回退 MySQL 口径。
func dashboardBucketExpression(granularity string) (string, error) {
	switch granularity {
	case "hour", "day", "week":
	default:
		return "", fmt.Errorf("unsupported dashboard granularity: %q", granularity)
	}

	// PostgreSQL：date_trunc('week') 本身以周一为起点，满足 week 口径
	if common.UsingPostgreSQL {
		switch granularity {
		case "hour":
			return "TO_CHAR(date_trunc('hour', to_timestamp(created_at)), 'YYYY-MM-DD HH24:00')", nil
		case "day":
			return "TO_CHAR(date_trunc('day', to_timestamp(created_at)), 'YYYY-MM-DD')", nil
		default:
			return "TO_CHAR(date_trunc('week', to_timestamp(created_at)), 'YYYY-MM-DD')", nil
		}
	}

	// SQLite：strftime('%w') 中 0=周日，用 (w+6)%7 回退到本周周一
	if common.UsingSQLite {
		switch granularity {
		case "hour":
			return "strftime('%Y-%m-%d %H:00', datetime(created_at, 'unixepoch'))", nil
		case "day":
			return "strftime('%Y-%m-%d', datetime(created_at, 'unixepoch'))", nil
		default:
			return "strftime('%Y-%m-%d', datetime(created_at, 'unixepoch', '-' || ((strftime('%w', datetime(created_at, 'unixepoch')) + 6) % 7) || ' days'))", nil
		}
	}

	// MySQL（默认）：WEEKDAY() 中 0=周一，回退 WEEKDAY() 天即本周周一
	switch granularity {
	case "hour":
		return "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d %H:00')", nil
	case "day":
		return "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d')", nil
	default:
		return "DATE_FORMAT(DATE_SUB(FROM_UNIXTIME(created_at), INTERVAL WEEKDAY(FROM_UNIXTIME(created_at)) DAY), '%Y-%m-%d')", nil
	}
}

// dashboardDimensionColumn 把白名单维度解析为可信的 logs 列名。
// 仅接受 model/channel/token/user 四个闭集取值，未知或注入形状输入一律报错，
// 调用方据此在访问数据库前拒绝非法维度。
func dashboardDimensionColumn(dimension string) (string, error) {
	switch dimension {
	case "model":
		return "model_name", nil
	case "channel":
		return "channel_name", nil
	case "token":
		return "token_name", nil
	case "user":
		return "username", nil
	default:
		return "", fmt.Errorf("unsupported dashboard dimension: %q", dimension)
	}
}

// IsValidDashboardDimension 报告 dimension 是否落在 dashboard 维度白名单闭集内。
// 供 HTTP 边界在触库前拒绝非法维度，白名单仍以 dashboardDimensionColumn 为唯一权威来源，
// 调用方无需（也不得）自行复制映射表。
func IsValidDashboardDimension(dimension string) bool {
	_, err := dashboardDimensionColumn(dimension)
	return err == nil
}

// IsValidDashboardGranularity 报告 granularity 是否落在 dashboard 时间粒度白名单闭集内。
// 供 HTTP 边界在触库前拒绝非法粒度，白名单仍以 dashboardBucketExpression 为唯一权威来源，
// 调用方无需（也不得）自行复制映射表。
func IsValidDashboardGranularity(granularity string) bool {
	_, err := dashboardBucketExpression(granularity)
	return err == nil
}

// DashboardAggregate 是 dashboard 单个 (bucket, key) 组合的聚合指标行。
// AvgTTFT/AvgElapsed 单位为毫秒；TTFTTotal/TTFTSamples/ElapsedTotal/ElapsedSamples
// 为内部样本字段，不参与 JSON 序列化，仅用于后续模型别名归并时保持正样本加权平均。
type DashboardAggregate struct {
	Bucket           string  `json:"bucket" gorm:"column:bucket"`
	Key              string  `json:"key" gorm:"column:key"`
	Requests         int64   `json:"requests" gorm:"column:requests"`
	Quota            int64   `json:"quota" gorm:"column:quota"`
	PromptTokens     int64   `json:"prompt_tokens" gorm:"column:prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens" gorm:"column:completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens" gorm:"column:cached_tokens"`
	AvgTTFT          float64 `json:"avg_ttft" gorm:"column:avg_ttft"`
	AvgElapsed       float64 `json:"avg_elapsed" gorm:"column:avg_elapsed"`
	TTFTTotal        int64   `json:"-" gorm:"column:ttft_total"`
	TTFTSamples      int64   `json:"-" gorm:"column:ttft_samples"`
	ElapsedTotal     int64   `json:"-" gorm:"column:elapsed_total"`
	ElapsedSamples   int64   `json:"-" gorm:"column:elapsed_samples"`
}

// SearchDashboardAggregates 按 (bucket, key) 分组加载消费日志的扁平聚合行：
// 仅统计 type=LogTypeConsume，created_at 取闭区间 [startTimestamp, endTimestamp]，
// 输出按 bucket 升序、同 bucket 内 key 升序，保证结果确定性。userID > 0 时按 user_id
// 限定，username != "" 时追加精确 username 限定。时间分桶表达式与维度列名均来自白名单，
// 非法值在访问数据库前直接报错；用户输入只作为参数绑定，绝不拼接进 SQL。
// avg_ttft/avg_elapsed 仅对正值样本求平均，并同时输出内部样本字段，
// 供后续模型别名归并时保持正样本加权平均。
// dimension=model 时复用 normalizeDashboardAggregates（其权威归一化来源为
// normalizeLogStatistics），使同一模型的大小写/厂商前缀/等价变体合并为单行、指标求和正确；
// channel/token/user 维度的 key 逐字保留，大小写不同即独立行，绝不经过归一化。
func SearchDashboardAggregates(userID int, startTimestamp, endTimestamp int64, username, dimension, granularity string) ([]*DashboardAggregate, error) {
	bucketExpression, err := dashboardBucketExpression(granularity)
	if err != nil {
		return nil, err
	}
	dimensionColumn, err := dashboardDimensionColumn(dimension)
	if err != nil {
		return nil, err
	}

	// key 是 MySQL 保留字：按方言引用别名，保证 SELECT/GROUP BY/ORDER BY 在三种数据库
	// 均可解析。未识别方言时与 dashboardBucketExpression 保持一致，回退 MySQL 口径。
	keyColumn := "`key`"
	if common.UsingPostgreSQL || common.UsingSQLite {
		keyColumn = `"key"`
	}

	// MySQL 默认 collation（如 utf8mb4_general_ci）大小写不敏感，直接 GROUP BY 列名会把仅
	// 大小写不同的 channel/token/user 键在 SQL 层错误合并，应用层无法恢复；故 MySQL 方言
	// （含未识别方言的回退分支）用 BINARY 强制字节级比较，字符集无关且比 COLLATE 更稳健。
	// PostgreSQL/SQLite 默认大小写敏感，保持原列名。SELECT/GROUP BY/ORDER BY 必须使用同一
	// 表达式，以兼容 MySQL 5.7+ 默认开启的 ONLY_FULL_GROUP_BY。model 维度的展示归一化仍由
	// Go 侧 normalizeDashboardAggregates 负责，本表达式不改变其最终语义。
	keyExpression := dimensionColumn
	if !common.UsingPostgreSQL && !common.UsingSQLite {
		keyExpression = "BINARY " + dimensionColumn
	}

	query := `
		SELECT ` + bucketExpression + ` AS bucket,
		` + keyExpression + ` AS ` + keyColumn + `,
		count(1) AS requests,
		sum(quota) AS quota,
		sum(prompt_tokens) AS prompt_tokens,
		sum(completion_tokens) AS completion_tokens,
		sum(cached_tokens) AS cached_tokens,
		avg(CASE WHEN first_token_time > 0 THEN first_token_time END) AS avg_ttft,
		avg(CASE WHEN elapsed_time > 0 THEN elapsed_time END) AS avg_elapsed,
		sum(CASE WHEN first_token_time > 0 THEN first_token_time ELSE 0 END) AS ttft_total,
		sum(CASE WHEN first_token_time > 0 THEN 1 ELSE 0 END) AS ttft_samples,
		sum(CASE WHEN elapsed_time > 0 THEN elapsed_time ELSE 0 END) AS elapsed_total,
		sum(CASE WHEN elapsed_time > 0 THEN 1 ELSE 0 END) AS elapsed_samples
		FROM logs
		WHERE type = ?`

	args := []interface{}{LogTypeConsume}
	if userID > 0 {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	if username != "" {
		query += " AND username = ?"
		args = append(args, username)
	}
	query += ` AND created_at >= ? AND created_at <= ?
		GROUP BY bucket, ` + keyExpression + `
		ORDER BY bucket, ` + keyExpression
	args = append(args, startTimestamp, endTimestamp)

	rows := make([]*DashboardAggregate, 0)
	if err := LOG_DB.Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	// 仅 model 维度做展示归一化：channel/token/user 的 key 必须逐字保留（大小写不同即独立行），
	// 绝不经过归一化。alias 加载与 SearchLogsByDayAndModel 完全对称：元数据加载失败时降级为
	// 空映射（仅剥离厂商前缀 + 小写化），不得让请求失败。
	if dimension == "model" {
		alias := map[string]string{}
		metadataList, metaErr := GetAllModelMetadata()
		if metaErr != nil {
			logger.Log.Warnf("failed to load model metadata for dashboard aggregates, fallback to lowercase only: %v", metaErr)
		} else {
			alias = buildCanonicalDisplayAlias(metadataList)
		}
		rows = normalizeDashboardAggregates(rows, alias)
	}
	return rows, nil
}

// normalizeDashboardAggregates 在模型维度上复用 normalizeLogStatistics 作为模型名归一化的
// 唯一权威来源（厂商前缀剥离、小写、别名归并、元数据加载失败降级），并按 (bucket, 归一化后 key)
// 合并聚合行：Requests/Quota/PromptTokens/CompletionTokens/CachedTokens 直接求和；
// avg_ttft/avg_elapsed 依据内部样本字段做正样本加权平均（TTFTTotal/TTFTSamples、
// ElapsedTotal/ElapsedSamples），样本数为 0 时结果为 0，避免对各行均值再做简单平均。
// 输出按 bucket 升序、同 bucket 内 key 升序，保持与聚合查询一致的确定性顺序。
//
// 约束：本函数只负责模型维度归一化，调用方必须仅在 dimension=model 时调用；channel/token/user
// 维度的 key 必须原样保留、大小写不同即保持独立行，不得经过本函数。
func normalizeDashboardAggregates(stats []*DashboardAggregate, alias map[string]string) []*DashboardAggregate {
	type aggregateKey struct {
		bucket string
		key    string
	}

	// 归一化结果只取决于原始 key（与 bucket 无关），按 key 缓存以避免重复归一化开销
	normalizedKeys := make(map[string]string, len(stats))
	resolveKey := func(key string) string {
		if resolved, ok := normalizedKeys[key]; ok {
			return resolved
		}
		resolved := key
		// 单行输入下 normalizeLogStatistics 只做厂商前缀剥离/小写/别名归并，不引入跨行合并语义
		if rows := normalizeLogStatistics([]*LogStatistic{{ModelName: key}}, alias); len(rows) > 0 {
			resolved = rows[0].ModelName
		}
		normalizedKeys[key] = resolved
		return resolved
	}

	merged := make(map[aggregateKey]*DashboardAggregate)
	for _, stat := range stats {
		// 防御 nil 元素：保持与 normalizeLogStatistics 一致的容错语义
		if stat == nil {
			continue
		}
		key := aggregateKey{bucket: stat.Bucket, key: resolveKey(stat.Key)}
		exist, ok := merged[key]
		if !ok {
			exist = &DashboardAggregate{Bucket: stat.Bucket, Key: key.key}
			merged[key] = exist
		}
		exist.Requests += stat.Requests
		exist.Quota += stat.Quota
		exist.PromptTokens += stat.PromptTokens
		exist.CompletionTokens += stat.CompletionTokens
		exist.CachedTokens += stat.CachedTokens
		exist.TTFTTotal += stat.TTFTTotal
		exist.TTFTSamples += stat.TTFTSamples
		exist.ElapsedTotal += stat.ElapsedTotal
		exist.ElapsedSamples += stat.ElapsedSamples
	}

	result := make([]*DashboardAggregate, 0, len(merged))
	for _, stat := range merged {
		if stat.TTFTSamples > 0 {
			stat.AvgTTFT = float64(stat.TTFTTotal) / float64(stat.TTFTSamples)
		}
		if stat.ElapsedSamples > 0 {
			stat.AvgElapsed = float64(stat.ElapsedTotal) / float64(stat.ElapsedSamples)
		}
		result = append(result, stat)
	}
	// 归并打乱了输入顺序，恢复与聚合查询一致的确定性顺序：bucket 升序、同 bucket 内 key 升序
	sort.Slice(result, func(i, j int) bool {
		if result[i].Bucket != result[j].Bucket {
			return result[i].Bucket < result[j].Bucket
		}
		return result[i].Key < result[j].Key
	})
	return result
}

// DashboardSummary 是一个请求时间范围与权限作用域下的 dashboard KPI 汇总。
// CacheHitRate/AvgRPM/AvgTPM 由查询结果与时间窗口推导，不映射数据库列；
// PromptTokens 仅作为命中率分母的内部字段，不参与 JSON 序列化。
// AvgTTFT/AvgElapsed 使用指针以区分「无正样本」（nil → JSON null，前端渲染为 --）
// 与「真实 0ms」：SQL 侧不做 COALESCE，聚合结果为 NULL 时由 GORM 扫描为 nil。
type DashboardSummary struct {
	TotalRequests int64    `json:"total_requests" gorm:"column:total_requests"`
	TotalQuota    int64    `json:"total_quota" gorm:"column:total_quota"`
	TotalTokens   int64    `json:"total_tokens" gorm:"column:total_tokens"`
	CachedTokens  int64    `json:"cached_tokens" gorm:"column:cached_tokens"`
	CacheHitRate  float64  `json:"cache_hit_rate" gorm:"-"`
	AvgTTFT       *float64 `json:"avg_ttft" gorm:"column:avg_ttft"`
	AvgElapsed    *float64 `json:"avg_elapsed" gorm:"column:avg_elapsed"`
	AvgRPM        float64  `json:"avg_rpm" gorm:"-"`
	AvgTPM        float64  `json:"avg_tpm" gorm:"-"`
	PromptTokens  int64    `json:"-" gorm:"column:prompt_tokens"`
}

// SearchDashboardSummary 加载指定闭区间内消费日志的 KPI 汇总：
// 仅统计 type=LogTypeConsume，created_at 取闭区间 [startTimestamp, endTimestamp]。
// userID > 0 时按 user_id 限定，username != "" 时追加精确 username 限定。
// total_tokens 为 prompt_tokens + completion_tokens（缓存命中的 token 不重复计入）；
// cache_hit_rate 以 prompt_tokens 为分母、cached_tokens 为分子，分母为 0 时返回 0，不产生 NaN。
// avg_ttft/avg_elapsed 仅对 > 0 的行求均值；无正样本时 SQL 聚合为 NULL，
// 经指针字段扫描为 nil 并以 JSON null 返回，使前端可区分「无样本」与「真实 0ms」。
// avg_rpm/avg_tpm 按请求时间范围的分钟数计算；非正时长由 HTTP 边界拒绝，这里仍防御性归零。
// 用户输入只作为参数绑定，绝不拼接进 SQL。
func SearchDashboardSummary(userID int, startTimestamp, endTimestamp int64, username string) (*DashboardSummary, error) {
	// 聚合查询无 GROUP BY，恒返回一行；sum 在无匹配行时为 NULL，用 COALESCE 归零，
	// 避免 NULL 扫描失败并保证空区间返回零值汇总而非报错。avg_ttft/avg_elapsed 故意不
	// COALESCE：保留 NULL 以表达「无正样本」，由 *float64 承载。
	query := `
		SELECT
		count(1) AS total_requests,
		COALESCE(sum(quota), 0) AS total_quota,
		COALESCE(sum(prompt_tokens) + sum(completion_tokens), 0) AS total_tokens,
		COALESCE(sum(cached_tokens), 0) AS cached_tokens,
		avg(CASE WHEN first_token_time > 0 THEN first_token_time END) AS avg_ttft,
		avg(CASE WHEN elapsed_time > 0 THEN elapsed_time END) AS avg_elapsed,
		COALESCE(sum(prompt_tokens), 0) AS prompt_tokens
		FROM logs
		WHERE type = ?`

	args := []interface{}{LogTypeConsume}
	if userID > 0 {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	if username != "" {
		query += " AND username = ?"
		args = append(args, username)
	}
	query += " AND created_at >= ? AND created_at <= ?"
	args = append(args, startTimestamp, endTimestamp)

	summary := &DashboardSummary{}
	if err := LOG_DB.Raw(query, args...).Scan(summary).Error; err != nil {
		return nil, err
	}

	// 分母为 0 时保持 0，避免 NaN/除零
	if summary.PromptTokens > 0 {
		summary.CacheHitRate = float64(summary.CachedTokens) / float64(summary.PromptTokens)
	}

	// 分钟数由请求时间范围换算；非正时长不产生速率（HTTP 边界已拦截，此处防御除零）
	if minutes := float64(endTimestamp-startTimestamp) / 60.0; minutes > 0 {
		summary.AvgRPM = float64(summary.TotalRequests) / minutes
		summary.AvgTPM = float64(summary.TotalTokens) / minutes
	}

	return summary, nil
}
