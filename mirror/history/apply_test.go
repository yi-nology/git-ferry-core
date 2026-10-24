package history

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
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

// TestApplyPortsPortWithScrubFile 回归:处置为 `port_with_scrub` 的文件
// 必须被移植(内容清理后),不能被静默丢弃。
//
// 丢一个文件 = 公开树少一份内容,且**不留任何痕迹** ——
// 不进 skipped、不进 mandatoryHits、审计报告里也看不见。
func TestApplyPortsPortWithScrubFile(t *testing.T) {
	apply, _, _ := applyFixture(t, fileSpec{
		path: "internal.txt",
		data: "host=git.enjoye.top\n",
	})
	rules := []ScrubRule{StripInternalHost("git.enjoye.top")}
	opts := Options{
		SourceDir: apply.SourceDir, TargetDir: apply.TargetDir,
		Rules: apply.Rules, ScrubRules: rules,
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	// 该文件必须落在 port_with_scrub 处置上。
	var disp Disposition
	for _, f := range plan.Files {
		if f.Path == "internal.txt" {
			disp = f.Disposition
		}
	}
	require.Equal(t, DispositionPortWithScrub, disp, "内网域名应落在 port_with_scrub")

	apply.ScrubRules = rules
	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	assert.Contains(t, res.AppliedFileList(), "internal.txt",
		"port_with_scrub 的文件必须被移植,不能静默消失")
	assert.NotContains(t, res.Skipped, "internal.txt")
	assert.NotEmpty(t, res.ScrubHits, "自动清理类的命中必须留痕")
	assert.Empty(t, res.MandatoryHits, "自动清理类不产生强制人工命中")

	// 落到工作区的内容必须已清理。
	workRepo, err := git.PlainOpen(res.RepoPath)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(res.RepoPath, "internal.txt"))
	require.NoError(t, err)
	assert.NotContains(t, string(got), "git.enjoye.top", "内网域名必须已清理")
	_ = workRepo
}

// TestApplyPortWithScrubMandatoryBlocks 回归:port_with_scrub 的文件若命中
// 强制人工规则(密钥类),必须跳过并在报告里出现 —— 密钥信号不能丢。
func TestApplyPortWithScrubMandatoryBlocks(t *testing.T) {
	apply, _, _ := applyFixture(t, fileSpec{
		path: "secret.txt",
		data: "token=ghp_abcdefghijklmnopqrstuvwxyz012345\n",
	})
	rules := []ScrubRule{
		{Name: "secret", Category: "secret",
			Pattern:  regexp.MustCompile(`ghp_[a-zA-Z0-9]{20,}`),
			Severity: ScrubMandatory},
	}
	opts := Options{
		SourceDir: apply.SourceDir, TargetDir: apply.TargetDir,
		Rules: apply.Rules, ScrubRules: rules,
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.ScrubRules = rules
	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	assert.Contains(t, res.Skipped, "secret.txt", "命中强制痕迹的文件不得被移植")
	assert.NotContains(t, res.AppliedFileList(), "secret.txt")
	assert.NotEmpty(t, res.MandatoryHits, "密钥命中必须上报,不能被静默吞掉")
}

// TestApplyDoesNotInheritSourceAuthor 回归:目标侧提交的作者**不能**继承源提交。
//
// 源侧是内网仓,作者是内网开发者(姓名 + 企业邮箱)——
// 继承过来等于把内网人员信息发布到公开仓。
// 时间戳仍然继承(公开树要保留源侧的时间线,且要保证两次移植 hash 相同)。
func TestApplyDoesNotInheritSourceAuthor(t *testing.T) {
	apply, _, _ := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)
	require.NotEmpty(t, res.Commits)

	workRepo, err := git.PlainOpen(res.RepoPath)
	require.NoError(t, err)
	head, err := workRepo.CommitObject(plumbing.NewHash(res.Head))
	require.NoError(t, err)

	assert.NotEqual(t, fixtureSig.Email, head.Author.Email,
		"不能继承源提交的作者邮箱(内网开发者邮箱会进公开仓)")
	assert.NotEqual(t, fixtureSig.Email, head.Committer.Email,
		"不能继承源提交的提交者邮箱")
	assert.Equal(t, DefaultPublicAuthor.Email, head.Author.Email,
		"默认用中性的公开身份")
	assert.Equal(t, DefaultPublicAuthor.Name, head.Author.Name)
}

// TestApplyUsesConfiguredAuthor 配置了公开身份时用它。
func TestApplyUsesConfiguredAuthor(t *testing.T) {
	apply, _, _ := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.Plan = plan
	apply.AuthorName = "Zhangyi"
	apply.AuthorEmail = "zhangyi@example.com"
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	workRepo, err := git.PlainOpen(res.RepoPath)
	require.NoError(t, err)
	head, err := workRepo.CommitObject(plumbing.NewHash(res.Head))
	require.NoError(t, err)
	assert.Equal(t, "Zhangyi", head.Author.Name)
	assert.Equal(t, "zhangyi@example.com", head.Author.Email)
}

// TestApplyKeepsExecutableBit 回归:新文件要保留源侧的可执行位。
//
// 丢了 +x,发布出去的脚本就不可执行 —— 公开仓里表现为「脚本跑不起来」,
// 而且只在 Linux 上复现,本地 macOS 常常看不出来。
func TestApplyKeepsExecutableBit(t *testing.T) {
	apply, _, _ := applyFixture(t)
	opts := Options{SourceDir: apply.SourceDir, TargetDir: apply.TargetDir, Rules: apply.Rules}

	// 给源仓加一个可执行脚本。
	repo, err := git.PlainOpen(apply.SourceDir)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	scriptPath := filepath.Join(apply.SourceDir, "deploy.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hi\n"), 0o755))
	_, err = wt.Add("deploy.sh")
	require.NoError(t, err)
	_, err = wt.Commit("add script", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
	require.NoError(t, err)

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	apply.Plan = plan
	res, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	workRepo, err := git.PlainOpen(res.RepoPath)
	require.NoError(t, err)
	head, err := workRepo.CommitObject(plumbing.NewHash(res.Head))
	require.NoError(t, err)
	tree, err := head.Tree()
	require.NoError(t, err)
	entry, err := tree.FindEntry("deploy.sh")
	require.NoError(t, err)
	assert.Equal(t, filemode.Executable, entry.Mode, "新文件必须保留可执行位")
}

// TestCleanupStaleProducts 回归:过期移植产物必须被清掉,
// 但**还没推送成功**的那个必须保留。
//
// 产物只在「apply 之后紧跟着的那次 push」里有用。留着就是纯占地方 ——
// 实测一次泄漏能攒到 5.48 GB(103 个目录)。
// 但只看「多旧」就删,会把待推送的产物删掉:推送必然失败且不可恢复。
func TestCleanupStaleProducts(t *testing.T) {
	root := t.TempDir()
	// 造一个「很旧」的产物目录(把 mtime 拨到两天前)。
	old := filepath.Join(root, productDirPrefix+"stale")
	require.NoError(t, os.MkdirAll(old, 0o755))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))

	// 造一个「很旧但还没推送」的产物目录:必须保留。
	pending := filepath.Join(root, productDirPrefix+"pending")
	require.NoError(t, os.MkdirAll(pending, 0o755))
	require.NoError(t, os.Chtimes(pending, past, past))

	// 造一个「刚建的」产物目录:不该被清。
	fresh := filepath.Join(root, productDirPrefix+"fresh")
	require.NoError(t, os.MkdirAll(fresh, 0o755))

	CleanupStaleProducts(root, pending)

	_, err := os.Stat(old)
	assert.True(t, os.IsNotExist(err), "过期产物必须被清掉")
	_, err = os.Stat(pending)
	assert.NoError(t, err, "还没推送成功的产物不能被清掉")
	_, err = os.Stat(fresh)
	assert.NoError(t, err, "刚建的产物不能被清掉")
}

// TestCleanupStaleProductsPendingHasUpperBound 待推送的产物**也有上限**:
// 一直不推送的产物不能重新变成泄漏(实测一次泄漏攒到 5.48 GB)。
func TestCleanupStaleProductsPendingHasUpperBound(t *testing.T) {
	root := t.TempDir()
	pending := filepath.Join(root, productDirPrefix+"pending")
	require.NoError(t, os.MkdirAll(pending, 0o755))
	old := time.Now().Add(-pendingProductTTL - time.Hour)
	require.NoError(t, os.Chtimes(pending, old, old))

	CleanupStaleProducts(root, pending)

	_, err := os.Stat(pending)
	assert.True(t, os.IsNotExist(err), "超过保护期上限的待推送产物也必须被清掉")
}
