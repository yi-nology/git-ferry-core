package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBundle_VerifyRestore(t *testing.T) {
	// 准备一个仓库并打 bundle
	src := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0o644))
	run("add", ".")
	run("commit", "-m", "c1")

	backup := t.TempDir()
	bundlePath := filepath.Join(backup, "t1-main-20260101.bundle")
	cmd := exec.Command("git", "bundle", "create", bundlePath, "refs/heads/main")
	cmd.Dir = src
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	// verify
	info, err := VerifyBundle(context.Background(), bundlePath)
	require.NoError(t, err)
	assert.True(t, info.Valid)
	assert.NotEmpty(t, info.RefSpecs)

	// restore 到空目录
	dest := t.TempDir() + "/restored"
	require.NoError(t, RestoreBundle(context.Background(), bundlePath, dest))
	_, err = os.Stat(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)

	// 非空目录拒绝
	require.Error(t, RestoreBundle(context.Background(), bundlePath, dest))

	// list
	list, err := ListBundles(backup, "t1")
	require.NoError(t, err)
	require.Len(t, list, 1)
}
