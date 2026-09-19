package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/apitype"
	"github.com/pai801/myapi/relay/channeltype"
)

// 本文件锁定 PRD §5.13 / AC7：对未注册 adaptor 的渠道类型（本仓不注册任何渠道，
// 故 54/55/56 等价于任意未知类型），读到 channels 表里对应 type 的存量行时，
// 列表 / 详情 / 更新 / 测试等全部代码路径必须「安全降级」——不 panic、不 500，
// 并给出可读的降级结果。
//
// ⚠️ 本文件只验证**降级分支**：controller 测试二进制不含任何渠道注册，故这些 type
// 恒为「未注册」、GetAdaptor 恒为 nil。
//
// 未注册渠道类型常量 unregisteredCT54/55/56 定义于 channel_test.go（同包），此处直接复用。

// initUnregisteredChannelTestDB 走生产 InitDB 路径初始化临时 SQLite 库（与
// initUpdateUserTestDB 同款），使存量行读写落真实 DB 而非内存 stub。
func initUnregisteredChannelTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	common.RedisEnabled = false
	common.UsingSQLite = true
	common.SQLitePath = filepath.Join(t.TempDir(), "myapi-unregistered-test.db")
	model.InitDB()
}

// insertUnregisteredChannel 插入一条 type=ct 的存量渠道行，返回其 id。
func insertUnregisteredChannel(t *testing.T, ct int, status int) int {
	t.Helper()
	ch := model.Channel{
		Type:   ct,
		Name:   fmt.Sprintf("legacy-unregistered-%d", ct),
		Key:    "legacy-key",
		Status: status,
		Group:  "default",
		Models: "gpt-3.5-turbo",
	}
	if err := model.DB.Create(&ch).Error; err != nil {
		t.Fatalf("insert type=%d channel: %v", ct, err)
	}
	return ch.Id
}

// TestUnregisteredChannelListAndDetailDegradeSafely 锁定 AC7 的核心路径：
// 列表（GetAllChannels / SearchChannels）与详情（GetChannel）读到未注册 type 的存量行时，
// 必须 200、返回该行、且不 panic；渠道类型数值原样透传给前端（由前端渲染兜底文案）。
func TestUnregisteredChannelListAndDetailDegradeSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)

	ids := map[int]int{}
	for _, ct := range []int{unregisteredCT54, unregisteredCT55, unregisteredCT56} {
		ids[ct] = insertUnregisteredChannel(t, ct, model.ChannelStatusEnabled)
	}

	// —— 列表 ——
	t.Run("list", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/?p=0", nil)
		GetAllChannels(c)

		if w.Code != http.StatusOK {
			t.Fatalf("GetAllChannels code = %d, want 200", w.Code)
		}
		var resp struct {
			Success bool             `json:"success"`
			Data    []*model.Channel `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal list response: %v (body=%s)", err, w.Body.String())
		}
		if !resp.Success {
			t.Fatalf("GetAllChannels success = false, body=%s", w.Body.String())
		}
		seen := map[int]bool{}
		for _, ch := range resp.Data {
			seen[ch.Type] = true
		}
		for _, ct := range []int{unregisteredCT54, unregisteredCT55, unregisteredCT56} {
			if !seen[ct] {
				t.Errorf("list missing unregistered type %d", ct)
			}
		}
	})

	// —— 搜索 ——
	t.Run("search", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/search?keyword=legacy-unregistered", nil)
		SearchChannels(c)
		if w.Code != http.StatusOK {
			t.Fatalf("SearchChannels code = %d, want 200", w.Code)
		}
	})

	// —— 详情 ——
	for _, ct := range []int{unregisteredCT54, unregisteredCT55, unregisteredCT56} {
		t.Run(fmt.Sprintf("detail-%d", ct), func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(ids[ct])}}
			c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/"+strconv.Itoa(ids[ct]), nil)
			GetChannel(c)

			if w.Code != http.StatusOK {
				t.Fatalf("GetChannel(%d) code = %d, want 200", ct, w.Code)
			}
			var resp struct {
				Success bool           `json:"success"`
				Data    *model.Channel `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal detail response: %v", err)
			}
			if !resp.Success || resp.Data == nil {
				t.Fatalf("GetChannel(%d) success=%v data=%v, want row", ct, resp.Success, resp.Data)
			}
			if resp.Data.Type != ct {
				t.Errorf("GetChannel(%d) type = %d, want %d", ct, resp.Data.Type, ct)
			}
		})
	}
}

// TestUnregisteredChannelUpdateDegradesSafely 锁定更新路径（含 T3a 的凭证压缩分支）：
// 未注册 type 的存量行保存时必须 200、不 panic；无压缩器时回退「按 '\n' 拆分」语义
// （多行 key 不被当成整段凭证，保持既有非压缩渠道行为）。
func TestUnregisteredChannelUpdateDegradesSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)

	for _, ct := range []int{unregisteredCT54, unregisteredCT55, unregisteredCT56} {
		t.Run(fmt.Sprintf("update-%d", ct), func(t *testing.T) {
			id := insertUnregisteredChannel(t, ct, model.ChannelStatusEnabled)

			payload := map[string]any{
				"id":     id,
				"type":   ct,
				"name":   fmt.Sprintf("renamed-%d", ct),
				"key":    "legacy-key",
				"models": "gpt-3.5-turbo",
				"group":  "default",
				"status": model.ChannelStatusEnabled,
			}
			body, _ := json.Marshal(payload)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/channel/", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			UpdateChannel(c)

			if w.Code != http.StatusOK {
				t.Fatalf("UpdateChannel(%d) code = %d, want 200 (body=%s)", ct, w.Code, w.Body.String())
			}
			var resp struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal update response: %v", err)
			}
			if !resp.Success {
				t.Fatalf("UpdateChannel(%d) success = false, body=%s", ct, w.Body.String())
			}
		})
	}
}

// TestUnregisteredChannelUpdateMultiLineKeyNotCompactedWhenUnregistered 断言未注册 type 的
// 更新走「无压缩器」路径：多行 key 按 '\n' 拆分语义处理，而不是被当成整段凭证 JSON。
// 本测试二进制不含任何渠道注册（见文件头），故该 type 恒为「未注册」。
func TestUnregisteredChannelUpdateMultiLineKeyNotCompactedWhenUnregistered(t *testing.T) {
	initUnregisteredChannelTestDB(t)
	insertUnregisteredChannel(t, unregisteredCT54, model.ChannelStatusEnabled)

	// compactChannelKey 是更新/新增共用的压缩接缝：未注册 type 必须回退（ok=false）。
	if got, ok := compactChannelKey(unregisteredCT54, "{\n  \"a\": 1\n}"); ok {
		t.Errorf("compactChannelKey(unregistered %d) = (%q, true), want ok=false (no compressor)", unregisteredCT54, got)
	}
}

// TestUnregisteredChannelBalanceUpdateDegradesSafely 锁定「更新余额」路径：
// channeltype.ChannelBaseURLs 是 map，未登记的 type 返回零值（不 panic）；
// 未注册 type 落到 switch default → 返回「尚未实现」降级错误，不 panic。
func TestUnregisteredChannelBalanceUpdateDegradesSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)
	id := insertUnregisteredChannel(t, unregisteredCT54, model.ChannelStatusEnabled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/update_balance/"+strconv.Itoa(id), nil)
	UpdateChannelBalance(c)

	if w.Code != http.StatusOK {
		t.Fatalf("UpdateChannelBalance code = %d, want 200 (graceful error body)", w.Code)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal balance response: %v", err)
	}
	if resp.Success {
		t.Errorf("UpdateChannelBalance on unregistered type success = true, want false (尚未实现)")
	}
}

// TestUnregisteredChannelDeleteDegradesSafely 锁定删除路径：未注册 type 的存量行可正常删除，
// 不 panic（Delete 不触碰 adaptor）。
func TestUnregisteredChannelDeleteDegradesSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)
	id := insertUnregisteredChannel(t, unregisteredCT56, model.ChannelStatusEnabled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/"+strconv.Itoa(id), nil)
	DeleteChannel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("DeleteChannel code = %d, want 200", w.Code)
	}
	var count int64
	model.DB.Model(&model.Channel{}).Where("id = ?", id).Count(&count)
	if count != 0 {
		t.Errorf("channel %d still present after delete (count=%d)", id, count)
	}
}

// TestUnregisteredChannelDeleteDisabledDegradesSafely 锁定批量清理「删除禁用渠道」路径：
// 被标记禁用的未注册 type 存量行可被正常清理，不 panic。
func TestUnregisteredChannelDeleteDisabledDegradesSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)
	id := insertUnregisteredChannel(t, unregisteredCT55, model.ChannelStatusManuallyDisabled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/disabled", nil)
	DeleteDisabledChannel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("DeleteDisabledChannel code = %d, want 200", w.Code)
	}
	var count int64
	model.DB.Model(&model.Channel{}).Where("id = ?", id).Count(&count)
	if count != 0 {
		t.Errorf("disabled unregistered channel %d still present (count=%d)", id, count)
	}
}

// TestUnregisteredChannelResetDegradesSafely 锁定「重置渠道」路径：读取未注册 type 的存量行
// 并清理其内存冷却状态，不 panic、不触碰 adaptor。
func TestUnregisteredChannelResetDegradesSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)
	id := insertUnregisteredChannel(t, unregisteredCT54, model.ChannelStatusAutoDisabled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/reset/"+strconv.Itoa(id), nil)
	ResetChannel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ResetChannel code = %d, want 200", w.Code)
	}
	var resp struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal reset response: %v", err)
	}
	if !resp.Success {
		t.Fatalf("ResetChannel success = false, body=%s", w.Body.String())
	}
}

// unregisteredAPIType 是一个确定未注册 adaptor 的 apiType：远大于内置枚举范围
// （apitype 内置 0..apitype.Dummy-1），registry 中没有任何登记。
const unregisteredAPIType = 9999

// TestUnregisteredAdaptorLookupIsNil 锁定崩溃防线的原语：未注册的 apiType 经
// relay.GetAdaptor 返回 nil，调用方据此优雅降级。此能力与具体渠道无关，
// 本仓不针对任何具体渠道类型断言。
func TestUnregisteredAdaptorLookupIsNil(t *testing.T) {
	if got := relay.GetAdaptor(unregisteredAPIType); got != nil {
		t.Fatalf("GetAdaptor(%d) = %T, want nil (unregistered apiType)", unregisteredAPIType, got)
	}
	// apitype.Dummy 是首个落在内置枚举之外的 apiType，同样必须为 nil。
	if got := relay.GetAdaptor(apitype.Dummy); got != nil {
		t.Fatalf("GetAdaptor(apitype.Dummy=%d) = %T, want nil", apitype.Dummy, got)
	}
}

// TestUnregisteredAdaptorChannelTestDegradesSafely 走一遍「测试渠道」调用方的降级路径：
// 用 channeltype.RegisterAPIType 把某渠道类型映射到一个未注册 adaptor 的 apiType
// （模拟扩展方登记了渠道类型但其 adaptor 尚未就绪），驱动 controller/channel-test.go
// 的 adaptor==nil 守卫。断言：200、success=false、绝不 panic（守卫缺失会在
// adaptor.Init 上 nil panic）。
func TestUnregisteredAdaptorChannelTestDegradesSafely(t *testing.T) {
	initUnregisteredChannelTestDB(t)

	const extChannelType = 1000 // 远大于内置渠道范围，避免与内置渠道冲突
	channeltype.RegisterAPIType(extChannelType, unregisteredAPIType)
	t.Cleanup(func() { channeltype.UnregisterAPIType(extChannelType) })

	id := insertUnregisteredChannel(t, extChannelType, model.ChannelStatusEnabled)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/test/"+strconv.Itoa(id), nil)
	TestChannel(c)

	if w.Code != http.StatusOK {
		t.Fatalf("TestChannel code = %d, want 200 (graceful error body)", w.Code)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal test-channel response: %v", err)
	}
	if resp.Success {
		t.Errorf("TestChannel on unregistered adaptor success = true, want false (degraded)")
	}

	// TestChannel 内部 `go channel.UpdateResponseTime(...)` 是 fire-and-forget：
	// 等它落库再结束测试，避免与 t.TempDir 清理竞争产生 disk I/O error 噪音日志。
	waitForResponseTimeWrite(t, id)
}

// waitForResponseTimeWrite 轮询等待 UpdateResponseTime 落库（test_time 由 0 变为非 0），
// 上限 2s。仅用于消除异步写库与临时库目录清理的竞争，不改变被测行为。
func waitForResponseTimeWrite(t *testing.T, id int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ch model.Channel
		if err := model.DB.Select("test_time").First(&ch, id).Error; err == nil && ch.TestTime != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBuildChannelId2ModelsSkipsUnregisteredAdaptor 锁定 controller/model.go 遍历循环的
// 崩溃防线（首崩点是 :90 GetChannelName 与 :117 Init(meta)）：当某渠道类型映射到的
// apiType 未注册 adaptor 时，buildChannelId2Models 必须安全跳过而非 nil panic。
//
// 用 channeltype.RegisterAPIType 构造该场景（把内置渠道 2 临时映射到未注册的 apiType），
// 断言该渠道被跳过；守卫缺失会在 adaptor.Init(meta) 上 panic。
func TestBuildChannelId2ModelsSkipsUnregisteredAdaptor(t *testing.T) {
	const mappedChannelType = 2 // API2D：内置映射到 OpenAI，本测试临时改指向未注册 apiType
	channeltype.RegisterAPIType(mappedChannelType, unregisteredAPIType)
	t.Cleanup(func() { channeltype.UnregisterAPIType(mappedChannelType) })

	// 守卫缺失时，此处会在 nil adaptor 的 Init(meta) 上 panic。
	buildChannelId2Models()

	if _, ok := channelId2Models[mappedChannelType]; ok {
		t.Errorf("channelId2Models contains channel type %d whose adaptor is unregistered, want skipped", mappedChannelType)
	}

	// 序列化后不得出现 null（前端把每个渠道的模型清单当数组用）。
	blob, err := json.Marshal(channelId2Models)
	if err != nil {
		t.Fatalf("marshal channelId2Models: %v", err)
	}
	if bytes.Contains(blob, []byte(":null")) {
		t.Errorf("channelId2Models serialized with null value: %s", blob)
	}
}
