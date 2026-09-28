// Package redact 提供凭据脱敏与有界 UTF-8 截断原语（IC-05）。
//
// 本包承载两类能力：
//   - 凭据脱敏与有界截断（Task 1.6）：RedactionPolicy / RedactCredentials / TruncateUTF8。
//
// 依赖方向：只依赖标准库，**不**依赖任何具体渠道包。渠道特有的凭据字段名表 / 授权前缀
// （如 buddy 的 secret/key/token/password/credential/authorization 与 `Bearer`）由渠道
// 调用点以 RedactionPolicy 注入；本包不硬编码任何渠道名与渠道政策。
package redact

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// RedactionPolicy 声明凭据字段名、授权前缀与有界输出大小。
type RedactionPolicy struct {
	// CredentialNames 是大小写不敏感的凭据字段名片段（如 secret / token / password）。
	// 命名字段名本身保留（可证明上游确实回了凭据），只把其值替换为 ***。
	CredentialNames []string
	// AuthPrefixes 是可注入的授权前缀（如 `Bearer`、`Cloud-IDE-JWT`）。前缀后紧跟的令牌样值
	// 会被整体替换为 ***。尾部空白会被忽略（`Bearer ` 与 `Bearer` 等价）。
	AuthPrefixes []string
	// MaxBytes 是输出字节上限；<=0 表示不截断。
	MaxBytes int
	// TruncatedMark 是发生截断时纳入 MaxBytes 之内的省略标记（应短于 MaxBytes）。
	TruncatedMark string
}

// tokenValuePattern 是授权前缀后令牌样值的字符集（长度 >=8）。
// 字符集含标准 base64 的 + / = 与 URL-safe 的 - _ .。
const tokenValuePattern = `[A-Za-z0-9._+/=\-]{8,}`

// bareTokenValuePattern 是「凭据字段名 + 空白 + 令牌样值」无分隔符形态的值字符集。
//
// 长度限定 >=16 的令牌字符集，避免把 `token expired`、`api key invalid` 等普通文案误抹：
// 值过短、含空格或标点即不匹配，从而不牺牲业务文案保真。
const bareTokenValuePattern = `[A-Za-z0-9._+/=\-]{16,}`

// credentialValueAlternation 是「字段名 + 分隔符(:或=)」之后值的形态集合：
//   - 闭合的双引号 / 单引号字符串（含转义）；
//   - **未闭合**的引号字符串——body 被截断或上游回传截断 JSON 时，形如 {"accessToken":"<secret>
//     的输入没有收尾引号，必须抹到行尾/串尾；
//   - 非 JSON 的裸值（可含空格），止于行尾或 , ; & 等分隔符。
const credentialValueAlternation = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|"[^"\n]*|'[^'\n]*|[^\r\n;,&"']+`

// regexCache 缓存按策略拼装的脱敏正则。策略声明是静态的（渠道调用点常量），键数量有界，
// 无需淘汰。正则为只读值，可安全并发共享。
var regexCache sync.Map

// compiledRegex 返回 pattern 对应的已编译正则（带进程内缓存）。
// pattern 由 regexp.QuoteMeta 处理过的静态片段拼装，故 MustCompile 不会 panic。
func compiledRegex(pattern string) *regexp.Regexp {
	if cached, ok := regexCache.Load(pattern); ok {
		return cached.(*regexp.Regexp)
	}
	compiled := regexp.MustCompile(pattern)
	actual, _ := regexCache.LoadOrStore(pattern, compiled)
	return actual.(*regexp.Regexp)
}

// credentialNamePattern 把字段名片段拼成「任意前缀 + 关键字 + 任意后缀」的名称模式。
//
// 用「前缀/后缀自由 + 关键字必须出现」而非固定枚举，是为了覆盖 accessToken / access_token /
// deviceToken / apiSecret / api_key / secretKey / clientSecret / sessionToken /
// accessTokenValue / X-Refresh-Token 等常见命名变体。字段名为空列表时返回空串。
func credentialNamePattern(names []string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		parts = append(parts, regexp.QuoteMeta(name))
	}
	if len(parts) == 0 {
		return ""
	}
	return `[a-z0-9_\-]*(?:` + strings.Join(parts, "|") + `)[a-z0-9_\-]*`
}

// RedactCredentials 抹除文本中的凭据值，同时保留安全的诊断字段名。
//
// 处理顺序与既有 buddy 实现逐字对齐，且**顺序敏感**：
//  1. **先抹授权前缀令牌**。字段名正则会把 `Authorization: Bearer <tok>` 中的 `Bearer`
//     当作字段值吞掉并替换为 ***，此后前缀正则再也匹配不到 `Bearer ` 前缀，令牌将裸奔
//     （实测 `Authorization: *** eyJ...`）。故必须先抹前缀令牌，再由字段名正则收尾。
//  2. 抹「凭据字段名 + 分隔符(:或=) + 值」形态（含未闭合引号、转义引号、裸值）。
//  3. 抹「凭据字段名 + 空白 + 令牌样值」的无分隔符形态。
//
// 最后当 policy.MaxBytes > 0 时按 policy.TruncatedMark 做有界截断（UTF-8 边界安全）。
//
// 这是**尽力而为的纵深防御**：覆盖已知命名与键值形态，但不保证穷尽所有凭据回显方式
// （例如 JSON 转义键名 `access\u0054oken` 不在覆盖范围），不作为「输出绝不含凭据」的充分证明。
func RedactCredentials(text string, policy RedactionPolicy) string {
	for _, prefix := range policy.AuthPrefixes {
		p := strings.TrimRight(prefix, " ")
		if p == "" {
			continue
		}
		pattern := `(?i)\b` + regexp.QuoteMeta(p) + `\s+` + tokenValuePattern
		text = compiledRegex(pattern).ReplaceAllString(text, p+" ***")
	}
	if namePattern := credentialNamePattern(policy.CredentialNames); namePattern != "" {
		fieldPattern := `(?i)("?` + namePattern + `"?\s*[:=]\s*)(?:` + credentialValueAlternation + `)`
		text = compiledRegex(fieldPattern).ReplaceAllString(text, "${1}***")
		barePattern := `(?i)\b(` + namePattern + `)\s+` + bareTokenValuePattern
		text = compiledRegex(barePattern).ReplaceAllString(text, "${1} ***")
	}
	if policy.MaxBytes > 0 {
		text = TruncateUTF8(text, policy.MaxBytes, policy.TruncatedMark)
	}
	return text
}

// TruncateUTF8 把 text 限制在 maxBytes 字节以内，且不切出半个 UTF-8 字符。
//
// maxBytes <= 0 表示不截断。发生截断时 mark 被纳入 maxBytes 之内（最终长度 <= maxBytes，
// 前提是 len(mark) <= maxBytes），并在截断点回退到字符边界（最多回退 utf8.UTFMax-1 字节）。
//
// 只在**截断点**回退，不改动截断点之前的字节：上游文本可能是非 UTF-8 编码（如 GBK），
// 用 utf8.ValidString(整个 cut) 判定会一路剥离到首个合法前缀，把大段证据塌缩掉。
func TruncateUTF8(text string, maxBytes int, mark string) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	limit := maxBytes - len(mark)
	if limit < 0 {
		limit = 0
	}
	cut := text[:limit]
	for i := 0; i < utf8.UTFMax-1 && len(cut) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + mark
}
