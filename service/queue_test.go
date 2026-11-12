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

// TestDurableQueue_BenignBusyDoesNotPoison 执行权被占(良性繁忙)的退避
// 不得烧光毒丸预算(#13):同一作业连续 6 次「认领→被占→退避」后,
// 作业必须仍是 pending(attempt=0),而不是被判 failed 丢弃。
// 此前退避只推 lease,作业停在 claimed 等 reaper 回收 ⇒ attempt 必然 +1,
// 5 次退避(约 2.5 分钟)后作业被毒丸化,该次触发永久丢失。
func TestDurableQueue_BenignBusyDoesNotPoison(t *testing.T) {
	cfg := &model.Config{Sync: model.SyncConfig{DurableQueue: true}}
	svc := newQueueTestService(t, "q-busy", cfg)
	// 占住执行权:dispatchOnce 认领后 guard.Acquire 必然失败(任务正忙)
	release, err := svc.guard.Acquire(context.Background(), "q-busy")
	require.NoError(t, err)
	defer release()

	require.NoError(t, svc.RunTaskWithTrigger(context.Background(), "q-busy", model.TriggerWebhook, nil))

	// waitRescheduled 等待异步 executeJob 完成「认领→被占→退避」回到 pending。
	waitRescheduled := func(round int) *model.ExecutionJob {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			ids, err := svc.jobs.ListPendingIDs(10)
			require.NoError(t, err)
			if len(ids) == 1 {
				job, err := svc.jobs.FindByID(ids[0])
				require.NoError(t, err)
				if job.Status == model.JobStatusPending && job.DeferCount >= round {
					return job
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("round %d: 2s 内作业未被退避重排回 pending", round)
		return nil
	}

	var jobID uint
	for round := 1; round <= 6; round++ {
		// 跳过上一轮退避窗口(not-before),模拟派发器下一轮再试
		if jobID != 0 {
			require.NoError(t, svc.jobs.DB().Model(&model.ExecutionJob{}).Where("id = ?", jobID).
				Update("lease_until", time.Now().Add(-time.Second)).Error)
		}
		svc.dispatchOnce()
		job := waitRescheduled(round)
		jobID = job.ID
		assert.Equal(t, model.JobStatusPending, job.Status, "round %d: 退避后必须放回 pending", round)
		assert.Zero(t, job.Attempt, "round %d: 良性繁忙不得累加 attempt(毒丸预算)", round)
		assert.Equal(t, round, job.DeferCount, "round %d: 退避次数单独记账", round)
	}

	// reaper 跑一遍也不得把退避中的作业毒丸化
	svc.reapExpiredJobs()
	job, err := svc.jobs.FindByID(jobID)
	require.NoError(t, err)
	assert.NotEqual(t, model.JobStatusFailed, job.Status,
		"连续 6 次执行权被占后作业必须还活着,不得被判 failed(触发永久丢失)")
	assert.Zero(t, job.Attempt)
}

// TestDurableQueue_RescheduleSkipsDeferredClaim 退避窗口内的作业不得被
// 派发器提前认领(not-before),防派发器对同一被占任务忙旋。
func TestDurableQueue_RescheduleSkipsDeferredClaim(t *testing.T) {
	cfg := &model.Config{Sync: model.SyncConfig{DurableQueue: true}}
	svc := newQueueTestService(t, "q-defer", cfg)
	require.NoError(t, svc.RunTaskWithTrigger(context.Background(), "q-defer", model.TriggerWebhook, nil))

	// 手工认领再退避(模拟一次「执行权被占」)
	ids, err := svc.jobs.ListPendingIDs(10)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	ok, err := svc.jobs.Claim(ids[0], svc.instanceID, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, svc.jobs.Reschedule(ids[0], svc.instanceID, 30*time.Second))

	// 退避窗口内:派发器认领必须被 not-before 挡住
	ok, err = svc.jobs.Claim(ids[0], svc.instanceID, time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "退避窗口内不得提前认领")

	// 窗口过后可正常认领
	require.NoError(t, svc.jobs.DB().Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
		Update("lease_until", time.Now().Add(-time.Second)).Error)
	ok, err = svc.jobs.Claim(ids[0], svc.instanceID, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestRenewLeaseTick_LostLeaseCancelsExecution 失锁哨兵接线(#14):
// 续租发现 0 行命中(旧属主已被接管)必须返回「停手」并触发取消回调,
// 而不是静默返回成功让旧 worker 继续推送(脑裂)。
func TestRenewLeaseTick_LostLeaseCancelsExecution(t *testing.T) {
	svc := newQueueTestService(t, "q-lease", &model.Config{})
	_, err := svc.jobs.Enqueue("q-lease", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, err := svc.jobs.ListPendingIDs(10)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// 对照:仍是属主 ⇒ 继续续租
	ok, err := svc.jobs.Claim(ids[0], svc.instanceID, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	cancelled := false
	assert.True(t, svc.renewLeaseTick(ids[0], func() { cancelled = true }))
	assert.False(t, cancelled, "仍持有租约时不得触发取消")

	// 失锁:reaper 回收 → 其它实例认领 ⇒ 旧属主续租必须报停
	_, _, err = svc.jobs.RequeueExpired(time.Now().Add(time.Hour))
	require.NoError(t, err)
	ok, err = svc.jobs.Claim(ids[0], "other-instance", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	assert.False(t, svc.renewLeaseTick(ids[0], func() { cancelled = true }),
		"失锁后续租循环必须终止")
	assert.True(t, cancelled, "失锁必须触发取消执行回调(旧属主停手,防脑裂)")
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
