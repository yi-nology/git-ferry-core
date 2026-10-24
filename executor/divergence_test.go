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

	_, err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
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

	_, err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
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

	_, err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
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
	_, err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
	require.NoError(t, err)
}

// stubApprover 记录 executor 对审批器的调用。
type stubApprover struct {
	approved  bool
	consumed  int
	requested int
	// checkedTargetRef 记录最近一次判定时传入的目标 SHA(验证"放行绑定目标状态")
	checkedTargetRef string
}

func (s *stubApprover) IsForcePushApproved(taskKey, branch, targetRef string) bool {
	s.checkedTargetRef = targetRef
	return s.approved
}
func (s *stubApprover) RequestApproval(taskKey, branch, reason, targetRef string) { s.requested++ }

// ConsumeForcePushApproval 镜像真实 store 的语义:用掉之后不再算已放行。
func (s *stubApprover) ConsumeForcePushApproval(taskKey, branch string) {
	s.consumed++
	s.approved = false
}

// TestCheckDivergencePolicy_ReportsApprovalUsage 分歧保护命中人工放行时,
// 必须把"用掉了这一次放行"如实报给调用方 —— 调用方据此在推送成功后兑现"一次性"。
//
// 回归背景:契约是"Admin 一次性放行后允许**本次** force";
// 若检查只放行不报使用情况,调用方无从消费 ⇒ 放行变成常驻后门。
func TestCheckDivergencePolicy_ReportsApprovalUsage(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	// 目标侧有独有提交 ⇒ 会走分歧分支
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "y.txt"), []byte("y"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	gitRun(t, dstWork, "push", "-q", dstBare, "main")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "approval-usage", SourceBranch: "main", TargetBranch: "main"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	commitToSrc(t, src, "v2", "c2")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 没有放行 ⇒ 拦下
	used, err := exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
	require.Error(t, err)
	require.False(t, used, "没放行时不该报'用掉了放行'")

	// 有放行 ⇒ 放行通过,且如实上报"用掉了"
	ap := &stubApprover{approved: true}
	exec.Approver = ap
	used, err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "block", "", task.Key, dstRepo, nil)
	require.NoError(t, err, "有放行时应当放行")
	require.True(t, used, "用了放行必须上报(调用方据此兑现一次性)")
	require.Equal(t, 0, ap.consumed, "检查阶段不消费放行 —— 消费发生在推送成功之后")
}

// TestCheckDivergencePolicy_BackupOnDemandWithoutBackupDir_ExplainsWhy
// backup_on_demand 但没配 sync.backup_dir 时,错误文案必须**说清楚为什么**。
//
// 回归背景:此前静默按 block 处理 ⇒ 策略与实际行为不符(操作者以为会
// "先快照再覆盖",实际被拦下),而且看不出原因,像是工具随机拦的。
func TestCheckDivergencePolicy_BackupOnDemandWithoutBackupDir_ExplainsWhy(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "y.txt"), []byte("y"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	gitRun(t, dstWork, "push", "-q", dstBare, "main")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "backup-on-demand", SourceBranch: "main", TargetBranch: "main"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	commitToSrc(t, src, "v2", "c2")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// backupDir 传空串 = 未配置 sync.backup_dir
	used, err := exec.checkDivergencePolicy(ctx, work, "main", "main", true, "backup_on_demand", "", task.Key, dstRepo, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sync.backup_dir",
		"backup_on_demand 兑现不了时,错误必须点名 backup_dir,否则看不出为什么被拦下")
	require.False(t, used)

	// 关键:不能落 pending 审批 —— 否则运维一次"放行"就会在**没有备份**的情况下
	// 覆盖目标侧独有提交,那正是这个策略要防的数据丢失。
	ap := &stubApprover{}
	exec.Approver = ap
	_, err = exec.checkDivergencePolicy(ctx, work, "main", "main", true, "backup_on_demand", "", task.Key, dstRepo, nil)
	require.Error(t, err)
	require.Equal(t, 0, ap.requested, "配置错时不得产生审批申请(否则能被放行成无备份覆盖)")
}
