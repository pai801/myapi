package controller

// 本文件覆盖契约「错误判定按渠道类型分派到渠道能力」在 relay/controller 侧的分派点
// （isErrorHappened）。全部用例以临时注册的适配器构造，收尾经 t.Cleanup 还原全局注册表，
// 不引入任何私有渠道名或私有类型号。
//
// 分派点结构（见 relay/controller/helper.go）：
//
//	adp := relay.GetAdaptor(meta.APIType)
//	if adp != nil {
//	    if semantics, ok := adp.(adaptor.ResponseSemantics); ok {
//	        return semantics.IsUpstreamError(meta, resp)
//	    }
//	}
//	// 以下为 HEAD 既有默认逻辑，逐字节不变
//
// 5.2 集成：本文件负责四路组合中的 relay-controller 两路——实现 ResponseSemantics 的渠道
// 覆写判错、未实现者走默认判定；controller 侧 Tester 两路由
// controller/channel_test_capability_test.go 覆盖。

import (
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/apitype"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
)

const (
	// 三个临时 apiType 与一个临时渠道类型均落在内置区间之外，
	// 避免污染 relay/register.go 登记的 21 个内置渠道或劫持内置映射。
	semanticsTrueAPIType  = 100001 // 命中 ResponseSemantics 且固定返回 true
	semanticsFalseAPIType = 100002 // 命中 ResponseSemantics 且固定返回 false
	semanticsPlainAPIType = 100003 // 未实现 ResponseSemantics，走默认逻辑
	semanticsTestChannel  = 100101 // 扩展渠道类型（内置范围之外）
)

// semanticsAdaptorStub 是本文件测试用的九方法 Adaptor 最小实现，**不实现** ResponseSemantics。
// 用于验证「未实现能力的适配器走既有默认逻辑」。
type semanticsAdaptorStub struct{}

func (s *semanticsAdaptorStub) Init(meta *meta.Meta) {}

func (s *semanticsAdaptorStub) GetRequestURL(meta *meta.Meta) (string, error) {
	return "", nil
}

func (s *semanticsAdaptorStub) SetupRequestHeader(c *gin.Context, req *http.Request, meta *meta.Meta) error {
	return nil
}

func (s *semanticsAdaptorStub) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	return nil, nil
}

func (s *semanticsAdaptorStub) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	return nil, nil
}

func (s *semanticsAdaptorStub) DoRequest(c *gin.Context, meta *meta.Meta, requestBody io.Reader) (*http.Response, error) {
	return nil, nil
}

func (s *semanticsAdaptorStub) DoResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	return nil, nil
}

func (s *semanticsAdaptorStub) GetModelList() []string { return nil }

func (s *semanticsAdaptorStub) GetChannelName() string { return "" }

// semanticsCapableAdaptorStub 在九方法基础上额外实现 ResponseSemantics：
// 固定返回 result，并记录被调用次数以证明分派确实命中能力。
type semanticsCapableAdaptorStub struct {
	semanticsAdaptorStub
	result bool
}

func (s *semanticsCapableAdaptorStub) IsUpstreamError(m *meta.Meta, resp *http.Response) bool {
	atomic.AddInt64(&semanticsCapableCalls, 1)
	return s.result
}

// semanticsCapableCalls 记录 ResponseSemantics 被分派调用的次数（用例间经 Store 复位）。
var semanticsCapableCalls int64

// registerSemanticsTestAdaptors 注册本文件所需的临时适配器与渠道映射；
// 返回前经 t.Cleanup 完整还原 adaptor / channeltype 两个全局注册表。
func registerSemanticsTestAdaptors(t *testing.T) {
	t.Helper()
	adaptor.Register(semanticsTrueAPIType, func() adaptor.Adaptor {
		return &semanticsCapableAdaptorStub{result: true}
	})
	adaptor.Register(semanticsFalseAPIType, func() adaptor.Adaptor {
		return &semanticsCapableAdaptorStub{result: false}
	})
	adaptor.Register(semanticsPlainAPIType, func() adaptor.Adaptor {
		return &semanticsAdaptorStub{}
	})
	// 模拟扩展渠道「自身注册」：渠道类型 → API 类型。分派点不读取该映射（只读 meta.APIType），
	// 但集成用例经 channeltype.ToAPIType 走完整映射链以证明「新增渠道无需改动分派逻辑」。
	channeltype.RegisterAPIType(semanticsTestChannel, semanticsTrueAPIType)
	t.Cleanup(func() {
		adaptor.Unregister(semanticsTrueAPIType)
		adaptor.Unregister(semanticsFalseAPIType)
		adaptor.Unregister(semanticsPlainAPIType)
		channeltype.UnregisterAPIType(semanticsTestChannel)
	})
}

// callIsErrorHappened 调用被测函数并把 panic 显式转为测试失败（用于锁定「不 panic」契约）。
func callIsErrorHappened(t *testing.T, m *meta.Meta, resp *http.Response) (got bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("isErrorHappened panicked (apiType=%d channelType=%d): %v", m.APIType, m.ChannelType, r)
		}
	}()
	return isErrorHappened(m, resp)
}

// semanticsJSONResponse 构造带 application/json Content-Type 的响应，用于流式检查分支。
func semanticsJSONResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       http.NoBody,
	}
}

// TestResponseSemanticsOverrideHitWinsOverDefault 覆盖任务 3.2：命中 ResponseSemantics 时，
// 其返回值即为判定结果，且优先于既有默认逻辑。
func TestResponseSemanticsOverrideHitWinsOverDefault(t *testing.T) {
	registerSemanticsTestAdaptors(t)
	atomic.StoreInt64(&semanticsCapableCalls, 0)

	// 默认会判错（500）的响应：能力返回 false 时必须被覆写为 false。
	m500 := &meta.Meta{APIType: semanticsFalseAPIType, ChannelType: channeltype.OpenAI}
	if got := callIsErrorHappened(t, m500, semanticsJSONResponse(http.StatusInternalServerError)); got {
		t.Fatalf("ResponseSemantics(false) over 500 response = %v, want false (override must win)", got)
	}

	// 默认不判错（200 非流式）的响应：能力返回 true 时必须被覆写为 true。
	m200 := &meta.Meta{APIType: semanticsTrueAPIType, ChannelType: channeltype.OpenAI}
	if got := callIsErrorHappened(t, m200, semanticsJSONResponse(http.StatusOK)); !got {
		t.Fatalf("ResponseSemantics(true) over 200 response = %v, want true (override must win)", got)
	}

	// 能力确实被分派调用（工厂每次新建实例，故用计数而非实例字段断言）。
	if n := atomic.LoadInt64(&semanticsCapableCalls); n != 2 {
		t.Fatalf("ResponseSemantics.IsUpstreamError call count = %d, want 2", n)
	}
}

// TestResponseSemanticsMissFallsBackToDefault 覆盖任务 3.3：适配器未实现 ResponseSemantics 时，
// 判定与既有默认逻辑一致（含 AwsClaude 空响应特例）。
func TestResponseSemanticsMissFallsBackToDefault(t *testing.T) {
	registerSemanticsTestAdaptors(t)
	atomic.StoreInt64(&semanticsCapableCalls, 0)

	// 注册的适配器本身不得实现 ResponseSemantics。
	if _, ok := adaptor.Get(semanticsPlainAPIType).(adaptor.ResponseSemantics); ok {
		t.Fatalf("plain test adaptor unexpectedly implements ResponseSemantics")
	}

	cases := []struct {
		name        string
		channelType int
		resp        *http.Response
		isStream    bool
		want        bool
	}{
		{"500 判错", channeltype.OpenAI, semanticsJSONResponse(http.StatusInternalServerError), false, true},
		{"200 非流式不判错", channeltype.OpenAI, semanticsJSONResponse(http.StatusOK), false, false},
		{"200 流式 JSON 判错", channeltype.OpenAI, semanticsJSONResponse(http.StatusOK), true, true},
		{"空响应判错（非 AwsClaude）", channeltype.OpenAI, nil, false, true},
		{"AwsClaude 空响应不判错", channeltype.AwsClaude, nil, false, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := &meta.Meta{APIType: semanticsPlainAPIType, ChannelType: tc.channelType, IsStream: tc.isStream}
			if got := callIsErrorHappened(t, m, tc.resp); got != tc.want {
				t.Fatalf("miss fallback = %v, want %v", got, tc.want)
			}
		})
	}

	if n := atomic.LoadInt64(&semanticsCapableCalls); n != 0 {
		t.Fatalf("capable path invoked %d times on miss-only test, want 0", n)
	}
}

// TestResponseSemanticsNilAdaptorSafeFallback 覆盖任务 3.3：adp == nil（apiType 未注册）时
// 安全回落既有默认逻辑，MUST NOT 因类型断言 panic。
func TestResponseSemanticsNilAdaptorSafeFallback(t *testing.T) {
	const unregisteredAPIType = 999999
	if a := adaptor.Get(unregisteredAPIType); a != nil {
		t.Fatalf("test precondition: apiType %d must be unregistered, got %T", unregisteredAPIType, a)
	}

	cases := []struct {
		name        string
		channelType int
		resp        *http.Response
		isStream    bool
		want        bool
	}{
		{"nil adp 500 判错", channeltype.OpenAI, semanticsJSONResponse(http.StatusInternalServerError), false, true},
		{"nil adp 200 非流式不判错", channeltype.OpenAI, semanticsJSONResponse(http.StatusOK), false, false},
		{"nil adp 空响应判错", channeltype.OpenAI, nil, false, true},
		{"nil adp AwsClaude 空响应不判错", channeltype.AwsClaude, nil, false, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := &meta.Meta{APIType: unregisteredAPIType, ChannelType: tc.channelType, IsStream: tc.isStream}
			if got := callIsErrorHappened(t, m, tc.resp); got != tc.want {
				t.Fatalf("nil-adp fallback = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResponseSemanticsDefaultEquivalenceForSpecialChannelTypes 覆盖任务 3.3：AwsClaude / DeepL /
// Replicate 三特判在**未实现能力**（走真实内置适配器）与**实现能力**（覆写）两种情形下均符合契约。
func TestResponseSemanticsDefaultEquivalenceForSpecialChannelTypes(t *testing.T) {
	registerSemanticsTestAdaptors(t)

	type scenario struct {
		name        string
		channelType int
		apiType     int
		resp        *http.Response
		isStream    bool
		wantDefault bool
	}
	scenarios := []scenario{
		// AwsClaude：空响应不判错；其余响应仍走通用状态码/流式检查。
		{"AwsClaude 空响应", channeltype.AwsClaude, apitype.AwsClaude, nil, false, false},
		{"AwsClaude 500", channeltype.AwsClaude, apitype.AwsClaude, semanticsJSONResponse(http.StatusInternalServerError), false, true},
		{"AwsClaude 200 流式 JSON", channeltype.AwsClaude, apitype.AwsClaude, semanticsJSONResponse(http.StatusOK), true, true},
		// DeepL：跳过流式检查；但非 200 仍在状态码分支判错。
		{"DeepL 200 流式 JSON 跳过流式检查", channeltype.DeepL, apitype.DeepL, semanticsJSONResponse(http.StatusOK), true, false},
		{"DeepL 500", channeltype.DeepL, apitype.DeepL, semanticsJSONResponse(http.StatusInternalServerError), false, true},
		// Replicate：201 建任务不判错；流式 JSON 为任务信息不判错。
		{"Replicate 201 建任务", channeltype.Replicate, apitype.Replicate, semanticsJSONResponse(http.StatusCreated), false, false},
		{"Replicate 200 流式 JSON", channeltype.Replicate, apitype.Replicate, semanticsJSONResponse(http.StatusOK), true, false},
		{"Replicate 500", channeltype.Replicate, apitype.Replicate, semanticsJSONResponse(http.StatusInternalServerError), false, true},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name+"/默认", func(t *testing.T) {
			m := &meta.Meta{APIType: sc.apiType, ChannelType: sc.channelType, IsStream: sc.isStream}
			if got := callIsErrorHappened(t, m, sc.resp); got != sc.wantDefault {
				t.Fatalf("default = %v, want %v (byte-identical legacy decision)", got, sc.wantDefault)
			}
		})
		t.Run(sc.name+"/能力覆写", func(t *testing.T) {
			// 选择与默认判定相反的临时能力，确保覆写可被观测。
			overrideAPIType := semanticsTrueAPIType
			if sc.wantDefault {
				overrideAPIType = semanticsFalseAPIType
			}
			m := &meta.Meta{APIType: overrideAPIType, ChannelType: sc.channelType, IsStream: sc.isStream}
			if got := callIsErrorHappened(t, m, sc.resp); got != !sc.wantDefault {
				t.Fatalf("override = %v, want %v", got, !sc.wantDefault)
			}
		})
	}
}

// TestResponseSemanticsFourWayComposition 覆盖任务 5.2 的 relay-controller 两路组合：
//   - 实现 ResponseSemantics 的扩展渠道（经 channeltype.RegisterAPIType 注册）覆写判错；
//   - 未实现能力的渠道走默认判定。
//
// 完整四路中的 controller 侧 Tester 两路由 controller/channel_test_capability_test.go 覆盖；
// 本用例锁定「错误语义能力」两路在同一分派点上的组合，证明扩展渠道接入无需改动分派逻辑。
func TestResponseSemanticsFourWayComposition(t *testing.T) {
	registerSemanticsTestAdaptors(t)
	atomic.StoreInt64(&semanticsCapableCalls, 0)

	t.Run("扩展渠道注册后按 APIType 分派到其能力", func(t *testing.T) {
		// 扩展渠道仅登记自身「渠道类型 → API 类型」映射，分派逻辑无需任何改动。
		apiType := channeltype.ToAPIType(semanticsTestChannel)
		if apiType != semanticsTrueAPIType {
			t.Fatalf("ToAPIType(%d) = %d, want %d", semanticsTestChannel, apiType, semanticsTrueAPIType)
		}
		// 该渠道的适配器实现了 ResponseSemantics 且返回 true：即使响应默认不判错，也必须判错。
		m := &meta.Meta{APIType: apiType, ChannelType: semanticsTestChannel, IsStream: false}
		if got := callIsErrorHappened(t, m, semanticsJSONResponse(http.StatusOK)); !got {
			t.Fatalf("capable extension channel over 200 = %v, want true", got)
		}
	})

	t.Run("未实现能力的渠道走默认判定", func(t *testing.T) {
		// 未实现 ResponseSemantics 的临时适配器：500 判错、200 非流式不判错。
		m500 := &meta.Meta{APIType: semanticsPlainAPIType, ChannelType: semanticsTestChannel, IsStream: false}
		if got := callIsErrorHappened(t, m500, semanticsJSONResponse(http.StatusInternalServerError)); !got {
			t.Fatalf("plain adaptor over 500 = %v, want true", got)
		}
		m200 := &meta.Meta{APIType: semanticsPlainAPIType, ChannelType: semanticsTestChannel, IsStream: false}
		if got := callIsErrorHappened(t, m200, semanticsJSONResponse(http.StatusOK)); got {
			t.Fatalf("plain adaptor over 200 = %v, want false", got)
		}
	})

	if n := atomic.LoadInt64(&semanticsCapableCalls); n != 1 {
		t.Fatalf("capable path invoked %d times, want 1 (only the capable extension channel)", n)
	}
}
