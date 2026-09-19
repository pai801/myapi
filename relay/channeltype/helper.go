package channeltype

import (
	"sort"
	"sync"

	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/apitype"
)

// apiTypeRegistry 保存「渠道类型 → API 类型」的外部扩展映射。
//
// 这是给**外部扩展方**用的插件接口：扩展方（例如自带扩展渠道的独立服务）在 init()
// 期调用 RegisterAPIType 登记自己的渠道类型，本仓自身不注册任何扩展渠道。
//
// 之所以做成可注册的表而非硬编码 switch：新增渠道时扩展方无需修改本仓源码，
// 从而做到「本仓一行代码都不用改」。
var (
	apiTypeMu       sync.RWMutex
	apiTypeRegistry = make(map[int]int)
)

// RegisterAPIType 登记一个「渠道类型 → API 类型」映射（供外部扩展方在 init() 期调用）。
//
// 约定：本注册表用于**扩展渠道**——即内置范围（0 .. Dummy-1）之外的新渠道类型。查找优先于
// 内置 switch（保持「注册表优先」语义），因此把**内置**渠道类型登记进来会覆盖其内置映射，
// 这几乎总是误用（例如把内置 OpenAI 劫持到别的 adaptor）。为避免静默劫持，channelType 落在
// 内置范围内时记录一条告警日志；**不 panic、不阻断**，注册仍然生效（保持既有语义不变）。
//
// 同一 channelType 重复注册为「后写覆盖」语义：便于扩展方在测试或多阶段装配中重装。
// 查找发生在运行时，注册发生在 init() 期，故用 RWMutex 保护并发读写。
func RegisterAPIType(channelType int, apiType int) {
	if channelType >= 0 && channelType < Dummy {
		logger.Log.Warnf("channeltype: RegisterAPIType(%d, %d) remaps a built-in channel type (built-in range 0..%d); this overrides the built-in mapping and may hijack built-in channels", channelType, apiType, Dummy-1)
	}
	apiTypeMu.Lock()
	defer apiTypeMu.Unlock()
	apiTypeRegistry[channelType] = apiType
}

// UnregisterAPIType 注销一个此前经 RegisterAPIType 登记的「渠道类型 → API 类型」映射。
//
// 与 RegisterAPIType 配套，供多阶段装配或测试在收尾时还原全局注册表，避免残留登记项
// 遮蔽内置映射。未登记的 channelType 调用本函数为空操作。
func UnregisterAPIType(channelType int) {
	apiTypeMu.Lock()
	defer apiTypeMu.Unlock()
	delete(apiTypeRegistry, channelType)
}

// RegisteredChannelTypes 返回当前所有已登记的扩展渠道类型（升序）。
//
// 与 RegisterAPIType 面向**外部扩展方**不同，本函数面向**本仓消费方**：供本仓在
// 不硬编码任何具体扩展渠道的前提下，枚举「内置范围（0 .. Dummy-1）之外」的渠道类型
// （例如构建模型清单时把扩展渠道一并纳入），从而做到「新增扩展渠道时本仓一行代码都不用改」。
//
// 返回副本而非内部切片，调用方可自由遍历/修改而不影响注册表。升序是为了让结果确定，
// 便于消费方与测试做稳定断言（map 迭代本身无序）。
func RegisteredChannelTypes() []int {
	apiTypeMu.RLock()
	defer apiTypeMu.RUnlock()
	out := make([]int, 0, len(apiTypeRegistry))
	for channelType := range apiTypeRegistry {
		out = append(out, channelType)
	}
	sort.Ints(out)
	return out
}

// lookupRegisteredAPIType 查询外部注册表；命中返回 (apiType, true)。
func lookupRegisteredAPIType(channelType int) (int, bool) {
	apiTypeMu.RLock()
	defer apiTypeMu.RUnlock()
	apiType, ok := apiTypeRegistry[channelType]
	return apiType, ok
}

// ToAPIType 把渠道类型映射为 API 类型。
//
// 先查外部注册表（RegisterAPIType 登记），命中即返回；未命中再走内置 switch（内置渠道）。
// 这样扩展方新增的渠道无需在本仓登记，未注册的渠道类型按既有语义回退到 OpenAI。
func ToAPIType(channelType int) int {
	if apiType, ok := lookupRegisteredAPIType(channelType); ok {
		return apiType
	}

	apiType := apitype.OpenAI
	switch channelType {
	case Anthropic:
		apiType = apitype.Anthropic
	case Baidu:
		apiType = apitype.Baidu
	case PaLM:
		apiType = apitype.PaLM
	case Zhipu:
		apiType = apitype.Zhipu
	case Ali:
		apiType = apitype.Ali
	case Xunfei:
		apiType = apitype.Xunfei
	case AIProxyLibrary:
		apiType = apitype.AIProxyLibrary
	case Tencent:
		apiType = apitype.Tencent
	case Gemini:
		apiType = apitype.Gemini
	case Ollama:
		apiType = apitype.Ollama
	case AwsClaude:
		apiType = apitype.AwsClaude
	case Coze:
		apiType = apitype.Coze
	case Cohere:
		apiType = apitype.Cohere
	case Cloudflare:
		apiType = apitype.Cloudflare
	case DeepL:
		apiType = apitype.DeepL
	case VertextAI:
		apiType = apitype.VertexAI
	case Replicate:
		apiType = apitype.Replicate
	case Proxy:
		apiType = apitype.Proxy
	case Codex:
		apiType = apitype.Codex
	case ChatGPTSub:
		apiType = apitype.ChatGPTSub
	}

	return apiType
}
