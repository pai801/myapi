package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/middleware"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/channeltype"
)

// 本文件是「需求 B：模型清单仅作展示与候选」的**回归守卫**（任务 5.2）。
//
// 契约：渠道的默认模型清单（adaptor.GetModelList 及其下发的展示清单 channelId2Models / modelsMap）
// 仅用于界面展示与候选，MUST NOT 成为出站请求的模型准入白名单。一个**不在任何展示清单内**的
// 模型名，在真实出站路径上 MUST NOT 被本地以「不在清单内」为由拒绝——模型合法性由上游裁决。
//
// 反向意义（务必知悉，这是本测试存在的全部理由）：
//
//	若未来有人把「模型必须在 GetModelList() / channelId2Models 内」这类本地白名单校验引入出站
//	路径（middleware.TokenAuth / Distribute / controller.Relay 或 adaptor），使不在展示清单内的
//	模型被本地拒绝，本测试必红（表现为：出站不再到达 httptest 上游，或返回 403/本地错误）。
//	删除契约 = 本测试变红，从而把「禁止耦合」从口头约定变成可执行的防线。
//
// 覆盖强度说明（避免高估）：
//   - 唯一与展示清单无关的**模型准入**在 middleware/auth.go 的 token 级 token.Models 限制
//     （isModelInList）——那是**令牌**级白名单，与渠道展示清单无关。本测试刻意使用
//     token.Models == nil 的令牌，以隔离出「渠道展示清单不参与出站准入」这一命题。
//   - 出站链路走真实生产中间件链：RelayPanicRecover → TokenAuth → TokenModelMapping → Distribute
//     → controller.Relay → adaptor.DoRequest，最终打到一个受控 httptest 上游。

// arbitraryProbeModel 是一个**刻意不在任何展示清单内**的模型名。它足够独特，不会与内置
// adaptor 的 GetModelList / CompatibleChannels 清单撞名。
const arbitraryProbeModel = "guard-probe-model-not-in-display-list-9x7"

// guardUpstream 记录最近一次收到的请求体，并按 OpenAI 非流式结构返回。
type guardUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   string
}

func newGuardUpstream() *guardUpstream {
	u := &guardUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.seen = string(raw)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"guard","object":"chat.completion","model":"` + arbitraryProbeModel +
			`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	return u
}

func (u *guardUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen
}

// TestOutboundNotRejectedByChannelDisplayList 证明：不在渠道展示清单内的模型名，在真实出站
// 路径上不会被本地白名单拒绝，且请求确实到达上游。（反向意义见文件头注释。）
func TestOutboundNotRejectedByDisplayList(t *testing.T) {
	// —— 前置：模型确实在任何展示清单之外（否则本测试证明不了任何东西）——
	ensureModelCatalog()
	for ct, list := range channelId2Models {
		for _, id := range list {
			if id == arbitraryProbeModel {
				t.Fatalf("前置失败：%q 出现在 channelId2Models[%d] 中，它本应在展示清单之外", arbitraryProbeModel, ct)
			}
		}
	}
	if _, ok := modelsMap[arbitraryProbeModel]; ok {
		t.Fatalf("前置失败：%q 出现在 modelsMap 中，它本应在展示清单之外", arbitraryProbeModel)
	}

	// —— 全局状态隔离 ——
	origDB := model.DB
	origLogDB := model.LOG_DB
	origSQLite := common.UsingSQLite
	origRedis := common.RedisEnabled
	origMem := config.MemoryCacheEnabled
	origApprox := config.ApproximateTokenEnabled
	origClient := client.HTTPClient
	common.UsingSQLite = true
	common.RedisEnabled = false
	config.MemoryCacheEnabled = false // SelectChannel 直查 DB，不污染全局渠道缓存
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() {
		model.DB = origDB
		model.LOG_DB = origLogDB
		common.UsingSQLite = origSQLite
		common.RedisEnabled = origRedis
		config.MemoryCacheEnabled = origMem
		config.ApproximateTokenEnabled = origApprox
		client.HTTPClient = origClient
	})

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "guard.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	model.DB = db
	model.LOG_DB = db

	// 受控上游：真实 adaptor.DoRequest 会打到它。
	upstream := newGuardUpstream()
	t.Cleanup(upstream.server.Close)
	client.HTTPClient = &http.Client{}

	// —— 播种：启用用户 + 无模型限制的令牌 + 支持任意模型名的渠道 ——
	const (
		guardUserID = 880101
		guardKey    = "guardtestkey1" // 不含 '-'，避免 token 后缀解析出指定渠道
		guardChanID = 880201
	)
	user := &model.User{
		Id: guardUserID, Username: "guard-user", Password: "guard-pass",
		DisplayName: "guard", Role: model.RoleCommonUser, Status: model.UserStatusEnabled,
		Quota: 1_000_000_000_000, AccessToken: "guard-access-token",
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// token.Models == nil：刻意不设令牌级模型白名单，隔离出「渠道展示清单不参与出站准入」。
	token := &model.Token{
		Id: 880301, UserId: guardUserID, Key: guardKey, Status: model.TokenStatusEnabled,
		Name: "guard", CreatedTime: time.Now().Unix(), Group: "default",
	}
	if err := db.Create(token).Error; err != nil {
		t.Fatalf("seed token: %v", err)
	}
	base := upstream.server.URL
	ch := &model.Channel{
		Id: guardChanID, Type: channeltype.OpenAI, Key: "sk-upstream-guard",
		Status: model.ChannelStatusEnabled, Name: "guard-channel",
		Models: arbitraryProbeModel, // 路由能力清单（channel.Models）与展示清单是两回事
		Group:  "default", BaseURL: &base,
	}
	if err := ch.Insert(); err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	// 清理本测试写入的亲和/冷却内存状态，避免污染同包其它测试。
	scope := middleware.AffinityScope{UserID: guardUserID, Group: "default"}
	t.Cleanup(func() {
		for _, k := range scope.KeysToSet(arbitraryProbeModel) {
			middleware.AffinityGlobal.Remove(k)
		}
		middleware.CooldownGlobal.ResetChannel(guardChanID)
	})

	// —— 真实中间件链 + controller.Relay ——
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.TokenModelMapping(), middleware.Distribute())
	engine.POST("/v1/chat/completions", Relay)

	body := `{"model":"` + arbitraryProbeModel + `","messages":[{"role":"user","content":"probe"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-"+guardKey)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	// 硬指标①：不得因「模型不在渠道展示清单内」被本地拒绝（尤其不得返回 403「该令牌无权使用模型」）。
	if recorder.Code == http.StatusForbidden {
		t.Fatalf("出站被本地 403 拒绝（疑似引入了白名单）：body=%s", recorder.Body.String())
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("出站未成功到达上游：status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// 硬指标②：请求确实到达受控上游，且上游收到的正是该模型名。
	seen := upstream.lastBody()
	if !strings.Contains(seen, arbitraryProbeModel) {
		t.Fatalf("上游未收到该模型名的请求：got=%q（出站路径可能引入了本地白名单拒绝）", seen)
	}

	// 收尾：等待 post-consume 异步结算落库，消除与临时库目录清理的竞争。
	waitForGuardSettle(t, guardChanID)
}

// waitForGuardSettle 轮询等待 post-consume 的终态写（channels.used_quota 更新）完成。
// 用精确条件而非固定睡眠，避免与 t.TempDir 清理竞争产生 disk I/O error 噪音。
func waitForGuardSettle(t *testing.T, channelID int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var used int64
		if err := model.DB.Model(&model.Channel{}).Where("id = ?", channelID).Select("used_quota").Scan(&used).Error; err == nil && used > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("等待 post-consume 结算超时：channel %d 的 used_quota 未更新", channelID)
}
