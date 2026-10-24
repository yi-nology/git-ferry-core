package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestCheckDivergencePolicy_ErrorReportsRealCount 错误文案里的"独有提交数"
// 必须是真实数量,不是展示列表的长度。
//
// 回归背景:文案用 len(Extra),而 Extra 是**有上限截断的展示列表**
// (maxDivergentLog=5),且不同 git 后端对"独有"的判定不一致
// (gogit 的 GetCommitsBetween 会把共同祖先也带进来)。
// ⇒ 目标侧只有 1 个独有提交时,文案却写 "has 5 commit(s) not in source",
// 运维按这个数字判断影响面会判错。
func TestCheckDivergencePolicy_ErrorReportsRealCount(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	// 目标侧只有 **1** 个独有提交
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "y.txt"), []byte("y"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	gitRun(t, dstWork, "push", "-q", dstBare, "main")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "div-count", SourceBranch: "main", TargetBranch: "main"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	commitToSrc(t, src, "v2", "c2")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has 1 commit(s) not in source",
		"错误文案里的独有提交数必须是真实数量(目标侧只有 1 个独有提交)")
}

// TestCheckDivergencePolicy_TargetHasUniqueCommits_Blocks 目标侧有独有提交时,
// block 策略必须拦下(不得覆盖目标侧)。
func TestCheckDivergencePolicy_TargetHasUniqueCommits_Blocks(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	// 目标侧独立提交一个 commit(与源端分叉)
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "y.txt"), []byte("y"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	gitRun(t, dstWork, "push", "-q", dstBare, "main")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "div-block", SourceBranch: "main", TargetBranch: "main"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	// 源端再提交一个,让源端与目标侧真正分叉
	commitToSrc(t, src, "v2", "c2")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
	require.Error(t, err, "目标侧有独有提交时 block 策略必须拦下")
}

// TestCheckDivergencePolicy_TargetUnreachable_FailsClosed 钉住"连不上目标 ≠ 无分歧"。
//
// 回归背景:分歧保护里 fetch 目标分支失败时,原实现直接放行(= return nil),
// 于是"目标侧有独有提交 + 这次连不上目标"会静默 force push 覆盖掉目标侧提交,
// 任务还报成功。现在:本地若已有该目标分支的远端跟踪 ref(=目标确实有这个分支),
// 连不上就必须报错,不能放行。
func TestCheckDivergencePolicy_TargetUnreachable_FailsClosed(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "div-unreachable", SourceBranch: "main", TargetBranch: "main"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	// 目标仓先有 main 分支,让本地能成功 fetch 一次并留下 refs/remotes/target/main
	gitRun(t, work, "push", "-q", dstBare, "refs/heads/main:refs/heads/main")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))
	gitRun(t, work, "fetch", "-q", "target", "main")
	require.NotEmpty(t, gitOut(t, work, "rev-parse", "refs/remotes/target/main"),
		"前置条件:本地应已留下目标分支的远端跟踪 ref")

	// 把 target remote 指向不存在的路径 ⇒ 这次 fetch 必然失败
	gitRun(t, work, "remote", "set-url", "target", filepath.Join(t.TempDir(), "does-not-exist.git"))

	err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
	require.Error(t, err, "连不上目标时不能当作无分歧放行,必须报错")
	require.Contains(t, err.Error(), "cannot verify divergence")
}

// TestCheckDivergencePolicy_FirstPush_NoTargetBranch_Allows 目标分支压根不存在
// (首次推送)⇒ 无分歧风险,应当放行。
func TestCheckDivergencePolicy_FirstPush_NoTargetBranch_Allows(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "div-first-push", SourceBranch: "main", TargetBranch: "main"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))

	// 目标仓是空仓(没有 main 分支)⇒ 首次推送,无分歧风险
	require.NoError(t, exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil))
}
