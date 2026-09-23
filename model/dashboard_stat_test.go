package model

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

// withDialect 临时把三库开关切换为互斥的单一方言，并在用例结束后恢复，
// 避免污染包内其他依赖 common.UsingXxx 的测试。
func withDialect(mysql, postgres, sqlite bool, fn func()) {
	origMySQL := common.UsingMySQL
	origPostgres := common.UsingPostgreSQL
	origSQLite := common.UsingSQLite
	defer func() {
		common.UsingMySQL = origMySQL
		common.UsingPostgreSQL = origPostgres
		common.UsingSQLite = origSQLite
	}()

	common.UsingMySQL = mysql
	common.UsingPostgreSQL = postgres
	common.UsingSQLite = sqlite
	fn()
}

// initTestMetadataDB 用独立的临时 SQLite 初始化全局元数据库（模型 alias 的权威来源），
// 并返回恢复函数。SearchDashboardAggregates 在 model 维度会读取该库构建等价映射，
// 因此相关用例必须先初始化它，避免空 DB 指针 panic；空表使 alias 为空，
// 锁定"仅剥离厂商前缀 + 小写化"的降级基线。
func initTestMetadataDB(t *testing.T) func() {
	t.Helper()

	origDB := DB
	dbPath := filepath.Join(t.TempDir(), "dashboard_stat_metadata_test.db")
	var err error
	DB, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open metadata db: %v", err)
	}
	if err := DB.AutoMigrate(&ModelMetadata{}); err != nil {
		t.Fatalf("failed to migrate model metadata: %v", err)
	}

	return func() { DB = origDB }
}

// TestDashboardBucketExpression 覆盖三库 × 三粒度的时间分桶表达式：
// 表达式必须来自可信白名单（未知粒度报错），hour 输出 YYYY-MM-DD HH:00、
// day/week 输出 YYYY-MM-DD，且 week 在三种方言下都回退到该周周一。
func TestDashboardBucketExpression(t *testing.T) {
	Convey("dashboardBucketExpression resolves trusted per-dialect bucket expressions", t, func() {
		Convey("MySQL expressions use FROM_UNIXTIME and Monday-based week", func() {
			withDialect(true, false, false, func() {
				cases := []struct {
					granularity string
					want        string
				}{
					{"hour", "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d %H:00')"},
					{"day", "DATE_FORMAT(FROM_UNIXTIME(created_at), '%Y-%m-%d')"},
					{"week", "DATE_FORMAT(DATE_SUB(FROM_UNIXTIME(created_at), INTERVAL WEEKDAY(FROM_UNIXTIME(created_at)) DAY), '%Y-%m-%d')"},
				}
				for _, tc := range cases {
					got, err := dashboardBucketExpression(tc.granularity)
					So(err, ShouldBeNil)
					So(got, ShouldEqual, tc.want)
				}
			})
		})

		Convey("PostgreSQL expressions use date_trunc/to_timestamp with Monday-based week", func() {
			withDialect(false, true, false, func() {
				cases := []struct {
					granularity string
					want        string
				}{
					{"hour", "TO_CHAR(date_trunc('hour', to_timestamp(created_at)), 'YYYY-MM-DD HH24:00')"},
					{"day", "TO_CHAR(date_trunc('day', to_timestamp(created_at)), 'YYYY-MM-DD')"},
					{"week", "TO_CHAR(date_trunc('week', to_timestamp(created_at)), 'YYYY-MM-DD')"},
				}
				for _, tc := range cases {
					got, err := dashboardBucketExpression(tc.granularity)
					So(err, ShouldBeNil)
					So(got, ShouldEqual, tc.want)
				}
			})
		})

		Convey("SQLite expressions use strftime and Monday-based week", func() {
			withDialect(false, false, true, func() {
				cases := []struct {
					granularity string
					want        string
				}{
					{"hour", "strftime('%Y-%m-%d %H:00', datetime(created_at, 'unixepoch'))"},
					{"day", "strftime('%Y-%m-%d', datetime(created_at, 'unixepoch'))"},
					{"week", "strftime('%Y-%m-%d', datetime(created_at, 'unixepoch', '-' || ((strftime('%w', datetime(created_at, 'unixepoch')) + 6) % 7) || ' days'))"},
				}
				for _, tc := range cases {
					got, err := dashboardBucketExpression(tc.granularity)
					So(err, ShouldBeNil)
					So(got, ShouldEqual, tc.want)
				}
			})
		})

		Convey("Unsupported granularities are rejected", func() {
			withDialect(true, false, false, func() {
				for _, granularity := range []string{"", "minute", "month", "year", "HOUR", "hour; DROP TABLE logs", "day'--"} {
					_, err := dashboardBucketExpression(granularity)
					So(err, ShouldNotBeNil)
				}
			})
		})

		Convey("SQLite week bucket resolves to the containing Monday at runtime", func() {
			origLogDB := LOG_DB
			defer func() { LOG_DB = origLogDB }()

			withDialect(false, false, true, func() {
				initTestLogDB(t)

				// 2024-07-03 12:34:56 UTC 是周三，所在周周一为 2024-07-01
				wednesday := time.Date(2024, 7, 3, 12, 34, 56, 0, time.UTC).Unix()
				// 2024-07-07 是周日，其所在周周一同样回退到 2024-07-01
				sunday := time.Date(2024, 7, 7, 23, 59, 0, 0, time.UTC).Unix()
				seed := []*Log{
					{UserId: 1, Username: "u1", Type: LogTypeConsume, CreatedAt: wednesday},
					{UserId: 1, Username: "u1", Type: LogTypeConsume, CreatedAt: sunday},
				}
				for _, l := range seed {
					So(LOG_DB.Create(l).Error, ShouldBeNil)
				}

				readBucket := func(granularity string, createdAt int64) string {
					expr, err := dashboardBucketExpression(granularity)
					So(err, ShouldBeNil)
					var bucket string
					So(LOG_DB.Raw(fmt.Sprintf("SELECT %s as bucket FROM logs WHERE created_at = ?", expr), createdAt).Scan(&bucket).Error, ShouldBeNil)
					return bucket
				}

				So(readBucket("hour", wednesday), ShouldEqual, "2024-07-03 12:00")
				So(readBucket("day", wednesday), ShouldEqual, "2024-07-03")
				So(readBucket("week", wednesday), ShouldEqual, "2024-07-01")
				// 周日必须回退到本周周一，而非跨到下一周或停留在周日
				So(readBucket("week", sunday), ShouldEqual, "2024-07-01")
			})
		})
	})
}

// TestDashboardDimensionColumn 覆盖维度白名单闭集映射：四个合法维度解析为可信列名，
// 未知/注入形状输入在访问数据库前即被拒绝（测试期间 LOG_DB 置 nil 以证明确实未触库）。
func TestDashboardDimensionColumn(t *testing.T) {
	Convey("dashboardDimensionColumn maps the closed dimension allow-list", t, func() {
		origLogDB := LOG_DB
		LOG_DB = nil
		defer func() { LOG_DB = origLogDB }()

		Convey("Supported dimensions map to trusted columns", func() {
			cases := []struct {
				dimension string
				want      string
			}{
				{"model", "model_name"},
				{"channel", "channel_name"},
				{"token", "token_name"},
				{"user", "username"},
			}
			for _, tc := range cases {
				got, err := dashboardDimensionColumn(tc.dimension)
				So(err, ShouldBeNil)
				So(got, ShouldEqual, tc.want)
			}
		})

		Convey("Unknown and injection-shaped dimensions are rejected without DB access", func() {
			for _, dimension := range []string{
				"", "models", "Model", "channel_name", "username", "id",
				"model_name", "model_name, username", "model_name; DROP TABLE logs",
				"model_name OR 1=1", "model_name`", "user'--", "model_name) UNION SELECT 1--",
			} {
				got, err := dashboardDimensionColumn(dimension)
				So(err, ShouldNotBeNil)
				So(got, ShouldEqual, "")
			}
		})
	})
}

// seedDashboardAggregateLogs 在内存 SQLite 的 logs 表种入聚合查询夹具：
// 含 consume 与非 consume 混入、first_token_time/elapsed_time 同时包含 0 与正值、
// 跨用户与跨天数据，用于锁定求和、正样本均值排除零值、排序确定性与作用域。
func seedDashboardAggregateLogs(t *testing.T, day1, day2 int64) {
	t.Helper()
	seed := []*Log{
		// day1 / gpt-4o：u1 三条，ttft 与 elapsed 均含 0 与正值
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day1,
			Quota: 100, PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5, FirstTokenTime: 200, ElapsedTime: 1000},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day1,
			Quota: 200, PromptTokens: 20, CompletionTokens: 40, CachedTokens: 15, FirstTokenTime: 0, ElapsedTime: 2000},
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day1,
			Quota: 300, PromptTokens: 30, CompletionTokens: 60, CachedTokens: 25, FirstTokenTime: 400, ElapsedTime: 0},
		// day2 / gpt-4o：u1 一条正值样本
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day2,
			Quota: 400, PromptTokens: 40, CompletionTokens: 80, CachedTokens: 35, FirstTokenTime: 600, ElapsedTime: 3000},
		// day1 / gpt-4o：u2 一条，用于 userID/username 作用域
		{UserId: 2, Username: "u2", Type: LogTypeConsume, ModelName: "gpt-4o", CreatedAt: day1,
			Quota: 500, PromptTokens: 50, CompletionTokens: 100, CachedTokens: 45, FirstTokenTime: 800, ElapsedTime: 4000},
		// day1 / gpt-4o-mini：全零延迟样本，均值应为 0 且样本数为 0
		{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o-mini", CreatedAt: day1,
			Quota: 50, PromptTokens: 5, CompletionTokens: 5, CachedTokens: 1},
		// 非 consume 日志必须被完全排除
		{UserId: 1, Username: "u1", Type: LogTypeManage, ModelName: "gpt-4o", CreatedAt: day1,
			Quota: 9999, PromptTokens: 999, CompletionTokens: 999, CachedTokens: 999, FirstTokenTime: 5000, ElapsedTime: 5000},
		{UserId: 2, Username: "u2", Type: LogTypeTest, ModelName: "gpt-4o", CreatedAt: day1,
			Quota: 8888, PromptTokens: 888, CompletionTokens: 888, FirstTokenTime: 5000, ElapsedTime: 5000},
	}
	for _, l := range seed {
		if err := LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed dashboard aggregate log: %v", err)
		}
	}
}

// TestSearchDashboardAggregates 覆盖通用聚合查询：内存 SQLite 夹具下按 (bucket, key)
// 分组，锁定求和正确、非 consume 日志被排除、first_token_time/elapsed_time 的零值样本
// 不参与均值、输出按 bucket/key 升序确定性排列，以及 userID/username 作用域。
func TestSearchDashboardAggregates(t *testing.T) {
	origLogDB := LOG_DB
	origUsingSQLite := common.UsingSQLite
	origRedisEnabled := common.RedisEnabled
	defer func() {
		LOG_DB = origLogDB
		common.UsingSQLite = origUsingSQLite
		common.RedisEnabled = origRedisEnabled
	}()

	initTestLogDB(t)
	// model 维度会读取元数据库构建等价映射，必须先初始化（空表 → 仅前缀剥离+小写化基线）
	restoreMetadata := initTestMetadataDB(t)
	defer restoreMetadata()

	day1 := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC).Unix()
	seedDashboardAggregateLogs(t, day1, day2)

	// 覆盖两天的闭区间
	start := time.Date(2024, 7, 3, 0, 0, 0, 0, time.UTC).Unix()
	end := time.Date(2024, 7, 4, 23, 59, 59, 0, time.UTC).Unix()

	Convey("SearchDashboardAggregates groups consume logs by bucket and key", t, func() {
		Convey("Aggregate totals exclude non-consume logs and order bucket/key ascending", func() {
			rows, err := SearchDashboardAggregates(0, start, end, "", "model", "day")
			So(err, ShouldBeNil)
			So(len(rows), ShouldEqual, 3)

			// 排序确定性：bucket 升序，同 bucket 内 key 升序
			So(rows[0].Bucket, ShouldEqual, "2024-07-03")
			So(rows[0].Key, ShouldEqual, "gpt-4o")
			So(rows[1].Bucket, ShouldEqual, "2024-07-03")
			So(rows[1].Key, ShouldEqual, "gpt-4o-mini")
			So(rows[2].Bucket, ShouldEqual, "2024-07-04")
			So(rows[2].Key, ShouldEqual, "gpt-4o")

			// day1/gpt-4o 聚合 u1 三条 + u2 一条；非 consume 的 9999/8888 必须被排除
			So(rows[0].Requests, ShouldEqual, 4)
			So(rows[0].Quota, ShouldEqual, 1100)
			So(rows[0].PromptTokens, ShouldEqual, 110)
			So(rows[0].CompletionTokens, ShouldEqual, 220)
			So(rows[0].CachedTokens, ShouldEqual, 90)
		})

		Convey("Latency averages exclude zero samples while sums keep positive totals", func() {
			rows, err := SearchDashboardAggregates(0, start, end, "", "model", "day")
			So(err, ShouldBeNil)

			// ttft 正值样本 200/400/800，零值被排除：1400/3 ≈ 466.67，而非 1400/4 = 350
			So(rows[0].TTFTTotal, ShouldEqual, 1400)
			So(rows[0].TTFTSamples, ShouldEqual, 3)
			So(rows[0].AvgTTFT, ShouldAlmostEqual, 1400.0/3.0, 1e-9)

			// elapsed 正值样本 1000/2000/4000，零值被排除：7000/3 ≈ 2333.33，而非 7000/4 = 1750
			So(rows[0].ElapsedTotal, ShouldEqual, 7000)
			So(rows[0].ElapsedSamples, ShouldEqual, 3)
			So(rows[0].AvgElapsed, ShouldAlmostEqual, 7000.0/3.0, 1e-9)

			// 全零样本组：均值为 0 且样本数为 0
			So(rows[1].AvgTTFT, ShouldEqual, 0)
			So(rows[1].AvgElapsed, ShouldEqual, 0)
			So(rows[1].TTFTSamples, ShouldEqual, 0)
			So(rows[1].ElapsedSamples, ShouldEqual, 0)

			// day2/gpt-4o 单条正值样本
			So(rows[2].AvgTTFT, ShouldEqual, 600)
			So(rows[2].AvgElapsed, ShouldEqual, 3000)
			So(rows[2].TTFTSamples, ShouldEqual, 1)
			So(rows[2].ElapsedSamples, ShouldEqual, 1)
		})

		Convey("userID > 0 scopes to that user only", func() {
			rows, err := SearchDashboardAggregates(1, start, end, "", "model", "day")
			So(err, ShouldBeNil)
			So(len(rows), ShouldEqual, 3)

			So(rows[0].Bucket, ShouldEqual, "2024-07-03")
			So(rows[0].Key, ShouldEqual, "gpt-4o")
			So(rows[0].Requests, ShouldEqual, 3)
			So(rows[0].Quota, ShouldEqual, 600)
			So(rows[0].PromptTokens, ShouldEqual, 60)
			So(rows[0].CompletionTokens, ShouldEqual, 120)
			So(rows[0].CachedTokens, ShouldEqual, 45)
			// 仅 u1 的正值样本：ttft 200/400 → 300；elapsed 1000/2000 → 1500
			So(rows[0].AvgTTFT, ShouldAlmostEqual, 300.0, 1e-9)
			So(rows[0].AvgElapsed, ShouldAlmostEqual, 1500.0, 1e-9)
		})

		Convey("username adds exact filtering on top of userID=0", func() {
			rows, err := SearchDashboardAggregates(0, start, end, "u2", "model", "day")
			So(err, ShouldBeNil)
			So(len(rows), ShouldEqual, 1)
			So(rows[0].Bucket, ShouldEqual, "2024-07-03")
			So(rows[0].Key, ShouldEqual, "gpt-4o")
			So(rows[0].Requests, ShouldEqual, 1)
			So(rows[0].Quota, ShouldEqual, 500)
			So(rows[0].AvgTTFT, ShouldEqual, 800)
			So(rows[0].AvgElapsed, ShouldEqual, 4000)
		})

		Convey("Invalid dimension or granularity is rejected before any database access", func() {
			origLogDBForReject := LOG_DB
			LOG_DB = nil
			defer func() { LOG_DB = origLogDBForReject }()

			_, err := SearchDashboardAggregates(0, start, end, "", "model_name", "day")
			So(err, ShouldNotBeNil)
			_, err = SearchDashboardAggregates(0, start, end, "", "model", "minute")
			So(err, ShouldNotBeNil)
		})
	})
}

// captureSQL 是一个仅记录 Trace SQL 的 GORM logger，用于在 dry-run 下断言生成的查询语句。
type captureSQL struct {
	sqls []string
}

func (c *captureSQL) LogMode(glogger.LogLevel) glogger.Interface    { return c }
func (c *captureSQL) Info(context.Context, string, ...interface{})  {}
func (c *captureSQL) Warn(context.Context, string, ...interface{})  {}
func (c *captureSQL) Error(context.Context, string, ...interface{}) {}
func (c *captureSQL) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	c.sqls = append(c.sqls, sql)
}

// captureDashboardAggregateSQL 在指定方言下用 dry-run 捕获 SearchDashboardAggregates 实际
// 生成的 SQL（不触真实数据库），返回拼接后的语句文本供表达式级断言。
func captureDashboardAggregateSQL(t *testing.T, dimension string) string {
	t.Helper()

	origLogDB := LOG_DB
	defer func() { LOG_DB = origLogDB }()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{DryRun: true})
	if err != nil {
		t.Fatalf("failed to open dry-run db: %v", err)
	}
	logger := &captureSQL{}
	db.Logger = logger
	LOG_DB = db

	// dry-run 下 Raw().Scan 会因无结果集返回错误，但 SQL 已在执行前被 Trace 记录；
	// 这里只关心生成的语句文本，故忽略查询错误。
	_, _ = SearchDashboardAggregates(0, 1, 2, "", dimension, "day")
	if len(logger.sqls) == 0 {
		t.Fatalf("no SQL captured for dimension %q", dimension)
	}
	return logger.sqls[len(logger.sqls)-1]
}

// TestSearchDashboardAggregatesDimensionCaseSensitivity 锁定非 model 维度键在 MySQL 方言下的
// 大小写敏感分组/排序：MySQL 默认 ci collation 会把 Token-A 与 token-a 在 SQL 层错误合并，
// 故 MySQL 必须使用 BINARY 表达式，且 SELECT/GROUP BY/ORDER BY 表达式一致（ONLY_FULL_GROUP_BY）；
// PostgreSQL/SQLite 默认大小写敏感，不得引入 BINARY。SQLite 运行时大小写独立行由
// TestNormalizeDashboardAggregates 的 token 用例覆盖。
func TestSearchDashboardAggregatesDimensionCaseSensitivity(t *testing.T) {
	Convey("Non-model dimension keys are case-sensitive on MySQL only", t, func() {
		Convey("MySQL uses BINARY for the dimension key in SELECT, GROUP BY, and ORDER BY", func() {
			withDialect(true, false, false, func() {
				sql := captureDashboardAggregateSQL(t, "token")
				So(sql, ShouldContainSubstring, "BINARY token_name")
				So(sql, ShouldContainSubstring, "GROUP BY bucket, BINARY token_name")
				So(sql, ShouldContainSubstring, "ORDER BY bucket, BINARY token_name")
			})
		})

		Convey("Unrecognized dialect falls back to the MySQL case-sensitive form", func() {
			withDialect(false, false, false, func() {
				sql := captureDashboardAggregateSQL(t, "channel")
				So(sql, ShouldContainSubstring, "BINARY channel_name")
				So(sql, ShouldContainSubstring, "GROUP BY bucket, BINARY channel_name")
				So(sql, ShouldContainSubstring, "ORDER BY bucket, BINARY channel_name")
			})
		})

		Convey("PostgreSQL and SQLite keep the plain case-sensitive column", func() {
			withDialect(false, true, false, func() {
				sql := captureDashboardAggregateSQL(t, "token")
				So(sql, ShouldNotContainSubstring, "BINARY")
				So(sql, ShouldContainSubstring, "GROUP BY bucket, token_name")
			})
			withDialect(false, false, true, func() {
				sql := captureDashboardAggregateSQL(t, "user")
				So(sql, ShouldNotContainSubstring, "BINARY")
				So(sql, ShouldContainSubstring, "GROUP BY bucket, username")
			})
		})
	})
}

// TestNormalizeDashboardAggregates 覆盖聚合行的模型维度归一化边界：模型名归一化必须复用
// normalizeLogStatistics（厂商前缀剥离/小写/别名归并/元数据降级），归并时 sum 指标求和、
// 延迟按内部正样本字段做加权平均（而非对各行的 AvgTTFT/AvgElapsed 简单平均），
// 无正样本时延迟为 0；token 等其他维度的 key 由调用方保证不经过本函数、原样保留，
// 大小写不同即保持独立行。
func TestNormalizeDashboardAggregates(t *testing.T) {
	Convey("normalizeDashboardAggregates normalizes model rows only", t, func() {
		Convey("Case and alias variants merge into one canonical row with summed metrics", func() {
			stats := []*DashboardAggregate{
				{Bucket: "2024-07-03", Key: "DeepSeek-v4-Pro", Requests: 2, Quota: 100, PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5},
				{Bucket: "2024-07-03", Key: "deepseek-v4-pro", Requests: 1, Quota: 50, PromptTokens: 5, CompletionTokens: 7, CachedTokens: 3},
				{Bucket: "2024-07-03", Key: "openai/DeepSeek-v4-Pro-0813", Requests: 4, Quota: 200, PromptTokens: 40, CompletionTokens: 80, CachedTokens: 10},
			}
			alias := map[string]string{"deepseekv4pro0813": "deepseek-v4-pro"}
			got := normalizeDashboardAggregates(stats, alias)

			So(len(got), ShouldEqual, 1)
			So(got[0].Bucket, ShouldEqual, "2024-07-03")
			So(got[0].Key, ShouldEqual, "deepseek-v4-pro")
			So(got[0].Requests, ShouldEqual, 7)
			So(got[0].Quota, ShouldEqual, 350)
			So(got[0].PromptTokens, ShouldEqual, 55)
			So(got[0].CompletionTokens, ShouldEqual, 107)
			So(got[0].CachedTokens, ShouldEqual, 18)
		})

		Convey("Merged latency is sample-weighted, not a plain average of row averages", func() {
			// 两行样本数不同：加权口径 1100/5 = 220；若误做简单平均则为 (100+250)/2 = 175
			stats := []*DashboardAggregate{
				{Bucket: "2024-07-03", Key: "DeepSeek-v4-Pro",
					TTFTTotal: 100, TTFTSamples: 1, AvgTTFT: 100,
					ElapsedTotal: 2000, ElapsedSamples: 2, AvgElapsed: 1000},
				{Bucket: "2024-07-03", Key: "deepseek-v4-pro",
					TTFTTotal: 1000, TTFTSamples: 4, AvgTTFT: 250,
					ElapsedTotal: 2000, ElapsedSamples: 1, AvgElapsed: 2000},
			}
			got := normalizeDashboardAggregates(stats, nil)

			So(len(got), ShouldEqual, 1)
			So(got[0].TTFTTotal, ShouldEqual, 1100)
			So(got[0].TTFTSamples, ShouldEqual, 5)
			So(got[0].AvgTTFT, ShouldAlmostEqual, 220.0, 1e-9)
			// elapsed 加权口径 4000/3 ≈ 1333.33；若误做简单平均则为 (1000+2000)/2 = 1500
			So(got[0].ElapsedTotal, ShouldEqual, 4000)
			So(got[0].ElapsedSamples, ShouldEqual, 3)
			So(got[0].AvgElapsed, ShouldAlmostEqual, 4000.0/3.0, 1e-9)
		})

		Convey("A group with no positive samples yields zero latency", func() {
			// 防御性锁定：均值只由正样本字段推导，输入均值不可信时无样本仍必须归零
			stats := []*DashboardAggregate{
				{Bucket: "2024-07-03", Key: "GPT-4o-Mini", AvgTTFT: 500, AvgElapsed: 500,
					TTFTTotal: 0, TTFTSamples: 0, ElapsedTotal: 0, ElapsedSamples: 0},
			}
			got := normalizeDashboardAggregates(stats, nil)

			So(len(got), ShouldEqual, 1)
			So(got[0].Key, ShouldEqual, "gpt-4o-mini")
			So(got[0].AvgTTFT, ShouldEqual, 0)
			So(got[0].AvgElapsed, ShouldEqual, 0)
		})

		Convey("Rows are split per bucket and returned in bucket/key ascending order", func() {
			stats := []*DashboardAggregate{
				{Bucket: "2024-07-04", Key: "gpt-4o", Requests: 1},
				{Bucket: "2024-07-03", Key: "GPT-4O", Requests: 2},
				{Bucket: "2024-07-03", Key: "gpt-4o-mini", Requests: 3},
			}
			got := normalizeDashboardAggregates(stats, nil)

			So(len(got), ShouldEqual, 3)
			So(got[0].Bucket, ShouldEqual, "2024-07-03")
			So(got[0].Key, ShouldEqual, "gpt-4o")
			So(got[0].Requests, ShouldEqual, 2)
			So(got[1].Bucket, ShouldEqual, "2024-07-03")
			So(got[1].Key, ShouldEqual, "gpt-4o-mini")
			So(got[1].Requests, ShouldEqual, 3)
			So(got[2].Bucket, ShouldEqual, "2024-07-04")
			So(got[2].Key, ShouldEqual, "gpt-4o")
			So(got[2].Requests, ShouldEqual, 1)
		})

		Convey("Empty alias degrades to vendor-prefix stripping plus lowercase only", func() {
			stats := []*DashboardAggregate{
				{Bucket: "2024-07-03", Key: "openai/GPT-4O", Requests: 1},
			}
			got := normalizeDashboardAggregates(stats, nil)

			So(len(got), ShouldEqual, 1)
			So(got[0].Key, ShouldEqual, "gpt-4o")
		})

		Convey("Token dimension keys stay verbatim so case variants remain distinct", func() {
			origLogDB := LOG_DB
			origUsingSQLite := common.UsingSQLite
			origRedisEnabled := common.RedisEnabled
			defer func() {
				LOG_DB = origLogDB
				common.UsingSQLite = origUsingSQLite
				common.RedisEnabled = origRedisEnabled
			}()

			initTestLogDB(t)
			// model 维度会读取元数据库构建等价映射，必须先初始化（空表 → 仅前缀剥离+小写化基线）
			restoreMetadata := initTestMetadataDB(t)
			defer restoreMetadata()

			// 同一份日志在模型名与 token 名上均只差大小写
			day := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()
			seed := []*Log{
				{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "GPT-4O", TokenName: "Token-A", CreatedAt: day, Quota: 10},
				{UserId: 1, Username: "u1", Type: LogTypeConsume, ModelName: "gpt-4o", TokenName: "token-a", CreatedAt: day, Quota: 20},
			}
			for _, l := range seed {
				So(LOG_DB.Create(l).Error, ShouldBeNil)
			}

			// token 维度：key 原样保留，大小写不同即两行独立（不得经过归一化）
			tokenRows, err := SearchDashboardAggregates(0, day, day, "", "token", "day")
			So(err, ShouldBeNil)
			So(len(tokenRows), ShouldEqual, 2)
			So(tokenRows[0].Key, ShouldEqual, "Token-A")
			So(tokenRows[0].Quota, ShouldEqual, 10)
			So(tokenRows[1].Key, ShouldEqual, "token-a")
			So(tokenRows[1].Quota, ShouldEqual, 20)

			// model 维度：链路内已由 SearchDashboardAggregates 归一化合并为单行，且 sum 指标正确
			modelRows, err := SearchDashboardAggregates(0, day, day, "", "model", "day")
			So(err, ShouldBeNil)
			So(len(modelRows), ShouldEqual, 1)
			So(modelRows[0].Key, ShouldEqual, "gpt-4o")
			So(modelRows[0].Requests, ShouldEqual, 2)
			So(modelRows[0].Quota, ShouldEqual, 30)
		})
	})
}

// TestSearchDashboardSummary 覆盖 KPI 汇总查询：在内存 SQLite 夹具下锁定
// total_tokens = prompt + completion（cached 不重复计入）、cache_hit_rate = cached/prompt
// 且 prompt 为 0 时返回 0（非 NaN）、延迟均值仅取正样本且无正样本为 0、
// avg_rpm/avg_tpm 按请求范围分钟数计算并对非正时长防御归零，
// 以及非 consume 日志排除与 userID/username 作用域。
func TestSearchDashboardSummary(t *testing.T) {
	origLogDB := LOG_DB
	origUsingSQLite := common.UsingSQLite
	origRedisEnabled := common.RedisEnabled
	defer func() {
		LOG_DB = origLogDB
		common.UsingSQLite = origUsingSQLite
		common.RedisEnabled = origRedisEnabled
	}()

	initTestLogDB(t)

	// 主夹具：复用聚合测试的消费/非消费混入、正/零延迟、跨用户与跨天数据
	day1 := time.Date(2024, 7, 3, 12, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2024, 7, 4, 12, 0, 0, 0, time.UTC).Unix()
	seedDashboardAggregateLogs(t, day1, day2)

	// 覆盖两天的闭区间
	start := time.Date(2024, 7, 3, 0, 0, 0, 0, time.UTC).Unix()
	end := time.Date(2024, 7, 4, 23, 59, 59, 0, time.UTC).Unix()

	// prompt_tokens=0 但 cached_tokens>0：命中率分母为 0 时必须返回 0 而非 NaN
	zeroPromptDay := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC).Unix()
	if err := LOG_DB.Create(&Log{
		UserId: 5, Username: "u5", Type: LogTypeConsume, CreatedAt: zeroPromptDay,
		Quota: 10, PromptTokens: 0, CompletionTokens: 10, CachedTokens: 7,
	}).Error; err != nil {
		t.Fatalf("failed to seed zero-prompt summary log: %v", err)
	}

	// 60 分钟窗口夹具：120 条请求、每条 prompt=25 + completion=25，
	// 合计 total_tokens=6000，用于断言 avg_rpm=120/60=2、avg_tpm=6000/60=100
	windowStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	windowEnd := windowStart + int64(time.Hour.Seconds())
	for i := 0; i < 120; i++ {
		if err := LOG_DB.Create(&Log{
			UserId: 4, Username: "u4", Type: LogTypeConsume, CreatedAt: windowStart + int64(i),
			Quota: 100, PromptTokens: 25, CompletionTokens: 25,
		}).Error; err != nil {
			t.Fatalf("failed to seed minute-window summary log: %v", err)
		}
	}

	Convey("SearchDashboardSummary aggregates consume logs into KPI totals", t, func() {
		Convey("Totals, token sum, and cache hit rate use prompt as denominator", func() {
			summary, err := SearchDashboardSummary(0, start, end, "")
			So(err, ShouldBeNil)
			So(summary.TotalRequests, ShouldEqual, 6)
			So(summary.TotalQuota, ShouldEqual, 1550)
			// total_tokens = prompt(155) + completion(305)；cached(126) 不重复计入
			So(summary.TotalTokens, ShouldEqual, 460)
			So(summary.CachedTokens, ShouldEqual, 126)
			So(summary.PromptTokens, ShouldEqual, 155)
			So(summary.CacheHitRate, ShouldAlmostEqual, 126.0/155.0, 1e-9)
		})

		Convey("Latency averages exclude zero samples and keep positive-only means", func() {
			summary, err := SearchDashboardSummary(0, start, end, "")
			So(err, ShouldBeNil)
			// ttft 正样本 200/400/600/800 → 500；零值样本不参与分母
			So(summary.AvgTTFT, ShouldNotBeNil)
			So(*summary.AvgTTFT, ShouldAlmostEqual, 500.0, 1e-9)
			// elapsed 正样本 1000/2000/3000/4000 → 2500
			So(summary.AvgElapsed, ShouldNotBeNil)
			So(*summary.AvgElapsed, ShouldAlmostEqual, 2500.0, 1e-9)
		})

		Convey("No positive samples yields null latency while totals stay zero-valued", func() {
			// u5 仅一条消费日志且 first_token_time/elapsed_time 均为 0：无正样本，
			// 延迟必须为 nil（JSON null）而非 0，前端才能渲染为 "--" 而非 "0.0 ms"
			summary, err := SearchDashboardSummary(5, zeroPromptDay, zeroPromptDay, "")
			So(err, ShouldBeNil)
			So(summary.AvgTTFT, ShouldBeNil)
			So(summary.AvgElapsed, ShouldBeNil)
			// total_* 的数值零仍是有效值，必须继续为 0 而非 nil
			So(summary.TotalRequests, ShouldEqual, 1)
			So(summary.TotalQuota, ShouldEqual, 10)
			So(summary.TotalTokens, ShouldEqual, 10)
			So(summary.CachedTokens, ShouldEqual, 7)
		})

		Convey("Zero prompt denominator yields zero cache hit rate instead of NaN", func() {
			summary, err := SearchDashboardSummary(5, zeroPromptDay, zeroPromptDay, "")
			So(err, ShouldBeNil)
			So(summary.TotalRequests, ShouldEqual, 1)
			So(summary.PromptTokens, ShouldEqual, 0)
			So(summary.CachedTokens, ShouldEqual, 7)
			So(summary.CacheHitRate, ShouldEqual, 0)
			// 显式排除 NaN：NaN 不满足自反相等
			So(summary.CacheHitRate == summary.CacheHitRate, ShouldBeTrue)
		})

		Convey("AvgRPM and AvgTPM use the requested range duration in minutes", func() {
			summary, err := SearchDashboardSummary(4, windowStart, windowEnd, "")
			So(err, ShouldBeNil)
			So(summary.TotalRequests, ShouldEqual, 120)
			So(summary.TotalTokens, ShouldEqual, 6000)
			// 60 分钟窗口：120/60=2 RPM，6000/60=100 TPM
			So(summary.AvgRPM, ShouldAlmostEqual, 2.0, 1e-9)
			So(summary.AvgTPM, ShouldAlmostEqual, 100.0, 1e-9)
		})

		Convey("Non-positive duration is defensively reduced to zero rates", func() {
			// HTTP 边界已拒绝非正时长，model 层仍需防御除零：零时长与倒置区间都返回 0
			zeroDuration, err := SearchDashboardSummary(0, start, start, "")
			So(err, ShouldBeNil)
			So(zeroDuration.AvgRPM, ShouldEqual, 0)
			So(zeroDuration.AvgTPM, ShouldEqual, 0)

			inverted, err := SearchDashboardSummary(0, end, start, "")
			So(err, ShouldBeNil)
			So(inverted.AvgRPM, ShouldEqual, 0)
			So(inverted.AvgTPM, ShouldEqual, 0)
		})

		Convey("userID > 0 scopes the summary to that user only", func() {
			summary, err := SearchDashboardSummary(1, start, end, "")
			So(err, ShouldBeNil)
			So(summary.TotalRequests, ShouldEqual, 5)
			So(summary.TotalQuota, ShouldEqual, 1050)
			So(summary.TotalTokens, ShouldEqual, 310)
			So(summary.CachedTokens, ShouldEqual, 81)
			So(summary.CacheHitRate, ShouldAlmostEqual, 81.0/105.0, 1e-9)
			// 仅 u1 的正值样本：ttft 200/400/600 → 400；elapsed 1000/2000/3000 → 2000
			So(summary.AvgTTFT, ShouldNotBeNil)
			So(*summary.AvgTTFT, ShouldAlmostEqual, 400.0, 1e-9)
			So(summary.AvgElapsed, ShouldNotBeNil)
			So(*summary.AvgElapsed, ShouldAlmostEqual, 2000.0, 1e-9)
		})

		Convey("username adds exact filtering on top of userID=0", func() {
			summary, err := SearchDashboardSummary(0, start, end, "u2")
			So(err, ShouldBeNil)
			So(summary.TotalRequests, ShouldEqual, 1)
			So(summary.TotalQuota, ShouldEqual, 500)
			So(summary.TotalTokens, ShouldEqual, 150)
			So(summary.CachedTokens, ShouldEqual, 45)
			So(summary.CacheHitRate, ShouldAlmostEqual, 45.0/50.0, 1e-9)
			So(summary.AvgTTFT, ShouldNotBeNil)
			So(*summary.AvgTTFT, ShouldEqual, 800)
			So(summary.AvgElapsed, ShouldNotBeNil)
			So(*summary.AvgElapsed, ShouldEqual, 4000)
		})

		Convey("Non-consume logs are excluded from every KPI", func() {
			// u1 在 day1 另有 LogTypeManage 的 9999 quota/999 tokens，必须完全排除
			summary, err := SearchDashboardSummary(1, start, day1, "")
			So(err, ShouldBeNil)
			So(summary.TotalQuota, ShouldEqual, 650)
			// prompt 65 + completion 125；LogTypeManage 的 999 不得混入
			So(summary.TotalTokens, ShouldEqual, 190)
			So(summary.TotalRequests, ShouldEqual, 4)
		})

		Convey("Empty range yields zero-valued summary instead of NULL scan failure", func() {
			emptyDay := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
			summary, err := SearchDashboardSummary(0, emptyDay, emptyDay+3599, "")
			So(err, ShouldBeNil)
			So(summary.TotalRequests, ShouldEqual, 0)
			So(summary.TotalQuota, ShouldEqual, 0)
			So(summary.TotalTokens, ShouldEqual, 0)
			So(summary.CachedTokens, ShouldEqual, 0)
			So(summary.CacheHitRate, ShouldEqual, 0)
			// 空区间无任何正样本：延迟为 nil（JSON null），total_* 的零值仍为 0
			So(summary.AvgTTFT, ShouldBeNil)
			So(summary.AvgElapsed, ShouldBeNil)
		})
	})
}
