package routeregistry_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/relay/routeregistry"
)

// fakeRegistrar 是注册表测试用的假注册方：只记录两个方法是否被调用。
type fakeRegistrar struct {
	publicCalled bool
	authCalled   bool
}

func (f *fakeRegistrar) RegisterPublicRoutes(g *gin.RouterGroup) { f.publicCalled = true }
func (f *fakeRegistrar) RegisterAuthRoutes(g *gin.RouterGroup)   { f.authCalled = true }

// TestRegistryEmptyReturnsNil 覆盖「无任何渠道注册」的真实情形：
// 空注册表必须返回 nil，且不 panic。
func TestRegistryEmptyReturnsNil(t *testing.T) {
	r := routeregistry.New()
	if got := r.Registered(); got != nil {
		t.Errorf("empty registry Registered() = %v, want nil", got)
	}
	if ids := r.RegisteredIDs(); len(ids) != 0 {
		t.Errorf("empty registry RegisteredIDs() = %v, want empty", ids)
	}
}

// TestRegistryOrderPreserved 覆盖保序：Registered 按注册顺序返回。
func TestRegistryOrderPreserved(t *testing.T) {
	r := routeregistry.New()
	r.Register("ext-a", &fakeRegistrar{})
	r.Register("ext-b", &fakeRegistrar{})

	if got := r.RegisteredIDs(); !reflect.DeepEqual(got, []string{"ext-a", "ext-b"}) {
		t.Fatalf("RegisteredIDs() = %v, want [ext-a ext-b] (order not preserved)", got)
	}
	if got := r.Registered(); len(got) != 2 {
		t.Fatalf("Registered() len = %d, want 2", len(got))
	}
}

// TestRegistryDuplicatePanics 覆盖重复注册同一 id 必须报错（与 chanregistry.Register 语义一致）。
func TestRegistryDuplicatePanics(t *testing.T) {
	r := routeregistry.New()
	r.Register("dup", &fakeRegistrar{})
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register did not panic")
		}
	}()
	r.Register("dup", &fakeRegistrar{})
}

// TestRegistryConcurrentAccess 在 -race 下验证读写并发安全。
func TestRegistryConcurrentAccess(t *testing.T) {
	r := routeregistry.New()
	r.Register("ext-a", &fakeRegistrar{})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = r.Registered() }()
		go func() { defer wg.Done(); _ = r.RegisteredIDs() }()
	}
	wg.Wait()
}
