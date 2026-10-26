package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/model"
)

// newRebuildTestService 造一个带真实临时目录的服务(只测 rebuild 语义,不碰 executor)。
func newRebuildTestService(t *testing.T, key string) (*Service, string, func()) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}, &model.Repo{}))
	// sqlite ":memory:" 每个连接是独立库 ⇒ 强制单连接
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	taskDAO := dao.NewSyncTaskDAO(db)
	runDAO := dao.NewSyncRunDAO(db)
	runStepDAO := dao.NewSyncRunStepDAO(db)
	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)

	tempDir := t.TempDir()
	svc := &Service{
		tasks:          NewTaskService(taskDAO, runDAO, runStepDAO, repoDAO),
		guard:          newLocalGuard(2),
		pendingWebhook: newPendingTriggerSet(),
		bgCtx:          context.Background(),
		config:         &model.Config{Git: model.GitConfig{TempDir: tempDir}},
	}
	// repo 服务要装上:executor 的 prepareRunContext 会查源仓库
	// (查不到 ⇒ 返回错误而不是 panic,接线用例才能正常跑完)。
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	svc.repos = NewRepoService(repoDAO, platformDAO, nil)
	require.NoError(t, taskDAO.Create(&model.SyncTask{
		Key: key, Name: "rebuild", Enabled: true, SourceBranch: "main", TargetBranch: "main",
		SourceRepoKey: "no-such-repo", TargetRepoKey: "no-such-repo",
	}))
	return svc, tempDir, func() {}
}

// seedWorkDir 在任务工作目录里放一个标记文件。
func seedWorkDir(t *testing.T, svc *Service, key string) string {
	t.Helper()
	dir := svc.GetTempDir(key)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "repo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo", "marker"), []byte("x"), 0o644))
	return dir
}

// TestRunWithRebuildPrep_ClearsWorkDir rebuild 触发器必须清空工作目录。
//
// 回归背景(P0,"报告与事实相反"):MCP 工具 rebuild_task 的描述写的是
// "清空任务工作目录并全量重建",实现却只是重新触发一次同步 ——
// 目录根本没清 ⇒ 所谓"重建"是增量同步,workdir 损坏/浅克隆修复不了。
//
// 这条用例是**反向验证**:去掉 runWithRebuildPrep 的清理,本用例变红。
func TestRunWithRebuildPrep_ClearsWorkDir(t *testing.T) {
	const key = "rebuild-clear"
	svc, _, _ := newRebuildTestService(t, key)
	dir := seedWorkDir(t, svc, key)

	require.NoError(t, svc.runWithRebuildPrep(key, model.TriggerRebuild))
	assert.NoDirExists(t, dir, "rebuild 必须清空工作目录(否则损坏的 workdir 永远修不好)")
}

// TestRunWithRebuildPrep_AIRebuildTrigger AI 发起的重建语义相同。
//
// MCP 的 rebuild_task 用的是 "ai_rebuild" 触发器 ⇒ 这个触发器也必须清目录,
// 否则 AI 侧点"全量重建"依然只是增量同步。
func TestRunWithRebuildPrep_AIRebuildTrigger(t *testing.T) {
	const key = "rebuild-ai"
	svc, _, _ := newRebuildTestService(t, key)
	dir := seedWorkDir(t, svc, key)

	require.NoError(t, svc.runWithRebuildPrep(key, model.TriggerAIRebuild))
	assert.NoDirExists(t, dir, "ai_rebuild 同样必须清空工作目录")
}

// TestRunWithRebuildPrep_KeepsWorkDirForOtherTriggers 其它触发器**不能**清目录。
//
// 增量同步保留工作区才有增量收益(全量克隆是大仓的主要成本)。
func TestRunWithRebuildPrep_KeepsWorkDirForOtherTriggers(t *testing.T) {
	const key = "rebuild-keep"
	svc, _, _ := newRebuildTestService(t, key)
	dir := seedWorkDir(t, svc, key)

	for _, trigger := range []string{model.TriggerManual, model.TriggerCron, model.TriggerWebhook} {
		require.NoError(t, svc.runWithRebuildPrep(key, trigger))
		assert.DirExists(t, dir, "触发器 %q 不得清空工作目录", trigger)
	}
}

// TestRunTaskWithTrigger_BusyTask_RebuildKeepsWorkDir 任务在跑时,重建请求
// **不得**把正在跑的工作树删掉。
//
// 回归背景(P0):壳层的 POST /ops/rebuild 原来是"先删工作目录,再受理"——
// 顺序正好反了:任务正在跑时,目录已经被删了(跑着的同步必炸),
// 而受理又因 ErrTaskRunning 失败 ⇒ 净效果是"把活着的同步搞死,重建还没排上"。
//
// 现在清目录归 core,在**拿到执行权之后**做 ⇒ 抢不到执行权就一步都不动。
func TestRunTaskWithTrigger_BusyTask_RebuildKeepsWorkDir(t *testing.T) {
	const key = "rebuild-busy"
	svc, _, _ := newRebuildTestService(t, key)
	dir := seedWorkDir(t, svc, key)

	// 占住同 taskKey 的执行权(模拟"已经有一个在跑")
	release, err := svc.guard.Acquire(context.Background(), key)
	require.NoError(t, err)
	defer release()

	err = svc.RunTaskWithTrigger(context.Background(), key, model.TriggerRebuild, nil)
	require.ErrorIs(t, err, ErrTaskRunning)
	assert.DirExists(t, dir, "任务在跑时不得清空工作目录(会把正在跑的同步的工作树删掉)")
}

// TestIsRebuildTrigger 触发器判定是 rebuild 语义的唯一事实源。
func TestIsRebuildTrigger(t *testing.T) {
	assert.True(t, model.IsRebuildTrigger(model.TriggerRebuild))
	assert.True(t, model.IsRebuildTrigger(model.TriggerAIRebuild))
	assert.False(t, model.IsRebuildTrigger(model.TriggerManual))
	assert.False(t, model.IsRebuildTrigger(model.TriggerCron))
	assert.False(t, model.IsRebuildTrigger(model.TriggerWebhook))
	assert.False(t, model.IsRebuildTrigger(""))
}

// TestRunTaskWithTrigger_RebuildTrigger_ClearsWorkDir **接线**验证:
// RunTaskWithTrigger 真的会在执行前清空工作目录。
//
// 这条用例是**反向验证**:把 RunTaskWithTrigger 里那句 runWithRebuildPrep
// 去掉,本用例变红(目录还在)。
//
// 执行本身会失败(测试里没有配仓库),但那不重要 —— 重要的是
// 「清目录」发生在「执行」之前。
func TestRunTaskWithTrigger_RebuildTrigger_ClearsWorkDir(t *testing.T) {
	const key = "rebuild-wiring"
	svc, _, _ := newRebuildTestService(t, key)
	dir := seedWorkDir(t, svc, key)

	exec, err := executor.NewExecutor(svc)
	require.NoError(t, err)
	svc.executor = exec

	// 没有配置源/目标仓库 ⇒ 执行必然失败;但工作目录必须已经被清空。
	err = svc.RunTaskWithTrigger(context.Background(), key, model.TriggerRebuild, nil)
	require.Error(t, err, "测试里没有仓库,执行本来就该失败")
	assert.NoDirExists(t, dir, "RunTaskWithTrigger 必须在执行前清空工作目录")
}
