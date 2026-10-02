// Package strutil 收敛 core 内字符串小工具,消除 executor/service 重复实现。
package strutil

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Truncate 按字节截断并追加省略号。保证结果是合法 UTF-8（回退到码点边界），
// 输入本身含非法字节时同样回退，避免截出乱码。
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n] + "…"
}

// TruncateRunes 按字符(rune)截断。
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Itoa64 int64 → 字符串。
func Itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// Itoa int → 字符串。
func Itoa(n int) string { return strconv.Itoa(n) }

// CSVEscape 转义 CSV 单元格。
func CSVEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// SanitizeFileToken 文件名安全化(防路径穿越)。
func SanitizeFileToken(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		return "unnamed"
	}
	return s
}

// SanitizePathToken 路径段安全化:去掉 / \ .. 与空格,防路径穿越。
func SanitizePathToken(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, " ", "_")
	if s == "" {
		return "unnamed"
	}
	return s
}

// BoolFact bool → "true"/"false"(健康评分 facts 用)。
func BoolFact(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
