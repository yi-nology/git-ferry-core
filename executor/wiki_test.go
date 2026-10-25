package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
)

func TestWikiURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/o/r.git", "https://github.com/o/r.wiki.git"},
		{"https://gitlab.com/o/r.git", "https://gitlab.com/o/r.wiki.git"},
		{"https://github.com/o/r.wiki.git", "https://github.com/o/r.wiki.git"},
		{"https://gitea.com/o/r", "https://gitea.com/o/r.wiki.git"},
		{"git@github.com:o/r.git", "git@github.com:o/r.wiki.git"},
		{"/local/path/repo", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := wikiURL(c.in); got != c.want {
			t.Errorf("wikiURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFileToken(t *testing.T) {
	assert.Equal(t, "my_task", strutil.SanitizeFileToken("my/task"))
	assert.Equal(t, "a_b", strutil.SanitizeFileToken(`a\b`))
	assert.Equal(t, "_", strutil.SanitizeFileToken(".."))
	assert.Equal(t, "unnamed", strutil.SanitizeFileToken(""))
}

// newWikiLab 造一对本地 wiki 裸仓(src/dst),并给出能推导出 wiki URL 的
// "仓库地址"(file:// + .git 结尾 ⇒ 推导成 <base>.wiki.git)。
func newWikiLab(t *testing.T) (srcWiki, dstWiki, srcRepoURL, dstRepoURL string) {
	t.Helper()
	dir := t.TempDir()
	srcWiki = filepath.Join(dir, "src.wiki.git")
	dstWiki = filepath.Join(dir, "dst.wiki.git")
	gitRun(t, dir, "init", "--bare", "-b", "main", srcWiki)
	gitRun(t, dir, "init", "--bare", "-b", "main", dstWiki)
	srcRepoURL = "file://" + filepath.Join(dir, "src.git")
	dstRepoURL = "file://" + filepath.Join(dir, "dst.git")
	return
}

// wikiWork 用给定裸仓建一个工作副本并提交内容,返回工作目录。
func wikiWork(t *testing.T, bare, name, body, msg string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), name)
	gitRun(t, t.TempDir(), "clone", "-q", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "page.md"), []byte(body), 0o640))
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", msg)
	gitRun(t, work, "push", "-q", "origin", "HEAD:main")
	return work
}

// wikiBranches 列裸仓的分支。
func wikiBranches(t *testing.T, bare string) []string {
	t.Helper()
	out := gitOut(t, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	var res []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			res = append(res, l)
		}
	}
	return res
}

// TestSyncWiki_PushesCurrentContent wiki 推的必须是**当前**内容,不是克隆时的旧内容。
//
// 回归背景(P0):wiki 的 fetch 只更新 refs/remotes/origin/*,本地 refs/heads/*
// 停在克隆那一刻的提交 ⇒ 推出去的 wiki 永远是旧内容(而且每次都覆盖目标,
// 目标侧人工改的内容被抹掉),而 details 写着 "wiki: done"。
func TestSyncWiki_PushesCurrentContent(t *testing.T) {
	srcWiki, dstWiki, srcURL, dstURL := newWikiLab(t)
	wikiWork(t, srcWiki, "src", "v1", "c1")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	ctx := context.Background()
	task := &model.SyncTask{Key: "wiki-1", SourceBranch: "main", TargetBranch: "main"}

	// 第一次同步:wiki 目录还不存在 ⇒ clone
	execWork := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(execWork, 0o750))
	var details strings.Builder
	require.NoError(t, exec.syncWiki(ctx, execWork, task,
		&model.Repo{CloneURL: srcURL}, &model.Repo{CloneURL: dstURL}, nil, nil, &details))

	// 源端 wiki 前进
	wikiWork(t, srcWiki, "src2", "v2", "c2")

	// 第二次同步:走 fetch 分支
	var details2 strings.Builder
	require.NoError(t, exec.syncWiki(ctx, execWork, task,
		&model.Repo{CloneURL: srcURL}, &model.Repo{CloneURL: dstURL}, nil, nil, &details2))

	// 目标 wiki 的内容必须是**最新的**
	dstCheck := filepath.Join(t.TempDir(), "check")
	gitRun(t, t.TempDir(), "clone", "-q", dstWiki, dstCheck)
	content, err := os.ReadFile(filepath.Join(dstCheck, "page.md"))
	require.NoError(t, err)
	assert.Equal(t, "v2", string(content), "目标 wiki 必须是最新内容(原来推的是克隆时的旧内容)")
}

// TestSyncWiki_SyncsAllBranches wiki 的**非默认分支**也要同步。
//
// 回归背景:clone 只把默认分支建成本地分支,其余分支只在 refs/remotes/origin/*;
// 推的却是 refs/heads/* ⇒ 非默认分支一次都没同步过。
func TestSyncWiki_SyncsAllBranches(t *testing.T) {
	srcWiki, dstWiki, srcURL, dstURL := newWikiLab(t)
	work := wikiWork(t, srcWiki, "src", "v1", "c1")
	// 源端加一个非默认分支
	gitRun(t, work, "checkout", "-q", "-b", "docs")
	require.NoError(t, os.WriteFile(filepath.Join(work, "docs.md"), []byte("d"), 0o640))
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-m", "docs")
	gitRun(t, work, "push", "-q", "origin", "docs")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	execWork := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(execWork, 0o750))
	var details strings.Builder
	require.NoError(t, exec.syncWiki(context.Background(), execWork,
		&model.SyncTask{Key: "wiki-2", SourceBranch: "main", TargetBranch: "main"},
		&model.Repo{CloneURL: srcURL}, &model.Repo{CloneURL: dstURL}, nil, nil, &details))

	branches := wikiBranches(t, dstWiki)
	assert.Contains(t, branches, "main", "默认分支必须同步")
	assert.Contains(t, branches, "docs", "非默认分支也必须同步(原来一次都没同步)")
}

// TestSyncWiki_RespectsDivergence 目标 wiki 有源端没有的提交时,不得覆盖。
//
// 回归背景:refspec 写的是 "+refs/heads/*:refs/heads/*" —— "+" 是**无条件强推**,
// 目标侧的人工提交被直接抹掉,而 task.GitForce 形同虚设。
func TestSyncWiki_RespectsDivergence(t *testing.T) {
	srcWiki, dstWiki, srcURL, dstURL := newWikiLab(t)
	wikiWork(t, srcWiki, "src", "v1", "c1")

	// 目标 wiki 有人工提交
	wikiWork(t, dstWiki, "dst", "human", "human-commit")

	svc := newMockService()
	exec, err := NewExecutor(svc)
	require.NoError(t, err)

	execWork := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(execWork, 0o750))
	var details strings.Builder
	err = exec.syncWiki(context.Background(), execWork,
		&model.SyncTask{Key: "wiki-3", SourceBranch: "main", TargetBranch: "main", GitForce: false},
		&model.Repo{CloneURL: srcURL}, &model.Repo{CloneURL: dstURL}, nil, nil, &details)
	require.Error(t, err, "目标有独有提交且未开 git_force ⇒ 必须拒绝覆盖")
	assert.Contains(t, strings.ToLower(err.Error()), "non-fast-forward")

	// 目标侧的人工提交必须还在
	dstCheck := filepath.Join(t.TempDir(), "check")
	gitRun(t, t.TempDir(), "clone", "-q", dstWiki, dstCheck)
	content, rerr := os.ReadFile(filepath.Join(dstCheck, "page.md"))
	require.NoError(t, rerr)
	assert.Equal(t, "human", string(content), "目标侧人工提交不该被覆盖")
}
