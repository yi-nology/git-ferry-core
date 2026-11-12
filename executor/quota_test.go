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

// TestDirSizeBytes_FailClosedOnRoot 根目录不可达 = 整体测量失败,必须报错
// (#16):此前回调把一切错误吞成 nil,测不出被当成 0 字节,fail-closed
// 分支是死代码。
func TestDirSizeBytes_FailClosedOnRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受 000 权限限制,无法构造不可读目录")
	}

	// 根目录不存在
	_, err := dirSizeBytes(filepath.Join(t.TempDir(), "no-such-dir"))
	require.Error(t, err, "目录不存在必须报错(此前 err=nil)")

	// 根目录不可读(000):读目录失败必须上抛
	root := t.TempDir()
	sub := filepath.Join(root, "heavy")
	require.NoError(t, os.MkdirAll(sub, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "pack"), make([]byte, 1<<20), 0o600))
	require.NoError(t, os.Chmod(root, 0o000))
	t.Cleanup(func() { _ = os.Chmod(root, 0o750) }) // 便于 TempDir 清理
	_, err = dirSizeBytes(root)
	require.Error(t, err, "根目录不可读必须报错,不得当成 0 字节放行")
}

// TestDirSizeBytes_ToleratesUnreadableChild 单个子项不可读仍容错跳过
// (如被并发清理的临时文件),不阻断整体统计 —— 与 fail-closed 不冲突:
// 容错的只是子项,根不可达必须报错。
func TestDirSizeBytes_ToleratesUnreadableChild(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受 000 权限限制,无法构造不可读目录")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o600))
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "b"), make([]byte, 250), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	size, err := dirSizeBytes(dir)
	require.NoError(t, err, "子项不可读只容错跳过,整体测量仍应成功")
	assert.Equal(t, int64(100), size)
}

// TestEnforceWorkdirQuota_FailClosedOnUnmeasurable 配额已配置而体积测不出
// ⇒ 必须中止(fail-closed,契约:测不准等于没保护),且不得误清工作区。
func TestEnforceWorkdirQuota_FailClosedOnUnmeasurable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 不受 000 权限限制,无法构造不可读目录")
	}
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o750))
	// 工作区本身不可读 ⇒ 测量失败(fail-closed 的触发场景)
	require.NoError(t, os.Chmod(workDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(workDir, 0o750) })

	e := &Executor{service: stubRunService{cfg: &model.Config{
		Sync: model.SyncConfig{MaxWorkdirMB: 1},
	}}}
	rc := &RunContext{
		WorkDir: workDir, RepoDir: repoDir,
		Details: &strings.Builder{}, Exec: e,
	}
	err := e.enforceWorkdirQuota(rc)
	require.Error(t, err, "体积测不出必须中止(fail-closed),不得当 0 字节放行")
	assert.Contains(t, err.Error(), "fail-closed")
}

// TestEnforceWorkdirQuota_CoversWholeWorkdir 测量口径必须覆盖整棵工作区
// (#17):wiki 是 WorkDir 下与 RepoDir 平级的第二个完整 clone,只测
// RepoDir 时配额被静默绕过。测量与清理口径(WorkDir)必须一致。
func TestEnforceWorkdirQuota_CoversWholeWorkdir(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o750))
	// 膨胀在 RepoDir 之外:与 repo 平级的 wiki clone(全历史 + LFS 同体量)
	wikiDir := filepath.Join(workDir, "wiki")
	require.NoError(t, os.MkdirAll(wikiDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(wikiDir, "objects.pack"), make([]byte, 4<<20), 0o600))

	e := &Executor{service: stubRunService{cfg: &model.Config{
		Sync: model.SyncConfig{MaxWorkdirMB: 1},
	}}}
	rc := &RunContext{
		WorkDir: workDir, RepoDir: repoDir,
		Details: &strings.Builder{}, Exec: e,
	}
	err := e.enforceWorkdirQuota(rc)
	require.Error(t, err, "工作区真实体积 4MB 超过 1MB 上限,只测 RepoDir(0 字节)会静默放行")
	assert.Contains(t, err.Error(), "磁盘配额")
	_, statErr := os.Stat(workDir)
	assert.True(t, os.IsNotExist(statErr), "超限清理口径也是整棵工作区")
}
