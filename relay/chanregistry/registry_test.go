package chanregistry_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pai801/myapi/relay/chanregistry"
)

// fakeCompressor 是注册表测试用的假压缩器：按支持的渠道集合回答 Supports。
type fakeCompressor struct {
	supports map[int]bool
	out      string
	err      error
}

func (f fakeCompressor) Supports(channelType int) bool { return f.supports[channelType] }

func (f fakeCompressor) CompactKey(raw string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.out != "" {
		return f.out, nil
	}
	return raw, nil
}

// TestRegistryEmptyReturnsNil 覆盖「无任何渠道注册压缩器」的真实情形：
// 空注册表对任意 channelType 都必须返回 nil（调用方据此回退原有路径），且不 panic。
func TestRegistryEmptyReturnsNil(t *testing.T) {
	r := chanregistry.New()
	for _, ct := range []int{54, 55, 56, 1, 0, -1} {
		if c := r.GetCompressor(ct); c != nil {
			t.Errorf("empty registry GetCompressor(%d) = %v, want nil", ct, c)
		}
	}
	if ids := r.RegisteredIDs(); len(ids) != 0 {
		t.Errorf("empty registry RegisteredIDs() = %v, want empty", ids)
	}
}

// TestRegistryRegisterThenGet 覆盖命中与未命中。
func TestRegistryRegisterThenGet(t *testing.T) {
	r := chanregistry.New()
	r.Register("ext-a", fakeCompressor{supports: map[int]bool{54: true, 55: true}})

	if c := r.GetCompressor(54); c == nil {
		t.Fatal("GetCompressor(54) = nil, want non-nil")
	}
	if c := r.GetCompressor(55); c == nil {
		t.Fatal("GetCompressor(55) = nil, want non-nil")
	}
	if c := r.GetCompressor(56); c != nil {
		t.Errorf("GetCompressor(56) = %v, want nil (not supported)", c)
	}
}

// TestRegistryOrderPreservedAndFirstMatchWins 覆盖保序：多个压缩器支持同一渠道时取先注册者。
func TestRegistryOrderPreservedAndFirstMatchWins(t *testing.T) {
	r := chanregistry.New()
	first := fakeCompressor{supports: map[int]bool{56: true}, out: "first"}
	second := fakeCompressor{supports: map[int]bool{56: true}, out: "second"}
	r.Register("ext-b", first)
	r.Register("other", second)

	if got := r.RegisteredIDs(); !reflect.DeepEqual(got, []string{"ext-b", "other"}) {
		t.Fatalf("RegisteredIDs() = %v, want [ext-b other] (order not preserved)", got)
	}
	c := r.GetCompressor(56)
	if c == nil {
		t.Fatal("GetCompressor(56) = nil, want non-nil")
	}
	got, err := c.CompactKey("x")
	if err != nil {
		t.Fatalf("CompactKey err = %v, want nil", err)
	}
	if got != "first" {
		t.Errorf("GetCompressor(56).CompactKey = %q, want %q (first registration must win)", got, "first")
	}
}

// TestRegistryDuplicatePanics 覆盖重复注册同一 id 必须报错（与 adaptor.Register 语义一致）。
func TestRegistryDuplicatePanics(t *testing.T) {
	r := chanregistry.New()
	r.Register("dup", fakeCompressor{supports: map[int]bool{1: true}})
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register did not panic")
		}
	}()
	r.Register("dup", fakeCompressor{supports: map[int]bool{2: true}})
}

// TestRegistryCompactKeyErrorPropagates 覆盖 CompactKey 的 error 原样透传（注册表不吞错）。
func TestRegistryCompactKeyErrorPropagates(t *testing.T) {
	r := chanregistry.New()
	wantErr := errors.New("boom")
	r.Register("bad", fakeCompressor{supports: map[int]bool{1: true}, err: wantErr})

	c := r.GetCompressor(1)
	if c == nil {
		t.Fatal("GetCompressor(1) = nil, want non-nil")
	}
	if _, err := c.CompactKey("x"); !errors.Is(err, wantErr) {
		t.Errorf("CompactKey err = %v, want %v", err, wantErr)
	}
}
