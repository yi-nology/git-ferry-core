package strutil

import "testing"

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
