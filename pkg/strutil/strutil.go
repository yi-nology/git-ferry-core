// Package strutil 收敛 core 内字符串小工具,消除 executor/service 重复实现。
package strutil

import (
	"strconv"
	"strings"
)

// Truncate 按字节截断并追加省略号。
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
