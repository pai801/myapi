package logger

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	DefaultLogStdout     = "console"
	DefaultLogLevel      = "info"
	DefaultLogDirectory  = "./log"
	DefaultLogMaxSize    = 100
	DefaultLogMaxBackups = 3
	DefaultLogMaxAge     = 7
)

type ILogger interface {
	Debugf(format string, args ...interface{})
	Debugw(msg string, keysAndValues ...interface{})
	Infof(format string, args ...interface{})
	Infow(msg string, keysAndValues ...interface{})
	Warnf(format string, args ...interface{})
	Warnw(msg string, keysAndValues ...interface{})
	Errorf(format string, args ...interface{})
	Errorw(msg string, keysAndValues ...interface{})
	DPanicf(format string, args ...interface{})
	DPanicw(msg string, keysAndValues ...interface{})
	Panicf(format string, args ...interface{})
	Panicw(msg string, keysAndValues ...interface{})
	Fatalf(format string, args ...interface{})
	Fatalw(msg string, keysAndValues ...interface{})
	Enable(level string) bool
}

var Log ILogger
var LogHandler http.Handler

func init() {
	// Initialize Log with a fallback logger that writes to stderr.
	// This ensures code that calls Log before SetupLogger (e.g. in tests)
	// does not panic with a nil pointer dereference.
	devCfg := zap.NewDevelopmentConfig()
	devCfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	devCfg.EncoderConfig.TimeKey = "" // omit timestamp in fallback
	sugar, err := devCfg.Build(zap.AddCallerSkip(1))
	if err == nil {
		Log = &impl{SugaredLogger: *sugar.Sugar()}
	}
}

type impl struct {
	zap.SugaredLogger
}

func (l *impl) Enable(level string) bool {
	le, err := zapcore.ParseLevel(level)
	if err != nil {
		return false
	}
	return l.Level().Enabled(le)
}

type LogCfg struct {
	Stdout     string `json:"stdout"`
	Level      string `json:"level"`
	Directory  string `json:"directory"`
	MaxSize    int    `json:"max_size"`
	MaxBackups int    `json:"max_backups"`
	MaxAge     int    `json:"max_age"`
}

func NewLogger(config *LogCfg) (ILogger, error) {
	writeSyncer := getLogWriter(config)
	atomicLevel := zap.NewAtomicLevel()
	level, err := zapcore.ParseLevel(config.Level)
	if err != nil {
		return nil, fmt.Errorf("failed to parse log level: %s", err)
	}
	atomicLevel.SetLevel(level)
	LogHandler = atomicLevel

	core := zapcore.NewCore(encoder(config), writeSyncer, atomicLevel)
	sugar := zap.New(core, zap.AddCaller()).Sugar()

	logger := &impl{SugaredLogger: *sugar}
	logger.Infof("logger initialized")
	Log = logger
	return logger, nil
}

func getLogWriter(config *LogCfg) zapcore.WriteSyncer {
	if config.Stdout == "console" {
		return zapcore.AddSync(os.Stdout)
	}
	if err := os.MkdirAll(config.Directory, 0755); err != nil {
		panic(fmt.Sprintf("failed to create log directory: %s", err))
	}
	return zapcore.AddSync(io.MultiWriter(&lumberjack.Logger{
		Filename:   config.Directory + "/log.log",
		MaxSize:    config.MaxSize, // megabytes
		MaxBackups: config.MaxBackups,
		MaxAge:     config.MaxAge, //days
		LocalTime:  true,
		Compress:   true, // disabled by default
	}))
}

// LogWriter is an io.Writer that forwards each write to Log.Infof.
// Use it to bridge stdlib log consumers (e.g. GORM) into the zap backend.
type LogWriter struct{}

func (w *LogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if msg != "" && Log != nil {
		Log.Infof(msg)
	}
	return len(p), nil
}

func encoder(config *LogCfg) zapcore.Encoder {
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"                                                     // 时间字段改成 timestamp
	encoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05.999Z07:00") // 精确到毫秒，符合 ISO-8601
	encoderConfig.LevelKey = "level"                                                        // 日志等级
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder                                 // INFO / ERROR 大写
	encoderConfig.MessageKey = "message"                                                    // 日志内容字段
	encoderConfig.CallerKey = "stack"                                                       // 显示调用堆栈（默认文件 + 行号）
	if config.Stdout == "console" {
		encoderConfig.EncodeCaller = zapcore.FullCallerEncoder
		return zapcore.NewConsoleEncoder(encoderConfig)
	}
	return zapcore.NewJSONEncoder(encoderConfig)
}
