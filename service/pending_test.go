package service

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// newPendingTestService 造一个只够测"触发被拒/待补跑记账"的服务(不碰 executor)。
func newPendingTestService(t *testing.T, key string) (*Service, func()) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{},
		&model.Repo{}, &model.PendingTrigger{}))
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
		tasks:   NewTaskService(taskDAO, runDAO, runStepDAO, repoDAO),
		guard:   newLocalGuard(2),
		pending: dao.NewPendingTriggerDAO(db),
		bgCtx:   context.Background(),
		config:  &model.Config{},
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
// 必须记一笔待补跑(落库)。
//
// 回归背景(P0,推送的提交被静默丢掉):webhook 推送到达时任务正在同步 ⇒
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
	pts, lerr := svc.pending.List()
	require.NoError(t, lerr)
	require.Len(t, pts, 1, "webhook 触发被拒时必须记一笔待补跑(否则这次推送的提交被丢掉)")
	assert.Equal(t, key, pts[0].TaskKey)
	assert.Equal(t, int64(1), pts[0].Generation)
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
	pts, lerr := svc.pending.List()
	require.NoError(t, lerr)
	assert.Empty(t, pts, "手动触发被拒不记待补跑(它的语义就是立刻失败)")
}

// TestPendingTrigger_Coalesces 同一任务积压多次只补跑一次(generation 留痕)。
func TestPendingTrigger_Coalesces(t *testing.T) {
	svc, release := newPendingTestService(t, "pending-coalesce")
	defer release()

	svc.markPendingWebhook("pending-coalesce")
	svc.markPendingWebhook("pending-coalesce")
	svc.markPendingWebhook("other-task")

	pts, err := svc.pending.List()
	require.NoError(t, err)
	require.Len(t, pts, 2, "两个任务各算一笔")
	for _, pt := range pts {
		if pt.TaskKey == "pending-coalesce" {
			assert.Equal(t, int64(2), pt.Generation, "重复记账合并,generation 留痕积压次数")
		}
	}

	claimed, err := svc.pending.Claim("pending-coalesce")
	require.NoError(t, err)
	assert.True(t, claimed, "认领应命中")
	claimed, err = svc.pending.Claim("pending-coalesce")
	require.NoError(t, err)
	assert.False(t, claimed, "认领只能一次(原子 DELETE,跨实例只有一个消费者)")
}

// TestFinishRun_ConsumesPending RunTaskAsync 路径收尾也消费待补跑(双路径覆盖)。
//
// 回归背景(P0):旧实现只有 RunTaskWithTrigger 收尾消费 pending,RunTaskAsync
// (手动/重试/auto_retry)跑完不消费 ⇒ 手动执行期间积压的 webhook 永远补不上。
//
// 本用例:pending 有一笔 + 执行权仍被占(模拟"另一个执行正在进行")⇒ finishRun
// 认领并发起补跑 ⇒ 补跑撞忙 ⇒ 依 ErrTaskRunning 分支**重新记账** ⇒ 最终收敛不丢。
func TestFinishRun_ConsumesPending(t *testing.T) {
	const key = "pending-finish-run"
	svc, release := newPendingTestService(t, key)
	defer release()

	svc.markPendingWebhook(key)
	svc.finishRun(key, model.TriggerManual, &model.SyncRun{ID: 1, TaskKey: key}, nil, func() {})

	// 补跑在 goroutine 里异步发生:撞忙(执行权还被测试占着)⇒ 重新记账。
	require.Eventually(t, func() bool {
		pts, err := svc.pending.List()
		return err == nil && len(pts) == 1
	}, 2*time.Second, 10*time.Millisecond,
		"finishRun 必须消费待补跑并发起补跑;撞忙时应重新记账,触发不丢")
}
