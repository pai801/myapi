package model

import "fmt"

type Message struct {
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`
	// Refusal 对应 Chat §3.1 assistant 消息顶层 refusal 字段；指针区分"未提供"与显式空串。
	Refusal          *string `json:"refusal,omitempty"`
	ReasoningContent any     `json:"reasoning_content,omitempty"`
	Name             *string `json:"name,omitempty"`
	ToolCalls        []Tool  `json:"tool_calls,omitempty"`
	ToolCallId       string  `json:"tool_call_id,omitempty"`
}

func (m Message) IsStringContent() bool {
	_, ok := m.Content.(string)
	return ok
}

func (m Message) StringContent() string {
	content, ok := m.Content.(string)
	if ok {
		return content
	}
	contentList, ok := m.Content.([]any)
	if ok {
		var contentStr string
		for _, contentItem := range contentList {
			contentMap, ok := contentItem.(map[string]any)
			if !ok {
				continue
			}
			if contentMap["type"] == ContentTypeText {
				if subStr, ok := contentMap["text"].(string); ok {
					contentStr += subStr
				}
			}
		}
		return contentStr
	}
	return ""
}

// ParseContent 防御性安全解析 message content（chat-completions-protocol.md §4）。
// 字符串 content 视为单一 text part；数组 content 逐 part 做类型检查，任何形状违约
// （非对象元素、必填子字段缺失或类型错误、未知 part type）均返回 error 且不 panic，
// 失败路径不返回部分结果，调用方不得 swallow。
func (m Message) ParseContent() ([]MessageContent, error) {
	if content, ok := m.Content.(string); ok {
		return []MessageContent{{
			Type: ContentTypeText,
			Text: content,
		}}, nil
	}
	if m.Content == nil {
		return nil, nil
	}
	anyList, ok := m.Content.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be string or array, got %T (chat §4)", m.Content)
	}
	parts := make([]MessageContent, 0, len(anyList))
	for i, contentItem := range anyList {
		contentMap, ok := contentItem.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content[%d]: part must be a JSON object (chat §4)", i)
		}
		ctype, ok := contentMap["type"].(string)
		if !ok {
			return nil, fmt.Errorf("content[%d]: type is required string (chat §4)", i)
		}
		switch ctype {
		case ContentTypeText:
			text, ok := contentMap["text"].(string)
			if !ok {
				return nil, fmt.Errorf("content[%d]: text is required string (chat §4)", i)
			}
			parts = append(parts, MessageContent{Type: ctype, Text: text})
		case ContentTypeImageURL:
			iu, ok := contentMap["image_url"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content[%d]: image_url must be an object (chat §4)", i)
			}
			urlStr, ok := iu["url"].(string)
			if !ok {
				return nil, fmt.Errorf("content[%d]: image_url.url is required string (chat §4)", i)
			}
			detail, _ := iu["detail"].(string)
			parts = append(parts, MessageContent{
				Type:     ctype,
				ImageURL: &ImageURL{Url: urlStr, Detail: detail},
			})
		case ContentTypeInputAudio:
			ia, ok := contentMap["input_audio"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content[%d]: input_audio must be an object (chat §4)", i)
			}
			parts = append(parts, MessageContent{Type: ctype, InputAudio: ia})
		case ContentTypeFile:
			fm, ok := contentMap["file"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content[%d]: file must be an object (chat §4)", i)
			}
			parts = append(parts, MessageContent{Type: ctype, File: fm})
		case ContentTypeRefusal:
			r, ok := contentMap["refusal"].(string)
			if !ok {
				return nil, fmt.Errorf("content[%d]: refusal is required string (chat §4)", i)
			}
			parts = append(parts, MessageContent{Type: ctype, Refusal: r})
		default:
			return nil, fmt.Errorf("content[%d]: unknown part type %q (chat §4)", i, ctype)
		}
	}
	return parts, nil
}

type ImageURL struct {
	Url    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// MessageContent 覆盖 Chat §4 全部 5 种 content part；audio/file 以原始对象承载，
// 必填子字段的合法性校验由各转换方的前置校验负责（如 chatgptsub T2）。
type MessageContent struct {
	Type       string    `json:"type,omitempty"`
	Text       string    `json:"text,omitempty"`
	Refusal    string    `json:"refusal,omitempty"`
	ImageURL   *ImageURL `json:"image_url,omitempty"`
	InputAudio any       `json:"input_audio,omitempty"`
	File       any       `json:"file,omitempty"`
}
