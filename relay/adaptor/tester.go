package adaptor

import (
	"context"

	"github.com/pai801/myapi/relay/meta"
)

// TestResult 是一次连通性探针的**结构化诊断材料**：由渠道实现采集，由 controller 统一落库。
//
// 渠道只负责**采集**，不负责**落库**：TestChannel 返回本结构体，controller.testChannel
// 决定何时、以何种形状写入 model.Log（见 controller/channel-test.go）。
//
// # 字段
//
//   - Summary：客户端可见的响应摘要，语义与加宽前的 TestChannel 返回值完全一致。
//   - RequestBody：管线实际产出的**出站**请求体；未构造出站请求时为空。
//   - ResponseBody：成功时为聚合后的 chat.completion 响应体；失败时为上游错误体节选。
//   - PromptTokens / CompletionTokens / CachedTokens：上游 usage 的三个 token 口径，
//     无 usage 时为 0；cached 口径同消费日志（见 relay/controller/helper.go 的 postConsumeQuota）。
//
// # 硬约束
//
// TestResult 只含 string 与 int，MUST NOT 引入 http.Header 或任何非标准库类型。
// 请求头不在本能力的数据面内（见 Tester 文档注释的职责边界）。
//
// # 跨仓镜像约束（私有库 MUST 遵守）
//
// TestResult 是**命名结构体**。私有库 myapi-server 的 channelkit 镜像契约要求「与公开库
// 逐字一致」，但 MUST NOT 另定义一份字段相同的 TestResult：Go 中两个字段相同的命名结构体
// 仍是**不同类型**，渠道实现的同一方法无法同时满足 adaptor.Tester 与 channelkit.Tester，
// 其 internal/channelkit/dependency_test.go 的编译期断言必然失败。
//
// 故镜像 MUST 以**类型别名**保持类型身份：
//
//	type TestResult = adaptor.TestResult
//
// 即：镜像仅在签名涉及内置类型时复写；签名涉及命名类型时以别名保类型身份。
type TestResult struct {
	Summary          string // 客户端可见文案（原返回值语义不变）
	RequestBody      string // 管线产出的出站请求体
	ResponseBody     string // 成功=聚合后的 chat.completion；失败=上游错误体节选
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

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
// 实现方只负责**发起一次探针**并返回**结构化诊断材料**（TestResult：摘要 + 出站请求体 +
// 响应体 + token 用量），MUST NOT 直接写入测试日志或访问数据库：测试日志
// （model.RecordTestLog）由 controller 统一记录，一次委托测试对应且仅对应 controller 侧的
// 一条日志。渠道只负责**采集**，落库时机与字段形状由 controller 决定。
//
// TestResult MUST NOT 承载请求头（无 http.Header 字段）：请求头不在本能力的数据面内，
// controller 在测试路径继续将 Log.RequestHeader 留空。
//
// # 失败语义
//
// 失败时 MUST 返回明确、可归类的错误（鉴权/网络/上游业务错误），MUST NOT 以合成成功
// （如伪造 200 摘要）冒充成功。实现方负责识别上游「HTTP 200 + 业务错误信封」这类伪成功，
// 并将其转为错误返回。
//
// 失败时 SHALL 仍交回**已捕获**的诊断材料（已构造出站请求时）：TestResult.RequestBody 为
// 出站体，TestResult.ResponseBody 为上游错误体节选。仅当出站请求尚未构造（如凭证解析即失败）
// 时 RequestBody 为空——这是正确语义（确实没发出请求）。
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
	// 成功时 result 承载可展示摘要与已采集的诊断材料、err 为 nil；失败时 err 描述错误类别，
	// MUST NOT 泄露凭证或上游响应原文，且 SHALL 仍交回已捕获的诊断材料（见「失败语义」）。
	// 实现方不得访问数据库或写入测试日志。
	TestChannel(ctx context.Context, m *meta.Meta) (TestResult, error)
}
