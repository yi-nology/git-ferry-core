package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// makeHistoryRepo 造一个仓库,写入若干文件并提交到 main 分支。
func makeHistoryRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	for path, content := range files {
		full := filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true}))
	sig := &object.Signature{Name: "t", Email: "t@local", When: time.Unix(1700000000, 0)}
	_, err = w.Commit("init", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	// 分支名固定为 main:目标侧克隆时按 refs/heads/main 取。
	head, err := r.Head()
	require.NoError(t, err)
	require.NoError(t, r.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"), head.Hash())))
}

// TestHistoryChannelLockSerializesSameChannel 回归:同一通道的写操作必须互斥。
//
// 同一通道并发执行会破坏哈希链:执行记录靠「取上一条已入链记录的哈希」续接,
// 两个并发操作会拿到同一个 prev —— 链分叉,校验时报「prev_hash mismatch」。
func TestHistoryChannelLockSerializesSameChannel(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	release1, err := h.lockChannel(ctx, 42)
	require.NoError(t, err)

	// 同一通道:第二次取锁必须等不到(而不是拿到第二把锁)。
	tctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	_, err = h.lockChannel(tctx, 42)
	require.Error(t, err, "同一通道的第二次取锁必须被挡住")

	// 不同通道:互不阻塞。
	release2, err := h.lockChannel(ctx, 43)
	require.NoError(t, err)
	release2()

	release1()

	// 释放后可以再取到。
	release3, err := h.lockChannel(ctx, 42)
	require.NoError(t, err)
	release3()
}

// TestHistoryApplyConcurrentKeepsChainIntact 回归:并发执行同一通道,
// 执行记录链必须仍然完整(不出现 prev_hash mismatch)。
func TestHistoryApplyConcurrentKeepsChainIntact(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")
	makeHistoryRepo(t, srcDir, map[string]string{
		"go.mod": "module git.enjoye.top/enjoydream/app\n",
		"new.go": "package app\n",
	})
	makeHistoryRepo(t, tgtDir, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "concurrent", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		TargetBranch: strPtr("main"), SourceBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.HistoryApply(ctx, ch.ID)
		}(i)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	require.Positive(t, ok, "至少要有一次执行成功")

	res, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, res.OK, "并发执行后链必须完整:"+res.Message)
	require.Equal(t, ok, res.Checked, "链上条目数必须等于成功执行数")
}
