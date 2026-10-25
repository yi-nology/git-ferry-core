package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitOnTop 在工作区加一个文件并提交,返回新提交的 hash。
func commitOnTop(t *testing.T, repo *git.Repository, wt *git.Worktree, file, content string) plumbing.Hash {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(wt.Filesystem.Root(), file), []byte(content), 0o600))
	_, err := wt.Add(file)
	require.NoError(t, err)
	sig := &object.Signature{Name: "t", Email: "t@local"}
	h, err := wt.Commit("c "+file, &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return h
}

// TestClassifyRemote_ConsistentWithPushGate 远端关系的判定必须只有一份口径。
//
// 回归背景(P1,报告与事实相反):预检(checkTargetImpact)与推送闸门
// (checkPushDivergence)各写一份分叉判定,口径相反:
//
//	① 远端**落后**于基线(推送闸门明确放行:推过去是快进)被预检报成
//	   「已分叉、推送需要强推」⇒ NeedsForce 把一次本可安全完成的同步**卡住**;
//	② 远端**已经是本次产物**(重复发布)也被报成「已分叉需强推」。
//
// 这条用例是**反向验证**:把 applyRemoteRelation 的映射改回"除快进外一律
// Diverged",本用例会变红。
func TestClassifyRemote_ConsistentWithPushGate(t *testing.T) {
	dir := t.TempDir()
	makeHistoryRepo(t, dir, map[string]string{"c0.txt": "0"})
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)

	c0 := mustHead(t, repo)
	c1 := commitOnTop(t, repo, wt, "c1.txt", "1")
	c2 := commitOnTop(t, repo, wt, "c2.txt", "2")
	c3 := commitOnTop(t, repo, wt, "c3.txt", "3")

	// 无关历史:另起一个没有父的提交
	unrelated := commitOrphan(t, repo, "orphan.txt", "x")

	base := c1.String()
	product := c2.String()

	cases := []struct {
		name       string
		remote     string
		wantRel    remoteRelation
		wantForce  bool
		wantFF     bool
		wantDiver  bool
		wantInNote string
	}{
		{name: "远端==基线(快进)", remote: base, wantRel: remoteSameAsBase, wantFF: true},
		{name: "远端==本次产物(重复发布)", remote: product, wantRel: remoteSameAsProduct},
		{name: "远端领先(基线是远端的祖先)", remote: c3.String(), wantRel: remoteAhead, wantForce: true, wantDiver: true},
		{name: "远端落后(远端是基线的祖先)", remote: c0.String(), wantRel: remoteBehind, wantFF: true},
		{name: "历史无关", remote: unrelated.String(), wantRel: remoteUnrelated, wantForce: true, wantDiver: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel, note := classifyRemote(repo, c.remote, base, product)
			assert.Equal(t, c.wantRel, rel)
			assert.Equal(t, c.wantForce, needForceForRelation(rel),
				"是否需要强推必须与推送闸门一致")

			imp := TargetImpact{}
			applyRemoteRelation(&imp, rel, note)
			assert.Equal(t, c.wantFF, imp.FastForward, "快进标记")
			assert.Equal(t, c.wantDiver, imp.Diverged, "分叉标记(为真会卡住同步)")
			assert.NotEmpty(t, imp.Note, "必须给出说明")
		})
	}
}

// TestApplyRemoteRelation_RemoteBehindIsNotBlocked 远端落后**不得**被判分叉。
//
// 回归背景:预检以前把"远端落后"报成 Diverged ⇒ report.NeedsForce=true ⇒
// Force=false 时直接不推 ⇒ 一次本可安全完成的同步被卡住。
func TestApplyRemoteRelation_RemoteBehindIsNotBlocked(t *testing.T) {
	imp := TargetImpact{}
	applyRemoteRelation(&imp, remoteBehind, "远端落后于本地基线,推送会把它往前推")
	assert.False(t, imp.Diverged, "远端落后不是分叉,不得卡住同步")
	assert.True(t, imp.FastForward, "远端落后时推送是快进")

	imp2 := TargetImpact{}
	applyRemoteRelation(&imp2, remoteSameAsProduct, "远端已经是本次移植的结果")
	assert.False(t, imp2.Diverged, "重复发布不是分叉,不得卡住同步")
}

func mustHead(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	head, err := repo.Head()
	require.NoError(t, err)
	return head.Hash()
}

// commitOrphan 造一个没有父提交的历史(与其它历史无关)。
func commitOrphan(t *testing.T, repo *git.Repository, file, content string) plumbing.Hash {
	t.Helper()
	blobObj := repo.Storer.NewEncodedObject()
	blobObj.SetType(plumbing.BlobObject)
	w, err := blobObj.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	blobHash, err := repo.Storer.SetEncodedObject(blobObj)
	require.NoError(t, err)

	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: file, Mode: filemode.Regular, Hash: blobHash},
	}}
	treeObj := repo.Storer.NewEncodedObject()
	require.NoError(t, tree.Encode(treeObj))
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	require.NoError(t, err)

	sig := object.Signature{Name: "t", Email: "t@local"}
	commit := &object.Commit{Author: sig, Committer: sig, Message: "orphan", TreeHash: treeHash}
	cObj := repo.Storer.NewEncodedObject()
	require.NoError(t, commit.Encode(cObj))
	h, err := repo.Storer.SetEncodedObject(cObj)
	require.NoError(t, err)
	return h
}
