package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

func TestGlobMatch(t *testing.T) {
	assert.True(t, matchBranchGlob("main", "main")) // 不应走 glob
	assert.True(t, matchBranchGlob("release/*", "release/1.0"))
	assert.True(t, matchBranchGlob("release/*", "release/1.0/hotfix"))
	assert.False(t, matchBranchGlob("release/*", "develop"))
	assert.True(t, matchBranchGlob("feat-?", "feat-a"))
	assert.False(t, matchBranchGlob("feat-?", "feat-ab"))
	assert.True(t, matchBranchGlob("*", "anything"))
	assert.True(t, matchBranchGlob("hotfix/*", "hotfix/urgent"))
}

func TestExpandBranchSpec_Single(t *testing.T) {
	branches, multi, err := expandBranchSpec("main", nil)
	require.NoError(t, err)
	assert.False(t, multi)
	assert.Equal(t, []string{"main"}, branches)
}

func TestExpandBranchSpec_Glob(t *testing.T) {
	list := func() ([]string, error) {
		return []string{"refs/heads/main", "refs/heads/release/1.0", "refs/heads/release/2.0", "refs/tags/v1"}, nil
	}
	branches, multi, err := expandBranchSpec("release/*", list)
	require.NoError(t, err)
	assert.True(t, multi)
	assert.ElementsMatch(t, []string{"release/1.0", "release/2.0"}, branches)
}

func TestExpandBranchSpec_NoMatch(t *testing.T) {
	list := func() ([]string, error) { return []string{"refs/heads/main"}, nil }
	_, _, err := expandBranchSpec("nope/*", list)
	require.Error(t, err)
}

func TestExpandBranchSpec_Empty(t *testing.T) {
	_, _, err := expandBranchSpec("", nil)
	require.Error(t, err)
}

func TestPushRefSpecs(t *testing.T) {
	specs := pushRefSpecs(nil, "main", "mirror-main", false)
	assert.Equal(t, []string{"refs/heads/main:refs/heads/mirror-main"}, specs)

	specs = pushRefSpecs([]string{"a", "b"}, "", "", true)
	assert.Equal(t, []string{"refs/heads/a:refs/heads/a", "refs/heads/b:refs/heads/b"}, specs)
}

func TestDivergentError_Classify(t *testing.T) {
	err := &divergentError{Branch: "main", Extra: []string{"abc fix"}}
	assert.True(t, isDivergent(err))
	assert.Equal(t, model.ErrorDivergent, ClassifyError(err))
	assert.Contains(t, err.Error(), "keep_divergent")
}

func TestClassifyError_Conflict(t *testing.T) {
	assert.Equal(t, model.ErrorConflict, ClassifyError(errStr("non-fast-forward")))
	assert.Equal(t, model.ErrorConflict, ClassifyError(errStr("Updates were rejected because the remote contains work")))
	assert.Equal(t, model.ErrorNetwork, ClassifyError(errStr("connection refused")))
}

type errStr string

func (e errStr) Error() string { return string(e) }
