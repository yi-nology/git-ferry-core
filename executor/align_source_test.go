package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// gitRun 在 dir 里跑一条 git 命令(带固定身份,避免依赖本机 git 配置)。
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// gitOut 返回 git 命令的输出(去空白)。
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// newLocalSyncLab 搭一套"源端工作仓 + 源端裸远端 + 目标端裸远端"。
// 返回 (源工作仓, 源裸远端, 目标裸远端)。
func newLocalSyncLab(t *testing.T) (src, srcBare, dstBare string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "src")
	srcBare = filepath.Join(dir, "src.git")
	dstBare = filepath.Join(dir, "dst.git")
	require.NoError(t, os.MkdirAll(src, 0o750))

	gitRun(t, src, "init", "-b", "main")
	gitRun(t, dir, "init", "--bare", "-b", "main", srcBare)
	gitRun(t, dir, "init", "--bare", "-b", "main", dstBare)
	gitRun(t, src, "remote", "add", "origin", srcBare)
	return src, srcBare, dstBare
}

// commitToSrc 在源端写一个文件并提交、推送到源裸远端,返回该提交 SHA。
func commitToSrc(t *testing.T, src, body, msg string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte(body), 0o640))
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-m", msg)
	gitRun(t, src, "push", "origin", "main")
	return gitOut(t, src, "rev-parse", "main")
}

// TestFetchRepo_IncrementalSync_AdvancesLocalBranch 钉住增量同步的核心语义。
//
// 回归背景:fetch 只更新 refs/remotes/origin/<branch>,本地分支仍停在旧提交;
// 而 push 推的是 refs/heads/<branch>(本地)。若不把本地分支推进到源端 ref,
// 目标侧永不前进 —— git 回 "already up-to-date" 被当成成功,
// 增量同步等于没跑(首次克隆正常,之后每次都空转)。
func TestFetchRepo_IncrementalSync_AdvancesLocalBranch(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	firstSHA := commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{
		Key: "align-test", SourceBranch: "main", TargetBranch: "main", SyncMode: "single",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	// 第一次同步:克隆源端 → 推送到目标端
	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, mustPush(t, exec, ctx, work, task, dstRepo, nil))
	require.Equal(t, firstSHA, gitOut(t, dstBare, "rev-parse", "main"),
		"首次同步应把源端内容推到目标端")

	// 源端再提交一次
	secondSHA := commitToSrc(t, src, "v2", "c2")
	require.NotEqual(t, firstSHA, secondSHA)

	// 第二次同步(增量):fetch 后目标侧必须前进到新提交
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))
	require.NoError(t, mustPush(t, exec, ctx, work, task, dstRepo, nil))
	require.Equal(t, secondSHA, gitOut(t, dstBare, "rev-parse", "main"),
		"增量同步后目标侧必须拿到源端新提交(fetch 之后本地分支要推进到 origin/<branch>)")
}

// TestAlignSourceBranch_NewBranchOnSource glob 多分支任务下,
// 源端新建的分支必须在本地建出来并指向源端 ref
// (只对齐"已存在的本地分支"会漏掉它,那个分支就永远推不出去)。
func TestAlignSourceBranch_NewBranchOnSource(t *testing.T) {
	src, srcBare, _ := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{
		Key: "align-new-branch", SourceBranch: "release/*", TargetBranch: "release/*", SyncMode: "all",
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))

	// 源端新建 release/1.0 分支
	gitRun(t, src, "checkout", "-b", "release/1.0")
	require.NoError(t, os.WriteFile(filepath.Join(src, "b.txt"), []byte("rel"), 0o640))
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-m", "rel")
	gitRun(t, src, "push", "origin", "release/1.0")
	relSHA := gitOut(t, src, "rev-parse", "release/1.0")
	gitRun(t, src, "checkout", "main")

	// fetch 之后本地应出现 release/1.0 且指向源端提交
	require.NoError(t, exec.fetchRepo(ctx, work, task, srcRepo, nil))
	require.Contains(t, gitOut(t, work, "for-each-ref", "--format=%(refname:short)", "refs/heads/"),
		"release/1.0", "源端新建的分支必须在本地建出来")
	require.Equal(t, relSHA, gitOut(t, work, "rev-parse", "refs/heads/release/1.0"),
		"新建的本地分支必须指向源端 ref")
}

// TestPushTags_DeliversTagsToTarget git_tags=true 时,源端的 tag 必须真的推到目标端。
//
// 回归背景:主推送只带分支 refspec(refs/heads/x:refs/heads/y),
// tag 只在 fetch / bundle / backup-remote 里被用到 —— **目标端从���没收到过 tag**,
// 而 git_tags 在文档里的语义是"同步 tags"。
func TestPushTags_DeliversTagsToTarget(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	// 源端打一个 tag 并推到源裸远端(clone 的是裸远端,tag 必须在远端才有)
	gitRun(t, src, "tag", "v1.0.0")
	gitRun(t, src, "push", "origin", "refs/tags/v1.0.0")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{
		Key: "tag-sync", SourceBranch: "main", TargetBranch: "main",
		SyncMode: "single", GitTags: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, mustPush(t, exec, ctx, work, task, dstRepo, nil))
	require.NoError(t, exec.pushTags(ctx, work, task, dstRepo, nil))

	// 目标端必须能看到这个 tag
	out := gitOut(t, dstBare, "tag", "--list")
	require.Contains(t, out, "v1.0.0", "git_tags=true 时 tag 必须推到目标端")
}

// TestPushTags_SkippedWhenDisabled git_tags=false 时不应推 tag。
func TestPushTags_SkippedWhenDisabled(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")
	gitRun(t, src, "tag", "v9.9.9")
	gitRun(t, src, "push", "origin", "refs/tags/v9.9.9")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{Key: "no-tags", SourceBranch: "main", TargetBranch: "main", SyncMode: "single"}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, mustPush(t, exec, ctx, work, task, dstRepo, nil))
	require.NoError(t, exec.pushTags(ctx, work, task, dstRepo, nil))

	require.NotContains(t, gitOut(t, dstBare, "tag", "--list"), "v9.9.9",
		"git_tags=false 时不应推 tag")
}

// TestPushTags_ConflictingTagDoesNotBlockOthers 目标端已有同名不同 SHA 的 tag 时,
// **别的** tag 仍然要送达。
//
// 回归背景:git 的批量 push(`refs/tags/*:refs/tags/*`)是**全有全无** ——
// 一个冲突的 tag 会让整批推失败,连本来没问题的 tag 也送不过去
// (实测:目标端有个同名不同 SHA 的 tag,源端所有 tag 都没同步上,只留一行日志)。
func TestPushTags_ConflictingTagDoesNotBlockOthers(t *testing.T) {
	src, srcBare, dstBare := newLocalSyncLab(t)
	work := filepath.Join(t.TempDir(), "work")
	commitToSrc(t, src, "v1", "c1")

	// 源端两个 tag
	gitRun(t, src, "tag", "v1.0.0")
	gitRun(t, src, "tag", "v2.0.0")
	gitRun(t, src, "push", "origin", "refs/tags/v1.0.0", "refs/tags/v2.0.0")

	// 目标端先造一个**同名不同 SHA** 的 v1.0.0(模拟目标端已有的历史 tag)。
	// 只推 tag、不动 main —— 否则分支推送会被判非快进而失败,测的就不是 tag 了。
	dstWork := filepath.Join(t.TempDir(), "dstwork")
	gitRun(t, t.TempDir(), "clone", "-q", srcBare, dstWork)
	gitRun(t, dstWork, "checkout", "-q", "-b", "side")
	require.NoError(t, os.WriteFile(filepath.Join(dstWork, "other.txt"), []byte("x"), 0o640))
	gitRun(t, dstWork, "add", ".")
	gitRun(t, dstWork, "commit", "-m", "target-side")
	gitRun(t, dstWork, "tag", "-f", "v1.0.0")
	gitRun(t, dstWork, "push", "-q", "--force", dstBare, "refs/tags/v1.0.0")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	task := &model.SyncTask{
		Key: "tag-conflict", SourceBranch: "main", TargetBranch: "main",
		SyncMode: "single", GitTags: true,
	}
	srcRepo := &model.Repo{CloneURL: srcBare}
	dstRepo := &model.Repo{CloneURL: dstBare}
	ctx := context.Background()

	require.NoError(t, exec.cloneRepo(ctx, work, srcRepo, task, nil))
	require.NoError(t, exec.ensureRemote(ctx, work, dstRepo))
	require.NoError(t, mustPush(t, exec, ctx, work, task, dstRepo, nil))

	// 冲突的 v1.0.0 推不过去(应当报错),但 v2.0.0 必须送达
	err = exec.pushTags(ctx, work, task, dstRepo, nil)
	require.Error(t, err, "冲突的 tag 应当被报告出来")
	require.Contains(t, err.Error(), "v1.0.0", "错误里应当点名冲突的 tag")

	out := gitOut(t, dstBare, "tag", "--list")
	require.Contains(t, out, "v2.0.0", "没冲突的 tag 必须送达(不能被一个冲突的 tag 连坐)")
}

// mustPush 调 exec.push 并只关心错误(忽略"是否用掉放行"的返回)。
func mustPush(t *testing.T, exec *Executor, ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) error {
	t.Helper()
	_, _, _, err := exec.push(ctx, dir, task, repo, platform)
	return err
}
