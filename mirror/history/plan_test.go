package history

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/mirror"
)

type fileSpec struct {
	path string
	data string
}

// commitFiles 在 dir 写入文件并提交,返回提交 hash。
func commitFiles(t *testing.T, dir string, files []fileSpec) plumbing.Hash {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, f.path)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(f.data), 0o644))
	}
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true}))
	h, err := w.Commit("fixture", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
	require.NoError(t, err)
	return h
}

var fixtureSig = object.Signature{
	Name:  "fixture",
	Email: "fixture@example.com",
}

// sourceFiles / targetFiles 是测试夹具:内部源仓与公开目标仓的内容。
//
// 关键设计:两棵树共享同源工作,但形态不同 —— 模块路径不同(靠身份改写对齐)、
// 目录名不同(靠路径规则对齐)、措辞不同(靠人工判定)。这正是真实场景。
var sourceFiles = []fileSpec{
	{path: "go.mod", data: "module git.enjoye.top/enjoydream/app\n"},
	{path: "main.go", data: "package app\n\nimport \"git.enjoye.top/enjoydream/app/internal/x\"\n"},
	{path: "internal/x/x.go", data: "package x\n"},
	{path: "new.go", data: "package app\n"},
	{path: "docs/internal/secret.md", data: "internal only\n"},
	{path: "internal-private/secret.txt", data: "must not publish\n"},
	{path: "common.txt", data: "line1\nline2\n"},
	{path: "mixed.txt", data: "a\nsrc\n"},
	{path: "behind.txt", data: "a\n"},
	{path: "scrub.txt", data: "push harbor.enjoye.top/x\n"},
}

var targetFiles = []fileSpec{
	{path: "go.mod", data: "module github.com/yi-nology/app\n"},
	{path: "main.go", data: "package app\n\nimport \"github.com/yi-nology/app/internal/x\"\n"},
	{path: "internal/x/x.go", data: "package x\n"},
	{path: "docs/archive/secret.md", data: "internal only\n"},
	{path: "common.txt", data: "line1\n"},
	{path: "mixed.txt", data: "a\ntgt\n"},
	{path: "behind.txt", data: "a\nb\n"},
	{path: "only-target.md", data: "target only\n"},
}

// testOptions 返回夹具对应的比对选项。
func testOptions(t *testing.T) (Options, string, string) {
	t.Helper()
	srcDir := filepath.Join(t.TempDir(), "src")
	tgtDir := filepath.Join(t.TempDir(), "tgt")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.MkdirAll(tgtDir, 0o755))
	commitFiles(t, srcDir, sourceFiles)
	commitFiles(t, tgtDir, targetFiles)

	opts := Options{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		Rules: []mirror.IdentityRule{
			{
				Source: "git.enjoye.top/enjoydream/app",
				Target: "github.com/yi-nology/app",
				Kind:   mirror.KindModule,
			},
		},
		PathRules: []PathRule{
			{From: "docs/internal", To: "docs/archive"},
			{From: "internal-private", To: ""},
		},
		ScrubRules: []ScrubRule{
			{Name: "internal-host", Category: "internal-host",
				Pattern: regexp.MustCompile(`harbor\.enjoye\.top`)},
		},
	}
	return opts, srcDir, tgtDir
}

func TestBuildPlanDispositions(t *testing.T) {
	opts, _, _ := testOptions(t)
	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)

	byPath := make(map[string]FilePlan, len(plan.Files))
	for _, f := range plan.Files {
		byPath[f.Path] = f
	}

	// 身份改写后一致 —— 这正是改写要达成的结果。
	assert.Equal(t, DispositionSame, byPath["go.mod"].Disposition, byPath["go.mod"].Reason)
	assert.Equal(t, DispositionSame, byPath["main.go"].Disposition, byPath["main.go"].Reason)
	assert.Equal(t, DispositionSame, byPath["internal/x/x.go"].Disposition)

	// 路径重命名后一致。
	assert.Equal(t, DispositionSame, byPath["docs/archive/secret.md"].Disposition, byPath["docs/archive/secret.md"].Reason)

	// 源侧独有 → 移植。
	assert.Equal(t, DispositionPort, byPath["new.go"].Disposition, byPath["new.go"].Reason)

	// 源侧超集 → 覆盖。
	assert.Equal(t, DispositionPort, byPath["common.txt"].Disposition, byPath["common.txt"].Reason)
	assert.Equal(t, 1, byPath["common.txt"].AddedLines)

	// 目标侧超集 → 保护,不得覆盖。
	assert.Equal(t, DispositionPreserve, byPath["behind.txt"].Disposition, byPath["behind.txt"].Reason)
	assert.Equal(t, DispositionPreserve, byPath["only-target.md"].Disposition)

	// 两侧各有独有 → 待人工确认。
	assert.Equal(t, DispositionHold, byPath["mixed.txt"].Disposition, byPath["mixed.txt"].Reason)

	// 源侧新增且命中痕迹 → 清理后移植。
	assert.Equal(t, DispositionPortWithScrub, byPath["scrub.txt"].Disposition, byPath["scrub.txt"].Reason)
	require.Len(t, byPath["scrub.txt"].ScrubHits, 1)
	assert.Equal(t, "internal-host", byPath["scrub.txt"].ScrubHits[0].Category)
	assert.Equal(t, 1, byPath["scrub.txt"].ScrubHits[0].Line)

	// 命中排除规则 → 不参与同步,但要在计划里可审计。
	assert.Equal(t, DispositionExclude, byPath["internal-private/secret.txt"].Disposition)
}

func TestBuildPlanSummary(t *testing.T) {
	opts, _, _ := testOptions(t)
	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)

	assert.Equal(t, 11, plan.Summary.Total)
	assert.Equal(t, 4, plan.Summary.Same) // go.mod / main.go / internal-x / docs-archive
	assert.Equal(t, 2, plan.Summary.Port) // new.go / common.txt
	assert.Equal(t, 1, plan.Summary.PortWithScrub)
	assert.Equal(t, 1, plan.Summary.Exclude)
	assert.Equal(t, 1, plan.Summary.Hold)
	assert.Equal(t, 2, plan.Summary.Preserve)
	assert.Equal(t, 1, plan.Summary.ScrubHits)
}

func TestBuildPlanIsDeterministic(t *testing.T) {
	opts, _, _ := testOptions(t)
	first, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	second, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, first, second, "同一输入两次生成的计划必须完全一致")
}

func TestBuildPlanDoesNotWriteRepos(t *testing.T) {
	opts, srcDir, tgtDir := testOptions(t)

	headOf := func(dir string) string {
		r, err := git.PlainOpen(dir)
		require.NoError(t, err)
		ref, err := r.Head()
		require.NoError(t, err)
		return ref.Hash().String()
	}
	srcBefore, tgtBefore := headOf(srcDir), headOf(tgtDir)

	_, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)

	assert.Equal(t, srcBefore, headOf(srcDir), "源仓 HEAD 不应被改动")
	assert.Equal(t, tgtBefore, headOf(tgtDir), "目标仓 HEAD 不应被改动")
}

func TestBuildPlanProtectedList(t *testing.T) {
	opts, _, _ := testOptions(t)
	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)

	protected := plan.Protected()
	require.Len(t, protected, 2)
	paths := []string{protected[0].Path, protected[1].Path}
	assert.Contains(t, paths, "behind.txt")
	assert.Contains(t, paths, "only-target.md")

	// 需要动作的文件不含 preserve 与 same。
	for _, f := range plan.Actionable() {
		assert.NotEqual(t, DispositionPreserve, f.Disposition)
		assert.NotEqual(t, DispositionSame, f.Disposition)
	}
}

// TestOriginCommitMap 锁定「路径归属于最近一次修改它的提交」。
//
// 曾经的缺陷:回溯到根提交时无条件覆盖,导致所有在根提交就存在的文件
// 都被标成根提交,真正的来源提交丢失。
func TestOriginCommitMap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)

	write := func(path, data string) {
		p := filepath.Join(dir, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o644))
	}
	commit := func(msg string) plumbing.Hash {
		require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true}))
		h, err := w.Commit(msg, &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
		require.NoError(t, err)
		return h
	}

	write("old.txt", "v1\n")
	first := commit("first")
	write("old.txt", "v2\n")
	write("new.txt", "n\n")
	second := commit("second")

	origins, err := originCommitMap(context.Background(), r, second)
	require.NoError(t, err)

	assert.Equal(t, second.String(), origins["old.txt"].Hash, "old.txt 最近一次修改是 second")
	assert.Equal(t, second.String(), origins["new.txt"].Hash, "new.txt 由 second 引入")
	assert.Equal(t, "second", origins["old.txt"].Subject)

	// 从第一个提交看,old.txt 的归属应是 first 而不是根提交。
	originsFirst, err := originCommitMap(context.Background(), r, first)
	require.NoError(t, err)
	assert.Equal(t, first.String(), originsFirst["old.txt"].Hash)
}
