//go:build history_live

package history

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/mirror"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// liveOptions 返回实仓场景的选项。
func liveOptions(src, tgt string) Options {
	return Options{
		SourceDir: src,
		TargetDir: tgt,
		Rules: []mirror.IdentityRule{
			{Source: "git.enjoye.top/enjoydream/code-review-service",
				Target: "github.com/yi-nology/code-review-service", Kind: mirror.KindModule},
			{Source: "git.enjoye.top", Target: "github.com", Kind: mirror.KindHost},
		},
		PathRules: []PathRule{
			{From: "docs/internal", To: "docs/archive"},
		},
		ScrubRules: []ScrubRule{
			{Name: "internal-host", Category: "internal-host",
				Pattern:  mustPattern(`[a-zA-Z0-9.-]*\.enjoye\.top`),
				Severity: ScrubAuto},
		},
	}
}

// mustPattern 编译正则,失败即 panic(测试夹具)。
func mustPattern(expr string) *regexp.Regexp {
	return regexp.MustCompile(expr)
}

// isAncestor 判断 anc 是否是 desc 的祖先。
func isAncestor(t *testing.T, repo *git.Repository, anc, desc plumbing.Hash) bool {
	t.Helper()
	seen := make(map[plumbing.Hash]bool)
	queue := []plumbing.Hash{desc}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if seen[h] {
			continue
		}
		seen[h] = true
		if h == anc {
			return true
		}
		commit, err := repo.CommitObject(h)
		if err != nil {
			continue
		}
		queue = append(queue, commit.ParentHashes...)
	}
	return false
}

// TestLiveApplyPush 用**本地裸镜像**当远端,端到端验证:
// 计划 → 解法 → 内容移植 → 双远端推送 → 校验既有提交 hash 未变。
//
// 刻意不推 GitHub:推送是对外可见动作,需要显式确认。
// 本地裸镜像能完整验证推送语义(refs 前进、对象可达、幂等重跑)。
func TestLiveApplyPush(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓移植(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
	}

	root := t.TempDir()
	// 裸镜像 = 留底目标;另一个 = 发布目标。先留底后发布。
	backupDir := filepath.Join(root, "backup.git")
	publishDir := filepath.Join(root, "publish.git")
	for _, dir := range []string{backupDir, publishDir} {
		_, err := git.PlainClone(dir, true, &git.CloneOptions{URL: tgt})
		require.NoError(t, err)
	}

	opts := liveOptions(src, tgt)
	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	// 移植前的目标 HEAD 与其提交数。
	before, err := git.PlainOpen(tgt)
	require.NoError(t, err)
	beforeHead, err := before.Head()
	require.NoError(t, err)
	beforeCount, err := before.CommitObjects()
	require.NoError(t, err)
	_ = beforeCount

	applyRes, err := Apply(context.Background(), ApplyOptions{
		SourceDir: src,
		TargetDir: tgt,
		TargetRef: "main",
		Rules:     opts.Rules,
		PathRules: opts.PathRules,
		Plan:      plan,
	})
	require.NoError(t, err)

	t.Logf("移植: 基线 HEAD %s → 新 HEAD %s,新建 %d 个提交,应用 %d 个文件,跳过 %d 个",
		applyRes.BaseHead[:8], applyRes.Head[:8], len(applyRes.Commits),
		applyRes.Files, len(applyRes.Skipped))
	for _, c := range applyRes.Commits {
		t.Logf("  %s → %s  %s  (%d 个文件)",
			c.SourceCommit[:8], c.TargetCommit[:8], c.SourceSubject, len(c.Files))
	}

	// 核心约束:既有提交 hash 不变 —— 旧 HEAD 必须仍是新 HEAD 的祖先。
	workRepo, err := git.PlainOpen(applyRes.RepoPath)
	require.NoError(t, err)
	assert.True(t, isAncestor(t, workRepo, beforeHead.Hash(), plumbing.NewHash(applyRes.Head)),
		"移植前的目标 HEAD 必须是新 HEAD 的祖先(既有提交 hash 不得改变)")

	// 幂等:同一输入两次移植,新提交 hash 必须一致。
	applyRes2, err := Apply(context.Background(), ApplyOptions{
		SourceDir: src,
		TargetDir: tgt,
		TargetRef: "main",
		Rules:     opts.Rules,
		PathRules: opts.PathRules,
		Plan:      plan,
	})
	require.NoError(t, err)
	assert.Equal(t, applyRes.Head, applyRes2.Head, "同一输入两次移植必须产生相同的 HEAD(幂等)")
	assert.Equal(t, applyRes.Commits, applyRes2.Commits, "同一输入两次移植必须产生相同的提交")

	// 推送:先留底后发布。
	pushRes, err := Push(context.Background(), PushOptions{
		RepoPath: applyRes.RepoPath,
		Targets: []PushTarget{
			{Name: "backup", URL: backupDir},
			{Name: "publish", URL: publishDir},
		},
		Branch: "main",
		Auth:   gitbackend.AuthConfig{Type: gitbackend.AuthNone},
		Force:  true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"backup", "publish"}, pushRes.Pushed, "必须先留底后发布")

	// 校验:两个远端的 HEAD 都前进了,且等于移植结果。
	for _, dir := range []string{backupDir, publishDir} {
		repo, err := git.PlainOpen(dir)
		require.NoError(t, err)
		head, err := repo.Head()
		require.NoError(t, err)
		assert.Equal(t, applyRes.Head, head.Hash().String(),
			"远端 %s 的 HEAD 未前进到移植结果", filepath.Base(dir))

		// 既有提交 hash 不变:原 HEAD 仍在历史里。
		commit, err := repo.CommitObject(beforeHead.Hash())
		require.NoError(t, err, "既有提交必须仍在远端历史中")
		assert.Equal(t, beforeHead.Hash().String(), commit.Hash.String())
	}

	// 校验:port 文件已进新 HEAD,preserve 文件逐字节未变。
	newHead, err := workRepo.CommitObject(plumbing.NewHash(applyRes.Head))
	require.NoError(t, err)
	newTree, err := newHead.Tree()
	require.NoError(t, err)
	newFiles, err := flattenTree(newTree)
	require.NoError(t, err)

	for _, path := range []string{"internal/modules/server.go", "internal/readiness/checks_test.go"} {
		_, ok := newFiles[path]
		assert.True(t, ok, "port 文件 %s 应出现在新 HEAD 中", path)
	}
	// 受保护文件:公开树独有的资产必须原样保留。
	for _, path := range []string{"docs/assets/logo.svg", "CODE_OF_CONDUCT.md"} {
		_, ok := newFiles[path]
		assert.True(t, ok, "受保护文件 %s 必须保留", path)
	}

	// 真正的验收:把**推出去的结果**拉下来,校验它 gofmt 干净且能编译。
	verifyPushedResult(t, backupDir, applyRes.Head)

	// 提交消息样例(人工审阅用)。
	if len(applyRes.Commits) > 0 {
		c, err := workRepo.CommitObject(plumbing.NewHash(applyRes.Commits[0].TargetCommit))
		require.NoError(t, err)
		t.Logf("提交消息样例:\n%s", c.Message)
	}
}

// verifyPushedResult 拉取推送后的结果,校验格式与编译。
func verifyPushedResult(t *testing.T, remoteDir, head string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "verify")
	repo, err := git.PlainClone(work, false, &git.CloneOptions{URL: remoteDir})
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{
		Hash:  plumbing.NewHash(head),
		Force: true,
	}))

	unformatted := unformattedFiles(work)
	assert.Empty(t, unformatted, "推送结果存在未格式化文件")

	if err := mirror.TypeCheckGate(context.Background(), work); err != nil {
		t.Errorf("推送结果未通过类型门禁: %v", err)
	} else {
		t.Log("推送结果: gofmt 干净 + 类型门禁通过")
	}
}
