package dao

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func setupJobTestDB(t *testing.T) *ExecutionJobDAO {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ExecutionJob{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewExecutionJobDAO(db)
}

// TestExecutionJob_EnqueueDedupes 同任务并发/重复触发合并:
// 已有 pending/claimed 作业时入队不新增(评审验收:重复投递不重复发布)。
func TestExecutionJob_EnqueueDedupes(t *testing.T) {
	d := setupJobTestDB(t)

	enq, err := d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)
	assert.True(t, enq)

	// 同任务重复触发 ⇒ 合并
	enq, err = d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)
	assert.False(t, enq, "已有待执行作业时不得重复入队")

	// 不同任务不受影响
	enq, err = d.Enqueue("task-b", model.TriggerCron, nil)
	require.NoError(t, err)
	assert.True(t, enq)

	n, err := d.CountPending()
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

// TestExecutionJob_ClaimSingleWinner 认领是原子的:
// 两个实例竞争同一作业,只有一个胜出(多实例不重复执行)。
func TestExecutionJob_ClaimSingleWinner(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, err := d.ListPendingIDs(10)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	ok, err := d.Claim(ids[0], "instance-1", time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "第一个认领者应胜出")

	ok, err = d.Claim(ids[0], "instance-2", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "已被认领的作业不得二次认领")

	// 胜者信息正确
	job, err := d.FindByID(ids[0])
	require.NoError(t, err)
	assert.Equal(t, model.JobStatusClaimed, job.Status)
	assert.Equal(t, "instance-1", job.Owner)
	require.NotNil(t, job.LeaseUntil)
}

// TestExecutionJob_ExpireAndRequeue lease 过期 ⇒ 放回 pending(attempt+1);
// 超 MaxJobAttempts ⇒ 判 failed 毒丸(接管风暴保护)。
func TestExecutionJob_ExpireAndRequeue(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerCron, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "dead-worker", -time.Minute) // TTL 负数 ⇒ 立即过期
	require.True(t, ok)

	requeued, poisoned, err := d.RequeueExpired(time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(1), requeued)
	assert.Zero(t, poisoned)

	job, _ := d.FindByID(ids[0])
	assert.Equal(t, model.JobStatusPending, job.Status)
	assert.Equal(t, 1, job.Attempt)

	// 反复过期直至毒丸:claim(过期) → requeue ×MaxJobAttempts
	for i := 1; i < model.MaxJobAttempts; i++ {
		ok, _ = d.Claim(ids[0], "dead-worker", -time.Minute)
		require.True(t, ok)
		_, _, err = d.RequeueExpired(time.Now())
		require.NoError(t, err)
	}
	ok, _ = d.Claim(ids[0], "dead-worker", -time.Minute)
	require.True(t, ok)
	_, poisoned, err = d.RequeueExpired(time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(1), poisoned, "第 MaxJobAttempts 次过期应判毒丸 failed")

	job, _ = d.FindByID(ids[0])
	assert.Equal(t, model.JobStatusFailed, job.Status)
	assert.Contains(t, job.Error, "max attempts")
}

// TestExecutionJob_Complete 完成写终态与 run 关联。
func TestExecutionJob_Complete(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "i1", time.Minute)
	require.True(t, ok)

	require.NoError(t, d.Complete(ids[0], "i1", 42, nil))
	job, _ := d.FindByID(ids[0])
	assert.Equal(t, model.JobStatusSucceeded, job.Status)
	require.NotNil(t, job.RunID)
	assert.Equal(t, uint(42), *job.RunID)

	// 非本人不得完成(防脑裂实例写别人的作业)
	_, err = d.Enqueue("task-b", model.TriggerManual, nil)
	require.NoError(t, err)
	ids2, _ := d.ListPendingIDs(10)
	var bID uint
	for _, id := range ids2 {
		j, _ := d.FindByID(id)
		if j.TaskKey == "task-b" {
			bID = id
		}
	}
	ok, _ = d.Claim(bID, "i1", time.Minute)
	require.True(t, ok)
	require.NoError(t, d.Complete(bID, "i2", 0, assert.AnError))
	job, _ = d.FindByID(bID)
	assert.Equal(t, model.JobStatusClaimed, job.Status, "非 owner 的 Complete 不得生效")
}

// TestExecutionJob_CleanupFinished 保留期外清理。
func TestExecutionJob_CleanupFinished(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	_, _ = d.Claim(ids[0], "i1", time.Minute)
	require.NoError(t, d.Complete(ids[0], "i1", 0, nil))

	// 把 finished_at 拨回 8 天前
	d.db.Model(&model.ExecutionJob{}).Where("id=?", ids[0]).
		Update("finished_at", time.Now().Add(-8*24*time.Hour))

	n, err := d.CleanupFinished(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}
