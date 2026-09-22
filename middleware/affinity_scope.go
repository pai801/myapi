package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/buger/jsonparser"
	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
)

// AffinityLevel 表示亲和键的粒度层级，值越小越粗。
// 解析时按 turn → session → user 由细到粗降级。
type AffinityLevel int

const (
	AffinityLevelUser    AffinityLevel = iota // 最粗，仅服务无会话标识的客户端（跨轮兜底）
	AffinityLevelSession                      // 会话级：多轮稳定
	AffinityLevelTurn                         // 最细，优先：一次用户发送内恒定
)

// String 返回层级名，供日志打印；只暴露层级名，避免泄漏原始 turn / session id。
func (l AffinityLevel) String() string {
	switch l {
	case AffinityLevelTurn:
		return "turn"
	case AffinityLevelSession:
		return "session"
	default:
		return "user"
	}
}

// affinityIDMaxLen 是 turn / session id 归一化后的最大长度。
// 超过则用 sha256 截断，防止超长请求头污染内存与日志。
const affinityIDMaxLen = 64

// affinityTurnHeaders 是 turn 层请求头（轮级：一次用户发送内跨 tool call / 重试恒定），按优先级排列。
//
// 红线一：绝不收 X-Request-Id / X-Client-Request-Id。
// 它们是 per-request 语义 —— 每次 HTTP 请求重新生成、重试之间互不相同
// （依据 docs/plan-buddy-channel.md:437、:452 及 :447-448 的真实抓包对照）。
// 若把它们当 turn key，会导致「每个请求一个 key → 亲和永远不命中」，等于静默关闭
// 亲和功能，且比显式关掉更难排查（代码看起来在跑，指标上表现为随机选路）。
//
// 注意与私有仓 myapi-server/internal/channel/buddy/headers.go:406 的
// conversationRequestIDFromContext 刻意背离：后者把 X-Client-Request-Id /
// X-Request-Id 收进了轮级聚合主键，因为那是「出站头必须发出一个值」的场景；
// 本处是「入站亲和键」，语义不同，不可互相照抄，否则后人会把 per-request 头加回来，
// 静默废掉亲和功能。
var affinityTurnHeaders = []string{
	"X-Conversation-Request-Id",
	"X-Query-Id",
	"X-Root-Request-Id",
}

// affinitySessionHeaders 是 session 层请求头（会话级：多轮稳定），按优先级排列。
// 下划线形态（conversation_id / session_id）与连字符形态（X-Conversation-Id /
// X-Session-Id）是不同的头名，必须分别收录（HTTP 头名大小写不敏感，但下划线/连字符不等价）。
var affinitySessionHeaders = []string{
	"conversation_id",
	"session_id",
	"X-Conversation-Id",
	"X-Session-Id",
	"Session-Id",
	"Thread-Id",
	"X-Parent-Session-Id",
	// 以下为业界调研补充的会话级头，追加在既有 7 项之后（不改变上面既有优先级）。
	// 每个头标注：谁在发 + 会话级语义 + 证据强度（强/中/弱）。
	// Claude Code 会话头：Claude Code 每会话恒定、跨轮复用；证据强度=中。
	"X-Claude-Code-Session-Id",
	// OpenCode 会话头：2026-09-05 起强制下发，上游据此路由与前缀缓存，缺失报 400
	// MissingSessionID；同一会话必须恒定；证据强度=强。
	"X-Opencode-Session",
	// LiteLLM 生态（Cline / Roo Code / Kilo Code）会话头，随会话稳定；证据强度=强。
	"X-Litellm-Session-Id",
	// Zed / ACP 系连接级会话头，连接（会话）内恒定；证据强度=中。
	"Acp-Connection-Id",
	// Zed / ACP 系会话头，随会话稳定；证据强度=中。
	"Acp-Session-Id",
}

// AffinityScope 描述一次请求可用的亲和维度；某层 id 为空表示该层不可用。
//
// 层级产出规则（见 Keys / KeysToSet）：turn 与 session 是可选细层，user 是无会话标识时的
// 兜底层。SessionID 非空且 AFFINITY_SESSION_EXCLUDES_USER 开启（默认）时，读写都不产出
// user 层——有会话的客户端靠 session 层拿跨轮亲和，避免用 user 层兜底导致同一用户所有会话
// 收敛到同一渠道；SessionID 为空（turn-only 客户端）时才产出 user 兜底，防止跨轮断链。
//
// 注意：本结构含 map 字段（reqHeaders），因此**不可比较** —— 不可用 `==` 判断、不可作 map key，
// 否则编译期报错（invalid operation: ... cannot be compared）。如需按值比较请显式逐字段比。
type AffinityScope struct {
	UserID    int
	Group     string
	TurnID    string // 空 = 该层不可用
	SessionID string // 空 = 该层不可用

	// turnDerived 仅表示当前非空 TurnID 来自 body 派生：真实 turn 头、空 turn id、
	// session/user 层均为 false。仅供统计分档（区分「真实头 turn」与「派生 turn」），
	// 不能影响 Keys / KeysToSet 的选路结果。
	turnDerived bool

	// reqHeaders 是本次请求原始头表的只读引用（不拷贝、不跨请求持有），仅供未命中时
	// 采样「客户端实际带了哪些候选头」。保留引用而非即时计算，使采样开销只在未命中路径发生；
	// 生命周期与请求一致，nonAutoDistribute 在请求内同步消费，不产生悬挂引用。
	reqHeaders http.Header
}

// AffinityKey 是一个具体层级的亲和键；Value 即 AffinityManager 内部 map 的 key。
type AffinityKey struct {
	Level AffinityLevel
	Value string
}

// ResolveAffinityScope 解析请求的亲和维度。
//
// turn / session 层优先读请求头（零成本）。当缺 session 头或（后续）缺 turn 头时，
// 按开关与请求特征决定是否读取请求 body：session 补取与 turn 派生共享同一次读取与同一份
// Raw（见 readAffinityBodyPayload），两个消费者各自用 jsonparser 只读扫描，不产出中间 map
// —— 下半区主流客户端（Cline / Roo Code / Kilo Code、Responses 多轮）不在头里传会话标识，
// 只放在 body 里。
//
// 红线二：取不到 turn 时绝不生成随机 id。这与私有仓
// myapi-server/internal/channel/buddy/headers.go:406 的 conversationRequestIDFromContext
// 刻意背离 —— 后者取不到时会生成随机 32hex，因为出站头必须发出一个值；而这里是
// 亲和键，生成随机 id 等于「每个请求一个 key」，会静默废掉亲和功能。缺失即降级到下一层。
func ResolveAffinityScope(c *gin.Context) AffinityScope {
	var hdr http.Header
	if c.Request != nil {
		hdr = c.Request.Header
	}
	turnID := firstNonEmptyHeader(c, affinityTurnHeaders)
	sessionID := firstNonEmptyHeader(c, affinitySessionHeaders)
	turnDerived := false

	// 按需读取：仅当 turn 派生（缺真实 turn 头且派生开关开启）或 session body 补取
	// （缺 session 头且补取开关开启）至少一方需要 body 时才读，两者都不需要则完全跳过，
	// 保持既有「按需读取」的零开销路径。派生分支必须与 session 分支并列判定：
	// session 补取被开关关闭但派生开启时仍要读，因为派生有自己的开关与守卫。
	needBody := (turnID == "" && affinityDeriveTurnIDEnabled()) ||
		(sessionID == "" && affinityBodySessionIDEnabled())
	// 方法 / Content-Type 守卫在此统一把关：GET 与非 JSON 请求不读 body（含派生路径）。
	if needBody && canReadAffinityJSONBody(c) {
		if shared := readAffinityBodyPayload(c); shared != nil {
			// session 提取仍受自身独立开关约束：补取关闭时即便已读 body 也不得取会话标识。
			if sessionID == "" && affinityBodySessionIDEnabled() {
				sessionID = sessionIDFromBody(shared.Raw)
			}
			// 真实 turn 头优先；派生仅在派生开关、session 锚点非空与 JSON 请求守卫
			// 全部满足时接入（进入本分支即已通过方法与 Content-Type 守卫）。派生直接消费
			// 共享 Raw，与 session 提取各自用 jsonparser 只读扫描，请求路径不发生第二次 body 读取。
			if turnID == "" && sessionID != "" && affinityDeriveTurnIDEnabled() {
				if derived := common.DeriveTurnIDFromBody(shared.Raw, sessionID); derived != "" {
					turnID = derived
					turnDerived = true
				}
			}
		}
	}

	return AffinityScope{
		UserID:      c.GetInt(ctxkey.Id),
		Group:       c.GetString(ctxkey.Group),
		TurnID:      turnID,
		SessionID:   sessionID,
		turnDerived: turnDerived,
		reqHeaders:  hdr,
	}
}

// affinityBodySessionIDFields 是按优先级排列的 body 字段路径，取首个归一化后非空者。
//
// 红线三（与红线一同型，只是发生在 body 字段而非 header 上）：绝不收「每次请求都会变化」的标识。
// 已移除 previous_response_id —— 它是**上一轮响应的 id**（relay/model/responses.go 的
// previous_response_id 字段）。OpenAI Responses 客户端链式续接时，每发一次请求就带一个
// 指向最近一次响应的新 previous_response_id，即**每次 LLM 调用都会变化**，属 per-request /
// per-turn 值，不具备会话级稳定性。把它当 session 键会：
//  1. 产生「每次请求一个 key → 亲和永远不命中」的死键，静默废掉亲和功能，比显式关掉更难排查；
//  2. 遮蔽排在后面的、真正稳定的 litellm_session_id（Cline / Roo Code / Kilo Code）——
//     只要请求里带 previous_response_id，稳定字段就永远取不到；
//  3. 白占 AFFINITY_MAX_ENTRIES 配额并推高淘汰频率，间接挤掉其他客户端的有效键。
//
// 切勿照抄 docs/plan-chatgpt-subscription-channel.md 把 previous_response_id 当粘性键的用法：
// 那是**上下文拼接**语义（保证上一轮内容被带上），与「亲和键需跨请求稳定」是两回事。
//
// 刻意不读 conversation.id：它是服务端在响应里下发的字段，请求中几乎不出现
// （依据 docs/responses-protocol.md 与 relay/model/responses.go 的结构体定义），
// 纳入只会徒增一次无效解引用。
var affinityBodySessionIDFields = [][]string{
	{"litellm_session_id"}, // Cline / Roo Code / Kilo Code，随会话稳定（首位）
	{"session_id"},
	{"conversation_id"},
	{"metadata", "cline_task_id"}, // Cline 会把任务 id 塞进 metadata 对象
}

// affinityBodyMethods 是允许读 body 提取会话标识的请求方法。
// 只有 POST / PUT / PATCH 在语义上携带请求体（GET / HEAD / DELETE 无 body），
// 限定范围可避免对无 body 的请求做无谓的读取尝试。
var affinityBodyMethods = map[string]bool{
	http.MethodPost:  true,
	http.MethodPut:   true,
	http.MethodPatch: true,
}

// affinityBodyPayload 表示一次请求内共享的完整 body。
// Raw 为请求内只读引用：不得原地修改、截断或跨请求持有，生命周期不超过当前请求，
// 也不得写入全局状态或派生缓存。两个消费者（session 提取与 turn 派生）各自用 jsonparser
// 只读扫描 Raw，不再产出中间 map。
type affinityBodyPayload struct {
	Raw []byte // 完整原始 body（只读，不得原地修改）
}

// affinityDeriveTurnIDEnabled 判定 turn 派生开关是否开启：仅当值等于 "false"
// （忽略大小写与首尾空白）时视为关闭，其余任何取值均视为开启（容错，与
// AFFINITY_BODY_SESSION_ID 惯例一致）。
func affinityDeriveTurnIDEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(config.AffinityDeriveTurnID), "false")
}

// affinityBodySessionIDEnabled 判定 session body 补取开关是否开启，容错规则同上。
func affinityBodySessionIDEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(config.AffinityBodySessionID), "false")
}

// affinitySessionExcludesUserEnabled 判定「有 session 时是否排除 user 层」开关是否开启，
// 容错规则同上：仅当值等于 "false"（忽略大小写与首尾空白）时视为关闭，其余任何取值均视为开启。
func affinitySessionExcludesUserEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(config.AffinitySessionExcludesUser), "false")
}

// canReadAffinityJSONBody 判断请求是否满足「读 body」的方法与 Content-Type 守卫：
// 方法属于 POST / PUT / PATCH，且 Content-Type 前缀为 application/json（大小写不敏感，
// 天然排除 multipart/form-data —— 音频转写 / 图像上传的 body 既无会话标识又很大）。
// nil 请求返回 false，且在此分支下不触碰 body（避免对无请求场景产生任何读取）。
func canReadAffinityJSONBody(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if !affinityBodyMethods[c.Request.Method] {
		return false
	}
	// HTTP 媒体类型按 RFC 大小写不敏感，故先 ToLower 再前缀匹配，
	// 否则客户端发 Application/JSON 会静默丢掉这次 body 读取机会（降级到 user 层）。
	return strings.HasPrefix(strings.ToLower(c.Request.Header.Get("Content-Type")), "application/json")
}

// readAffinityBodyPayload 读取并恢复完整请求体，且至多一次 IO。
//
// 必须使用 common.GetRequestBodyReusable：它在读取后恢复 c.Request.Body（下游有路由
// 直接读 c.Request.Body —— proxy.go / audio.go / text.go / image.go），并复用既有
// ctxkey.KeyRequestBody 缓存，保证同一请求最多一次 IO。绝不截断请求体。
//
// 失败一律静默降级（返回 nil），绝不 abort 请求、绝不报错、绝不打印 body 内容或会话 id：
// 亲和只是选路优化，不能因 body 形态异常而影响主流程，也不应泄漏用户数据。
// 读取失败、空 body 或非法 JSON 均返回 nil。
//
// json.Valid 预检是安全红线而非性能优化：jsonparser 对畸形 JSON 会部分成功（例如
// `{"litellm_session_id":"leaked","messages":[` 仍能取出 session 值），若不预检就会从
// 损坏/截断的请求里采集会话标识并据此选路。预检失败即拒绝，绝不从半截 JSON 提取任何标识。
func readAffinityBodyPayload(c *gin.Context) *affinityBodyPayload {
	if c == nil || c.Request == nil {
		return nil
	}
	body, err := common.GetRequestBodyReusable(c)
	if err != nil || len(body) == 0 {
		return nil
	}
	if !json.Valid(body) {
		return nil
	}
	return &affinityBodyPayload{Raw: body}
}

// sessionIDFromBody 从只读 body 按既有字段优先级提取并归一化会话标识。
//
// 本函数不再自行读取 body：开关、方法与 Content-Type 守卫由调用方（ResolveAffinityScope）
// 统一把关，body 由两个消费者（session 提取与 turn 派生）共享，避免重复读取。
// 缺失或类型不符一律返回空串并沿既有层级降级，绝不报错、绝不打印会话 id。
func sessionIDFromBody(body []byte) string {
	for _, path := range affinityBodySessionIDFields {
		raw, err := jsonparser.GetString(body, path...)
		if err != nil {
			continue
		}
		if id := normalizeAffinityID(raw); id != "" {
			return id
		}
	}
	return ""
}

// firstNonEmptyHeader 按给定顺序返回首个归一化后非空的头值。
func firstNonEmptyHeader(c *gin.Context, names []string) string {
	for _, name := range names {
		if v := normalizeAffinityID(c.GetHeader(name)); v != "" {
			return v
		}
	}
	return ""
}

// normalizeAffinityID 归一化 id：TrimSpace 后为空返回空串（视为该层不可用）；
// 超过 affinityIDMaxLen 则 sha256 取前 16 位 hex。
func normalizeAffinityID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" {
		return ""
	}
	if len(id) > affinityIDMaxLen {
		sum := sha256.Sum256([]byte(id))
		return hex.EncodeToString(sum[:8])
	}
	return id
}

// Keys 返回由细到粗的候选键序列（turn → session[ → user]），不可用的层不产出。
//
// 层级产出规则：
//   - 回退模式 AFFINITY_KEY_MODE=user：只返回 user 层一个键（旧行为），不受其它开关影响；
//   - SessionID 非空且未关闭 AFFINITY_SESSION_EXCLUDES_USER：产出 [turn?, session]，**不含 user 层**。
//     有会话的客户端靠 session 层拿跨轮亲和即可；保留 user 层会让新会话首请求必然命中该用户的
//     最近渠道、跳过随机散开，并因每次成功都改写 user 键而把同用户所有会话收敛到同一渠道；
//   - SessionID 为空（无会话标识，含 turn-only 客户端）：产出 [turn?, user]，保留 user 兜底。
//     turn id 每轮换新，若连 user 兜底也不产出，下一轮将整体 miss → 跨轮断链，比改造前更差。
//
// 因此返回值在「有 session 且开关开启」时至少一个元素（session），否则至少一个元素（user）。
func (s AffinityScope) Keys(model string) []AffinityKey {
	// 应急回退开关，必须容错：忽略大小写与首尾空白，避免运维误写 USER/"user " 时静默按 auto 走、开关形同虚设。
	if strings.EqualFold(strings.TrimSpace(config.AffinityKeyMode), "user") {
		return []AffinityKey{{
			Level: AffinityLevelUser,
			// 回退模式必须与改造前的 buildKey 逐字节等价：旧格式为 fmt.Sprintf("%d:%s", userId, model)，
			// 不含 group。若沿用分层键的 level|group|uid|model| 形态，同一用户跨 group 将不再共享
			// 亲和，违背「AFFINITY_KEY_MODE=user = 一键回到旧行为」的承诺。
			Value: fmt.Sprintf("%d:%s", s.UserID, model),
		}}
	}

	keys := make([]AffinityKey, 0, 3)
	if s.TurnID != "" {
		keys = append(keys, AffinityKey{
			Level: AffinityLevelTurn,
			Value: buildAffinityKeyValue(AffinityLevelTurn, s.Group, s.UserID, model, s.TurnID),
		})
	}
	if s.SessionID != "" {
		keys = append(keys, AffinityKey{
			Level: AffinityLevelSession,
			Value: buildAffinityKeyValue(AffinityLevelSession, s.Group, s.UserID, model, s.SessionID),
		})
		// 有会话标识时读写都不产出 user 层（默认行为）：详见函数头注释与 AffinityScope 说明。
		// 关闭 AFFINITY_SESSION_EXCLUDES_USER 即回退到旧行为，此时继续产出 user 兜底键。
		if affinitySessionExcludesUserEnabled() {
			return keys
		}
	}
	keys = append(keys, AffinityKey{
		Level: AffinityLevelUser,
		Value: buildAffinityKeyValue(AffinityLevelUser, s.Group, s.UserID, model, ""),
	})
	return keys
}

// KeysToSet 返回一次成功转发后需要写入的亲和键：与 Keys 完全一致（写你读得到的层），
// 不复制一份逻辑，避免读写层集漂移导致「写了的读不到 / 读了的不更新」。
//
// 为什么写全部可用层而非只写最细层：细层 id 会轮换/过期，粗层若不写入就永远 miss。
//   - turn id 按定义每轮换新：只写 turn 时，下一轮 turn 键必然 miss；
//   - turn-only 客户端（无任何 session 标识）只有 turn 键：若连 user 兜底也不写，下一轮 turn
//     换新后 turn miss、user 层从未写过也 miss → 跨轮亲和整体断链，比改造前更差。
//
// 为什么有 session 时不写 user 层（默认，AFFINITY_SESSION_EXCLUDES_USER 开启）：
// user 层是「同用户最近一次成功渠道」的粗绑定，一旦在 session 流量下持续被改写，同一 userId 的
// 所有会话会被逐步收敛到同一渠道（热点）；而 session 层已能提供跨轮亲和，无需 user 兜底。
// 排除 user 层后，新会话首请求必然 miss → 走加权随机散开，随后绑定自己的 session 键。
//
// 回退模式（AFFINITY_KEY_MODE=user）下 Keys 只产出 user 层一个键，此时也只写那一个，行为不变。
func (s AffinityScope) KeysToSet(model string) []AffinityKey {
	return s.Keys(model)
}

// ShouldRecordAffinity 判断一次成功转发是否需要写入亲和键。
//
// requestModel == "auto" 时返回 false：auto 请求经 SelectChannel 分流到 autoDistribute，
// 用 nextAutoChannel 轮询选路，从不查询亲和（见 middleware/distributor.go SelectChannel /
// autoDistribute）。因此 model 字段为 "auto" 的亲和键在任何读取路径下都不会被 Get 命中，
// 写入只会产生永不命中的死键——白占 maxEntries 配额并推高淘汰频率。
//
// 空串不在本函数的判定范围内：调用方 controller/relay.go 在取 requestModel 时已把空串
// 归一化为 "auto"，故是否传入空串由调用方决定，本函数只排除 "auto"。
func ShouldRecordAffinity(requestModel string) bool {
	return requestModel != "auto"
}

// buildAffinityKeyValue 构造键值：level 前缀 + group + userId 共同构成命名空间，
// 保证不同用户/分组不串号；id 为空串时即 user 层。
func buildAffinityKeyValue(level AffinityLevel, group string, userID int, model, id string) string {
	return fmt.Sprintf("%d|%s|%d|%s|%s", level, group, userID, model, id)
}
