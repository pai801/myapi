package channeltype

import "fmt"

// ChannelBaseURLs 是内置渠道类型的默认 BaseURL 表，key 为渠道类型。
//
// 用 map 而非 slice：缺失的渠道类型返回零值 ""（比 slice 越界 panic 更安全），
// 且外部扩展方的扩展渠道无需在本表占位——扩展方按需自行维护其默认 BaseURL。
var ChannelBaseURLs = map[int]string{
	0:  "",
	1:  "https://api.openai.com",
	2:  "https://oa.api2d.net",
	3:  "",
	4:  "https://api.closeai-proxy.xyz",
	5:  "https://api.openai-sb.com",
	6:  "https://api.openaimax.com",
	7:  "https://api.ohmygpt.com",
	8:  "",
	9:  "https://api.caipacity.com",
	10: "https://api.aiproxy.io",
	11: "https://generativelanguage.googleapis.com",
	12: "https://api.api2gpt.com",
	13: "https://api.aigc2d.com",
	14: "https://api.anthropic.com",
	15: "https://aip.baidubce.com",
	16: "https://open.bigmodel.cn",
	17: "https://dashscope.aliyuncs.com",
	18: "",
	19: "https://ai.360.cn",
	20: "https://openrouter.ai/api",
	21: "https://api.aiproxy.io",
	22: "https://fastgpt.run/api/openapi",
	23: "https://hunyuan.tencentcloudapi.com",
	24: "https://generativelanguage.googleapis.com",
	25: "https://api.moonshot.cn",
	26: "https://api.baichuan-ai.com",
	27: "https://api.minimax.chat",
	28: "https://api.mistral.ai",
	29: "https://api.groq.com/openai",
	30: "http://localhost:11434",
	31: "https://api.lingyiwanwu.com",
	32: "https://api.stepfun.com",
	33: "",
	34: "https://api.coze.com",
	35: "https://api.cohere.com",
	36: "https://api.deepseek.com",
	37: "https://api.cloudflare.com",
	38: "https://api-free.deepl.com",
	39: "https://api.together.xyz",
	40: "https://ark.cn-beijing.volces.com",
	41: "https://api.novita.ai/v3/openai",
	42: "",
	43: "",
	44: "https://api.siliconflow.cn",
	45: "https://api.x.ai",
	46: "https://api.replicate.com/v1/models/",
	47: "https://qianfan.baidubce.com",
	48: "https://spark-api-open.xf-yun.com",
	49: "https://dashscope.aliyuncs.com",
	50: "",
	51: "https://generativelanguage.googleapis.com/v1beta/openai/",
	52: "",                    // Codex
	53: "https://chatgpt.com", // ChatGPTSub
}

func init() {
	// 不变量：每个内置渠道类型（0..Dummy-1）都必须在本表中有一条表项（允许空串占位）。
	// 新增内置渠道却忘了登记 BaseURL 时启动即 panic 暴露，等价于改造前
	// 「len(slice) 必须等于渠道类型数」的长度断言。
	for ct := 0; ct < Dummy; ct++ {
		if _, ok := ChannelBaseURLs[ct]; !ok {
			panic(fmt.Sprintf("channeltype: ChannelBaseURLs missing entry for channel type %d", ct))
		}
	}
}
