package codex

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

// thinkTagState 状态机阶段。
type thinkTagState int

const (
	thinkStateNone   thinkTagState = iota // 等待开头的 <think>
	thinkStateInside                      // 在 <think>...</think> 内
	thinkStateDone                        // 已经匹配过一次，剩余都是正文
)

// thinkTagStateMachine 字符级状态机，识别跨 chunk 边界的 <think>...</think>。
//
// 仅允许在响应起始位置触发 <think>（CanStart）：一旦在非起始位置出现 "<think>" 或
// 其前缀，立即关闭检测窗口，剩余流量按普通文本透传，避免误判正文中的 "<think>"。
//
// 流结束时若 ThinkTagBuf 仍有内容（如 "<thi" 或 "</thi"），由调用方通过 Drain 取走兜底。
type thinkTagStateMachine struct {
	State     thinkTagState
	Buf       strings.Builder // 缓存跨 chunk 边界的不完整标签片段
	CanStart  bool            // 仅在响应起始位置允许触发 <think>
	LeadingWS strings.Builder // 缓存开头的空白字符，如果最终匹配到 <think> 则丢弃，否则作为正文输出
}

// Reset 把状态机恢复到初始状态。FirstChunk 路径应调用。
func (m *thinkTagStateMachine) Reset() {
	m.State = thinkStateNone
	m.Buf.Reset()
	m.CanStart = true
	m.LeadingWS.Reset()
}

// Feed 接收新 chunk，返回应分别送往 reasoning / content 通道的字符串切片。
// 状态机保留尾部可能的标签前缀（如 "<thi"）以便和下个 chunk 续接。
func (m *thinkTagStateMachine) Feed(chunk string) (reasoningParts, contentParts []string) {
	if chunk == "" {
		return nil, nil
	}
	pending := m.Buf.String() + chunk
	m.Buf.Reset()

	for len(pending) > 0 {
		switch m.State {
		case thinkStateNone: // 等待 <think>
			if !m.CanStart {
				contentParts = append(contentParts, pending)
				pending = ""
				continue
			}

			// 1. 提取前导空白字符
			var ws strings.Builder
			i := 0
			for i < len(pending) {
				c := pending[i]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
					ws.WriteByte(c)
					i++
				} else {
					break
				}
			}
			if ws.Len() > 0 {
				m.LeadingWS.WriteString(ws.String())
				pending = pending[i:]
				if len(pending) == 0 {
					// 当前 chunk 全是空白，继续等待
					return nil, nil
				}
			}

			// 2. 检查 <think> 标签
			idx := strings.Index(pending, thinkOpenTag)
			if idx >= 0 {
				// idx > 0 说明 <think> 不在最开头（在空白字符之后还有其他非空白字符，然后才是 <think>）
				// 此时关闭检测窗口，把之前缓存的空白、当前 pending 里的内容全部作为正文
				if idx > 0 {
					if m.LeadingWS.Len() > 0 {
						contentParts = append(contentParts, m.LeadingWS.String())
						m.LeadingWS.Reset()
					}
					contentParts = append(contentParts, pending)
					pending = ""
					m.CanStart = false
					continue
				}
				// 匹配成功！丢弃前导空白，进入 thinkStateInside
				m.LeadingWS.Reset()
				m.State = thinkStateInside
				pending = pending[len(thinkOpenTag):]
				continue
			}

			// 未找到完整 <think>：若 pending 整体是 "<think>" 的非空前缀，保留到下个 chunk
			if isStrictPrefix(thinkOpenTag, pending) {
				m.Buf.WriteString(pending)
				return reasoningParts, contentParts
			}

			// 否则关闭检测窗口，把缓存的前导空白和当前 pending 全部作为正文
			if m.LeadingWS.Len() > 0 {
				contentParts = append(contentParts, m.LeadingWS.String())
				m.LeadingWS.Reset()
			}
			contentParts = append(contentParts, pending)
			pending = ""
			m.CanStart = false
		case thinkStateInside: // 等待 </think>
			idx := strings.Index(pending, thinkCloseTag)
			if idx >= 0 {
				if idx > 0 {
					reasoningParts = append(reasoningParts, pending[:idx])
				}
				pending = pending[idx+len(thinkCloseTag):]
				m.State = thinkStateDone
				continue
			}
			// 未找到完整 </think>：把末尾可能是 "</think>" 前缀的部分缓存
			keep := suffixThatCouldBePrefix(pending, thinkCloseTag)
			if keep > 0 {
				if len(pending) > keep {
					reasoningParts = append(reasoningParts, pending[:len(pending)-keep])
				}
				m.Buf.WriteString(pending[len(pending)-keep:])
				return reasoningParts, contentParts
			}
			reasoningParts = append(reasoningParts, pending)
			pending = ""
		case thinkStateDone: // 之后都是正文
			contentParts = append(contentParts, pending)
			pending = ""
		}
	}
	return reasoningParts, contentParts
}

// Drain 在流结束时取走状态机尾部缓冲。
// 返回 (剩余文本, 是否应进入 reasoning 通道)。无残留时返回 ("", false)。
func (m *thinkTagStateMachine) Drain() (remaining string, toReasoning bool) {
	if m.State == thinkStateNone {
		// 如果还在等待状态，需要把缓存的前导空白和 Buf 里的内容合并返回
		var sb strings.Builder
		if m.LeadingWS.Len() > 0 {
			sb.WriteString(m.LeadingWS.String())
			m.LeadingWS.Reset()
		}
		if m.Buf.Len() > 0 {
			sb.WriteString(m.Buf.String())
			m.Buf.Reset()
		}
		return sb.String(), false
	}

	if m.Buf.Len() == 0 {
		return "", false
	}
	remaining = m.Buf.String()
	m.Buf.Reset()
	// Inside：未闭合的 <think>... → reasoning；其他状态 → content
	toReasoning = m.State == thinkStateInside
	return remaining, toReasoning
}

// isStrictPrefix 报告 s 是否为 full 的非空严格前缀（s != full 且 full[:len(s)] == s）。
func isStrictPrefix(full, s string) bool {
	if s == "" || len(s) >= len(full) {
		return false
	}
	return full[:len(s)] == s
}

// suffixThatCouldBePrefix 返回 s 末尾最长的、可能成为 tag 严格前缀的长度。
// 例如 suffixThatCouldBePrefix("abc</thi", "</think>") == 5（"</thi"）。
func suffixThatCouldBePrefix(s, tag string) int {
	maxLen := len(s)
	if maxLen >= len(tag) {
		maxLen = len(tag) - 1
	}
	for k := maxLen; k > 0; k-- {
		if s[len(s)-k:] == tag[:k] {
			return k
		}
	}
	return 0
}

func getStringValue(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getIntValue(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func getObjectValue(m map[string]interface{}, key string) interface{} {
	if v, ok := m[key]; ok {
		return v
	}
	return nil
}

// normalizeParameters 规范化工具参数，确保有完整的 JSONSchema 结构
func normalizeParameters(params interface{}) map[string]interface{} {
	if params == nil {
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
			"required":   []interface{}{},
		}
	}

	if p, ok := params.(map[string]interface{}); ok {
		result := make(map[string]interface{})
		for k, v := range p {
			result[k] = v
		}
		if _, has := result["type"]; !has {
			result["type"] = "object"
		}
		if _, has := result["properties"]; !has {
			result["properties"] = map[string]interface{}{}
		}
		if _, has := result["required"]; !has {
			result["required"] = []interface{}{}
		}
		return result
	}

	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
		"required":   []interface{}{},
	}
}

// extractThinkTag 从完整文本中提取开头位置的 <think>...</think>。
// 返回 (剩余文本, 思考内容, 是否检测到 think)。
// 仅在文本开头匹配，避免误判正文中的 "<think>"。未闭合的 <think> 视为全部为思考内容。
func extractThinkTag(content string) (text string, thinking string, hasThink bool) {
	trimmed := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(trimmed, thinkOpenTag) {
		return content, "", false
	}
	inner := trimmed[len(thinkOpenTag):]
	closeIdx := strings.Index(inner, thinkCloseTag)
	if closeIdx < 0 {
		return "", inner, true
	}
	thinking = inner[:closeIdx]
	remaining := inner[closeIdx+len(thinkCloseTag):]
	remaining = strings.TrimLeft(remaining, " \t\r\n")
	return remaining, thinking, true
}

// extractThinkingFromContent 从 content 中提取思考内容（支持字符串和数组两种格式）
// 返回 (思考内容, 剩余内容)
func extractThinkingFromContent(content interface{}) (string, interface{}) {
	if content == nil {
		return "", nil
	}

	switch v := content.(type) {
	case string:
		remaining, thinking, hasThink := extractThinkTag(v)
		if hasThink {
			return thinking, remaining
		}
		return "", v
	case []interface{}:
		var thinkingParts []string
		var remainingBlocks []interface{}

		for _, block := range v {
			if blockMap, ok := block.(map[string]interface{}); ok {
				blockType, _ := blockMap["type"].(string)
				if blockType == "text" || blockType == "output_text" || blockType == "input_text" {
					if text, ok := blockMap["text"].(string); ok {
						remaining, thinking, hasThink := extractThinkTag(text)
						if hasThink {
							thinkingParts = append(thinkingParts, thinking)
							if remaining != "" {
								remainingBlocks = append(remainingBlocks, map[string]interface{}{
									"type": blockType,
									"text": remaining,
								})
							}
						} else {
							remainingBlocks = append(remainingBlocks, block)
						}
					} else {
						remainingBlocks = append(remainingBlocks, block)
					}
				} else {
					remainingBlocks = append(remainingBlocks, block)
				}
			} else {
				remainingBlocks = append(remainingBlocks, block)
			}
		}

		if len(thinkingParts) > 0 {
			return strings.Join(thinkingParts, "\n"), remainingBlocks
		}
		if len(remainingBlocks) > 0 {
			return "", remainingBlocks
		}
		return "", content
	}

	return "", content
}

// CodexCustomToolKind classifies the type of Codex custom tool.
type CodexCustomToolKind string

const (
	CodexCustomToolRaw        CodexCustomToolKind = "raw"
	CodexCustomToolApplyPatch CodexCustomToolKind = "apply_patch"
	CodexCustomToolExec       CodexCustomToolKind = "exec"
	CodexCustomToolBuiltIn    CodexCustomToolKind = "builtin"
)

// CodexCustomToolSpec describes a single Codex custom tool and its upstream proxy.
type CodexCustomToolSpec struct {
	OpenAIName        string
	GrammarDefinition string
	Kind              CodexCustomToolKind
	ProxyAction       string // "", add_file, delete_file, update_file, replace_file, batch
}

// CodexFunctionToolSpec describes a normal function tool for namespace tracking.
type CodexFunctionToolSpec struct {
	Namespace string
	Name      string
}

// CodexToolContext holds parsed information about all tools in a request.
type CodexToolContext struct {
	CustomTools       map[string]CodexCustomToolSpec
	FunctionTools     map[string]CodexFunctionToolSpec
	HasCustomTools    bool
	HasNamespaceTools bool
}

// BuildCodexToolContextFromRequest builds Codex tool context from Responses request JSON.
func buildCodexToolContextFromRequest(requestRawJSON []byte) CodexToolContext {
	if len(requestRawJSON) == 0 {
		return CodexToolContext{
			CustomTools:   make(map[string]CodexCustomToolSpec),
			FunctionTools: make(map[string]CodexFunctionToolSpec),
		}
	}

	req := gjson.ParseBytes(requestRawJSON)
	tools := req.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return CodexToolContext{
			CustomTools:   make(map[string]CodexCustomToolSpec),
			FunctionTools: make(map[string]CodexFunctionToolSpec),
		}
	}

	var rawTools []interface{}
	for _, t := range tools.Array() {
		rawTools = append(rawTools, t.Value())
	}

	return BuildCodexToolContextFromRaw(rawTools)
}

// BuildCodexToolContextFromRaw builds Codex tool context from raw tools slice.
func BuildCodexToolContextFromRaw(tools []interface{}) CodexToolContext {
	ctx := CodexToolContext{
		CustomTools:   make(map[string]CodexCustomToolSpec),
		FunctionTools: make(map[string]CodexFunctionToolSpec),
	}

	for _, rawTool := range tools {
		if name, ok := rawTool.(string); ok && name != "" {
			if action := proxyActionFromUpstreamName(name); strings.HasPrefix(name, "apply_patch_") && action != "" {
				ctx.CustomTools[name] = CodexCustomToolSpec{OpenAIName: "apply_patch", Kind: CodexCustomToolApplyPatch, ProxyAction: action}
			} else {
				ctx.CustomTools[name] = CodexCustomToolSpec{OpenAIName: name, Kind: CodexCustomToolRaw}
			}
			ctx.HasCustomTools = true
			continue
		}
		tool, ok := rawTool.(map[string]interface{})
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		switch toolType {
		case "custom":
			name, _ := tool["name"].(string)
			if name == "" {
				continue
			}
			kind, grammarDef := detectCodexCustomToolKind(tool)
			spec := CodexCustomToolSpec{
				OpenAIName:        name,
				GrammarDefinition: grammarDef,
				Kind:              kind,
			}
			switch kind {
			case CodexCustomToolApplyPatch:
				ctx.CustomTools[name] = spec
				for _, suffix := range []string{"_add_file", "_delete_file", "_update_file", "_replace_file", "_batch"} {
					proxySpec := spec
					proxySpec.ProxyAction = strings.TrimPrefix(suffix, "_")
					ctx.CustomTools[name+suffix] = proxySpec
				}
			default:
				ctx.CustomTools[name] = spec
			}
			ctx.HasCustomTools = true
		case "function":
			name, _ := tool["name"].(string)
			if name == "" {
				continue
			}
			ctx.FunctionTools[name] = CodexFunctionToolSpec{Name: name}
		case "namespace":
			addNamespaceToolsToContext(&ctx, tool)
		case "web_search", "local_shell", "computer_use":
			name, _ := tool["name"].(string)
			if name == "" {
				name = toolType
			}
			ctx.CustomTools[name] = CodexCustomToolSpec{OpenAIName: name, Kind: CodexCustomToolBuiltIn}
			ctx.HasCustomTools = true
		}
	}

	return ctx
}

// flattenNamespaceToolName returns the flat function name for a namespace tool child.
// Expects namespace names to end with "__" per Codex convention.
func flattenNamespaceToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	if name == "" {
		return namespace
	}
	if strings.HasSuffix(namespace, "__") || strings.HasPrefix(name, "__") {
		return namespace + name
	}
	return namespace + "__" + name
}

func addNamespaceToolsToContext(ctx *CodexToolContext, namespaceTool map[string]interface{}) {
	namespace, _ := namespaceTool["name"].(string)
	children, _ := namespaceTool["tools"].([]interface{})
	for _, raw := range children {
		child, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		childType, _ := child["type"].(string)
		switch childType {
		case "function":
			name, _ := child["name"].(string)
			if name == "" {
				continue
			}
			flat := flattenNamespaceToolName(namespace, name)
			if spec, exists := ctx.FunctionTools[flat]; exists && spec.Namespace == "" {
				continue
			}
			ctx.FunctionTools[flat] = CodexFunctionToolSpec{
				Namespace: namespace,
				Name:      name,
			}
			ctx.HasNamespaceTools = true
		}
	}
}

// OpenAINameForFunctionTool returns the unflattened (name, namespace) for an upstream flat function name.
func (ctx CodexToolContext) OpenAINameForFunctionTool(upstreamName string) (name string, namespace string) {
	spec, ok := ctx.FunctionTools[upstreamName]
	if !ok {
		return upstreamName, ""
	}
	if spec.Name == "" {
		return upstreamName, spec.Namespace
	}
	return spec.Name, spec.Namespace
}

func detectCodexCustomToolKind(tool map[string]interface{}) (CodexCustomToolKind, string) {
	name, _ := tool["name"].(string)
	format, _ := tool["format"].(map[string]interface{})
	grammarDef := ""
	if format != nil {
		grammarDef, _ = format["definition"].(string)
	}
	if name == "apply_patch" {
		return CodexCustomToolApplyPatch, grammarDef
	}
	if grammarDef != "" {
		if strings.Contains(grammarDef, "begin_patch") &&
			strings.Contains(grammarDef, "end_patch") &&
			strings.Contains(grammarDef, "add_hunk") {
			return CodexCustomToolApplyPatch, grammarDef
		}
	}
	if name == "exec" {
		return CodexCustomToolExec, grammarDef
	}
	return CodexCustomToolRaw, grammarDef
}

// IsCustomToolProxy returns whether the given upstream name is a Codex custom tool proxy.
func (ctx CodexToolContext) IsCustomToolProxy(upstreamName string) bool {
	_, ok := ctx.CustomTools[upstreamName]
	return ok
}

// OriginalCustomToolName returns the original Codex tool name for a proxy name.
func (ctx CodexToolContext) OriginalCustomToolName(upstreamName string) string {
	if spec, ok := ctx.CustomTools[upstreamName]; ok {
		return spec.OpenAIName
	}
	return upstreamName
}

func proxyActionFromUpstreamName(name string) string {
	switch {
	case strings.HasSuffix(name, "_add_file"):
		return "add_file"
	case strings.HasSuffix(name, "_delete_file"):
		return "delete_file"
	case strings.HasSuffix(name, "_update_file"):
		return "update_file"
	case strings.HasSuffix(name, "_replace_file"):
		return "replace_file"
	case strings.HasSuffix(name, "_batch"):
		return "batch"
	default:
		return ""
	}
}

// ReconstructCustomToolCallInput reconstructs raw custom tool input from proxy function arguments.
func reconstructCustomToolCallInput(ctx CodexToolContext, upstreamName, rawArguments string) string {
	spec, ok := ctx.CustomTools[upstreamName]
	if !ok {
		return rawArguments
	}

	switch spec.Kind {
	case CodexCustomToolApplyPatch:
		action := spec.ProxyAction
		if action == "" {
			action = proxyActionFromUpstreamName(upstreamName)
		}
		return ApplyPatchInputFromProxyArguments(rawArguments, action)
	default:
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(rawArguments), &parsed); err != nil {
			return rawArguments
		}
		if input, ok := parsed["input"].(string); ok {
			return input
		}
		return rawArguments
	}
}

// ApplyPatchInputFromProxyArguments reconstructs apply_patch grammar input from proxy arguments.
func ApplyPatchInputFromProxyArguments(rawArguments string, action string) string {
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rawArguments), &parsed); err != nil {
		return rawArguments
	}
	return applyPatchInputFromParsedArgs(parsed, action, rawArguments)
}

func applyPatchInputFromParsedArgs(args map[string]interface{}, action, rawArguments string) string {
	if input, ok := args["input"].(string); ok && action != "" {
		var nested map[string]interface{}
		if err := json.Unmarshal([]byte(input), &nested); err == nil {
			for key, value := range nested {
				if _, exists := args[key]; !exists {
					args[key] = value
				}
			}
		}
	}

	var ops []ApplyPatchOperation

	switch action {
	case "add_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		ops = append(ops, ApplyPatchOperation{Type: "add_file", Path: path, Content: content})
	case "delete_file":
		path, _ := args["path"].(string)
		ops = append(ops, ApplyPatchOperation{Type: "delete_file", Path: path})
	case "update_file":
		path, _ := args["path"].(string)
		moveTo, _ := args["move_to"].(string)
		ops = append(ops, ApplyPatchOperation{Type: "update_file", Path: path, MoveTo: moveTo, Hunks: parseHunksFromRaw(args["hunks"])})
	case "replace_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		ops = append(ops, ApplyPatchOperation{Type: "replace_file", Path: path, Content: content})
	case "batch":
		if rawOps, _ := args["operations"].([]interface{}); rawOps != nil {
			for _, rawOp := range rawOps {
				opMap, ok := rawOp.(map[string]interface{})
				if !ok {
					continue
				}
				opType, _ := opMap["type"].(string)
				path, _ := opMap["path"].(string)
				ops = append(ops, ApplyPatchOperation{
					Type:    opType,
					Path:    path,
					MoveTo:  mapString(opMap, "move_to"),
					Content: mapString(opMap, "content"),
					Hunks:   parseHunksFromRaw(opMap["hunks"]),
				})
			}
		}
	default:
		if input, ok := args["input"].(string); ok {
			return input
		}
		return rawArguments
	}

	if len(ops) == 0 {
		return rawArguments
	}
	return BuildApplyPatchInput(ops)
}

func mapString(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func parseHunksFromRaw(raw interface{}) []ApplyPatchHunk {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	hunks := make([]ApplyPatchHunk, 0, len(arr))
	for _, rawHunk := range arr {
		hunkMap, ok := rawHunk.(map[string]interface{})
		if !ok {
			continue
		}
		hunks = append(hunks, ApplyPatchHunk{
			Context: mapString(hunkMap, "context"),
			Lines:   parseLineOpsFromRaw(hunkMap["lines"]),
		})
	}
	return hunks
}

func parseLineOpsFromRaw(raw interface{}) []ApplyPatchLineOp {
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	lines := make([]ApplyPatchLineOp, 0, len(arr))
	for _, rawLine := range arr {
		lineMap, ok := rawLine.(map[string]interface{})
		if !ok {
			continue
		}
		lines = append(lines, ApplyPatchLineOp{
			Op:   mapString(lineMap, "op"),
			Text: mapString(lineMap, "text"),
		})
	}
	return lines
}

// ApplyPatchOperation represents a single patch operation.
type ApplyPatchOperation struct {
	Type    string
	Path    string
	MoveTo  string
	Content string
	Hunks   []ApplyPatchHunk
}

// ApplyPatchHunk represents a patch hunk.
type ApplyPatchHunk struct {
	Context string
	Lines   []ApplyPatchLineOp
}

// ApplyPatchLineOp represents a patch line operation.
type ApplyPatchLineOp struct {
	Op   string
	Text string
}

// BuildApplyPatchInput builds raw apply_patch grammar input from operations.
func BuildApplyPatchInput(ops []ApplyPatchOperation) string {
	var sb strings.Builder
	sb.WriteString("*** Begin Patch\n")
	for _, op := range ops {
		switch op.Type {
		case "add_file":
			sb.WriteString("*** Add File: ")
			sb.WriteString(op.Path)
			sb.WriteString("\n")
			writeApplyPatchAddedContent(&sb, op.Content)
		case "delete_file":
			sb.WriteString("*** Delete File: ")
			sb.WriteString(op.Path)
			sb.WriteString("\n")
		case "update_file":
			sb.WriteString("*** Update File: ")
			sb.WriteString(op.Path)
			sb.WriteString("\n")
			if op.MoveTo != "" {
				sb.WriteString("*** Move to: ")
				sb.WriteString(op.MoveTo)
				sb.WriteString("\n")
			}
			for _, hunk := range op.Hunks {
				if hunk.Context != "" {
					sb.WriteString("@@ ")
					sb.WriteString(hunk.Context)
					sb.WriteString("\n")
				} else {
					sb.WriteString("@@\n")
				}
				for _, line := range hunk.Lines {
					sb.WriteString(lineOpPrefix(line.Op))
					sb.WriteString(line.Text)
					sb.WriteString("\n")
				}
			}
		case "replace_file":
			sb.WriteString("*** Delete File: ")
			sb.WriteString(op.Path)
			sb.WriteString("\n")
			sb.WriteString("*** Add File: ")
			sb.WriteString(op.Path)
			sb.WriteString("\n")
			writeApplyPatchAddedContent(&sb, op.Content)
		}
	}
	sb.WriteString("*** End Patch")
	return sb.String()
}

func writeApplyPatchAddedContent(sb *strings.Builder, content string) {
	if content == "" {
		return
	}
	content = strings.TrimSuffix(content, "\n")
	for _, line := range strings.Split(content, "\n") {
		sb.WriteString("+")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
}

func lineOpPrefix(op string) string {
	switch op {
	case "context":
		return " "
	case "add":
		return "+"
	case "remove", "delete":
		return "-"
	default:
		return " "
	}
}
