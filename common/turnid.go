package common

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// DeriveTurnIDFromBody 是纯函数：sessionID 必须非空，任何失败返回空串，绝不 panic、绝不随机、绝不记录敏感值。
func DeriveTurnIDFromBody(body []byte, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return DeriveTurnIDFromPayload(payload, sessionID)
}

// DeriveTurnIDFromPayload 从已解析 JSON payload 确定性派生 turn id；payload 仅可读且仅在当前请求内使用，无有效真实 user 消息或无法提取其文本时返回空串。
func DeriveTurnIDFromPayload(payload map[string]any, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return ""
	}
	// 从末尾向前扫描最后一条真实 user 消息。跳过与回溯语义如下（两条降级路径刻意不对称）：
	//
	// 跳过（continue 并继续回溯）：非真实 user 消息 —— 除 role 非 "user"、消息非对象外，还包括
	// content 为 null / 数字 / 对象（三者走 realUserTextFingerprint 的 default 分支）与空数组 []
	// （走 []any 分支的 !hasNonToolResult 路径），四种形态一律判为非真实 user；跳过即回溯到更早的真实 user。
	//
	// 边界后果：末条 user 消息形态畸形时会锚到上一轮的真实 user，派生值复用上一轮的键，跨轮轮换在此失效。
	// 此为接受的取舍，但两种路径后果不同：旧派生键仍在亲和表内（stale 命中）时 Get 直接命中并复用上一轮的
	// 渠道、不回落粗层 —— 生产写入把 turn/session 键同写为同一渠道，故通常等价于「同 session 复用上一轮
	// 渠道」、符合亲和意图；但 session 键从未写入或曾指向不同渠道时，仍可能选到 session 层不会选的渠道
	// （跨轮串扰）。仅当旧键已过期/被淘汰（键失效）才回落 session/user 层，不劣于改造前行为。两种路径均绝不 panic。
	//
	// 不回溯（立即 return 空串）：真实 user 消息但无文本（如仅含 image block、text 字段缺失
	// 或非字符串）。它是本轮真实的用户输入，只是无法提取指纹；回溯会把本轮错锚到上一轮，
	// 故宁可整体降级为空串，交由上层落 session 层。
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		text, real, hasText := realUserTextFingerprint(message)
		if !real {
			continue
		}
		if !hasText {
			return ""
		}
		sum := sha256.Sum256([]byte(sessionID + "\x00" + strconv.Itoa(i) + "\x00" + text))
		return hex.EncodeToString(sum[:])[:32]
	}
	return ""
}

// realUserTextFingerprint 判定消息是否为真实 user 消息，并返回其文本指纹与是否提取到文本。
// 真实 user 消息指 content 为字符串，或 content 数组至少含一个非 tool_result block；
// 纯 tool_result 的 user 消息是 Anthropic 工具结果载体，必须跳过，否则工具循环每步换键。
// 字符串 content 的原值即为文本（空串也算提取成功）；数组按序拼接 type == "text" 且 text 为字符串的 block。
// 真实 user 消息但无法提取文本（如纯 image block、text 字段缺失或非字符串）时 hasText 为 false，派生必须失败降级。
func realUserTextFingerprint(message map[string]any) (text string, real bool, hasText bool) {
	content, exists := message["content"]
	if !exists {
		return "", false, false
	}
	switch v := content.(type) {
	case string:
		return v, true, true
	case []any:
		var builder strings.Builder
		hasNonToolResult := false
		hasText = false
		for _, raw := range v {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType != "tool_result" {
				hasNonToolResult = true
			}
			if blockType == "text" {
				if blockText, ok := block["text"].(string); ok {
					hasText = true
					builder.WriteString(blockText)
				}
			}
		}
		if !hasNonToolResult {
			return "", false, false
		}
		return builder.String(), true, hasText
	default:
		return "", false, false
	}
}
