package relay

import (
	"testing"

	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/apitype"
)

// TestGetAdaptorIsolatesFactoryPanic 在**生产入口** relay.GetAdaptor 上锁定 PRD §5.10 / AC9：
// 扩展 adaptor 的 factory panic 时，主进程不崩、返回降级值 nil。
//
// 与 relay/adaptor 包内 TestRegistry/factory_panic_isolated 互补：那条直接测 adaptor.Get，
// 本条测 controller/relay 实际调用的 relay.GetAdaptor 入口，并显式断言 panic 未冒泡到调用方。
//
// 手法：用 MustRegister 覆盖一个已注册 apiType 的 factory 为 panic 版本，t.Cleanup 还原，
// 避免污染同包其它测试（如 relay/adaptor_test.go 的 RegisteredAPITypes 完整性断言）。
func TestGetAdaptorIsolatesFactoryPanic(t *testing.T) {
	const probe = apitype.OpenAI
	real := GetAdaptor(probe)
	if real == nil {
		t.Fatal("precondition: apitype.OpenAI adaptor must be registered")
	}
	adaptor.MustRegister(probe, func() adaptor.Adaptor { panic("injected factory panic") })
	t.Cleanup(func() { adaptor.MustRegister(probe, func() adaptor.Adaptor { return real }) })

	var got adaptor.Adaptor
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("GetAdaptor propagated panic to caller (主进程会崩): %v", rec)
			}
		}()
		got = GetAdaptor(probe)
	}()

	if got != nil {
		t.Errorf("GetAdaptor on panicking factory = %v, want nil (降级返回值)", got)
	}
}
