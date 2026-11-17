package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
)

// newRetryTestService 共享 retry_schedules / 执行队列的轻量服务。
func newRetryTestService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	return &Service{
		tasks:          NewTaskService(repository.NewSyncTaskRepository(db), repository.NewSyncRunRepository(db), repository.NewSyncRunStepRepository(db), nil),
		jobs:           repository.NewExecutionJobRepository(db),
		retrySchedules: repository.NewRetryScheduleRepository(db),
		bgCtx:          context.Background(),
		config:         &model.Config{Sync: model.SyncConfig{DurableQueue: true}},
	}
}

func setupRetryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "retry.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}, &model.Repo{},
		&model.ExecutionJob{}, &model.ExecutionTaskState{},
		&model.RetrySchedule{}, &model.MigrationMapping{},
	))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

func mkRetryRun(t *testing.T, svc *Service, taskKey, trigger, status string, end time.Time) *model.SyncRun {
	t.Helper()
	r := &model.SyncRun{
		TaskKey: taskKey, TriggerSource: trigger, Status: status,
		StartTime: end.Add(-time.Second), EndTime: &end,
	}
	require.NoError(t, svc.tasks.runRepository.Create(r))
	return r
}

// TestScheduleDeferredRetry_PersistsIdempotently 冷却期判定落库:
// 同一条失败 run 只允许一条排程,root/attempt/limit/reason 全部落库。
func TestScheduleDeferredRetry_PersistsIdempotently(t *testing.T) {
	db := setupRetryTestDB(t)
	svc := newRetryTestService(t, db)
	require.NoError(t, svc.tasks.taskRepository.Create(&model.SyncTask{Key: "t-retry", Name: "t", Enabled: true}))

	now := time.Now()
	root := mkRetryRun(t, svc, "t-retry", model.TriggerManual, model.StatusFailed, now.Add(-time.Minute))
	failedRetry := mkRetryRun(t, svc, "t-retry", model.TriggerAutoRetry, model.StatusFailed, now)

	d := Decision{
		Deferred: true, RetryAfter: time.Minute, Attempt: 2,
		AttemptLimit: 3, Reason: "cooldown",
	}
	require.NoError(t, svc.scheduleDeferredRetry(failedRetry, d))
	require.NoError(t, svc.scheduleDeferredRetry(failedRetry, d), "重复判定必须幂等")

	var rows []model.RetrySchedule
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1, "run_id 唯一:同一条失败运行只有一条排程")
	got := rows[0]
	assert.Equal(t, failedRetry.ID, got.RunID)
	assert.Equal(t, root.ID, got.RootRunID, "root_run_id 必须指向链头而非重试自身")
	assert.Equal(t, 2, got.Attempt)
	assert.Equal(t, 3, got.AttemptLimit)
	assert.Equal(t, "cooldown", got.Reason)
	assert.Equal(t, model.RetrySchedulePending, got.Status)
	assert.WithinDuration(t, now.Add(time.Minute), got.NextAttemptAt, 2*time.Second)
}

// TestRetryDispatch_DueScheduleEnqueuesAfterRestart 验收(#39):
// 冷却期内失败 → 排程落库;进程重启后(新 Service)到点仍会触发重跑。
func TestRetryDispatch_DueScheduleEnqueuesAfterRestart(t *testing.T) {
	db := setupRetryTestDB(t)
	svc := newRetryTestService(t, db)
	require.NoError(t, svc.tasks.taskRepository.Create(&model.SyncTask{
		Key: "t-restart", Name: "t", Enabled: true, SourceBranch: "main", TargetBranch: "main",
	}))

	now := time.Now()
	root := mkRetryRun(t, svc, "t-restart", model.TriggerManual, model.StatusFailed, now.Add(-time.Minute))
	failedRetry := mkRetryRun(t, svc, "t-restart", model.TriggerAutoRetry, model.StatusFailed, now)
	require.NoError(t, svc.scheduleDeferredRetry(failedRetry, Decision{
		Deferred: true, RetryAfter: time.Minute, Attempt: 2, AttemptLimit: 3, Reason: "cooldown",
	}))

	// 模拟重启:新的 Service 实例(内存 decided 丢失),排程仍在 DB。
	restarted := newRetryTestService(t, db)
	var sch model.RetrySchedule
	require.NoError(t, db.First(&sch).Error)
	// 尚未到点:不触发。
	restarted.retryDispatchOnce()
	var n int64
	require.NoError(t, db.Model(&model.ExecutionJob{}).Count(&n).Error)
	assert.Zero(t, n, "未到点不得提前触发")

	// 到点:任意实例认领并触发(trigger=auto_retry 入队)。
	require.NoError(t, db.Model(&model.RetrySchedule{}).Where("id = ?", sch.ID).
		Update("next_attempt_at", time.Now().Add(-time.Second)).Error)
	restarted.retryDispatchOnce()
	require.Eventually(t, func() bool {
		var c int64
		require.NoError(t, db.Model(&model.ExecutionJob{}).Count(&c).Error)
		return c == 1
	}, 2*time.Second, 5*time.Millisecond, "重启后到点排程必须触发重跑")
	var job model.ExecutionJob
	require.NoError(t, db.First(&job).Error)
	assert.Equal(t, model.TriggerAutoRetry, job.Trigger)
	assert.Equal(t, "t-restart", job.TaskKey)
	assert.NotZero(t, job.Generation, "重跑仍走既有队列代次语义")

	var after model.RetrySchedule
	require.NoError(t, db.First(&after, sch.ID).Error)
	assert.Equal(t, model.RetryScheduleDispatched, after.Status)
	assert.Equal(t, root.ID, sch.RootRunID)
}

// TestRetryDispatch_DisabledTaskCancels 任务禁用/删除 ⇒ 排程取消,不再拉起。
func TestRetryDispatch_DisabledTaskCancels(t *testing.T) {
	db := setupRetryTestDB(t)
	svc := newRetryTestService(t, db)
	require.NoError(t, svc.tasks.taskRepository.Create(&model.SyncTask{Key: "t-disabled", Name: "t", Enabled: true}))
	// GORM 对 bool 零值会落 default:true,显式 Update 才是真正禁用。
	require.NoError(t, db.Model(&model.SyncTask{}).Where("`key` = ?", "t-disabled").
		Update("enabled", false).Error)

	failed := mkRetryRun(t, svc, "t-disabled", model.TriggerAutoRetry, model.StatusFailed, time.Now())
	require.NoError(t, svc.scheduleDeferredRetry(failed, Decision{
		Deferred: true, RetryAfter: time.Second, Attempt: 2, AttemptLimit: 3, Reason: "cooldown",
	}))
	require.NoError(t, db.Model(&model.RetrySchedule{}).Where("run_id = ?", failed.ID).
		Update("next_attempt_at", time.Now().Add(-time.Second)).Error)

	svc.retryDispatchOnce()
	var sch model.RetrySchedule
	require.NoError(t, db.Where("run_id = ?", failed.ID).First(&sch).Error)
	assert.Equal(t, model.RetryScheduleCancelled, sch.Status)
	var n int64
	require.NoError(t, db.Model(&model.ExecutionJob{}).Count(&n).Error)
	assert.Zero(t, n, "禁用任务不得被排程拉起")
}

// TestRetryScheduleClaimDue_SingleWinner 条件认领:两个实例并发扫描,
// 同一排程只有一个认领者(另一个看到 next_attempt_at 已被推后)。
func TestRetryScheduleClaimDue_SingleWinner(t *testing.T) {
	db := setupRetryTestDB(t)
	d := repository.NewRetryScheduleRepository(db)
	require.NoError(t, db.AutoMigrate(&model.RetrySchedule{}))
	_, err := d.Create(&model.RetrySchedule{
		RunID: 1, TaskKey: "t", Attempt: 1, AttemptLimit: 1,
		NextAttemptAt: time.Now().Add(-time.Second), Status: model.RetrySchedulePending,
	})
	require.NoError(t, err)

	now := time.Now()
	first, err := d.ClaimDue(now, 10, time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)
	second, err := d.ClaimDue(now, 10, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, second, "同一排程在认领租约内不得被第二个实例重复认领")
}
