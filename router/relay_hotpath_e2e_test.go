package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/client"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/channeltype"
)

// 本文件是任务 7.5 的端到端冒烟（AC-8）：走**完整生产路由链**
// （CORS/Gzip → RelayPanicRecover → TokenAuth → TokenModelMapping → Distribute → controller.Relay），
// 用受控 httptest 上游捕获实际转发到上游的请求字节，逐条验证：
//
//  1. chat（JSON POST）happy path：请求体被完整转发（字节相等）、流式 SSE 正常返回、
//     usage 与消费日志字段正确；
//  2. responses（JSON POST）happy path：直连渠道下请求体被完整转发、非流式 usage 与日志正确；
//  3. 下游**直读 `c.Request.Body`** 的四类路由（proxy / audio / text / image）在中间件链
//     （TokenAuth 的 getRequestModel、Distribute 的 ResolveAffinityScope）之后仍收到**完整无截断**
//     的 body，且路由结果（HTTP 状态与上游响应）不变。
//
// 关键设计：这四类路由的处理器都用 `c.Request.Body` / `io.Copy(c.Request.Body)` 直读 body，
// 因此「上游实际收到的字节 == 客户端发出的字节」是「上游收到完整 body」的**跨进程边界**证据
// （不是断言测试自己写下的值，而是比对上游实际收到的真实字节）。
//
// ⚠️ 覆盖强度限定（务必知悉，避免高估本用例的保护范围）：
//
//	image / audio / text 三个子用例**不能**证明「中间件链一定恢复了 c.Request.Body」。因为它们的
//	处理器自身在直读 body 之前会先走 UnmarshalBodyReusable / getRequestBody —— 后者优先从
//	ctxkey.KeyRequestBody 缓存重读并重建 c.Request.Body，故**即使中间件恢复点被全部删除**，
//	这三条路径仍能拿到完整 body、上游仍收到完整字节（全量变异验证：删除中间件恢复点后三者仍绿）。
//	真正对「中间件恢复点」敏感、能因恢复点缺失而变红的，**只有 proxy 子用例**：RelayProxyHelper
//	不做任何 unmarshal/缓存重读，直接把 c.Request.Body 交给 adaptor，是唯一「裸读」路径。
//
//	因此本文件的覆盖强度应表述为：proxy 子用例锁定中间件恢复点契约；image/audio/text 锁定的是
//	「各自处理器的缓存重读 + 恢复」契约。若要独立锁定中间件恢复点，需另有针对性的变异用例，
//	不能拿这三条路径的绿色反推中间件已恢复 body。
//
// 断言以真实数据为准，绝不为了凑绿而放松：proxy 路径若中间件读走 body 且不恢复、其自身也无
// 缓存可读，上游读到的就是空 body，本文件必然变红（这正是 affinity 改造期间发生过的
// 「测试全绿但没碰到真实路径」事故的反面）。

// ---------------------------------------------------------------------------
// 受控上游：记录每个 path 最近一次收到的原始请求体，并按 path 返回对应协议响应。
// ---------------------------------------------------------------------------

type relayE2EUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	seen   map[string]string
}

func newRelayE2EUpstream() *relayE2EUpstream {
	u := &relayE2EUpstream{seen: make(map[string]string)}
	u.server = httptest.NewServer(http.HandlerFunc(u.handle))
	return u
}

func (u *relayE2EUpstream) close() { u.server.Close() }

func (u *relayE2EUpstream) lastBody(path string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen[path]
}

func (u *relayE2EUpstream) record(r *http.Request) string {
	raw, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.seen[r.URL.Path] = string(raw)
	u.mu.Unlock()
	return string(raw)
}

func (u *relayE2EUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body := u.record(r)

	switch r.URL.Path {
	case "/v1/chat/completions":
		// 流式 chat：两条 chunk（含 usage）+ [DONE]，usage 由第二帧给出以便日志字段确定性断言。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`,
			"",
			`data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1,"model":"gpt-4-turbo","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n")))
	case "/v1/responses":
		// Responses 直连（DeepSeek 渠道）：非流式 JSON，usage 用于日志字段断言。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_e2e","object":"response","status":"completed","model":"deepseek-chat","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}`))
	case "/v1/images/generations":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":1,"data":[{"url":"https://example.invalid/e2e.png"}]}`))
	case "/v1/audio/speech":
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("E2E-AUDIO-BYTES"))
	case "/v1/moderations":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"modr-e2e","model":"text-moderation-stable","results":[{"flagged":false}]}`))
	default:
		// proxy 直读路由：把收到的 body 原样回显（既证明上游收到完整 body，也让路由结果可断言）。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		payload := map[string]string{"echo": body}
		_ = json.NewEncoder(w).Encode(payload)
	}
}

// ---------------------------------------------------------------------------
// 数据播种：走生产 InitDB 路径初始化临时 SQLite 库，并播种 user / token / channel。
// ---------------------------------------------------------------------------

type relayE2EFixture struct {
	upstream *relayE2EUpstream
	settle   *relayE2EAsyncTracker

	relayUserID   int
	relayTokenKey string
	adminUserID   int
	adminTokenKey string
}

// relayE2EAsyncTracker 跟踪本测试触发的「结算终态写」，用于在 t.Cleanup 中**确定性等待**
// post-consume 等异步 goroutine 结束，避免两类竞态：
//
//	A. tempdir 竞态：测试返回后异步 goroutine 继续写临时库，与 t.TempDir 的目录删除竞态
//	   （TempDir RemoveAll cleanup: ... directory not empty）；
//	B. 跨轮竞态：-count>=2 时上一轮的在途 goroutine 与下一轮 model.InitDB 的全局句柄替换互相干扰
//	   （disk I/O error / attempt to write a readonly database，并把上一轮的日志写进下一轮的库，
//	   导致「日志 token 字段」断言读到跨轮残留记录）。
//
// 信号选择依据（调查结论）：所有**成功计费**的 relay 路径，其 post-consume 异步 goroutine 的
// 最后一条 DB 语句恒为 `UpdateChannelUsedQuota`（写 channels.used_quota），见三处源头：
//   - relay/controller/helper.go postConsumeQuota               → model.UpdateChannelUsedQuota（函数末行）
//   - relay/controller/responses.go postConsumeQuotaForResponses → dbmodel.UpdateChannelUsedQuota（函数末行）
//   - relay/controller/audio.go 的 `defer func(){ go func(){...} }` → model.UpdateChannelUsedQuota（块内末行）
//
// 故「channels 表 UPDATE 完成数」达到本测试的计费请求数，即代表全部结算 goroutine 已执行到
// 最后一条语句、随后必然返回。等待实现用轮询，但**退出条件是精确计数**而非时间窗口，故为确定性
// 屏障，不是「睡一会儿碰运气」。非计费请求（proxy 直转）不产生该终态写。
//
// 本追踪器只登记在测试自建的临时库上（每轮 InitDB 产生全新 *gorm.DB 与其 callbacks），
// 不修改任何生产代码，也不新增第三方依赖（gorm 为既有依赖）。
type relayE2EAsyncTracker struct {
	channelUpdates int32 // atomic：已完成的 channels 表 UPDATE 计数
	expected       int32 // atomic：本测试声明的预期结算终态写数量（累计；0 表示无需等待）
}

func newRelayE2EAsyncTracker(t *testing.T, db *gorm.DB) *relayE2EAsyncTracker {
	t.Helper()
	tr := &relayE2EAsyncTracker{}
	name := fmt.Sprintf("relay_e2e_settle_track_%d", time.Now().UnixNano())
	// 挂在 gorm:commit_or_rollback_transaction 之后（而非 gorm:update 之后）：gorm 默认给单条
	// UPDATE 包一层事务，只有事务提交完成后该写才真正落盘。以提交完成作为信号，可确保计数递增时
	// 终态写已在临时库里可见/持久，等待因此是「写已完成」的确定性证明，而非「SQL 已发出」。
	err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(name, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "channels" {
			atomic.AddInt32(&tr.channelUpdates, 1)
		}
	})
	if err != nil {
		t.Fatalf("注册结算终态写追踪回调失败: %v", err)
	}
	// 刻意不 Remove：每轮 setupRelayE2E 都经 model.InitDB() 得到全新的 *gorm.DB（与其独立 callbacks），
	// 旧回调随上一轮 DB 一起被丢弃，无需清理；显式 Remove 反而会触发 gorm 的 warn 日志噪音。
	return tr
}

// expect 声明本测试将产生 n 次结算终态写。务必在发出会触发结算的请求**之前**（或紧接请求之后、
// 任何断言之前）调用，使清理阶段的 drain 即使遇到中途 t.Fatalf 也能等到在途 goroutine 结束，
// 从而彻底消除 tempdir / 跨轮竞态。
func (tr *relayE2EAsyncTracker) expect(n int) {
	atomic.AddInt32(&tr.expected, int32(n))
}

// wait 轮询等待已完成 channels 更新数达到当前声明的期望值（精确计数的确定性屏障）。超时返回 false。
func (tr *relayE2EAsyncTracker) wait() bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&tr.channelUpdates) >= atomic.LoadInt32(&tr.expected) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// await 在测试体内等待结算终态写全部完成（超时视为致命失败，便于快速定位 post-consume 卡死）。
func (tr *relayE2EAsyncTracker) await(t *testing.T) {
	t.Helper()
	if !tr.wait() {
		t.Fatalf("等待结算终态写超时：已完成 %d 次 channels 更新，期望至少 %d 次（post-consume 未结束）",
			atomic.LoadInt32(&tr.channelUpdates), atomic.LoadInt32(&tr.expected))
	}
}

// drain 在清理阶段同步等待已声明的结算终态写全部完成；无声明时立即返回。
// 用 Errorf 而非 Fatalf：清理阶段即使超时也不中断后续清理（TempDir 删除仍要执行）。
func (tr *relayE2EAsyncTracker) drain(t *testing.T) {
	t.Helper()
	if atomic.LoadInt32(&tr.expected) == 0 {
		return
	}
	if !tr.wait() {
		t.Errorf("清理阶段等待结算终态写超时：已完成 %d 次 channels 更新，期望至少 %d 次",
			atomic.LoadInt32(&tr.channelUpdates), atomic.LoadInt32(&tr.expected))
	}
}

func setupRelayE2E(t *testing.T) *relayE2EFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// 走生产 InitDB / InitLogDB 路径：临时 SQLite 库 + 内存回退（Redis 已在 router TestMain 关闭）。
	t.Setenv("SQL_DSN", "")
	t.Setenv("LOG_SQL_DSN", "")
	common.RedisEnabled = false
	common.UsingSQLite = true
	common.SQLitePath = filepath.Join(t.TempDir(), "relay-e2e.db")
	model.InitDB()
	model.InitLogDB()

	// 避免 tiktoken 需要网络/磁盘缓存：估算路径走近似实现，不影响上游 usage 提取。
	origApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = origApproximate })

	f := &relayE2EFixture{upstream: newRelayE2EUpstream()}
	t.Cleanup(f.upstream.close)

	// 上游 HTTP client：生产默认由 common/client.Init() 注入，测试显式覆盖。
	origClient := client.HTTPClient
	client.HTTPClient = &http.Client{}
	t.Cleanup(func() { client.HTTPClient = origClient })

	// 结算终态写追踪器：注册于 t.TempDir() 之后，使下方的 drain+关库清理（LIFO）先于
	// TempDir 的目录删除执行 —— 先等在途 goroutine 结束，再删临时目录/换库，消除两类竞态。
	f.settle = newRelayE2EAsyncTracker(t, model.DB)
	t.Cleanup(func() {
		f.settle.drain(t)
		// 主动释放 SQLite 句柄（DB 与 LOG_DB 同源时 CloseDB 只关一次），
		// 避免残留连接/FD 与 t.TempDir 的目录删除竞争。
		_ = model.CloseDB()
	})

	f.relayUserID = seedE2EUser(t, model.RoleCommonUser, "e2e-relay-user")
	f.relayTokenKey = seedE2EToken(t, f.relayUserID, "default")
	f.adminUserID = seedE2EUser(t, model.RoleAdminUser, "e2e-admin-user")
	f.adminTokenKey = seedE2EToken(t, f.adminUserID, "default")

	base := f.upstream.server.URL
	// 每个渠道使用唯一模型，使 selectChannel 的候选集唯一、选路确定。
	seedE2EChannel(t, 0, "e2e-chat", channeltype.OpenAI, "gpt-4-turbo", base)
	seedE2EChannel(t, 0, "e2e-responses", channeltype.DeepSeek, "deepseek-chat", base)
	seedE2EChannel(t, 0, "e2e-image", channeltype.OpenAI, "dall-e-2", base)
	seedE2EChannel(t, 0, "e2e-audio", channeltype.OpenAI, "tts-1", base)
	seedE2EChannel(t, 0, "e2e-moderations", channeltype.OpenAI, "text-moderation-stable", base)
	// proxy 走指定渠道分支，模型无关；固定 id 以便路径参数引用。
	seedE2EChannel(t, 9001, "e2e-proxy", channeltype.Proxy, "any-model", base)
	return f
}

func seedE2EUser(t *testing.T, role int, username string) int {
	t.Helper()
	user := &model.User{
		Username:    username,
		Password:    "test-password",
		DisplayName: username,
		Role:        role,
		Status:      model.UserStatusEnabled,
		Quota:       1_000_000_000,
		// access_token 有唯一索引：显式给出唯一值，避免多个种子用户都是空串而相互冲突。
		AccessToken: fmt.Sprintf("e2e-access-%s", username),
	}
	if err := model.DB.Create(user).Error; err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	return user.Id
}

func seedE2EToken(t *testing.T, userID int, group string) string {
	t.Helper()
	key := fmt.Sprintf("e2ekey%d", userID)
	token := &model.Token{
		UserId:      userID,
		Key:         key,
		Status:      model.TokenStatusEnabled,
		Name:        "e2e",
		CreatedTime: time.Now().Unix(),
		Group:       group,
	}
	if err := model.DB.Create(token).Error; err != nil {
		t.Fatalf("seed token for user %d: %v", userID, err)
	}
	return key
}

func seedE2EChannel(t *testing.T, id int, name string, channelType int, modelName, baseURL string) {
	t.Helper()
	base := baseURL
	ch := &model.Channel{
		Id:      id,
		Type:    channelType,
		Key:     "sk-e2e-upstream",
		Status:  model.ChannelStatusEnabled,
		Name:    name,
		Models:  modelName,
		Group:   "default",
		BaseURL: &base,
	}
	if err := ch.Insert(); err != nil {
		t.Fatalf("seed channel %s: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// 引擎装配与请求工具
// ---------------------------------------------------------------------------

func newRelayE2EEngine() *gin.Engine {
	r := gin.New()
	SetRelayRouter(r)
	return r
}

func doRelayE2E(t *testing.T, engine *gin.Engine, tokenKey, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-"+tokenKey)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

// waitConsumeLog 轮询等待消费日志落库（post-consume 在 goroutine 中执行），返回最新一条。
func waitConsumeLog(t *testing.T, userID int) *model.Log {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var entry model.Log
		err := model.LOG_DB.
			Where("user_id = ? AND type = ?", userID, model.LogTypeConsume).
			Order("id desc").First(&entry).Error
		if err == nil {
			return &entry
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 user %d 的消费日志超时（post-consume 未落库）", userID)
	return nil
}

// ---------------------------------------------------------------------------
// 用例 1：chat 流式 happy path（请求体完整转发 + SSE + usage/日志字段）
// ---------------------------------------------------------------------------

func TestRelayHotPathE2E_ChatStreamHappyPath(t *testing.T) {
	f := setupRelayE2E(t)
	engine := newRelayE2EEngine()

	body := `{"model":"gpt-4-turbo","stream":true,"messages":[{"role":"user","content":"hello e2e"}]}`
	recorder := doRelayE2E(t, engine, f.relayTokenKey, http.MethodPost, "/v1/chat/completions", body)
	// 声明本请求成功计费的结算终态写（post-consume 在 goroutine 内，终态写 = channels 更新 1 次）。
	// 提前声明使清理阶段无论断言是否中途失败都能等在途 goroutine 结束，杜绝 tempdir/跨轮竞态。
	f.settle.expect(1)

	if recorder.Code != http.StatusOK {
		t.Fatalf("chat stream 期望 200，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}

	// 1) 上游收到的请求字节必须与客户端发出的逐字节相等（中间件链未消费/截断 body）。
	if got := f.upstream.lastBody("/v1/chat/completions"); got != body {
		t.Fatalf("上游收到的 chat 请求体与客户端不一致：\n got=%q\nwant=%q", got, body)
	}

	// 2) 流式响应被完整转发：含数据帧与 [DONE]。
	resp := recorder.Body.String()
	if !strings.Contains(resp, `"content":"Hi"`) {
		t.Fatalf("流式响应缺少内容帧，实际 body=%q", resp)
	}
	if !strings.Contains(resp, "[DONE]") {
		t.Fatalf("流式响应缺少 [DONE] 终止帧，实际 body=%q", resp)
	}

	// 3) usage 与日志字段正确（来自上游 usage，非本地估算）。
	entry := waitConsumeLog(t, f.relayUserID)
	if entry.PromptTokens != 7 || entry.CompletionTokens != 3 {
		t.Fatalf("chat 日志 token 字段错误：prompt=%d completion=%d（期望 7/3）", entry.PromptTokens, entry.CompletionTokens)
	}
	if !entry.IsStream {
		t.Fatalf("chat 流式请求的日志 is_stream 必须为 true")
	}
	if entry.ModelName != "gpt-4-turbo" {
		t.Fatalf("chat 日志 model_name=%q（期望 gpt-4-turbo）", entry.ModelName)
	}

	// 4) 同步等待这条 chat 的 post-consume goroutine 彻底结束（已在上方声明的终态写全部完成）。
	f.settle.await(t)
}

// ---------------------------------------------------------------------------
// 用例 2：responses 直连 happy path（请求体完整转发 + usage/日志字段）
// ---------------------------------------------------------------------------

func TestRelayHotPathE2E_ResponsesDirectHappyPath(t *testing.T) {
	f := setupRelayE2E(t)
	engine := newRelayE2EEngine()

	body := `{"model":"deepseek-chat","input":"hello responses e2e"}`
	recorder := doRelayE2E(t, engine, f.relayTokenKey, http.MethodPost, "/v1/responses", body)
	// 声明本请求成功计费的结算终态写（post-consume goroutine 的 channels 更新 1 次）。
	f.settle.expect(1)

	if recorder.Code != http.StatusOK {
		t.Fatalf("responses 期望 200，实际 %d（body=%s）", recorder.Code, recorder.Body.String())
	}

	// 直连渠道（DeepSeek）下 origin == actual，无模型改写，上游必须收到逐字节相同的原始 body。
	if got := f.upstream.lastBody("/v1/responses"); got != body {
		t.Fatalf("上游收到的 responses 请求体与客户端不一致：\n got=%q\nwant=%q", got, body)
	}
	if !strings.Contains(recorder.Body.String(), `"id":"resp_e2e"`) {
		t.Fatalf("responses 响应未被透传，实际 body=%q", recorder.Body.String())
	}

	entry := waitConsumeLog(t, f.relayUserID)
	if entry.PromptTokens != 11 || entry.CompletionTokens != 5 {
		t.Fatalf("responses 日志 token 字段错误：prompt=%d completion=%d（期望 11/5）", entry.PromptTokens, entry.CompletionTokens)
	}
	if entry.IsStream {
		t.Fatalf("responses 非流式请求的日志 is_stream 必须为 false")
	}
	if entry.ModelName != "deepseek-chat" {
		t.Fatalf("responses 日志 model_name=%q（期望 deepseek-chat）", entry.ModelName)
	}

	// 同步等待这条 responses 的 post-consume goroutine 彻底结束（已声明的终态写全部完成）。
	f.settle.await(t)
}

// ---------------------------------------------------------------------------
// 用例 3：下游直读 c.Request.Body 的四类路由收到完整 body（AC-8 核心）
// ---------------------------------------------------------------------------

func TestRelayHotPathE2E_DirectBodyReadRouteFamilies(t *testing.T) {
	f := setupRelayE2E(t)
	engine := newRelayE2EEngine()

	// 每一行都是一次真实 HTTP 请求；断言上游实际收到的字节 == 客户端发出的字节。
	cases := []struct {
		name       string
		family     string
		tokenKey   string
		method     string
		path       string
		body       string
		upstream   string
		wantInResp string
		// settles 声明本子用例成功转发后必然产生的「结算终态写」（channels 表 UPDATE）数量，
		// 供测试结束前确定性等待全部在途结算 goroutine 结束（见 relayE2EAsyncTracker 注释）：
		//   - image：结算发生在 RelayImageHelper 内、请求返回前（同步），故计数在断言时已完成；
		//   - audio / text：结算在 post-consume goroutine 内（异步），是 flaky 的主要来源；
		//   - proxy：RelayProxyHelper 不做计费，无终态写。
		settles int
	}{
		{
			name:       "image",
			family:     "image（requestBody = c.Request.Body）",
			tokenKey:   f.relayTokenKey,
			method:     http.MethodPost,
			path:       "/v1/images/generations",
			body:       `{"model":"dall-e-2","prompt":"a small e2e cat","n":1,"size":"256x256"}`,
			upstream:   "/v1/images/generations",
			wantInResp: `"url":"https://example.invalid/e2e.png"`,
			settles:    1,
		},
		{
			name:       "audio",
			family:     "audio（io.Copy(requestBody, c.Request.Body)）",
			tokenKey:   f.relayTokenKey,
			method:     http.MethodPost,
			path:       "/v1/audio/speech",
			body:       `{"model":"tts-1","input":"hello e2e audio","voice":"alloy"}`,
			upstream:   "/v1/audio/speech",
			wantInResp: "E2E-AUDIO-BYTES",
			settles:    1,
		},
		{
			name:       "text",
			family:     "text（getRequestBody 返回 c.Request.Body）",
			tokenKey:   f.relayTokenKey,
			method:     http.MethodPost,
			path:       "/v1/moderations",
			body:       `{"model":"text-moderation-stable","input":"hello e2e moderation"}`,
			upstream:   "/v1/moderations",
			wantInResp: `"id":"modr-e2e"`,
			settles:    1,
		},
		{
			name:       "proxy",
			family:     "proxy（RelayProxyHelper 直接传 c.Request.Body）",
			tokenKey:   f.adminTokenKey,
			method:     http.MethodPost,
			path:       "/v1/myapi/proxy/9001/v1/echo",
			body:       `{"probe":"complete-body","padding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
			upstream:   "/v1/echo",
			wantInResp: `"echo"`,
			settles:    0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRelayE2E(t, engine, tc.tokenKey, tc.method, tc.path, tc.body)
			// 声明本子用例的结算终态写数量（proxy 为 0）。提前声明，使清理阶段 drain 生效于
			// 任意中途失败路径，杜绝在途 goroutine 与 TempDir 删除/下一轮换库的竞态。
			f.settle.expect(tc.settles)

			if recorder.Code != http.StatusOK {
				t.Fatalf("[%s] 期望 200，实际 %d（body=%s）", tc.family, recorder.Code, recorder.Body.String())
			}
			// body 完整性：跨进程边界后上游收到的字节必须与客户端发出的逐字节相等。
			if got := f.upstream.lastBody(tc.upstream); got != tc.body {
				t.Fatalf("[%s] 上游收到的请求体与客户端不一致（body 被截断/消费）：\n got=%q\nwant=%q",
					tc.family, got, tc.body)
			}
			// 路由结果不变：上游响应被正常透传/回显。
			if !strings.Contains(recorder.Body.String(), tc.wantInResp) {
				t.Fatalf("[%s] 响应未包含预期结果 %q，实际 body=%q", tc.family, tc.wantInResp, recorder.Body.String())
			}
		})
	}

	// 同步等待全部子用例的结算终态写完成（audio/text 的 post-consume 为异步），
	// 再让清理阶段删临时库/换库；否则在途 goroutine 与 TempDir 删除/下一轮 InitDB 竞态。
	f.settle.await(t)
}

// ---------------------------------------------------------------------------
// 反向证伪：证明本文件不是在断言「测试自己写下的值」。
// ---------------------------------------------------------------------------
//
// ⚠️ 定位说明（请勿误解本用例证明了什么）：
//
//	本用例**不是**对生产链路的变异，而是**自建一条合成路由**：一个受控「坏中间件」读走
//	c.Request.Body 且不恢复，加一个**裸读 handler**（直接拿 c.Request.Body 发往上游，不做
//	UnmarshalBodyReusable / 缓存重读）。
//
//	它证明的是一个**弱命题**：当「中间件不恢复 body」且「handler 也无缓存可重读」时，
//	「上游收到的字节 == 客户端发出的字节」这一断言确实会变红 —— 即正向断言具备区分度、
//	不是恒真。它**不**直接证明生产链路的某个中间件有/无恢复点（那需要针对真实中间件的变异）。
//
//	本用例选用裸读 handler（而非复刻 image.go 的缓存重读路径）正是为了体现区分度：
//	image/audio/text 的生产处理器会从缓存重读，即使中间件不恢复也读得到完整 body，
//	故它们无法充当本反向证伪的探针；只有裸读路径能暴露「未恢复」这一缺陷，这与文件头
//	「覆盖强度限定」中「仅 proxy 对恢复点敏感」的结论一致。
// ---------------------------------------------------------------------------

func TestRelayHotPathE2E_ReverseProof_BodyNotRestoredIsDetectable(t *testing.T) {
	f := setupRelayE2E(t)

	// 受控「坏中间件」：读走 c.Request.Body 且不恢复，模拟中间件链未做 body 恢复的缺陷。
	r := gin.New()
	r.Use(func(c *gin.Context) {
		_, _ = io.ReadAll(c.Request.Body)
		c.Next()
	})
	r.POST("/v1/images/generations", func(c *gin.Context) {
		// 裸读 handler：直接把 c.Request.Body 交给上游（不做缓存重读）。
		// 若中间件读走 body 且不恢复，这里读到 0 字节并发往上游 —— 探针因此对「未恢复」敏感。
		req, _ := http.NewRequest(http.MethodPost, f.upstream.server.URL+"/v1/images/generations", c.Request.Body)
		resp, err := client.HTTPClient.Do(req)
		if err != nil {
			c.String(http.StatusInternalServerError, "boom")
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	})

	body := `{"model":"dall-e-2","prompt":"reverse proof","n":1,"size":"256x256"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 坏中间件下上游收到空 body —— 说明本文件的「字节相等」断言具备区分度，不是恒真。
	if got := f.upstream.lastBody("/v1/images/generations"); got != "" {
		t.Fatalf("反向证伪失败：未恢复 body 时上游仍收到 %q，说明正向断言空间无意义", got)
	}
}
