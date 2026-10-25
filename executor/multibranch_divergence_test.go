package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestPush_MultiBranch_BlockPolicyBlocksDivergentBranch 多分支任务的分歧保护
// 必须**真的拦下来**。
//
// 回归背景(P0):分歧判定把 glob("release/*")当分支名传给了 git rev-list,
// 含 * 的 rev 必然报错 ⇒ 旧实现 fail-open 直接放行 ⇒
// block 策略不拦、backup_on_demand 不备份,随后强推照常执行 ——
// 分歧保护对多分支**整体失效**(目标侧独有提交被覆盖,任务仍报 success)。
func TestPush_MultiBranch_BlockPolicyBlocksDivergentBranch(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "release/1.0", "r1", "cr1")
	commitBranchToSrc(t, src, "release/2.0", "r2", "cr2")

	// 目标侧:release/1.0 有一个源端没有的提交(分歧)
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	gitRun(t, dstWork, "checkout", "-q", "release/1.0")
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "only-target.txt"), []byte("t"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	// 只推到目标裸仓(推到源端会把"目标独有提交"也带进源端 ⇒ 分歧被抹掉)
	gitRun(t, dstWork, "push", "-q", dstBare, "release/1.0:release/1.0")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "multi-div", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, ForcePushPolicy: "block", KeepDivergent: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 首次 clone 之后本地就要有全部分支(否则多分支同步形同虚设)
	for _, b := range []string{"release/1.0", "release/2.0"} {
		_, rerr := exec.backend.RevParse(ctx, work, "refs/heads/"+b)
		require.NoError(t, rerr, "多分支首次 clone 后本地应建出 %s", b)
	}

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.Error(t, err, "多分支 + block 策略:目标侧有独有提交时必须被拦下")
	assert.True(t, isDivergent(err), "应是分歧保护错误,实际=%v", err)
	assert.Contains(t, err.Error(), "release/1.0", "错误要点名是哪个分支分歧")

	// 目标侧那个独有提交必须还在(没被覆盖)
	out := gitOut(t, dstBare, "log", "--oneline", "release/1.0")
	assert.Contains(t, out, "target-only", "目标侧独有提交不该被覆盖")
}

// TestPush_MultiBranch_BlockPolicyAllowsWhenNoDivergence 没有分歧时,
// 多分支同步必须**正常推上去**(别把防护做成"一律拦")。
func TestPush_MultiBranch_BlockPolicyAllowsWhenNoDivergence(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "release/1.0", "r1", "cr1")
	commitBranchToSrc(t, src, "release/2.0", "r2", "cr2")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "multi-ok", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, ForcePushPolicy: "block", KeepDivergent: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)

	branches := targetBranches(t, dstBare)
	assert.Contains(t, branches, "release/1.0")
	assert.Contains(t, branches, "release/2.0", "多分支的两个分支都该推上去,实际=%v", branches)
}

// TestPush_SingleBranch_BlockPolicyFailsClosedWhenCompareFails 目标分支存在、
// 却**算不出**分歧时必须 fail-closed(报错),不能放行。
//
// 回归背景:旧实现"算不出来就保守放行,交给 push 决定" —— 可 force 推送
// **正是**会覆盖目标侧独有提交的那一步,交给 push 决定 = 没有保护。
func TestPush_SingleBranch_BlockPolicyFailsClosedWhenCompareFails(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "fail-closed", SourceBranch: "main", TargetBranch: "main",
		GitForce: true, ForcePushPolicy: "block", KeepDivergent: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 目标端**不可达**,但本地已有该远端跟踪 ref(说明目标仓确实有这个分支)
	// ⇒ 必须 fail-closed。
	gitRun(t, work, "push", "-q", dstBare, "refs/heads/main:refs/heads/main")
	gitRun(t, work, "fetch", "-q", "target", "main")
	require.NotEmpty(t, gitOut(t, work, "rev-parse", "refs/remotes/target/main"),
		"前置条件:本地应已留下目标分支的远端跟踪 ref")
	gitRun(t, work, "remote", "set-url", "target", filepath.Join(t.TempDir(), "does-not-exist.git"))

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.Error(t, err, "目标端已知存在却连不上时必须报错,不能放行")
	assert.Contains(t, err.Error(), "cannot verify divergence")
}
