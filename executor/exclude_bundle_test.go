package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestAlignSourceBranches_RespectsExcludePatterns 被排除名单命中的分支
// **不得**被 align 重新造出来。
//
// 回归背景(P2):增量路径的顺序是 fetch → excludeRefs(删匹配 ref) →
// alignSourceBranches(把 refs/remotes/origin/<b> 写回 refs/heads/<b>)。
// align **不看排除名单** ⇒ 被排除的分支删掉之后又被重新创建成本地分支,
// 下一次 push 照样推给目标 ⇒ 黑名单形同虚设。
func TestAlignSourceBranches_RespectsExcludePatterns(t *testing.T) {
	src, srcBare, _ := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "secret/1.0", "s1", "cs1")
	commitBranchToSrc(t, src, "release/1.0", "r1", "cr1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	// glob 要**覆盖**被排除的分支,否则缺陷根本不会触发
	// (旧 align 只对齐匹配 glob 的分支)。
	task := &model.SyncTask{
		Key: "excl", SourceBranch: "*", TargetBranch: "*",
		ExcludeRefPatterns: "refs/heads/secret/*",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 被排除的分支不得被重建
	_, err = exec.backend.RevParse(ctx, work, "refs/heads/secret/1.0")
	assert.Error(t, err, "命中排除名单的分支不该被 align 重新造出来")

	// 没被排除的分支必须在
	_, err = exec.backend.RevParse(ctx, work, "refs/heads/release/1.0")
	assert.NoError(t, err, "未被排除的分支必须被对齐")
}

// TestWriteBundle_ExpandsGlob 多分支任务的冷备必须**真的产出来**。
//
// 回归背景(P2):writeBundle 把 "refs/heads/" + SourceBranch 当 rev,
// 多分支时那是 "refs/heads/release/*" ⇒ 不是合法 rev ⇒
// git bundle create 退出码 128 ⇒ **多分支任务永远产不出冷备**,
// 而 bundle 阶段是 optional(失败只进 details)⇒ 同步仍报成功、冷备静默停止。
func TestWriteBundle_ExpandsGlob(t *testing.T) {
	src, srcBare, _ := newLocalSyncLab(t)
	// writeBundle 期望的是 **workDir**(仓库在 <workDir>/repo),
	// 与管线里的 WorkDir 布局一致。
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "release/1.0", "r1", "cr1")
	commitBranchToSrc(t, src, "release/2.0", "r2", "cr2")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{Key: "bundle-multi", SourceBranch: "release/*", TargetBranch: "release/*"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	ctx := context.Background()
	require.NoError(t, os.MkdirAll(work, 0o750))
	require.NoError(t, exec.cloneRepo(ctx, filepath.Join(work, RepoDir), srcRepo, task, nil))
	require.NoError(t, exec.fetchRepo(ctx, filepath.Join(work, RepoDir), task, srcRepo, nil))

	backupDir := t.TempDir()
	var details strings.Builder
	path, err := exec.writeBundle(ctx, work, backupDir, task, &details)
	require.NoError(t, err, "多分支任务必须能产出冷备(原来必然失败)")
	require.NotEmpty(t, path)
	_, serr := os.Stat(path)
	require.NoError(t, serr, "冷备文件必须存在")

	// bundle 里必须含两个分支
	out := gitOut(t, filepath.Dir(path), "bundle", "list-heads", filepath.Base(path))
	assert.Contains(t, out, "release/1.0")
	assert.Contains(t, out, "release/2.0")
}

// TestRotateBundles_WiredToKeep 写完冷备后必须按 backup_keep 轮转。
//
// 回归背景(P2):rotateBundles **从来没有生产调用点** ⇒ 配置里
// "每任务保留最近 N 份"看起来是生效的,实际冷备目录只增不减,
// 把磁盘吃满(而 bundle 阶段失败又只是 optional,同步仍报成功)。
func TestRotateBundles_WiredToKeep(t *testing.T) {
	dir := t.TempDir()
	// 造 5 份同名任务的历史冷备
	for i := 1; i <= 5; i++ {
		name := "taskX-main-2026010" + string(rune('0'+i)) + "-000000.bundle"
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	removed := rotateBundles(dir, "taskX", 2)
	assert.Equal(t, 3, removed, "只保留最近 2 份 ⇒ 应删掉 3 份")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "目录里应只剩 2 份")
}
