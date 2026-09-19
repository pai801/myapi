// Package chandesc 承载「渠道能力清单」（ChannelDescriptor）的声明与注册（PRD §5.12 层1）。
//
// 与 relay/chanregistry、relay/routeregistry 同属「能力注册表」家族，独立成包的理由相同：
// 让 adaptor 接口包保持纯净，且本包可被主仓 controller 与扩展方同时引用。
//
// 设计目标（PRD §5.12 层1 / 决策 D7）：把「渠道元数据」从散落的五处（channeltype 枚举、
// url.go 表项、adaptor GetModelList、前端常量、locales）收敛为「一次注册、多处消费」的单一
// 真源。前端不再硬编码渠道类型，改为消费后端下发的清单（描述符驱动 + 通用渲染器）。
//
// 主仓不注册任何渠道 → 注册表为空；扩展渠道的 descriptor 由扩展方在 init() 中注入。
// 本包不 import 任何具体渠道实现，也不 import gin。
package chandesc

// PanelType 是「前端面板渲染器」类型（PRD §5.12 层2）。
//
// 前端按此值选择通用渲染器；未实现（或未知）的类型一律回退到 PanelManual
// ——「密钥框必须保持可手写」是降级路径（PRD §5.12 层2 / 方案 A）。
type PanelType string

const (
	// PanelManual：手写密钥框（方案 A 降级路径）。阶段 1 唯一实现的渲染器。
	PanelManual PanelType = "manual"
	// PanelOAuthDeviceCode：设备码轮询（扩展渠道的常见 OAuth 形态）。阶段 2 由通用渲染器消费。
	PanelOAuthDeviceCode PanelType = "oauth-device-code"
	// PanelOAuthAuthorizationCode：授权码回调（扩展渠道的常见 OAuth 形态）。阶段 2 由通用渲染器消费。
	PanelOAuthAuthorizationCode PanelType = "oauth-authorization-code"
)

// Capabilities 是渠道能力位（PRD §5.12 层1「能力位」）。
type Capabilities struct {
	// SupportsBalance 该渠道是否支持查询余额。
	SupportsBalance bool `json:"supports_balance"`
	// SupportsModelList 该渠道是否支持拉取/展示模型清单。
	SupportsModelList bool `json:"supports_model_list"`
	// SupportsTest 该渠道是否支持连通性测试。
	SupportsTest bool `json:"supports_test"`
	// SupportsCustomHeaders 该渠道是否支持自定义出站请求头。
	// 前端据此渲染「通用请求头编辑器」，从而不在前端写渠道专属分支。
	SupportsCustomHeaders bool `json:"supports_custom_headers"`
	// CustomHeadersKey 是「自定义出站请求头」在渠道 config JSON 中的键名。
	// 仅当 SupportsCustomHeaders 为 true 时有效。前端据此读写该配置项，
	// 从而不在前端硬编码任何渠道专属键名 —— 键名由扩展方自行选择。
	CustomHeadersKey string `json:"custom_headers_key,omitempty"`
}

// OAuthMeta 是 oauth-* 渲染器预留的元数据（阶段 2 消费；阶段 1 只做字段占位）。
type OAuthMeta struct {
	// StartPath / PollPath 是登录端点（相对 /api 前缀，如 /channel/{ext}/login/start）。
	StartPath string `json:"start_path,omitempty"`
	PollPath  string `json:"poll_path,omitempty"`
	// Params 是渲染器在发起 start 请求时透传的静态参数（如渠道自有的 realm=cn/global）。
	Params map[string]string `json:"params,omitempty"`
}

// PanelMeta 是「前端面板」的元数据（PRD §5.12 层1「前端面板」）。
type PanelMeta struct {
	// KeyPrompt / KeyPromptI18n：手写密钥框的提示文案（manual 渲染器消费）。
	KeyPrompt     string            `json:"key_prompt,omitempty"`
	KeyPromptI18n map[string]string `json:"key_prompt_i18n,omitempty"`
	// Description / DescriptionI18n：面板说明文案（各渲染器通用）。
	Description     string            `json:"description,omitempty"`
	DescriptionI18n map[string]string `json:"description_i18n,omitempty"`
	// OAuth：oauth-* 渲染器元数据（阶段 2）。
	OAuth *OAuthMeta `json:"oauth,omitempty"`
}

// Descriptor 是单个渠道的能力清单（PRD §5.12 层1）。
//
// 字段可空者一律带 omitempty：无注册时清单为空，有注册时按需下发。
type Descriptor struct {
	// ID 是注册身份（稳定字符串键；重复注册 panic）。
	ID string `json:"id"`
	// ChannelType 是渠道类型数值（对应 channeltype 枚举；清单按此索引）。
	ChannelType int `json:"channel_type"`
	// Name 是渠道展示名（NameI18n 未命中时的回退）。
	Name string `json:"name"`
	// NameI18n 是本地化展示名（语言码 -> 文案）。
	NameI18n map[string]string `json:"name_i18n,omitempty"`
	// Color 是前端标签色（semantic-ui 色名，可空）。
	Color string `json:"color,omitempty"`
	// DefaultBaseURL 是该渠道的默认 BaseURL（可空）。
	DefaultBaseURL string `json:"default_base_url,omitempty"`
	// AuthMode 是鉴权方式（如 api-key / oauth-device-code / oauth-authorization-code）。
	AuthMode string `json:"auth_mode,omitempty"`
	// ModelListSource 是模型清单来源（如 static / upstream）。
	ModelListSource string `json:"model_list_source,omitempty"`
	// Capabilities 是能力位。
	Capabilities Capabilities `json:"capabilities"`
	// PanelType 是前端面板渲染器类型。
	PanelType PanelType `json:"panel_type"`
	// Panel 是面板元数据。
	Panel PanelMeta `json:"panel"`
}

// Clone 返回 Descriptor 的深拷贝。
//
// 嵌套的 map（NameI18n / Panel.KeyPromptI18n / Panel.DescriptionI18n / Panel.OAuth.Params）
// 与 OAuth 指针都会被复制：注册表返回给调用方的是副本，调用方改写嵌套 map 不会污染注册表
// 内部状态（浅拷贝只复制了 map header，底层数组仍是共享的）。
func (d Descriptor) Clone() Descriptor {
	c := d
	c.NameI18n = cloneStringMap(d.NameI18n)
	c.Panel.KeyPromptI18n = cloneStringMap(d.Panel.KeyPromptI18n)
	c.Panel.DescriptionI18n = cloneStringMap(d.Panel.DescriptionI18n)
	if d.Panel.OAuth != nil {
		oauth := *d.Panel.OAuth
		oauth.Params = cloneStringMap(d.Panel.OAuth.Params)
		c.Panel.OAuth = &oauth
	}
	return c
}

// cloneStringMap 复制一个 string->string 映射；nil 原样返回 nil（保持 omitempty 语义）。
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
