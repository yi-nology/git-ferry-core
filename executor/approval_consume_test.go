package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestExecute_ForcePush_ConsumesApprovalOnce 端到端钉住"放行是一次性的":
// 一次成功的强推之后,这条放行必须被**用掉** —— 同任务同分支再出现分歧时
// 不得继续放行(否则一次人工放行 = 该分支上的常驻后门)。
func TestExecute_ForcePush_ConsumesApprovalOnce(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	commitToSrc(t, src, "v1", "c1")

	// 目标侧有独有提交 ⇒ 触发分歧
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "y.txt"), []byte("y"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-only")
	gitRun(t, dstWork, "push", "-q", dstBare, "main")

	tempDir := t.TempDir()
	svc := newMockService()
	svc.tempDir = tempDir
	svc.repos = map[string]*model.Repo{
		"source-repo": {Key: "source-repo", Name: "src", CloneURL: srcBare, DefaultBranch: "main"},
		"target-repo": {Key: "target-repo", Name: "dst", CloneURL: dstBare, DefaultBranch: "main"},
	}
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	ap := &stubApprover{approved: true}
	exec.Approver = ap

	task := &model.SyncTask{
		Key: "consume-approval", SourceRepoKey: "source-repo", TargetRepoKey: "target-repo",
		SourceBranch: "main", TargetBranch: "main", SyncMode: "single",
		GitForce: true, KeepDivergent: true, ForcePushPolicy: "block",
	}

	// 第一次:有放行 ⇒ 强推成功,且放行被用掉
	_, _ = exec.Execute(context.Background(), task, "manual", nil)
	require.Equal(t, 1, ap.consumed, "一次成功的强推之后必须把放行用掉")
	require.False(t, ap.approved, "用掉之后应当不再算已放行(下一次分歧必须重新审批)")
}

// TestExecute_MultiBranch_ConsumesApprovalOnce 多分支任务的人工放行也必须被用掉。
//
// 回归背景(P1,自查发现):判定时放行按**具体分支**记
// (IsForcePushApproved(taskKey, "<具体分支>", sha)),
// 而消费点用 task.TargetBranch —— 多分支任务里那是 glob("release/*")⇒
// DAO 的 WHERE task_key=? AND branch=? 匹配不到任何行 ⇒ found=false ⇒
// **放行既不被消费、也不报错**,approved 一直挂着 ⇒
// 同一次人工放行可以反复授权覆盖("一次性"退化成常驻后门)。
func TestExecute_MultiBranch_ConsumesApprovalOnce(t *testing.T) {
	srcBare, dstBare, work := multiDivergentLab(t)

	tempDir := t.TempDir()
	svc := newMockService()
	svc.tempDir = tempDir
	svc.repos = map[string]*model.Repo{
		"source-repo": {Key: "source-repo", Name: "src", CloneURL: srcBare, DefaultBranch: "main"},
		"target-repo": {Key: "target-repo", Name: "dst", CloneURL: dstBare, DefaultBranch: "main"},
	}
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	ap := &stubApprover{approved: true}
	exec.Approver = ap

	task := &model.SyncTask{
		Key: "consume-multi", SourceRepoKey: "source-repo", TargetRepoKey: "target-repo",
		SourceBranch: "release/*", TargetBranch: "release/*", SyncMode: "single",
		GitForce: true, KeepDivergent: true, ForcePushPolicy: "block",
	}
	_ = work

	_, _ = exec.Execute(context.Background(), task, "manual", nil)
	require.Equal(t, 1, ap.consumed, "多分支的一次成功强推之后也必须把放行用掉")
	require.Equal(t, "release/1.0", ap.consumedBranch,
		"必须按**具体分支**消费(用 task.TargetBranch 这个 glob 会匹配不到任何记录)")
	require.False(t, ap.approved, "用掉之后应当不再算已放行")
}
