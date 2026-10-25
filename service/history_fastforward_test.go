package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// TestFastForwardMirrorBranches_AdvancesLocalBranch 拉取之后本地分支必须快进。
//
// 回归背景(P0,历史同步第二次起就发不出去):gitbackend.Fetch 只更新
// refs/remotes/origin/*,**不动本地 refs/heads/*** ⇒ 镜像目录里的本地分支永远
// 停在首次克隆时。而差异计算与基线判定读的都是本地分支 ⇒
// ① 已发布的文件每次仍判"需移植";② 第二次起的发布一律被判"远端已分叉"拦下
// (只能强推,强推会丢掉远端上的人工提交)。
//
// 这条用例是**反向验证**:把 fastForwardMirrorBranches 去掉,本地分支停在旧提交
// ⇒ 断言失败。
func TestFastForwardMirrorBranches_AdvancesLocalBranch(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")
	workDir := filepath.Join(root, "work")

	makeHistoryRepo(t, seedDir, map[string]string{"a.txt": "a"})
	makeBareRepo(t, remoteDir)
	seedRemoteFrom(t, remoteDir, seedDir, "main")
	setBareHeadToMain(t, remoteDir)

	// 克隆成镜像目录
	_, err := git.PlainClone(workDir, false, &git.CloneOptions{URL: remoteDir})
	require.NoError(t, err)

	// 远端前进一个提交
	commitToBare(t, remoteDir, "main", "b.txt", "b")

	// 生产路径同款:fetch 只更新 refs/remotes/origin/*
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{})
	require.NoError(t, err)
	_, err = backend.Fetch(t.Context(), gitbackend.FetchOptions{
		RepoPath: workDir, Remote: "origin", Tags: true,
	})
	require.NoError(t, err)

	localBefore := branchHash(t, workDir, "main")
	remoteNow := bareBranchHash(t, remoteDir, "main")
	assert.NotEqual(t, remoteNow, localBefore,
		"前提:fetch 之后本地分支仍是旧的(只有远端跟踪 ref 前进了)")

	// 快进
	require.NoError(t, fastForwardMirrorBranches(workDir, "origin"))

	assert.Equal(t, remoteNow, branchHash(t, workDir, "main"),
		"快进后本地分支必须等于远端 HEAD(否则基线永远陈旧)")
}

// TestFastForwardMirrorBranches_KeepsDivergedLocal 快进**只**处理可快进的情况。
//
// 本地有远端没有的提交(分叉)时不能动本地分支 —— 那种情况要交给推送闸门报告,
// 不能在这里悄悄改基线。
func TestFastForwardMirrorBranches_KeepsDivergedLocal(t *testing.T) {
	root := t.TempDir()
	remoteDir := filepath.Join(root, "remote.git")
	seedDir := filepath.Join(root, "seed")
	workDir := filepath.Join(root, "work")

	makeHistoryRepo(t, seedDir, map[string]string{"a.txt": "a"})
	makeBareRepo(t, remoteDir)
	seedRemoteFrom(t, remoteDir, seedDir, "main")
	setBareHeadToMain(t, remoteDir)

	repo, err := git.PlainClone(workDir, false, &git.CloneOptions{URL: remoteDir})
	require.NoError(t, err)

	// 远端前进
	commitToBare(t, remoteDir, "main", "b.txt", "b")

	// 本地另起一个提交(与远端分叉)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "local.txt"), []byte("l"), 0o600))
	_, err = wt.Add("local.txt")
	require.NoError(t, err)
	sig := &object.Signature{Name: "l", Email: "l@local"}
	localCommit, err := wt.Commit("local only", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	backend, err := gitbackend.NewGitBackend(gitbackend.Options{})
	require.NoError(t, err)
	_, err = backend.Fetch(t.Context(), gitbackend.FetchOptions{
		RepoPath: workDir, Remote: "origin", Tags: true,
	})
	require.NoError(t, err)

	require.NoError(t, fastForwardMirrorBranches(workDir, "origin"))

	assert.Equal(t, localCommit.String(), branchHash(t, workDir, "main"),
		"本地与远端分叉时不得改动本地分支(该由推送闸门报告)")
}

// setBareHeadToMain 让裸仓的 HEAD 指向 main(裸仓默认是 master)。
func setBareHeadToMain(t *testing.T, dir string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))))
}

// branchHash 读本地目录里某分支的提交。
func branchHash(t *testing.T, dir, branch string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	require.NoError(t, err)
	return ref.Hash().String()
}

// bareBranchHash 读裸仓库里某分支的提交。
func bareBranchHash(t *testing.T, dir, branch string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	require.NoError(t, err)
	return ref.Hash().String()
}
