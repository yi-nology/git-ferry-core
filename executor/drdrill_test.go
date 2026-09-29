package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTestBundle 造一个含单 commit 单分支的 bundle。
func makeTestBundle(t *testing.T, dir, name string) string {
	t.Helper()
	src := filepath.Join(dir, "src")
	require.NoError(t, os.MkdirAll(src, 0o750))
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o640))
	run("add", ".")
	run("commit", "-m", "init")
	bundle := filepath.Join(dir, name)
	run("bundle", "create", bundle, "main")
	return bundle
}

func TestRunDRDrill_Success(t *testing.T) {
	dir := t.TempDir()
	bundle := makeTestBundle(t, dir, "task1-main-20260101-000000.bundle")

	rep, err := RunDRDrill(context.Background(), bundle)
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.True(t, rep.Success, "errors=%v fsck=%v missing=%v", rep.Errors, rep.FsckOutput, rep.MissingRefs)
	assert.True(t, rep.FsckOK)
	assert.Empty(t, rep.MissingRefs)
	assert.NotEmpty(t, rep.RefSpecs)
	assert.GreaterOrEqual(t, rep.CommitCount, 1)
	assert.NotEmpty(t, rep.EstRTO)
}

func TestRunDRDrill_MissingBundle(t *testing.T) {
	_, err := RunDRDrill(context.Background(), filepath.Join(t.TempDir(), "nope.bundle"))
	assert.Error(t, err)
}

func TestBatchDRDrill(t *testing.T) {
	dir := t.TempDir()
	b1 := makeTestBundle(t, filepath.Join(dir, "b1"), "a-main-20260101-000000.bundle")
	b2 := makeTestBundle(t, filepath.Join(dir, "b2"), "b-main-20260101-000000.bundle")

	reports, summary := BatchDRDrill(context.Background(), []string{b1, b2})
	assert.Len(t, reports, 2)
	assert.Equal(t, 2, summary["total"])
	assert.Equal(t, 2, summary["success"])
}

func TestDrillHistoryChain(t *testing.T) {
	dir := t.TempDir()
	bundle := makeTestBundle(t, dir, "task-main-20260101-000000.bundle")
	hist := filepath.Join(dir, "hist.jsonl")

	rep, err := RunDRDrill(context.Background(), bundle)
	require.NoError(t, err)
	e1, err := SaveDrillReport(hist, rep)
	require.NoError(t, err)
	assert.NotEmpty(t, e1.Hash)
	assert.Empty(t, e1.PrevHash)

	e2, err := SaveDrillReport(hist, rep)
	require.NoError(t, err)
	assert.Equal(t, e1.Hash, e2.PrevHash)

	ok, n, broken := VerifyDrillChain(hist)
	assert.True(t, ok, "broken=%s", broken)
	assert.Equal(t, 2, n)

	// 篡改后应检出
	require.NoError(t, os.WriteFile(hist, []byte(`{"report":{"bundle_name":"x"},"prev_hash":"","hash":"deadbeef"}`+"\n"), 0o640))
	ok, _, broken = VerifyDrillChain(hist)
	assert.False(t, ok)
	assert.NotEmpty(t, broken)
}

func TestBuildAndVerifyManifest(t *testing.T) {
	dir := t.TempDir()
	_ = makeTestBundle(t, dir, "task-main-20260101-000000.bundle")

	m, err := BuildManifest(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, m.EntryCount)
	assert.NotEmpty(t, m.MerkleRoot)
	_, err = SaveManifest(m)
	require.NoError(t, err)

	res, err := VerifyManifest(dir)
	require.NoError(t, err)
	assert.True(t, res.OK, "msg=%s", res.Message)

	// 篡改 bundle 内容
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bundle") {
			p := filepath.Join(dir, e.Name())
			data, _ := os.ReadFile(p)
			require.NoError(t, os.WriteFile(p, append(data, 'X'), 0o640))
		}
	}
	res, err = VerifyManifest(dir)
	require.NoError(t, err)
	assert.False(t, res.OK)
	assert.NotEmpty(t, res.HashMismatch)
}

func TestManifestEmptyDir(t *testing.T) {
	dir := t.TempDir()
	m, err := BuildManifest(dir)
	require.NoError(t, err)
	assert.Equal(t, 0, m.EntryCount)
	assert.Empty(t, m.MerkleRoot)
}

func TestNormalizeRefDiff(t *testing.T) {
	missing, extra := diffRefs(
		[]string{"refs/heads/main", "refs/heads/dev"},
		map[string]string{
			"refs/heads/main": "aaa",
			"refs/tags/v1":    "bbb",
		},
	)
	assert.Equal(t, []string{"refs/heads/dev"}, missing)
	assert.Len(t, extra, 1)
	assert.Contains(t, extra[0], "v1")
}

func TestFormatDuration(t *testing.T) {
	assert.Equal(t, "500ms", formatDuration(500*time.Millisecond))
	assert.Contains(t, formatDuration(2*time.Second), "s")
}
