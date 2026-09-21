package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor/chatgptsub"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/relaymode"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// recordingLogger 捕获 Infof 输出，其余方法委托给真实 logger。
type recordingLogger struct {
	logger.ILogger
	infos []string
}

func (s *recordingLogger) Infof(format string, args ...interface{}) {
	s.infos = append(s.infos, fmt.Sprintf(format, args...))
}

func captureLogger(t *testing.T) *recordingLogger {
	t.Helper()
	spy := &recordingLogger{ILogger: logger.Log}
	old := logger.Log
	logger.Log = spy
	t.Cleanup(func() { logger.Log = old })
	return spy
}

// TestRecordActualChannel 锁定写入点：仅当 meta.ChannelId 与选路渠道不同才写 ActualChannelId
// 并打一条归因日志；无覆盖 / nil / 非正渠道一律不写。
func TestRecordActualChannel(t *testing.T) {
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(nil)
		c.Set(ctxkey.ChannelId, 100)
		return c
	}

	t.Run("sticky 覆盖时写入 ActualChannelId 并打日志", func(t *testing.T) {
		spy := captureLogger(t)
		c := newCtx()
		recordActualChannel(c, &meta.Meta{ChannelId: 200})
		assert.Equal(t, 200, c.GetInt(ctxkey.ActualChannelId))
		if assert.Len(t, spy.infos, 1) {
			assert.Contains(t, spy.infos[0], "#100")
			assert.Contains(t, spy.infos[0], "#200")
		}
	})

	t.Run("无覆盖时不写键、不打日志", func(t *testing.T) {
		spy := captureLogger(t)
		c := newCtx()
		recordActualChannel(c, &meta.Meta{ChannelId: 100})
		assert.Equal(t, 0, c.GetInt(ctxkey.ActualChannelId))
		assert.Empty(t, spy.infos)
	})

	t.Run("meta 为 nil 不 panic、不写键", func(t *testing.T) {
		c := newCtx()
		assert.NotPanics(t, func() { recordActualChannel(c, nil) })
		assert.Equal(t, 0, c.GetInt(ctxkey.ActualChannelId))
	})

	t.Run("meta.ChannelId<=0 不写键", func(t *testing.T) {
		c := newCtx()
		recordActualChannel(c, &meta.Meta{ChannelId: 0})
		assert.Equal(t, 0, c.GetInt(ctxkey.ActualChannelId))
	})
}

// TestRecordActualChannelName 锁定 P2-1：sticky 覆盖时一并写入 ActualChannelName（实际渠道名 B），
// 无覆盖 / nil / 非正渠道一律不写，保持与 ActualChannelId 对称的 no-op 语义。
func TestRecordActualChannelName(t *testing.T) {
	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(nil)
		c.Set(ctxkey.ChannelId, 100)
		c.Set(ctxkey.ChannelName, "channel-A")
		return c
	}

	t.Run("sticky 覆盖时写入实际渠道名 B", func(t *testing.T) {
		c := newCtx()
		recordActualChannel(c, &meta.Meta{ChannelId: 200, ChannelName: "channel-B"})
		assert.Equal(t, 200, c.GetInt(ctxkey.ActualChannelId))
		assert.Equal(t, "channel-B", c.GetString(ctxkey.ActualChannelName))
	})

	t.Run("无覆盖时不写 ActualChannelName（no-op）", func(t *testing.T) {
		c := newCtx()
		recordActualChannel(c, &meta.Meta{ChannelId: 100, ChannelName: "channel-A"})
		assert.Equal(t, 0, c.GetInt(ctxkey.ActualChannelId))
		assert.Equal(t, "", c.GetString(ctxkey.ActualChannelName))
	})

	t.Run("meta 为 nil 不写 ActualChannelName", func(t *testing.T) {
		c := newCtx()
		assert.NotPanics(t, func() { recordActualChannel(c, nil) })
		assert.Equal(t, "", c.GetString(ctxkey.ActualChannelName))
	})

	t.Run("meta.ChannelId<=0 不写 ActualChannelName", func(t *testing.T) {
		c := newCtx()
		recordActualChannel(c, &meta.Meta{ChannelId: 0, ChannelName: "channel-B"})
		assert.Equal(t, "", c.GetString(ctxkey.ActualChannelName))
	})
}

// stickyAttrTestDBSeq 为每个集成级归因用例生成独立的内存 sqlite 库名，隔离用例间数据。
var stickyAttrTestDBSeq int64

// TestStickyOverrideAttributionEndToEnd 是 advisory-1 要求的「真实来源」集成级用例：
// 它不手工拼 meta，而是用内存 DB 落一个启用渠道 B、令 chatgptsub 的 sticky 真实命中，
// 经真实 SetupRequestHeader 把选路渠道 A 覆盖为 B，再调 recordActualChannel。
// 这是唯一能锁住 chatgptsub sticky 源头行为的用例 —— 若源头漏改 meta.ChannelName（P1 根因），
// 则 m.ChannelName 停留在 A 的名字，本用例变红（见 advisory-1 反向证伪）。
func TestStickyOverrideAttributionEndToEnd(t *testing.T) {
	const (
		selected = 100
		bound    = 200
		group    = "default"
		session  = "sess-sticky-attr-e2e"
	)

	// 内存 DB：仅建 Channel 表并落一个启用渠道 B（名字 B-name）。
	dsn := fmt.Sprintf("file:sticky_attr_e2e_%d?mode=memory&cache=shared", atomic.AddInt64(&stickyAttrTestDBSeq, 1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if sqlDB, dbErr := db.DB(); dbErr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(&dbmodel.Channel{}); err != nil {
		t.Fatalf("migrate channel table: %v", err)
	}
	baseURL := "https://chatgpt.com"
	if err := db.Create(&dbmodel.Channel{
		Id:      bound,
		Name:    "B-name",
		Key:     "bound-key",
		Status:  dbmodel.ChannelStatusEnabled,
		BaseURL: &baseURL,
	}).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	prevDB := dbmodel.DB
	dbmodel.DB = db
	t.Cleanup(func() {
		dbmodel.DB = prevDB
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	// 令 sticky 真实命中：会话 session 绑定到渠道 B（DefaultStickyManager 与 adaptor 内部
	// 使用的 stickyManager 指向同一实例）。
	chatgptsub.DefaultStickyManager.Set(group, session, bound)

	// 起点：选路渠道 A。gin.Context 携带会话头与 A 的 id/name。
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	clientReq := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	clientReq.Header.Set("conversation_id", session)
	c.Request = clientReq
	c.Set(ctxkey.ChannelId, selected)
	c.Set(ctxkey.ChannelName, "A-name")

	m := &meta.Meta{
		Mode:        relaymode.ChatCompletions,
		ChannelId:   selected,
		ChannelName: "A-name",
		Group:       group,
		BaseURL:     "https://chatgpt.com",
		APIKey:      "selected-key",
	}

	upReq, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	a := &chatgptsub.Adaptor{}
	if err := a.SetupRequestHeader(c, upReq, m); err != nil {
		t.Fatalf("SetupRequestHeader: %v", err)
	}

	// 源头必须把 id 与 name 一起改写到 B（P1）。
	assert.Equal(t, bound, m.ChannelId, "sticky 命中后 meta.ChannelId 应为实际渠道 B")
	assert.Equal(t, "B-name", m.ChannelName, "sticky 命中后 meta.ChannelName 应为实际渠道 B 的名字（P1）")

	// 归因落到实际渠道 B 的 id 与名字。
	recordActualChannel(c, m)
	assert.Equal(t, bound, c.GetInt(ctxkey.ActualChannelId), "归因渠道 id 应为实际渠道 B")
	assert.Equal(t, "B-name", c.GetString(ctxkey.ActualChannelName), "归因渠道名应为实际渠道 B 的名字")
}
