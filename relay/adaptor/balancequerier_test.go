package adaptor_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/meta"
)

// capabilityStub 是一个「adaptor 形状」的渠道桩：它同时实现 ModelLister 与
// BalanceQuerier 两个可选能力接口，用于验证可选能力接口可被同一渠道类型并存实现，
// 且签名与既有 ModelLister 同构。
type capabilityStub struct{}

// FetchModels 满足 adaptor.ModelLister。
func (capabilityStub) FetchModels(ctx context.Context, m *meta.Meta) ([]string, error) {
	return nil, nil
}

// QueryBalance 满足 adaptor.BalanceQuerier。
func (capabilityStub) QueryBalance(ctx context.Context, m *meta.Meta) (float64, error) {
	return 0, nil
}

// 编译期签名断言：桩类型满足两个可选能力接口。
var (
	_ adaptor.ModelLister    = capabilityStub{}
	_ adaptor.BalanceQuerier = capabilityStub{}
)

// TestBalanceQuerierSignature 断言 BalanceQuerier 的签名与 ModelLister 同构：
// 入参同为 (context.Context, *meta.Meta)，出参同为 (值, error) 两元组。
func TestBalanceQuerierSignature(t *testing.T) {
	bq := reflect.TypeOf((*adaptor.BalanceQuerier)(nil)).Elem()
	ml := reflect.TypeOf((*adaptor.ModelLister)(nil)).Elem()

	bqMethod, ok := bq.MethodByName("QueryBalance")
	if !ok {
		t.Fatal("BalanceQuerier has no method QueryBalance")
	}
	mlMethod, ok := ml.MethodByName("FetchModels")
	if !ok {
		t.Fatal("ModelLister has no method FetchModels")
	}

	if bqMethod.Type.NumIn() != mlMethod.Type.NumIn() {
		t.Fatalf("param count mismatch: BalanceQuerier=%d, ModelLister=%d",
			bqMethod.Type.NumIn(), mlMethod.Type.NumIn())
	}
	// 接口方法的 reflect 类型不含 receiver，入参从 0 起：context.Context、*meta.Meta。
	for i := 0; i < bqMethod.Type.NumIn(); i++ {
		if bqMethod.Type.In(i) != mlMethod.Type.In(i) {
			t.Errorf("param %d type mismatch: BalanceQuerier=%v, ModelLister=%v",
				i, bqMethod.Type.In(i), mlMethod.Type.In(i))
		}
	}
	// 出参形状：两元组，第二元为 error。
	if bqMethod.Type.NumOut() != 2 || mlMethod.Type.NumOut() != 2 {
		t.Fatalf("return count mismatch: BalanceQuerier=%d, ModelLister=%d",
			bqMethod.Type.NumOut(), mlMethod.Type.NumOut())
	}
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if bqMethod.Type.Out(1) != errorType {
		t.Errorf("BalanceQuerier second return = %v, want error", bqMethod.Type.Out(1))
	}
	if mlMethod.Type.Out(1) != errorType {
		t.Errorf("ModelLister second return = %v, want error", mlMethod.Type.Out(1))
	}

	// 入参类型显式核对，防止 reflect 比较双方同时漂移。
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	metaPtr := reflect.TypeOf((*meta.Meta)(nil))
	if got := bqMethod.Type.In(0); got != ctxType {
		t.Errorf("BalanceQuerier first param = %v, want %v", got, ctxType)
	}
	if got := bqMethod.Type.In(1); got != metaPtr {
		t.Errorf("BalanceQuerier second param = %v, want %v", got, metaPtr)
	}
}
