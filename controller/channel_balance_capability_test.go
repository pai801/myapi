package controller

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
)

// 本文件覆盖任务 2.2：updateChannelBalance 的可选能力分派三条路径
// （命中能力 → 委托并落库；未命中能力 → 走既有 switch / 尚未实现；查询失败 → 不落库且返回失败）。
//
// 复用同包 model_catalog_test.go 的 fakeCatalogAdaptor 桩方法（9 个必选方法），
// 只补 QueryBalance（可选能力）与可观测的 Init 捕获。临时渠道经真实注册路径
// （adaptor.Register + channeltype.RegisterAPIType）登记，收尾必须注销，避免污染同包其它测试。

// fakeBalanceQuerierAdaptor 是一个实现了可选能力 adaptor.BalanceQuerier 的临时渠道。
// 嵌入 fakeCatalogAdaptor 复用 9 个必选方法桩，仅覆盖 Init（捕获 meta）与 QueryBalance。
type fakeBalanceQuerierAdaptor struct {
	fakeCatalogAdaptor

	balance  float64
	queryErr error
	initMeta *meta.Meta
	captured *meta.Meta
	calls    int
}

func (f *fakeBalanceQuerierAdaptor) Init(m *meta.Meta) { f.initMeta = m }

func (f *fakeBalanceQuerierAdaptor) QueryBalance(_ context.Context, m *meta.Meta) (float64, error) {
	f.calls++
	f.captured = m
	if f.queryErr != nil {
		return 0, f.queryErr
	}
	return f.balance, nil
}

// registerBalanceQuerierChannel 走真实注册路径登记「渠道类型 → apiType → adaptor」，并登记收尾注销。
func registerBalanceQuerierChannel(t *testing.T, apiType, channelType int, adp adaptor.Adaptor) {
	t.Helper()
	adaptor.Register(apiType, func() adaptor.Adaptor { return adp })
	channeltype.RegisterAPIType(channelType, apiType)
	t.Cleanup(func() {
		channeltype.UnregisterAPIType(channelType)
		adaptor.Unregister(apiType)
	})
}

// initBalanceCapabilityTestDB 用临时文件 SQLite 初始化 model.DB：
// channel.UpdateBalance 是真实落库调用（非 stub），断言「写入 / 不写入」必须落真实 DB。
func initBalanceCapabilityTestDB(t *testing.T) {
	t.Helper()
	origDB := model.DB
	origSQLite := common.UsingSQLite
	common.UsingSQLite = true
	t.Cleanup(func() {
		model.DB = origDB
		common.UsingSQLite = origSQLite
	})

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "balance-capability.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("migrate channel: %v", err)
	}
	model.DB = db
}

func seedBalanceChannel(t *testing.T, ch *model.Channel) {
	t.Helper()
	if err := model.DB.Create(ch).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
}

// --- 路径 1：命中能力 → 委托渠道实现、由 controller 统一落库 ---

func TestUpdateChannelBalance_CapabilityHitDelegatesAndPersists(t *testing.T) {
	initBalanceCapabilityTestDB(t)

	const (
		extAPIType     = 401
		extChannelType = 1101
		wantBalance    = 123.45
	)
	base := "https://balance.example.com"
	ch := &model.Channel{
		Type:    extChannelType,
		Name:    "cap-hit",
		Key:     "sk-hit-key",
		Status:  model.ChannelStatusEnabled,
		Group:   "default",
		BaseURL: &base,
		Config:  `{"region":"cn","custom_key":"custom-value"}`,
	}
	seedBalanceChannel(t, ch)

	adp := &fakeBalanceQuerierAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-hit"},
		balance:            wantBalance,
	}
	registerBalanceQuerierChannel(t, extAPIType, extChannelType, adp)

	got, err := updateChannelBalance(ch)
	if err != nil {
		t.Fatalf("updateChannelBalance error = %v, want nil", err)
	}
	if got != wantBalance {
		t.Errorf("returned balance = %v, want %v", got, wantBalance)
	}

	// 委托前 adp.Init(m)，且 QueryBalance 与 Init 拿到同一个 *meta.Meta，各字段与渠道记录一致。
	if adp.calls != 1 {
		t.Fatalf("QueryBalance calls = %d, want 1", adp.calls)
	}
	if adp.captured == nil {
		t.Fatal("captured meta is nil")
	}
	if adp.initMeta != adp.captured {
		t.Error("adp.Init(m) 与 QueryBalance(ctx, m) 必须拿到同一个 *meta.Meta")
	}
	m := adp.captured
	if m.ChannelType != extChannelType {
		t.Errorf("meta.ChannelType = %d, want %d", m.ChannelType, extChannelType)
	}
	if m.ChannelId != ch.Id {
		t.Errorf("meta.ChannelId = %d, want %d", m.ChannelId, ch.Id)
	}
	if m.APIType != extAPIType {
		t.Errorf("meta.APIType = %d, want %d", m.APIType, extAPIType)
	}
	if m.BaseURL != base {
		t.Errorf("meta.BaseURL = %q, want %q", m.BaseURL, base)
	}
	if m.APIKey != "sk-hit-key" {
		t.Errorf("meta.APIKey = %q, want channel key", m.APIKey)
	}
	if m.Config.Region != "cn" {
		t.Errorf("meta.Config.Region = %q, want cn", m.Config.Region)
	}
	if v, ok := m.Config.Raw["custom_key"]; !ok || v != "custom-value" {
		t.Errorf("meta.Config.Raw[custom_key] = %v, want custom-value（必须含 Raw 全量键）", v)
	}

	// 落库：重读 DB 行，余额与更新时间戳已刷新。
	var stored model.Channel
	if err := model.DB.First(&stored, ch.Id).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	if stored.Balance != wantBalance {
		t.Errorf("stored balance = %v, want %v", stored.Balance, wantBalance)
	}
	if stored.BalanceUpdatedTime == 0 {
		t.Error("stored balance_updated_time = 0, want 刷新后的时间戳")
	}
	// 其它列 MUST 保持不变。
	if stored.Status != model.ChannelStatusEnabled {
		t.Errorf("stored status = %d, want %d（不得改动）", stored.Status, model.ChannelStatusEnabled)
	}
	if stored.Key != "sk-hit-key" {
		t.Errorf("stored key = %q, want unchanged", stored.Key)
	}
	if stored.Config != `{"region":"cn","custom_key":"custom-value"}` {
		t.Errorf("stored config = %q, want unchanged", stored.Config)
	}
	if stored.Name != "cap-hit" {
		t.Errorf("stored name = %q, want unchanged", stored.Name)
	}
}

// --- 路径 1 补充：空 BaseURL 时回落内置默认基址（有效基址显式计算）---

func TestUpdateChannelBalance_CapabilityHitUsesDefaultBaseURL(t *testing.T) {
	initBalanceCapabilityTestDB(t)

	const extAPIType = 402
	// 复用内置 DeepSeek 渠道类型（ChannelBaseURLs 有默认基址），临时把其 apiType 指向实现能力的桩。
	ch := &model.Channel{
		Type:   channeltype.DeepSeek,
		Name:   "cap-default-base",
		Key:    "sk-default-base",
		Status: model.ChannelStatusEnabled,
		Group:  "default",
		// 刻意不设 BaseURL
	}
	seedBalanceChannel(t, ch)

	adp := &fakeBalanceQuerierAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-default-base"},
		balance:            7,
	}
	registerBalanceQuerierChannel(t, extAPIType, channeltype.DeepSeek, adp)

	if _, err := updateChannelBalance(ch); err != nil {
		t.Fatalf("updateChannelBalance error = %v, want nil", err)
	}
	// 能力分支优先于 DeepSeek 的内置 switch：命中桩即证明分派先行。
	if adp.calls != 1 {
		t.Fatalf("QueryBalance calls = %d, want 1（能力分支必须优先于内置 switch）", adp.calls)
	}
	if want := channeltype.ChannelBaseURLs[channeltype.DeepSeek]; adp.captured.BaseURL != want {
		t.Errorf("meta.BaseURL = %q, want 内置默认基址 %q", adp.captured.BaseURL, want)
	}
}

// --- 路径 2：未命中能力 → 既有 switch / 尚未实现，行为不变 ---

func TestUpdateChannelBalance_CapabilityMissFallsBackToLegacySwitch(t *testing.T) {
	t.Run("unregistered adaptor returns 尚未实现", func(t *testing.T) {
		const extChannelType = 1102
		// 映射到确定未注册的 apiType：relay.GetAdaptor 返回 nil，落既有 switch default。
		channeltype.RegisterAPIType(extChannelType, unregisteredAPIType)
		t.Cleanup(func() { channeltype.UnregisterAPIType(extChannelType) })

		ch := &model.Channel{
			Type:   extChannelType,
			Key:    "sk-miss",
			Status: model.ChannelStatusEnabled,
			Group:  "default",
		}
		if _, err := updateChannelBalance(ch); err == nil || err.Error() != "尚未实现" {
			t.Fatalf("err = %v, want 尚未实现", err)
		}
	})

	t.Run("built-in Azure branch unchanged", func(t *testing.T) {
		// 内置 Azure 未实现可选能力，落既有 case Azure → 尚未实现（逐字不变）。
		ch := &model.Channel{
			Type:   channeltype.Azure,
			Key:    "sk-azure",
			Status: model.ChannelStatusEnabled,
			Group:  "default",
		}
		if _, err := updateChannelBalance(ch); err == nil || err.Error() != "尚未实现" {
			t.Fatalf("err = %v, want 尚未实现（内置分支必须不变）", err)
		}
	})
}

// --- 路径 3：查询失败 → 不落库、返回明确失败、文案不含凭证 ---

func TestUpdateChannelBalance_CapabilityFailureDoesNotWrite(t *testing.T) {
	initBalanceCapabilityTestDB(t)

	const (
		extAPIType     = 403
		extChannelType = 1103
	)
	secretKey := "sk-FAILURE-CANARY-7c1d"
	ch := &model.Channel{
		Type:   extChannelType,
		Name:   "cap-fail",
		Key:    secretKey,
		Status: model.ChannelStatusEnabled,
		Group:  "default",
	}
	seedBalanceChannel(t, ch)

	// 渠道实现返回受控失败（真实实现须自行脱敏）。
	adp := &fakeBalanceQuerierAdaptor{
		fakeCatalogAdaptor: fakeCatalogAdaptor{name: "cap-fail"},
		queryErr:           errors.New("balance query failed: upstream returned an error"),
	}
	registerBalanceQuerierChannel(t, extAPIType, extChannelType, adp)

	balance, err := updateChannelBalance(ch)
	if err == nil {
		t.Fatal("err = nil, want non-nil（查询失败必须返回明确失败）")
	}
	if balance != 0 {
		t.Errorf("balance = %v, want 0（失败不得以合成值冒充成功）", balance)
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("错误文案泄露了渠道密钥：%v", err)
	}

	// 不落库：余额与更新时间戳保持原值。
	var stored model.Channel
	if err := model.DB.First(&stored, ch.Id).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	if stored.Balance != 0 {
		t.Errorf("stored balance = %v, want 0（失败 MUST NOT 落库）", stored.Balance)
	}
	if stored.BalanceUpdatedTime != 0 {
		t.Errorf("stored balance_updated_time = %d, want 0（失败 MUST NOT 落库）", stored.BalanceUpdatedTime)
	}
}
