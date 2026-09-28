// scheduler_test.go 覆盖通用调度骨架的记录层、异步执行纪律、生命周期层与信号（对照 buddytask /
// autoclawtask 的测试维度，light 档：源→目标映射 + 等价断言）。
//
// 全部离线：执行体经 TaskSpec.Run 注入桩；DataStoreReady / Now 经 Options 注入，不触真实 DB / 网络。
// 信号层测试经 exitSignalOpsForWatch 注入假信号源，**绝不**向自身进程投递真实 SIGTERM。
package taskscheduler

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pai801/myapi/common/logger"
)

// testZone 固定时区，避免用例依赖真实当前时间导致 flaky。
var testZone = time.FixedZone("UTC+8", 8*60*60)

// ── 测试设施 ───────────────────────────────────────────────────

// captureLogger 是 logger.ILogger 的内存桩：把日志收进切片供断言。
type captureLogger struct {
	mu      sync.Mutex
	records []string
}

func (l *captureLogger) record(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, fmt.Sprintf(format, args...))
}

func (l *captureLogger) Enable(string) bool { return true }

func (l *captureLogger) Debugf(f string, a ...interface{})  { l.record(f, a...) }
func (l *captureLogger) Debugw(m string, k ...interface{})  { l.record("%s %v", m, k) }
func (l *captureLogger) Infof(f string, a ...interface{})   { l.record(f, a...) }
func (l *captureLogger) Infow(m string, k ...interface{})   { l.record("%s %v", m, k) }
func (l *captureLogger) Warnf(f string, a ...interface{})   { l.record(f, a...) }
func (l *captureLogger) Warnw(m string, k ...interface{})   { l.record("%s %v", m, k) }
func (l *captureLogger) Errorf(f string, a ...interface{})  { l.record(f, a...) }
func (l *captureLogger) Errorw(m string, k ...interface{})  { l.record("%s %v", m, k) }
func (l *captureLogger) DPanicf(f string, a ...interface{}) { l.record(f, a...) }
func (l *captureLogger) DPanicw(m string, k ...interface{}) { l.record("%s %v", m, k) }
func (l *captureLogger) Panicf(f string, a ...interface{})  { l.record(f, a...) }
func (l *captureLogger) Panicw(m string, k ...interface{})  { l.record("%s %v", m, k) }
func (l *captureLogger) Fatalf(f string, a ...interface{})  { l.record(f, a...) }
func (l *captureLogger) Fatalw(m string, k ...interface{})  { l.record("%s %v", m, k) }

// installCaptureLogger 替换全局 logger.Log 并在用例结束后恢复。
func installCaptureLogger(t *testing.T) *captureLogger {
	t.Helper()
	cap := &captureLogger{}
	old := logger.Log
	logger.Log = cap
	t.Cleanup(func() { logger.Log = old })
	return cap
}

// lines 返回捕获到的全部日志行快照。
func (l *captureLogger) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.records))
	copy(out, l.records)
	return out
}

// waitForLogLine 轮询直到捕获到含 sub 的日志行；超时 Fatal。
func waitForLogLine(t *testing.T, cap *captureLogger, sub string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, line := range cap.lines() {
			if strings.Contains(line, sub) {
				return line
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no captured log line containing %q within %s; lines=%v", sub, timeout, cap.lines())
	return ""
}

// waitDone 阻塞直到 s 的后台生命周期退出（s.done 关闭），超时 Fatal。
func waitDone(t *testing.T, s *Scheduler) {
	t.Helper()
	if s == nil {
		return
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler background lifecycle did not exit within timeout")
	}
}

// waitIdle 轮询直到任务槽位空闲或超时。
func waitIdle(t *testing.T, s *Scheduler, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Snapshot()[id].Current == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %q run did not reach finished within timeout", id)
}

// waitInt32AtLeast 轮询直到计数器达到 want，超时 Fatal。
func waitInt32AtLeast(t *testing.T, counter *atomic.Int32, want int32, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s count = %d, want at least %d", what, counter.Load(), want)
}

// okRun 是空结果执行体桩。
func okRun(context.Context, RunOptions) (any, error) { return nil, nil }

// alwaysEnabled / fixedHours 是声明字段的便捷桩。
func alwaysEnabled() bool { return true }
func fixedHours(h ...int) func() []int {
	return func() []int { return h }
}

// newSchedulerForTest 构造一个仅含单任务、可注入 Options 的调度器。
func newSchedulerForTest(t *testing.T, spec TaskSpec, opts Options) *Scheduler {
	t.Helper()
	if spec.Enabled == nil {
		spec.Enabled = alwaysEnabled
	}
	s := New([]TaskSpec{spec}, opts)
	// 默认注入「不注册真实信号」的 watchSignals，避免测试向进程注册真实信号处理器。
	s.watchSignals = func(parent context.Context) context.Context {
		ctx, cancel := context.WithCancel(parent)
		t.Cleanup(cancel)
		return ctx
	}
	return s
}

// ── 1.2 记录层：单飞槽位与快照 ─────────────────────────────────

// TestStartRunOccupiesSlot 覆盖「占位成功」：空闲槽位被占、current 指向本轮、状态为 running。
func TestStartRunOccupiesSlot(t *testing.T) {
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{})
	slot := s.byID["a"]

	ref := newRunRef(time.Now())
	got, err := slot.startRun(ref)
	if err != nil {
		t.Fatalf("startRun on idle slot error: %v", err)
	}
	if got.RunID != ref.RunID || got.State != RunStateRunning {
		t.Errorf("returned ref = %+v, want running ref %q", got, ref.RunID)
	}
	if snap := slot.snapshot(); snap.Current == nil || snap.Current.RunID != ref.RunID {
		t.Errorf("current not occupied: %+v", snap.Current)
	}
}

// TestStartRunBusyReturnsErrBusyWithCurrent 覆盖「占用返回 ErrBusy」：后到者得 ErrBusy 且回带当前轮次。
func TestStartRunBusyReturnsErrBusyWithCurrent(t *testing.T) {
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{})
	slot := s.byID["a"]

	first := newRunRef(time.Now())
	if _, err := slot.startRun(first); err != nil {
		t.Fatalf("first startRun error: %v", err)
	}
	second := newRunRef(time.Now())
	got, err := slot.startRun(second)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second startRun err = %v, want ErrBusy", err)
	}
	if got.RunID != first.RunID {
		t.Errorf("busy ref = %q, want current running run %q", got.RunID, first.RunID)
	}
	if snap := slot.snapshot(); snap.Current.RunID != first.RunID {
		t.Errorf("busy start overwrote current: %+v", snap.Current)
	}
}

// TestFinishRunMigratesToLastCompleted 覆盖「完成迁移 lastCompleted」：current 清空、记录迁移。
func TestFinishRunMigratesToLastCompleted(t *testing.T) {
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{})
	slot := s.byID["a"]

	ref := newRunRef(time.Now())
	if _, err := slot.startRun(ref); err != nil {
		t.Fatalf("startRun error: %v", err)
	}
	slot.finishRun(RunRecord{RunID: ref.RunID, State: RunStateFinished, StartedAt: ref.StartedAt, Result: "payload"})

	snap := slot.snapshot()
	if snap.Current != nil {
		t.Errorf("current = %+v, want nil after finish", snap.Current)
	}
	if snap.LastCompleted == nil || snap.LastCompleted.RunID != ref.RunID || snap.LastCompleted.Result != "payload" {
		t.Fatalf("lastCompleted = %+v, want migrated record", snap.LastCompleted)
	}
}

// TestSnapshotIsValueCopy 覆盖值拷贝快照：改写快照不回写内部状态。
func TestSnapshotIsValueCopy(t *testing.T) {
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{})
	slot := s.byID["a"]
	ref := newRunRef(time.Now())
	if _, err := slot.startRun(ref); err != nil {
		t.Fatalf("startRun error: %v", err)
	}
	slot.setNextFire(time.Date(2026, 9, 22, 9, 0, 0, 0, testZone))
	slot.finishRun(RunRecord{RunID: ref.RunID, State: RunStateFinished})

	snap := s.Snapshot()["a"]
	// 篡改快照的指针字段与值字段。
	snap.Current = nil
	snap.NextFire = time.Time{}
	snap.LastCompleted.RunID = "tampered"

	again := s.Snapshot()["a"]
	if again.NextFire.IsZero() {
		t.Error("next_fire mutation leaked into internal state")
	}
	if again.LastCompleted == nil || again.LastCompleted.RunID != ref.RunID {
		t.Errorf("last_completed mutation leaked: %+v", again.LastCompleted)
	}
}

// ── 2.1 异步执行纪律 ───────────────────────────────────────────

// TestSubmitReturnsImmediately 覆盖「提交即返回」：执行体阻塞时 Submit 仍立即返回 running 引用。
func TestSubmitReturnsImmediately(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Run: func(context.Context, RunOptions) (any, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}}, Options{})

	returned := make(chan RunRef, 1)
	go func() {
		ref, err := s.Submit("a")
		if err != nil {
			t.Errorf("Submit error: %v", err)
		}
		returned <- ref
	}()

	select {
	case ref := <-returned:
		if ref.State != RunStateRunning {
			t.Errorf("ref state = %q, want running", ref.State)
		}
	case <-time.After(time.Second):
		t.Fatal("Submit blocked; MUST return immediately")
	}

	<-started
	if snap := s.Snapshot()["a"]; snap.Current == nil {
		t.Error("current must be occupied while running")
	}
	close(release)
	waitIdle(t, s, "a", 5*time.Second)
}

// TestExecutionNotBoundToRequestContext 覆盖「请求 ctx 取消不影响执行」：执行体收到的是进程 ctx，
// 请求侧 ctx 取消后任务仍跑完。
func TestExecutionNotBoundToRequestContext(t *testing.T) {
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()

	gotCtx := make(chan context.Context, 1)
	release := make(chan struct{})
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Run: func(ctx context.Context, _ RunOptions) (any, error) {
		gotCtx <- ctx
		<-release
		return "done", nil
	}}, Options{})
	s.ctx = processCtx
	s.started = true

	// 触发请求侧的 ctx：Submit 不接收它，取消它绝不应影响已提交的执行。
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	if _, err := s.Submit("a"); err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	cancelRequest()
	_ = requestCtx

	captured := <-gotCtx
	if captured != processCtx {
		t.Errorf("execution ctx = %v, want process (signal) ctx", captured)
	}
	close(release)
	waitIdle(t, s, "a", 5*time.Second)

	rec := s.Snapshot()["a"].LastCompleted
	if rec == nil || rec.Result != "done" || rec.Error != "" {
		t.Errorf("run did not complete cleanly after request cancel: %+v", rec)
	}
}

// TestExecuteRecoversPanic 覆盖「panic 收敛为该轮失败」：进程不崩溃、记为 finished + error。
func TestExecuteRecoversPanic(t *testing.T) {
	cap := installCaptureLogger(t)
	const secret = "sekretPanicToken1234567890"
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Run: func(context.Context, RunOptions) (any, error) {
		panic("boom accessToken=" + secret)
	}}, Options{})

	ref, err := s.Submit("a")
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	waitIdle(t, s, "a", 5*time.Second)

	rec := s.Snapshot()["a"].LastCompleted
	if rec == nil || rec.RunID != ref.RunID {
		t.Fatalf("lastCompleted = %+v, want record for %q", rec, ref.RunID)
	}
	if rec.State != RunStateFinished {
		t.Errorf("state = %q, want finished", rec.State)
	}
	if !strings.HasPrefix(rec.Error, "panic: ") {
		t.Errorf("error = %q, want panic summary", rec.Error)
	}
	if strings.Contains(rec.Error, secret) {
		t.Errorf("panic summary leaked credential: %q", rec.Error)
	}
	if rec.FinishedAt.IsZero() {
		t.Error("finished_at must be set on panic path")
	}
	// 进程仍存活：再次提交应可正常执行（槽位已释放）。
	if _, err := s.Submit("a"); err != nil {
		t.Fatalf("Submit after recovered panic error: %v", err)
	}
	waitIdle(t, s, "a", 5*time.Second)
	if got := cap.lines(); len(got) == 0 {
		t.Error("panic must be logged")
	}
}

// TestSubmitUnknownTask 覆盖未注册 ID 返回可判定的错误（装配期编程错误）。
func TestSubmitUnknownTask(t *testing.T) {
	s := New(nil, Options{})
	if _, err := s.Submit("nope"); !errors.Is(err, errUnknownTask) {
		t.Errorf("Submit unknown err = %v, want errUnknownTask", err)
	}
}

// TestCrossTaskSingleFlightIndependent 覆盖不同任务互不阻塞（各自独立单飞槽位）。
func TestCrossTaskSingleFlightIndependent(t *testing.T) {
	releaseA := make(chan struct{})
	startedA := make(chan struct{}, 1)
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Run: func(context.Context, RunOptions) (any, error) {
		startedA <- struct{}{}
		<-releaseA
		return nil, nil
	}}, Options{})
	s.slots = append(s.slots, &taskSlot{spec: TaskSpec{ID: "b", Enabled: alwaysEnabled, Run: okRun}})
	s.byID["b"] = s.slots[len(s.slots)-1]

	if _, err := s.Submit("a"); err != nil {
		t.Fatalf("Submit a error: %v", err)
	}
	<-startedA
	if _, err := s.Submit("b"); err != nil {
		t.Fatalf("task b blocked by running task a: %v", err)
	}
	waitIdle(t, s, "b", 5*time.Second)
	if s.Snapshot()["a"].Current == nil {
		t.Error("task a must still be running while b completed")
	}
	close(releaseA)
	waitIdle(t, s, "a", 5*time.Second)
}

// ── 2.2 仅内存、仅两份记录 ─────────────────────────────────────

// TestOnlyLatestCompletedRetained 覆盖多轮相继完成后只保留最近一次完成。
func TestOnlyLatestCompletedRetained(t *testing.T) {
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Run: okRun}, Options{})
	var lastRunID string
	for i := 0; i < 3; i++ {
		ref, err := s.Submit("a")
		if err != nil {
			t.Fatalf("Submit #%d error: %v", i, err)
		}
		lastRunID = ref.RunID
		waitIdle(t, s, "a", 5*time.Second)
	}
	snap := s.Snapshot()["a"]
	if snap.Current != nil {
		t.Errorf("current = %+v, want nil", snap.Current)
	}
	if snap.LastCompleted == nil || snap.LastCompleted.RunID != lastRunID {
		t.Errorf("lastCompleted = %+v, want latest run %q (only one record retained)", snap.LastCompleted, lastRunID)
	}
}

// schedulerSource / signalsSource 是编译期嵌入的源码副本，供结构性断言（尊重 overlay）。
//
//go:embed scheduler.go
var schedulerSource string

//go:embed signals.go
var signalsSource string

// allowedImports 是本包允许导入的包集合：仅标准库中与内存/信号相关的少数包，加上日志与共享脱敏工具。
// 任何数据层 / 文件系统包一旦被引入即失败（轮次状态仅存内存的红线，spec「仅内存、仅两份记录」）。
//
// 本包属公开模块，MUST NOT 依赖任何私有（myapi-server）包：白名单只列公开模块（myapi）与标准库路径，
// 私有路径不在白名单内，且下方对 `myapi-server` 子串额外显式拒绝（见 TestSourcesImportNoPersistenceOrFilesystem）。
var allowedImports = map[string]bool{
	"context": true, "errors": true, "fmt": true, "os": true, "os/signal": true,
	"strings": true, "sync": true, "sync/atomic": true, "syscall": true, "time": true,
	"github.com/pai801/myapi/common/logger": true,
	"github.com/pai801/myapi/common/redact": true,
}

// forbiddenCallSelectors 是禁止出现的持久化 / 文件系统调用（防御性结构守卫）。
var forbiddenCallSelectors = map[string]bool{
	"AutoMigrate": true, "CreateTable": true, "WriteFile": true, "OpenFile": true,
	"Open": true, "Exec": true, "Save": true,
}

// privateModulePath 是私有模块路径前缀：公开 relay/taskscheduler MUST NOT 依赖任何私有包。
const privateModulePath = "github.com/pai801/myapi-server"

// TestSourcesImportNoPersistenceOrFilesystem 断言本包源码不导入数据层 / 文件系统包，
// 且无持久化调用（结构性守卫：轮次状态仅存进程内内存，MUST NOT 建表/加列/落盘）。
//
// 同时断言不导入任何私有（myapi-server）包：本包是公开模块的一部分，私有路径既不在白名单内，
// 又被下方显式拒绝——双重保证守卫不会因白名单误加私有项而失效。
func TestSourcesImportNoPersistenceOrFilesystem(t *testing.T) {
	fset := token.NewFileSet()
	for _, src := range []string{schedulerSource, signalsSource} {
		file, err := parser.ParseFile(fset, "embedded.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse embedded source failed: %v", err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, privateModulePath) {
				t.Errorf("private import %q: the public scheduler MUST NOT depend on the private module", path)
			}
			if !allowedImports[path] {
				t.Errorf("disallowed import %q: the scheduler MUST NOT import data-layer/filesystem packages", path)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if forbiddenCallSelectors[sel.Sel.Name] {
				t.Errorf("forbidden persistence call %q found in scheduler sources", sel.Sel.Name)
			}
			return true
		})
	}
}

// credentialFieldFragments 是凭据字段名片段（快照 MUST NOT 含凭据）。
var credentialFieldFragments = []string{"secret", "key", "token", "password", "credential", "authorization"}

// TestSnapshotTypesContainNoCredentialFields 覆盖「快照不含凭据字段」：结构性扫描对外快照类型的字段名。
func TestSnapshotTypesContainNoCredentialFields(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "scheduler.go", schedulerSource, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse embedded scheduler.go failed: %v", err)
	}

	want := map[string]bool{"TaskSnapshot": false, "RunRecord": false, "RunRef": false}
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		if _, tracked := want[ts.Name.Name]; !tracked {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		want[ts.Name.Name] = true
		for _, field := range st.Fields.List {
			for _, name := range field.Names {
				lower := strings.ToLower(name.Name)
				for _, frag := range credentialFieldFragments {
					if strings.Contains(lower, frag) {
						t.Errorf("%s field %q looks credential-bearing: snapshots MUST NOT expose credentials",
							ts.Name.Name, name.Name)
					}
				}
			}
		}
		return true
	})
	for name, found := range want {
		if !found {
			t.Fatalf("type %s not found in scheduler.go; scan target is not the expected declaration", name)
		}
	}
}

// ── 3.1 生命周期：唯一实例与幂等 Start ─────────────────────────

// TestNewDoesNotTouchDataStore 覆盖「注册阶段不访问数据层」：New 不得调用 DataStoreReady。
func TestNewDoesNotTouchDataStore(t *testing.T) {
	var probes atomic.Int32
	_ = New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{
		DataStoreReady: func() bool { probes.Add(1); return true },
	})
	if got := probes.Load(); got != 0 {
		t.Errorf("DataStoreReady called %d time(s) during New, want 0", got)
	}
}

// TestStartRepeatedStartsOneLifecycle 覆盖重复 Start 只启动一套后台生命周期、只派生一次信号 ctx。
func TestStartRepeatedStartsOneLifecycle(t *testing.T) {
	installCaptureLogger(t)

	var lifecycleStarts, watchCalls atomic.Int32
	s := New([]TaskSpec{{ID: "a", Enabled: func() bool { return false }, Run: okRun}}, Options{
		DataStoreReady: func() bool { return true },
	})
	s.onStart = func() { lifecycleStarts.Add(1) }
	s.watchSignals = func(parent context.Context) context.Context {
		watchCalls.Add(1)
		ctx, cancel := context.WithCancel(parent)
		t.Cleanup(cancel)
		return ctx
	}

	ctx, cancel := context.WithCancel(context.Background())
	// 注册顺序：晚于 installCaptureLogger → LIFO 下先 cancel+wait，再恢复 logger。
	t.Cleanup(func() {
		cancel()
		waitDone(t, s)
	})

	returned := make(chan struct{})
	go func() { s.Start(ctx); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Start blocked; MUST be nonblocking")
	}

	waitInt32AtLeast(t, &lifecycleStarts, 1, 2*time.Second, "lifecycle start")
	s.Start(ctx) // 重复调用：必须幂等早退
	time.Sleep(100 * time.Millisecond)

	if got := lifecycleStarts.Load(); got != 1 {
		t.Errorf("background lifecycle starts = %d, want 1", got)
	}
	if got := watchCalls.Load(); got != 1 {
		t.Errorf("signal ctx derivation count = %d, want 1 (handler registered once)", got)
	}
	if s.ctx == nil || s.ctx == ctx {
		t.Error("scheduler ctx must be a child derived from the injected parent")
	}
}

// TestStartRegistersSignalHandlerOnce 覆盖信号处理器只注册一次（经假信号源计数 Notify）。
func TestStartRegistersSignalHandlerOnce(t *testing.T) {
	installCaptureLogger(t)

	fake := installFakeExitSignalOps(t)
	s := New([]TaskSpec{{ID: "a", Enabled: func() bool { return false }, Run: okRun}}, Options{
		DataStoreReady: func() bool { return true },
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		waitDone(t, s)
	})

	s.Start(ctx)
	s.Start(ctx)
	s.Start(ctx)
	waitInt32AtLeast(t, &fake.notifyCalls, 1, 2*time.Second, "notify")
	time.Sleep(50 * time.Millisecond)

	if got := fake.notifyCalls.Load(); got != 1 {
		t.Errorf("signal Notify registered %d time(s), want exactly 1", got)
	}
}

// ── 3.2 waitForDB ──────────────────────────────────────────────

// TestWaitForDBImmediateReady 覆盖首次探测即就绪、只探测一次。
func TestWaitForDBImmediateReady(t *testing.T) {
	var probes atomic.Int32
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{
		DataStoreReady: func() bool { probes.Add(1); return true },
		DBReadyPoll:    time.Millisecond,
		DBReadyMax:     time.Second,
	})
	if !s.waitForDB(context.Background()) {
		t.Fatal("waitForDB returned false, want true")
	}
	if probes.Load() != 1 {
		t.Errorf("probes = %d, want 1", probes.Load())
	}
}

// TestWaitForDBPollsUntilReady 覆盖延迟就绪：轮询到就绪后返回 true。
func TestWaitForDBPollsUntilReady(t *testing.T) {
	var probes atomic.Int32
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{
		DataStoreReady: func() bool { return probes.Add(1) >= 3 },
		DBReadyPoll:    time.Millisecond,
		DBReadyMax:     time.Second,
	})
	if !s.waitForDB(context.Background()) {
		t.Fatal("waitForDB returned false, want true once ready")
	}
	if probes.Load() < 3 {
		t.Errorf("probes = %d, want >= 3", probes.Load())
	}
}

// TestWaitForDBTimeoutWarnsNonFatally 覆盖超时非致命：返回 false 且输出 WARN。
func TestWaitForDBTimeoutWarnsNonFatally(t *testing.T) {
	cap := installCaptureLogger(t)
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{
		DataStoreReady: func() bool { return false },
		DBReadyPoll:    time.Millisecond,
		DBReadyMax:     20 * time.Millisecond,
	})
	if s.waitForDB(context.Background()) {
		t.Fatal("waitForDB returned true, want false on timeout")
	}
	waitForLogLine(t, cap, "datastore not ready", time.Second)
}

// TestWaitForDBCanceledContext 覆盖 ctx 取消立即返回、不告警超时。
func TestWaitForDBCanceledContext(t *testing.T) {
	cap := installCaptureLogger(t)
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Run: okRun}}, Options{
		DataStoreReady: func() bool { return false },
		DBReadyPoll:    time.Millisecond,
		DBReadyMax:     time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.waitForDB(ctx) {
		t.Fatal("waitForDB returned true, want false when canceled")
	}
	for _, line := range cap.lines() {
		if strings.Contains(line, "datastore not ready") {
			t.Errorf("cancellation must not emit the timeout warning: %q", line)
		}
	}
}

// TestStartDBTimeoutReturnsWithoutLoop 覆盖 DB 始终不可用 → 非致命返回、排程未进入。
func TestStartDBTimeoutReturnsWithoutLoop(t *testing.T) {
	cap := installCaptureLogger(t)
	var loopEntries atomic.Int32
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Hours: fixedHours(0)}, Options{
		DataStoreReady: func() bool { return false },
		DBReadyPoll:    time.Millisecond,
		DBReadyMax:     20 * time.Millisecond,
	})
	s.onLoop = func() { loopEntries.Add(1) }

	done := make(chan struct{})
	go func() { s.start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("start did not return after readiness timeout")
	}
	if nf := s.Snapshot()["a"].NextFire; !nf.IsZero() {
		t.Errorf("next_fire = %v, want zero", nf)
	}
	if loopEntries.Load() != 0 {
		t.Error("loop must not run when datastore is not ready")
	}
	waitForLogLine(t, cap, "datastore not ready", time.Second)
}

// ── 3.3 loop ───────────────────────────────────────────────────

// waitParkAtLeast 轮询直到 park 计数达到 want。
func waitParkAtLeast(t *testing.T, parks *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if parks.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("park count = %d, want at least %d", parks.Load(), want)
}

// TestLoopDispatchesWhenFireTimeReached 覆盖到点触发主路径。
func TestLoopDispatchesWhenFireTimeReached(t *testing.T) {
	fireTarget := time.Date(2026, 9, 22, 9, 0, 0, 0, testZone)
	before := fireTarget.Add(-200 * time.Millisecond)
	after := fireTarget.Add(time.Second)

	var calls atomic.Int32
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Hours: fixedHours(9), Run: func(context.Context, RunOptions) (any, error) {
		calls.Add(1)
		return nil, nil
	}}}, Options{})
	var nowCalls atomic.Int32
	s.opts.Now = func() time.Time {
		if nowCalls.Add(1) == 1 {
			return before
		}
		return after
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.loop(ctx); close(done) }()

	waitInt32AtLeast(t, &calls, 1, 2*time.Second, "dispatch")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
}

// TestLoopPassesNominalFire 覆盖到点把「本轮算出的名义整点」经 RunOptions.NominalFire 传入执行体。
//
// 可证伪性：若实现丢弃 nominal（传零值），断言在 NominalFire 上变红。
func TestLoopPassesNominalFire(t *testing.T) {
	fireTarget := time.Date(2026, 9, 22, 9, 0, 0, 0, testZone)
	before := fireTarget.Add(-200 * time.Millisecond)
	after := fireTarget.Add(time.Second)

	gotNominal := make(chan time.Time, 1)
	s := New([]TaskSpec{{ID: "a", Enabled: alwaysEnabled, Hours: fixedHours(9), Run: func(_ context.Context, opts RunOptions) (any, error) {
		gotNominal <- opts.NominalFire
		return nil, nil
	}}}, Options{})
	var nowCalls atomic.Int32
	s.opts.Now = func() time.Time {
		if nowCalls.Add(1) == 1 {
			return before
		}
		return after
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.loop(ctx); close(done) }()

	select {
	case nominal := <-gotNominal:
		if !nominal.Equal(fireTarget) {
			t.Errorf("NominalFire = %v, want the nominal fire instant %v (not the jittered dispatch time %v)",
				nominal, fireTarget, after)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task was not dispatched within timeout")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
}

// TestLoopEmptyHoursDoesNotSpin 覆盖无排程（小时列表为空）时不空转，只 park 一次。
func TestLoopEmptyHoursDoesNotSpin(t *testing.T) {
	var calls, parks atomic.Int32
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Enabled: alwaysEnabled, Hours: fixedHours(), Run: func(context.Context, RunOptions) (any, error) {
		calls.Add(1)
		return nil, nil
	}}, Options{})
	s.parkHook = func() { parks.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.loop(ctx); close(done) }()

	waitParkAtLeast(t, &parks, 1, 2*time.Second)
	time.Sleep(50 * time.Millisecond)

	if nf := s.Snapshot()["a"].NextFire; !nf.IsZero() {
		t.Errorf("next_fire = %v, want zero", nf)
	}
	if calls.Load() != 0 {
		t.Errorf("task dispatched despite empty hours: %d", calls.Load())
	}
	if got := parks.Load(); got != 1 {
		t.Errorf("park count = %d, want exactly 1 (no spin)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
}

// TestStartAllDisabledDoesNotSpin 覆盖全部任务关闭时不进 loop、不空转。
func TestStartAllDisabledDoesNotSpin(t *testing.T) {
	var loopEntries, parks, calls atomic.Int32
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Enabled: func() bool { return false }, Hours: fixedHours(0), Run: func(context.Context, RunOptions) (any, error) {
		calls.Add(1)
		return nil, nil
	}}, Options{DataStoreReady: func() bool { return true }})
	s.onLoop = func() { loopEntries.Add(1) }
	s.parkHook = func() { parks.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.start(ctx); close(done) }()

	waitParkAtLeast(t, &parks, 1, 2*time.Second)
	time.Sleep(50 * time.Millisecond)

	if loopEntries.Load() != 0 {
		t.Errorf("timer loop entered %d time(s), want 0", loopEntries.Load())
	}
	if calls.Load() != 0 {
		t.Errorf("task dispatched while disabled: %d", calls.Load())
	}
	if got := parks.Load(); got != 1 {
		t.Errorf("park count = %d, want exactly 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("start did not return after cancel")
	}
}

// TestStartLogsTimezoneAndTasks 覆盖启动可观测行打印时区与各任务开关/小时，且恰好一次。
func TestStartLogsTimezoneAndTasks(t *testing.T) {
	cap := installCaptureLogger(t)
	s := newSchedulerForTest(t, TaskSpec{ID: "buddy-checkin", Hours: fixedHours(9, 21)}, Options{
		DataStoreReady: func() bool { return true },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.start(ctx); close(done) }()

	line := waitForLogLine(t, cap, "scheduler starting", 2*time.Second)
	if !strings.Contains(line, "timezone="+time.Local.String()) {
		t.Errorf("startup line %q missing timezone", line)
	}
	if !strings.Contains(line, "buddy-checkin{enabled=true,hours=[9 21]}") {
		t.Errorf("startup line %q missing task config", line)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("start did not return after cancel")
	}
	count := 0
	for _, l := range cap.lines() {
		if strings.Contains(l, "scheduler starting") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("startup log emitted %d time(s), want exactly 1", count)
	}
}

// TestNextFireAndNearestFireWait 覆盖整点计算与最近时点纯函数。
func TestNextFireAndNearestFireWait(t *testing.T) {
	now := time.Date(2026, 9, 22, 8, 30, 0, 0, testZone)

	if got := NextFire(now, nil); !got.IsZero() {
		t.Errorf("NextFire(nil) = %v, want zero", got)
	}
	if got := NextFire(now, []int{9, 21}); !got.Equal(time.Date(2026, 9, 22, 9, 0, 0, 0, testZone)) {
		t.Errorf("NextFire = %v, want 09:00 same day", got)
	}
	// 当日该小时已过 → 顺延次日同小时。
	if got := NextFire(now, []int{7}); !got.Equal(time.Date(2026, 9, 23, 7, 0, 0, 0, testZone)) {
		t.Errorf("NextFire = %v, want 07:00 next day", got)
	}
	// 恰在整点 → 返回 now 本身（含 now）。
	at := time.Date(2026, 9, 22, 9, 0, 0, 0, testZone)
	if got := NextFire(at, []int{9}); !got.Equal(at) {
		t.Errorf("NextFire(at) = %v, want %v", got, at)
	}

	zero := time.Time{}
	soon := now.Add(3 * time.Minute)
	later := now.Add(10 * time.Minute)
	if _, ok := nearestFireWait(now, zero, zero); ok {
		t.Error("all-zero candidates must report no scheduling")
	}
	if got, ok := nearestFireWait(now, zero, later); !ok || got != 10*time.Minute {
		t.Errorf("single candidate wait = %v ok=%t", got, ok)
	}
	if got, ok := nearestFireWait(now, soon, later); !ok || got != 3*time.Minute {
		t.Errorf("nearest wait = %v ok=%t", got, ok)
	}
}

// ── 3.4 Submit 三态与快照内容 ─────────────────────────────────

// TestSubmitThreeStateMapping 覆盖三态映射：空闲→引用、忙→ErrBusy（回带当前引用）、关→ErrDisabled。
func TestSubmitThreeStateMapping(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	enabled := true
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Enabled: func() bool { return enabled }, Run: func(context.Context, RunOptions) (any, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}}, Options{})

	// 空闲 → 返回运行中引用。
	ref, err := s.Submit("a")
	if err != nil {
		t.Fatalf("idle Submit error: %v", err)
	}
	if ref.State != RunStateRunning {
		t.Errorf("ref state = %q, want running", ref.State)
	}
	<-started

	// 忙 → ErrBusy 且回带当前运行轮次标识。
	busy, err := s.Submit("a")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("busy Submit err = %v, want ErrBusy", err)
	}
	if busy.RunID != ref.RunID {
		t.Errorf("busy ref = %q, want current run %q", busy.RunID, ref.RunID)
	}

	// 关 → ErrDisabled（总闸优先，即使槽位空闲）。
	close(release)
	waitIdle(t, s, "a", 5*time.Second)
	enabled = false
	if _, err := s.Submit("a"); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled Submit err = %v, want ErrDisabled", err)
	}
}

// TestSnapshotContentShape 覆盖快照内容：开关 / 下次触发 / 当前运行 / 最近一次完成。
func TestSnapshotContentShape(t *testing.T) {
	enabled := true
	s := newSchedulerForTest(t, TaskSpec{ID: "a", Enabled: func() bool { return enabled }, Run: okRun}, Options{})

	if snap := s.Snapshot()["a"]; !snap.Enabled || snap.Current != nil || snap.LastCompleted != nil || !snap.NextFire.IsZero() {
		t.Errorf("initial snapshot = %+v, want enabled with empty run state", snap)
	}

	ref, err := s.Submit("a")
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	waitIdle(t, s, "a", 5*time.Second)

	snap := s.Snapshot()["a"]
	if !snap.Enabled {
		t.Error("enabled = false, want true")
	}
	if snap.Current != nil {
		t.Errorf("current = %+v, want nil after completion", snap.Current)
	}
	if snap.LastCompleted == nil || snap.LastCompleted.RunID != ref.RunID {
		t.Errorf("lastCompleted = %+v, want run %q", snap.LastCompleted, ref.RunID)
	}

	enabled = false
	if snap := s.Snapshot()["a"]; snap.Enabled {
		t.Error("enabled = true after gate closed")
	}
}

// ── 信号层 ─────────────────────────────────────────────────────

// fakeExitSignalOps 是 exitSignalOps 的假实现：记录事件并允许测试投递信号（绝不投真实 SIGTERM）。
type fakeExitSignalOps struct {
	mu          sync.Mutex
	ch          chan<- os.Signal
	notifyCalls atomic.Int32
	stopCalls   atomic.Int32
	resends     []os.Signal
}

func (f *fakeExitSignalOps) notify(c chan<- os.Signal, _ ...os.Signal) {
	f.mu.Lock()
	f.ch = c
	f.mu.Unlock()
	f.notifyCalls.Add(1)
}

func (f *fakeExitSignalOps) stop(chan<- os.Signal) { f.stopCalls.Add(1) }

func (f *fakeExitSignalOps) resend(sig os.Signal) error {
	f.mu.Lock()
	f.resends = append(f.resends, sig)
	f.mu.Unlock()
	return nil
}

func (f *fakeExitSignalOps) deliver(t *testing.T, sig os.Signal) {
	t.Helper()
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	if ch == nil {
		t.Fatal("deliver called before notify registered a channel")
	}
	ch <- sig
}

func (f *fakeExitSignalOps) resendSnapshot() []os.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]os.Signal(nil), f.resends...)
}

// installFakeExitSignalOps 把包级原语集合替换为假实现，并在用例结束后恢复。
func installFakeExitSignalOps(t *testing.T) *fakeExitSignalOps {
	t.Helper()
	fake := &fakeExitSignalOps{}
	prev := exitSignalOpsForWatch
	exitSignalOpsForWatch = exitSignalOps{notify: fake.notify, stop: fake.stop, resend: fake.resend}
	t.Cleanup(func() { exitSignalOpsForWatch = prev })
	return fake
}

// waitContextCanceled 轮询直到 ctx 被取消，超时 Fatal。
func waitContextCanceled(t *testing.T, ctx context.Context, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("ctx was not canceled within timeout")
}

// waitResendsAtLeast 轮询直到重发计数达到 want，超时 Fatal。
func waitResendsAtLeast(t *testing.T, fake *fakeExitSignalOps, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(fake.resendSnapshot()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("resend calls = %d, want at least %d", len(fake.resendSnapshot()), want)
}

// TestWatchExitSignalsResendsOnSignal 覆盖方案 A：投递信号 → Stop 被调用 + ctx 取消 + 同一信号被重发。
func TestWatchExitSignalsResendsOnSignal(t *testing.T) {
	fake := installFakeExitSignalOps(t)

	ctx := watchExitSignals(context.Background())
	fake.deliver(t, os.Interrupt)

	waitContextCanceled(t, ctx, 2*time.Second)
	// ctx 取消发生在 ops.stop 之后、resend 之前，故 stop 已可见；resend 需单独等待。
	if fake.stopCalls.Load() != 1 {
		t.Errorf("stop calls = %d, want 1", fake.stopCalls.Load())
	}
	waitResendsAtLeast(t, fake, 1, 2*time.Second)
	if resends := fake.resendSnapshot(); len(resends) != 1 || resends[0] != os.Interrupt {
		t.Errorf("resends = %v, want [interrupt]", resends)
	}
}

// waitStopAtLeast 轮询直到 stop 被调用达到 want，超时 Fatal。
func waitStopAtLeast(t *testing.T, fake *fakeExitSignalOps, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fake.stopCalls.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stop calls = %d, want at least %d", fake.stopCalls.Load(), want)
}

// TestWatchExitSignalsParentCancelDoesNotResend 覆盖父 ctx 先取消：仅 Stop、绝不重发信号。
func TestWatchExitSignalsParentCancelDoesNotResend(t *testing.T) {
	fake := installFakeExitSignalOps(t)

	parent, cancel := context.WithCancel(context.Background())
	ctx := watchExitSignals(parent)
	cancel()

	waitContextCanceled(t, ctx, 2*time.Second)
	// 父取消会经 context 传播立即取消派生 ctx；但 Stop 由处理器 goroutine 调用，需等待其执行。
	waitStopAtLeast(t, fake, 1, 2*time.Second)
	if resends := fake.resendSnapshot(); len(resends) != 0 {
		t.Errorf("resends = %v, want none on parent cancellation", resends)
	}
}

// TestWatchExitSignalsNilParent 覆盖 parent 为 nil 时回落 Background，不 panic。
func TestWatchExitSignalsNilParent(t *testing.T) {
	_ = installFakeExitSignalOps(t)
	ctx := watchExitSignals(nil)
	if ctx == nil {
		t.Fatal("watchExitSignals(nil) returned nil ctx")
	}
	if ctx.Err() != nil {
		t.Errorf("ctx unexpectedly canceled: %v", ctx.Err())
	}
}

// TestSafeRunSummaryRedactsAndBounds 覆盖错误摘要脱敏与有界。
func TestSafeRunSummaryRedactsAndBounds(t *testing.T) {
	const secret = "sekretToken1234567890"
	got := safeRunSummary("enumerate failed accessToken=" + secret + "\nAuthorization: Bearer " + secret)
	if strings.Contains(got, secret) {
		t.Errorf("summary leaked credential: %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("summary must escape raw CR/LF: %q", got)
	}
	long := safeRunSummary(strings.Repeat("x", 1000))
	if len(long) > maxRunErrorSummaryBytes {
		t.Errorf("summary length = %d, want <= %d", len(long), maxRunErrorSummaryBytes)
	}
}

// ── 4.1 包级单例与注册入口 ─────────────────────────────────────

// resetSingleton 清空包级单例状态，保证用例间互不干扰，并在用例结束后恢复（LIFO 下先停后台再恢复）。
func resetSingleton(t *testing.T) {
	t.Helper()
	singletonMu.Lock()
	prev, prevOpts := singleton, singletonOpts
	singleton, singletonOpts = nil, Options{}
	singletonMu.Unlock()
	t.Cleanup(func() {
		singletonMu.Lock()
		singleton, singletonOpts = prev, prevOpts
		singletonMu.Unlock()
	})
}

// injectSingletonWatch 给包级单例注入「不注册真实信号」的 watchSignals 缝（避免测试向进程注册信号）。
func injectSingletonWatch(t *testing.T, s *Scheduler) {
	t.Helper()
	s.watchSignals = func(parent context.Context) context.Context {
		ctx, cancel := context.WithCancel(parent)
		t.Cleanup(cancel)
		return ctx
	}
}

// TestSingletonRegisterAccumulates 覆盖「多次 Register 累积」：追加声明全部进入单例任务集。
func TestSingletonRegisterAccumulates(t *testing.T) {
	resetSingleton(t)

	Register(TaskSpec{ID: "one", Enabled: alwaysEnabled, Run: okRun})
	Register(TaskSpec{ID: "two", Enabled: alwaysEnabled, Run: okRun})

	snap := Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot size = %d, want 2 (multiple Register calls must accumulate)", len(snap))
	}
	for _, id := range []string{"one", "two"} {
		if _, ok := snap[id]; !ok {
			t.Errorf("registered task %q missing from snapshot: %v", id, snap)
		}
	}
}

// TestSingletonRegisterDuplicateIDIsIdempotentFirstWins 覆盖「同 ID 重复注册」：幂等去重、首次出现者生效。
func TestSingletonRegisterDuplicateIDIsIdempotentFirstWins(t *testing.T) {
	resetSingleton(t)

	Register(TaskSpec{ID: "dup", Enabled: alwaysEnabled, Run: func(context.Context, RunOptions) (any, error) {
		return "first", nil
	}})
	Register(TaskSpec{ID: "dup", Enabled: alwaysEnabled, Run: func(context.Context, RunOptions) (any, error) {
		return "second", nil
	}})

	if got := len(Snapshot()); got != 1 {
		t.Fatalf("snapshot size = %d, want 1 (same ID must not duplicate a slot)", got)
	}

	s := singletonInstance()
	if _, err := Submit("dup"); err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	waitIdle(t, s, "dup", 5*time.Second)

	rec := s.Snapshot()["dup"].LastCompleted
	if rec == nil || rec.Result != "first" {
		t.Errorf("lastCompleted = %+v, want result from the FIRST declaration", rec)
	}
}

// TestSingletonRegisterDoesNotTouchDataStore 覆盖「注册阶段不访问数据层」：Register 不得调用 DataStoreReady。
func TestSingletonRegisterDoesNotTouchDataStore(t *testing.T) {
	resetSingleton(t)

	var probes atomic.Int32
	Configure(Options{DataStoreReady: func() bool { probes.Add(1); return true }})
	Register(TaskSpec{ID: "a", Enabled: alwaysEnabled, Run: okRun})

	if got := probes.Load(); got != 0 {
		t.Errorf("DataStoreReady called %d time(s) during Register, want 0", got)
	}
}

// TestSingletonStartRepeatedStartsOneLifecycle 覆盖「重复 Start 只启动一套生命周期、只注册一次信号处理器」。
func TestSingletonStartRepeatedStartsOneLifecycle(t *testing.T) {
	resetSingleton(t)
	installCaptureLogger(t)
	fake := installFakeExitSignalOps(t)
	Configure(Options{DataStoreReady: func() bool { return true }})

	s := singletonInstance()
	var lifecycleStarts atomic.Int32
	s.onStart = func() { lifecycleStarts.Add(1) }

	Register(TaskSpec{ID: "a", Enabled: func() bool { return false }, Run: okRun})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		waitDone(t, s)
	})

	Start(ctx)
	Start(ctx)
	Start(ctx)

	waitInt32AtLeast(t, &lifecycleStarts, 1, 2*time.Second, "lifecycle start")
	time.Sleep(100 * time.Millisecond)
	if got := lifecycleStarts.Load(); got != 1 {
		t.Errorf("background lifecycle starts = %d, want 1 (repeated Start must early-return)", got)
	}

	waitInt32AtLeast(t, &fake.notifyCalls, 1, 2*time.Second, "notify")
	time.Sleep(50 * time.Millisecond)
	if got := fake.notifyCalls.Load(); got != 1 {
		t.Errorf("signal Notify registered %d time(s), want exactly 1", got)
	}
}

// TestSingletonRegisterAfterStartJoinsLaterRounds 覆盖关键语义：首次 Start 之后再 Register，
// 该任务被纳入后续轮次（W3 新增 traetask 依赖）。
//
// 可证伪性：等到排程循环**已算出首轮任务集**（首任务 next_fire 已写入、此时仅含 first）后才注册
// later。若实现只在 Start 时捕获任务集、不动态重读，later 永不触发 → 断言在 laterCalls 上变红。
func TestSingletonRegisterAfterStartJoinsLaterRounds(t *testing.T) {
	resetSingleton(t)
	installCaptureLogger(t)

	// 注入推进时钟：整点 09:00 落在启动后约 2s，给「启动后注册」留出充足窗口。
	base := time.Date(2026, 9, 22, 8, 59, 58, 0, testZone)
	clockStart := time.Now()
	Configure(Options{
		DataStoreReady: func() bool { return true },
		Now:            func() time.Time { return base.Add(time.Since(clockStart)) },
	})

	s := singletonInstance()
	injectSingletonWatch(t, s)

	var firstCalls, laterCalls atomic.Int32
	Register(TaskSpec{ID: "first", Enabled: alwaysEnabled, Hours: fixedHours(9), Run: func(context.Context, RunOptions) (any, error) {
		firstCalls.Add(1)
		return nil, nil
	}})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		waitDone(t, s)
	})
	Start(ctx)

	// 等到首任务 next_fire 已写入（证明排程循环已用「仅含 first」的任务集完成首轮计算）后再注册。
	waitNextFireSet(t, s, "first", 2*time.Second)
	Register(TaskSpec{ID: "later", Enabled: alwaysEnabled, Hours: fixedHours(9), Run: func(context.Context, RunOptions) (any, error) {
		laterCalls.Add(1)
		return nil, nil
	}})

	waitInt32AtLeast(t, &firstCalls, 1, 5*time.Second, "first task dispatch")
	waitInt32AtLeast(t, &laterCalls, 1, 5*time.Second, "later-registered task dispatch")
}

// waitNextFireSet 轮询直到任务的下次触发时刻非零（证明排程循环已完成一次含该任务的重算），超时 Fatal。
func waitNextFireSet(t *testing.T, s *Scheduler, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if snap, ok := s.Snapshot()[id]; ok && !snap.NextFire.IsZero() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task %q next_fire was not set within %s", id, timeout)
}

// TestSingletonRegisterWakesLoopImmediately 覆盖动态唤醒：已有任务的下一次触发远在将来时，启动后
// 新注册的任务仍被及时纳入重算——证明 Register 唤醒排程循环动态重读任务集，而非死等旧时点。
//
// 可证伪性（确定性，不依赖墙钟推进）：first 的触发在 23:00（约 14h 后），循环 timer 因此长阻塞；
// 若 Register 不唤醒循环，later 的 next_fire 永不写入 → waitNextFireSet 超时变红。
func TestSingletonRegisterWakesLoopImmediately(t *testing.T) {
	resetSingleton(t)
	installCaptureLogger(t)

	Configure(Options{DataStoreReady: func() bool { return true }})

	s := singletonInstance()
	injectSingletonWatch(t, s)

	Register(TaskSpec{ID: "first", Enabled: alwaysEnabled, Hours: fixedHours(23), Run: okRun})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		waitDone(t, s)
	})
	Start(ctx)

	// 首任务 next_fire 已写入（首轮计算完成，仅含 first）后，注册远未到点的新任务。
	waitNextFireSet(t, s, "first", 2*time.Second)
	if _, ok := s.Snapshot()["later"]; ok {
		t.Fatal("precondition violated: later must not be registered yet")
	}
	Register(TaskSpec{ID: "later", Enabled: alwaysEnabled, Hours: fixedHours(9), Run: okRun})

	// 唤醒生效 ⟹ 循环立即重算并把 later 纳入任务集（next_fire 写入）。
	waitNextFireSet(t, s, "later", 2*time.Second)
}

// TestSingletonConfigureDefaultsDataStoreReady 覆盖未 Configure 时单例按零值默认装配（DataStoreReady 恒就绪）。
func TestSingletonConfigureDefaultsDataStoreReady(t *testing.T) {
	resetSingleton(t)
	s := singletonInstance()
	if s.opts.DataStoreReady == nil || !s.opts.DataStoreReady() {
		t.Error("singleton default DataStoreReady must report ready")
	}
}
