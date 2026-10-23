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

// commitSeq 在 dir 里按顺序提交若干批文件,返回提交 hash 序列。
func commitSeq(t *testing.T, dir string, batches [][]fileSpec) []plumbing.Hash {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)

	var hashes []plumbing.Hash
	for _, batch := range batches {
		for _, f := range batch {
			p := filepath.Join(dir, f.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(f.data), 0o644))
		}
		require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true}))
		h, err := w.Commit("fixture", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
		require.NoError(t, err)
		hashes = append(hashes, h)
	}
	return hashes
}

// applyFixture 构造源仓与目标仓,返回选项与两仓路径。
// extraSource 追加到源仓最后一个提交的文件(用于痕迹夹具)。
func applyFixture(t *testing.T, extraSource ...fileSpec) (ApplyOptions, string, string) {
	t.Helper()
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")

	// 目标仓:两个提交。
	commitSeq(t, tgtDir, [][]fileSpec{
		{{path: "go.mod", data: "module github.com/yi-nology/app\n"}},
		{{path: "keep.go", data: "package app\n"},
			{path: "docs/keep.md", data: "keep\n"}},
	})
	// 源仓:同名但模块路径不同,外加一个新文件。
	// extraSource 并入最后一个提交(痕迹夹具要落在已提交内容里)。
	lastBatch := []fileSpec{
		{path: "new.go", data: "package app\n"},
		{path: "go.mod", data: "module git.enjoye.top/enjoydream/app\n"},
	}
	lastBatch = append(lastBatch, extraSource...)
	commitSeq(t, srcDir, [][]fileSpec{
		{{path: "go.mod", data: "module git.enjoye.top/enjoydream/app\n"}},
		{{path: "keep.go", data: "package app\n"},
			{path: "docs/keep.md", data: "keep\n"}},
		lastBatch,
	})

	opts := Options{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		Rules: []mirror.IdentityRule{
			{Source: "git.enjoye.top/enjoydream/app", Target: "github.com/yi-nology/app",
				Kind: mirror.KindModule},
		},
	}
	apply := ApplyOptions{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		TargetRef: "master",
		Rules:     opts.Rules,
	}
	return apply, srcDir, tgtDir
}

func TestApplyPreservesExistingCommits(t *testing.T) {
	apply, _, tgtDir := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	tgtRepo, err := git.PlainOpen(tgtDir)
	require.NoError(t, err)
	beforeHead, err := tgtRepo.Head()
	require.NoError(t, err)

	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	// 既有提交 hash 不变:旧 HEAD 仍是新 HEAD 的祖先。
	workRepo, err := git.PlainOpen(res.RepoPath)
	require.NoError(t, err)
	assert.True(t, isAncestorRepo(t, workRepo, beforeHead.Hash(), plumbing.NewHash(res.Head)),
		"既有提交必须原样保留")

	// 目标仓本身没被改动。
	afterHead, err := tgtRepo.Head()
	require.NoError(t, err)
	assert.Equal(t, beforeHead.Hash().String(), afterHead.Hash().String(),
		"原始目标仓不应被移植过程改动")
}

func TestApplyIsIdempotent(t *testing.T) {
	apply, _, _ := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.Plan = plan
	first, err := Apply(context.Background(), apply)
	require.NoError(t, err)
	second, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	assert.Equal(t, first.Head, second.Head, "同一输入两次移植必须产生相同 HEAD")
	assert.Equal(t, first.Commits, second.Commits)
}

func TestApplyPortsNewFile(t *testing.T) {
	apply, _, _ := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)
	require.NotEmpty(t, res.Commits, "应产生新提交")

	workRepo, err := git.PlainOpen(res.RepoPath)
	require.NoError(t, err)
	head, err := workRepo.CommitObject(plumbing.NewHash(res.Head))
	require.NoError(t, err)
	tree, err := head.Tree()
	require.NoError(t, err)
	files, err := flattenTree(tree)
	require.NoError(t, err)

	_, ok := files["new.go"]
	assert.True(t, ok, "源侧独有文件应被移植")
	// 既有文件保留。
	_, ok = files["docs/keep.md"]
	assert.True(t, ok, "既有文件必须保留")
}

func TestApplyScrubAutoCleans(t *testing.T) {
	apply, _, _ := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	// 给源侧文件塞一个内网域名。
	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.ScrubRules = []ScrubRule{
		StripInternalHost("git.enjoye.top"),
	}
	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)
	assert.Empty(t, res.MandatoryHits, "内网域名是可自动清理类,不应产生强制 hold")
}

func TestApplyScrubMandatoryBlocks(t *testing.T) {
	apply, _, _ := applyFixture(t, fileSpec{
		path: "secret.go",
		data: "package app\n\nconst token = \"ghp_abcdefghijklmnopqrstuvwxyz012345\"\n",
	})
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.ScrubRules = []ScrubRule{
		{Name: "secret", Category: "secret",
			Pattern:  regexp.MustCompile(`ghp_[a-zA-Z0-9]{20,}`),
			Severity: ScrubMandatory},
	}
	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)
	assert.NotEmpty(t, res.MandatoryHits, "密钥类命中必须报告")
	assert.Contains(t, res.Skipped, "secret.go", "命中强制痕迹的文件不得被移植")
}

func TestBuildTreeFromFilesOrdering(t *testing.T) {
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	st := r.Storer

	files := map[string]treeFile{
		"a.txt":     {mode: 0o644, hash: plumbing.NewHash("1111111111111111111111111111111111111111")},
		"b/c.txt":   {mode: 0o644, hash: plumbing.NewHash("2222222222222222222222222222222222222222")},
		"b.txt":     {mode: 0o644, hash: plumbing.NewHash("3333333333333333333333333333333333333333")},
		"b.d/e.txt": {mode: 0o644, hash: plumbing.NewHash("4444444444444444444444444444444444444444")},
	}
	hash, err := buildTreeFromFiles(st, files)
	require.NoError(t, err)

	tree, err := object.GetTree(st, hash)
	require.NoError(t, err)

	var names []string
	for _, e := range tree.Entries {
		names = append(names, e.Name)
	}
	// git 的 tree 排序:目录按「name/」比较,故 "b.d" 排在 "b.txt" 之前、
	// "b" 目录排在两者之后。
	assert.Equal(t, []string{"a.txt", "b.d", "b.txt", "b"}, names)
}

// isAncestorRepo 判断 anc 是否是 desc 的祖先。
func isAncestorRepo(t *testing.T, repo *git.Repository, anc, desc plumbing.Hash) bool {
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
