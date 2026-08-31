package model

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestNormalizeLogStatistics 覆盖统计归一化纯函数：大小写合并、等价组归并、
// 误合并防护、输出排序与空映射降级，均不依赖 DB。
func TestNormalizeLogStatistics(t *testing.T) {
	Convey("normalizeLogStatistics normalizes and merges statistics", t, func() {
		Convey("Case variants of the same model merge into one lowercase row", func() {
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "DeepSeek-v4-Pro", RequestCount: 2, Quota: 100, PromptTokens: 10, CompletionTokens: 20},
				{Day: "2024-07-03", ModelName: "deepseek-v4-pro", RequestCount: 1, Quota: 50, PromptTokens: 5, CompletionTokens: 7},
			}
			got := normalizeLogStatistics(stats, map[string]string{})
			So(len(got), ShouldEqual, 1)
			So(got[0].ModelName, ShouldEqual, "deepseek-v4-pro")
			So(got[0].RequestCount, ShouldEqual, 3)
			So(got[0].Quota, ShouldEqual, 150)
			So(got[0].PromptTokens, ShouldEqual, 15)
			So(got[0].CompletionTokens, ShouldEqual, 27)
		})

		Convey("Alias members merge into the canonical display name", func() {
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "DeepSeek-v4-Pro-0813", RequestCount: 1, Quota: 10, PromptTokens: 1, CompletionTokens: 2},
				{Day: "2024-07-03", ModelName: "DEEPSEEK-V4-PRO-0813", RequestCount: 1, Quota: 15, PromptTokens: 2, CompletionTokens: 3},
				{Day: "2024-07-03", ModelName: "deepseek-v4-pro", RequestCount: 2, Quota: 20, PromptTokens: 3, CompletionTokens: 4},
			}
			alias := map[string]string{"deepseekv4pro0813": "deepseek-v4-pro"}
			got := normalizeLogStatistics(stats, alias)
			So(len(got), ShouldEqual, 1)
			So(got[0].ModelName, ShouldEqual, "deepseek-v4-pro")
			So(got[0].RequestCount, ShouldEqual, 4)
			So(got[0].Quota, ShouldEqual, 45)
			So(got[0].PromptTokens, ShouldEqual, 6)
			So(got[0].CompletionTokens, ShouldEqual, 9)
		})

		Convey("Path prefixes are stripped so cross-vendor variants merge", func() {
			// 锁定路径前缀语义：展示名只取最后一段路径，"openai/deepseek-v4-pro" 与
			// "anthropic/deepseek-v4-pro" 无需 alias 配置即归并到同一展示名。展示口径
			// 与路由侧取末段一致（有意为之），此用例用于防止未来悄然变更。
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "openai/deepseek-v4-pro", RequestCount: 1, Quota: 10, PromptTokens: 1, CompletionTokens: 2},
				{Day: "2024-07-03", ModelName: "anthropic/deepseek-v4-pro", RequestCount: 2, Quota: 20, PromptTokens: 3, CompletionTokens: 4},
			}
			// 空 alias 即可归并，锁定"归并不依赖等价映射命中"的行为
			got := normalizeLogStatistics(stats, nil)
			So(len(got), ShouldEqual, 1)
			So(got[0].ModelName, ShouldEqual, "deepseek-v4-pro")
			So(got[0].RequestCount, ShouldEqual, 3)
			So(got[0].Quota, ShouldEqual, 30)
			So(got[0].PromptTokens, ShouldEqual, 4)
			So(got[0].CompletionTokens, ShouldEqual, 6)
		})

		Convey("Vendor prefix is stripped even without alias config", func() {
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "openai/gpt-4o", RequestCount: 1, Quota: 10, PromptTokens: 1, CompletionTokens: 2},
				{Day: "2024-07-03", ModelName: "GPT-4o", RequestCount: 2, Quota: 20, PromptTokens: 3, CompletionTokens: 4},
			}
			got := normalizeLogStatistics(stats, nil)
			So(len(got), ShouldEqual, 1)
			So(got[0].ModelName, ShouldEqual, "gpt-4o")
			So(got[0].RequestCount, ShouldEqual, 3)
			So(got[0].Quota, ShouldEqual, 30)
			So(got[0].PromptTokens, ShouldEqual, 4)
			So(got[0].CompletionTokens, ShouldEqual, 6)
		})

		Convey("Alias lookup applies after vendor prefix stripping", func() {
			// 锁定"先剥离前缀、再查等价映射"两段逻辑的组合行为：
			// "openai/DeepSeek-v4-Pro-0813" 剥离厂商前缀并小写后，简化名命中
			// 等价条目，最终归并到等价主名展示名。
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "openai/DeepSeek-v4-Pro-0813", RequestCount: 2, Quota: 30, PromptTokens: 4, CompletionTokens: 5},
			}
			alias := map[string]string{"deepseekv4pro0813": "deepseek-v4-pro"}
			got := normalizeLogStatistics(stats, alias)
			So(len(got), ShouldEqual, 1)
			So(got[0].ModelName, ShouldEqual, "deepseek-v4-pro")
			So(got[0].RequestCount, ShouldEqual, 2)
			So(got[0].Quota, ShouldEqual, 30)
			So(got[0].PromptTokens, ShouldEqual, 4)
			So(got[0].CompletionTokens, ShouldEqual, 5)
		})

		Convey("Distinct models are only lowercased and never falsely merged", func() {
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "GPT-4o", RequestCount: 1, Quota: 50},
				{Day: "2024-07-03", ModelName: "gpt-4o-mini", RequestCount: 2, Quota: 60},
			}
			// 非空映射下未命中的模型也不受影响
			alias := map[string]string{"deepseekv4pro0813": "deepseek-v4-pro"}
			got := normalizeLogStatistics(stats, alias)
			So(len(got), ShouldEqual, 2)
			So(got[0].ModelName, ShouldEqual, "gpt-4o")
			So(got[0].RequestCount, ShouldEqual, 1)
			So(got[0].Quota, ShouldEqual, 50)
			So(got[1].ModelName, ShouldEqual, "gpt-4o-mini")
			So(got[1].RequestCount, ShouldEqual, 2)
			So(got[1].Quota, ShouldEqual, 60)
		})

		Convey("Output is sorted by day then model name", func() {
			stats := []*LogStatistic{
				{Day: "2024-07-04", ModelName: "gpt-4o", RequestCount: 1},
				{Day: "2024-07-03", ModelName: "zeta-model", RequestCount: 1},
				{Day: "2024-07-03", ModelName: "Alpha-Model", RequestCount: 1},
			}
			got := normalizeLogStatistics(stats, map[string]string{})
			So(len(got), ShouldEqual, 3)
			So(got[0].Day, ShouldEqual, "2024-07-03")
			So(got[0].ModelName, ShouldEqual, "alpha-model")
			So(got[1].Day, ShouldEqual, "2024-07-03")
			So(got[1].ModelName, ShouldEqual, "zeta-model")
			So(got[2].Day, ShouldEqual, "2024-07-04")
			So(got[2].ModelName, ShouldEqual, "gpt-4o")
		})

		Convey("Empty alias map falls back to lowercase-only normalization", func() {
			stats := []*LogStatistic{
				{Day: "2024-07-03", ModelName: "DeepSeek-v4-Pro-0813", RequestCount: 1},
			}
			got := normalizeLogStatistics(stats, nil)
			So(len(got), ShouldEqual, 1)
			So(got[0].ModelName, ShouldEqual, "deepseek-v4-pro-0813")
		})
	})
}

// TestBuildCanonicalDisplayAlias 验证等价展示映射的构建规则：跳过无主名记录、
// key 用简化名、value 用小写主名（保留连字符等展示字符）。
func TestBuildCanonicalDisplayAlias(t *testing.T) {
	Convey("buildCanonicalDisplayAlias maps simplified member name to lowercase canonical name", t, func() {
		metadataList := []*ModelMetadata{
			{Name: "deepseek-v4-pro-0813", CanonicalName: "DeepSeek-V4-Pro"},
			{Name: "gpt-4o", CanonicalName: ""},
		}
		alias := buildCanonicalDisplayAlias(metadataList)
		So(len(alias), ShouldEqual, 1)
		So(alias["deepseekv4pro0813"], ShouldEqual, "deepseek-v4-pro")
	})
}

// TestSearchLogsByDayAndModelCanonicalMerge 端到端验证：内存 SQLite 种大小写混合的
// type=2 消费日志 + 一条带 CanonicalName 的元数据，SearchLogsByDayAndModel 返回归并结果。
func TestSearchLogsByDayAndModelCanonicalMerge(t *testing.T) {
	// 恢复全局 DB/LOG_DB 及被 initTestLogDB 改动的开关，避免污染包内其他测试
	origDB := DB
	origLogDB := LOG_DB
	origUsingSQLite := common.UsingSQLite
	origRedisEnabled := common.RedisEnabled
	defer func() {
		DB = origDB
		LOG_DB = origLogDB
		common.UsingSQLite = origUsingSQLite
		common.RedisEnabled = origRedisEnabled
	}()

	initTestLogDB(t)

	// 元数据走全局 DB，与日志的 LOG_DB 相互独立，需单独初始化
	dbPath := filepath.Join(t.TempDir(), "log_stat_test.db")
	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open metadata db: %v", err)
	}
	if err := DB.AutoMigrate(&ModelMetadata{}); err != nil {
		t.Fatalf("failed to migrate model metadata: %v", err)
	}

	day1 := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC).Unix()

	seed := []*Log{
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "DeepSeek-v4-Pro", Quota: 100, PromptTokens: 10, CompletionTokens: 20, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "deepseek-v4-pro", Quota: 150, PromptTokens: 11, CompletionTokens: 21, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "DeepSeek-v4-Pro-0813", Quota: 200, PromptTokens: 12, CompletionTokens: 22, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o", Quota: 50, PromptTokens: 5, CompletionTokens: 6, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o-mini", Quota: 60, PromptTokens: 7, CompletionTokens: 8, CreatedAt: day1},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "GPT-4O", Quota: 70, PromptTokens: 9, CompletionTokens: 10, CreatedAt: day2},
		// type=2 之外的日志不参与统计
		{UserId: 1, Username: "u1", Type: LogTypeManage, ModelName: "DeepSeek-v4-Pro", CreatedAt: day1},
	}
	for _, l := range seed {
		if err := LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}

	if err := DB.Create(&ModelMetadata{Name: "deepseek-v4-pro-0813", CanonicalName: "deepseek-v4-pro"}).Error; err != nil {
		t.Fatalf("failed to seed model metadata: %v", err)
	}

	Convey("SearchLogsByDayAndModel merges canonical groups end to end", t, func() {
		stats, err := SearchLogsByDayAndModel(1, int(day1)-86400, int(day2)+86400, "")
		So(err, ShouldBeNil)
		// 07-03: deepseek-v4-pro(3) / gpt-4o(1) / gpt-4o-mini(1)；07-04: gpt-4o(1)
		So(len(stats), ShouldEqual, 4)

		So(stats[0].Day, ShouldEqual, "2024-07-03")
		So(stats[0].ModelName, ShouldEqual, "deepseek-v4-pro")
		So(stats[0].RequestCount, ShouldEqual, 3)
		So(stats[0].Quota, ShouldEqual, 450)
		So(stats[0].PromptTokens, ShouldEqual, 33)
		So(stats[0].CompletionTokens, ShouldEqual, 63)

		So(stats[1].Day, ShouldEqual, "2024-07-03")
		So(stats[1].ModelName, ShouldEqual, "gpt-4o")
		So(stats[1].RequestCount, ShouldEqual, 1)
		So(stats[1].Quota, ShouldEqual, 50)

		So(stats[2].Day, ShouldEqual, "2024-07-03")
		So(stats[2].ModelName, ShouldEqual, "gpt-4o-mini")
		So(stats[2].RequestCount, ShouldEqual, 1)

		So(stats[3].Day, ShouldEqual, "2024-07-04")
		So(stats[3].ModelName, ShouldEqual, "gpt-4o")
		So(stats[3].RequestCount, ShouldEqual, 1)
		So(stats[3].Quota, ShouldEqual, 70)
	})
}
