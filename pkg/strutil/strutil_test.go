package strutil

import (
	"testing"
	"unicode/utf8"
)

func TestSanitizeFileToken(t *testing.T) {
	if got := SanitizeFileToken("a/b"); got != "a_b" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizeFileToken(""); got != "unnamed" {
		t.Fatalf("got %q", got)
	}
}

func TestTruncateCSVEscape(t *testing.T) {
	if got := Truncate("abcdef", 2); got != "ab…" {
		t.Fatalf("got %q", got)
	}
	if got := CSVEscape("a,b"); got != `"a,b"` {
		t.Fatalf("got %q", got)
	}
	if got := Itoa(7); got != "7" {
		t.Fatalf("got %q", got)
	}
}

// TestTruncateUTF8 截断必须落在码点边界，中文不截出乱码。
func TestTruncateUTF8(t *testing.T) {
	s := "中文字符串测试"
	// "中"=3B "文"=3B "字"=3B → 3 字节处是边界内，4 字节落在"文"中间
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7} {
		got := Truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("n=%d result invalid UTF-8: %q", n, got)
		}
	}
	if got := Truncate(s, 6); got != "中文…" {
		t.Fatalf("got %q", got)
	}
	if got := Truncate(s, 100); got != s {
		t.Fatalf("got %q", got)
	}
	if got := Truncate(s, 0); got != "" {
		t.Fatalf("got %q", got)
	}
	// 输入本身含非法字节：同样回退到合法边界
	bad := "ab\xff\xffcd"
	if got := Truncate(bad, 3); !utf8.ValidString(got) {
		t.Fatalf("got %q invalid", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := TruncateRunes("中文字符串", 2); got != "中文…" {
		t.Fatalf("got %q", got)
	}
	if got := TruncateRunes("abc", 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := TruncateRunes("abc", 0); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizePathToken(t *testing.T) {
	if got := SanitizePathToken("a b/c"); got != "a_b_c" {
		t.Fatalf("got %q", got)
	}
	if got := SanitizePathToken(""); got != "unnamed" {
		t.Fatalf("got %q", got)
	}
}

func TestBoolFact(t *testing.T) {
	if BoolFact(true) != "true" || BoolFact(false) != "false" {
		t.Fatal("BoolFact wrong")
	}
}
