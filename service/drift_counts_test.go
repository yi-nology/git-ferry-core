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

// TestDetectDrift_ComparesSourceWithTarget 漂移检测要比的是**源端 vs 目标端**。
//
// 语义（重写后）：漂移 = 目标端与源端不一致。
// 原来比的是"工作区本地分支 vs 源端远端" ⇒ 报出来的多半是"源端前进了"
// —— 那是**正常状态**(下次同步就推上去了),不是漂移;
// 而"目标端跑偏了(目标有源端没有的提交)"从来没被检查过。
//
// 三个场景各测一遍：
//   - 目标落后 ⇒ source ahead(下次同步会推上去)
//   - 目标多出提交 ⇒ target ahead(**真正的漂移**)
//   - 两边一致 ⇒ in sync
func TestDetectDrift_ComparesSourceWithTarget(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	root := t.TempDir()
	src := filepath.Join(root, "src.git")
	tgt := filepath.Join(root, "tgt.git")
	seed := filepath.Join(root, "seed")
	require.NoError(t, os.MkdirAll(seed, 0o750))
	driftGit(t, seed, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "a.txt"), []byte("v1"), 0o640))
	driftGit(t, seed, "add", ".")
	driftGit(t, seed, "commit", "-m", "c1")
	driftGit(t, seed, "init", "--bare", "-b", "main", src)
	driftGit(t, seed, "init", "--bare", "-b", "main", tgt)
	driftGit(t, seed, "remote", "add", "origin", src)
	driftGit(t, seed, "push", "-q", "origin", "main")
	// 目标先与源端一致
	driftGit(t, seed, "push", "-q", tgt, "main:main")

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "drift.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Platform{}, &model.Repo{}, &model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}))

	plat := &model.Platform{Key: "gh", Name: "GH", Type: "github", APIURL: "https://api.github.invalid"}
	require.NoError(t, db.Create(plat).Error)
	require.NoError(t, db.Create(&model.Repo{
		Key: "acme/src", Name: "src", Platform: "github", PlatformID: plat.ID,
		PlatformOwner: "acme", PlatformRepo: "src", CloneURL: src, DefaultBranch: "main",
	}).Error)
	require.NoError(t, db.Create(&model.Repo{
		Key: "acme/tgt", Name: "tgt", Platform: "github", PlatformID: plat.ID,
		PlatformOwner: "acme", PlatformRepo: "tgt", CloneURL: tgt, DefaultBranch: "main",
	}).Error)

	taskKey := "drift-task"
	require.NoError(t, db.Create(&model.SyncTask{
		Key: taskKey, Name: "drift", SourceRepoKey: "acme/src", TargetRepoKey: "acme/tgt",
		SourceBranch: "main", TargetBranch: "main", Enabled: true,
	}).Error)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	mgr := sdkprov.NewManager(30 * 1e9)

	tempDir := filepath.Join(root, "tmp")
	require.NoError(t, os.MkdirAll(tempDir, 0o750))

	svc := &Service{
		config:    &model.Config{Git: model.GitConfig{TempDir: tempDir}},
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
		tasks: NewTaskService(
			dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db),
			dao.NewSyncRunStepDAO(db), repoDAO,
		),
	}

	// ① 两边一致 ⇒ in sync
	rep, err := svc.DetectDrift(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rep.Items, 1)
	assert.False(t, rep.Items[0].Drifted, "两边一致不该判漂移: %s", rep.Items[0].Message)
	assert.Contains(t, rep.Items[0].Message, "in sync")

	// ② 目标多出提交 ⇒ target ahead(**真正的漂移**:目标有源端没有的东西)
	tgtWork := filepath.Join(root, "tgtwork")
	driftGit(t, root, "clone", "-q", tgt, tgtWork)
	require.NoError(t, os.WriteFile(filepath.Join(tgtWork, "only-target.txt"), []byte("x"), 0o640))
	driftGit(t, tgtWork, "add", ".")
	driftGit(t, tgtWork, "commit", "-m", "target-only")
	driftGit(t, tgtWork, "push", "-q", "origin", "main")

	rep, err = svc.DetectDrift(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rep.Items, 1)
	item := rep.Items[0]
	assert.True(t, item.Drifted)
	assert.Contains(t, item.Message, "target ahead", "目标有源端没有的提交 —— 这才是真漂移: %s", item.Message)
	assert.Equal(t, 1, item.RemoteAhead, "目标端独有 1 个提交")
	assert.Equal(t, 0, item.LocalAhead, "源端没有独有提交")

	// ③ 源端也前进 ⇒ 两边各有独有提交 ⇒ diverged
	require.NoError(t, os.WriteFile(filepath.Join(seed, "b.txt"), []byte("v2"), 0o640))
	driftGit(t, seed, "add", ".")
	driftGit(t, seed, "commit", "-m", "c2")
	driftGit(t, seed, "push", "-q", "origin", "main")

	rep, err = svc.DetectDrift(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rep.Items, 1)
	item = rep.Items[0]
	assert.True(t, item.Drifted)
	assert.Contains(t, item.Message, "diverged", "两边各有独有提交: %s", item.Message)
	assert.Equal(t, 1, item.LocalAhead, "源端独有 1 个提交 —— 必须是真实数字")
	assert.Equal(t, 1, item.RemoteAhead, "目标端独有 1 个提交")
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
