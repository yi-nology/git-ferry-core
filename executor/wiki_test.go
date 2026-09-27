package executor

import "testing"

func TestWikiURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/o/r.git", "https://github.com/o/r.wiki.git"},
		{"https://gitlab.com/o/r.git", "https://gitlab.com/o/r.wiki.git"},
		{"https://github.com/o/r.wiki.git", "https://github.com/o/r.wiki.git"},
		{"https://gitea.com/o/r", "https://gitea.com/o/r.wiki.git"},
		{"git@github.com:o/r.git", "git@github.com:o/r.wiki.git"},
		{"/local/path/repo", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := wikiURL(c.in); got != c.want {
			t.Errorf("wikiURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
