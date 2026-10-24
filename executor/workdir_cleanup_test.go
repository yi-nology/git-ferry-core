package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestExecute_FailedRun_KeepsUsableWorkdir 失败时**保留**可用的工作区。
//
// 回归背景:失败即删整个 workdir ⇒ 下一次执行被迫全量克隆,
// "增量 fetch"(大仓的主要成本)收益在第一次失败后归零。
// 而失败常常与本地状态无关(推送被分歧保护拦下、鉴权失败、网络抖动)。
func TestExecute_FailedRun_KeepsUsableWorkdir(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	commitToSrc(t, src, "v1", "c1")

	// 目标侧有独有提交 + block 策略 ⇒ 推送会被拦下 ⇒ 本次执行失败
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

	task := &model.SyncTask{
		Key: "keep-workdir", SourceRepoKey: "source-repo", TargetRepoKey: "target-repo",
		SourceBranch: "main", TargetBranch: "main", SyncMode: "single",
		GitForce: true, KeepDivergent: true, ForcePushPolicy: "block",
	}

	_, _ = exec.Execute(context.Background(), task, "manual", nil)

	repoDir := filepath.Join(tempDir, RepoDir)
	require.DirExists(t, repoDir, "失败后应保留可用的工作区(下一次走增量 fetch)")
	// 工作区要真的是个能用的仓库
	_, err = exec.backend.RevParse(context.Background(), repoDir, "HEAD")
	require.NoError(t, err, "保留的工作区必须可用")
}

// TestExecute_FailedRun_RemovesBrokenWorkdir 工作区本身是坏的(克隆没成功/被中断)
// ⇒ 必须删掉,否则下一次执行会在坏仓库上继续失败。
func TestExecute_FailedRun_RemovesBrokenWorkdir(t *testing.T) {
	tempDir := t.TempDir()
	svc := newMockService()
	svc.tempDir = tempDir
	svc.repos = map[string]*model.Repo{
		"source-repo": {Key: "source-repo", Name: "src", CloneURL: filepath.Join(tempDir, "missing.git"), DefaultBranch: "main"},
		"target-repo": {Key: "target-repo", Name: "dst", CloneURL: filepath.Join(tempDir, "missing.git"), DefaultBranch: "main"},
	}
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{
		Key: "broken-workdir", SourceRepoKey: "source-repo", TargetRepoKey: "target-repo",
		SourceBranch: "main", TargetBranch: "main", SyncMode: "single",
	}

	_, _ = exec.Execute(context.Background(), task, "manual", nil)

	repoDir := filepath.Join(tempDir, RepoDir)
	_, statErr := os.Stat(repoDir)
	require.True(t, os.IsNotExist(statErr),
		"坏工作区必须删掉(克隆失败后留下的半成品会让下一次执行继续失败): %s", repoDir)
}
