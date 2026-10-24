package mirror

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishAbortsOnSecretInBinaryBlob 二进制 blob 里的密钥必须中止发布。
//
// 回归背景:二进制/超限 blob 走"原样保留"路径,此前**完全跳过痕迹清理**
// (只查 module path 残留)⇒ 二进制里的密钥(ghp_xxx 等)原样进公开 module,
// 而密钥类(mandatory)规则一次都不会触发 —— 规则形同虚设。
func TestPublishAbortsOnSecretInBinaryBlob(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	mirrorDir := filepath.Join(root, "mirror.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(mirrorDir, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
		// 二进制里带着密钥:改写不了,但绝不许发。
		{path: "secret.bin", data: "\x00\x01ghp_AAAAAAAABBBBBBBBCCCCCCCC\x00payload"},
	})
	annotatedTag(t, r, "v0.0.2", h)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{mirrorDir}})
	require.NoError(t, err)

	rep, err := Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.2"},
		ScrubRules: []ScrubRule{{
			Name:     "token",
			Category: "secret",
			Pattern:  regexp.MustCompile(`ghp_[A-Za-z0-9]+`),
			Severity: ScrubMandatory,
		}},
	})
	require.Error(t, err, "二进制里的密钥必须中止发布")
	assert.Contains(t, err.Error(), "secret.bin")

	// 报告里如实列出
	require.Len(t, rep.Tags, 1)
	assert.NotEmpty(t, rep.Tags[0].ScrubHits, "命中必须报出来")

	// **未推送**:镜像远端不应有任何引用
	mirror, err := git.PlainOpen(mirrorDir)
	require.NoError(t, err)
	_, err = mirror.Reference(plumbing.NewTagReferenceName("v0.0.2"), false)
	assert.Error(t, err, "密钥文件不该被推出去")
}
