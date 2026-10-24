package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// makeBareRepo 造一个裸仓库(当推送目标的本地替身)。
func makeBareRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(dir, true)
	require.NoError(t, err)
}

// commitToBare 在裸仓库里直接造一个提交(模拟「别人往远端推了东西」)。
//
// 裸仓库没有工作区,所以只能用对象 API 造 blob → tree → commit。
func commitToBare(t *testing.T, dir, branch, path, content string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	blobObj := repo.Storer.NewEncodedObject()
	blobObj.SetType(plumbing.BlobObject)
	writer, err := blobObj.Writer()
	require.NoError(t, err)
	_, err = writer.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	blobHash, err := repo.Storer.SetEncodedObject(blobObj)
	require.NoError(t, err)

	head, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	require.NoError(t, err)
	parent, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	baseTree, err := parent.Tree()
	require.NoError(t, err)

	entries := []object.TreeEntry{}
	if err := baseTree.Files().ForEach(func(f *object.File) error {
		entries = append(entries, object.TreeEntry{
			Name: f.Name, Mode: f.Mode, Hash: f.Hash,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, object.TreeEntry{
		Name: path, Mode: filemode.Regular, Hash: blobHash,
	})
	tree := &object.Tree{Entries: entries}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, tree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	sig := object.Signature{Name: "other", Email: "other@example.com",
		When: time.Unix(1700000001, 0)}
	commit := &object.Commit{
		Author: sig, Committer: sig,
		Message:      "someone else",
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{head.Hash()},
	}
	commitObj := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(commitObj))
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(branch), commitHash)))
}

// TestPushRefusesWhenRemoteMoved 回归:远端出现我们不知道的提交时,
// 推送必须**被拒绝**,而不是强推把对方的提交静默丢掉。
//
// 背景:pushRun 恒 `Force: true`,分叉检测只在预检路径 ——
// 实测:远端别人的提交被强推后剩 0 条,没有任何报错。
func TestPushRefusesWhenRemoteMoved(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")
	remoteDir := filepath.Join(root, "remote.git")

	makeHistoryRepo(t, srcDir, map[string]string{
		"go.mod": "module git.enjoye.top/enjoydream/app\n",
		"new.go": "package app\n",
	})
	makeHistoryRepo(t, tgtDir, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})
	makeBareRepo(t, remoteDir)

	// 远端与目标仓同一历史(发布目标就是目标仓的镜像)。
	seedRemoteFrom(t, remoteDir, tgtDir, "main")

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "pushguard", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		TargetBranch: strPtr("main"), SourceBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: remoteDir,
	})
	require.NoError(t, err)

	// 先 apply(产出待推送的提交)。
	_, err = h.HistoryApply(ctx, ch.ID)
	require.NoError(t, err)

	// 别人往远端推了一个提交。
	commitToBare(t, remoteDir, "main", "other.txt", "someone else's commit\n")

	// 推送必须被拒绝。
	_, err = h.HistoryPush(ctx, ch.ID)
	require.Error(t, err, "远端出现未知提交时推送必须被拒绝")
	require.Contains(t, err.Error(), "分叉")

	// 远端的提交必须还在(没被静默丢掉)。
	repo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	head, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.FindEntry("other.txt")
	require.NoError(t, err, "远端别人的提交不能被静默丢弃")

	// 强制推送**必须有批准者**(admin 角色):入口层校验角色后把批准者填进来。
	// 没有批准者 → core 直接拒绝(免得哪条入口忘了判角色)。
	_, err = h.HistoryPushWithOptions(ctx, ch.ID, HistoryPushOptions{Force: true})
	require.Error(t, err, "没有批准者的强制推送必须被拒绝")
	require.Contains(t, err.Error(), "批准者")

	// 显式指定强制推送(带批准者)时才允许覆盖。
	_, err = h.HistoryPushWithOptions(ctx, ch.ID, HistoryPushOptions{
		Force: true, ForceApprovedBy: "admin",
	})
	require.NoError(t, err, "显式指定强制推送应当允许覆盖")
}

// seedRemoteFrom 把源仓的某个分支灌进裸仓库(当发布目标的本地替身)。
func seedRemoteFrom(t *testing.T, bareDir, srcDir, branch string) {
	t.Helper()
	repo, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: "seed", URLs: []string{srcDir},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Fetch(&git.FetchOptions{
		RemoteName: "seed",
		RefSpecs:   []config.RefSpec{config.RefSpec("refs/heads/" + branch + ":refs/heads/" + branch)},
		Tags:       git.AllTags,
	}))
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(branch), ref.Hash())))
}
