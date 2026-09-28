// Package taskscheduler 提供可复用的旁路任务调度骨架（记录层 + 异步执行纪律 + 生命周期层）。
//
// 设计（openspec/changes/common-task-scheduler/design.md D1）：
// 调用方以 TaskSpec 声明任务（ID / 进程级开关 / 本地时区整点小时列表 / 执行体）。生产路径经**包级
// 单例**装配：各任务来源包 Register(自身声明) 后幂等调用 Start，进程内只有一套 Scheduler、一套
// 后台生命周期与一套排程循环；New 保留为实例级装配入口（供测试与兼容），包级入口是其薄封装。
// 单例的排程循环每轮动态读取全部已注册任务，故首次 Start 之后注册的任务会被后续轮次纳入。由此
// 消除「每新增一个定时任务就复制一套调度骨架」的重复。
//
// 三层职责：
//   - **记录层**：按任务 ID 的单飞槽位（current / lastCompleted / nextFire）、值拷贝快照、
//     RunState / RunRef / RunRecord；
//   - **异步执行纪律**：Submit 提交即返回，执行 goroutine 不绑定触发请求 ctx、必 recover panic
//     并记为该轮失败、手动与定时共享同一单飞锁；
//   - **生命周期层**：Start 幂等（只首个生效、信号处理器只注册一次）、waitForDB 非致命等待、
//     本地时区整点排程（到点把名义整点经 RunOptions.NominalFire 透传）、无排程 park 不空转。
//
// 持久化红线：轮次状态仅存进程内内存（锁 + 结构体），MUST NOT 建表 / 加列 / 落盘；每项任务只保留
// 「当前运行」与「最近一次完成」两份记录，更早历史可丢弃，进程重启丢失历史可接受。故本文件不
// 导入任何数据层 / 文件系统包（见 scheduler_test.go 的导入白名单结构断言）。
//
// 通用层不解释具体结果类型：执行体经 any 承载各任务自己的报告，JSON 形状由各包保证（D1/D2）。
package taskscheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/redact"
)

// ── 轮次状态与哨兵错误 ─────────────────────────────────────────

// RunState 是一轮的运行状态：运行中或已完成。
type RunState string

const (
	RunStateRunning  RunState = "running"
	RunStateFinished RunState = "finished"
)

// ErrBusy 表示该任务已有轮次在运行（单飞冲突），调用方应映射为 HTTP 409 并回带当前轮次标识。
// ErrDisabled 表示进程级总闸关闭，调用方应映射为「未启用」语义而非错误（MUST NOT 返回 5xx）。
//
// 二者是哨兵错误，供上层用 errors.Is 判定。
var (
	ErrBusy     = errors.New("taskscheduler: task already running")
	ErrDisabled = errors.New("taskscheduler: task disabled")
)

// errUnknownTask 表示 Submit 收到的任务 ID 未注册（装配期编程错误，非运行期业务状态）。
var errUnknownTask = errors.New("taskscheduler: unknown task")

// RunRef 是一轮运行的轻量引用（提交时立即返回给调用方）。
type RunRef struct {
	RunID     string    `json:"run_id"`
	State     RunState  `json:"state"`
	StartedAt time.Time `json:"started_at"`
}

// RunRecord 是一轮运行的完整记录：轮次标识、起止时刻、执行体结果与（脱敏后的）错误摘要。
//
// Result 承载各任务自己的报告（逐渠道/逐账号明细与汇总计数），通用层**不解释**其类型；
// 各包 MUST 保证其 JSON 形状且不含凭据（D1/D2）。错误摘要由本层脱敏 + 有界（见 safeRunSummary）。
type RunRecord struct {
	RunID      string    `json:"run_id"`
	State      RunState  `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Result     any       `json:"result,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// TaskSnapshot 是单个任务的对外快照：开关、下次触发、当前运行、最近一次完成。
//
// MUST NOT 含任何凭据字段（快照类型层面的结构性保证见 scheduler_test.go 的字段名断言）。
type TaskSnapshot struct {
	Enabled       bool       `json:"enabled"`
	NextFire      time.Time  `json:"next_fire"`
	Current       *RunRef    `json:"current,omitempty"`
	LastCompleted *RunRecord `json:"last_completed,omitempty"`
}

// RunOptions 是执行体的运行选项。
//
// NominalFire 是触发本轮的**名义时刻**（本地时区整点），由排程循环在到点时传入；手动触发为零值。
// 通用层不解释该字段，仅透传——它是 autoclaw 维护轮按账号节流基准正确性的前提（D4/A3）。
type RunOptions struct {
	NominalFire time.Time
}

// ── 任务声明与装配 ─────────────────────────────────────────────

// TaskSpec 是一项任务的声明（注册输入）。
type TaskSpec struct {
	// ID 是稳定标识，供状态快照与手动触发入口按标识寻址（同一调度器内 MUST 唯一）。
	ID string
	// Enabled 是进程级总闸（读 env，保持各任务既有语义）；nil 视为关闭。
	Enabled func() bool
	// Hours 是本地时区整点小时列表（空 = 不排程）；nil 视为不排程。
	Hours func() []int
	// Run 是执行体，结果经 any 承载（JSON 形状由各包保证）；nil 记为该轮失败。
	Run func(ctx context.Context, opts RunOptions) (any, error)
}

// Options 是调度器的装配选项。
type Options struct {
	// DataStoreReady 报告数据层是否就绪；nil 视为恒就绪（生产由装配处注入真实判据）。
	// MUST NOT 在装配阶段（New）被调用。
	DataStoreReady func() bool
	// DBReadyPoll 是数据层就绪的轮询间隔（<=0 取默认 2s）。
	DBReadyPoll time.Duration
	// DBReadyMax 是等待数据层就绪的总时长上限（<=0 取默认 5min），超时告警后非致命退出。
	DBReadyMax time.Duration
	// Now 是可注入时钟（nil 取 time.Now），排程重算、到点判定与轮次时刻统一读取它。
	Now func() time.Time
}

const (
	defaultDBReadyPoll = 2 * time.Second
	defaultDBReadyMax  = 5 * time.Minute
)

// normalizeOptions 补齐零值默认，保证生产行为与各包既有实现逐字一致。
func normalizeOptions(opts Options) Options {
	if opts.DataStoreReady == nil {
		opts.DataStoreReady = func() bool { return true }
	}
	if opts.DBReadyPoll <= 0 {
		opts.DBReadyPoll = defaultDBReadyPoll
	}
	if opts.DBReadyMax <= 0 {
		opts.DBReadyMax = defaultDBReadyMax
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return opts
}

// Scheduler 装配多个 TaskSpec，统一排程、统一快照、统一生命周期。
//
// 生产路径由**包级单例**承载（见 Register / Start）：各任务来源经 Register 追加声明、经 Start
// 幂等启动，进程内只有一套排程循环与生命周期。New 保留为实例级装配入口（供测试与兼容），
// 包级入口是其薄封装。各任务来源 MUST NOT 各自 New + Start。
type Scheduler struct {
	// regMu 保护 slots / byID：Register 可在排程循环运行期间追加任务，故 loop / Snapshot / submit
	// 的读取必须与追加互斥（动态任务集）。
	regMu sync.RWMutex
	slots []*taskSlot
	byID  map[string]*taskSlot
	opts  Options

	// startMu 串行化 Start 的「检查并安装」，并保护 ctx / started 的读写（Submit 可能在 Start
	// 之前或并发调用，故 processContext 亦经它读取）。
	startMu sync.Mutex
	started bool
	ctx     context.Context
	// done 在后台生命周期（start）返回时关闭，作为「后台已完全退出」的可观测信号（测试隔离用）。
	done chan struct{}

	// wake 在 Register 追加任务后唤醒可能处于 park 或长 timer 等待的排程循环，使其立即重算并
	// 纳入新任务；容量 1 且非阻塞发送，多次注册合并为一次唤醒（动态任务集）。
	wake chan struct{}

	// watchSignals 是派生「进程退出信号 ctx」的注入缝（生产恒为 watchExitSignals）。
	watchSignals func(context.Context) context.Context

	// 生命周期层的测试注入点（生产为零值，行为零变更）。
	onStart  func()
	onLoop   func()
	parkHook func()
}

// taskSlot 是单项任务的独立单飞槽位：互斥锁 + 当前运行 + 最近一次完成 + 下次触发。
type taskSlot struct {
	mu            sync.RWMutex
	spec          TaskSpec
	current       *RunRef
	lastCompleted *RunRecord
	nextFire      time.Time
}

// New 一次性装配调度器；MUST NOT 在此访问数据层（就绪判据只在 Start 后的后台路径调用）。
//
// specs 中 ID 重复时只保留**首次**出现的声明（装配期编程错误，静默去重以避免重复排程）。
func New(specs []TaskSpec, opts Options) *Scheduler {
	s := &Scheduler{
		byID:         make(map[string]*taskSlot, len(specs)),
		opts:         normalizeOptions(opts),
		done:         make(chan struct{}),
		wake:         make(chan struct{}, 1),
		watchSignals: watchExitSignals,
	}
	s.appendSpecs(specs...)
	return s
}

// appendSpecs 在注册锁内把声明追加进任务集，返回是否新增了任务。
//
// 同 ID 采用**幂等去重（首次出现者生效）**：与 New 既有口径一致，避免同一任务被重复排程。
// 返回是否新增用于决定是否需要唤醒排程循环（纯重复注册不唤醒）。
func (s *Scheduler) appendSpecs(specs ...TaskSpec) bool {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	added := false
	for _, spec := range specs {
		if _, dup := s.byID[spec.ID]; dup {
			continue
		}
		slot := &taskSlot{spec: spec}
		s.slots = append(s.slots, slot)
		s.byID[spec.ID] = slot
		added = true
	}
	return added
}

// register 追加声明并唤醒可能处于 park / 长 timer 等待的排程循环，使其重算并纳入新任务。
func (s *Scheduler) register(specs ...TaskSpec) {
	if s.appendSpecs(specs...) {
		s.notifyWake()
	}
}

// notifyWake 非阻塞唤醒排程循环：容量 1，多次注册合并为一次唤醒（无等待者时丢弃）。
func (s *Scheduler) notifyWake() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// slotsSnapshot 返回任务切片的只读快照，使排程循环 / 快照 / 就绪判定在注册追加时无数据竞争。
func (s *Scheduler) slotsSnapshot() []*taskSlot {
	s.regMu.RLock()
	defer s.regMu.RUnlock()
	out := make([]*taskSlot, len(s.slots))
	copy(out, s.slots)
	return out
}

// slotByID 在注册锁内按 ID 取槽位（未注册返回 nil）。
func (s *Scheduler) slotByID(id string) *taskSlot {
	s.regMu.RLock()
	defer s.regMu.RUnlock()
	return s.byID[id]
}

// ── 包级单例与注册入口（C1 §4.1）──────────────────────────────
//
// 生产路径由包级单例承载（design D1）：各任务来源包经 Register 登记自身声明，再幂等调用 Start；
// 进程内只有一套排程循环与一套生命周期。单例的排程循环动态读取全部已注册任务，故首次 Start 之后
// 注册的任务（如 W3 新增的 traetask）会被后续轮次纳入，无需重启调度器。
//
// 注册与启动 MUST NOT 访问数据层：Configure 只保存装配选项，New / appendSpecs 不调用 DataStoreReady，
// 就绪判据只在 Start 之后的 waitForDB 后台路径读取（spec「DB 就绪等待与信号退出」）。

var (
	// singletonMu 保护 singleton 的惰性创建与读取。
	singletonMu sync.Mutex
	// singleton 是进程级唯一调度器；首次 Register / Start 时按 singletonOpts 惰性创建。
	singleton *Scheduler
	// singletonOpts 是单例的装配选项，由 Configure 在首次 Start 之前设置。
	singletonOpts Options
)

// Configure 设置包级单例的装配选项（数据层就绪判据、轮询间隔/上限、时钟注入）。
//
// 生产装配处 MUST 在首次 Start 之前调用，使单例经 waitForDB 等待数据层就绪；未调用时用零值默认
// （DataStoreReady 恒就绪、时钟为 time.Now）。首次 Start 之后调用**不重启生命周期**，也不改写
// 正在被排程循环读取的选项（避免数据竞争）。
//
// MUST NOT 在此访问数据层：仅保存选项，就绪判据延后到后台路径读取。
func Configure(opts Options) {
	singletonMu.Lock()
	defer singletonMu.Unlock()
	singletonOpts = opts
	if singleton == nil {
		return
	}
	// 仅在尚未 Start 时改写选项：Start 之后 opts 会被后台循环读取，改写将构成数据竞争。
	singleton.startMu.Lock()
	if !singleton.started {
		singleton.opts = normalizeOptions(opts)
	}
	singleton.startMu.Unlock()
}

// singletonInstance 返回包级单例，首次调用时按当前 singletonOpts 惰性创建。
func singletonInstance() *Scheduler {
	singletonMu.Lock()
	defer singletonMu.Unlock()
	if singleton == nil {
		singleton = New(nil, singletonOpts)
	}
	return singleton
}

// Register 向包级单例追加任务声明（可多次调用、累积）。
//
// 同 ID 采用**幂等去重（首次出现者生效）**，与 New 口径一致；追加后唤醒排程循环，使新任务被后续
// 轮次纳入——含首次 Start 之后注册的任务（不必重启调度器）。MUST NOT 在注册阶段访问数据层。
func Register(specs ...TaskSpec) {
	singletonInstance().register(specs...)
}

// Start 启动包级单例（首次调用生效、重复调用幂等早退）。
//
// 薄封装 (*Scheduler).Start：并发/重复调用只有首个成功安装者启动一套后台生命周期与一套信号处理器。
func Start(ctx context.Context) {
	singletonInstance().Start(ctx)
}

// Submit 经包级单例异步提交一次手动触发（薄封装 (*Scheduler).Submit）。
//
// 供各任务来源包的 Run*Now 直接委托，无需 registry 注入（design D2）。
func Submit(id string) (RunRef, error) {
	return singletonInstance().Submit(id)
}

// Snapshot 返回包级单例的凭据安全值拷贝快照（薄封装 (*Scheduler).Snapshot）。
//
// 供各任务来源包的 TasksStatus 直接委托（design D2）；尚未 Register 任何任务时返回空快照，不 panic。
func Snapshot() map[string]TaskSnapshot {
	return singletonInstance().Snapshot()
}

// now 返回调度器当前时间（测试可注入时钟）。
func (s *Scheduler) now() time.Time {
	if s.opts.Now == nil {
		return time.Now()
	}
	return s.opts.Now()
}

// enabled 报告任务的进程级总闸取值；nil 开关视为关闭。
func (t *taskSlot) enabled() bool {
	if t.spec.Enabled == nil {
		return false
	}
	return t.spec.Enabled()
}

// hours 返回任务的整点小时列表；nil 视为不排程。
func (t *taskSlot) hours() []int {
	if t.spec.Hours == nil {
		return nil
	}
	return t.spec.Hours()
}

// ── 记录层：单飞槽位与值拷贝快照 ───────────────────────────────

// startRun 原子地占用槽位：空闲则把 current 置为 run 并返回之；已有轮次在运行则**不改动**槽位，
// 返回当前轮次引用与 ErrBusy（供 409 回带当前轮次标识）。
func (t *taskSlot) startRun(run RunRef) (RunRef, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current != nil {
		return *t.current, ErrBusy
	}
	ref := run
	t.current = &ref
	return ref, nil
}

// finishRun 完成一轮：把本轮记录迁移为「最近一次完成」并清空 current。
//
// 记录以值拷贝存入（在**加锁前**完成，缩短持锁时间）；只保留最近一次，更早的完成轮次被覆盖丢弃。
func (t *taskSlot) finishRun(record RunRecord) {
	cloned := cloneRunRecord(&record)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastCompleted = cloned
	t.current = nil
}

// setNextFire 写入任务的下次触发时刻（由 loop 调用）。
func (t *taskSlot) setNextFire(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextFire = at
}

// snapshot 在读锁保护下生成完全脱离内部状态的值拷贝快照。
func (t *taskSlot) snapshot() TaskSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return TaskSnapshot{
		Enabled:       t.enabled(),
		NextFire:      t.nextFire,
		Current:       cloneRunRef(t.current),
		LastCompleted: cloneRunRecord(t.lastCompleted),
	}
}

// cloneRunRef 返回 RunRef 的值拷贝（RunRef 无引用字段，浅拷贝即完全隔离）。
func cloneRunRef(ref *RunRef) *RunRef {
	if ref == nil {
		return nil
	}
	cp := *ref
	return &cp
}

// cloneRunRecord 返回 RunRecord 的值拷贝：结构体与时间字段完全隔离，调用方改写快照不回写内部。
//
// Result 是 any，通用层无法做类型无关的深拷贝；其隔离性由各包保证——执行体返回的 Result MUST 是
// 不可变快照值（各包自己的报告类型），通用层只透传（D1/D2）。
func cloneRunRecord(rec *RunRecord) *RunRecord {
	if rec == nil {
		return nil
	}
	cp := *rec
	return &cp
}

// runIDSeq 是进程内单调递增的轮次序号，与时间戳共同保证 RunID 在进程内唯一。
var runIDSeq atomic.Uint64

// newRunRef 创建一轮运行引用（RunID 形如 `<UTC纳秒时间戳>-<递增序号>`）。
func newRunRef(now time.Time) RunRef {
	return RunRef{
		RunID:     fmt.Sprintf("%s-%d", now.UTC().Format("20060102T150405.000000000"), runIDSeq.Add(1)),
		State:     RunStateRunning,
		StartedAt: now,
	}
}

// ── 异步执行纪律：Submit / execute ─────────────────────────────

// Submit 异步提交一次手动触发（名义时刻为零值），语义见 submit 的三态映射。
//
// 前置条件：装配处 MUST 在暴露本入口之前调用 Start；尚未 Start 时执行体收到的上下文回落
// context.Background()（无进程退出信号），但提交与单飞语义不变。
func (s *Scheduler) Submit(id string) (RunRef, error) {
	return s.submit(id, time.Time{})
}

// submit 原子地返回 ErrDisabled、ErrBusy（回带当前轮次）或新一轮引用。
//
// 语义（spec「任务级单飞」「异步执行纪律」）：
//   - 未注册 ID → 编程错误，返回包装 errUnknownTask 的 error；
//   - 进程级总闸关闭 → ErrDisabled（总闸优先于一切）；
//   - 已有轮次在运行 → 返回 (当前运行中 RunRef, ErrBusy)，供上层映射 409 并回带 run_id；
//   - 空闲 → 原子占位后启动后台 goroutine 并**立即**返回 (新 RunRef, nil)。
//
// busy 时**绝不**等待 / 排队：不阻塞调用方，也不启动第二个实例。
func (s *Scheduler) submit(id string, nominal time.Time) (RunRef, error) {
	slot := s.slotByID(id)
	if slot == nil {
		return RunRef{}, fmt.Errorf("%w: %q", errUnknownTask, id)
	}
	if !slot.enabled() {
		return RunRef{}, ErrDisabled
	}
	ref, err := slot.startRun(newRunRef(s.now()))
	if err != nil {
		// ErrBusy：ref 是当前运行中轮次，原样回带给 409 响应体。
		return ref, err
	}
	// 只把进程 ctx 交给执行 goroutine；调用方（HTTP 请求）立即返回，互不牵连。
	go s.execute(slot, ref, nominal)
	return ref, nil
}

// execute 记录一轮完成或已恢复的 panic，并只保留最近一次完成记录。
//
// 三条不可动摇的纪律：
//  1. 执行 goroutine **不绑定触发请求 ctx**——它只接收 s.processContext()（进程退出信号 ctx），
//     请求返回后任务继续跑完，仅进程退出可令其在渠道间隔处提前收尾；
//  2. MUST recover panic：panic 记为该轮次失败，**不得**崩溃进程；
//  3. 手动（Submit）与定时（loop 的 dispatch）共用本入口，故天然共享同一任务级单飞槽位。
func (s *Scheduler) execute(slot *taskSlot, ref RunRef, nominal time.Time) {
	record := RunRecord{
		RunID:     ref.RunID,
		State:     RunStateFinished,
		StartedAt: ref.StartedAt,
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			record.Result = nil
			record.Error = panicSafeSummary(recovered)
			record.FinishedAt = s.now()
			// **本顺序承重**：日志读必须在 finishRun 解锁之前完成，观察者经同一槽位锁建立
			// happens-before，把此处对全局 logger 的读排在任何后续测试改写 logger 之前。
			logger.Log.Errorf("taskscheduler: task %s run %s panicked: %s", slot.spec.ID, ref.RunID, record.Error)
			slot.finishRun(record)
		}
	}()

	if slot.spec.Run == nil {
		record.Error = errorSafeSummary(errors.New("taskscheduler: task has no run function"))
		record.FinishedAt = s.now()
		slot.finishRun(record)
		return
	}

	result, err := slot.spec.Run(s.processContext(), RunOptions{NominalFire: nominal})
	record.Result = result
	record.FinishedAt = s.now()
	if err != nil {
		record.Error = errorSafeSummary(err)
	}
	slot.finishRun(record)
}

// processContext 返回执行 goroutine 应绑定的上下文：优先用 Start 注入的进程退出信号 ctx，
// 尚未注入时回落 context.Background()。**绝不**返回触发请求 ctx。
func (s *Scheduler) processContext() context.Context {
	s.startMu.Lock()
	ctx := s.ctx
	s.startMu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// ── 快照 ───────────────────────────────────────────────────────

// Snapshot 返回各任务的凭据安全值拷贝快照（键为任务 ID）。
//
// 每项任务含：进程级开关取值、下次触发时刻、当前运行中轮次（若有）与最近一次完成轮次（若有）。
// 尚未 Start 或尚无轮次时对应字段为零值，MUST NOT panic。
func (s *Scheduler) Snapshot() map[string]TaskSnapshot {
	slots := s.slotsSnapshot()
	out := make(map[string]TaskSnapshot, len(slots))
	for _, slot := range slots {
		out[slot.spec.ID] = slot.snapshot()
	}
	return out
}

// ── 生命周期层：Start / waitForDB / loop ───────────────────────

// Start 启动非阻塞的后台生命周期（DB 等待 + 排程循环），并为本次运行派生信号 ctx。
//
// 「只一次」由 startMu 内的**原子检查并安装**保证：并发/重复调用时，只有首个成功安装者启动后台
// 生命周期，后来者立即返回，绝不重复启动循环、绝不重复注册信号处理器。
//
// **信号 ctx 在包内自建**（见 signals.go）：传入 ctx 作为父 ctx，经 watchSignals 派生出一个
// 「进程收到第一个中断/终止信号即取消」的子 ctx，供排程循环与执行体使用。上层因此只需传中性 ctx。
//
// **关键顺序**：s.ctx 必须在启动后台 goroutine **之前**赋值（同一临界区内完成），使任何经
// processContext 读取的调用方看到的 ctx 已是最终值，杜绝数据竞争。
//
// 非阻塞：安装完成后立即返回。ctx 为 nil 时回落 Background，避免后台 select 空指针。
func (s *Scheduler) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return
	}
	// 仅在**首个**安装者路径上派生信号 ctx：重复 Start 早退，不会重复注册信号处理器。
	watch := s.watchSignals
	if watch == nil {
		watch = watchExitSignals
	}
	runCtx := watch(ctx)
	if runCtx == nil {
		runCtx = context.Background()
	}
	s.ctx = runCtx
	s.started = true
	s.startMu.Unlock()

	go s.start(runCtx)
}

// start 负责 DB 就绪等待、启动可观测与单一排程循环。
//
// 三条纪律：
//  1. DB 未就绪（等待超时 / ctx 取消）即**非致命**返回，不影响进程其他能力；
//  2. 就绪后**一次性**打印生效时区与各任务的开关/小时取值（启动可观测）；
//  3. 全部任务关闭时**不进入** timer 循环——只等待退出信号或新注册唤醒，杜绝空转唤醒。
//
// **动态任务集**：全部任务关闭时进入 park，但 Register 追加任务会经 wake 唤醒本驱动重新判定；
// 若新任务启用则进入 loop 纳入排程（支撑「首个启动之后注册的任务被后续轮次纳入」）。
//
// 返回前关闭 s.done，作为「后台生命周期已完全退出」的可观测信号（测试在恢复全局 logger 之前
// 等待它，消除数据竞争）。本方法对每个实例至多被调用一次（Start 的「只一次」保证）。
func (s *Scheduler) start(ctx context.Context) {
	defer close(s.done)
	if s.onStart != nil {
		s.onStart()
	}
	if !s.waitForDB(ctx) {
		return
	}

	logger.Log.Infof("taskscheduler: scheduler starting: timezone=%s tasks=%s",
		time.Local.String(), s.describeTasks())

	for {
		if s.anyEnabled() {
			s.loop(ctx)
			return
		}
		// 全部任务关闭：不排程、不唤醒，等退出信号或新注册唤醒后重新判定。
		if !s.park(ctx) {
			return
		}
	}
}

// describeTasks 生成启动可观测行的任务摘要（开关 + 小时列表，不含任何凭据）。
func (s *Scheduler) describeTasks() string {
	slots := s.slotsSnapshot()
	parts := make([]string, 0, len(slots))
	for _, slot := range slots {
		parts = append(parts, fmt.Sprintf("%s{enabled=%t,hours=%v}", slot.spec.ID, slot.enabled(), slot.hours()))
	}
	return strings.Join(parts, " ")
}

// anyEnabled 报告是否有任一任务的进程级总闸开启。
func (s *Scheduler) anyEnabled() bool {
	for _, slot := range s.slotsSnapshot() {
		if slot.enabled() {
			return true
		}
	}
	return false
}

// park 在阻塞等待退出信号之前调用一次 parkHook（若注入），随后阻塞至 ctx 取消或新注册唤醒。
//
// 返回值报告是否被 Register 唤醒（true → 调用方应重算并纳入新任务）；ctx 取消返回 false。
//
// 抽成方法是为了让 start 与 loop 的两条 park 分支共享同一「进入 park 即计数一次」的语义：正确实现
// 计数恒为 1；自旋实现会反复回到 park 分支 → 计数持续增长，测试据此可证伪空转。
func (s *Scheduler) park(ctx context.Context) bool {
	if s.parkHook != nil {
		s.parkHook()
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	}
}

// waitForDB 按固定轮询周期等待数据层就绪，直到就绪、ctx 取消或达到最大等待。
//
// 语义：
//   - 首次检查**立即执行**（不得先等一个轮询间隔）——数据层可能早已就绪；
//   - 未就绪则按 DBReadyPoll 周期重试；
//   - 总等待达到 DBReadyMax 仍不可用 → WARN 后返回 false（**非致命**，绝不 panic/退出进程）；
//   - ctx 取消 → 立即返回 false。
func (s *Scheduler) waitForDB(ctx context.Context) bool {
	if s.opts.DataStoreReady() {
		return true
	}

	poll := time.NewTimer(s.opts.DBReadyPoll)
	defer poll.Stop()
	maxWait := time.NewTimer(s.opts.DBReadyMax)
	defer maxWait.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-maxWait.C:
			logger.Log.Warnf("taskscheduler: datastore not ready after %s; scheduler will not start (non-fatal)", s.opts.DBReadyMax)
			return false
		case <-poll.C:
			if s.opts.DataStoreReady() {
				return true
			}
			// poll.C 已被消费，直接 Reset 不会丢失事件。
			poll.Reset(s.opts.DBReadyPoll)
		}
	}
}

// loop 等待配置的本地时区整点触发；ctx 取消即停止派发。
//
// 每轮以 now 的本地时区重算**全部已注册任务**的下一次触发（NextFire）并写入槽位供状态查询；等到
// 最近时点后经 submit（与手动触发共享同一单飞锁）触发。全部任务均无有效时点（小时列表为空）时
// **不空转**，进入 park 等待退出信号或新注册唤醒。到点后以**实际**当前时间判定哪些任务已到期，
// 并把「本轮算出的名义整点」经 RunOptions.NominalFire 透传（D4）。
//
// **动态任务集**：Register 追加任务会经 wake 唤醒本循环，使其立即重算并纳入新任务——包括在长
// timer 等待期间注册的情形（不必等到旧时点）。
func (s *Scheduler) loop(ctx context.Context) {
	if s.onLoop != nil {
		s.onLoop()
	}
	for {
		now := s.now()

		slots := s.slotsSnapshot()
		fires := make([]time.Time, len(slots))
		candidates := make([]time.Time, 0, len(slots))
		for i, slot := range slots {
			var next time.Time
			if slot.enabled() {
				next = NextFire(now, slot.hours())
			}
			slot.setNextFire(next)
			fires[i] = next
			if !next.IsZero() {
				candidates = append(candidates, next)
			}
		}

		wait, scheduled := nearestFireWait(now, candidates...)
		if !scheduled {
			// 无任何可排程时点（小时列表为空）：不周期性唤醒，等退出信号或新注册唤醒。
			if !s.park(ctx) {
				return
			}
			// 被新注册唤醒：重算并纳入新任务。
			continue
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			// 新注册任务：放弃旧时点，立即重算以纳入新任务。
			timer.Stop()
			continue
		case <-timer.C:
		}

		// 到点后以**实际**当前时间判定哪些任务已到期（多任务可能同一时点触发）。
		due := s.now()
		for i, slot := range slots {
			fire := fires[i]
			if fire.IsZero() || fire.After(due) {
				continue
			}
			if slot.enabled() {
				s.dispatch(slot.spec.ID, fire)
			}
		}
	}
}

// dispatch 经共享单飞入口触发一次任务；忙（ErrBusy）或总闸关闭（ErrDisabled）都只记录，
// **绝不阻塞**后续排程轮次。nominal 为本轮名义触发时刻。
func (s *Scheduler) dispatch(id string, nominal time.Time) {
	if _, err := s.submit(id, nominal); err != nil {
		logger.Log.Debugf("taskscheduler: timer trigger for %s skipped: %v", id, err)
	}
}

// ── 纯函数：整点计算与最近时点 ─────────────────────────────────

// NextFire 返回 now 之后（含 now 本身）最近的本地时区整点触发时刻：
// 逐个配置小时取当日该时刻，若已早于 now 则顺延至次日同小时，再取其中最小值。
// hours 为空表示该任务不参与排程，返回零值 time.Time{}。返回值保留 now 的 Location。
//
// 前置条件：hours 中每个元素 MUST ∈ [0,23]（校验由各包 env 解析负责）；NextFire 本身不做防御性
// 校验——越界值会被 time.Date 归一化，可能落在过去而引发忙循环，故调用方 MUST 保证入参合法。
func NextFire(now time.Time, hours []int) time.Time {
	if len(hours) == 0 {
		return time.Time{}
	}

	loc := now.Location()
	var next time.Time
	set := false
	for _, hour := range hours {
		candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
		if candidate.Before(now) {
			candidate = candidate.AddDate(0, 0, 1)
		}
		if !set || candidate.Before(next) {
			next = candidate
			set = true
		}
	}
	return next
}

// nearestFireWait 返回 now 到最近一个非零触发时点的等待时长；候选全为零值时 ok=false
// （表示无任何排程，调用方应只等退出信号而非空转）。
func nearestFireWait(now time.Time, fires ...time.Time) (time.Duration, bool) {
	var (
		best time.Duration
		ok   bool
	)
	for _, fire := range fires {
		if fire.IsZero() {
			continue
		}
		if d := fire.Sub(now); !ok || d < best {
			best = d
			ok = true
		}
	}
	return best, ok
}

// ── 错误摘要脱敏与有界 ─────────────────────────────────────────

// maxRunErrorSummaryBytes 是轮次错误摘要的字节上限（含 panic 摘要）。
//
// 轮次记录会经状态快照对外序列化，有界可防止上游/panic 文案无限膨胀。
const maxRunErrorSummaryBytes = 200

// runSummaryEscaper 把裸 CR/LF 转为可见转义，避免 panic/错误文案换行伪造日志条目（日志注入）。
var runSummaryEscaper = strings.NewReplacer("\r", `\r`, "\n", `\n`)

// runRedactionPolicy 是通用层的凭据字段名 / 授权前缀政策（与 buddy / autoclaw 各包同源）。
//
// 政策在此集中声明：各包迁移到本模块后错误摘要的脱敏口径逐字一致，避免快照经状态查询泄漏凭据。
var runRedactionPolicy = redact.RedactionPolicy{
	CredentialNames: []string{"secret", "key", "token", "password", "credential", "authorization"},
	AuthPrefixes:    []string{"Bearer"},
}

// safeRunSummary 生成凭据安全且有界的摘要：先抹授权前缀令牌、再抹键值形态与裸名形态凭据值，
// 转义裸 CR/LF，最后按 UTF-8 字符边界截断到 maxRunErrorSummaryBytes。
//
// 脱敏与截断复用 redact 共享实现（顺序敏感：必须先跑授权前缀正则，否则字段名正则会把
// `Authorization: Bearer <tok>` 中的 `Bearer` 当字段值吞掉，令牌将裸奔）。
func safeRunSummary(text string) string {
	text = redact.RedactCredentials(text, runRedactionPolicy)
	text = runSummaryEscaper.Replace(text)
	return redact.TruncateUTF8(text, maxRunErrorSummaryBytes, "")
}

// panicSafeSummary 把 recover() 得到的任意值转成安全摘要（前缀 `panic: `）。
func panicSafeSummary(recovered any) string {
	return safeRunSummary(fmt.Sprintf("panic: %v", recovered))
}

// errorSafeSummary 把执行体返回的 error 转成安全摘要（nil 返回空串）。
func errorSafeSummary(err error) string {
	if err == nil {
		return ""
	}
	return safeRunSummary(err.Error())
}
