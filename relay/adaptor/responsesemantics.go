package adaptor

import (
	"net/http"

	"github.com/pai801/myapi/relay/meta"
)

// ResponseSemantics 是渠道可**选择性**实现的「上游响应是否判错」能力接口。
//
// 它是独立于 Adaptor 必选九方法接口（见 interface.go）的**可选**接口，MUST NOT
// 被并入 Adaptor：既有 Adaptor 实现无需任何改动即可继续编译与运行。未实现本接口的
// 渠道，其判错由 relay/controller 的 isErrorHappened 走公开库既有默认逻辑。
//
// # 分派方式：接口存在性即能力发现
//
// relay/controller.isErrorHappened 对 relay.GetAdaptor(meta.APIType) 返回的实例做类型断言：
// 实现本接口则以其 IsUpstreamError 返回值为准，否则走既有默认判定。渠道「实现接口即启用」，
// 新增渠道无需改动公开库 controller 的分派逻辑。
//
// # 能力范围：仅「是否判错」这一二值判定
//
// 本接口**只**抽象「上游响应是否判错」这一二值决策，MUST NOT 承载错误分类、致命性（Fatal）、
// 可重试性、冷却权重、自动禁用或任何其它共享错误处理策略；后者由公开库统一策略负责，
// MUST NOT 由本能力外溢承载。
//
// # 默认判定保留在公开库
//
// 对未实现本接口的渠道，判错保持公开库既有默认逻辑（AwsClaude 空响应不判错、DeepL 跳过
// 流式检查、Replicate 保留 201 与流式特例等），该默认逻辑 MUST NOT 因引入本能力而回归。
//
// # 入参与 nil 语义
//
// resp 可能为 nil，实现方自行拥有该情形下的判定责任（默认逻辑对 nil 的既有行为由公开库
// 回退分支保留）。本接口只返回 bool，不返回、也不抛出独立的 error。
type ResponseSemantics interface {
	// IsUpstreamError 判定 resp 对 m 而言是否应进入共享的上游错误处理。
	// resp 可能为 nil，由实现方决定该情形的判定；仅返回布尔值。
	IsUpstreamError(m *meta.Meta, resp *http.Response) bool
}
