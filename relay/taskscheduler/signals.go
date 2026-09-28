// signals.go 承载通用任务调度器的**进程退出信号监听**（design D1/D3；与 buddytask / autoclawtask
// 同方案，迁移后成为唯一实现）。
//
// 调度器自建 context.Background() 派生 ctx，注册中断（os.Interrupt）与终止（syscall.SIGTERM）信号；
// 进程退出信号到来即停排程循环。信号监听因此**属于调度器内部**，上层（registry）只需传中性 ctx。
//
// 直接使用 signal.NotifyContext 存在「信号被吞」缺陷：NotifyContext 收到信号后只取消其派生 ctx，
// **不会**撤销信号转递（撤销必须调用它返回的 stop，而该 stop 被有意丢弃以覆盖整个进程生命周期）。
// 信号既已被消费，OS 默认处置（终止进程）便不再生效，进程需再收到一次信号才可能退出——而
// deploy.sh 只发一次 SIGTERM（等 10s 后 kill -9），停机因此被拖满 10 秒。
//
// 方案 A（Notify + Stop + 自我重发）：
//
//	收到信号 → signal.Stop（撤销转递，恢复 OS 默认处置）→ cancel（取消排程 ctx）
//	→ syscall.Kill(pid, 同一信号)（自我重发）→ 默认处置终止进程（exit code 143）。
//
// 净效果：ctx 取消（排程感知退出）与进程退出（exit code 143）同时成立，且只依赖**第一个** SIGTERM。
//
// 本文件不导入任何数据层 / 文件系统包。
package taskscheduler

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

// exitSignals 是自建 ctx 监听的中断 / 终止信号集合。
var exitSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// exitSignalOps 把退出信号处理依赖的三个原语收拢为可替换集合。
//
// 单独抽出是为了给单元测试留注入缝：包内测试绝不能向自身进程投递真实 SIGTERM（会直接杀死测试
// 进程），只能经假信号源驱动处理器，故 Notify / Stop / Kill 三个调用点都必须可替换。
type exitSignalOps struct {
	// notify 开始把信号转递到 c（等价 signal.Notify(c, sigs...)）。
	notify func(c chan<- os.Signal, sigs ...os.Signal)
	// stop 撤销经 c 建立的转递（等价 signal.Stop(c)）。
	stop func(c chan<- os.Signal)
	// resend 把信号重发给当前进程，交由 OS 默认处置（等价 syscall.Kill(syscall.Getpid(), sig)）。
	resend func(sig os.Signal) error
}

// defaultExitSignalOps 是生产实现：直接指向 os/signal 与 syscall。
var defaultExitSignalOps = exitSignalOps{
	notify: signal.Notify,
	stop:   signal.Stop,
	resend: resendExitSignal,
}

// exitSignalOpsForWatch 是 watchExitSignals 实际使用的原语集合（生产恒为 defaultExitSignalOps）。
//
// 做成包级可替换变量是必要的注入缝：Start 在「只一次」保护下自行派生 ctx，测试无法在调用点注入
// 假信号源。
var exitSignalOpsForWatch = defaultExitSignalOps

// resendExitSignal 把信号重发给当前进程，使 OS 默认处置接管。
//
// 仅接受 syscall.Signal（os.Interrupt 与 syscall.SIGTERM 均满足）；其他实现视为不可重发，返回错误
// 而不 panic——重发失败只影响退出速度，不应反过来崩溃进程。
func resendExitSignal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return errors.New("taskscheduler: exit signal is not a syscall.Signal")
	}
	return syscall.Kill(syscall.Getpid(), s)
}

// watchExitSignals 返回 parent 的子 ctx，在进程收到第一个退出信号时取消。
//
// 语义（方案 A，见文件头注释）：
//   - 收到 os.Interrupt / SIGTERM：撤销转递 → 取消派生 ctx → 自我重发同一信号 → 默认处置终止进程；
//   - parent 先取消（非信号路径）：仅撤销转递，**绝不**重发信号（否则会误杀进程）；
//   - 返回的 ctx 供排程循环使用：ctx.Err() != nil 即「进程退出中，停止后续触发」。
//
// 非阻塞：信号处理器在后台 goroutine 中等待，本函数立即返回。
func watchExitSignals(parent context.Context) context.Context {
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithCancel(parent)
	// 取一份原语集合快照：处理器 goroutine 只读该副本，避免运行期读取包级变量。
	ops := exitSignalOpsForWatch
	ch := make(chan os.Signal, 1)
	ops.notify(ch, exitSignals...)

	go func() {
		select {
		case sig := <-ch:
			// **顺序承重**：必须先撤销转递（恢复默认处置）再重发信号；若先重发，信号会被再次
			// 投递进本 channel 而非触发默认处置，进程将永不退出。
			ops.stop(ch)
			cancel()
			_ = ops.resend(sig)
		case <-parent.Done():
			// 父 ctx 先取消：撤销转递即可，避免长期占用信号处理器。
			ops.stop(ch)
			cancel()
		}
	}()

	return ctx
}
