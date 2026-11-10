package mirror

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleRules() []ScrubRule {
	return []ScrubRule{
		{
			Name: "alias", Category: "internal-host",
			Pattern:  regexp.MustCompile(`\brd1\b`),
			Severity: ScrubAuto, Replacement: "cluster-a",
			PathAllowlist: []string{"**/*_test.go"},
		},
		{
			Name: "secret", Category: "secret",
			Pattern:  regexp.MustCompile(`SECRET-[A-Za-z0-9]+`),
			Severity: ScrubMandatory,
		},
	}
}

// TestBuildPolicySnapshotDeterministic 快照是确定性的:同输入 ⇒ 同字节
// (同 tag 重跑报告一致),规则保持声明顺序,allowlist 保留。
func TestBuildPolicySnapshotDeterministic(t *testing.T) {
	m := Mapping{Source: "example.com/foo", Target: "github.com/org/bar"}
	a := BuildPolicySnapshot(m, sampleRules())
	b := BuildPolicySnapshot(m, sampleRules())
	assert.Equal(t, a, b, "同输入必须产出逐字节一致的快照")

	var snap PolicySnapshot
	require.NoError(t, json.Unmarshal([]byte(a), &snap))
	assert.Equal(t, 1, snap.Version)
	assert.Equal(t, "example.com/foo", snap.Mapping.Source)
	assert.Equal(t, "github.com/org/bar", snap.Mapping.Target)
	require.Len(t, snap.ScrubRules, 2)
	assert.Equal(t, "alias", snap.ScrubRules[0].Name)
	assert.Equal(t, "auto", snap.ScrubRules[0].Severity)
	assert.Equal(t, []string{"**/*_test.go"}, snap.ScrubRules[0].PathAllowlist)
	assert.Equal(t, "mandatory", snap.ScrubRules[1].Severity)
	assert.Empty(t, snap.ScrubRules[1].PathAllowlist)
}

// TestPolicySnapshotRuleOrderStable 规则顺序变化 ⇒ 快照变化
// (顺序是策略的一部分,verifycmd 按同一顺序扫)。
func TestPolicySnapshotRuleOrderStable(t *testing.T) {
	m := Mapping{Source: "example.com/foo", Target: "github.com/org/bar"}
	fwd := BuildPolicySnapshot(m, sampleRules())
	rules := sampleRules()
	rules[0], rules[1] = rules[1], rules[0]
	rev := BuildPolicySnapshot(m, rules)
	assert.NotEqual(t, fwd, rev, "规则顺序变化必须反映在快照里")
}
