package service

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// newQueueTestService 带持久化队列的轻量服务(不跑 executor/派发器)。
func newQueueTestService(t *testing.T, key string, cfg *model.Config) *Service {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{},
		&model.Repo{}, &model.PendingTrigger{}, &model.ExecutionJob{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	svc := &Service{
		tasks:   NewTaskService(dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db), dao.NewSyncRunStepDAO(db), repoDAO),
		guard:   newLocalGuard(2),
		pending: dao.NewPendingTriggerDAO(db),
		jobs:    dao.NewExecutionJobDAO(db),
		bgCtx:   context.Background(),
		config:  cfg,
	}
	require.NoError(t, svc.tasks.taskDAO.Create(&model.SyncTask{
		Key: key, Name: "q", Enabled: true, SourceBranch: "main", TargetBranch: "main",
	}))
	return svc
}

func TestDurableQueue_ModeSwitches(t *testing.T) {
	svc := newQueueTestService(t, "q-mode", &model.Config{})
	assert.False(t, svc.durableQueueOn())
	assert.True(t, svc.executesJobs())

	svc.config.Sync.DurableQueue = true
	assert.True(t, svc.durableQueueOn())
	assert.True(t, svc.executesJobs(), "ExecuteJobs=nil ⇒ 本实例执行(默认合一)")

	off := false
	svc.config.Sync.ExecuteJobs = &off
	assert.False(t, svc.executesJobs(), "ExecuteJobs=false ⇒ 纯 API 实例只入队")

	on := true
	svc.config.Sync.ExecuteJobs = &on
	assert.True(t, svc.executesJobs())
}

// TestRunTaskWithTrigger_QueueMode_Enqueues 队列模式:触发改为入队,
// 重复触发合并(评审验收:重复投递不重复发布)。
func TestRunTaskWithTrigger_QueueMode_Enqueues(t *testing.T) {
	svc := newQueueTestService(t, "q-task", &model.Config{
		Sync: model.SyncConfig{DurableQueue: true},
	})

	err := svc.RunTaskWithTrigger(context.Background(), "q-task", model.TriggerWebhook, nil)
	require.NoError(t, err, "队列模式下触发=入队,受理即 nil")
	ids, err := svc.jobs.ListPendingIDs(10)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	job, err := svc.jobs.FindByID(ids[0])
	require.NoError(t, err)
	assert.Equal(t, "q-task", job.TaskKey)
	assert.Equal(t, model.TriggerWebhook, job.Trigger)

	// 重复触发 ⇒ 合并,不新增作业
	err = svc.RunTaskWithTrigger(context.Background(), "q-task", model.TriggerWebhook, nil)
	require.NoError(t, err)
	ids, err = svc.jobs.ListPendingIDs(10)
	require.NoError(t, err)
	assert.Len(t, ids, 1, "并发/重复触发必须合并,不得重复入队")
}

// TestRunTaskAsync_QueueMode_Enqueues 手动/重试入口同样入队。
func TestRunTaskAsync_QueueMode_Enqueues(t *testing.T) {
	svc := newQueueTestService(t, "q-async", &model.Config{
		Sync: model.SyncConfig{DurableQueue: true},
	})
	require.NoError(t, svc.RunTaskAsync("q-async", model.TriggerManual, nil))
	ids, err := svc.jobs.ListPendingIDs(10)
	require.NoError(t, err)
	assert.Len(t, ids, 1)
}

// TestRunTask_DirectMode_Unchanged 队列关闭=旧直跑语义保持(守卫未拿到时照旧报错)。
func TestRunTask_DirectMode_Unchanged(t *testing.T) {
	svc, release := newPendingTestService(t, "q-direct")
	defer release()

	err := svc.RunTaskWithTrigger(context.Background(), "q-direct", model.TriggerWebhook, nil)
	require.ErrorIs(t, err, ErrTaskRunning, "队列关闭时直跑路径不变")
	n, err := svc.jobs.CountPending()
	require.NoError(t, err)
	assert.Zero(t, n, "直跑模式不得入队")
}
