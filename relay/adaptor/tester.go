package adaptor

import (
	"context"

	"github.com/pai801/myapi/relay/meta"
)

// Tester 是渠道可**选择性**实现的「连通性测试」能力接口。
//
// 它是独立于 Adaptor 必选九方法接口（见 interface.go）的**可选**接口，MUST NOT
// 被并入 Adaptor：既有 Adaptor 实现无需任何改动即可继续编译与运行。未实现本接口的
// 渠道，其连通性测试由 controller 走既有通用测试流程（非流式请求 + HTTP 200 +
// JSON choices[0].content 上游约定）。
//
// # 分派方式：接口存在性即能力发现
//
// controller.testChannel 对 relay.GetAdaptor(apiType) 返回的实例做类型断言：实现本接口
// 则委托给渠道自身的 TestChannel，否则通用流程。渠道「实现接口即启用」，新增渠道无需
// 改动公开库 controller 的分派逻辑。Go 侧能力发现以**接口存在性**为唯一依据；
// chandesc.Capabilities.SupportsTest（见 relay/chandesc/descriptor.go）仅为纯前端提示
// 元数据，MUST NOT 被当作 Go 侧行为分支，MUST NOT 形成与之并行的第二真源。
//
// 与 ModelLister（见 modellister.go）、BalanceQuerier（见 balancequerier.go）同构：
// 入参为 context.Context + *meta.Meta，出参为值 + error，便于分派方以统一的类型断言
// 模式处理各类可选能力。
//
// # 上下文（入参 m）
//
// m 提供渠道身份与上下文，字段取自 meta.Meta 现有字段（本接口不新增任何字段）：
//   - ChannelType：渠道类型数值（对应 channeltype 枚举）。
//   - APIType：由 ChannelType 推导的适配器类型，可用于渠道实现内的分支判断。
//   - ChannelId：渠道记录 id（新建渠道可能为 0）。
//   - BaseURL：渠道配置中的代理基址；为空时渠道实现应按其声明默认基址回落。
//     与推理路径同口径：relay/meta/relay_meta.go 的 GetByContext 在 BaseURL 为空时回落到
//     channeltype.ChannelBaseURLs[ChannelType]。故缺基址 MUST NOT 导致本能力被拒绝。
//   - APIKey：渠道密钥（形态由渠道自定义，鉴权解耦由渠道实现负责）。
//   - Config：解析后的渠道配置（model.ChannelConfig，含 typed 字段与只读 Raw 全量键视图）。
//   - OriginModelName / ActualModelName：既有模型名映射语义，探针上下文/日志按需沿用。
//
// # 职责边界
//
// 实现方只负责**发起一次探针**并返回**可展示的响应摘要**（message）。MUST NOT 访问数据库，
// MUST NOT 写入测试日志：测试日志（model.RecordTestLog）由 controller 统一记录，一次委托测试
// 对应且仅对应 controller 侧的一条日志。
//
// # 失败语义
//
// 失败时 MUST 返回明确、可归类的错误（鉴权/网络/上游业务错误），MUST NOT 以合成成功
// （如伪造 200 摘要）冒充成功。实现方负责识别上游「HTTP 200 + 业务错误信封」这类伪成功，
// 并将其转为错误返回。
//
// # 凭证安全约束
//
// 错误文案 MUST NOT 包含密钥或任何凭证信息（APIKey、token、cookie 等），MUST NOT 包含
// 上游响应原文；上游响应体仅可作为服务端诊断证据（如写入日志），MUST NOT 随错误回传至
// 客户端或对外暴露。
//
// # 实现方自由度
//
// 实现方自行选择出站客户端（可用渠道指纹客户端，以与推理路径保持出站一致性）与
// 端点/鉴权形态；响应摘要的文案与长度语义由渠道实现负责统一。
type Tester interface {
	// TestChannel 使用 ctx 与 m 探测该渠道的连通性。
	// 成功时 message 为可展示的响应摘要、err 为 nil；失败时 err 描述错误类别，
	// MUST NOT 泄露凭证或上游响应原文。实现方不得访问数据库或写入测试日志。
	TestChannel(ctx context.Context, m *meta.Meta) (string, error)
}
