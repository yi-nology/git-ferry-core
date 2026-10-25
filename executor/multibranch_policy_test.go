package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// multiDivergentLab 造一个"多分支 + 目标侧有独有提交"的场景。
// 返回:源裸仓、目标裸仓、工作目录。
func multiDivergentLab(t *testing.T) (srcBare, dstBare, work string) {
	t.Helper()
	src, srcBare, dstBare := newLocalSyncLab(t)
	work = filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "release/1.0", "r1", "cr1")
	commitBranchToSrc(t, src, "release/2.0", "r2", "cr2")

	// 目标侧 release/1.0 有一个源端没有的提交(分歧)
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	gitRun(t, dstWork, "checkout", "-q", "release/1.0")
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "only-target.txt"), []byte("t"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	gitRun(t, dstWork, "push", "-q", dstBare, "release/1.0:release/1.0")
	return srcBare, dstBare, work
}

// TestPush_MultiBranch_TargetFetchFails_FailsClosed 目标端 fetch 失败时必须 fail-closed。
//
// 回归背景(P0,本轮自查发现):多分支路径调 fetchTargetForDivergence(nil) ——
// 而"目标端是否已知存在"的判定用的是**同一个参数**,nil ⇒ 循环体一次都不执行
// ⇒ 恒返回 false ⇒ fetch 失败被当成"目标端没有这些分支(首次推送)"放行。
// 紧接着逐分支判定里 RevParse("refs/remotes/target/<b>") 也失败(从没 fetch 过)
// ⇒ 同样放行 ⇒ **目标侧独有提交被静默覆盖**,而 fail-closed 成了死代码。
//
// 现实触发:目标端 ref 过多/超大、镜像里有 broken ref、读被限流而写正常。
func TestPush_MultiBranch_TargetFetchFails_FailsClosed(t *testing.T) {
	srcBare, dstBare, work := multiDivergentLab(t)

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "multi-fetch-fail", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, ForcePushPolicy: "block",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))
	// 先成功 fetch 一次目标端,留下 refs/remotes/target/* (⇒ 目标分支"已知存在")
	_, ferr := exec.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: work, Remote: "target",
	})
	require.NoError(t, ferr)
	require.NotEmpty(t, gitOut(t, work, "rev-parse", "refs/remotes/target/release/1.0"),
		"前置条件:本地应已留下目标分支的远端跟踪 ref")

	// 把 target remote 指向不存在的路径 ⇒ 这次 fetch 必然失败
	gitRun(t, work, "remote", "set-url", "target", filepath.Join(t.TempDir(), "does-not-exist.git"))

	_, _, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.Error(t, err, "目标端 fetch 失败且目标分支已知存在 ⇒ 必须 fail-closed,不能放行覆盖")

	// 目标侧独有提交必须还在
	out := gitOut(t, dstBare, "log", "--oneline", "release/1.0")
	assert.Contains(t, out, "target-only", "目标侧独有提交不该被覆盖")
}

// TestPush_MultiBranch_AllowPolicy_AllowsForcePush 策略 allow 时必须**放行**。
//
// 回归背景(P1,本轮自查发现):多分支路径直接调 checkDivergenceForBranch,
// 而策略归一(normalizeDivergencePolicy)只在单分支入口做 ⇒
// "allow" 落到 behind>0 分支 ⇒ 返回 divergentError ⇒
// 显式允许覆盖的任务反而被拒,错误文案还建议"enable keep_divergent=false"
// (而用户已经显式把策略设成了 allow)。
func TestPush_MultiBranch_AllowPolicy_AllowsForcePush(t *testing.T) {
	srcBare, dstBare, work := multiDivergentLab(t)

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "multi-allow", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, ForcePushPolicy: "allow", // 显式允许覆盖
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	_, _, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err, "allow 策略必须放行(目标有独有提交也不拦)")

	// 覆盖确实发生了(force 推送)
	out := gitOut(t, dstBare, "log", "--oneline", "release/1.0")
	assert.NotContains(t, out, "target-only", "allow + force ⇒ 目标独有提交被覆盖")
}

// TestPush_MultiBranch_EmptyPolicy_TreatedAsBlock 空策略必须按最安全的 block 处理。
//
// 回归背景(删除旧版本兼容时):旧的 keep_divergent 布尔字段曾提供一条旁路 ——
// 策略字段没配时,布尔字段说了算(keep_divergent=false ⇒ allow)。
// 该字段已删除,策略空值只能有一个含义:block(最安全)。
//
// 这条用例是**反向验证**:若有人再引入"空策略 ⇒ allow"或"布尔旁路",
// 本用例会变红(目标侧独有提交被覆盖 / 不再报分歧错误)。
func TestPush_MultiBranch_EmptyPolicy_TreatedAsBlock(t *testing.T) {
	srcBare, dstBare, work := multiDivergentLab(t)

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "multi-empty-policy", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, // ForcePushPolicy 留空 ⇒ 必须按 block 处理
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	_, _, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.Error(t, err, "空策略必须按 block 拦下(目标侧有独有提交)")
	assert.True(t, isDivergent(err), "必须是分歧保护错误,实际: %v", err)

	// 目标独有提交必须还在(没被覆盖)
	out := gitOut(t, dstBare, "log", "--oneline", "release/1.0")
	assert.Contains(t, out, "target-only", "空策略不得覆盖目标侧独有提交")
}

// TestPush_MultiBranch_ApprovalIsConsumed 多分支的人工放行**必须被消费掉**。
//
// 回归背景(P1,本轮自查发现):判定时放行按**具体分支**记
// (checkDivergenceForBranch → IsForcePushApproved(taskKey, b, sha)),
// 而消费点用 task.TargetBranch —— 多分支任务里那是 glob("release/*")⇒
// DAO 的 WHERE task_key=? AND branch=? 匹配不到任何行 ⇒ found=false ⇒
// **放行既不被消费、也不报错**,approved 一直挂着 ⇒
// "一次性放行"对多分支退化成常驻后门(同一次授权可反复覆盖)。
func TestPush_MultiBranch_ApprovalIsConsumed(t *testing.T) {
	srcBare, dstBare, work := multiDivergentLab(t)

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)
	approver := &stubApprover{approved: true}
	exec.Approver = approver

	task := &model.SyncTask{
		Key: "multi-approve", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, ForcePushPolicy: "block",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	_, _, approvalBranch, err := exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err, "有人工放行 ⇒ 应放行")
	require.Equal(t, "release/1.0", approvalBranch,
		"必须带回这次放行对应的分支(多分支时 task.TargetBranch 是 glob,不能用)")
	assert.NotEmpty(t, approver.checkedTargetRef,
		"判定应带具体分支的目标 SHA(绑定目标状态)")
}

// TestCheckDivergenceForBranch_UnknownTargetState_FailsClosed 目标端状态
// **未知**(取回失败)时,即使本地查不到目标 ref 也必须 fail-closed。
//
// 回归背景(P0,自查发现):"本地查不到 refs/remotes/target/<b>"既可能是
// "目标端没这个分支",也可能是"目标端连不上/状态未知"——
// 不区分就会把"不知道"当成"没有" ⇒ 放行 ⇒ 目标侧独有提交被静默覆盖。
//
// 场景:新 workdir(本地从没 fetch 过目标端)+ 目标端取回失败。
func TestCheckDivergenceForBranch_UnknownTargetState_FailsClosed(t *testing.T) {
	srcBare, _, work := multiDivergentLab(t)

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "unknown-state", SourceBranch: "release/*", TargetBranch: "release/*",
		GitForce: true, ForcePushPolicy: "block",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	ctx := context.Background()
	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 本地没有目标端的远端跟踪 ref(从没 fetch 过目标端)
	_, rerr := exec.backend.RevParse(ctx, work, "refs/remotes/target/release/1.0")
	require.Error(t, rerr, "前置条件:本地不应有目标端的远端跟踪 ref")

	// 目标端状态未知(targetKnown=false)⇒ 必须报错,不能放行
	used, _, err := exec.checkDivergenceForBranch(ctx, work,
		"release/1.0", "release/1.0", true, "block", "", task.Key, false)
	require.Error(t, err, "目标端状态未知时必须 fail-closed(不能把'不知道'当成'没有')")
	require.False(t, used)

	// 目标端状态**已知**(fetch 成功)且目标端确实没这个分支 ⇒ 首次推送,放行
	used, _, err = exec.checkDivergenceForBranch(ctx, work,
		"release/1.0", "release/1.0", true, "block", "", task.Key, true)
	require.NoError(t, err, "目标端可达且没有该分支 ⇒ 首次推送,应当放行")
	require.False(t, used)
}
