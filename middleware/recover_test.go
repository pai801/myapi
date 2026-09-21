package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
	. "github.com/smartystreets/goconvey/convey"
)

// panicCaptureLogger 只实现 panic 恢复用到的 Errorf，其余方法走 nil 内嵌接口（不应被调用）
type panicCaptureLogger struct {
	logger.ILogger

	mu      sync.Mutex
	entries []string
}

func (l *panicCaptureLogger) Errorf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprintf(format, args...))
}

func (l *panicCaptureLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.entries, "\n")
}

func withPanicCaptureLogger(t *testing.T) *panicCaptureLogger {
	t.Helper()
	original := logger.Log
	captured := &panicCaptureLogger{}
	logger.Log = captured
	t.Cleanup(func() { logger.Log = original })
	return captured
}

func withPanicLogBodyMax(t *testing.T, value int) {
	t.Helper()
	original := config.PanicLogBodyMaxBytes
	config.PanicLogBodyMaxBytes = value
	t.Cleanup(func() { config.PanicLogBodyMaxBytes = original })
}

func TestFormatRequestBodyForLogShortBody(t *testing.T) {
	Convey("短 body 原样输出", t, func() {
		withPanicLogBodyMax(t, config.PanicLogBodyMaxBytes)
		body := []byte(`{"prompt":"hi"}`)
		So(formatRequestBodyForLog(body), ShouldEqual, `{"prompt":"hi"}`)
	})
}

func TestFormatRequestBodyForLogTruncatesLongBody(t *testing.T) {
	Convey("超长 body 截断且带原始总长度标记", t, func() {
		withPanicLogBodyMax(t, 10)
		body := []byte("0123456789ABCDEFGHIJ") // 20 bytes
		out := formatRequestBodyForLog(body)
		So(out, ShouldEqual, "0123456789...(truncated, total 20 bytes)")
		So(out, ShouldContainSubstring, "total 20 bytes")
	})
}

func TestFormatRequestBodyForLogOmittedWhenDisabled(t *testing.T) {
	Convey("配置为 0 时不输出任何 body 内容，只输出长度", t, func() {
		withPanicLogBodyMax(t, 0)
		body := []byte("SUPER-SECRET-PROMPT")
		out := formatRequestBodyForLog(body)
		So(out, ShouldNotContainSubstring, "SUPER-SECRET-PROMPT")
		So(out, ShouldContainSubstring, "<omitted>")
		So(out, ShouldContainSubstring, fmt.Sprintf("length=%d", len(body)))
	})
}

func TestFormatRequestBodyForLogOmittedWhenNegative(t *testing.T) {
	Convey("配置为负值时同样视为关闭", t, func() {
		withPanicLogBodyMax(t, -1)
		body := []byte("SUPER-SECRET-PROMPT")
		out := formatRequestBodyForLog(body)
		So(out, ShouldNotContainSubstring, "SUPER-SECRET-PROMPT")
		So(out, ShouldContainSubstring, "<omitted>")
	})
}

func TestPanicLogBodyMaxBytesDefault(t *testing.T) {
	Convey("panic 日志请求体上限默认为 256 字节", t, func() {
		So(config.PanicLogBodyMaxBytes, ShouldEqual, 256)
	})
}

func TestFormatRequestBodyForLogEscapesNewlines(t *testing.T) {
	Convey("body 中的 CR/LF 被转义，输出不含真实换行（防 CWE-117 日志注入）", t, func() {
		withPanicLogBodyMax(t, config.PanicLogBodyMaxBytes)
		body := []byte("line1\nline2\r\nline3")
		out := formatRequestBodyForLog(body)
		So(out, ShouldNotContainSubstring, "\n")
		So(out, ShouldNotContainSubstring, "\r")
		So(out, ShouldContainSubstring, `line1\nline2\r\nline3`)
	})
}

func TestFormatRequestBodyForLogEscapesNewlinesInTruncatedBody(t *testing.T) {
	Convey("截断路径同样转义 CR/LF", t, func() {
		withPanicLogBodyMax(t, 8)
		body := []byte("aaaa\nbbbb\ncccc") // 14 bytes
		out := formatRequestBodyForLog(body)
		So(out, ShouldNotContainSubstring, "\n")
		So(out, ShouldContainSubstring, `aaaa\nbbb...(truncated, total 14 bytes)`)
	})
}

func TestFormatRequestBodyForLogTruncatesBeforeEscaping(t *testing.T) {
	Convey("按原始字节截断后再转义，截断位置以原始字节计", t, func() {
		withPanicLogBodyMax(t, 4)
		body := []byte("ab\ncdef") // 7 bytes，前 4 原始字节为 "ab\nc"
		out := formatRequestBodyForLog(body)
		So(out, ShouldEqual, `ab\nc...(truncated, total 7 bytes)`)
	})
}

func TestFormatRequestBodyForLogTruncationKeepsValidUTF8(t *testing.T) {
	Convey("按字节截断多字节字符后仍是合法 UTF-8", t, func() {
		// "中" 占 3 字节，截断到 4 字节会落在第二个字符中间
		withPanicLogBodyMax(t, 4)
		body := []byte("中文测试") // 12 bytes
		out := formatRequestBodyForLog(body)
		So(utf8.ValidString(out), ShouldBeTrue)
		So(out, ShouldContainSubstring, "truncated, total 12 bytes")
	})
}

// newPanicEngine 构造一个只挂载 RelayPanicRecover 的引擎（gin.New 不含默认 Recovery，
// 故若恢复逻辑自身二次 panic，会直接冒泡到 ServeHTTP 调用方，测试可捕获）。
func newPanicEngine(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RelayPanicRecover())
	engine.Any("/boom", handler)
	return engine
}

func TestRelayPanicRecoverNilBodyDoesNotPanic(t *testing.T) {
	Convey("c.Request.Body == nil 时恢复流程不二次 panic，仍返回 500 并记录请求体不可用", t, func() {
		withPanicLogBodyMax(t, config.PanicLogBodyMaxBytes)
		captured := withPanicCaptureLogger(t)

		engine := newPanicEngine(func(c *gin.Context) {
			c.Request.Body = nil
			panic("boom")
		})

		req := httptest.NewRequest(http.MethodGet, "/boom", nil)
		req.Body = nil
		w := httptest.NewRecorder()

		So(func() { engine.ServeHTTP(w, req) }, ShouldNotPanic)
		So(w.Code, ShouldEqual, http.StatusInternalServerError)
		So(captured.joined(), ShouldContainSubstring, "panic detected: boom")
		So(captured.joined(), ShouldContainSubstring, "request: GET /boom")
		So(captured.joined(), ShouldContainSubstring, "request body: <unavailable>")
	})
}

func TestRelayPanicRecoverLogsTruncatedBody(t *testing.T) {
	Convey("恢复流程按上限截断并输出 body，且返回 500", t, func() {
		withPanicLogBodyMax(t, 5)
		captured := withPanicCaptureLogger(t)

		engine := newPanicEngine(func(c *gin.Context) {
			panic("boom")
		})

		req := httptest.NewRequest(http.MethodPost, "/boom", strings.NewReader("0123456789"))
		w := httptest.NewRecorder()

		So(func() { engine.ServeHTTP(w, req) }, ShouldNotPanic)
		So(w.Code, ShouldEqual, http.StatusInternalServerError)
		So(captured.joined(), ShouldContainSubstring, "request body: 01234...(truncated, total 10 bytes)")
	})
}

func TestRelayPanicRecoverOmittedBodyWhenDisabled(t *testing.T) {
	Convey("配置为 0 时恢复流程不输出 body 内容，只输出长度", t, func() {
		withPanicLogBodyMax(t, 0)
		captured := withPanicCaptureLogger(t)

		engine := newPanicEngine(func(c *gin.Context) {
			panic("boom")
		})

		req := httptest.NewRequest(http.MethodPost, "/boom", strings.NewReader("SUPER-SECRET-PROMPT"))
		w := httptest.NewRecorder()

		So(func() { engine.ServeHTTP(w, req) }, ShouldNotPanic)
		So(w.Code, ShouldEqual, http.StatusInternalServerError)
		So(captured.joined(), ShouldNotContainSubstring, "SUPER-SECRET-PROMPT")
		So(captured.joined(), ShouldContainSubstring, "request body: <omitted>, length=19")
	})
}
