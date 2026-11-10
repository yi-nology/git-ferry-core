package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/mirror"
)

func samplePolicy(t *testing.T) ([]mirror.ScrubRule, mirror.Mapping) {
	t.Helper()
	snap := mirror.BuildPolicySnapshot(
		mirror.Mapping{Source: "example.com/foo", Target: "github.com/org/bar"},
		[]mirror.ScrubRule{{
			Name: "alias", Category: "internal-host",
			Pattern:  mustCompile(t, `\brd1\b`),
			Severity: mirror.ScrubAuto, Replacement: "cluster-a",
			PathAllowlist: []string{"**/*_test.go"},
		}},
	)
	rules, mapping, err := loadPolicy([]byte(snap))
	require.NoError(t, err)
	return rules, mapping
}

func mustCompile(t *testing.T, p string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(p)
	require.NoError(t, err)
	return re
}

// TestLoadPolicyRoundTrip 快照 → 规则 round-trip:字段全部还原。
func TestLoadPolicyRoundTrip(t *testing.T) {
	rules, mapping := samplePolicy(t)
	require.Len(t, rules, 1)
	assert.Equal(t, "alias", rules[0].Name)
	assert.Equal(t, "cluster-a", rules[0].Replacement)
	assert.Equal(t, []string{"**/*_test.go"}, rules[0].PathAllowlist)
	assert.Equal(t, "example.com/foo", mapping.Source)
}

// TestLoadPolicyStrict 未知字段 = 解析失败(fail-closed;两侧 schema
// 漂移必须红,不能静默忽略)。
func TestLoadPolicyStrict(t *testing.T) {
	_, _, err := loadPolicy([]byte(`{"version":1,"mapping":{},"scrubRules":[],"unknown":1}`))
	require.Error(t, err, "未知字段必须报错")
	_, _, err = loadPolicy([]byte(`{"version":2,"mapping":{},"scrubRules":[]}`))
	require.Error(t, err, "未知版本必须报错")
	_, _, err = loadPolicy([]byte(`{"version":1,"mapping":{},"scrubRules":[{"name":"x","pattern":"([bad"}]}`))
	require.Error(t, err, "坏正则必须报错")
}

// TestScanAll_EndToEnd 文件面复验:命中指名 文件:行+规则;白名单内
// 放行但留痕;二进制里的身份/密钥不因二进制形态放行。
func TestScanAll_EndToEnd(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	write("svc/target.go", "cluster rd1 up\n")
	write("svc/target_test.go", "cluster rd1 up\n")       // 白名单内
	write("README.md", "go get example.com/foo@latest\n") // 身份残留
	write("bin.dat", "example.com/foo\x00payload")        // 二进制身份
	write(".git/hidden", "rd1\n")                         // .git 排除

	rules, mapping := samplePolicy(t)
	hits, err := scanAll([]string{root}, rules, mapping)
	require.NoError(t, err)

	var failed, allowed int
	byRule := map[string]int{}
	for _, h := range hits {
		if h.Allowlisted {
			allowed++
			continue
		}
		failed++
		byRule[h.Rule]++
		assert.NotEqual(t, ".git/hidden", h.Path, ".git 不得进入扫描")
	}
	assert.Equal(t, 1, byRule["alias"], "白名单外的 rd1 必须命中")
	assert.Equal(t, 2, byRule["identity-residual"], "身份残留必须命中(README 文本 + bin.dat 二进制)")
	assert.Equal(t, 3, failed)
	assert.Equal(t, 1, allowed, "白名单内的命中要放行但留痕")
}
