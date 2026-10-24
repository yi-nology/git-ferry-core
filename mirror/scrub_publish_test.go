package mirror

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishScrubsInternalTraces 回归:镜像发布要**清掉内容里的内网痕迹**。
//
// 背景:身份改写只管 **module path** —— 而内容里还有别的内网标识:
// 内网 registry 地址、部署代号、令牌。它们原样进**公开 module** 就是泄漏
// (公开 module 是对外可见、任何人能 `go get` 的)。
//
// 历史同步侧早就有痕迹规则,镜像侧没有 —— 同一个仓、两条发布链路、
// 两套公开化口径。
func TestPublishScrubsInternalTraces(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
		{path: "deploy.yml", data: "image: registry.internal.corp/ci/base:1.2\nenv: PROD-SHANGHAI-01\n"},
	})
	annotatedTag(t, r, "v0.1.0", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	rep, err := Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.1.0"},
		ScrubRules: []ScrubRule{
			{
				Name: "strip-internal-registry", Category: "internal-host",
				Pattern:  regexp.MustCompile(`registry\.internal\.corp`),
				Severity: ScrubAuto, Replacement: "registry.public.example",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, rep.Tags, 1)
	require.Len(t, rep.Tags[0].ScrubHits, 1, "命中必须出现在报告里")

	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	f, err := blobAt(t, mirror, plumbing.NewTagReferenceName("v0.1.0"), "deploy.yml")
	require.NoError(t, err)
	data, err := readBlobFile(f)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "registry.internal.corp",
		"内网 registry 地址不许进公开 module:%s", data)
	assert.Contains(t, string(data), "registry.public.example")
}

// TestPublishAbortsOnMandatoryScrubHit 回归:密钥类命中必须**中止发布**。
//
// mandatory 规则不做自动替换(自动替换可能留下半吊子状态,比留痕更危险),
// 但「留痕」不等于「可以发布」—— 密钥进公开 module 就是泄漏。
// 历史同步侧的处理是「该文件不发布」;镜像发布是整棵树,只能整棵中止。
func TestPublishAbortsOnMandatoryScrubHit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
		{path: "ci.env", data: "TOKEN=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"},
	})
	annotatedTag(t, r, "v0.1.0", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	_, err = Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.1.0"},
		ScrubRules: []ScrubRule{
			{
				Name: "token", Category: "secret",
				Pattern:  regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),
				Severity: ScrubMandatory,
			},
		},
	})
	require.Error(t, err, "密钥类命中必须中止发布")
	assert.Contains(t, err.Error(), "中止")

	// **未推送**:远端不应有任何引用。
	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	_, err = mirror.Reference(plumbing.NewTagReferenceName("v0.1.0"), false)
	assert.Error(t, err, "中止的发布不许留下 tag")
	_, err = mirror.Reference(plumbing.NewBranchReferenceName("main"), false)
	assert.Error(t, err, "中止的发布不许留下 main 快照")
}
