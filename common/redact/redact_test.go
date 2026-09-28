package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// testRedactionPolicy 是测试用凭据政策：字段名表与授权前缀均显式注入，
// 与 buddy 生产调用点同源，但本包不硬编码任何渠道名。
var testRedactionPolicy = RedactionPolicy{
	CredentialNames: []string{"secret", "key", "token", "password", "credential", "authorization"},
	AuthPrefixes:    []string{"Bearer"},
}

// TestRedactCredentialsRemovesCredentialValues 断言脱敏后输出不含完整凭据值，
// 覆盖 JSON 字段、头回显、裸名、未闭合引号、单引号、base64 令牌等回显形态。
func TestRedactCredentialsRemovesCredentialValues(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{"json field", `{"accessToken":"sk-live-JSONSECRET123456"}`, "sk-live-JSONSECRET123456"},
		{"bearer header echo", `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig`, "eyJhbGciOiJIUzI1NiJ9.payload.sig"},
		{"plain bearer", `denied for Bearer tok-ABCDEF123456`, "tok-ABCDEF123456"},
		{"refresh token field", `{"refresh_token":"rt-secret-value-1234567890"}`, "rt-secret-value-1234567890"},
		{"api key underscore", `{"api_key":"sk-VAR-api_key-123456"}`, "sk-VAR-api_key-123456"},
		{"header echo field", `X-Refresh-Token: sk-VAR-refreshHeader-123456`, "sk-VAR-refreshHeader-123456"},
		{"non-json equals", `access_token=sk-VAR-equalsForm-123456`, "sk-VAR-equalsForm-123456"},
		{"non-json colon", `accessToken: sk-VAR-colonForm-123456`, "sk-VAR-colonForm-123456"},
		{"unclosed quote", `{"error":"boom","clientSecret":"sk-live-TRUNCATEDSECRETVALUE`, "sk-live-TRUNCATEDSECRETVALUE"},
		{"single quoted", `accessToken='sk-BYPASS-singleQuoted-123456'`, "sk-BYPASS-singleQuoted-123456"},
		{"spaced bare value", `accessToken: sk BYPASS spaced 123456`, "sk BYPASS spaced 123456"},
		{"bare name form", `accessToken sk-BYPASS-noSeparator-123456`, "sk-BYPASS-noSeparator-123456"},
		{"bearer base64 chars", `denied for Bearer abc+def/ghi==extra`, "abc+def/ghi==extra"},
		{"password field", `{"password":"sk-VAR-password-123456"}`, "sk-VAR-password-123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactCredentials(tc.input, testRedactionPolicy)
			if strings.Contains(got, tc.secret) {
				t.Errorf("RedactCredentials(%q) = %q, must not contain the full credential %q", tc.input, got, tc.secret)
			}
		})
	}
}

// TestRedactCredentialsPreservesFieldNamesAndProse 断言字段名本身被保留（可证明上游确实回了凭据），
// 且关键字后的普通文案不被误抹（不牺牲业务文案保真）。
func TestRedactCredentialsPreservesFieldNamesAndProse(t *testing.T) {
	got := RedactCredentials(`{"accessToken":"sk-secret-1234567890"}`, testRedactionPolicy)
	if !strings.Contains(got, "accessToken") {
		t.Errorf("field name must be retained, got %q", got)
	}

	prose := RedactCredentials("token expired, please login again", testRedactionPolicy)
	if prose != "token expired, please login again" {
		t.Errorf("ordinary prose after a keyword must be preserved, got %q", prose)
	}
}

// TestRedactCredentialsPolicyIsInjected 断言本包不硬编码渠道凭据名：空政策下不做任何脱敏，
// 自定义政策下按注入的名称/前缀脱敏（渠道政策由调用点声明）。
func TestRedactCredentialsPolicyIsInjected(t *testing.T) {
	const input = `accessToken: sk-should-stay`

	if got := RedactCredentials(input, RedactionPolicy{}); got != input {
		t.Errorf("empty policy must not redact anything, got %q", got)
	}

	custom := RedactionPolicy{CredentialNames: []string{"accessToken"}, AuthPrefixes: []string{"Cloud-IDE-JWT"}}
	if got := RedactCredentials(input, custom); strings.Contains(got, "sk-should-stay") {
		t.Errorf("custom policy must redact the injected name, got %q", got)
	}
	jwt := `header Cloud-IDE-JWT eyJhbGciOiJIUzI1NiJ9.payload`
	if got := RedactCredentials(jwt, custom); strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9.payload") {
		t.Errorf("custom policy must redact the injected auth prefix, got %q", got)
	}
}

// TestRedactCredentialsAppliesMaxBytes 断言政策声明的 MaxBytes 生效（脱敏后仍受上界约束）。
func TestRedactCredentialsAppliesMaxBytes(t *testing.T) {
	policy := testRedactionPolicy
	policy.MaxBytes = 40
	policy.TruncatedMark = "…"

	got := RedactCredentials(strings.Repeat("a", 200), policy)
	if len(got) > 40 {
		t.Errorf("len = %d, want <= 40", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated output must carry the declared mark, got %q", got)
	}
}

// TestTruncateUTF8AtDeclaredLength 断言截断发生在约定长度：ASCII 输入恰好截到上限
// （含 mark 时总长仍 <= 上限），未超限输入原样返回。
func TestTruncateUTF8AtDeclaredLength(t *testing.T) {
	long := strings.Repeat("x", 100)

	if got := TruncateUTF8(long, 20, ""); len(got) != 20 {
		t.Errorf("len = %d, want exactly 20", len(got))
	}
	if got := TruncateUTF8(long, 20, "…(truncated)"); len(got) != 20 {
		t.Errorf("len with mark = %d, want exactly 20 (mark counts inside the bound)", len(got))
	}
	if got := TruncateUTF8("short", 20, ""); got != "short" {
		t.Errorf("input within bound must be unchanged, got %q", got)
	}
}

// TestTruncateUTF8BoundarySafety 断言多字节 rune 不被切分：截断点回退到字符边界，
// 输出仍是合法 UTF-8 且不超上界。
func TestTruncateUTF8BoundarySafety(t *testing.T) {
	// "中" 为 3 字节：max=8 落在第 3 个字符中间，须回退到 6 字节（2 个完整字符）。
	multi := strings.Repeat("中", 100)
	got := TruncateUTF8(multi, 8, "")
	if !utf8.ValidString(got) {
		t.Errorf("truncated output must be valid UTF-8, got %q", got)
	}
	if len(got) > 8 {
		t.Errorf("len = %d, want <= 8", len(got))
	}
	if got != strings.Repeat("中", 2) {
		t.Errorf("truncation must fall back to the rune boundary, got %q", got)
	}

	// 含 mark 时同样不得切出半个字符。
	gotMark := TruncateUTF8(multi, 9, "…")
	if !utf8.ValidString(gotMark) {
		t.Errorf("truncated output with mark must be valid UTF-8, got %q", gotMark)
	}
	if len(gotMark) > 9 {
		t.Errorf("len with mark = %d, want <= 9", len(gotMark))
	}
}

// TestTruncateUTF8NoTruncationWhenMaxNonPositive 断言 MaxBytes <= 0 表示不截断。
func TestTruncateUTF8NoTruncationWhenMaxNonPositive(t *testing.T) {
	long := strings.Repeat("中", 100)

	if got := TruncateUTF8(long, 0, "…"); got != long {
		t.Errorf("maxBytes=0 must not truncate, got len=%d", len(got))
	}
	if got := TruncateUTF8(long, -1, "…"); got != long {
		t.Errorf("negative maxBytes must not truncate, got len=%d", len(got))
	}
}
