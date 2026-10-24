package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// e2eRepo 造一个仓库(工作树或裸仓库),返回路径。
func e2eRepo(t *testing.T, dir string, bare bool, files map[string]string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	r, err := git.PlainInit(dir, bare)
	require.NoError(t, err)
	if bare {
		return dir
	}
	wt, err := r.Worktree()
	require.NoError(t, err)
	for p, content := range files {
		full := filepath.Join(dir, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	require.NoError(t, wt.AddWithOptions(&git.AddOptions{All: true}))
	sig := &object.Signature{Name: "e2e", Email: "e2e@local", When: time.Unix(1700000000, 0)}
	_, err = wt.Commit("init", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	head, err := r.Head()
	require.NoError(t, err)
	require.NoError(t, r.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"), head.Hash())))
	// **HEAD 也要指到 main**:否则后续提交都落在默认分支(master)上,
	// 而执行读的是 main —— 看不到任何新提交。
	require.NoError(t, r.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))))
	return dir
}

// e2eSeedBare 把工作仓的 main 灌进裸仓库(当推送目标的本地替身)。
func e2eSeedBare(t *testing.T, bareDir, srcDir string) {
	t.Helper()
	r, err := git.PlainOpen(bareDir)
	require.NoError(t, err)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "seed", URLs: []string{srcDir}})
	require.NoError(t, err)
	require.NoError(t, r.Fetch(&git.FetchOptions{
		RemoteName: "seed",
		RefSpecs:   []config.RefSpec{"refs/heads/main:refs/heads/main"},
	}))
	ref, err := r.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	require.NoError(t, r.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("main"), ref.Hash())))
}

// TestHistoryEndToEnd 端到端演练:建通道 → 配推送目标 → 裁决 → 预检 →
// 移植 → 推送 → 校验审计链 → 回滚 → 再推。
//
// 这是**整条流水线**的门禁:三轮改动动过锁、哈希链、强制推送闸门、
// 产物目录回收、路径闸门 —— 单测各自绿,拼起来不一定不炸。
func TestHistoryEndToEnd(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	applyCount := 0
	root := t.TempDir()
	srcDir := e2eRepo(t, filepath.Join(root, "src"), false, map[string]string{
		"go.mod":      "module git.enjoye.top/enjoydream/app\n",
		"internal.go": "package app\n\n// 新增:源侧独有的能力\nfunc Extra() {}\n",
	})
	tgtDir := e2eRepo(t, filepath.Join(root, "tgt"), false, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})
	// 推送目标:本地裸仓库当「远端」的替身(两个:先留底后发布)。
	backupDir := e2eRepo(t, filepath.Join(root, "backup.git"), true, nil)
	publishDir := e2eRepo(t, filepath.Join(root, "publish.git"), true, nil)
	e2eSeedBare(t, backupDir, tgtDir)
	e2eSeedBare(t, publishDir, tgtDir)

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "e2e", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceBranch: strPtr("main"), TargetBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "backup", RepoURL: backupDir, CredType: model.HistoryCredNone,
	})
	require.NoError(t, err)
	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: publishDir, CredType: model.HistoryCredNone,
	})
	require.NoError(t, err)

	// 裁决:不需要(没有冲突文件)。
	// 预检:不推送,只摊开影响面。
	pv, err := h.HistoryPreview(ctx, ch.ID)
	require.NoError(t, err)
	require.NotEmpty(t, pv.NewHead, "预检应给出新的 HEAD")
	require.NotEqual(t, pv.BaseHead, pv.NewHead, "预检应与基线不同")
	require.True(t, pv.Gate.OK, "门禁应通过: "+pv.Gate.Note)
	require.Empty(t, pv.PendingConflicts, "本夹具不应有冲突")

	// 移植:产出待推送的提交。
	applyRun, err := h.HistoryApply(ctx, ch.ID)
	require.NoError(t, err)
	require.NotEmpty(t, applyRun.Head)
	require.NotEqual(t, applyRun.BaseHead, applyRun.Head, "移植应产生新提交")

	// 推送:两个目标都应落地。
	pushRes, err := h.HistoryPush(ctx, ch.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"backup", "publish"}, pushRes.Pushed)

	// 发布目标的 main 必须真的前进了。
	published, err := git.PlainOpen(publishDir)
	require.NoError(t, err)
	head, err := published.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	require.Equal(t, applyRun.Head, head.Hash().String(), "发布目标应指向移植后的 HEAD")

	// 审计链:每一步都入链,链必须完整。
	chain, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, chain.OK, "审计链必须完整:"+chain.Message)
	require.Equal(t, 2, chain.Checked, "apply + push 两条记录")

	// 回滚:退回基线 commit(用完整 hash,避免短 hash 歧义)。
	rb, err := h.HistoryRollback(ctx, applyRun.ID, applyRun.BaseHead)
	require.NoError(t, err)
	require.Equal(t, applyRun.BaseHead, rb.Heads["publish"], "回滚后应指向基线")

	published, err = git.PlainOpen(publishDir)
	require.NoError(t, err)
	head, err = published.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	require.Equal(t, applyRun.BaseHead, head.Hash().String(), "回滚后发布目标应回到基线")

	// 再推:把移植结果重新推上去。
	again, err := h.HistoryPush(ctx, ch.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"backup", "publish"}, again.Pushed)

	published, err = git.PlainOpen(publishDir)
	require.NoError(t, err)
	head, err = published.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	require.Equal(t, applyRun.Head, head.Hash().String(), "再推后应回到移植后的 HEAD")

	// 链仍然完整(apply / push / rollback / push 四条)。
	chain, err = h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, chain.OK, "审计链必须仍然完整:"+chain.Message)
	require.Equal(t, 4, chain.Checked, "apply/push/rollback/push 四条记录")

	// 冲突路径:两边都改了同一个文件 → 产生 hold → 裁决 → 再走一遍。
	//
	// 顺利路径跑通不代表冲突路径跑得通:裁决写回、覆盖的解法、
	// 以及「裁决后重新计算计划」这三步都在这条路径上。
	conflictFile := "shared.go"
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, conflictFile),
		[]byte("package app\n\n// 源侧版本\nfunc Shared() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tgtDir, conflictFile),
		[]byte("package app\n\n// 目标侧版本\nfunc Shared() {}\n"), 0o644))
	commitAll(t, srcDir)
	commitAll(t, tgtDir)

	plan, err := h.HistoryPlan(ctx, ch.ID)
	require.NoError(t, err)
	held := 0
	for _, f := range plan.Files {
		if f.Path == conflictFile {
			held++
		}
	}
	require.Equal(t, 1, held, "两边都改过的文件应判为 hold")

	// 裁决:取源侧版本(公开化改造的方向)。
	_, err = h.SetHistoryOverride(ctx, ch.ID, HistoryOverrideInput{
		Path: conflictFile, Verdict: model.HistoryVerdictTakeSource,
		Reason: "e2e", DecidedBy: "e2e",
	})
	require.NoError(t, err)

	// 裁决后重新预检:该文件不再是待人工。
	// (裁决是在 resolve 阶段应用的 —— plan 只是差异判定,不含裁决。)
	pv2, err := h.HistoryPreview(ctx, ch.ID)
	require.NoError(t, err)
	for _, c := range pv2.PendingConflicts {
		require.NotEqual(t, conflictFile, c.Path,
			"裁决后该文件不该仍待人工")
	}

	// **裁决必须真的进产物**(内容级断言)。
	//
	// 只断言「不再待人工」是不够的:曾经 apply 路径根本不读裁决,
	// 预检说「这个文件会被发布」、实际发布的却是旧内容 —— 而报告显示一切正常。
	applyAfter, err := h.HistoryApply(ctx, ch.ID)
	applyCount++
	require.NoError(t, err)
	afterRun, err := h.runs.FindByID(applyAfter.ID)
	require.NoError(t, err)
	produced, err := os.ReadFile(filepath.Join(afterRun.WorkRepoPath, conflictFile))
	require.NoError(t, err, "裁决后的产物里应有该文件")
	assert.Contains(t, string(produced), "源侧版本",
		"take_source 裁决后,产物必须是**源侧**内容")
	assert.NotContains(t, string(produced), "目标侧版本",
		"产物不能还是目标侧的旧内容")

	// **source_branch 场景**:通道配了源分支时,发布的内容必须来自那个分支。
	//
	// 曾经 apply 写死读 HEAD(计划却按 source_branch 算)→ 发布的是另一棵树,
	// 而门禁验的还是计划那棵 —— 没验到真正发出去的内容。
	branchFile := "branchy.go"
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, branchFile),
		[]byte("package app\n\n// BRANCH-VERSION\n"), 0o644))
	e2eBranch(t, srcDir, "feature", branchFile)
	// HEAD 仍在 main(内容是 MAIN-VERSION),feature 分支才是 BRANCH-VERSION。

	branchCh, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "branchy", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceBranch: strPtr("feature"), TargetBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)
	branchRun, err := h.HistoryApply(ctx, branchCh.ID)
	applyCount++
	require.NoError(t, err)
	branchStored, err := h.runs.FindByID(branchRun.ID)
	require.NoError(t, err)
	branchOut, err := os.ReadFile(filepath.Join(branchStored.WorkRepoPath, branchFile))
	require.NoError(t, err, "产物里应有该文件")
	assert.Contains(t, string(branchOut), "BRANCH-VERSION",
		"发布的内容必须来自 source_branch 指定的分支")
	assert.NotContains(t, string(branchOut), "MAIN-VERSION",
		"不能发布 HEAD 的内容")

	applyCount++ // 每次 apply 产一个产物目录
	// 产物目录:推送后**仍然保留**(回滚要用它 —— 备份里的对象就是从这儿取的),
	// 但只保留到 TTL(默认 24h),不会无限堆积。
	entries, err := os.ReadDir(h.productRoot())
	require.NoError(t, err)
	products := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "history-repo-") {
			products++
		}
	}
	require.Equal(t, applyCount, products,
		"产物目录数应等于 apply 次数(每次 apply 一个;推送与回滚复用,不另建)")

	// 执行记录里记着产物路径(推送时服务端自取,不回显给调用方)。
	stored, err := h.runs.FindByID(applyRun.ID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.WorkRepoPath, "执行记录应记下产物路径")
	_, err = os.Stat(stored.WorkRepoPath)
	require.NoError(t, err, "产物目录应存在")
}

// TestHistorySyncRunsPipelineOnce 回归:一键同步必须**只跑一遍**流水线。
//
// 旧实现是「先 HistoryPreview(临时目录物化 + 门禁)再 applyRun(产物目录物化 + 门禁)」:
// plan/resolve 各两遍、物化两遍、门禁两遍 —— 实仓上白跑一遍
// (plan+resolve≈65s、物化≈50s、类型门禁≈60s),一键同步的时间直接翻倍。
//
// 更要紧的是**一致性**:两遍算出的 head 可能不同(两次计算之间仓变了)
// ⇒ 预检报告说的不是真正要推的那棵树。
func TestHistorySyncRunsPipelineOnce(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")
	remoteDir := filepath.Join(root, "remote.git")

	makeHistoryRepo(t, srcDir, map[string]string{
		"go.mod": "module git.enjoye.top/enjoydream/app\n",
		"new.go": "package app\n\nfunc Extra() {}\n",
	})
	makeHistoryRepo(t, tgtDir, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})
	makeBareRepo(t, remoteDir)
	seedRemoteFrom(t, remoteDir, tgtDir, "main")

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "once", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
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

	before := h.MaterializeRuns()
	rep, err := h.HistorySync(ctx, HistorySyncOptions{ChannelID: ch.ID})
	require.NoError(t, err)
	assert.Equal(t, int64(1), h.MaterializeRuns()-before,
		"一键同步只应物化一次(旧实现跑两遍:预检一遍、执行一遍)")

	require.NotNil(t, rep.Preview, "一键同步必须给出预检报告")
	require.NotNil(t, rep.Apply, "一键同步必须给出执行记录")
	assert.Equal(t, rep.Apply.Head, rep.Preview.NewHead,
		"预检报告说的必须**就是**真正推送的那棵树(同一份物化结果)")
	assert.Empty(t, rep.Blocked, "这个夹具不应有阻塞项:%v", rep.Blocked)

	// 远端真的前进了。
	mirror, err := git.PlainOpen(remoteDir)
	require.NoError(t, err)
	head, err := mirror.Reference(plumbing.NewBranchReferenceName("main"), true)
	require.NoError(t, err)
	assert.Equal(t, rep.Apply.Head, head.Hash().String())
}

// commitAll 把工作区当前内容提交(用于造冲突夹具)。
func commitAll(t *testing.T, dir string) {
	t.Helper()
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.AddWithOptions(&git.AddOptions{All: true}))
	sig := &object.Signature{Name: "e2e", Email: "e2e@local", When: time.Unix(1700000100, 0)}
	_, err = wt.Commit("update", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
}

// e2eBranch 在仓库里建一个分支并提交内容(HEAD 保持在原处)。
func e2eBranch(t *testing.T, dir, branch, file string) {
	t.Helper()
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	wt, err := r.Worktree()
	require.NoError(t, err)
	orig, err := r.Head()
	require.NoError(t, err)

	require.NoError(t, wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch), Create: true,
	}))
	require.NoError(t, wt.AddWithOptions(&git.AddOptions{All: true}))
	sig := &object.Signature{Name: "e2e", Email: "e2e@local", When: time.Unix(1700000200, 0)}
	_, err = wt.Commit("branch", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	// 切回原分支:HEAD 必须留在原处,否则测的就不是「HEAD 与分支不同」了。
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{Hash: orig.Hash()}))
}
