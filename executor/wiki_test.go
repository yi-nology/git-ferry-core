package executor

import (
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"testing"

	"github.com/stretchr/testify/assert"
)

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

func TestSanitizeFileToken(t *testing.T) {
	assert.Equal(t, "my_task", strutil.SanitizeFileToken("my/task"))
	assert.Equal(t, "a_b", strutil.SanitizeFileToken(`a\b`))
	assert.Equal(t, "_", strutil.SanitizeFileToken(".."))
	assert.Equal(t, "unnamed", strutil.SanitizeFileToken(""))
}
