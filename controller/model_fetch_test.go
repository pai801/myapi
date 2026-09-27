package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
)

// 本文件覆盖任务 1.2 / 1.3 / 1.4 / 2.1 / 2.2 / 2.3 / 2.4 / 3.1 / 3.2：
// FetchChannelModels 的「渠道能力分派」骨架与参数装配、失败语义，以及通用回退的回归保护。
//
// 复用同包 model_catalog_test.go 的 fakeCatalogAdaptor 桩方法（9 个必选方法），
// 只补 FetchModels（可选能力）与可观测的 Init 捕获。临时渠道经真实注册路径
// （adaptor.Register + channeltype.RegisterAPIType）登记，收尾必须注销，避免污染同包其它测试。

// fakeModelListerAdaptor 是一个实现了可选能力 adaptor.ModelLister 的临时渠道。
// 嵌入 fakeCatalogAdaptor 复用 9 个必选方法桩，仅覆盖 Init（捕获 meta）与 FetchModels。
type fakeModelListerAdaptor struct {
	fakeCatalogAdaptor

	fetchErr error
	initMeta *meta.Meta
	captured *meta.Meta
	calls    int
}

func (f *fakeModelListerAdaptor) Init(m *meta.Meta) { f.initMeta = m }

func (f *fakeModelListerAdaptor) FetchModels(_ context.Context, m *meta.Meta) ([]string, error) {
	f.calls++
	f.captured = m
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.models, nil
}

// registerModelListerChannel 走真实注册路径登记「渠道类型 → apiType → adaptor」，并登记收尾注销。
func registerModelListerChannel(t *testing.T, apiType, channelType int, adp adaptor.Adaptor) {
	t.Helper()
	adaptor.Register(apiType, func() adaptor.Adaptor { return adp })
	channeltype.RegisterAPIType(channelType, apiType)
	t.Cleanup(func() {
		channeltype.UnregisterAPIType(channelType)
		adaptor.Unregister(apiType)
	})
}

// fetchModelsResponse 对应 FetchChannelModels 的响应信封。
type fetchModelsResponse struct {
	Success bool     `json:"success"`
	Message string   `json:"message"`
	Data    []string `json:"data"`
}

// callFetchChannelModels 以给定请求体调用 FetchChannelModels，返回 recorder 与解析后的信封。
func callFetchChannelModels(t *testing.T, payload any) (*httptest.ResponseRecorder, fetchModelsResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	FetchChannelModels(c)

	var resp fetchModelsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}
	return w, resp
}

// newOpenAIListUpstream 起一个只认 GET {base}/v1/models 的上游，返回 OpenAI 列表结构，
// 并记录收到的 Authorization 头，供「通用回退逐字节」断言。
func newOpenAIListUpstream(t *testing.T, modelIDs []string, seenAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if seenAuth != nil {
			*seenAuth = r.Header.Get("Authorization")
		}
		items := make([]map[string]string, 0, len(modelIDs))
		for _, id := range modelIDs {
			items = append(items, map[string]string{"id": id, "object": "model"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- 任务 1.2 / 2.1 / 2.2：委托成功 + meta 装配 + 请求 config（含 Raw）---

func TestFetchChannelModels_DelegatesToModelLister(t *testing.T) {
	const (
		extAPIType     = 301
		extChannelType = 1001
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"ext-m1", "ext-m2"}, name: "ext-lister"},
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
		"base_url":     "https://up.example.com/", // 末尾斜杠应被 TrimRight
		"config":       map[string]any{"region": "cn", "custom_key": "custom-value"},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if !resp.Success {
		t.Fatalf("success = false, body=%s", w.Body.String())
	}
	if fmt.Sprint(resp.Data) != fmt.Sprint([]string{"ext-m1", "ext-m2"}) {
		t.Errorf("data = %v, want [ext-m1 ext-m2]", resp.Data)
	}

	// 任务 2.1：委托前 adp.Init(m)，且 m 各字段与请求一致。
	if adp.calls != 1 {
		t.Fatalf("FetchModels calls = %d, want 1", adp.calls)
	}
	if adp.captured == nil {
		t.Fatal("captured meta is nil")
	}
	if adp.initMeta != adp.captured {
		t.Error("adp.Init(m) 与 FetchModels(ctx, m) 必须拿到同一个 *meta.Meta")
	}
	m := adp.captured
	if m.ChannelType != extChannelType {
		t.Errorf("meta.ChannelType = %d, want %d", m.ChannelType, extChannelType)
	}
	if m.ChannelId != 0 {
		t.Errorf("meta.ChannelId = %d, want 0 (request 未传 channel_id)", m.ChannelId)
	}
	if m.BaseURL != "https://up.example.com" {
		t.Errorf("meta.BaseURL = %q, want trimmed %q", m.BaseURL, "https://up.example.com")
	}
	if m.APIKey != "sk-req-key" {
		t.Errorf("meta.APIKey = %q, want request key", m.APIKey)
	}
	if m.APIType != extAPIType {
		t.Errorf("meta.APIType = %d, want %d", m.APIType, extAPIType)
	}
	// 任务 2.2：请求 config 被解析为 ChannelConfig，且含 typed 字段与 Raw 全量键。
	if m.Config.Region != "cn" {
		t.Errorf("meta.Config.Region = %q, want cn", m.Config.Region)
	}
	if got, ok := m.Config.Raw["custom_key"]; !ok || got != "custom-value" {
		t.Errorf("meta.Config.Raw[custom_key] = %v, want custom-value（必须含 Raw 全量键）", got)
	}
}

// --- 任务 1.3：实现 ModelLister 的渠道空 base_url 仍被委托（不返回 400）---

func TestFetchChannelModels_EmptyBaseURLStillDelegates(t *testing.T) {
	const (
		extAPIType     = 302
		extChannelType = 1002 // 不在 ChannelBaseURLs 中，无默认基址
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"only-model"}, name: "ext-nobase"},
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
		// 刻意不传 base_url
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（实现能力的渠道空 base_url MUST NOT 返回 400）body=%s", w.Code, w.Body.String())
	}
	if !resp.Success || adp.calls != 1 {
		t.Fatalf("success=%v calls=%d, want true/1", resp.Success, adp.calls)
	}
	if adp.captured.BaseURL != "" {
		t.Errorf("meta.BaseURL = %q, want empty（无请求值且无默认基址）", adp.captured.BaseURL)
	}
	if strings.Contains(resp.Message, "base_url is required") {
		t.Errorf("message 不得为缺少基址错误：%q", resp.Message)
	}
}

// --- 任务 1.4：adp == nil（未注册 apiType）安全走通用回退、不 panic ---

func TestFetchChannelModels_NilAdaptorFallsBackSafely(t *testing.T) {
	const extChannelType = 1003
	// 把渠道类型映射到一个确定未注册的 apiType：relay.GetAdaptor 返回 nil。
	channeltype.RegisterAPIType(extChannelType, unregisteredAPIType)
	t.Cleanup(func() { channeltype.UnregisterAPIType(extChannelType) })

	upstream := newOpenAIListUpstream(t, []string{"fallback-model"}, nil)
	origClient := client.HTTPClient
	client.HTTPClient = &http.Client{}
	t.Cleanup(func() { client.HTTPClient = origClient })

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
		"base_url":     upstream.URL,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（nil adaptor 必须安全回退，不 panic）body=%s", w.Code, w.Body.String())
	}
	if !resp.Success || len(resp.Data) != 1 || resp.Data[0] != "fallback-model" {
		t.Fatalf("回退结果异常：success=%v data=%v body=%s", resp.Success, resp.Data, w.Body.String())
	}
}

// --- 任务 2.2：请求不带 config + channel_id → 回落 DB 的 LoadConfig() ---

func TestFetchChannelModels_ConfigFallsBackToChannelRecord(t *testing.T) {
	origDB := model.DB
	origSQLite := common.UsingSQLite
	common.UsingSQLite = true
	t.Cleanup(func() {
		model.DB = origDB
		common.UsingSQLite = origSQLite
	})

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	model.DB = db

	ch := &model.Channel{
		Id:     501,
		Name:   "db-config-channel",
		Key:    "sk-db-key",
		Type:   1,
		Status: model.ChannelStatusEnabled,
		Group:  "default",
		Config: `{"region":"us","db_only_key":"db-value"}`,
	}
	if err := db.Create(ch).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	const (
		extAPIType     = 303
		extChannelType = 1005
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"db-model"}, name: "ext-dbcfg"},
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"channel_id":   501,
		"key":          "sk-req-key", // 提供 key 但**不**提供 config → 仅因取 config 加载 channel
	})

	if w.Code != http.StatusOK || !resp.Success {
		t.Fatalf("status=%d success=%v body=%s", w.Code, resp.Success, w.Body.String())
	}
	m := adp.captured
	if m == nil {
		t.Fatal("captured meta is nil")
	}
	if m.ChannelId != 501 {
		t.Errorf("meta.ChannelId = %d, want 501", m.ChannelId)
	}
	if m.Config.Region != "us" {
		t.Errorf("meta.Config.Region = %q, want us（回落 DB LoadConfig）", m.Config.Region)
	}
	if got, ok := m.Config.Raw["db_only_key"]; !ok || got != "db-value" {
		t.Errorf("meta.Config.Raw[db_only_key] = %v, want db-value（DB 路径同样须含 Raw）", got)
	}
	// 请求已提供 key：不得被 DB key 覆盖。
	if m.APIKey != "sk-req-key" {
		t.Errorf("meta.APIKey = %q, want request key（请求 key 优先）", m.APIKey)
	}
}

// --- 任务 2.3：失败不回退 + 受控错误文案（含渠道名与归类原因）---

func TestFetchChannelModels_ListerFailureDoesNotFallBack(t *testing.T) {
	const (
		extAPIType     = 304
		extChannelType = 1006
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "ext-failing"},
		fetchErr:           errors.New("upstream returned 401 unauthorized"),
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	// 通用回退目标：若发生任何回退出站，本计数器会被触发。
	fallbackHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(upstream.Close)
	origClient := client.HTTPClient
	client.HTTPClient = &http.Client{}
	t.Cleanup(func() { client.HTTPClient = origClient })

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
		"base_url":     upstream.URL,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", w.Code, w.Body.String())
	}
	if resp.Success {
		t.Fatalf("success = true, want false body=%s", w.Body.String())
	}
	if !strings.Contains(resp.Message, "ext-failing") {
		t.Errorf("message = %q, want 含渠道名 ext-failing", resp.Message)
	}
	if !strings.Contains(resp.Message, modelListerReasonAuth) {
		t.Errorf("message = %q, want 含归类原因 %q", resp.Message, modelListerReasonAuth)
	}
	if fallbackHits != 0 {
		t.Errorf("通用回退出站发生 %d 次，want 0（失败 MUST NOT 回退）", fallbackHits)
	}
}

// --- 任务 2.3 / spec R5：上游业务错误信封视为失败（不得当空清单成功）---

func TestFetchChannelModels_UpstreamBusinessEnvelopeIsFailure(t *testing.T) {
	const (
		extAPIType     = 307
		extChannelType = 1011
	)
	// 代表「上游 HTTP 200 + 业务错误信封」（如 {"code":"100002","desc":"缺少 token"}）：
	// 渠道实现负责把它识别为伪成功并转为错误返回（见 modellister.go 失败语义）。
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "ext-biz-envelope"},
		fetchErr:           errors.New("upstream business error: code=100002 desc=缺少 token"),
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（业务错误信封 MUST NOT 被当作成功）body=%s", w.Code, w.Body.String())
	}
	if resp.Success {
		t.Fatalf("success = true, want false（业务错误信封不得冒充成功）body=%s", w.Body.String())
	}
	// 该文本含 "business"，归类应为「上游业务错误」。
	if !strings.Contains(resp.Message, modelListerReasonUpstream) {
		t.Errorf("message = %q, want 含归类原因 %q", resp.Message, modelListerReasonUpstream)
	}
	// 不得被解析为空清单成功：data 不应存在（成功路径才写 data）。
	if len(resp.Data) != 0 {
		t.Errorf("data = %v, want empty（失败不得返回清单）", resp.Data)
	}
}

// --- 任务 2.3：网络错误归类 ---

func TestFetchChannelModels_NetworkErrorClassification(t *testing.T) {
	const (
		extAPIType     = 308
		extChannelType = 1012
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "ext-net-fail"},
		fetchErr:           context.DeadlineExceeded,
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
	})

	if w.Code != http.StatusBadRequest || resp.Success {
		t.Fatalf("status=%d success=%v, want 400/false body=%s", w.Code, resp.Success, w.Body.String())
	}
	if !strings.Contains(resp.Message, modelListerReasonNetwork) {
		t.Errorf("message = %q, want 含归类原因 %q", resp.Message, modelListerReasonNetwork)
	}
}

// --- spec R5：单渠道失败隔离（失败不污染其它渠道）---

func TestFetchChannelModels_FailureIsolationAcrossChannels(t *testing.T) {
	const (
		failAPIType     = 309
		failChannelType = 1013
		okAPIType       = 310
		okChannelType   = 1014
	)
	failing := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "ext-fail-iso"},
		fetchErr:           errors.New("boom"),
	}
	healthy := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"iso-model-a", "iso-model-b"}, name: "ext-ok-iso"},
	}
	registerModelListerChannel(t, failAPIType, failChannelType, failing)
	registerModelListerChannel(t, okAPIType, okChannelType, healthy)

	// 先请求失败渠道 → 400。
	failW, failResp := callFetchChannelModels(t, map[string]any{
		"channel_type": failChannelType,
		"key":          "sk-req-key",
	})
	if failW.Code != http.StatusBadRequest || failResp.Success {
		t.Fatalf("失败渠道 status=%d success=%v, want 400/false", failW.Code, failResp.Success)
	}

	// 再请求正常渠道 → 仍 200 且 data 正确（失败未污染）。
	okW, okResp := callFetchChannelModels(t, map[string]any{
		"channel_type": okChannelType,
		"key":          "sk-req-key",
	})
	if okW.Code != http.StatusOK || !okResp.Success {
		t.Fatalf("正常渠道 status=%d success=%v, want 200/true（单渠道失败必须隔离）body=%s",
			okW.Code, okResp.Success, okW.Body.String())
	}
	if fmt.Sprint(okResp.Data) != fmt.Sprint([]string{"iso-model-a", "iso-model-b"}) {
		t.Errorf("正常渠道 data = %v, want [iso-model-a iso-model-b]", okResp.Data)
	}
}

// --- 任务 2.4：失败响应不泄露凭证 ---

func TestFetchChannelModels_ListerFailureDoesNotLeakCredential(t *testing.T) {
	const (
		extAPIType     = 305
		extChannelType = 1007
		secretKey      = "sk-LEAK-CANARY-9f3a7b21"
	)
	// 渠道错误里**故意**回显 key，验证 controller 不会把 err.Error() 原文回给客户端。
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "ext-leaky"},
		fetchErr:           fmt.Errorf("upstream rejected credential %s", secretKey),
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          secretKey,
	})

	if w.Code != http.StatusBadRequest || resp.Success {
		t.Fatalf("status=%d success=%v, want 400/false body=%s", w.Code, resp.Success, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretKey) {
		t.Errorf("响应体泄露了 key 字面值：%s", w.Body.String())
	}
}

// --- 任务 2.2（已拍板边界）：显式 config: null 视为「已提供」，不回落 DB ---

func TestFetchChannelModels_ExplicitNullConfigDoesNotFallBack(t *testing.T) {
	origDB := model.DB
	origSQLite := common.UsingSQLite
	common.UsingSQLite = true
	t.Cleanup(func() {
		model.DB = origDB
		common.UsingSQLite = origSQLite
	})

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	model.DB = db

	ch := &model.Channel{
		Id: 502, Name: "db-config-null", Key: "sk-db-key", Type: 1,
		Status: model.ChannelStatusEnabled, Group: "default",
		Config: `{"region":"must-not-leak-from-db"}`,
	}
	if err := db.Create(ch).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	const (
		extAPIType     = 306
		extChannelType = 1010
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"m"}, name: "ext-nullcfg"},
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	// 请求体里 config 为字面 null（RawMessage 非 nil）→ 视为已提供 → 得零值，不回落 DB。
	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"channel_id":   502,
		"key":          "sk-req-key",
		"config":       nil,
	})

	if w.Code != http.StatusOK || !resp.Success {
		t.Fatalf("status=%d success=%v body=%s", w.Code, resp.Success, w.Body.String())
	}
	if adp.captured == nil {
		t.Fatal("captured meta is nil")
	}
	if adp.captured.Config.Region != "" {
		t.Errorf("meta.Config.Region = %q, want empty（显式 null 不得回落 DB）", adp.captured.Config.Region)
	}
	if adp.captured.Config.Raw != nil {
		t.Errorf("meta.Config.Raw = %v, want nil（JSON null 的口径）", adp.captured.Config.Raw)
	}
}

// --- M2 advisory 2：请求 config 为非对象/非法 JSON 时安全降级 ---
//
// 说明：json.RawMessage **只能承载合法 JSON**（外层 body 必须先解析成功，否则在
// ShouldBindJSON 处即 400）。故「请求 config 为非法 JSON 字面量」在本 harness 下不可达
// （json.Marshal 会对非法 RawMessage 直接报错）。可达且等价意图的降级路径是：config 为
// **合法 JSON 但非对象**（如 JSON 字符串 `"config":"not-a-json"`）——LoadConfig 对它
// 解析失败，须安全降级为零值、不 panic、请求照常成功。
func TestFetchChannelModels_NonObjectRequestConfigDegradesSafely(t *testing.T) {
	const (
		extAPIType     = 311
		extChannelType = 1015
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"degrade-model"}, name: "ext-badcfg"},
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	// config 为 JSON 字符串（合法 JSON、非对象）：RawMessage 非 nil（视为已提供），
	// 但 LoadConfig 解析失败 → 零值降级。
	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
		"config":       "not-a-json",
	})

	if w.Code != http.StatusOK || !resp.Success {
		t.Fatalf("status=%d success=%v, want 200/true（非对象 config 必须安全降级）body=%s",
			w.Code, resp.Success, w.Body.String())
	}
	if adp.captured == nil {
		t.Fatal("captured meta is nil")
	}
	cfg := adp.captured.Config
	if cfg.Region != "" || cfg.SK != "" || cfg.AK != "" || cfg.APIVersion != "" || cfg.Raw != nil {
		t.Errorf("meta.Config = %+v, want 零值（解析失败须降级）", cfg)
	}
	if fmt.Sprint(resp.Data) != fmt.Sprint([]string{"degrade-model"}) {
		t.Errorf("data = %v, want [degrade-model]", resp.Data)
	}
}

// --- M2 advisory 2：DB 中 channel config 为非法 JSON 时安全降级 ---

func TestFetchChannelModels_InvalidDBConfigDegradesSafely(t *testing.T) {
	origDB := model.DB
	origSQLite := common.UsingSQLite
	common.UsingSQLite = true
	t.Cleanup(func() {
		model.DB = origDB
		common.UsingSQLite = origSQLite
	})

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	model.DB = db

	// DB 里的 config 是**非法 JSON**：LoadConfig 会返回错误，须降级为零值。
	ch := &model.Channel{
		Id: 503, Name: "db-bad-config", Key: "sk-db-key", Type: 1,
		Status: model.ChannelStatusEnabled, Group: "default",
		Config: `{invalid`,
	}
	if err := db.Create(ch).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	const (
		extAPIType     = 312
		extChannelType = 1016
	)
	adp := &fakeModelListerAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{models: []string{"ok-after-degrade"}, name: "ext-dbbad"},
	}
	registerModelListerChannel(t, extAPIType, extChannelType, adp)

	// 带 channel_id、不带 config → 回落 DB LoadConfig（此处 DB config 非法）。
	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"channel_id":   503,
		"key":          "sk-req-key",
	})

	if w.Code != http.StatusOK || !resp.Success {
		t.Fatalf("status=%d success=%v, want 200/true（DB config 非法必须安全降级）body=%s",
			w.Code, resp.Success, w.Body.String())
	}
	if adp.captured == nil {
		t.Fatal("captured meta is nil")
	}
	cfg := adp.captured.Config
	if cfg.Region != "" || cfg.SK != "" || cfg.AK != "" || cfg.APIVersion != "" || cfg.Raw != nil {
		t.Errorf("meta.Config = %+v, want 零值（DB LoadConfig 失败须降级）", cfg)
	}
}

// --- 任务 3.1：未实现 ModelLister 的渠道走通用回退（Bearer + {base}/v1/models）---

func TestFetchChannelModels_GenericFallbackUnchanged(t *testing.T) {
	const extChannelType = 1008
	// 映射到内置 OpenAI apiType：该 adaptor 未实现 ModelLister → 通用回退。
	channeltype.RegisterAPIType(extChannelType, 0 /* apitype.OpenAI */)
	t.Cleanup(func() { channeltype.UnregisterAPIType(extChannelType) })

	var seenAuth string
	upstream := newOpenAIListUpstream(t, []string{"openai-a", "openai-b"}, &seenAuth)
	origClient := client.HTTPClient
	client.HTTPClient = &http.Client{}
	t.Cleanup(func() { client.HTTPClient = origClient })

	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-generic",
		"base_url":     upstream.URL,
	})

	if w.Code != http.StatusOK || !resp.Success {
		t.Fatalf("status=%d success=%v body=%s", w.Code, resp.Success, w.Body.String())
	}
	if fmt.Sprint(resp.Data) != fmt.Sprint([]string{"openai-a", "openai-b"}) {
		t.Errorf("data = %v, want [openai-a openai-b]", resp.Data)
	}
	if seenAuth != "Bearer sk-generic" {
		t.Errorf("上游 Authorization = %q, want %q（通用回退须逐字节保持）", seenAuth, "Bearer sk-generic")
	}
}

// --- 任务 3.2：无 base_url 且无默认基址 → 「缺少基址」400（仅约束通用回退）---

func TestFetchChannelModels_MissingBaseURLReturns400(t *testing.T) {
	const extChannelType = 1009 // 未注册：ToAPIType 回退 OpenAI（无 ModelLister），无默认基址
	w, resp := callFetchChannelModels(t, map[string]any{
		"channel_type": extChannelType,
		"key":          "sk-req-key",
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", w.Code, w.Body.String())
	}
	if resp.Success {
		t.Fatalf("success = true, want false body=%s", w.Body.String())
	}
	if !strings.Contains(resp.Message, "base_url is required") {
		t.Errorf("message = %q, want 缺少基址错误", resp.Message)
	}
}
