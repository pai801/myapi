package common

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/buger/jsonparser"
)

// DeriveTurnIDFromBody 是纯函数：sessionID 必须非空，任何失败返回空串，绝不 panic、绝不随机、绝不记录敏感值。
//
// 实现用 jsonparser 直接扫描原始 body，不再 json.Unmarshal 到 map[string]any（后者对 465KB 量级的
// tool-loop payload 会产生约 900µs / 900KB 分配）。刻意不在函数内做 json.Valid 预检，预检开销由
// 调用方按需承担（middleware 已强制 json.Valid，见 readAffinityBodyPayload）。
//
// 降级边界（勿过度概括）：ArrayEach 仅在 messages 缺失 / 非数组 / 数组本身未闭合时报错降级为空串。
// 若 messages 数组完整闭合而 body 其余部分畸形，本函数**仍会派生**——这是与旧实现（整体
// json.Unmarshal 失败即空串）的已知差异，例如 `{"messages":[{"role":"user","content":"hi"}],"bad":[`
// 旧实现返回空串、本函数返回派生值。生产路径依赖调用方 json.Valid 预检拦截此类输入
// （middleware/affinity_scope.go 的 readAffinityBodyPayload），故实际风险受控；未来新增调用方
// 必须先做 json.Valid 预检，否则会从畸形 body 派生。
//
// 与 encoding/json 的已知差异：重复键取第一个（encoding/json 取最后一个）。本项目可接受。
func DeriveTurnIDFromBody(body []byte, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	// jsonparser 的 ArrayEach 只支持正向遍历，故先正向收集每个元素的下标与类型元数据（仅持有 body
	// 的子切片引用，不拷贝字节），再反向扫描最后一条真实 user 消息。收集全部元素（含非对象）以保证
	// 下标与 messages 数组内 0-based 下标一致。
	type messageElem struct {
		raw []byte
		vt  jsonparser.ValueType
	}
	var elems []messageElem
	_, err := jsonparser.ArrayEach(body, func(value []byte, dataType jsonparser.ValueType, _ int, _ error) {
		elems = append(elems, messageElem{raw: value, vt: dataType})
	}, "messages")
	if err != nil {
		// 键缺失、非数组、空 body 或畸形 JSON（含 messages 数组未闭合）一律降级为空串。
		return ""
	}
	// 从末尾向前扫描最后一条真实 user 消息。跳过与回溯语义（两条降级路径刻意不对称）：
	//
	// 跳过（继续回溯）：非真实 user 消息 —— 除 role 非 "user"、消息非对象外，还包括 content 为
	// null / 数字 / 对象（default 分支）与空数组 []（无任何非 tool_result block），四种形态一律
	// 判为非真实 user；跳过即回溯到更早的真实 user。
	//
	// 边界后果：末条 user 消息形态畸形时会锚到上一轮的真实 user，派生值复用上一轮的键，跨轮轮换
	// 在此失效。此为接受的取舍，两种路径均绝不 panic。
	//
	// 不回溯（立即 return 空串）：真实 user 消息但无文本（如仅含 image block、text 字段缺失或
	// 非字符串）。它是本轮真实的用户输入，只是无法提取指纹；回溯会把本轮错锚到上一轮，故宁可整体
	// 降级为空串，交由上层落 session 层。
	for i := len(elems) - 1; i >= 0; i-- {
		elem := elems[i]
		if elem.vt != jsonparser.Object {
			continue
		}
		if role, roleErr := jsonparser.GetString(elem.raw, "role"); roleErr != nil || role != "user" {
			continue
		}
		text, real, hasText := realUserMessageText(elem.raw)
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

// realUserMessageText 判定消息（调用方已确认其为 JSON 对象且 role == "user"）是否为真实 user 消息，
// 并返回其文本指纹与是否提取到文本。
// 真实 user 消息指 content 为字符串，或 content 数组至少含一个非 tool_result block；
// 纯 tool_result 的 user 消息是 Anthropic 工具结果载体，必须跳过，否则工具循环每步换键。
// 字符串 content 的原值即为文本（空串也算提取成功）；数组按序拼接 type == "text" 且 text 为字符串的 block。
// 真实 user 消息但无法提取文本（如纯 image block、text 字段缺失或非字符串）时 hasText 为 false，派生必须失败降级。
func realUserMessageText(message []byte) (text string, real bool, hasText bool) {
	content, dataType, _, err := jsonparser.Get(message, "content")
	if err != nil {
		// content 键缺失：不是真实 user 消息，跳过。
		return "", false, false
	}
	switch dataType {
	case jsonparser.String:
		decoded, decodeErr := jsonparser.GetString(message, "content")
		if decodeErr != nil {
			// 字符串解码失败理论上不可达（调用方已 json.Valid 预检）；不冒险继续派生。
			return "", false, false
		}
		return decoded, true, true
	case jsonparser.Array:
		var builder strings.Builder
		hasNonToolResult := false
		hasText = false
		// 只有对象 block 参与判定：非对象 block 与旧实现一致地被整体跳过（不计入 hasNonToolResult）。
		_, eachErr := jsonparser.ArrayEach(content, func(block []byte, blockType jsonparser.ValueType, _ int, _ error) {
			if blockType != jsonparser.Object {
				return
			}
			bt, _ := jsonparser.GetString(block, "type")
			if bt != "tool_result" {
				hasNonToolResult = true
			}
			if bt == "text" {
				if blockText, textErr := jsonparser.GetString(block, "text"); textErr == nil {
					hasText = true
					builder.WriteString(blockText)
				}
			}
		})
		if eachErr != nil {
			return "", false, false
		}
		if !hasNonToolResult {
			return "", false, false
		}
		return builder.String(), true, hasText
	default:
		// null / 数字 / 对象 content 一律判为非真实 user 消息，跳过。
		return "", false, false
	}
}
