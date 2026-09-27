package adaptor

import (
	"context"

	"github.com/pai801/myapi/relay/meta"
)

// ModelLister 是渠道可**选择性**实现的「模型清单拉取」能力接口。
//
// 它是独立于 Adaptor 必选九方法接口（见 interface.go）的**可选**接口，MUST NOT
// 被并入 Adaptor：既有 Adaptor 实现无需任何改动即可继续编译与运行。未实现本接口的
// 渠道，其拉取模型请求由 controller.FetchChannelModels 走通用 OpenAI 兼容回退路径
// （Authorization: Bearer + {base}/v1/models，失败回退 {base}/models）。
//
// 分派方式：controller.FetchChannelModels 对 relay.GetAdaptor(apiType) 返回的实例做
// 类型断言，实现本接口则委托给渠道自身实现，否则通用回退。渠道「实现接口即启用」，
// 新增渠道无需改动公开库的 controller 分派逻辑。
//
// # 上下文（入参 m）
//
// m 提供渠道身份与上下文，字段取自 meta.Meta 现有字段（本接口不新增任何字段）：
//   - ChannelType：渠道类型数值（对应 channeltype 枚举）。
//   - ChannelId：渠道记录 id（新建渠道可能为 0）。
//   - BaseURL：渠道配置中的代理基址；为空时渠道实现应按其声明默认基址回落。
//     与推理路径同口径：relay/meta/relay_meta.go 的 GetByContext 在 BaseURL 为空时回落到
//     channeltype.ChannelBaseURLs[ChannelType]。故缺基址 MUST NOT 导致本能力被拒绝。
//   - APIKey：渠道密钥（形态由渠道自定义，鉴权解耦由渠道实现负责）。
//   - Config：解析后的渠道配置（model.ChannelConfig，含 typed 字段与只读 Raw 全量键视图）。
//
// # 归一责任在渠道侧
//
// 返回的 []string 即**最终模型 id 列表**：去重、保序、trim 均由实现方负责。
// 调用方仅过滤空串，不做任何二次归一（如排序、去重或大小写折叠），以免与渠道语义冲突。
//
// # 失败语义
//
// 失败时 MUST 返回明确错误（可归类的鉴权/网络/上游业务错误），MUST NOT 返回空清单
// 冒充成功。实现方负责识别上游「HTTP 200 + 业务错误信封」这类伪成功
// （如 {"code":"100002","desc":"…"}），并将其转为错误返回。
//
// # 实现方自由度
//
// 实现方自行选择出站客户端（可用渠道指纹客户端，以与推理路径保持出站一致性）与
// 端点/鉴权形态。错误文案 MUST NOT 泄露密钥或凭证；上游响应体仅可作为服务端诊断证据。
type ModelLister interface {
	// FetchModels 拉取该渠道的模型清单，返回最终模型 id 列表。
	// 归一（去重/保序/trim）由实现方负责；失败必须返回明确错误，不得以空清单冒充成功。
	FetchModels(ctx context.Context, m *meta.Meta) ([]string, error)
}
