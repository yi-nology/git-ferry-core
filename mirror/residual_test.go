package mirror

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishAbortsOnResidualIdentity 回归:改写不掉的二进制/超限文件里
// **不许藏着内网标识**。
//
// 背景(第四轮实测):二进制与超限 blob 走「原样保留」路径,
// 而「原样保留」被当成了「里面没有内网标识」—— 于是
// `bin.dat`(`example.com/foo\x00payload`)原样进了公开仓。
// 报告还写着「二进制被排除在改写之外」,看上去是有意为之。
//
// 现在残留判据不是「文件大不大」,而是「**规则本该改写这里,却没改写**」;
// 命中即中止发布(不推送),并把文件列出来。
func TestPublishAbortsOnResidualIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
		// 二进制里带着内网 module path:改写不了,也不许发。
		{path: "bin.dat", data: "example.com/foo\x00payload"},
	})
	annotatedTag(t, r, "v0.0.1", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	rep, err := Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	})
	require.Error(t, err, "带内网标识残留的文件必须中止发布")
	assert.Contains(t, err.Error(), "残留")
	assert.Contains(t, err.Error(), "bin.dat")

	// 报告里也要如实列出残留文件(不能只报错不给清单)。
	require.Len(t, rep.Tags, 1)
	assert.Equal(t, []string{"bin.dat"}, rep.Tags[0].ResidualFiles)

	// **未推送**:镜像远端不应有任何引用。
	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	_, err = mirror.Reference(plumbing.NewTagReferenceName("v0.0.1"), false)
	assert.Error(t, err)
	_, err = mirror.Reference(plumbing.NewBranchReferenceName("main"), false)
	assert.Error(t, err)
}

// TestPublishRewritesSymlinkTarget 回归:symlink 的**目标**也要过改写。
//
// symlink 的 blob 内容就是目标路径串 —— 里面照样可能写着内网 module path
// (指向内网目录的软链)。不改写 = 内网标识照样进公开仓。
func TestPublishRewritesSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
		{path: "link", linkTo: "example.com/foo/target"},
	})
	annotatedTag(t, r, "v0.0.1", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	rep, err := Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	})
	require.NoError(t, err)
	require.Len(t, rep.Tags, 1)
	assert.Contains(t, rep.Tags[0].ReplacedFiles, "link",
		"symlink 目标被改写时必须计入改写清单")

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	linkFile, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.0.1"), "link")
	require.NoError(t, err)
	assert.Equal(t, filemode.Symlink, linkFile.Mode, "软链的 mode 必须保留")
	data, err := readBlobFile(linkFile)
	require.NoError(t, err)
	assert.Equal(t, "example.org/bar/target", string(data),
		"symlink 目标里的内网 module path 必须被改写")
}
