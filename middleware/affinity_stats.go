package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/logger"
)

// affinityStats 是亲和查询的旁路观测器：只计数、不改选路、不返回任何值。
//
// 目的：上线后判断 turn → session → user 三级分层亲和改造是否真的生效、命中落在哪一层，
// 以及未命中时客户端到底发了哪些候选头（据此判断还缺哪些头）。
//
// 计数分六档且互斥完备：turn 命中 / 派生 turn 命中 / session 命中 / user 命中 / 回落（键命中但渠道不在候选集，
// 最终加权随机）/ 未命中（无键命中）。单列「回落」是因为键命中 ≠ 亲和生效，混计会高估改造效果。
// 派生 turn 必须与真实 turn 头分档：真实 turn 头与 body 派生 turn 是两种不同来源，混在一档就无法判断
// 「派生键是否真的轮内稳定复用」—— 这正是本次改造要验证的核心问题，也是把五档扩为六档的唯一目的。
//
// 约束：
//   - 计数全部走 sync/atomic，热路径无锁，埋点开销可忽略；
//   - 绝不记录 turn / session id 的原始值 —— 只记录「层级名」与「头名」，头值一律不进内存/日志；
//   - 不引入后台 goroutine（本项目反面教材见 relay/adaptor/chatgptsub/sticky.go 的 init() 无法停止的
//     Cleanup goroutine）。到点输出采用「惰性检查 + CAS 抢占」，由计数埋点自身触发。
type affinityStats struct {
	turnHits atomic.Int64

	// turnDerivedHits 记录「派生 turn 命中」：turn 层键由 body 派生（非真实 turn 头）且其渠道
	// 在候选集内被选中。与 turnHits 严格互斥 —— 同一次选路尝试只会进入其中一档，
	// 因此单独观测即可回答「派生键是否真的轮内稳定复用」。
	turnDerivedHits atomic.Int64

	sessionHits atomic.Int64
	// userHits 记录 user 层命中。注意：开启 AFFINITY_SESSION_EXCLUDES_USER（默认）后，有 session
	// 的请求不再读写 user 键，故本档只反映无会话标识客户端（turn-only 等）的 user 层兜底命中。
	userHits atomic.Int64

	// fallbacks 记录「亲和键命中、但该渠道不在本次候选集内 → 回落加权随机」的次数。
	// 必须与命中分开计数：键命中 ≠ 亲和生效，把此档混入命中会高估改造效果，
	// 使观测数据失去判断「改造是否真的生效」的意义。
	fallbacks atomic.Int64

	misses atomic.Int64

	// headerCounts[i] 累计「未命中请求中出现 affinityCandidateHeaders[i]」的次数。
	// 下标与 affinityCandidateHeaders 一一对应，天然有界（15 项），不存在无界增长。
	headerCounts []atomic.Int64

	// lastFlushNano 是上一次输出窗口的起点（UnixNano）；0 表示尚未初始化。
	lastFlushNano atomic.Int64
}

// affinityCandidateHeaders 是未命中采样关注的候选头全集：turn 头在前、session 头在后。
// 单一真源取自 affinity_scope.go 的既有头族常量，不复制清单，避免两处漂移。
var affinityCandidateHeaders = append(append([]string{}, affinityTurnHeaders...), affinitySessionHeaders...)

// affinityStatsNow 是时钟注入点，默认 time.Now；测试可替换以推进时间、无需 sleep。
var affinityStatsNow = time.Now

// affinityStatsGlobal 是生产路径使用的全局观测器。
var affinityStatsGlobal = newAffinityStats()

func newAffinityStats() *affinityStats {
	return &affinityStats{headerCounts: make([]atomic.Int64, len(affinityCandidateHeaders))}
}

// affinityStatsEnabled 表示统计是否开启（周期 > 0）。
// 关闭态下所有埋点在最入口直接返回：既不计数、也不采样，真正做到「关闭=零开销」——
// 避免在热路径（冷启动、无亲和用户、每会话首轮的非空头 miss）仍付出 Get + TrimSpace + 原子加的成本。
//
// 只读启动值：config.AffinityStatsIntervalSeconds 仅由启动时的 env 注入，运行期不写。
// 此处非原子读是安全的；若将来引入配置热更新，需先把它改为原子读（当前无此需求，勿过度设计）。
func affinityStatsEnabled() bool {
	return config.AffinityStatsIntervalSeconds > 0
}

// recordHit 记录一次亲和**真正生效**的命中：亲和键命中且其渠道在候选集内被选中。
// 按命中层级分别计数（各层独立，互不污染）。
//
// derived 仅在 level 为 turn 时有效：它表示该 turn 键来自 body 派生而非真实 turn 头。
// 非 turn 层不存在派生语义，一律忽略 derived，绝不进入派生档位（保证六档互斥不变式）。
func (s *affinityStats) recordHit(level AffinityLevel, derived bool) {
	if !affinityStatsEnabled() {
		return
	}
	switch level {
	case AffinityLevelTurn:
		// 真实 turn 头与派生 turn 分档：混计会让「派生键是否稳定复用」无法判断。
		if derived {
			s.turnDerivedHits.Add(1)
		} else {
			s.turnHits.Add(1)
		}
	case AffinityLevelSession:
		s.sessionHits.Add(1)
	default:
		s.userHits.Add(1)
	}
	s.maybeFlush()
}

// recordFallback 记录一次「亲和键命中但未生效」：键命中了，但该渠道不在本次候选集内，
// 最终回落加权随机选路。单列此档，避免与命中混计而高估亲和生效率。
func (s *affinityStats) recordFallback() {
	if !affinityStatsEnabled() {
		return
	}
	s.fallbacks.Add(1)
	s.maybeFlush()
}

// recordMiss 记录一次亲和未命中，并采样本次请求实际带了哪些候选头 —— 只记头名，绝不记头值。
// 判定「出现」的口径与亲和解析一致：TrimSpace 后非空才算（纯空白头视同缺失）。
func (s *affinityStats) recordMiss(h http.Header) {
	if !affinityStatsEnabled() {
		return
	}
	s.misses.Add(1)
	if h != nil {
		for i, name := range affinityCandidateHeaders {
			if strings.TrimSpace(h.Get(name)) != "" {
				s.headerCounts[i].Add(1)
			}
		}
	}
	s.maybeFlush()
}

// maybeFlush 惰性判断是否到达输出周期：到点则由一个 goroutine 通过 CAS 抢占并输出。
// interval <= 0 表示关闭：直接返回，既不初始化窗口也不输出。
//
// 选择「惰性检查」而非 ticker/后台 goroutine：无需管理 stop channel，进程退出不残留 goroutine。
// 代价是「到点」的实际输出发生在周期届满后的第一次埋点（无流量则本轮窗口顺延到有流量时输出）。
//
// 同一窗口只输出一行由末尾的 CompareAndSwap 保证：并发到达的多个埋点里只有一个 CAS 成功，
// 其余看到已被改写的 lastFlushNano 直接返回。切勿把该 CAS 改成 Store —— Store 会让并发埋点
// 全部通过检查，同一窗口输出多行（计数虽被 Swap(0) 清零不会重复，但汇总行会重复打印）。
func (s *affinityStats) maybeFlush() {
	// 非原子读启动值：AffinityStatsIntervalSeconds 运行期不写，此处读安全；若将来支持热更新需改原子读。
	interval := config.AffinityStatsIntervalSeconds
	if interval <= 0 {
		return
	}
	now := affinityStatsNow().UnixNano()
	last := s.lastFlushNano.Load()
	if last == 0 {
		// 首个埋点仅打点窗口起点，不输出，避免启动瞬间就打出一条空汇总。
		s.lastFlushNano.CompareAndSwap(0, now)
		return
	}
	if now-last < int64(time.Duration(interval)*time.Second) {
		return
	}
	// CAS 抢占：并发到达的多个埋点中只有一个负责输出，其余直接返回。
	if !s.lastFlushNano.CompareAndSwap(last, now) {
		return
	}
	// 输出**实际**窗口时长 (now-last)/1s，而非配置周期：惰性 flush 是空闲后首个埋点触发的，
	// 真实覆盖时长可能远大于配置值，用配置值冒充实际窗口会让观测失真。
	s.flush(int((now - last) / int64(time.Second)))
}

// flush 读取并重置全部计数，输出一行汇总日志。
//
// 重置策略选「输出后清零」而非滚动窗口：计数键固定且数量极小（6 个计数 + 15 个头名），
// 每个窗口相互独立、含义清晰；滚动窗口需要维护多桶状态，收益（平滑毛刺）对观测目的不必要。
//
// 每个计数器用 Swap(0) 原子「读并清零」，因此并发期间任何在 Swap 之前完成的 Add 都会被本窗口
// 计入、不会丢失（跨计数器的快照不是同一瞬时，对分布观测可接受）。
func (s *affinityStats) flush(windowSeconds int) {
	turn := s.turnHits.Swap(0)
	turnDerived := s.turnDerivedHits.Swap(0)
	session := s.sessionHits.Swap(0)
	user := s.userHits.Swap(0)
	fallbacks := s.fallbacks.Swap(0)
	misses := s.misses.Swap(0)

	var b strings.Builder
	// 窗口标签写实际时长；六档口径写清含义，不让人对着裸数字猜。
	b.WriteString("affinity stats (window ")
	b.WriteString(strconv.Itoa(windowSeconds))
	b.WriteString("s): hit{turn=")
	b.WriteString(strconv.FormatInt(turn, 10))
	// turn_derived 固定紧跟 turn：派生档与真实 turn 档相邻，便于一眼对照两者占比。
	b.WriteString(", turn_derived=")
	b.WriteString(strconv.FormatInt(turnDerived, 10))
	b.WriteString(", session=")
	b.WriteString(strconv.FormatInt(session, 10))
	b.WriteString(", user=")
	b.WriteString(strconv.FormatInt(user, 10))
	b.WriteString("}(亲和键命中且渠道在候选集内被选中)")
	b.WriteString(" fallback=")
	b.WriteString(strconv.FormatInt(fallbacks, 10))
	b.WriteString("(亲和键命中但渠道不在候选集,已回落加权随机;含重试时上次失败渠道被 filterLastFailedChannel 剔除的情形)")
	b.WriteString(" miss=")
	b.WriteString(strconv.FormatInt(misses, 10))
	b.WriteString("(无亲和键命中)")
	// 未命中请求中各候选头出现的次数；只输出头名与计数，绝不输出头值。
	b.WriteString("; miss_headers{")
	for i, name := range affinityCandidateHeaders {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		b.WriteString("=")
		// 下标守卫：正常情况下 headerCounts 与 affinityCandidateHeaders 等长（二者均在包初始化时定长，
		// 运行期不变），故永不越界。此处仅兜底「将来有人往头族常量 append 致两者错位」的极端情形，
		// 使其退化为记 0 而非 index out of range panic —— 观测器绝不该拖垮主链路。
		var count int64
		if i < len(s.headerCounts) {
			count = s.headerCounts[i].Swap(0)
		}
		b.WriteString(strconv.FormatInt(count, 10))
	}
	b.WriteString("}(未命中请求中各候选头出现次数,仅头名不记值)")
	// 口径澄清一：仅统计「非 auto 选路、且存在候选渠道」的选路尝试。
	// auto 请求与「模型无任何候选渠道」的提前 return（在亲和 Get 之前返回）都不在此列，
	// 因此本汇总与总请求数对账时天然对不上，需按此口径解释。
	// 口径澄清二：重试路径每次重试都重新选路并各计一次，故计数是「选路尝试次数」而非「请求数」。
	b.WriteString("; 口径=选路尝试次数(仅非 auto 且存在候选渠道的选路尝试;重试每次各计一次)")
	logger.Log.Infof("%s", b.String())
}
