package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/meta"
)

func TestTTFTWriter_StreamingRecordsFirstTokenTime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// 模拟请求开始于 100ms 前
	m := &meta.Meta{IsStream: true, StartTime: time.Now().Add(-100 * time.Millisecond)}
	wrapTTFTWriter(c, m)

	if _, err := c.Writer.Write([]byte("data: {\"id\":\"1\"}\n")); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	ttft := c.GetInt64(ctxkey.FirstTokenTime)
	if ttft <= 0 {
		t.Fatalf("expected positive TTFT recorded, got %d", ttft)
	}
	if ttft < 50 || ttft > 500 {
		t.Fatalf("expected TTFT around 100ms, got %d", ttft)
	}
}

func TestTTFTWriter_NonStreamingKeepsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	m := &meta.Meta{IsStream: false, StartTime: time.Now().Add(-100 * time.Millisecond)}
	wrapTTFTWriter(c, m)

	if _, err := c.Writer.Write([]byte(`{"id":"1"}`)); err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	if ttft := c.GetInt64(ctxkey.FirstTokenTime); ttft != 0 {
		t.Fatalf("expected no TTFT for non-streaming, got %d", ttft)
	}
}

func TestTTFTWriter_FirstWriteOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	m := &meta.Meta{IsStream: true, StartTime: time.Now().Add(-50 * time.Millisecond)}
	wrapTTFTWriter(c, m)

	first, err := c.Writer.Write([]byte("event: response.created\n"))
	if err != nil || first == 0 {
		t.Fatalf("first write failed: n=%d err=%v", first, err)
	}
	firstTTFT := c.GetInt64(ctxkey.FirstTokenTime)
	if firstTTFT <= 0 {
		t.Fatalf("expected TTFT after first write, got %d", firstTTFT)
	}

	// 模拟 200ms 后的第二次写，TTFT 不应被覆盖
	time.Sleep(50 * time.Millisecond)
	if _, err := c.Writer.Write([]byte("data: {\"type\":\"response.output_text.delta\"}\n")); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	secondTTFT := c.GetInt64(ctxkey.FirstTokenTime)
	if secondTTFT != firstTTFT {
		t.Fatalf("expected TTFT to stay at first-write value, got %d -> %d", firstTTFT, secondTTFT)
	}
}

func TestGetFirstTokenTime_DefaultZero(t *testing.T) {
	if got := getFirstTokenTime(context.Background()); got != 0 {
		t.Fatalf("expected 0 from empty ctx, got %d", got)
	}
	ctx := context.WithValue(context.Background(), CtxKeyFirstTokenTime, int64(123))
	if got := getFirstTokenTime(ctx); got != 123 {
		t.Fatalf("expected 123 from ctx, got %d", got)
	}
}
