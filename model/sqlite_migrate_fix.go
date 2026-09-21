package model

import (
	"fmt"
	"regexp"

	"gorm.io/gorm"

	"github.com/pai801/myapi/common/logger"
)

// 背景：SQLite 部署下服务重启会 FATAL 起不来（failed to migrate database: invalid DDL, unbalanced brackets）。
//
// 根因在 gorm.io/driver/sqlite 的 AlterColumn：它用非贪婪正则在**第一个逗号**处截断列定义
//
//	reg = "(`|'|\"| )<列名>(`|'|\"| ) .*?(,|\\)\\s*$)"
//
// 而 groups.model_ratio 的列类型是 decimal(10,4)（**类型串里自带逗号**），于是
//
//	... `model_ratio` decimal(10,4) DEFAULT 1, ...
//	                           ^ 在这里被截断
//
// 替换后变成 `model_ratio` ?,4) DEFAULT 1 —— 多出一个右括号，随后 recreateTable 里的
// parseDDL 做括号配平检查时失败，整个 migrateDB 返回错误，进程 FATAL。
// 因为首次启动时表不存在（走 CreateTable），第二次启动时才走 AlterColumn，所以表现为
// 「第一次能起来，之后永远起不来」。
//
// 修复分两步，缺一不可：
//  1. 模型侧：把 type:decimal(10,4) 换成 precision:10;scale:4，让 SQLite 侧生成
//     comma-free 的 real（sqlite 的 DataTypeOf 对 Float 恒返回 real，忽略 precision），
//     同时 MySQL 侧仍然生成 decimal(10, 4)，MySQL schema 语义不变。
//  2. 存量库：已经落盘的 decimal(10,4) 不会因为改 tag 自动消失，而只要它还在，
//     AlterColumn 就依旧会炸。所以迁移前先把这类「带逗号参数的列类型」重建掉。
//
// 触发条件只有一条：SQLite + 表 DDL 里存在 (decimal|numeric)(数字,数字)。命中概率极低，
// 未命中时本文件所有函数都是 no-op（只多几次 sqlite_master 查询）。

// sqliteCommaTypeRegexp 匹配 decimal(10,4) / numeric(10, 2) 这类带逗号参数的列类型。
var sqliteCommaTypeRegexp = regexp.MustCompile(`(?i)\b(?:decimal|numeric)\s*\(\s*\d+\s*,\s*\d+\s*\)`)

// fixSQLiteCommaTypedColumns 把 db 上这些模型对应表中「带逗号参数的列类型」重建为 real。
// 仅对 SQLite 生效；其它方言直接返回 nil。
func fixSQLiteCommaTypedColumns(db *gorm.DB, models ...interface{}) error {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "sqlite" {
		return nil
	}

	for _, model := range models {
		table, err := sqliteTableNameOf(db, model)
		if err != nil || table == "" {
			continue
		}
		if err := fixSQLiteTableCommaTypes(db, table); err != nil {
			return fmt.Errorf("failed to repair legacy comma-typed column on table %s: %w", table, err)
		}
	}
	return nil
}

// fixSQLiteTableCommaTypes 重建单张表：新表 DDL 由旧 DDL 文本替换而来（只改类型，列序不变），
// 然后 create → INSERT SELECT * → DROP → RENAME，全程在一个事务里。
func fixSQLiteTableCommaTypes(db *gorm.DB, table string) error {
	var rawDDL string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = ? AND name = ?", "table", table).
		Scan(&rawDDL).Error; err != nil {
		return err
	}
	if rawDDL == "" || !sqliteCommaTypeRegexp.MatchString(rawDDL) {
		return nil // 表不存在，或类型里没有逗号：无需处理
	}

	fixedDDL := sqliteCommaTypeRegexp.ReplaceAllString(rawDDL, "real")

	// 把 "CREATE TABLE ..." 后面的表名换成临时表名。RE2 不支持反向引用，两侧引号分别匹配。
	// 覆盖 SQLite 落盘时可能出现的几种写法：裸表名、`t`、`"t"`、[t]、以及带 schema 前缀的 main.t。
	createReg := regexp.MustCompile(`(?i)^\s*CREATE TABLE\s+` +
		`(?:["'\x60\[]\w+["'\x60\]]\s*\.\s*)?` + // 可选 schema 前缀
		`["'\x60\[]?` + regexp.QuoteMeta(table) + `["'\x60\]]?`)
	tmpTable := table + "__fix_tmp"
	newDDL := createReg.ReplaceAllString(fixedDDL, "CREATE TABLE `"+tmpTable+"`")
	if newDDL == fixedDDL {
		// 改写不了就**不要**返回错误：migrateDB 里任何 error 都会让进程 FATAL，
		// 那正是本次要消灭的故障形态。带逗号的类型还在的话，后续 AutoMigrate 大概率
		// 仍然起得来（见下面 default:1 的说明），只是地雷没挖掉——告警即可。
		logger.Log.Warnf("sqlite: 表 %s 存在带逗号的列类型但无法改写其 CREATE TABLE 语句，已跳过修复（服务仍会尝试启动）: %s",
			table, rawDDL)
		return nil
	}

	return db.Transaction(func(tx *gorm.DB) error {
		// 上一轮异常中断可能留下同名临时表
		if err := tx.Exec(fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tmpTable)).Error; err != nil {
			return err
		}
		if err := tx.Exec(newDDL).Error; err != nil {
			return err
		}
		// 新表列序与旧表完全一致（同一份 DDL 只改了类型），可以 SELECT *
		if err := tx.Exec(fmt.Sprintf("INSERT INTO `%s` SELECT * FROM `%s`", tmpTable, table)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf("DROP TABLE `%s`", table)).Error; err != nil {
			return err
		}
		return tx.Exec(fmt.Sprintf("ALTER TABLE `%s` RENAME TO `%s`", tmpTable, table)).Error
	})
}

// sqliteTableNameOf 解析模型对应的表名，避免把表名硬编码成字符串而随模型漂移。
func sqliteTableNameOf(db *gorm.DB, model interface{}) (string, error) {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil {
		return "", err
	}
	return stmt.Table, nil
}
