package main

import (
	"embed"
	"os"
	"strconv"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	_ "github.com/joho/godotenv/autoload"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/i18n"
	"github.com/pai801/myapi/common/logcleanup"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/controller"
	"github.com/pai801/myapi/middleware"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/active"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/router"
)

//go:embed web/build/*
var buildFS embed.FS

func main() {
	common.Init()
	logger.SetupLogger()
	logger.Log.Infof("MyAPI %s started", common.Version)

	if os.Getenv("GIN_MODE") != gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}
	if config.DebugEnabled {
		logger.Log.Infof("running in debug mode")
	}

	// Initialize SQL Database
	model.InitDB()
	model.InitLogDB()
	logcleanup.Start()

	var err error
	err = model.CreateRootAccountIfNeed()
	if err != nil {
		logger.Log.Fatalf("database init error: " + err.Error())
	}
	defer func() {
		err := model.CloseDB()
		if err != nil {
			logger.Log.Fatalf("failed to close database: " + err.Error())
		}
	}()

	// Initialize Redis
	err = common.InitRedisClient()
	if err != nil {
		logger.Log.Fatalf("failed to initialize Redis: " + err.Error())
	}

	// Initialize options
	model.InitOptionMap()
	if common.RedisEnabled {
		// for compatibility with old versions
		config.MemoryCacheEnabled = true
	}
	if config.MemoryCacheEnabled {
		logger.Log.Infof("memory cache enabled")
		logger.Log.Infof("sync frequency: %d seconds", config.SyncFrequency)
		model.InitChannelCache()
	}
	if config.MemoryCacheEnabled {
		go model.SyncOptions(config.SyncFrequency)
		go model.SyncChannelCache(config.SyncFrequency)
	}
	if os.Getenv("CHANNEL_TEST_FREQUENCY") != "" {
		frequency, err := strconv.Atoi(os.Getenv("CHANNEL_TEST_FREQUENCY"))
		if err != nil {
			logger.Log.Fatalf("failed to parse CHANNEL_TEST_FREQUENCY: " + err.Error())
		}
		go controller.AutomaticallyTestChannels(frequency)
	}
	if os.Getenv("BATCH_UPDATE_ENABLED") == "true" {
		config.BatchUpdateEnabled = true
		logger.Log.Infof("batch update enabled with interval " + strconv.Itoa(config.BatchUpdateInterval) + "s")
		model.InitBatchUpdater()
	}
	// 启动活跃请求 TTL 清理：每 30 秒清理过期条目
	// TTL 与 RELAY_TIMEOUT 联动：有超时设置时 TTL = 超时 + 2 分钟宽限期
	// RELAY_TIMEOUT=0（无超时）时使用 30 分钟兜底 TTL
	cleanupTTL := 30 * time.Minute
	if config.RelayTimeout > 0 {
		cleanupTTL = time.Duration(config.RelayTimeout)*time.Second + 2*time.Minute
	}
	active.StartCleanupLoop(30*time.Second, cleanupTTL)
	if config.EnableMetric {
		logger.Log.Infof("metric enabled, will disable channel if too much request failed")
	}
	openai.InitTokenEncoders()
	client.Init()

	// Initialize i18n
	if err := i18n.Init(); err != nil {
		logger.Log.Fatalf("failed to initialize i18n: " + err.Error())
	}

	// Initialize HTTP server
	server := gin.New()
	server.Use(gin.Recovery())
	// This will cause SSE not to work!!!
	//server.Use(gzip.Gzip(gzip.DefaultCompression))
	server.Use(middleware.RequestId())
	server.Use(middleware.Language())
	middleware.SetUpLogger(server)
	// Initialize session store
	store := cookie.NewStore([]byte(config.SessionSecret))
	server.Use(sessions.Sessions("session", store))

	router.SetRouter(server, buildFS)
	var port = os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}
	logger.Log.Infof("server started on http://localhost:%s", port)
	err = server.Run(":" + port)
	if err != nil {
		logger.Log.Fatalf("failed to start HTTP server: " + err.Error())
	}
}
