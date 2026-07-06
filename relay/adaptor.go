package relay

import (
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/aiproxy"
	"github.com/pai801/myapi/relay/adaptor/ali"
	"github.com/pai801/myapi/relay/adaptor/anthropic"
	"github.com/pai801/myapi/relay/adaptor/aws"
	"github.com/pai801/myapi/relay/adaptor/baidu"
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

func GetAdaptor(apiType int) adaptor.Adaptor {
	switch apiType {
	case apitype.AIProxyLibrary:
		return &aiproxy.Adaptor{}
	case apitype.Ali:
		return &ali.Adaptor{}
	case apitype.Anthropic:
		return &anthropic.Adaptor{}
	case apitype.AwsClaude:
		return &aws.Adaptor{}
	case apitype.Baidu:
		return &baidu.Adaptor{}
	case apitype.Gemini:
		return &gemini.Adaptor{}
	case apitype.OpenAI:
		return &openai.Adaptor{}
	case apitype.PaLM:
		return &palm.Adaptor{}
	case apitype.Tencent:
		return &tencent.Adaptor{}
	case apitype.Xunfei:
		return &xunfei.Adaptor{}
	case apitype.Zhipu:
		return &zhipu.Adaptor{}
	case apitype.Ollama:
		return &ollama.Adaptor{}
	case apitype.Coze:
		return &coze.Adaptor{}
	case apitype.Cohere:
		return &cohere.Adaptor{}
	case apitype.Cloudflare:
		return &cloudflare.Adaptor{}
	case apitype.DeepL:
		return &deepl.Adaptor{}
	case apitype.VertexAI:
		return &vertexai.Adaptor{}
	case apitype.Proxy:
		return &proxy.Adaptor{}
	case apitype.Replicate:
		return &replicate.Adaptor{}
	case apitype.Codex:
		return &codex.Adaptor{
			OpenAiImpl: &openai.Adaptor{},
		}
	}
	return nil
}
