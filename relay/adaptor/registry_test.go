package adaptor_test

import (
	"testing"

	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/gemini"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/apitype"

	// 空导入触发 relay/register.go 的集中注册，使 registry 处于运行期真实状态。
	_ "github.com/pai801/myapi/relay"
)

// testAPIType 取一个落在真实枚举区间之外的基数，避免污染内置完整性断言。
const testAPIType = 100000

// builtinAPITypes 是 relay/register.go 应当登记的 21 个内置渠道，顺序与注册顺序一致。
//
// 扩展渠道不在其中：本仓不注册任何扩展渠道（扩展方在其 init() 中经 adaptor registry 注册），
// 故此处只列 21 项（PRD §5.5）。
var builtinAPITypes = []int{
	apitype.AIProxyLibrary, apitype.Ali, apitype.Anthropic, apitype.AwsClaude,
	apitype.Baidu, apitype.Gemini, apitype.OpenAI, apitype.PaLM,
	apitype.Tencent, apitype.Xunfei, apitype.Zhipu, apitype.Ollama,
	apitype.Coze, apitype.Cohere, apitype.Cloudflare, apitype.DeepL,
	apitype.VertexAI, apitype.Proxy, apitype.Replicate, apitype.Codex,
	apitype.ChatGPTSub,
}

// TestRegistry 用有序子测试覆盖 registry 语义。
func TestRegistry(t *testing.T) {
	// 本子测试必须最先执行：它断言内置注册表恰好 21 项；后续子测试若用 MustRegister
	// 覆盖会影响计数。请勿为本测试或本文件加 t.Parallel()。
	t.Run("builtin_completeness", func(t *testing.T) {
		if len(builtinAPITypes) != 21 {
			t.Fatalf("test table size = %d, want 21", len(builtinAPITypes))
		}
		types := adaptor.RegisteredAPITypes()
		if len(types) != 21 {
			t.Fatalf("registered adaptor count = %d, want 21", len(types))
		}
		for i, at := range builtinAPITypes {
			if types[i] != at {
				t.Errorf("RegisteredAPITypes()[%d] = %d, want %d (order not preserved)", i, types[i], at)
			}
			if a := adaptor.Get(at); a == nil {
				t.Errorf("Get(%d) = nil, want non-nil adaptor", at)
			}
		}
	})

	t.Run("concrete_types", func(t *testing.T) {
		if _, ok := adaptor.Get(apitype.OpenAI).(*openai.Adaptor); !ok {
			t.Errorf("Get(apitype.OpenAI) = %T, want *openai.Adaptor", adaptor.Get(apitype.OpenAI))
		}
	})

	t.Run("register_then_get", func(t *testing.T) {
		adaptor.Register(testAPIType, func() adaptor.Adaptor { return &openai.Adaptor{} })
		a := adaptor.Get(testAPIType)
		if a == nil {
			t.Fatal("Get after Register = nil, want non-nil")
		}
		if _, ok := a.(*openai.Adaptor); !ok {
			t.Errorf("Get(%d) = %T, want *openai.Adaptor", testAPIType, a)
		}
	})

	t.Run("unregistered_returns_nil", func(t *testing.T) {
		if a := adaptor.Get(testAPIType + 500); a != nil {
			t.Errorf("Get(unregistered) = %v, want nil", a)
		}
	})

	t.Run("duplicate_register_panics", func(t *testing.T) {
		const dup = testAPIType + 1
		adaptor.Register(dup, func() adaptor.Adaptor { return &openai.Adaptor{} })
		defer func() {
			if r := recover(); r == nil {
				t.Error("duplicate Register did not panic")
			}
		}()
		adaptor.Register(dup, func() adaptor.Adaptor { return &openai.Adaptor{} })
	})

	t.Run("must_register_overrides", func(t *testing.T) {
		const over = testAPIType + 2
		adaptor.Register(over, func() adaptor.Adaptor { return &openai.Adaptor{} })
		adaptor.MustRegister(over, func() adaptor.Adaptor { return &gemini.Adaptor{} })
		if _, ok := adaptor.Get(over).(*gemini.Adaptor); !ok {
			t.Errorf("Get after MustRegister override = %T, want *gemini.Adaptor", adaptor.Get(over))
		}
	})

	t.Run("factory_panic_isolated", func(t *testing.T) {
		const boom = testAPIType + 3
		adaptor.Register(boom, func() adaptor.Adaptor { panic("boom") })
		if a := adaptor.Get(boom); a != nil {
			t.Errorf("Get on panicking factory = %v, want nil", a)
		}
	})
}
