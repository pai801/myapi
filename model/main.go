package model

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/env"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/random"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	glog "gorm.io/gorm/logger"
)

var DB *gorm.DB
var LOG_DB *gorm.DB

func CreateRootAccountIfNeed() error {
	var user User
	//if user.Status != util.UserStatusEnabled {
	if err := DB.First(&user).Error; err != nil {
		logger.Log.Infof("no user exists, creating a root user for you: username is root, password is 123456")
		hashedPassword, err := common.Password2Hash("123456")
		if err != nil {
			return err
		}
		accessToken := random.GetUUID()
		if config.InitialRootAccessToken != "" {
			accessToken = config.InitialRootAccessToken
		}
		rootUser := User{
			Username:    "root",
			Password:    hashedPassword,
			Role:        RoleRootUser,
			Status:      UserStatusEnabled,
			DisplayName: "Root User",
			AccessToken: accessToken,
			Quota:       500000000000000,
		}
		DB.Create(&rootUser)
		if config.InitialRootToken != "" {
			logger.Log.Infof("creating initial root token as requested")
			token := Token{
				Id:           1,
				UserId:       rootUser.Id,
				Key:          config.InitialRootToken,
				Status:       TokenStatusEnabled,
				Name:         "Initial Root Token",
				CreatedTime:  helper.GetTimestamp(),
				AccessedTime: helper.GetTimestamp(),
			}
			DB.Create(&token)
		}
	}
	return nil
}

func chooseDB(envName string) (*gorm.DB, error) {
	dsn := os.Getenv(envName)

	switch {
	case strings.HasPrefix(dsn, "postgresql://"):
		// Use PostgreSQL
		return openPostgreSQL(dsn)
	case dsn != "":
		// Use MySQL
		return openMySQL(dsn)
	default:
		// Use SQLite
		return openSQLite()
	}
}

func openPostgreSQL(dsn string) (*gorm.DB, error) {
	logger.Log.Infof("using PostgreSQL as database")
	newLogger := glog.New(
		log.New(&logger.LogWriter{}, "\r\n", log.LstdFlags),
		glog.Config{
			SlowThreshold:             15 * time.Second, // 慢 SQL 阈值改为 5s
			LogLevel:                  glog.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  true,
		},
	)
	common.UsingPostgreSQL = true
	return gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true, // disables implicit prepared statement usage
	}), &gorm.Config{
		PrepareStmt: true, // precompile SQL
		Logger:      newLogger,
	})
}

func openMySQL(dsn string) (*gorm.DB, error) {
	logger.Log.Infof("using MySQL as database")
	common.UsingMySQL = true
	return gorm.Open(mysql.Open(dsn), &gorm.Config{
		PrepareStmt: true, // precompile SQL
	})
}

func openSQLite() (*gorm.DB, error) {
	logger.Log.Infof("SQL_DSN not set, using SQLite as database")
	common.UsingSQLite = true
	dsn := fmt.Sprintf("%s?_busy_timeout=%d", common.SQLitePath, common.SQLiteBusyTimeout)
	return gorm.Open(sqlite.Open(dsn), &gorm.Config{
		PrepareStmt: true, // precompile SQL
	})
}

func InitDB() {
	var err error
	DB, err = chooseDB("SQL_DSN")
	if err != nil {
		logger.Log.Fatalf("failed to initialize database: " + err.Error())
		return
	}

	sqlDB := setDBConns(DB)

	if common.UsingMySQL {
		_, _ = sqlDB.Exec("DROP INDEX idx_channels_key ON channels;") // TODO: delete this line when most users have upgraded
	}

	logger.Log.Infof("database migration started")
	if err = migrateDB(); err != nil {
		logger.Log.Fatalf("failed to migrate database: " + err.Error())
		return
	}
	logger.Log.Infof("database migrated")
}

func migrateDB() error {
	var err error
	if err = DB.AutoMigrate(&Channel{}); err != nil {
		return err
	}
	if err = DB.AutoMigrate(&Token{}); err != nil {
		return err
	}
	// 清理已废弃的列
	tokenColsToDrop := []string{"expired_time", "remain_quota", "unlimited_quota", "used_quota"}
	for _, col := range tokenColsToDrop {
		if DB.Migrator().HasColumn(&Token{}, col) {
			if err = DB.Migrator().DropColumn(&Token{}, col); err != nil {
				return fmt.Errorf("failed to drop column %s: %w", col, err)
			}
		}
	}
	// 清理遗留的过期/耗尽 token 状态，重置为 disabled
	DB.Model(&Token{}).Where("status NOT IN (1, 2)").Update("status", TokenStatusDisabled)
	if err = DB.AutoMigrate(&User{}); err != nil {
		return err
	}
	if err = DB.AutoMigrate(&Option{}); err != nil {
		return err
	}
	if err = DB.AutoMigrate(&Ability{}); err != nil {
		return err
	}
	if err = DB.AutoMigrate(&Log{}); err != nil {
		return err
	}
	if err = DB.AutoMigrate(&ModelMetadata{}); err != nil {
		return err
	}
	return nil
}

func InitLogDB() {
	if os.Getenv("LOG_SQL_DSN") == "" {
		LOG_DB = DB
		return
	}

	logger.Log.Infof("using secondary database for table logs")
	var err error
	LOG_DB, err = chooseDB("LOG_SQL_DSN")
	if err != nil {
		logger.Log.Fatalf("failed to initialize secondary database: " + err.Error())
		return
	}

	setDBConns(LOG_DB)

	logger.Log.Infof("secondary database migration started")
	err = migrateLOGDB()
	if err != nil {
		logger.Log.Fatalf("failed to migrate secondary database: " + err.Error())
		return
	}
	logger.Log.Infof("secondary database migrated")
}

func migrateLOGDB() error {
	var err error
	if err = LOG_DB.AutoMigrate(&Log{}); err != nil {
		return err
	}
	return nil
}

func setDBConns(db *gorm.DB) *sql.DB {
	if config.DebugSQLEnabled {
		db = db.Debug()
	}

	sqlDB, err := db.DB()
	if err != nil {
		logger.Log.Fatalf("failed to connect database: " + err.Error())
		return nil
	}

	sqlDB.SetMaxIdleConns(env.Int("SQL_MAX_IDLE_CONNS", 100))
	sqlDB.SetMaxOpenConns(env.Int("SQL_MAX_OPEN_CONNS", 1000))
	sqlDB.SetConnMaxLifetime(time.Second * time.Duration(env.Int("SQL_MAX_LIFETIME", 60)))
	return sqlDB
}

func closeDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	err = sqlDB.Close()
	return err
}

func CloseDB() error {
	if LOG_DB != DB {
		err := closeDB(LOG_DB)
		if err != nil {
			return err
		}
	}
	return closeDB(DB)
}
