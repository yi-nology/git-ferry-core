package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestDirSizeBytes 递归统计(含子目录,不跟随符号链接)。
func TestDirSizeBytes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o600))
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "b"), make([]byte, 250), 0o600))

	size, err := dirSizeBytes(dir)
	require.NoError(t, err)
	assert.Equal(t, int64(350), size)

	// 空目录 = 0
	empty, err := dirSizeBytes(t.TempDir())
	require.NoError(t, err)
	assert.Zero(t, empty)
}

// TestEnforceWorkdirQuota 三种形态:未配置放行 / 未超限放行 / 超限中止并清理。
func TestEnforceWorkdirQuota(t *testing.T) {
	newRC := func(t *testing.T, limitMB int, fileBytes int) (*RunContext, string) {
		workDir := t.TempDir()
		repoDir := filepath.Join(workDir, "repo")
		require.NoError(t, os.MkdirAll(repoDir, 0o750))
		if fileBytes > 0 {
			require.NoError(t, os.WriteFile(filepath.Join(repoDir, "obj.pack"), make([]byte, fileBytes), 0o600))
		}
		e := &Executor{service: stubRunService{cfg: &model.Config{
			Sync: model.SyncConfig{MaxWorkdirMB: limitMB},
		}}}
		return &RunContext{
			WorkDir: workDir, RepoDir: repoDir,
			Details: &strings.Builder{}, Exec: e,
		}, workDir
	}

	// 未配置(0)= 不限制
	rc, _ := newRC(t, 0, 10<<20)
	require.NoError(t, rc.Exec.enforceWorkdirQuota(rc))

	// 未超限
	rc, workDir := newRC(t, 8, 4<<20)
	require.NoError(t, rc.Exec.enforceWorkdirQuota(rc))
	_, statErr := os.Stat(workDir)
	assert.NoError(t, statErr, "未超限不得清理工作区")

	// 超限:中止 + 工作区已清理(reclaim 磁盘)
	rc, workDir = newRC(t, 1, 4<<20)
	err := rc.Exec.enforceWorkdirQuota(rc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "磁盘配额")
	_, statErr = os.Stat(workDir)
	assert.True(t, os.IsNotExist(statErr), "超限必须清理工作区 reclaim 磁盘")
	assert.Contains(t, rc.Details.String(), "quota exceeded")
}
