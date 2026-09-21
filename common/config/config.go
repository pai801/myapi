package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pai801/myapi/common/env"

	"github.com/google/uuid"
)

var SystemName string
var ServerAddress = "http://localhost:3000"
var Footer = ""
var Logo = ""

func init() {
	SystemName = os.Getenv("SYSTEM_NAME")
	if SystemName == "" {
		SystemName = "MyApi"
	}
}

// Any options with "Secret", "Token" in its key won't be return by GetOptions

var SessionSecret = uuid.New().String()
var JWTExpiresIn = env.Int("JWT_EXPIRES_IN", 604800) // 默认 7 天
var JWTSecret = "myapi-jwt-secret"

var OptionMap map[string]string
var OptionMapRWMutex sync.RWMutex

var ItemsPerPage = 10
var MaxRecentItems = 100

var PasswordLoginEnabled = true
var TurnstileCheckEnabled = false

var DebugEnabled = strings.ToLower(os.Getenv("DEBUG")) == "true"
var DebugSQLEnabled = strings.ToLower(os.Getenv("DEBUG_SQL")) == "true"
var MemoryCacheEnabled = strings.ToLower(os.Getenv("MEMORY_CACHE_ENABLED")) == "true"


var TurnstileSiteKey = ""
var TurnstileSecretKey = ""

var LogConsumeEnabled = true

var ChannelDisableThreshold = 5.0
var AutomaticDisableChannelEnabled = false
var AutomaticEnableChannelEnabled = false
var ApproximateTokenEnabled = false
var RetryTimes = 0

var RootUserEmail = ""

var requestInterval, _ = strconv.Atoi(os.Getenv("POLLING_INTERVAL"))
var RequestInterval = time.Duration(requestInterval) * time.Second

var SyncFrequency = env.Int("SYNC_FREQUENCY", 10*60) // unit is second

var BatchUpdateEnabled = false
var BatchUpdateInterval = env.Int("BATCH_UPDATE_INTERVAL", 5)

var RelayTimeout = env.Int("RELAY_TIMEOUT", 0) // unit is second

// MaxLoggedBodySize 是消费日志中记录请求体的最大字节数，超过则只记录 "[body too large: N bytes]"
//
// 注意：本项只管消费日志（业务记录，默认 2MB 属合理量级），不控制 panic 恢复日志；
// 后者由 PanicLogBodyMaxBytes 单独控制，二者不是同一个开关，改一处时勿顺手改另一处。
var MaxLoggedBodySize = env.Int("MAX_LOGGED_BODY_SIZE", 2*1024*1024) // 默认 2MB

// PanicLogBodyMaxBytes 是 panic 恢复日志中打印请求体的最大字节数（对原始 body 计）。
//
// 与 MaxLoggedBodySize 不是同一个开关：MaxLoggedBodySize 管消费日志（业务记录，默认 2MB），
// 本项只管 panic 恢复日志（错误日志）。二者名字相近、都涉及「日志里的 body 大小」，极易混用。
//
// 本项默认取 256（远小于消费日志的 2MB）：panic 日志常被长期留存、采集到日志平台，
// 甚至在提 issue 时外传给第三方，比消费日志更敏感，故默认更保守。
// 需要更多排障上下文可显式调大，需要彻底关闭则设 0。
//
// 三档语义：
//   - > 0：按原始字节截断，最多记录前 N 字节，并标注原始总长度；
//   - == 0 或 < 0：完全不记录请求体内容，只记录长度 —— 应急关闭开关；
//   - 调大上限需知悉日志落盘风险：panic 日志通常被长期留存、被多人访问、被采集到日志平台，
//     上限越大越可能把请求体里的用户源码、提示词甚至凭据写进日志。
//
// 默认 256：凭据或敏感前缀往往出现在请求体开头，2048 足以把一整段凭据/敏感内容完整记录，
// 防泄漏效果不足；256 仍能让排障看出请求体的开头结构（是哪个接口、形态是否异常），
// 但不足以泄漏完整提示词或源码。需要更多上下文的运维可显式调大，需要彻底关闭则设 0。
var PanicLogBodyMaxBytes = env.Int("PANIC_LOG_BODY_MAX_BYTES", 256)

var GeminiSafetySetting = env.String("GEMINI_SAFETY_SETTING", "BLOCK_NONE")

// All duration's unit is seconds
// Shouldn't larger then RateLimitKeyExpirationDuration
var (
	GlobalApiRateLimitNum            = env.Int("GLOBAL_API_RATE_LIMIT", 480)
	GlobalApiRateLimitDuration int64 = 3 * 60

	GlobalWebRateLimitNum            = env.Int("GLOBAL_WEB_RATE_LIMIT", 240)
	GlobalWebRateLimitDuration int64 = 3 * 60

	UploadRateLimitNum            = 10
	UploadRateLimitDuration int64 = 60

	DownloadRateLimitNum            = 10
	DownloadRateLimitDuration int64 = 60

	CriticalRateLimitNum            = 20
	CriticalRateLimitDuration int64 = 20 * 60
)

var RateLimitKeyExpirationDuration = 20 * time.Minute

var EnableMetric = env.Bool("ENABLE_METRIC", false)
var MetricQueueSize = env.Int("METRIC_QUEUE_SIZE", 10)
var MetricSuccessRateThreshold = env.Float64("METRIC_SUCCESS_RATE_THRESHOLD", 0.8)
var MetricSuccessChanSize = env.Int("METRIC_SUCCESS_CHAN_SIZE", 1024)
var MetricFailChanSize = env.Int("METRIC_FAIL_CHAN_SIZE", 128)

var InitialRootToken = os.Getenv("INITIAL_ROOT_TOKEN")

var InitialRootAccessToken = os.Getenv("INITIAL_ROOT_ACCESS_TOKEN")

var GeminiVersion = env.String("GEMINI_VERSION", "v1")

var OnlyOneLogFile = env.Bool("ONLY_ONE_LOG_FILE", false)

var RelayProxy = env.String("RELAY_PROXY", "")
var UserContentRequestProxy = env.String("USER_CONTENT_REQUEST_PROXY", "")
var UserContentRequestTimeout = env.Int("USER_CONTENT_REQUEST_TIMEOUT", 30)

// TrustedProxies 逗号分隔的受信任代理 IP/CIDR；为空表示不信任任何代理，ClientIP 取直连地址
var TrustedProxies = env.String("TRUSTED_PROXIES", "")

var EnforceIncludeUsage = env.Bool("ENFORCE_INCLUDE_USAGE", false)
var TestPrompt = env.String("TEST_PROMPT", "Output only your specific model name with no additional text.")

var ChannelCooldownSeconds = env.Int("CHANNEL_COOLDOWN_SECONDS", 600)
// ChannelCooldownErrorThreshold 累计错误权重达到该阈值才进入冷却；权重 1 的错误需累计该次数
var ChannelCooldownErrorThreshold = env.Int("CHANNEL_COOLDOWN_ERROR_THRESHOLD", 3)
// ChannelCooldownErrorWindowSeconds 累计窗口，超过该时长未失败则计数清零
var ChannelCooldownErrorWindowSeconds = env.Int("CHANNEL_COOLDOWN_ERROR_WINDOW_SECONDS", 120)
var AffinityExpireSeconds = env.Int("AFFINITY_EXPIRE_SECONDS", 300)

// AffinityKeyMode 亲和键模式：auto = turn→session→user 分层键；user = 旧行为（一键回退开关）
var AffinityKeyMode = env.String("AFFINITY_KEY_MODE", "auto")

// AffinityTurnExpireSeconds turn 层（轮级）亲和 TTL
var AffinityTurnExpireSeconds = env.Int("AFFINITY_TURN_EXPIRE_SECONDS", 120)

// AffinitySessionExpireSeconds session 层（会话级）亲和 TTL
var AffinitySessionExpireSeconds = env.Int("AFFINITY_SESSION_EXPIRE_SECONDS", 1800)

// AffinityBodySessionID 控制 session 层亲和是否允许在「所有 session 头都未命中」时
// 回退到请求 body 提取会话标识（Cline / Roo / Kilo 的 litellm_session_id 等）。
//
// 存字符串而非 bool，以支持容错关闭判断：仅当值等于 "false"（忽略大小写与首尾空白）时
// 视为关闭，其余任何取值都视为开启（默认 "true"）。这是应急开关，静默失效等于没有，
// 故关闭判断必须容错（参考 AFFINITY_KEY_MODE 的处理）。
var AffinityBodySessionID = env.String("AFFINITY_BODY_SESSION_ID", "true")

// DefaultAffinityMaxEntries 是亲和表容量上限的默认值。作为单一真源，同时供
// AFFINITY_MAX_ENTRIES 的 env 默认值与 middleware.NewAffinityManager 的兜底共用，
// 避免在两处复制魔法数字 20000。
const DefaultAffinityMaxEntries = 20000

// AffinityMaxEntries 亲和表容量上限，触顶时先清过期、仍超限则按最早过期淘汰约 5%
var AffinityMaxEntries = env.Int("AFFINITY_MAX_ENTRIES", DefaultAffinityMaxEntries)

// AffinityStatsIntervalSeconds 亲和命中分布统计的输出周期（秒）。<=0 表示完全关闭统计：
// 埋点入口直接返回，零计数、零采样、零输出。用于上线后判断三级分层亲和改造是否生效。
//
// 只读启动值：仅在进程启动时由 env 注入，运行期不再改写；埋点侧直接读该变量（非原子）。
// 勿在运行期热更新此变量，否则与埋点的并发读构成数据竞争。
var AffinityStatsIntervalSeconds = env.Int("AFFINITY_STATS_INTERVAL_SECONDS", 300)
