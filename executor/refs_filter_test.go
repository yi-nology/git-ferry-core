package executor

import "testing"

func TestMatchesInclude(t *testing.T) {
	cases := []struct {
		include, branch string
		want            bool
	}{
		{"", "main", true},
		{"main,release/*", "main", true},
		{"main,release/*", "release/1.0", true},
		{"main,release/*", "feature/x", false},
		{"main", "Main", false},
	}
	for i, c := range cases {
		if got := matchesInclude(c.include, c.branch); got != c.want {
			t.Fatalf("case %d: matchesInclude(%q,%q)=%v want %v", i, c.include, c.branch, got, c.want)
		}
	}
}

func TestMatchRefGlob(t *testing.T) {
	cases := []struct {
		pattern, ref string
		want         bool
	}{
		{"refs/pull/*", "refs/pull/1/head", true},
		{"refs/pull/*", "refs/heads/main", false},
		{"refs/merge-requests/*", "refs/merge-requests/3/head", true},
		{"refs/heads/*", "refs/heads/feature/x", true},
	}
	for i, c := range cases {
		if got := matchRefGlob(c.pattern, c.ref); got != c.want {
			t.Fatalf("case %d: matchRefGlob(%q,%q)=%v want %v", i, c.pattern, c.ref, got, c.want)
		}
	}
}

func TestSplitCSVNonEmpty(t *testing.T) {
	got := splitCSVNonEmpty(" a, ,b ,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v", got)
	}
	if splitCSVNonEmpty("") != nil {
		t.Fatal("want nil")
	}
}
