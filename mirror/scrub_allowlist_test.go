package mirror

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func autoRule(name, pattern, replacement string, allow []string) ScrubRule {
	return ScrubRule{
		Name: name, Category: "test",
		Pattern:  regexp.MustCompile(pattern),
		Severity: ScrubAuto, Replacement: replacement,
		PathAllowlist: allow,
	}
}

// TestScrubAllowlistPassesWithTrace 白名单放行但留痕:
// 内容不动、不计 Cleaned,命中记入 ScrubHits(Allowlisted=true)。
func TestScrubAllowlistPassesWithTrace(t *testing.T) {
	rule := autoRule("alias", `rd1`, "cluster-a", []string{"**/*_test.go"})

	res := ApplyScrub("svc/target_test.go", []byte("host rd1 up\n"), []ScrubRule{rule})
	assert.Equal(t, "host rd1 up\n", string(res.Content), "白名单内内容必须原样保留")
	assert.False(t, res.Cleaned, "放行不计 Cleaned")
	require.Len(t, res.Hits, 1, "放行也要留痕")
	hit := res.Hits[0]
	assert.True(t, hit.Allowlisted)
	assert.False(t, hit.Mandatory)
	assert.Equal(t, "svc/target_test.go", hit.Path)
	assert.Equal(t, 1, hit.Line)
}

// TestScrubAllowlistOutsideAborts 白名单外的命中照常清理(默认行为不变)。
func TestScrubAllowlistOutsideAborts(t *testing.T) {
	rule := autoRule("alias", `rd1`, "cluster-a", []string{"**/*_test.go"})

	res := ApplyScrub("svc/target.go", []byte("host rd1 up\n"), []ScrubRule{rule})
	assert.Equal(t, "host cluster-a up\n", string(res.Content))
	assert.True(t, res.Cleaned)
	require.Len(t, res.Hits, 1)
	assert.False(t, res.Hits[0].Allowlisted)
}

// TestScrubMandatoryIgnoresAllowlist 密钥类没有放行面:
// 即使配置了白名单,mandatory 命中仍然成立(本包防御性兜底;
// service 层在建通道时直接拒绝该组合)。
func TestScrubMandatoryIgnoresAllowlist(t *testing.T) {
	rule := ScrubRule{
		Name: "secret", Category: "secret",
		Pattern:       regexp.MustCompile(`SECRET-[A-Za-z0-9]+`),
		Severity:      ScrubMandatory,
		PathAllowlist: []string{"**/*"},
	}

	res := ApplyScrub("tests/fixture.go", []byte("key = SECRET-abcdef\n"), []ScrubRule{rule})
	require.Len(t, res.MandatoryHits, 1, "mandatory 命中不得被白名单放行")
	assert.Equal(t, "key = SECRET-abcdef\n", string(res.Content), "mandatory 内容不动(redact 只作用于命中的展示文本)")
}

// TestScrubHitPathRecorded 命中带文件路径(allowlist 留痕的前提)。
func TestScrubHitPathRecorded(t *testing.T) {
	rule := autoRule("alias", `rd1`, "", nil)
	res := ApplyScrub("docs/x.md", []byte("rd1\n"), []ScrubRule{rule})
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "docs/x.md", res.Hits[0].Path)
}
