package mirror

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeGitURL 钉归一化语义:同仓不同写法归一到同一形态。
func TestNormalizeGitURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://github.com/org/bar.git", "github.com/org/bar"},
		{"https://github.com/org/bar", "github.com/org/bar"},
		{"https://user:tok@github.com/org/bar.git", "github.com/org/bar"},
		{"ssh://git@github.com/org/bar.git", "github.com/org/bar"},
		{"ssh://git@github.com:2222/org/bar.git", "github.com/org/bar"},
		{"git@github.com:org/bar.git", "github.com/org/bar"},
		{"github.com:org/bar.git", "github.com/org/bar"},
		{"HTTPS://GitHub.com/org/bar.git", "github.com/org/bar"},
		{"https://github.com/org/bar.git?x=1#f", "github.com/org/bar"},
		// 本地路径不剥 .git:那是磁盘上真实目录名的一部分(与 URL 的
		// 协议装饰不同),剥了反而会把两个不同目录当成同一仓。
		{"/tmp/mirror.git", "local:/tmp/mirror.git"},
		{"/tmp/mirror", "local:/tmp/mirror"},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, NormalizeGitURL(c.in), "in=%q", c.in)
	}
}

// TestSameGitLocation 同一性判定:等价形式=同仓;不同仓不等。
func TestSameGitLocation(t *testing.T) {
	assert.True(t, SameGitLocation("https://github.com/org/bar.git", "git@github.com:org/bar"))
	assert.True(t, SameGitLocation("ssh://git@github.com:22/org/bar.git", "https://github.com/org/bar"))
	assert.False(t, SameGitLocation("https://github.com/org/bar", "https://github.com/org/baz"))
	assert.False(t, SameGitLocation("https://github.com/org/bar", "/tmp/mirror"))
	assert.False(t, SameGitLocation("", "https://github.com/org/bar"))
}

// TestGuardSelfPublish_RefusesRemoteURLCollision 判据 2:镜像目标与
// canonical 其他远端同址(名字不同、URL 相同的错配)⇒ 拒绝。
//
// **变异验收**:这里用「https 形式 vs scp 形式」的等价 URL ——
// 如果有人删掉归一化、改成原始字符串比对,本用例立刻红(门禁有牙)。
func TestGuardSelfPublish_RefusesRemoteURLCollision(t *testing.T) {
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	_, err = r.CreateRemote(&config.RemoteConfig{
		Name: "origin", URLs: []string{"git@github.com:org/bar.git"},
	})
	require.NoError(t, err)

	err = guardSelfPublish(r, dir, "https://github.com/org/bar.git", "example.org/bar", "github")
	require.Error(t, err, "等价 URL 必须被识别为同址")
	assert.Contains(t, err.Error(), "自毁保护")
	assert.Contains(t, err.Error(), "origin")
}

// TestGuardSelfPublish_RefusesLocalPathSelf 本地路径守卫:目标就是
// 源工作区本身 ⇒ 拒绝。
func TestGuardSelfPublish_RefusesLocalPathSelf(t *testing.T) {
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	err = guardSelfPublish(r, dir, dir, "example.org/bar", "github")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "源仓库工作区本身")

	// 不同路径放行
	require.NoError(t, guardSelfPublish(r, dir, filepath.Join(filepath.Dir(dir), "other.git"), "example.org/bar", "github"))
}

// TestGuardSelfPublish_ModuleIdentityMismatch 判据 3:目标身份是网络
// 身份、URL 却指向别的 host ⇒ 拒绝(抓「remote 指回内网、TargetModule
// 是公开身份」的漏网配置)。
func TestGuardSelfPublish_ModuleIdentityMismatch(t *testing.T) {
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	err = guardSelfPublish(r, dir, "https://git.internal.example/enjoydream/bar.git", "github.com/org/bar", "github")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不一致")

	// 身份与 URL 一致 ⇒ 放行
	require.NoError(t, guardSelfPublish(r, dir, "https://github.com/org/bar.git", "github.com/org/bar", "github"))
}

// TestGuardSelfPublish_LocalPathSkipsModuleCheck 本地路径目标(测试/开发
// 常态)不做身份↔URL 判定 —— 没有可对齐的网络身份。
func TestGuardSelfPublish_LocalPathSkipsModuleCheck(t *testing.T) {
	dir := t.TempDir()
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	mirror := filepath.Join(filepath.Dir(dir), "mirror.git")
	require.NoError(t, guardSelfPublish(r, dir, mirror, "github.com/org/bar", "github"))
}

// TestPublishAbortsOnSelfDestructSameURL 端到端:镜像远端配成源仓库
// origin 的同址仓库 ⇒ Publish 拒绝且**零写入**(远端无任何引用)。
func TestPublishAbortsOnSelfDestructSameURL(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "canonical")
	bare := filepath.Join(root, "home.git")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	_, err := git.PlainInit(bare, true)
	require.NoError(t, err)

	r, h := commitFiles(t, dir, []fileSpec{
		{path: "go.mod", data: "module example.com/foo\n\ngo 1.21\n"},
		{path: "main.go", data: "package main\n\nfunc main() {}\n"},
	})
	annotatedTag(t, r, "v0.0.1", h)
	// github 镜像远端被错配成 origin 同址的 bare(此处即同一 URL)。
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{bare}})
	require.NoError(t, err)
	_, err = r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	require.NoError(t, err)

	_, err = Publish(context.Background(), Options{
		RepoDir: dir,
		Mapping: Mapping{Source: "example.com/foo", Target: "example.org/bar"},
		Tags:    []string{"v0.0.1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "自毁保护")

	// 零写入:bare 里没有任何引用(tag/main 都没进来)。
	m, err := git.PlainOpen(bare)
	require.NoError(t, err)
	_, err = m.Reference(plumbing.NewTagReferenceName("v0.0.1"), false)
	assert.Error(t, err, "中止后远端不得有 tag")
	_, err = m.Reference(plumbing.NewBranchReferenceName("main"), false)
	assert.Error(t, err, "中止后远端不得有 main")
}
