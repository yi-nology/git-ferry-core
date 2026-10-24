package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGateBlocksApplyAndPush 回归:编译不过的树**不许推出去**。
//
// 背景(第四轮实测):apply 与 push 两条路径**都不跑类型门禁**,
// apply 只往执行记录里写一句占位文字("见物化目录的 gofmt 校验"),
// push 完全不看门禁 —— 于是「源仓里有编译不过的文件」照样被推到公开仓,
// 而报告看上去一切正常。预检路径(one-shot sync)倒是拦得住,于是形成
// 「预检拦得住、两步法拦不住」的错觉。
//
// 现在门禁跑在**产物**上,结论写进执行记录,推送前重新核对:
//   - 门禁不通过 → apply 状态 failed,推送被拒;
//   - 产物在 apply 之后被改动 → 推送被拒。
func TestGateBlocksApplyAndPush(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")
	remoteDir := filepath.Join(root, "remote.git")

	makeHistoryRepo(t, srcDir, map[string]string{
		"go.mod": "module git.enjoye.top/enjoydream/app\n",
		"ok.go":  "package app\n\nfunc Good() string { return \"ok\" }\n",
		// 类型错误:返回值与声明不符。门禁必须抓住它。
		"bad.go": "package app\n\nfunc Bad() string { return 42 }\n",
	})
	makeHistoryRepo(t, tgtDir, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})
	makeBareRepo(t, remoteDir)
	seedRemoteFrom(t, remoteDir, tgtDir, "main")

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "gateguard", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceBranch: strPtr("main"), TargetBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)
	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: remoteDir,
	})
	require.NoError(t, err)

	// apply:门禁不通过 → 状态必须是 failed,门禁结论必须落进执行记录。
	applyRes, err := h.HistoryApply(ctx, ch.ID)
	require.NoError(t, err, "apply 本身不报错(失败信息在记录里)")
	require.Equal(t, "failed", applyRes.Status,
		"门禁不通过的移植不能算成功:applyRes.Error=%s", applyRes.Error)
	require.Contains(t, applyRes.Error, "门禁未通过")

	var gate historyGateResult
	require.NoError(t, json.Unmarshal([]byte(applyRes.GateReport), &gate),
		"门禁结论必须以结构化形式落进执行记录")
	assert.False(t, gate.OK, "门禁结论必须记为不通过")
	assert.NotEmpty(t, gate.TypeCheckErr, "必须留下类型检查失败原因")
	assert.NotEmpty(t, gate.Head, "门禁结论必须记下它验的是哪棵树")

	// push:必须被拒绝。
	_, err = h.HistoryPush(ctx, ch.ID)
	require.Error(t, err, "门禁不通过的产物不许推送")
	require.Contains(t, err.Error(), "门禁")

	// 远端必须还是原样:编译不过的文件不能落到公开仓。
	repo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	head, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.FindEntry("bad.go")
	require.Error(t, err, "编译不过的文件不能被推到公开仓")
}

// TestGateBlocksPushWhenProductTampered 回归:apply 之后产物被改动,
// 推送必须被拒绝 —— 门禁验的是 apply 那一刻的树,不是现在这棵。
//
// 少了这道核对,「apply 通过 → 有人改了产物 → push」会把一棵
// 门禁从没验过的树推出去。
func TestGateBlocksPushWhenProductTampered(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")
	remoteDir := filepath.Join(root, "remote.git")

	makeHistoryRepo(t, srcDir, map[string]string{
		"go.mod": "module git.enjoye.top/enjoydream/app\n",
		"ok.go":  "package app\n\nfunc Good() string { return \"ok\" }\n",
	})
	makeHistoryRepo(t, tgtDir, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})
	makeBareRepo(t, remoteDir)
	seedRemoteFrom(t, remoteDir, tgtDir, "main")

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "tamper", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceBranch: strPtr("main"), TargetBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)
	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: remoteDir,
	})
	require.NoError(t, err)

	applyRes, err := h.HistoryApply(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, "success", applyRes.Status, "正常内容应当通过门禁:applyRes.Error=%s", applyRes.Error)

	// 产物落地后,有人在产物仓里又提交了一次(内容编译不过)。
	stored, err := h.runs.FindByID(applyRes.ID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.WorkRepoPath)
	tamperProduct(t, stored.WorkRepoPath)

	_, err = h.HistoryPush(ctx, ch.ID)
	require.Error(t, err, "产物被改动后推送必须被拒绝")
	require.Contains(t, err.Error(), "不一致")

	// 远端不能被推进到被篡改后的 HEAD。
	repo, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	head, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.NotEqual(t, applyRes.Head, head.Hash().String(),
		"被篡改的产物不能推上远端")
}

// tamperProduct 在产物仓里造一个新提交(模拟 apply 之后产物被改动)。
func tamperProduct(t *testing.T, repoPath string) {
	t.Helper()
	r, err := git.PlainOpen(repoPath)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "bad.go"),
		[]byte("package app\n\nfunc Bad() string { return 42 }\n"), 0o644))
	_, err = wt.Add("bad.go")
	require.NoError(t, err)
	sig := &object.Signature{Name: "tamper", Email: "t@local", When: time.Unix(1700009999, 0)}
	_, err = wt.Commit("tamper", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
}
