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
