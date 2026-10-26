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

// newPendingTestService 造一个只够测"触发被拒"的服务(不碰 executor)。
func newPendingTestService(t *testing.T, key string) (*Service, func()) {
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

	svc := &Service{
		tasks:          NewTaskService(taskDAO, runDAO, runStepDAO, repoDAO),
		guard:          newLocalGuard(2),
		pendingWebhook: newPendingTriggerSet(),
		bgCtx:          context.Background(),
		config:         &model.Config{},
	}
	require.NoError(t, taskDAO.Create(&model.SyncTask{
		Key: key, Name: "pending", Enabled: true, SourceBranch: "main", TargetBranch: "main",
	}))

	// 占住同 taskKey 的执行权(模拟"已经有一个在跑")
	release, err := svc.guard.Acquire(context.Background(), key)
	require.NoError(t, err)
	return svc, release
}

// TestRunTaskWithTrigger_BusyTask_RecordsPendingWebhook 任务在跑时,webhook 触发
// 必须记一笔待补跑。
//
// 回归背景(P1,推送的提交被静默丢掉):webhook 推送到达时任务正在同步 ⇒
// guard 快速失败(ErrTaskRunning)⇒ **连运行记录都没有** ⇒ 自动重跑机制
// (RetryTracker,只认 status=failed 的运行)看不到它 ⇒ 这次推送的提交被丢掉,
// 要等下一次推送或 cron 才补上。
//
// 这条用例是**反向验证**:去掉 RunTaskWithTrigger 里的 pending 记账,本用例变红。
func TestRunTaskWithTrigger_BusyTask_RecordsPendingWebhook(t *testing.T) {
	const key = "pending-webhook"
	svc, release := newPendingTestService(t, key)
	defer release()

	err := svc.RunTaskWithTrigger(context.Background(), key, model.TriggerWebhook, nil)
	require.ErrorIs(t, err, ErrTaskRunning)
	require.Equal(t, 1, svc.pendingWebhook.Len(),
		"webhook 触发被拒时必须记一笔待补跑(否则这次推送的提交被丢掉)")
}

// TestRunTaskWithTrigger_BusyTask_NonWebhookTriggerNotPending 非 webhook 触发不记待补跑。
//
// 手动/cron 触发的语义是"立刻失败,让调用方知道" ⇒ 不该在背后再补跑一次。
func TestRunTaskWithTrigger_BusyTask_NonWebhookTriggerNotPending(t *testing.T) {
	const key = "pending-manual"
	svc, release := newPendingTestService(t, key)
	defer release()

	err := svc.RunTaskWithTrigger(context.Background(), key, model.TriggerManual, nil)
	require.ErrorIs(t, err, ErrTaskRunning)
	require.Equal(t, 0, svc.pendingWebhook.Len(),
		"手动触发被拒不记待补跑(它的语义就是立刻失败)")
}

// TestPendingTriggerSet_Coalesces 同一任务积压多次只补跑一次。
func TestPendingTriggerSet_Coalesces(t *testing.T) {
	p := newPendingTriggerSet()
	assert.Equal(t, 0, p.Len())

	p.Mark("task-a")
	p.Mark("task-a")
	p.Mark("task-b")
	assert.Equal(t, 2, p.Len(), "两个任务各算一笔")

	assert.True(t, p.Take("task-a"), "取走应命中")
	assert.False(t, p.Take("task-a"), "取走只能一次(补跑一次就够)")
	assert.True(t, p.Take("task-b"))
	assert.Equal(t, 0, p.Len())
}
