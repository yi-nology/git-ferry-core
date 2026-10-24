package mirror

import (
	"context"
	"go/format"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompareVersions 单元:版本比较的全序。
//
// 它决定「公开仓的 main 快照该指向哪个已发布版本」——
// 判错的后果是默认分支指向旧版本(补历史时回退)。
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.1.0", "v1.0.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v0.9.0", "v1.0.0", -1}, // 补历史场景:main 不该被它带走
		{"v1.0.0", "v1.0.0-rc1", 1},
		{"v1.0.0-rc1", "v1.0.0-rc2", -1},
		// 字母数字混合标识按 ASCII 排序(语义化版本的规矩):"rc10" < "rc2"。
		// 只有**纯数字**段才按数字比。
		{"v1.0.0-rc2", "v1.0.0-rc10", 1},
		{"v1.0.0-2", "v1.0.0-10", -1}, // 纯数字段按数字比
		{"v1.0.0-rc1", "v1.0.0-beta1", 1},
		{"1.0.0", "v1.0.0", 0},                // 没有 v 前缀也认
		{"v1.0.0+build1", "v1.0.0+build2", 0}, // 构建元数据不参与比较
	}
	for _, c := range cases {
		got := compareVersions(c.a, c.b)
		assert.Equal(t, c.want, got, "compareVersions(%q,%q)", c.a, c.b)
		assert.Equal(t, -got, compareVersions(c.b, c.a), "compareVersions 必须反对称")
	}
}

// TestMainSnapshotNotRegressedByBackfill 回归:补发历史版本**不得**把公开仓的
// 默认分支(main)回退到旧快照。
//
// 背景(第四轮实测):原实现是「main ← 本次发布的 tag」,
// 于是先发 v1.0.0、再补发 v0.9.0 ⇒ main 变成 v0.9.0 的快照 ——
// 任何 `git clone` 公开仓、或 `go get module@main` 的人拿到的是过期代码。
func TestMainSnapshotNotRegressedByBackfill(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	// 两个版本,内容不同(⇒ 合成 tree 不同 ⇒ 合成 commit 不同)。
	r, hNew := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
	})
	annotatedTag(t, r, "v1.0.0", hNew)
	// 旧版本:在同一个仓里再提交一次(内容更旧)。
	wt, err := r.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\n// 老版本\nfunc main() {}\n"), 0o644))
	_, err = wt.Add("main.go")
	require.NoError(t, err)
	hOld, err := wt.Commit("old", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
	require.NoError(t, err)
	annotatedTag(t, r, "v0.9.0", hOld)

	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	ctx := context.Background()
	opts := Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
	}

	// 先发新版本。
	rep, err := Publish(ctx, Options{RepoDir: opts.RepoDir, Mapping: opts.Mapping, Tags: []string{"v1.0.0"}})
	require.NoError(t, err)
	require.Len(t, rep.Tags, 1)
	newCommit := rep.Tags[0].Commit

	// 再补发旧版本。
	_, err = Publish(ctx, Options{RepoDir: opts.RepoDir, Mapping: opts.Mapping, Tags: []string{"v0.9.0"}})
	require.NoError(t, err)

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	mainRef, err := mirror.Reference(plumbing.NewBranchReferenceName("main"), false)
	require.NoError(t, err)
	assert.Equal(t, newCommit, mainRef.Hash().String(),
		"补发历史版本不得把公开默认分支回退到旧快照")
}

// TestPublishedTreeIsGofmtClean 回归:镜像发布的产物必须 **gofmt 干净**。
//
// 背景(第四轮实测):镜像发布侧只改写、**不归一化**,而历史同步侧改写后还要 gofmt
// ⇒ 同一份源码经两条链路发布出**不同的字节**:
// 身份改写会改变 import 的字典序位置(git.enjoye.top/… → github.com/…),
// 于是公开仓的 gofmt CI 挂、`go get` 用户拿到未格式化代码,
// 而且**类型门禁看不出来**(编译不看 import 顺序)。
func TestPublishedTreeIsGofmtClean(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	// **源侧本身是 gofmt 干净的**("fmt" < "zzz…" 按字典序),
	// 但身份改写把 module path 换成首字母更小的("aaa…")⇒ import 顺序翻转。
	// 不归一化的话,公开仓里就是一份未格式化的 Go 文件。
	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module zzz.example/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nimport (\n\t\"fmt\"\n\t\"zzz.example/foo/sub\"\n)\n\nfunc main() { fmt.Println(sub.Hello()) }\n"},
		{path: "sub/sub.go", data: "package sub\n\nfunc Hello() string { return \"hello\" }\n"},
	})
	// 先确认源侧确实是 gofmt 干净的(否则这条断言证明不了什么)。
	srcMain, err := os.ReadFile(filepath.Join(dir, "main.go"))
	require.NoError(t, err)
	srcFormatted, ferr := format.Source(srcMain)
	require.NoError(t, ferr)
	require.Equal(t, string(srcFormatted), string(srcMain), "源侧夹具必须本身 gofmt 干净")
	annotatedTag(t, r, "v0.1.0", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	_, err = Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "zzz.example/foo", Target: "aaa.example/bar"},
		Tags:    []string{"v0.1.0"},
	})
	require.NoError(t, err)

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	f, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.1.0"), "main.go")
	require.NoError(t, err)
	data, err := readBlobFile(f)
	require.NoError(t, err)

	formatted, ferr := format.Source(data)
	require.NoError(t, ferr, "发布的 Go 文件必须是可解析的")
	assert.Equal(t, string(formatted), string(data),
		"发布的 Go 文件必须 gofmt 干净(身份改写会翻转 import 顺序,必须归一化)")
	assert.Contains(t, string(data), "aaa.example/bar/sub", "module path 改写生效")
}

// TestRepublishKeepsRemoteTagObject 幂等/重发时**不得改写远端 tag 对象**。
//
// 背景:源仓「重打」同名 tag(内容不变、提交元数据变了)⇒ 本次合成的
// commit 与远端已有的不同、但 tree 相同。此时若照旧强推,
// 远端 tag 对象的 hash 就变了 ⇒
// **已经 pin 了 pseudo-version**(形如 v0.0.0-<时间>-<commit12>)的消费者
// 拉不到那个版本了。module 的 zip/h1 只由 tree 决定(内容没变),
// 但对外看就是「这个 tag 被替换过」。
//
// 正确行为:内容一致 ⇒ 什么都不动。
func TestRepublishKeepsRemoteTagObject(t *testing.T) {
	dir, mirrorDir := makeCanonical(t)
	ctx := context.Background()
	opts := Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	}

	rep1, err := Publish(ctx, opts)
	require.NoError(t, err)
	require.Len(t, rep1.Tags, 1)
	require.False(t, rep1.Tags[0].TagAlreadyPublished, "首次发布当然要推")

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	before := remoteTagHash(t, mirror, "v0.0.1")

	// 源仓重打同名 tag:内容不变(tree 相同),提交元数据变了。
	//
	// go-git 不许空提交(clean working tree),所以直接用对象 API 造一个
	// 「同 tree、不同签名」的提交 —— 这正是「重打 tag」在现实里的形态。
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := r.Head()
	require.NoError(t, err)
	headCommit, err := r.CommitObject(head.Hash())
	require.NoError(t, err)
	otherSig := object.Signature{
		Name: "other", Email: "other@example.com",
		When: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	commitObj := &object.Commit{
		Author: otherSig, Committer: otherSig,
		Message:      "re-tag\n",
		TreeHash:     headCommit.TreeHash, // ← 同一个 tree
		ParentHashes: []plumbing.Hash{headCommit.Hash},
	}
	enc := r.Storer.NewEncodedObject()
	require.NoError(t, commitObj.Encode(enc))
	h2, err := r.Storer.SetEncodedObject(enc)
	require.NoError(t, err)
	require.NoError(t, r.DeleteTag("v0.0.1"))
	annotatedTag(t, r, "v0.0.1", h2)

	rep2, err := Publish(ctx, opts)
	require.NoError(t, err)
	require.Len(t, rep2.Tags, 1)
	assert.True(t, rep2.Tags[0].TagAlreadyPublished,
		"内容一致应当被识别为「远端已有同内容版本」")
	assert.NotEqual(t, rep1.Tags[0].Commit, rep2.Tags[0].Commit,
		"本次合成的 commit 确实变了(源仓重打了 tag)—— 这正是不能重推的理由")

	after := remoteTagHash(t, mirror, "v0.0.1")
	assert.Equal(t, before, after,
		"远端 tag 对象不得被改写(pseudo-version 的稳定性依赖它)")
}

// remoteTagHash 取远端同名 tag 指向的对象 hash。
func remoteTagHash(t *testing.T, mirror *git.Repository, tag string) string {
	t.Helper()
	ref, err := mirror.Reference(plumbing.NewTagReferenceName(tag), true)
	require.NoError(t, err)
	return ref.Hash().String()
}
