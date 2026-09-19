package chandesc

import (
	"fmt"
	"sync"
	"testing"
)

// TestEmptyRegistry 锁定默认行为：空注册表 Get 返回 nil、All 返回空切片（非 nil）。
func TestEmptyRegistry(t *testing.T) {
	r := New()
	if got := r.Get(54); got != nil {
		t.Errorf("Get on empty registry = %+v, want nil", got)
	}
	all := r.All()
	if all == nil {
		t.Fatalf("All on empty registry = nil, want non-nil empty slice")
	}
	if len(all) != 0 {
		t.Errorf("All on empty registry len = %d, want 0", len(all))
	}
	if ids := r.RegisteredIDs(); len(ids) != 0 {
		t.Errorf("RegisteredIDs on empty registry = %v, want empty", ids)
	}
}

// TestRegisterGetAllPreserveOrder 断言注册、按类型查找与保序返回。
func TestRegisterGetAllPreserveOrder(t *testing.T) {
	r := New()
	r.Register(Descriptor{ID: "a", ChannelType: 10, Name: "A"})
	r.Register(Descriptor{ID: "b", ChannelType: 20, Name: "B"})
	r.Register(Descriptor{ID: "c", ChannelType: 30, Name: "C"})

	if got := r.Get(20); got == nil || got.ID != "b" {
		t.Errorf("Get(20) = %+v, want id=b", got)
	}
	if got := r.Get(999); got != nil {
		t.Errorf("Get(999) = %+v, want nil", got)
	}
	ids := r.RegisteredIDs()
	want := []string{"a", "b", "c"}
	if len(ids) != len(want) {
		t.Fatalf("RegisteredIDs = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("RegisteredIDs = %v, want %v", ids, want)
		}
	}
	all := r.All()
	if len(all) != 3 || all[0].ID != "a" || all[2].ID != "c" {
		t.Errorf("All order = %+v, want [a b c]", all)
	}
}

// TestRegisterDuplicateIDPanics 断言重复 ID 属于启动期代码错误，直接 panic。
func TestRegisterDuplicateIDPanics(t *testing.T) {
	r := New()
	r.Register(Descriptor{ID: "dup", ChannelType: 1})
	assertPanics(t, func() { r.Register(Descriptor{ID: "dup", ChannelType: 2}) })
}

// TestRegisterDuplicateChannelTypePanics 断言同一渠道类型重复注册 panic（避免前端按类型查找时静默覆盖）。
func TestRegisterDuplicateChannelTypePanics(t *testing.T) {
	r := New()
	r.Register(Descriptor{ID: "x", ChannelType: 7})
	assertPanics(t, func() { r.Register(Descriptor{ID: "y", ChannelType: 7}) })
}

// TestRegisterEmptyIDPanics 断言空 ID 属于启动期代码错误。
func TestRegisterEmptyIDPanics(t *testing.T) {
	r := New()
	assertPanics(t, func() { r.Register(Descriptor{ChannelType: 3}) })
}

// TestGetReturnsCopy 断言 Get 返回副本，调用方改写不影响注册表内部状态。
func TestGetReturnsCopy(t *testing.T) {
	r := New()
	r.Register(Descriptor{ID: "a", ChannelType: 5, Name: "orig"})
	got := r.Get(5)
	got.Name = "mutated"
	if again := r.Get(5); again.Name != "orig" {
		t.Errorf("Get returned shared state: name = %q, want orig", again.Name)
	}
}

// TestGetReturnsDeepCopy 断言 Get 的深拷贝：改写嵌套 map（NameI18n / OAuth.Params）不污染注册表。
func TestGetReturnsDeepCopy(t *testing.T) {
	r := New()
	r.Register(Descriptor{
		ID:          "a",
		ChannelType: 5,
		NameI18n:    map[string]string{"zh": "原始"},
		Panel: PanelMeta{
			OAuth: &OAuthMeta{StartPath: "/s", Params: map[string]string{"realm": "cn"}},
		},
	})
	got := r.Get(5)
	got.NameI18n["zh"] = "被改"
	got.Panel.OAuth.Params["realm"] = "global"
	got.Panel.OAuth.StartPath = "/changed"

	again := r.Get(5)
	if again.NameI18n["zh"] != "原始" {
		t.Errorf("Get leaked NameI18n map: got %q, want 原始", again.NameI18n["zh"])
	}
	if again.Panel.OAuth == nil || again.Panel.OAuth.Params["realm"] != "cn" {
		t.Errorf("Get leaked OAuth.Params: got %+v, want realm=cn", again.Panel.OAuth)
	}
	if again.Panel.OAuth.StartPath != "/s" {
		t.Errorf("Get leaked OAuth pointer: StartPath = %q, want /s", again.Panel.OAuth.StartPath)
	}
}

// TestAllReturnsDeepCopy 断言 All 的深拷贝：改写返回项的嵌套 map 不污染注册表。
func TestAllReturnsDeepCopy(t *testing.T) {
	r := New()
	r.Register(Descriptor{ID: "a", ChannelType: 5, NameI18n: map[string]string{"zh": "原始"}})
	all := r.All()
	all[0].NameI18n["zh"] = "被改"
	if again := r.All(); again[0].NameI18n["zh"] != "原始" {
		t.Errorf("All leaked NameI18n map: got %q, want 原始", again[0].NameI18n["zh"])
	}
}

// TestConcurrentAccess 在 -race 下验证读写并发安全。
func TestConcurrentAccess(t *testing.T) {
	r := New()
	for i := 0; i < 50; i++ {
		r.Register(Descriptor{ID: fmt.Sprintf("desc-%d", i), ChannelType: i + 100})
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = r.Get(n + 100)
			_ = r.All()
			_ = r.RegisteredIDs()
		}(i)
	}
	wg.Wait()
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic, got none")
		}
	}()
	fn()
}
