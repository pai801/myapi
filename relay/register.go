package relay

// 集中注册：由 relay/adaptor.go 的 switch 平移而来，共 21 项，勿随意增删。
// 各内置渠道在此登记一次，GetAdaptor 通过 adaptor registry 查表（PRD §5.1 / §5.2 决策 D3）。
// 注册顺序与改造前的 switch case 顺序保持一致，供 RegisteredAPITypes 保序遍历。
//
// 扩展渠道不在此登记：由外部插件在其 init() 中经 adaptor registry 注册（PRD §5.5 / §5.11）。
// 未注册的 apiType GetAdaptor 返回 nil，调用方（controller/model.go 两个遍历循环）已有 nil 守卫。

import (
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/aiproxy"
	"github.com/pai801/myapi/relay/adaptor/ali"
	"github.com/pai801/myapi/relay/adaptor/anthropic"
	"github.com/pai801/myapi/relay/adaptor/aws"
	"github.com/pai801/myapi/relay/adaptor/baidu"
	"github.com/pai801/myapi/relay/adaptor/chatgptsub"
	"github.com/pai801/myapi/relay/adaptor/cloudflare"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/adaptor/cohere"
	"github.com/pai801/myapi/relay/adaptor/coze"
	"github.com/pai801/myapi/relay/adaptor/deepl"
	"github.com/pai801/myapi/relay/adaptor/gemini"
	"github.com/pai801/myapi/relay/adaptor/ollama"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/adaptor/palm"
	"github.com/pai801/myapi/relay/adaptor/proxy"
	"github.com/pai801/myapi/relay/adaptor/replicate"
	"github.com/pai801/myapi/relay/adaptor/tencent"
	"github.com/pai801/myapi/relay/adaptor/vertexai"
	"github.com/pai801/myapi/relay/adaptor/xunfei"
	"github.com/pai801/myapi/relay/adaptor/zhipu"
	"github.com/pai801/myapi/relay/apitype"
)

func init() {
	adaptor.Register(apitype.AIProxyLibrary, func() adaptor.Adaptor { return &aiproxy.Adaptor{} })
	adaptor.Register(apitype.Ali, func() adaptor.Adaptor { return &ali.Adaptor{} })
	adaptor.Register(apitype.Anthropic, func() adaptor.Adaptor { return &anthropic.Adaptor{} })
	adaptor.Register(apitype.AwsClaude, func() adaptor.Adaptor { return &aws.Adaptor{} })
	adaptor.Register(apitype.Baidu, func() adaptor.Adaptor { return &baidu.Adaptor{} })
	adaptor.Register(apitype.Gemini, func() adaptor.Adaptor { return &gemini.Adaptor{} })
	adaptor.Register(apitype.OpenAI, func() adaptor.Adaptor { return &openai.Adaptor{} })
	adaptor.Register(apitype.PaLM, func() adaptor.Adaptor { return &palm.Adaptor{} })
	adaptor.Register(apitype.Tencent, func() adaptor.Adaptor { return &tencent.Adaptor{} })
	adaptor.Register(apitype.Xunfei, func() adaptor.Adaptor { return &xunfei.Adaptor{} })
	adaptor.Register(apitype.Zhipu, func() adaptor.Adaptor { return &zhipu.Adaptor{} })
	adaptor.Register(apitype.Ollama, func() adaptor.Adaptor { return &ollama.Adaptor{} })
	adaptor.Register(apitype.Coze, func() adaptor.Adaptor { return &coze.Adaptor{} })
	adaptor.Register(apitype.Cohere, func() adaptor.Adaptor { return &cohere.Adaptor{} })
	adaptor.Register(apitype.Cloudflare, func() adaptor.Adaptor { return &cloudflare.Adaptor{} })
	adaptor.Register(apitype.DeepL, func() adaptor.Adaptor { return &deepl.Adaptor{} })
	adaptor.Register(apitype.VertexAI, func() adaptor.Adaptor { return &vertexai.Adaptor{} })
	adaptor.Register(apitype.Proxy, func() adaptor.Adaptor { return &proxy.Adaptor{} })
	adaptor.Register(apitype.Replicate, func() adaptor.Adaptor { return &replicate.Adaptor{} })
	adaptor.Register(apitype.Codex, func() adaptor.Adaptor { return &codex.Adaptor{OpenAiImpl: &openai.Adaptor{}} })
	adaptor.Register(apitype.ChatGPTSub, func() adaptor.Adaptor { return &chatgptsub.Adaptor{OpenAiImpl: &openai.Adaptor{}} })

	// 凭证压缩能力（PRD §5.11 / §7.3）不在主仓登记：需要压缩的渠道 Key 是整段凭证 JSON，
	// 其压缩器由扩展方在 init() 里经 chanregistry.Register 注入。无注册时
	// controller 查不到即回退到原有的「按 '\n' 拆分」路径。
}
