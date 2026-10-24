package mirror

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/go-git-platform/gitbackend"
)

// racingBackend 在**tag 推送成功之后**立刻插入一次"并发发布者"。
//
// 它是 TOCTOU 的确定性复现:发布方在推 tag **之前**判断「远端有没有更高版本」,
// 而并发发布者恰好出现在那之后 ⇒ 发布方手里的"远端最高版本"已经过期。
type racingBackend struct {
	gitbackend.GitBackend
	mu          sync.Mutex
	onTagPushed func()
}

func (b *racingBackend) Push(ctx context.Context, opts gitbackend.PushOptions) (*gitbackend.PushResult, error) {
	res, err := b.GitBackend.Push(ctx, opts)
	if err != nil {
		return res, err
	}
	// 只在推 tag 之后插手(推 main 之前)。
	isTag := false
	for _, rs := range opts.RefSpecs {
		if strings.HasPrefix(rs, "refs/tags/") {
			isTag = true
		}
	}
	if isTag {
		b.mu.Lock()
		fn := b.onTagPushed
		b.onTagPushed = nil
		b.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
	return res, nil
}

// createMirrorTag 在镜像仓里塞一个指定名字的 tag(指向已存在的某个 commit)。
// 用来模拟"另一个发布者刚发布了更高版本"。
func createMirrorTag(t *testing.T, mirrorDir, name string) {
	t.Helper()
	m, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	// 找一个已存在的 commit 当锚点
	var anchor plumbing.Hash
	iter, err := m.Tags()
	require.NoError(t, err)
	ref, err := iter.Next()
	require.NoError(t, err, "镜像仓里应已有 tag")
	require.NotNil(t, ref)
	tagObj, err := m.TagObject(ref.Hash())
	require.NoError(t, err)
	anchor = tagObj.Target
	_, err = m.CreateTag(name, anchor, nil)
	require.NoError(t, err)
}

// TestPublish_MainSnapshotNoRegressionOnConcurrentHigherVersion 并发出现更高版本时,
// 本次发布**不得**把 main 快照推上去 —— 那会把公开仓的默认分支回退到旧版本。
//
// 回归背景:main 快照该指向谁,是在**推 tag 之前**算好的(推完再算,远端已经有
// 本次这个 tag,判断会失真)。但"算"与"推"之间隔着一个网络往返 ——
// 并发发布者正好在这个窗口里发布了更高版本,此时我们再强推 main 就是回退。
//
// 修法:推 main 前**重读**远端 tag 列表(排除本次已推的 tag),
// 发现远端已有更新版本就不动 main。
func TestPublish_MainSnapshotNoRegressionOnConcurrentHigherVersion(t *testing.T) {
	dir, mirrorDir := makeCanonical(t)
	ctx := context.Background()

	real, err := gitbackend.NewGitBackend(gitbackend.Options{})
	require.NoError(t, err)
	backend := &racingBackend{GitBackend: real}
	backend.onTagPushed = func() {
		// 并发发布者:塞一个远高于本次的版本
		createMirrorTag(t, mirrorDir, "v9.9.9")
	}

	opts := Options{
		RepoDir:         dir,
		Mapping:         Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:            []string{"v0.0.1"},
		backendOverride: backend,
	}
	rep, err := Publish(ctx, opts)
	require.NoError(t, err)
	require.Len(t, rep.Tags, 1)
	tr := rep.Tags[0]

	// 本次 tag 本身必须发布成功（main 不动 ≠ 发布失败）
	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	tagRef, err := mirror.Reference(plumbing.NewTagReferenceName("v0.0.1"), true)
	require.NoError(t, err, "本次 tag 必须已发布")
	require.NotNil(t, tagRef)

	// 远端确实出现了更高版本
	_, err = mirror.Reference(plumbing.NewTagReferenceName("v9.9.9"), true)
	require.NoError(t, err, "并发发布者的 tag 应存在")

	// main 快照不得被回退
	mainRef, merr := mirror.Reference(plumbing.NewBranchReferenceName("main"), true)
	if merr == nil {
		assert.NotEqual(t, tr.Commit, mainRef.Hash().String(),
			"main 快照不得指向本次发布的旧版本(会把默认分支回退)")
	}
	assert.False(t, tr.MainUpdated, "远端已有更高版本时不该更新 main")
	assert.Contains(t, tr.MainSkipped, "v9.9.9", "报告要说清为什么没动 main")
}

// TestPublish_MainSnapshotUpdatedWhenNewer 本次版本确实更新 ⇒ main 必须被更新。
//
// 这是上一条的反向对照:不能因为加了防护就把 main 更新彻底关掉。
func TestPublish_MainSnapshotUpdatedWhenNewer(t *testing.T) {
	dir, mirrorDir := makeCanonical(t)
	ctx := context.Background()

	opts := Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	}
	rep, err := Publish(ctx, opts)
	require.NoError(t, err)
	require.Len(t, rep.Tags, 1)
	tr := rep.Tags[0]

	assert.True(t, tr.MainUpdated, "首次发布必须更新 main 快照")
	assert.Empty(t, tr.MainSkipped)

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	mainRef, err := mirror.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err, "main 快照必须存在")
	assert.Equal(t, tr.Commit, mainRef.Hash().String(), "main 必须指向本次发布的版本")
}
