package mirror

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fixtureSig = object.Signature{
	Name:  "fixture",
	Email: "fixture@example.com",
	When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

type fileSpec struct {
	path string
	data string
	exec bool
}

// commitFiles 在 dir 写入文件并提交,返回仓库与 commit。
func commitFiles(t *testing.T, dir string, files []fileSpec) (*git.Repository, plumbing.Hash) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, f.path)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(f.data), 0o644))
		if f.exec {
			require.NoError(t, os.Chmod(p, 0o755))
		}
	}
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true}))
	h, err := w.Commit("fixture", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
	require.NoError(t, err)
	return r, h
}

func annotatedTag(t *testing.T, r *git.Repository, name string, h plumbing.Hash) {
	t.Helper()
	_, err := r.CreateTag(name, h, &git.CreateTagOptions{
		Message: "fixture tag " + name,
		Tagger:  &fixtureSig,
	})
	require.NoError(t, err)
}

// makeCanonical 构造 canonical 仓库(含源前缀的文本/脚本/二进制文件,
// 附注 tag v0.0.1,远端 github 指向本地 bare 镜像)。
func makeCanonical(t *testing.T) (dir, mirrorDir string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "canonical")
	mirrorDir = filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{"go.mod", "module example.com/foo\n\ngo 1.21\n", false},
		{"main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/foo/sub\"\n)\n\nfunc main() { fmt.Println(sub.Hello()) }\n", false},
		{"sub/sub.go", "package sub\n\nfunc Hello() string { return \"hello\" }\n", false},
		{"README.md", "# foo\n\n用法: go get example.com/foo@latest,子路径见 example.com/foo/llm。\n", false},
		{"bin.dat", "example.com/foo\x00payload", false},
		{"run.sh", "#!/bin/sh\necho example.com/foo\n", true},
	})
	annotatedTag(t, r, "v0.0.1", h)
	_, err = r.CreateRemote(&config.RemoteConfig{
		Name: "github",
		URLs: []string{mirrorDir},
	})
	require.NoError(t, err)
	return dir, mirrorDir
}

func snapshotFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	}))
	return out
}

func blobAt(t *testing.T, r *git.Repository, ref plumbing.ReferenceName, path string) (*object.File, error) {
	t.Helper()
	commit, err := resolveRefCommit(r, ref)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	return tree.File(path)
}

func TestPublishEndToEndAndIdempotent(t *testing.T) {
	dir, mirrorDir := makeCanonical(t)
	ctx := context.Background()

	baseline := snapshotFiles(t, dir)
	preGoMod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)

	opts := Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	}
	rep1, err := Publish(ctx, opts)
	require.NoError(t, err)
	require.Len(t, rep1.Tags, 1)
	tr1 := rep1.Tags[0]

	// 报告:改写清单覆盖全部文本文件,二进制被排除
	assert.ElementsMatch(t,
		[]string{"README.md", "go.mod", "main.go", "run.sh"},
		tr1.ReplacedFiles)
	assert.NotEmpty(t, tr1.SourceCommit)
	assert.NotEmpty(t, tr1.Tree)
	assert.Equal(t, "go list -m example.org/bar@v0.0.1", tr1.VerifyCmd)

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)

	// 快照语义:孤儿 commit(无父提交)
	commit, err := resolveRefCommit(mirror, plumbing.NewTagReferenceName("v0.0.1"))
	require.NoError(t, err)
	assert.Empty(t, commit.ParentHashes)
	assert.Equal(t, tr1.Commit, commit.Hash.String())

	// 身份改写结果:go.mod / import / 文档 / 脚本,二进制原样,exec 位保留
	goModFile, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.0.1"), "go.mod")
	require.NoError(t, err)
	goModData, err := readBlobFile(goModFile)
	require.NoError(t, err)
	assert.Equal(t, "module example.org/bar\n\ngo 1.21\n", string(goModData))

	mainFile, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.0.1"), "main.go")
	require.NoError(t, err)
	mainData, err := readBlobFile(mainFile)
	require.NoError(t, err)
	assert.Contains(t, string(mainData), "\"example.org/bar/sub\"")
	assert.NotContains(t, string(mainData), "example.com/foo")

	binFile, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.0.1"), "bin.dat")
	require.NoError(t, err)
	binData, err := readBlobFile(binFile)
	require.NoError(t, err)
	assert.Equal(t, "example.com/foo\x00payload", string(binData))

	shFile, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.0.1"), "run.sh")
	require.NoError(t, err)
	assert.Equal(t, filemode.Executable, shFile.Mode)
	shData, err := readBlobFile(shFile)
	require.NoError(t, err)
	assert.Contains(t, string(shData), "echo example.org/bar")
	assert.NotContains(t, string(shData), "example.com/foo")

	// main 快照指向合成 commit
	mainRef, err := mirror.Reference(plumbing.NewBranchReferenceName("main"), false)
	require.NoError(t, err)
	assert.Equal(t, tr1.Commit, mainRef.Hash().String())

	// canonical 零改动:工作区文件一致,合成对象不落入 canonical 对象库
	assert.Equal(t, baseline, snapshotFiles(t, dir))
	postGoMod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, preGoMod, postGoMod)
	canonical, err := git.PlainOpen(dir)
	require.NoError(t, err)
	commitHash := plumbing.NewHash(tr1.Commit)
	assert.Error(t, canonical.Storer.HasEncodedObject(commitHash), "合成 commit 不应进入 canonical 对象库")

	// canonical 的 tag 引用不被镜像发布污染(bash 版在 worktree 中
	// tag -f 曾覆盖共享 refs,导致 canonical 路径 go get 身份错误)
	canonCommit, err := resolveRefCommit(canonical, plumbing.NewTagReferenceName("v0.0.1"))
	require.NoError(t, err)
	assert.Equal(t, tr1.SourceCommit, canonCommit.Hash.String())

	// 幂等重跑:tree 与 commit 完全一致,远端 tree 匹配
	rep2, err := Publish(ctx, opts)
	require.NoError(t, err)
	require.Len(t, rep2.Tags, 1)
	tr2 := rep2.Tags[0]
	assert.Equal(t, tr1.Tree, tr2.Tree)
	assert.Equal(t, tr1.Commit, tr2.Commit)
	assert.True(t, tr2.RemoteTagExisted)
	assert.True(t, tr2.TreeMatchedRemote)
	assert.False(t, tr2.OverwroteDivergent)
}

func readBlobFile(f *object.File) ([]byte, error) {
	r, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func TestPublishBuildGateAbortsBeforePush(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	// 改写后仍引用 example.org/bar/nope,但该包不存在 → 门禁失败
	r, h := commitFiles(t, dir, []fileSpec{
		{"go.mod", "module example.com/foo\n\ngo 1.21\n", false},
		{"main.go", "package main\n\nimport _ \"example.com/foo/nope\"\n\nfunc main() {}\n", false},
	})
	annotatedTag(t, r, "v0.9.9", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	_, err = Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.9.9"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "编译门禁")

	// 未推送:镜像远端不应有任何引用
	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	_, err = mirror.Reference(plumbing.NewTagReferenceName("v0.9.9"), false)
	assert.Error(t, err)
	_, err = mirror.Reference(plumbing.NewBranchReferenceName("main"), false)
	assert.Error(t, err)
}

func TestPublishDivergentRemoteGuard(t *testing.T) {
	dir, mirrorDir := makeCanonical(t)
	ctx := context.Background()
	opts := Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	}
	_, err := Publish(ctx, opts)
	require.NoError(t, err)

	// 伪造远端分歧:内容不同的仓库强推同名 tag
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, os.MkdirAll(other, 0o755))
	r2, h2 := commitFiles(t, other, []fileSpec{
		{"go.mod", "module other.com/x\n\ngo 1.21\n", false},
		{"a.txt", "divergent\n", false},
	})
	annotatedTag(t, r2, "v0.0.1", h2)
	_, err = r2.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)
	require.NoError(t, r2.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		RefSpecs:   []config.RefSpec{"refs/tags/v0.0.1:refs/tags/v0.0.1"},
		Force:      true,
	}))

	// 默认拒绝覆盖已发布版本
	_, err = Publish(ctx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "拒绝覆盖")

	// 显式允许后覆盖成功
	opts.AllowOverwrite = true
	rep, err := Publish(ctx, opts)
	require.NoError(t, err)
	tr := rep.Tags[0]
	assert.True(t, tr.OverwroteDivergent)

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	commit, err := resolveRefCommit(mirror, plumbing.NewTagReferenceName("v0.0.1"))
	require.NoError(t, err)
	assert.Equal(t, tr.Tree, commit.TreeHash.String())
}

func TestPublishRejectsInvalidInput(t *testing.T) {
	dir, _ := makeCanonical(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"源模块与 go.mod 不一致", func(o *Options) {
			o.Mapping = Mapping{Source: "example.com/wrong", Target: "example.org/bar"}
		}, "不一致"},
		{"源与目标相同", func(o *Options) {
			o.Mapping = Mapping{Source: "example.com/foo", Target: "example.com/foo"}
		}, "无改写意义"},
		{"tag 不存在", func(o *Options) {
			o.Mapping = Mapping{Source: "example.com/foo", Target: "example.org/bar"}
			o.Tags = []string{"v9.9.9"}
		}, "不存在"},
		{"远端名非法", func(o *Options) {
			o.Mapping = Mapping{Source: "example.com/foo", Target: "example.org/bar"}
			o.Remote = "-evil"
		}, "'-'"},
		{"未提供 tag", func(o *Options) {
			o.Tags = nil
		}, "至少"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{RepoDir: dir, Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"}, Tags: []string{"v0.0.1"}}
			tc.mutate(&opts)
			_, err := Publish(ctx, opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestReadModulePath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module \"example.com/quoted\"\n\ngo 1.21\n"), 0o644))
	p, err := readModulePath(dir)
	require.NoError(t, err)
	assert.Equal(t, "example.com/quoted", p)
}

func TestVerifyModuleRejectsInvalidInput(t *testing.T) {
	assert.Error(t, VerifyModule(context.Background(), "example.com/x", "has space"))
	assert.Error(t, VerifyModule(context.Background(), "", "v1.0.0"))
	assert.Error(t, VerifyModule(context.Background(), "example.com/x", "-bad"))
}

func TestVerifyModuleOfflineFails(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	oldAttempts, oldWait := verifyAttempts, verifyRetryWait
	verifyAttempts, verifyRetryWait = 1, 0
	defer func() { verifyAttempts, verifyRetryWait = oldAttempts, oldWait }()

	err := VerifyModule(context.Background(), "example.com/some/module", "v0.1.0")
	require.Error(t, err)
}

// TestVerifyModuleLive 走真实网络验证已发布的 GitHub 镜像模块。
// 需要 MIRROR_LIVE=1 触发。
func TestVerifyModuleLive(t *testing.T) {
	if os.Getenv("MIRROR_LIVE") == "" {
		t.Skip("跳过真网验证(设置 MIRROR_LIVE=1 触发)")
	}
	require.NoError(t, VerifyModule(context.Background(), "github.com/yi-nology/agentkit", "v0.9.5"))
}
