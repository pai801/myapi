package model

import (
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// 回归用例：SQLite 部署下「第一次启动成功、之后每次启动 FATAL」。
// 覆盖路径：存量库里残留 decimal(10,4) 这种带逗号的列类型时，
// gorm sqlite 的 AlterColumn 会生成非法 DDL（invalid DDL, unbalanced brackets）。

func openTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "t.db")), &gorm.Config{PrepareStmt: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

func tableDDL(t *testing.T, db *gorm.DB, table string) string {
	t.Helper()
	var ddl string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = ? AND name = ?", "table", table).
		Scan(&ddl).Error; err != nil {
		t.Fatalf("read ddl of %s: %v", table, err)
	}
	return ddl
}

// TestSQLiteLegacyDecimalColumnDoesNotBreakRestart 用「旧版」DDL 建表后，
// 必须能连续多次 AutoMigrate 而不报错（即模拟反复重启）。
func TestSQLiteLegacyDecimalColumnDoesNotBreakRestart(t *testing.T) {
	db := openTestSQLite(t)

	// 旧版模型（type:decimal(10,4);default:1.0）落盘后的样子
	if err := db.Exec(`CREATE TABLE "groups" (` +
		"`id` integer,`name` varchar(32),`model_ratio` decimal(10,4) DEFAULT 1,`created_time` integer," +
		"PRIMARY KEY (`id`))").Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := db.Exec("INSERT INTO `groups`(`id`,`name`,`model_ratio`,`created_time`) VALUES (1,'default',1.0,100),(2,'vip',0.5,200)").
		Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 第一轮：修复 + 迁移
	if err := fixSQLiteCommaTypedColumns(db, &Group{}); err != nil {
		t.Fatalf("fixSQLiteCommaTypedColumns: %v", err)
	}
	if err := db.AutoMigrate(&Group{}); err != nil {
		t.Fatalf("first AutoMigrate: %v", err)
	}

	ddl := tableDDL(t, db, "groups")
	t.Logf("repaired DDL: %s", ddl)
	if sqliteCommaTypeRegexp.MatchString(ddl) {
		t.Fatalf("repaired DDL still contains a comma-typed column: %s", ddl)
	}

	// 第二轮、第三轮：模拟再次重启，不能报错，也不应再改动 DDL
	for i := 2; i <= 3; i++ {
		if err := fixSQLiteCommaTypedColumns(db, &Group{}); err != nil {
			t.Fatalf("round %d fix: %v", i, err)
		}
		if err := db.AutoMigrate(&Group{}); err != nil {
			t.Fatalf("round %d AutoMigrate: %v", i, err)
		}
	}
	if got := tableDDL(t, db, "groups"); got != ddl {
		t.Fatalf("DDL changed on later restart, means migration is not stable:\n before: %s\n after:  %s", ddl, got)
	}

	// 数据必须保留
	var count int64
	if err := db.Model(&Group{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows preserved, got %d", count)
	}
	var ratio float64
	if err := db.Model(&Group{}).Where("name = ?", "vip").Pluck("model_ratio", &ratio).Error; err != nil {
		t.Fatalf("pluck: %v", err)
	}
	if ratio != 0.5 {
		t.Fatalf("expected model_ratio 0.5 preserved, got %v", ratio)
	}
}

// TestSQLiteFreshGroupTableIsCommaFree 全新库：建表后类型里不能出现逗号，
// 且后续重启不会触发多余的 AlterColumn（DDL 保持不变）。
//
// 注意：gorm sqlite 对带 uniqueIndex 的表每轮 AutoMigrate 都会走一次
// recreateTable（先 DropConstraint 再重建），首轮之后表名的引号形态会稳定下来。
// 因此这里先跑两轮「预热」，再断言之后不再变化。
func TestSQLiteFreshGroupTableIsCommaFree(t *testing.T) {
	db := openTestSQLite(t)

	if err := db.AutoMigrate(&Group{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ddl := tableDDL(t, db, "groups")
	t.Logf("fresh DDL: %s", ddl)
	if sqliteCommaTypeRegexp.MatchString(ddl) {
		t.Fatalf("fresh DDL contains comma-typed column: %s", ddl)
	}
	if !strings.Contains(ddl, "real") {
		t.Fatalf("expected model_ratio to be real on sqlite, got: %s", ddl)
	}

	// 预热一轮，让表名引号形态稳定
	if err := db.AutoMigrate(&Group{}); err != nil {
		t.Fatalf("warmup AutoMigrate: %v", err)
	}
	settled := tableDDL(t, db, "groups")

	for i := 3; i <= 4; i++ {
		if err := db.AutoMigrate(&Group{}); err != nil {
			t.Fatalf("round %d AutoMigrate: %v", i, err)
		}
	}
	if got := tableDDL(t, db, "groups"); got != settled {
		t.Fatalf("DDL changed on restart:\n before: %s\n after:  %s", settled, got)
	}
}

// TestFixSQLiteCommaTypedColumnsNoOpWhenClean 没有逗号类型时必须是 no-op（DDL 原样不动）。
func TestFixSQLiteCommaTypedColumnsNoOpWhenClean(t *testing.T) {
	db := openTestSQLite(t)
	if err := db.AutoMigrate(&Group{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := tableDDL(t, db, "groups")
	if err := fixSQLiteCommaTypedColumns(db, &Channel{}, &Token{}, &User{}, &Option{},
		&Ability{}, &Log{}, &ModelMetadata{}, &Group{}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	if after := tableDDL(t, db, "groups"); after != before {
		t.Fatalf("no-op check failed:\n before: %s\n after:  %s", before, after)
	}
}

// TestSQLiteLegacyTableSurvivesAutoMigrateWithDefaultOne 守护 `default:1`。
//
// 这条是 reviewer 指出的缺口补的：把 tag 的 default 从 1 改回 1.0，其它 4 条用例**仍然全绿**，
// 无人报警；但 1.0 会让 gorm 认为默认值不匹配（dv="1" vs DefaultValue="1.0"）从而每轮都去
// AlterColumn —— 对存量 decimal(10,4) 库就是每启动重建一次表，且一旦 gorm 修掉
// `alterColumn = dv != ...` 那个赋值笔误就会直接 FATAL。所以必须有一条用例盯住它。
//
// 判据：存量库（未修复）上跑 AutoMigrate 必须**成功且 DDL 一字不动**。
// default=1 时 gorm 认为该列已是最新 → 既不 AlterColumn 也不重建；改成 1.0 则当场报错。
func TestSQLiteLegacyTableSurvivesAutoMigrateWithDefaultOne(t *testing.T) {
	db := openTestSQLite(t)

	if err := db.Exec(`CREATE TABLE "groups" (` +
		"`id` integer,`name` varchar(32),`model_ratio` decimal(10,4) DEFAULT 1,`created_time` integer," +
		"PRIMARY KEY (`id`))").Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	before := tableDDL(t, db, "groups")

	// 注意：这里**不**跑修复，就是为了让本用例对 default 值敏感。
	if err := db.AutoMigrate(&Group{}); err != nil {
		t.Fatalf("AutoMigrate on legacy table must succeed with default=1 (改成 1.0 会在这里炸): %v", err)
	}
	if after := tableDDL(t, db, "groups"); after != before {
		t.Fatalf("AutoMigrate rebuilt the table, meaning the column was considered out of date "+
			"(default 值又被判为不匹配了):\n before: %s\n after:  %s", before, after)
	}
}

// TestSQLiteRepairCleansUpLeftoverTempTable 覆盖「上一轮异常中断留下 groups__fix_tmp」的分支：
// 修复必须先 DROP 掉残留临时表，否则 CREATE TABLE 会撞名失败，把一次可自愈的中断变成启动 FATAL。
func TestSQLiteRepairCleansUpLeftoverTempTable(t *testing.T) {
	db := openTestSQLite(t)

	if err := db.Exec(`CREATE TABLE "groups" (` +
		"`id` integer,`name` varchar(32),`model_ratio` decimal(10,4) DEFAULT 1,`created_time` integer," +
		"PRIMARY KEY (`id`))").Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := db.Exec("INSERT INTO `groups`(`id`,`name`,`model_ratio`,`created_time`) VALUES (1,'default',1.0,100)").
		Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 模拟上一轮修到一半崩了：留一张同名临时表
	if err := db.Exec("CREATE TABLE `groups__fix_tmp` (`id` integer)").Error; err != nil {
		t.Fatalf("create leftover temp table: %v", err)
	}

	if err := fixSQLiteCommaTypedColumns(db, &Group{}); err != nil {
		t.Fatalf("fix with leftover temp table: %v", err)
	}
	if err := db.AutoMigrate(&Group{}); err != nil {
		t.Fatalf("AutoMigrate after fix: %v", err)
	}

	var left int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE name LIKE '%__fix_tmp'").Scan(&left).Error; err != nil {
		t.Fatalf("query leftover: %v", err)
	}
	if left != 0 {
		t.Fatalf("temp table left behind: %d", left)
	}
	var count int64
	if err := db.Model(&Group{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row preserved, got %d", count)
	}
}

// TestSQLiteLegacyDecimalColumnBreaksAlterColumn 反向证伪：证明底层缺陷真实存在，
// 且正是本文件的修复把它消掉的——否则上面那几条「修复后能反复重启」的用例都可能是恒真。
//
// 注意一个反直觉的事实（本用例把它固化下来）：只把 tag 的 default 从 1.0 改成 1，
// 存量库其实**已经不会崩了**——因为 MigrateColumn 里默认值那一段是
// `alterColumn = dv != field.DefaultValue`（**赋值而非 ||=**），dv("1") 与
// DefaultValue("1") 相等就把前面「类型不匹配」置起的 alterColumn 又**重置回 false**，
// 于是根本不会走到 AlterColumn。但这是依赖 gorm 的一处疑似笔误，极其脆弱：
// 一旦 gorm 把它改成 ||=，或任何其它原因触发一次 AlterColumn，存量 decimal(10,4) 立刻
// 又会让服务 FATAL 起不来。所以修复仍然要做——它把地雷整颗挖掉，而不是指望没人踩。
func TestSQLiteLegacyDecimalColumnBreaksAlterColumn(t *testing.T) {
	db := openTestSQLite(t)

	if err := db.Exec(`CREATE TABLE "groups" (` +
		"`id` integer,`name` varchar(32),`model_ratio` decimal(10,4) DEFAULT 1,`created_time` integer," +
		"PRIMARY KEY (`id`))").Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	// 未修复：只要真的走到 AlterColumn，必炸
	err := db.Migrator().AlterColumn(&Group{}, "model_ratio")
	if err == nil {
		// gorm 自己把缺陷修好了 —— 这不是回归，跳过并提示可以拆掉修复代码。
		// 用 Skip 而不是 Fatal：否则升级依赖后本用例会永久变红、掩盖真正的回归。
		t.Skip("gorm sqlite 已不再对 decimal(10,4) 报错，model/sqlite_migrate_fix.go 的存量库修复可以评估移除")
	}
	if !strings.Contains(err.Error(), "unbalanced brackets") {
		t.Fatalf("expected the known gorm sqlite failure, got: %v", err)
	}

	// 修复后：同一个操作不再失败，DDL 里的逗号类型也没了
	if err := fixSQLiteCommaTypedColumns(db, &Group{}); err != nil {
		t.Fatalf("fix: %v", err)
	}
	if err := db.Migrator().AlterColumn(&Group{}, "model_ratio"); err != nil {
		t.Fatalf("AlterColumn after repair: %v", err)
	}
	if ddl := tableDDL(t, db, "groups"); sqliteCommaTypeRegexp.MatchString(ddl) {
		t.Fatalf("comma-typed column survived repair: %s", ddl)
	}
}
