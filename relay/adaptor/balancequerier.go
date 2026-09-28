package adaptor

import (
	"context"

	"github.com/pai801/myapi/relay/meta"
)

// BalanceQuerier 是渠道可**选择性**实现的「余额查询」能力接口。
//
// 它是独立于 Adaptor 必选九方法接口（见 interface.go）的**可选**接口，MUST NOT
// 被并入 Adaptor：既有 Adaptor 实现无需任何改动即可继续编译与运行。渠道「实现接口即
// 启用」——实现了本接口的渠道，其余额查询请求由分派方做类型断言后委托给渠道自身实现；
// 未实现的渠道保持既有（legacy）行为，不因缺少本能力而报错或改变原有语义。
//
// 与 ModelLister（见 modellister.go）同构：入参为 context.Context + *meta.Meta，
// 出参为值 + error，便于分派方以统一的类型断言模式处理各类可选能力。
//
// # 上下文（入参 m）
//
// m 提供渠道身份与上下文，字段取自 meta.Meta 现有字段（本接口不新增任何字段）：
//   - ChannelType：渠道类型数值（对应 channeltype 枚举）。
//   - ChannelId：渠道记录 id（新建渠道可能为 0）。
//   - APIType：由 ChannelType 推导的适配器类型，可用于渠道实现内的分支判断。
//   - BaseURL：渠道配置中的代理基址；为空时渠道实现应按其声明默认基址回落。
//     与推理路径同口径：relay/meta/relay_meta.go 的 GetByContext 在 BaseURL 为空时回落到
//     channeltype.ChannelBaseURLs[ChannelType]。故缺基址 MUST NOT 导致本能力被拒绝。
//   - APIKey：渠道密钥（形态由渠道自定义，鉴权解耦由渠道实现负责）。
//   - Config：解析后的渠道配置（model.ChannelConfig，含 typed 字段与只读 Raw 全量键视图）。
//
// # 失败语义
//
// 失败时 MUST 返回明确错误（可归类的鉴权/网络/上游业务错误），MUST NOT 以合成值
// （如 0）冒充成功。查询不到余额与余额确为 0 是两种不同语义，前者必须以错误表达，
// 否则调用方无法区分「余额为零」与「查询失败」，会导致错误的业务决策。
//
// # 凭证安全约束
//
// 错误文案 MUST NOT 包含密钥或任何凭证信息（APIKey、token、cookie 等）；上游响应体
// 仅可作为服务端诊断证据（如写入日志），MUST NOT 随错误回传至客户端或对外暴露。
//
// # 实现方自由度
//
// 实现方自行选择出站客户端（可用渠道指纹客户端，以与推理路径保持出站一致性）与
// 端点/鉴权形态；余额的单位与精度语义由渠道实现负责统一。
type BalanceQuerier interface {
	// QueryBalance 查询该渠道的余额，返回以渠道语义为准的余额数值。
	// 失败必须返回明确错误，不得以合成零值冒充成功；错误文案不得泄露密钥或凭证。
	QueryBalance(ctx context.Context, m *meta.Meta) (float64, error)
}
