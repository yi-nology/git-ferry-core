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

// targetBranches 列目标裸仓的分支名。
func targetBranches(t *testing.T, dstBare string) []string {
	t.Helper()
	out := gitOut(t, dstBare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	var res []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

// commitBranchToSrc 在源端新建一个**非默认**分支并推送。
//
// 用非默认分支是因为:裸仓默认拒绝删除 HEAD 指向的分支
// (receive.denyDeleteCurrent),而"源端删掉分支"正是这几个用例要模拟的场景。
func commitBranchToSrc(t *testing.T, src, branch, body, msg string) {
	t.Helper()
	gitRun(t, src, "checkout", "-b", branch)
	require.NoError(t, os.WriteFile(filepath.Join(src, "b.txt"), []byte(body), 0o640))
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-m", msg)
	gitRun(t, src, "push", "origin", branch)
}

// TestPush_PruneKeepsBranchWhenSourceAlive 源端还有分支时,
// **绝不能**删目标侧同名分支。
//
// 回归背景(P0):push 成功后无条件执行 pruneRemoteBranch(推 ":refs/heads/<tgt>",
// git 语义 = 删除该分支)—— 也就是"推上去紧接着删掉"。
// 任何勾了 git_push_prune 的单分支任务,每次同步结束目标侧该分支都没了,
// 而任务报 success(数据在目标侧消失,报告却说成功)。
//
// 语义(git_push_prune 的原意)="目标上**源已不存在**的同名分支一并删掉"。
func TestPush_PruneKeepsBranchWhenSourceAlive(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "feature/x", "b1", "cb1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "prune-alive", SourceBranch: "feature/x", TargetBranch: "feature/x",
		GitPushPrune: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)

	// 目标侧必须有该分支(不该被 prune 掉)
	branches := targetBranches(t, dstBare)
	assert.Contains(t, branches, "feature/x", "同步后目标侧必须有该分支(不该被 prune 掉)")
	require.Len(t, branches, 1, "目标侧不该多出/少掉分支,实际=%v", branches)
}

// TestPush_PruneKeepsDefaultBranchWhenSourceAlive 目标侧默认分支(HEAD)也不该被 prune。
//
// 这一条单列是因为它的失败形态不同:目标裸仓的 HEAD 指向该分支时,
// git 拒绝删除当前分支 ⇒ 旧实现会让**每次同步都失败**
// ("By default, deleting the current branch is denied")——
// 任务被配置成永远跑不成功,同样是一种"坏了但没人知道"。
func TestPush_PruneKeepsDefaultBranchWhenSourceAlive(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "prune-head", SourceBranch: "main", TargetBranch: "main",
		GitPushPrune: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err, "同步必须成功(旧实现会在这里被 prune 失败拖成错误)")
	assert.Contains(t, targetBranches(t, dstBare), "main", "目标侧默认分支不该被删")
}

// TestPush_PruneDeletesBranchWhenSourceGone 源端删掉分支后,
// 目标侧同名分支要被删掉(这才是 git_push_prune 的语义)。
//
// 同时钉住"源端删了分支就不能再把本地残留推回去" ——
// 源端已经删掉的分支,目标侧不该"复活"它。
func TestPush_PruneDeletesBranchWhenSourceGone(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "feature/x", "b1", "cb1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "prune-gone", SourceBranch: "feature/x", TargetBranch: "feature/x",
		GitPushPrune: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 先正常同步一次,目标侧有 main
	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)
	require.Contains(t, targetBranches(t, dstBare), "feature/x")

	// 源端删掉 main 并推送
	gitRun(t, src, "push", "origin", "--delete", "feature/x")
	// 源端裸仓确实没有 feature/x 了
	require.NotContains(t, targetBranches(t, srcBare), "feature/x")

	// 增量 fetch:源端分支没了,fetch 会报 "couldn't find remote ref" ⇒
	// 必须被识别成"源端删了"(镜像删除),而不是整次同步失败。
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil),
		"源端删除分支不应让整次同步失败(应走镜像删除)")

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)

	assert.NotContains(t, targetBranches(t, dstBare), "feature/x",
		"源端已删除该分支,目标侧同名分支应被删掉(镜像删除)")
}

// TestPush_SourceGone_NoPruneKeepsTarget 没开 git_push_prune 时,
// 源端删了分支也不该动目标侧(不删也不推)。
func TestPush_SourceGone_NoPruneKeepsTarget(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "feature/x", "b1", "cb1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "prune-off", SourceBranch: "feature/x", TargetBranch: "feature/x",
		GitPushPrune: false,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))
	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)
	require.Contains(t, targetBranches(t, dstBare), "feature/x")

	// 源端删掉 main
	gitRun(t, src, "push", "origin", "--delete", "feature/x")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))
	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)

	assert.Contains(t, targetBranches(t, dstBare), "feature/x",
		"未开 git_push_prune 时目标侧应保持不动")
}

// TestPush_SourceGone_DoesNotResurrectBranch 源端删了分支后,
// **不能**把本地残留的分支推回目标(源端已删,目标侧不该复活它)。
func TestPush_SourceGone_DoesNotResurrectBranch(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "feature/x", "b1", "cb1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "no-resurrect", SourceBranch: "feature/x", TargetBranch: "feature/x",
		GitPushPrune: false,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 源端删掉 main(但本地工作区还留着 refs/heads/main 这个残留)
	gitRun(t, src, "push", "origin", "--delete", "feature/x")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 本地确实还留着那个分支(这正是"残留"的来源)
	_, err = exec.backend.RevParse(ctx, work, "refs/heads/feature/x")
	require.NoError(t, err, "前提:本地还留着残留分支")

	_, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)

	// 目标侧不该出现 main(源端已经删了)
	assert.NotContains(t, targetBranches(t, dstBare), "feature/x",
		"源端已删除的分支不能被本地残留推回目标")
	_ = os.Getenv("HOME")
}
