package logger

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

var setupLogOnce sync.Once

// SetupLogger initializes the zap-backed global logger.
// Called once at startup. Uses LogDir and config.DebugEnabled to configure output and level.
func SetupLogger() {
	setupLogOnce.Do(func() {
		cfg := &LogCfg{
			Stdout: DefaultLogStdout,
			Level:  DefaultLogLevel,
		}
		if LogDir != "" {
			cfg.Stdout = "file"
		} else {
			cfg.Stdout = "console"
		}
		cfg.Directory = LogDir
		cfg.MaxSize = DefaultLogMaxSize
		cfg.MaxBackups = DefaultLogMaxBackups
		cfg.MaxAge = DefaultLogMaxAge
		if config.DebugEnabled {
			cfg.Level = "debug"
		}
		if _, err := NewLogger(cfg); err != nil {
			log.Fatalf("failed to initialize logger: %v", err)
		}

		// When file logging is configured, also wire Gin error/recovery output
		// and any stray stderr writes to the lumberjack sink so they follow the
		// same log destination.
		if LogDir != "" {
			lj := &lumberjack.Logger{
				Filename:   filepath.Join(LogDir, "access.log"),
				MaxSize:    DefaultLogMaxSize,
				MaxBackups: DefaultLogMaxBackups,
				MaxAge:     DefaultLogMaxAge,
				LocalTime:  true,
				Compress:   true,
			}
			gin.DefaultErrorWriter = io.MultiWriter(os.Stderr, lj)
		}
	})
}
