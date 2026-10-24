package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"
)

// driftGit 在 dir 里跑一条 git 命令(带固定身份)。
func driftGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// TestDetectDrift_ComputesRealAheadBehind 漂移检测必须给出**真实的** ahead/behind。
//
// 回归背景:rev-list --count A..B 要求两个 sha 都在本地对象库里;
// 远端(ls-remote 拿到的)那个 sha 没被 fetch 过 ⇒ 命令直接失败,
// 旧实现静默给 0/0 并报 "diverged" ⇒ "漂移了多少"这个最该有的信息永远是空的。
// 修法:先 fetch 源端分支(带凭证)再算。
func TestDetectDrift_ComputesRealAheadBehind(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	root := t.TempDir()
	src := filepath.Join(root, "src.git")
	seed := filepath.Join(root, "seed")
	require.NoError(t, os.MkdirAll(seed, 0o750))
	driftGit(t, seed, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v1"), 0o640))
	driftGit(t, seed, "add", ".")
	driftGit(t, seed, "commit", "-m", "c1")
	driftGit(t, seed, "init", "--bare", "-b", "main", src)
	driftGit(t, seed, "remote", "add", "origin", src)
	driftGit(t, seed, "push", "-q", "origin", "main")

	// 工作区:从源端克隆(模拟 executor 的 clone)
	work := filepath.Join(root, "work")
	driftGit(t, root, "clone", "-q", src, work)

	// 装配 service
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "drift.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Platform{}, &model.Repo{}, &model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}))

	plat := &model.Platform{Key: "gh", Name: "GH", Type: "github", APIURL: "https://api.github.invalid"}
	require.NoError(t, db.Create(plat).Error)
	require.NoError(t, db.Create(&model.Repo{
		Key: "acme/src", Name: "src", Platform: "github", PlatformID: plat.ID,
		PlatformOwner: "acme", PlatformRepo: "src", CloneURL: src, DefaultBranch: "main",
	}).Error)

	taskKey := "drift-task"
	require.NoError(t, db.Create(&model.SyncTask{
		Key: taskKey, Name: "drift", SourceRepoKey: "acme/src", TargetRepoKey: "acme/src",
		SourceBranch: "main", TargetBranch: "main", Enabled: true,
	}).Error)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	mgr := sdkprov.NewManager(30 * 1e9)

	tempDir := filepath.Join(root, "tmp")
	require.NoError(t, os.MkdirAll(tempDir, 0o750))
	// 工作区要放在 service 的 TempDir 下:<TempDir>/<taskKey>/repo
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, taskKey), 0o750))
	driftGit(t, root, "clone", "-q", src, filepath.Join(tempDir, taskKey, "repo"))

	// **工作区克隆之后**源端才前进 ⇒ 工作区落后 1 个提交
	require.NoError(t, os.WriteFile(filepath.Join(seed, "b.txt"), []byte("v2"), 0o640))
	driftGit(t, seed, "add", ".")
	driftGit(t, seed, "commit", "-m", "c2")
	driftGit(t, seed, "push", "-q", "origin", "main")

	svc := &Service{
		config:    &model.Config{Git: model.GitConfig{TempDir: tempDir}},
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
		tasks: NewTaskService(
			dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db),
			dao.NewSyncRunStepDAO(db), repoDAO,
		),
	}

	rep, err := svc.DetectDrift(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rep.Items, 1)
	item := rep.Items[0]
	assert.True(t, item.Drifted, "源端前进了 ⇒ 应判定为漂移")
	assert.Equal(t, 1, item.RemoteAhead, "远端(源端)领先 1 个提交 —— 必须是真实数字")
	assert.Equal(t, 0, item.LocalAhead, "本地没有独有提交")
	assert.Contains(t, item.Message, "source ahead", "判词要说明是谁领先: %s", item.Message)
	assert.Equal(t, 1, rep.Drifted)
}

// TestDetectDrift_RespectsBudget 漂移检测必须有**总预算**。
//
// 回归背景:每个漂移任务都要 fetch 一次源端才能算出 ahead/behind,
// 耗时与"漂移任务数 × 单次 fetch 耗时"成正比。上游慢、任务多时
// (200 个漂移任务 × 30s 拨号超时)整轮能跑到几十分钟,而调用方早已超时 ——
// 服务端还在空转。预算耗尽后剩下的任务要如实标"未检查"。
func TestDetectDrift_RespectsBudget(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "drift2.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Platform{}, &model.Repo{}, &model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}))

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	mgr := sdkprov.NewManager(30 * 1e9)

	svc := &Service{
		config:    &model.Config{Git: model.GitConfig{TempDir: t.TempDir()}},
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
		tasks: NewTaskService(
			dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db),
			dao.NewSyncRunStepDAO(db), repoDAO,
		),
	}

	// 至少要有一个任务,循环才会走到预算判定
	require.NoError(t, db.Create(&model.SyncTask{
		Key: "t-budget", Name: "budget", SourceRepoKey: "acme/none",
		TargetRepoKey: "acme/none", SourceBranch: "main", TargetBranch: "main", Enabled: true,
	}).Error)

	// 预算已经耗尽的 ctx ⇒ 立刻如实报"未检查",不去碰网络
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	rep, err := svc.DetectDrift(ctx, nil)
	require.NoError(t, err)
	require.NotEmpty(t, rep.Warnings, "预算耗尽必须如实报出来")
	assert.Contains(t, rep.Warnings[0], "预算")
}
