package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// TestPruneRemoteBranchesMirror_MultiBranch 多分支的镜像删除:
// 目标上"源端已不存在"的分支要被删掉,源端还在的分支要留着。
//
// 回归背景:多分支的 git_push_prune 以前**只告警不实现**
// ("SDK 的 ListRemoteBranches 不带凭证 ⇒ 暂不实现")——
// 而配置里明明有开关,看着像生效的,实际什么都不删。
//
// 判定依据改成"两端 fetch 之后的远端跟踪 ref"(不需要新的 SDK 读原语):
// 目标有、源端没有 ⇒ 删。
func TestPruneRemoteBranchesMirror_MultiBranch(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "release/1.0", "r1", "cr1")

	// 目标侧多一个分支:release/old(源端已经没有它了)
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	gitRun(t, dstWork, "checkout", "-q", "-b", "release/old")
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "old.txt"), []byte("o"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "old-branch")
	// **只推到目标裸仓**:推到 origin(= 源端裸仓)会把"目标独有分支"带进源端,
	// 分歧被抹掉 ⇒ 源端也有它 ⇒ 镜像删除本就不该删(测试就白测了)。
	gitRun(t, dstWork, "push", "-q", dstBare, "release/old:release/old")
	// 目标侧也应有 release/1.0(源端还在的那个)⇒ 镜像删除**不能**碰它
	// (从源工作仓推:dstWork 是 srcBare 的克隆,只有默认分支是本地分支)
	gitRun(t, src, "push", "-q", dstBare, "release/1.0:release/1.0")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "multi-prune", SourceBranch: "release/*", TargetBranch: "release/*",
		GitPushPrune: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// **经由 push() 走完整链路**:直接调 helper 测不到"调用点有没有接上"
	// (曾经出现过"helper 写好了但 push() 里没调"的空跑)。
	_, _, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)

	branches := targetBranches(t, dstBare)
	assert.NotContains(t, branches, "release/old", "源端已不存在的分支应被删掉")
	assert.Contains(t, branches, "release/1.0", "源端还在的分支必须留着")
}

// TestPruneRemoteBranchesMirror_EmptySourceDeletesNothing 源端分支列表为空时
// **一个都不能删**。
//
// 这是最重要的安全阀:源端 fetch 失败/仓是空时,若照样删,就会把目标上的
// 全部分支删光(灾难级)。
func TestPruneRemoteBranchesMirror_EmptySourceDeletesNothing(t *testing.T) {
	_, _, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	// 目标侧有分支
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", dstBare, dstWork)
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "a.txt"), []byte("a"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "c")
	gitRun(t, dstWork, "push", "-q", "origin", "main")
	require.NoError(t, os.MkdirAll(work, 0o750))
	gitRun(t, work, "init", "-q", "-b", "main")
	gitRun(t, work, "remote", "add", "target", dstBare)
	gitRun(t, work, "remote", "add", "origin", dstBare)

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{Key: "empty-source", SourceBranch: "release/*", TargetBranch: "release/*"}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.pruneRemoteBranchesMirror(ctx, work, task, dstRepo, nil))

	assert.Contains(t, targetBranches(t, dstBare), "main",
		"源端分支列表为空时绝不能删目标分支(安全阀)")
}

// TestSyncWiki_PushesPerBranch wiki 逐分支推:一个分支非快进**不挡**其它分支。
//
// 回归背景:整批推("refs/heads/*:refs/heads/*")时,目标上任一分支非快进
// ⇒ 平台拒收整批 ⇒ 其它无分歧的分支也同步不了。
func TestSyncWiki_PushesPerBranch(t *testing.T) {
	srcWiki, dstWiki, srcURL, dstURL := newWikiLab(t)
	// 源 wiki:main + docs
	wikiWork(t, srcWiki, "src", "v1", "c1")
	work := wikiWork(t, srcWiki, "src2", "v2", "c2")
	gitRun(t, work, "checkout", "-q", "-b", "docs")
	require.NoError(t, os.WriteFile(filepath.Join(work, "d.md"), []byte("d"), 0o640))
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "docs")
	gitRun(t, work, "push", "-q", "origin", "docs")

	// 目标 wiki 的 main 有人工提交(非快进)
	wikiWork(t, dstWiki, "dst", "human", "human-commit")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	execWork := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(execWork, 0o750))
	var details strings.Builder
	err = exec.syncWiki(context.Background(), execWork,
		&model.SyncTask{Key: "wiki-per-branch", SourceBranch: "main", TargetBranch: "main"},
		&model.Repo{CloneURL: srcURL}, &model.Repo{CloneURL: dstURL}, nil, nil, &details)
	require.Error(t, err, "main 非快进应当失败")
	assert.Contains(t, err.Error(), "main", "错误要点名是哪个分支")

	// **docs 照样同步过去了**(整批推时它会被 main 的失败一起挡住)
	branches := wikiBranches(t, dstWiki)
	assert.Contains(t, branches, "docs", "一个分支失败不该挡住其它分支")
	assert.Contains(t, branches, "main", "目标原有的 main 仍在")
}

// TestPruneRemoteBranchesMirror_SourceDeletedBranch 源端**先有**这个分支、
// 后来删掉 ⇒ 目标侧的同名分支必须被删掉。
//
// 回归背景:这条专门钉住"源端 fetch **恒剪枝**"这件事。
//
// 剪枝是镜像语义的基石:源端删了分支,本地若还留着它的远端跟踪 ref ⇒
// ① align 把本地分支对齐到旧提交 ⇒ 源端已删的分支又被推回目标(分支"复活");
// ② 判断"源端还在不在"靠的就是 ref 被剪掉 ⇒ 不剪的话镜像删除永远不触发。
//
// 而 gogit 后端(生产用的)只在 Prune=true 时才剪,native 恒剪
// ⇒ 不显式传 true,同一份配置在两个后端行为不同,生产上镜像删除一直没生效。
func TestPruneRemoteBranchesMirror_SourceDeletedBranch(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	commitBranchToSrc(t, src, "release/gone", "g1", "cg1")

	// 目标侧有同一个分支
	gitRun(t, src, "push", "-q", dstBare, "release/gone:release/gone")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	task := &model.SyncTask{
		Key: "source-gone", SourceBranch: "release/*", TargetBranch: "release/*",
		GitPushPrune: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 源端删掉这个分支
	gitRun(t, src, "push", "-q", "origin", "--delete", "release/gone")
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	// 前置条件:本地不应还留着源端已删分支的远端跟踪 ref(恒剪枝 ⇒ 被剪掉)
	_, rerr := exec.backend.RevParse(ctx, work, "refs/remotes/origin/release/gone")
	require.Error(t, rerr, "源端已删的分支,其远端跟踪 ref 必须被剪掉")

	_, _, _, err = exec.push(ctx, work, task, dstRepo, nil)
	require.NoError(t, err)
	assert.NotContains(t, targetBranches(t, dstBare), "release/gone",
		"源端已删除的分支,目标侧同名分支必须被删掉")
}

// pruneRecordingBackend 记录每次 Fetch 的入参(用于钉住"恒剪枝"这个语义)。
type pruneRecordingBackend struct {
	gitbackend.GitBackend
	mu    sync.Mutex
	fetch []gitbackend.FetchOptions
}

func (b *pruneRecordingBackend) Fetch(ctx context.Context, opts gitbackend.FetchOptions) (*gitbackend.FetchResult, error) {
	b.mu.Lock()
	b.fetch = append(b.fetch, opts)
	b.mu.Unlock()
	return b.GitBackend.Fetch(ctx, opts)
}

// TestFetchRepo_AlwaysPrunes 源端 fetch 必须**恒剪枝**。
//
// 回归背景:剪掉"源端已删分支"的远端跟踪 ref 是镜像语义的基石 ——
// 不剪的话 ①陈旧跟踪 ref 留在本地 ⇒ align 把本地分支对齐到旧提交
// ⇒ 源端已删的分支又被推回目标(分支"复活");
// ②"源端是否还在这个分支"的判断靠的就是 ref 被剪掉 ⇒ 镜像删除永远不触发。
//
// 而 **gogit 后端(生产用的)只在 Prune=true 时才剪**,native 恒剪
// ⇒ 同一份配置在两个后端行为不同,生产上镜像删除其实一直没生效。
//
// native 后端恒剪 ⇒ 这条只能用"记录入参"的桩来钉住,跑真实 git 是测不出来的。
func TestFetchRepo_AlwaysPrunes(t *testing.T) {
	src, srcBare, _ := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)
	rec := &pruneRecordingBackend{GitBackend: exec.backend}
	exec.backend = rec

	task := &model.SyncTask{
		Key: "always-prune", SourceBranch: "main", TargetBranch: "main",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.NotEmpty(t, rec.fetch, "应当发生过 fetch")
	for _, f := range rec.fetch {
		assert.True(t, f.Prune, "源端 fetch 必须恒剪枝(remote=%s)", f.Remote)
	}
}
