package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
)

// 本文件覆盖任务 1.2 / 1.3 / 1.4 / 2.1 / 2.2 / 2.3 与 5.2（controller 半边）：
// testChannel 的可选测试能力分派四条路径——命中 Tester 委托、委托失败、未注册适配器降级、
// 未实现 Tester 走通用流程——以及委托路径的「日志恰好一次」与「失败文案不泄露凭证」。
//
// 复用同包 model_catalog_test.go 的 fakeCatalogAdaptor 桩方法（9 个必选方法），
// 只补 TestChannel（可选能力）与可观测的 Init / 通用流程调用计数。临时渠道经真实注册路径
// （adaptor.Register + channeltype.RegisterAPIType）登记，收尾必须注销，避免污染同包其它测试。
//
// 公共库测试一律使用通用临时注册，不引入任何具体渠道名或私有类型号。

// 编译期守卫：两个桩都必须实现必选 Adaptor；fakeTesterAdaptor 额外必须实现可选 Tester。
// fakeGenericFlowAdaptor 刻意不实现 Tester，其「未实现」由用例内运行期前置断言兜底。
var (
	_ adaptor.Adaptor = (*fakeTesterAdaptor)(nil)
	_ adaptor.Tester  = (*fakeTesterAdaptor)(nil)
	_ adaptor.Adaptor = (*fakeGenericFlowAdaptor)(nil)
)

// fakeTesterAdaptor 是一个实现了可选能力 adaptor.Tester 的临时渠道。
// 嵌入 fakeCatalogAdaptor 复用 9 个必选方法桩，仅覆盖 Init（捕获 meta）、TestChannel，
// 并覆盖通用流程三方法以计数：命中委托时它们 MUST 恒为 0（证明未走通用流程）。
type fakeTesterAdaptor struct {
	fakeCatalogAdaptor

	summary string
	testErr error

	initMeta *meta.Meta
	captured *meta.Meta
	calls    int

	convertCalls    int
	doRequestCalls  int
	doResponseCalls int
}

func (f *fakeTesterAdaptor) Init(m *meta.Meta) { f.initMeta = m }

func (f *fakeTesterAdaptor) ConvertRequest(*gin.Context, int, *relaymodel.GeneralOpenAIRequest) (any, error) {
	f.convertCalls++
	return nil, nil
}

func (f *fakeTesterAdaptor) DoRequest(*gin.Context, *meta.Meta, io.Reader) (*http.Response, error) {
	f.doRequestCalls++
	return nil, nil
}

func (f *fakeTesterAdaptor) DoResponse(*gin.Context, *http.Response, *meta.Meta) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	f.doResponseCalls++
	return nil, nil
}

func (f *fakeTesterAdaptor) TestChannel(_ context.Context, m *meta.Meta) (string, error) {
	f.calls++
	f.captured = m
	if f.testErr != nil {
		return "", f.testErr
	}
	return f.summary, nil
}

// fakeGenericFlowAdaptor 是一个**未实现** Tester 的临时渠道：它刻意让通用流程可完整跑通，
// 用于证明「未实现能力的渠道逐字节走既有 ConvertRequest → DoRequest → DoResponse →
// parseTestResponse 流程」，并暴露各步调用计数。
type fakeGenericFlowAdaptor struct {
	fakeCatalogAdaptor

	content string

	convertCalls    int
	doRequestCalls  int
	doResponseCalls int
}

func (f *fakeGenericFlowAdaptor) ConvertRequest(_ *gin.Context, _ int, request *relaymodel.GeneralOpenAIRequest) (any, error) {
	f.convertCalls++
	return request, nil
}

func (f *fakeGenericFlowAdaptor) DoRequest(*gin.Context, *meta.Meta, io.Reader) (*http.Response, error) {
	f.doRequestCalls++
	// 返回 nil 响应：通用流程据此跳过非 200 分支，直接进入 DoResponse。
	return nil, nil
}

func (f *fakeGenericFlowAdaptor) DoResponse(c *gin.Context, _ *http.Response, _ *meta.Meta) (*relaymodel.Usage, *relaymodel.ErrorWithStatusCode) {
	f.doResponseCalls++
	// 按 OpenAI 上游约定写出 choices[0].content，供 parseTestResponse 解析。
	c.JSON(http.StatusOK, gin.H{"choices": []gin.H{{"message": gin.H{"content": f.content}}}})
	return &relaymodel.Usage{}, nil
}

// registerTesterChannel 走真实注册路径登记「渠道类型 → apiType → adaptor」，并登记收尾注销。
func registerTesterChannel(t *testing.T, apiType, channelType int, adp adaptor.Adaptor) {
	t.Helper()
	adaptor.Register(apiType, func() adaptor.Adaptor { return adp })
	channeltype.RegisterAPIType(channelType, apiType)
	t.Cleanup(func() {
		channeltype.UnregisterAPIType(channelType)
		adaptor.Unregister(apiType)
	})
}

// initChannelTestCapabilityLogDB 用临时文件 SQLite 初始化 model.LOG_DB：
// testChannel 的测试日志经真实 model.RecordTestLog 落库，断言「恰好一次」必须落真实 DB。
func initChannelTestCapabilityLogDB(t *testing.T) {
	t.Helper()
	origLogDB := model.LOG_DB
	origSQLite := common.UsingSQLite
	common.UsingSQLite = true
	t.Cleanup(func() {
		model.LOG_DB = origLogDB
		common.UsingSQLite = origSQLite
	})

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "channel-test-capability.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite log db: %v", err)
	}
	if err := db.AutoMigrate(&model.Log{}); err != nil {
		t.Fatalf("migrate log: %v", err)
	}
	model.LOG_DB = db
}

// newTesterChannel 构造一个可直接传入 testChannel 的临时渠道，并把 request.Model 与
// channel.Models 对齐，使 testChannel 的模型名上下文准备后 modelName 保持稳定（便于日志断言）。
// id 用于区分同测试内的多条渠道（testChannel 不要求渠道已落库），保证日志按 channel_id 隔离计数。
func newTesterChannel(id, ct int, name, key, baseURL string) (*model.Channel, *relaymodel.GeneralOpenAIRequest) {
	const modelName = "cap-model"
	ch := &model.Channel{
		Id:      id,
		Type:    ct,
		Name:    name,
		Key:     key,
		Status:  model.ChannelStatusEnabled,
		Group:   "default",
		Models:  modelName,
		BaseURL: &baseURL,
	}
	return ch, &relaymodel.GeneralOpenAIRequest{Model: modelName}
}

// countChannelTestLogs 统计指定渠道已落库的测试日志条数。
func countChannelTestLogs(t *testing.T, channelID int) int64 {
	t.Helper()
	var n int64
	if err := model.LOG_DB.Model(&model.Log{}).Where("channel_id = ?", channelID).Count(&n).Error; err != nil {
		t.Fatalf("count test logs for channel %d: %v", channelID, err)
	}
	return n
}

// waitForChannelTestLogs 轮询等待通用路径的异步日志落库（上限 2s），
// 消除「go model.RecordTestLog」与临时库目录清理的竞争，避免 flaky。
func waitForChannelTestLogs(t *testing.T, channelID int, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countChannelTestLogs(t, channelID) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待渠道 %d 的测试日志超时：want >= %d", channelID, want)
}

// --- (a) 委托命中：实现 Tester 的渠道被委托，返回其摘要，且不走通用流程 ---

func TestChannelCapability_DelegatesToTester(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const (
		extAPIType     = 511
		extChannelType = 1201
		wantSummary    = "渠道专属探针成功：pong"
	)
	base := "https://cap-tester.example.com"
	ch, request := newTesterChannel(2101, extChannelType, "cap-delegate", "sk-delegate-key", base)

	adp := &fakeTesterAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-delegate"},
		summary:            wantSummary,
	}
	registerTesterChannel(t, extAPIType, extChannelType, adp)

	gotMessage, err, openaiErr := testChannel(context.Background(), ch, request)
	if err != nil {
		t.Fatalf("testChannel error = %v, want nil", err)
	}
	if openaiErr != nil {
		t.Fatalf("openaiErr = %v, want nil（委托成功无 openaiErr）", openaiErr)
	}
	if gotMessage != wantSummary {
		t.Errorf("responseMessage = %q, want %q（必须返回渠道摘要）", gotMessage, wantSummary)
	}

	// 委托一次，且 Init 与 TestChannel 拿到同一个 *meta.Meta，各字段与渠道记录一致。
	if adp.calls != 1 {
		t.Fatalf("TestChannel calls = %d, want 1", adp.calls)
	}
	if adp.captured == nil {
		t.Fatal("captured meta is nil")
	}
	if adp.initMeta != adp.captured {
		t.Error("adp.Init(m) 与 TestChannel(ctx, m) 必须拿到同一个 *meta.Meta")
	}
	m := adp.captured
	if m.ChannelType != extChannelType {
		t.Errorf("meta.ChannelType = %d, want %d", m.ChannelType, extChannelType)
	}
	if m.APIType != extAPIType {
		t.Errorf("meta.APIType = %d, want %d", m.APIType, extAPIType)
	}
	if m.BaseURL != base {
		t.Errorf("meta.BaseURL = %q, want %q", m.BaseURL, base)
	}
	if m.APIKey != "sk-delegate-key" {
		t.Errorf("meta.APIKey = %q, want channel key", m.APIKey)
	}

	// 委托路径 MUST NOT 运行通用流程三方法。
	if adp.convertCalls != 0 || adp.doRequestCalls != 0 || adp.doResponseCalls != 0 {
		t.Errorf("委托命中时通用流程被调用：ConvertRequest=%d DoRequest=%d DoResponse=%d, want 0/0/0",
			adp.convertCalls, adp.doRequestCalls, adp.doResponseCalls)
	}
}

// --- (b) 委托失败：返回明确错误、测试结果为失败、openaiErr 为 nil ---

func TestChannelCapability_FailureReturnsErrorAndFailureResult(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const (
		extAPIType     = 512
		extChannelType = 1202
	)
	ch, request := newTesterChannel(2102, extChannelType, "cap-fail", "sk-fail-key", "https://cap-fail.example.com")

	wantErr := errors.New("authentication failed: upstream rejected credentials")
	adp := &fakeTesterAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-fail"},
		testErr:            wantErr,
	}
	registerTesterChannel(t, extAPIType, extChannelType, adp)

	gotMessage, err, openaiErr := testChannel(context.Background(), ch, request)
	if err == nil {
		t.Fatal("err = nil, want non-nil（委托失败必须返回明确失败）")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want 受控错误 %v（必须原样回传渠道失败原因）", err, wantErr)
	}
	if gotMessage != "" {
		t.Errorf("responseMessage = %q, want \"\"（失败不得以合成成功冒充）", gotMessage)
	}
	if openaiErr != nil {
		t.Errorf("openaiErr = %v, want nil（委托失败为受控 err）", openaiErr)
	}
	if adp.calls != 1 {
		t.Errorf("TestChannel calls = %d, want 1", adp.calls)
	}
}

// --- (c) 未注册适配器：不 panic，错误文案与引入本能力前一致 ---

func TestChannelCapability_UnregisteredAdaptorDegradesWithoutPanic(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const extChannelType = 1203
	// 映射到确定未注册的 apiType（unregisteredAPIType 定义于同包 reserved_channel_degradation_test.go）。
	channeltype.RegisterAPIType(extChannelType, unregisteredAPIType)
	t.Cleanup(func() { channeltype.UnregisterAPIType(extChannelType) })

	ch, request := newTesterChannel(2103, extChannelType, "cap-unregistered", "sk-unregistered-key", "https://cap-unreg.example.com")

	// 守卫缺失会在 adaptor.Init / 类型断言上 nil panic，此处即证明安全降级。
	gotMessage, err, openaiErr := testChannel(context.Background(), ch, request)
	if err == nil {
		t.Fatal("err = nil, want invalid api type error")
	}
	wantErrText := "invalid api type: 9999, adaptor is nil"
	if err.Error() != wantErrText {
		t.Errorf("err = %q, want %q（错误文案必须逐字不变）", err.Error(), wantErrText)
	}
	if gotMessage != "" {
		t.Errorf("responseMessage = %q, want \"\"", gotMessage)
	}
	if openaiErr != nil {
		t.Errorf("openaiErr = %v, want nil", openaiErr)
	}
	// 未注册路径在写日志前即返回：不得产生任何测试日志。
	if n := countChannelTestLogs(t, ch.Id); n != 0 {
		t.Errorf("test logs = %d, want 0（未注册路径不写日志）", n)
	}
}

// --- (d) 未实现 Tester：走既有通用流程，行为与引入本能力前等价 ---

func TestChannelCapability_NonTesterUsesGenericFlow(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const (
		extAPIType     = 513
		extChannelType = 1204
		wantContent    = "generic-ok"
	)
	ch, request := newTesterChannel(2104, extChannelType, "cap-generic", "sk-generic-key", "https://cap-generic.example.com")

	adp := &fakeGenericFlowAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-generic"},
		content:            wantContent,
	}
	registerTesterChannel(t, extAPIType, extChannelType, adp)

	// 前置：该适配器确实不实现 Tester，确保本用例证明的是「通用回退」而非委托。
	var asAdaptor adaptor.Adaptor = adp
	if _, ok := asAdaptor.(adaptor.Tester); ok {
		t.Fatal("前置失败：fakeGenericFlowAdaptor 不应实现 Tester")
	}

	gotMessage, err, openaiErr := testChannel(context.Background(), ch, request)
	if err != nil {
		t.Fatalf("testChannel error = %v, want nil", err)
	}
	if openaiErr != nil {
		t.Fatalf("openaiErr = %v, want nil", openaiErr)
	}
	if gotMessage != wantContent {
		t.Errorf("responseMessage = %q, want %q（通用流程经 parseTestResponse 解析）", gotMessage, wantContent)
	}

	// 通用流程四步均被走到：ConvertRequest → DoRequest → DoResponse → parseTestResponse。
	if adp.convertCalls != 1 || adp.doRequestCalls != 1 || adp.doResponseCalls != 1 {
		t.Errorf("通用流程调用计数 = ConvertRequest:%d DoRequest:%d DoResponse:%d, want 1/1/1",
			adp.convertCalls, adp.doRequestCalls, adp.doResponseCalls)
	}

	// 通用路径仍由 controller 统一写恰好一条日志（异步，需轮询等待落库）。
	waitForChannelTestLogs(t, ch.Id, 1)
	if n := countChannelTestLogs(t, ch.Id); n != 1 {
		t.Errorf("test logs = %d, want 1（通用路径恰好一条）", n)
	}
}

// --- (e) 委托路径日志恰好一次，且复用既有日志字段形状 ---

func TestChannelCapability_LogsExactlyOnceOnDelegatedPath(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const (
		extAPIType     = 516
		extChannelType = 1207
		wantSummary    = "探针摘要"
	)
	ch, request := newTesterChannel(2107, extChannelType, "cap-log", "sk-log-key", "https://cap-log.example.com")

	adp := &fakeTesterAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-log"},
		summary:            wantSummary,
	}
	registerTesterChannel(t, extAPIType, extChannelType, adp)

	if _, err, _ := testChannel(context.Background(), ch, request); err != nil {
		t.Fatalf("testChannel error = %v, want nil", err)
	}

	// 委托分支同步落库：无需轮询即可确定断言「恰好一次」。
	if n := countChannelTestLogs(t, ch.Id); n != 1 {
		t.Fatalf("test logs = %d, want 1（委托路径恰好一次）", n)
	}

	var log model.Log
	if err := model.LOG_DB.Where("channel_id = ?", ch.Id).First(&log).Error; err != nil {
		t.Fatalf("load test log: %v", err)
	}
	if log.Type != model.LogTypeTest {
		t.Errorf("log.Type = %d, want %d（测试日志）", log.Type, model.LogTypeTest)
	}
	if log.ModelName != "cap-model" {
		t.Errorf("log.ModelName = %q, want %q", log.ModelName, "cap-model")
	}
	if !strings.Contains(log.Content, "测试成功") || !strings.Contains(log.Content, wantSummary) {
		t.Errorf("log.Content = %q, want 含「测试成功」与摘要", log.Content)
	}
	if log.ResponseBody != wantSummary {
		t.Errorf("log.ResponseBody = %q, want 摘要 %q", log.ResponseBody, wantSummary)
	}
	if log.ElapsedTime < 0 {
		t.Errorf("log.ElapsedTime = %d, want >= 0", log.ElapsedTime)
	}
	if log.ChannelId != ch.Id {
		t.Errorf("log.ChannelId = %d, want %d", log.ChannelId, ch.Id)
	}
}

// --- (f) 委托失败文案不泄露渠道密钥 ---

func TestChannelCapability_FailureTextDoesNotLeakKey(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const (
		extAPIType     = 517
		extChannelType = 1208
	)
	secretKey := "sk-CANARY-LEAK-9f3a"
	ch, request := newTesterChannel(2108, extChannelType, "cap-leak", secretKey, "https://cap-leak.example.com")

	// 渠道实现返回受控、已脱敏的失败文案（真实实现须自行脱敏）。
	adp := &fakeTesterAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-leak"},
		testErr:            errors.New("network error: upstream unreachable"),
	}
	registerTesterChannel(t, extAPIType, extChannelType, adp)

	_, err, _ := testChannel(context.Background(), ch, request)
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	// 证明渠道实现确实拿到了密钥（否则本断言空洞），但回传文案绝不包含它。
	if adp.captured == nil || adp.captured.APIKey != secretKey {
		t.Fatalf("前置失败：渠道实现未拿到渠道密钥")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("错误文案泄露了渠道密钥：%v", err)
	}

	// 失败日志同样不得泄露密钥。
	var log model.Log
	if err := model.LOG_DB.Where("channel_id = ?", ch.Id).First(&log).Error; err != nil {
		t.Fatalf("load failure test log: %v", err)
	}
	if strings.Contains(log.Content, secretKey) {
		t.Errorf("失败日志泄露了渠道密钥：%q", log.Content)
	}
	if strings.Contains(log.RequestBody, secretKey) || strings.Contains(log.ResponseBody, secretKey) {
		t.Errorf("失败日志的请求/响应体泄露了渠道密钥：req=%q resp=%q", log.RequestBody, log.ResponseBody)
	}
	if !strings.Contains(log.Content, "测试失败") {
		t.Errorf("log.Content = %q, want 含「测试失败」", log.Content)
	}
}

// --- 5.2（controller 半边）：四条路径在同一进程内组合，互不干扰 ---

func TestChannelCapability_FourPathsCompose(t *testing.T) {
	initChannelTestCapabilityLogDB(t)

	const (
		testerAPIType      = 514
		testerChannelType  = 1205
		genericAPIType     = 515
		genericChannelType = 1206
		unregChannelType   = 1209
		wantTesterSummary  = "compose-tester-ok"
		wantGeneric        = "compose-generic-ok"
	)

	// 路径 1：实现 Tester 的渠道 → 委托并返回摘要。
	testerAdp := &fakeTesterAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "compose-tester"},
		summary:            wantTesterSummary,
	}
	registerTesterChannel(t, testerAPIType, testerChannelType, testerAdp)
	testerCh, testerReq := newTesterChannel(2201, testerChannelType, "compose-tester", "sk-compose-tester", "https://compose-tester.example.com")
	gotMsg, err, openaiErr := testChannel(context.Background(), testerCh, testerReq)
	if err != nil || openaiErr != nil || gotMsg != wantTesterSummary {
		t.Fatalf("路径1（委托）: msg=%q err=%v openaiErr=%v, want (%q,nil,nil)", gotMsg, err, openaiErr, wantTesterSummary)
	}
	if testerAdp.calls != 1 || testerAdp.convertCalls != 0 {
		t.Errorf("路径1: TestChannel calls=%d convertCalls=%d, want 1/0", testerAdp.calls, testerAdp.convertCalls)
	}
	if n := countChannelTestLogs(t, testerCh.Id); n != 1 {
		t.Errorf("路径1: test logs = %d, want 1", n)
	}

	// 路径 2：未实现 Tester 的渠道 → 走通用流程，结果互不干扰。
	genericAdp := &fakeGenericFlowAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "compose-generic"},
		content:            wantGeneric,
	}
	registerTesterChannel(t, genericAPIType, genericChannelType, genericAdp)
	genericCh, genericReq := newTesterChannel(2202, genericChannelType, "compose-generic", "sk-compose-generic", "https://compose-generic.example.com")
	gotMsg, err, openaiErr = testChannel(context.Background(), genericCh, genericReq)
	if err != nil || openaiErr != nil || gotMsg != wantGeneric {
		t.Fatalf("路径2（通用）: msg=%q err=%v openaiErr=%v, want (%q,nil,nil)", gotMsg, err, openaiErr, wantGeneric)
	}
	if genericAdp.convertCalls != 1 || genericAdp.doRequestCalls != 1 || genericAdp.doResponseCalls != 1 {
		t.Errorf("路径2: 通用流程计数 = %d/%d/%d, want 1/1/1",
			genericAdp.convertCalls, genericAdp.doRequestCalls, genericAdp.doResponseCalls)
	}
	waitForChannelTestLogs(t, genericCh.Id, 1)
	if n := countChannelTestLogs(t, genericCh.Id); n != 1 {
		t.Errorf("路径2: test logs = %d, want 1", n)
	}

	// 路径 3：未注册适配器 → 安全降级，错误文案不变。
	channeltype.RegisterAPIType(unregChannelType, unregisteredAPIType)
	t.Cleanup(func() { channeltype.UnregisterAPIType(unregChannelType) })
	unregCh, unregReq := newTesterChannel(2203, unregChannelType, "compose-unreg", "sk-compose-unreg", "https://compose-unreg.example.com")
	gotMsg, err, openaiErr = testChannel(context.Background(), unregCh, unregReq)
	if err == nil || err.Error() != "invalid api type: 9999, adaptor is nil" {
		t.Fatalf("路径3（未注册）: err = %v, want invalid api type: 9999, adaptor is nil", err)
	}
	if gotMsg != "" || openaiErr != nil {
		t.Errorf("路径3: msg=%q openaiErr=%v, want (\"\",nil)", gotMsg, openaiErr)
	}
	if n := countChannelTestLogs(t, unregCh.Id); n != 0 {
		t.Errorf("路径3: test logs = %d, want 0", n)
	}

	// 路径 4：委托失败 → 明确失败、openaiErr 为 nil、结果互不影响。
	failAdp := &fakeTesterAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "compose-fail"},
		testErr:            errors.New("upstream business error: request rejected"),
	}
	const failAPIType = 518
	const failChannelType = 1210
	registerTesterChannel(t, failAPIType, failChannelType, failAdp)
	failCh, failReq := newTesterChannel(2204, failChannelType, "compose-fail", "sk-compose-fail", "https://compose-fail.example.com")
	gotMsg, err, openaiErr = testChannel(context.Background(), failCh, failReq)
	if err == nil || openaiErr != nil || gotMsg != "" {
		t.Fatalf("路径4（委托失败）: msg=%q err=%v openaiErr=%v, want (\"\",err,nil)", gotMsg, err, openaiErr)
	}
	if n := countChannelTestLogs(t, failCh.Id); n != 1 {
		t.Errorf("路径4: test logs = %d, want 1", n)
	}
}
