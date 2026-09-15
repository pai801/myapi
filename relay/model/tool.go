package model

type Tool struct {
	Id       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"` // when splicing claude tools stream messages, it is empty
	Function Function `json:"function"`
}

type Function struct {
	Description string `json:"description,omitempty"`
	Name        string `json:"name,omitempty"`       // when splicing claude tools stream messages, it is empty
	Parameters  any    `json:"parameters,omitempty"` // request
	Arguments   any    `json:"arguments,omitempty"`  // response
	// Strict 用指针区分"未提供"与显式 false（chat-completions-protocol.md §5.1），
	// 并在 Chat→Responses 工具扁平转换中原样保留。
	Strict *bool `json:"strict,omitempty"`
}
