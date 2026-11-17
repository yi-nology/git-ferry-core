package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
	"gorm.io/gorm"
)

// setupCronTestDB 共享调度测试库:sync_tasks + 执行队列表(触发走 durable
// queue,避免在调度测试里真的跑 executor)。
func setupCronTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "cron.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}, &model.Repo{},
		&model.ExecutionJob{}, &model.ExecutionTaskState{}, model.RetrySchedule{}, model.MigrationMapping{},
	))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

// setupCronTestService 独立实例(可多个共享同一 db 模拟多副本)。
func setupCronTestService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	return &Service{
		tasks:  NewTaskService(repository.NewSyncTaskRepository(db), repository.NewSyncRunRepository(db), repository.NewSyncRunStepRepository(db), nil),
		jobs:   repository.NewExecutionJobRepository(db),
		bgCtx:  context.Background(),
		config: &model.Config{Sync: model.SyncConfig{DurableQueue: true}},
	}
}

func mustCreateCronTask(t *testing.T, svc *Service, key, expr string) *model.SyncTask {
	t.Helper()
	task := &model.SyncTask{
		Key: key, Name: key, Cron: expr, Enabled: true,
		SourceBranch: "main", TargetBranch: "main",
	}
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	return task
}

func pendingCronJobs(t *testing.T, svc *Service) []model.ExecutionJob {
	t.Helper()
	var jobs []model.ExecutionJob
	require.NoError(t, svc.jobs.DB().Where("status = ?", model.JobStatusPending).Find(&jobs).Error)
	return jobs
}

// TestCronTick_SeedsThenFiresOnce 共享调度:
//  1. next_fire_at 为空 ⇒ 只播种(不触发);
//  2. 到点 ⇒ 条件抢占一次并触发,next_fire_at 推进到未来;
//  3. 同一到点周期重复扫描不重复触发。
func TestCronTick_SeedsThenFiresOnce(t *testing.T) {
	db := setupCronTestDB(t)
	svc := setupCronTestService(t, db)
	task := mustCreateCronTask(t, svc, "cron-seed", "* * * * *")

	require.NoError(t, svc.runCronTick(context.Background()))
	got, err := svc.tasks.taskRepository.FindByKey(task.Key)
	require.NoError(t, err)
	require.NotNil(t, got.NextFireAt, "首次扫描必须播种 next_fire_at")
	assert.True(t, got.NextFireAt.After(time.Now()), "播种的是下一次触发,不得立即执行")
	assert.Empty(t, pendingCronJobs(t, svc), "播种不触发")

	// 模拟到点。
	require.NoError(t, db.Model(&model.SyncTask{}).Where("`key` = ?", task.Key).
		Update("next_fire_at", time.Now().Add(-time.Second)).Error)
	require.NoError(t, svc.runCronTick(context.Background()))

	require.Eventually(t, func() bool { return len(pendingCronJobs(t, svc)) == 1 },
		2*time.Second, 5*time.Millisecond, "到点必须触发一次(入队)")
	jobs := pendingCronJobs(t, svc)
	assert.Equal(t, model.TriggerCron, jobs[0].Trigger)
	assert.Equal(t, task.Key, jobs[0].TaskKey)

	// 下一轮扫描:next_fire_at 已推进,不得重复触发。
	require.NoError(t, svc.runCronTick(context.Background()))
	assert.Len(t, pendingCronJobs(t, svc), 1, "同一到点周期只允许一个实例触发一次")
}

// TestCron_CrossInstanceSharedView 实例 A 新建的 cron 任务,实例 B 无需重启
// 即在下一轮扫描看到并触发 —— 有效调度计划是 DB 单一事实源(#40)。
func TestCron_CrossInstanceSharedView(t *testing.T) {
	db := setupCronTestDB(t)
	a := setupCronTestService(t, db)
	b := setupCronTestService(t, db)

	task := mustCreateCronTask(t, a, "cron-cross", "*/5 * * * *")
	require.NoError(t, a.runCronTick(context.Background())) // A 播种
	// 模拟到点(此刻 A 若退出,B 仍应接手)。
	require.NoError(t, db.Model(&model.SyncTask{}).Where("`key` = ?", task.Key).
		Update("next_fire_at", time.Now().Add(-time.Second)).Error)

	// B 没有本地点表/注册动作,只在 DB 看到到期任务并抢占触发。
	require.NoError(t, b.runCronTick(context.Background()))
	require.Eventually(t, func() bool { return len(pendingCronJobs(t, b)) == 1 },
		2*time.Second, 5*time.Millisecond, "B 必须接手 A 的调度")

	// A 同时扫描也不得双触发(条件更新单胜者)。
	require.NoError(t, a.runCronTick(context.Background()))
	assert.Len(t, pendingCronJobs(t, a), 1, "跨实例同一到点周期不得重复触发")
}

// TestCron_ModificationVisibleToOtherInstance 改 cron/禁用只改 DB,
// 其它实例下一轮看到同一计划(不再有各实例各一份的旧表达式)。
func TestCron_ModificationVisibleToOtherInstance(t *testing.T) {
	db := setupCronTestDB(t)
	a := setupCronTestService(t, db)
	b := setupCronTestService(t, db)

	// 用两个「下一次触发时间必然不同」的表达式(避免 * * * * 与
	// */10 * * * * 在整十分钟点撞成同一时刻,造成测试 flaky)。
	task := mustCreateCronTask(t, a, "cron-mod", "0 0 1 1 *")
	require.NoError(t, a.runCronTick(context.Background()))
	old, err := a.tasks.taskRepository.FindByKey(task.Key)
	require.NoError(t, err)
	require.NotNil(t, old.NextFireAt)

	// A 修改表达式 ⇒ 排程清空,B 下一轮按新表达式重新播种。
	newExpr := "0 0 1 2 *"
	_, err = a.UpdateTask(context.Background(), &model.UpdateTaskRequest{Key: task.Key, Cron: &newExpr})
	require.NoError(t, err)
	mid, err := a.tasks.taskRepository.FindByKey(task.Key)
	require.NoError(t, err)
	assert.Nil(t, mid.NextFireAt, "cron 变更必须清空旧排程,防旧表达式继续触发")

	require.NoError(t, b.runCronTick(context.Background()))
	got, err := b.tasks.taskRepository.FindByKey(task.Key)
	require.NoError(t, err)
	require.NotNil(t, got.NextFireAt)
	assert.NotEqual(t, *old.NextFireAt, *got.NextFireAt, "B 必须按新表达式重新播种")

	// 禁用 ⇒ 不再被扫描/触发。
	disabled := false
	_, err = a.UpdateTask(context.Background(), &model.UpdateTaskRequest{Key: task.Key, Enabled: &disabled})
	require.NoError(t, err)
	require.NoError(t, db.Model(&model.SyncTask{}).Where("`key` = ?", task.Key).
		Update("next_fire_at", time.Now().Add(-time.Second)).Error)
	require.NoError(t, b.runCronTick(context.Background()))
	assert.Empty(t, pendingCronJobs(t, b), "禁用任务不得触发")
}

// TestCron_InvalidExpressionAndNil 旧 addCronJob 入口保留校验语义。
func TestCron_InvalidExpressionAndNil(t *testing.T) {
	db := setupCronTestDB(t)
	svc := setupCronTestService(t, db)

	assert.Error(t, svc.addCronJob(&model.SyncTask{Key: "bad", Cron: "invalid-cron-expression"}))
	assert.Error(t, svc.addCronJob(nil))
	assert.NoError(t, svc.addCronJob(&model.SyncTask{Key: "ok", Cron: "*/5 * * * *"}))

	// 标准五字段表达式仍被接受。
	for _, expr := range []string{"* * * * *", "*/5 * * * *", "0 0 * * *", "0 9 * * 1-5", "30 2 1 * *"} {
		assert.NoError(t, svc.addCronJob(&model.SyncTask{Key: "five-" + expr, Cron: expr}))
	}
}

// TestStartCronJobs_SharedScheduler start/stop 幂等,首次扫描播种。
func TestStartCronJobs_SharedScheduler(t *testing.T) {
	db := setupCronTestDB(t)
	svc := setupCronTestService(t, db)
	mustCreateCronTask(t, svc, "cron-start", "*/5 * * * *")

	require.NoError(t, svc.startCronJobs())
	require.NoError(t, svc.startCronJobs(), "重复 Start 必须幂等")
	got, err := svc.tasks.taskRepository.FindByKey("cron-start")
	require.NoError(t, err)
	assert.NotNil(t, got.NextFireAt)
	svc.stopCronJobs()
	svc.stopCronJobs() // 幂等
}
