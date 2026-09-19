package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/logger"
	. "github.com/smartystreets/goconvey/convey"
)

type loggedEntry struct {
	msg string
	kv  []interface{}
}

// captureLogger 替换全局 logger.Log，仅实现访问日志用到的 Infow，其余方法走 nil 内嵌接口（不应被调用）
type captureLogger struct {
	logger.ILogger

	mu      sync.Mutex
	entries []loggedEntry
}

func (l *captureLogger) Infow(msg string, keysAndValues ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, loggedEntry{msg: msg, kv: keysAndValues})
}

func (l *captureLogger) snapshot() []loggedEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]loggedEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func withCaptureLogger(t *testing.T) *captureLogger {
	t.Helper()
	original := logger.Log
	captured := &captureLogger{}
	logger.Log = captured
	t.Cleanup(func() { logger.Log = original })
	return captured
}

func withSkipPathsEnv(t *testing.T, value string, unset bool) {
	t.Helper()
	original, existed := os.LookupEnv("LOG_SKIP_PATHS")
	t.Cleanup(func() {
		if existed {
			os.Setenv("LOG_SKIP_PATHS", original)
		} else {
			os.Unsetenv("LOG_SKIP_PATHS")
		}
	})
	if unset {
		os.Unsetenv("LOG_SKIP_PATHS")
		return
	}
	os.Setenv("LOG_SKIP_PATHS", value)
}

// withRegisteredSkipPath 模拟扩展方注册：经 RegisterDefaultSkipPath 追加一条默认跳过路径，
// 测试结束还原包级默认名单，避免污染同包其它用例。
func withRegisteredSkipPath(t *testing.T, path string) {
	t.Helper()
	original := defaultSkipPaths
	t.Cleanup(func() { defaultSkipPaths = original })
	RegisterDefaultSkipPath(path)
}

func newLoggedEngine(paths ...string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetUpLogger(engine)
	for _, p := range paths {
		engine.GET(p, func(c *gin.Context) { c.Status(http.StatusOK) })
	}
	return engine
}

func request(engine *gin.Engine, target string) {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
}

func loggedMessages(entries []loggedEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.msg)
	}
	return strings.Join(parts, "\n")
}

func TestSetUpLoggerSkipsRegisteredCallbackPath(t *testing.T) {
	Convey("注册进默认名单的回调路径不落访问日志（防 refreshToken 泄漏）", t, func() {
		captured := withCaptureLogger(t)
		withSkipPathsEnv(t, "", true)
		withRegisteredSkipPath(t, "/api/ext/callback")
		engine := newLoggedEngine("/api/ext/callback")

		request(engine, "/api/ext/callback?code=acme&refreshToken=SECRET-RT&userJwt=SECRET-JWT")

		So(captured.snapshot(), ShouldHaveLength, 0)
		So(loggedMessages(captured.snapshot()), ShouldNotContainSubstring, "refreshToken")
	})
}

func TestSetUpLoggerLogsUnregisteredCallbackPath(t *testing.T) {
	Convey("未注册的回调路径照常落日志（默认名单只含 /api/status）", t, func() {
		captured := withCaptureLogger(t)
		withSkipPathsEnv(t, "", true)
		engine := newLoggedEngine("/api/ext/callback")

		request(engine, "/api/ext/callback?code=acme")

		So(captured.snapshot(), ShouldHaveLength, 1)
	})
}

func TestSetUpLoggerStillLogsOtherPaths(t *testing.T) {
	Convey("普通路径访问日志行为不变（含 query）", t, func() {
		captured := withCaptureLogger(t)
		withSkipPathsEnv(t, "", true)
		engine := newLoggedEngine("/api/channel")

		request(engine, "/api/channel?p=1")

		entries := captured.snapshot()
		So(entries, ShouldHaveLength, 1)
		So(entries[0].msg, ShouldEqual, "/api/channel?p=1")
	})
}

func TestSetUpLoggerDefaultSkipDoesNotCoverSiblingPaths(t *testing.T) {
	Convey("默认 skip 名单的前缀只覆盖 /api/ext/callback，不误伤同族路由", t, func() {
		captured := withCaptureLogger(t)
		withSkipPathsEnv(t, "", true)
		engine := newLoggedEngine("/api/channel/ext/login/start")

		request(engine, "/api/channel/ext/login/start?channelId=7")

		entries := captured.snapshot()
		So(entries, ShouldHaveLength, 1)
		So(entries[0].msg, ShouldEqual, "/api/channel/ext/login/start?channelId=7")
	})
}

func TestGetSkipPathsDefaultAndOverride(t *testing.T) {
	Convey("默认名单只含 /api/status（无渠道注册）", t, func() {
		withSkipPathsEnv(t, "", true)
		So(getSkipPaths(), ShouldResemble, []string{"/api/status"})
		So(DefaultSkipPaths(), ShouldResemble, []string{"/api/status"})
	})
	Convey("RegisterDefaultSkipPath 追加的路径进入默认名单", t, func() {
		withSkipPathsEnv(t, "", true)
		withRegisteredSkipPath(t, "/api/ext/callback")
		So(getSkipPaths(), ShouldResemble, []string{"/api/status", "/api/ext/callback"})
	})
	Convey("显式设置 LOG_SKIP_PATHS 时以显式值为准，不与默认合并", t, func() {
		withSkipPathsEnv(t, "/api/status,/foo", false)
		So(getSkipPaths(), ShouldResemble, []string{"/api/status", "/foo"})
	})
}
