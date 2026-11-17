package repository

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oekit/pkg/ptrx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func setupJobTestDB(t *testing.T) *ExecutionJobRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ExecutionJob{}, &model.ExecutionTaskState{}))
	// 活动槽唯一索引与生产对齐(migration v7 显式建,模型 tag 无索引——
	// 见 migrate.go v7 注:索引必须晚于存量去重建立)。
	require.NoError(t, db.Exec(
		"CREATE UNIQUE INDEX IF NOT EXISTS uk_execution_jobs_active_slot ON execution_jobs (active_key)").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewExecutionJobRepository(db)
}

// taskState 读某任务的队列状态行(测试断言代次账本用)。
func taskState(t *testing.T, d *ExecutionJobRepository, taskKey string) *model.ExecutionTaskState {
	t.Helper()
	var st model.ExecutionTaskState
	require.NoError(t, d.db.Where("task_key = ?", taskKey).First(&st).Error)
	return &st
}

// leaseVersion 读作业当前租约纪元(fencing token,#37 测试用)。
func leaseVersion(t *testing.T, d *ExecutionJobRepository, id uint) int64 {
	t.Helper()
	job, err := d.FindByID(id)
	require.NoError(t, err)
	return job.LeaseVersion
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

	require.NoError(t, d.Complete(ids[0], "i1", leaseVersion(t, d, ids[0]), 42, nil))
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
	require.ErrorIs(t, d.Complete(bID, "i2", leaseVersion(t, d, bID), 0, assert.AnError), ErrLeaseLost,
		"非 owner 的 Complete 必须返回失锁哨兵(此前静默返回 nil,调用方无感)")
	job, _ = d.FindByID(bID)
	assert.Equal(t, model.JobStatusClaimed, job.Status, "非 owner 的 Complete 不得生效")
}

// TestExecutionJob_CoalescedDuringExecutionGetsCatchUpJob 执行中到达的触发不得被
// 合并丢弃(#36):作业 claimed 期间新提交的 webhook 触发按合并语义不重复发布,
// 但必须有「待办承接」——作业收尾时若仍有未消费的触发代次,必须再入一个追赶
// 作业,最后一次触发对应的提交一定被执行。
//
// 回归背景(P0,同步空窗无上限):旧实现 claimed 期间的触发被 WHERE NOT EXISTS
// 合并丢弃且不记任何待办 ⇒ 作业 succeeded 后 pending=0,新提交只能等下一次
// webhook/cron。本用例去掉收尾承接即变红。
func TestExecutionJob_CoalescedDuringExecutionGetsCatchUpJob(t *testing.T) {
	d := setupJobTestDB(t)

	enq, err := d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)
	require.True(t, enq)
	ids, err := d.ListPendingIDs(10)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	ok, err := d.Claim(ids[0], "worker-A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// 执行中连发 N 次 webhook:合并语义(不重复发布),但不得丢
	for i := 0; i < 5; i++ {
		enq, err = d.Enqueue("task-a", model.TriggerWebhook, nil)
		require.NoError(t, err)
		assert.False(t, enq, "执行中到达的触发按合并语义不重复发布")
	}

	require.NoError(t, d.Complete(ids[0], "worker-A", leaseVersion(t, d, ids[0]), 42, nil))

	// 收尾后必须有追赶作业承接(否则被合并的触发永久丢失)
	pending, err := d.CountPending()
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending, "执行中被合并的触发必须在收尾时再入作业承接(#36)")

	// 追赶作业携带最新代次,收尾 CAS 已把已覆盖代次上推
	st := taskState(t, d, "task-a")
	assert.Equal(t, int64(6), st.DesiredGeneration, "N=5 次执行中触发 + 首次受理 = 6 个代次")
	assert.Equal(t, int64(1), st.CompletedGeneration, "首次作业覆盖代次 1")
	ids2, err := d.ListPendingIDs(10)
	require.NoError(t, err)
	require.Len(t, ids2, 1)
	job2, err := d.FindByID(ids2[0])
	require.NoError(t, err)
	assert.Equal(t, int64(6), job2.Generation, "追赶作业必须携带最新代次(最后一次触发对应的提交一定被执行)")
	require.NotNil(t, st.ActiveJobID)
	assert.Equal(t, ids2[0], *st.ActiveJobID, "状态行活动槽指向追赶作业")

	// 追赶执行收尾:全部代次覆盖,不再产生新作业(收敛,不无限循环)
	ok, err = d.Claim(ids2[0], "worker-A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, d.Complete(ids2[0], "worker-A", leaseVersion(t, d, ids2[0]), 43, nil))
	pending, err = d.CountPending()
	require.NoError(t, err)
	assert.Zero(t, pending, "代次追平后不得再入作业(合并语义:不放大执行次数)")
	st = taskState(t, d, "task-a")
	assert.Equal(t, int64(6), st.CompletedGeneration)
	assert.Nil(t, st.ActiveJobID, "活动槽必须随收尾释放")

	// 合并语义总量:1 次初始执行 + 1 次追赶执行承接全部 5 次执行中触发
	var total int64
	require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("task_key = ?", "task-a").Count(&total).Error)
	assert.Equal(t, int64(2), total, "执行中连发 N 次触发合并为 1 次追赶作业,不得放大执行次数")
}

// TestExecutionJob_PoisonedJobStillCatchesUp 毒丸作业的代次未消费:
// reaper 判毒丸时必须在同一事务里释放活动槽并补追赶作业(#36),
// 否则执行中被合并的触发随毒丸一起蒸发。
func TestExecutionJob_PoisonedJobStillCatchesUp(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "dead-worker", -time.Minute) // TTL 负数 ⇒ 立即过期
	require.True(t, ok)
	// 把接管预算烧到上限:再过期一次即毒丸
	require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
		Update("attempt", model.MaxJobAttempts).Error)

	_, poisoned, err := d.RequeueExpired(time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(1), poisoned)

	// 毒丸了,但触发不能丢:追赶作业承接代次
	pending, err := d.CountPending()
	require.NoError(t, err)
	assert.Equal(t, int64(1), pending, "毒丸作业的代次必须由追赶作业承接(#36)")
	ids2, _ := d.ListPendingIDs(1)
	require.Len(t, ids2, 1)
	job2, _ := d.FindByID(ids2[0])
	assert.Equal(t, int64(1), job2.Generation)
	st := taskState(t, d, "task-a")
	assert.Equal(t, int64(1), st.DesiredGeneration)
	assert.Zero(t, st.CompletedGeneration, "毒丸作业的代次不得计入已覆盖")
	require.NotNil(t, st.ActiveJobID)
	assert.Equal(t, ids2[0], *st.ActiveJobID)
}

// TestExecutionJob_ConcurrentEnqueueSingleActiveSlot 并发入队单胜者:
// 同任务并发触发,只有一条活动作业胜出,其余按合并承接(代次累计)。
// -race 下并发跑;活动槽唯一索引兜底锁定读之外的任何竞态。
func TestExecutionJob_ConcurrentEnqueueSingleActiveSlot(t *testing.T) {
	d := setupJobTestDB(t)

	const n = 10
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			enq, err := d.Enqueue("task-race", model.TriggerWebhook, nil)
			require.NoError(t, err)
			results[idx] = enq
		}(i)
	}
	wg.Wait()

	var total int64
	require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("task_key = ?", "task-race").Count(&total).Error)
	assert.Equal(t, int64(1), total, "并发入队只允许一条活动作业(单胜者)")

	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	assert.Equal(t, 1, wins, "恰好一次入队胜出")

	st := taskState(t, d, "task-race")
	assert.Equal(t, int64(n), st.DesiredGeneration, "每次触发都递增代次,合并的不丢")
	require.NotNil(t, st.ActiveJobID)
}

// TestExecutionJob_ActiveSlotUniqueIndex 活动槽唯一索引是硬保证:
// 绕过状态行直接插第二条活动作业必须被唯一索引拒绝(防任何账本/竞态
// 缺陷造成同任务双活动作业、双执行)。
func TestExecutionJob_ActiveSlotUniqueIndex(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)

	dup := model.ExecutionJob{
		TaskKey: "task-a", Trigger: model.TriggerWebhook,
		Status: model.JobStatusPending, Generation: 1,
		ActiveKey: ptrx.Of("task-a"), QueuedAt: time.Now(),
	}
	err = d.db.Create(&dup).Error
	require.Error(t, err, "第二条活动作业必须被 uk_execution_jobs_active_slot 拒绝")
}

// TestExecutionJob_CompleteRejectedAfterPoison 毒丸终态不可被旧属主覆写(#37):
// reaper 判毒丸(failed)后,原属主的 Complete 必须收到 ErrLeaseLost——
// 旧实现 Complete 的 WHERE 只有 id+owner(毒丸路径不清 owner),终态被改成
// succeeded:被判定毒丸的作业(可能正是双实例并发推送的现场)从队列上消失。
func TestExecutionJob_CompleteRejectedAfterPoison(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "worker-A", -time.Minute) // TTL 负数 ⇒ 租约立即过期
	require.True(t, ok)
	require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
		Update("attempt", model.MaxJobAttempts).Error)

	_, poisoned, err := d.RequeueExpired(time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(1), poisoned)
	job, _ := d.FindByID(ids[0])
	require.Equal(t, model.JobStatusFailed, job.Status)

	// 旧属主回头补终态:必须被拒(0 行命中),毒丸终态原样保留
	err = d.Complete(ids[0], "worker-A", leaseVersion(t, d, ids[0]), 42, nil)
	require.ErrorIs(t, err, ErrLeaseLost, "毒丸终态不得被失去执行权的旧 worker 覆写(#37)")
	job, _ = d.FindByID(ids[0])
	assert.Equal(t, model.JobStatusFailed, job.Status, "终态必须仍是 failed")
	assert.Nil(t, job.RunID, "旧属主的 run_id 不得写入")
}

// TestExecutionJob_CompleteRejectedAfterLeaseExpired 租约过期后 Complete 必须
// 失锁(#37):旧实现不校验 lease_until>now——租约已过期的属主仍能写终态,
// 而该作业随时可能被 reaper 放回、被其它实例认领 ⇒ 双属主并发执行窗口。
func TestExecutionJob_CompleteRejectedAfterLeaseExpired(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "inst-A", time.Minute)
	require.True(t, ok)
	// 租约过期(还没被 reaper 接管)
	require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
		Update("lease_until", time.Now().Add(-time.Second)).Error)

	err = d.Complete(ids[0], "inst-A", leaseVersion(t, d, ids[0]), 42, nil)
	require.ErrorIs(t, err, ErrLeaseLost, "租约过期后 Complete 必须失锁(#37)")
	job, _ := d.FindByID(ids[0])
	assert.Equal(t, model.JobStatusClaimed, job.Status, "终态不得被写入")
	assert.Nil(t, job.RunID)
}

// TestExecutionJob_ReaperSkipsRenewedLease reaper 必须复核 lease_until(#37):
// reaper 选出过期候选之后、UPDATE 之前,worker 续租成功 ⇒ UPDATE 仍把作业
// 放回 pending,随后被第三实例认领 ⇒ 同一任务双属主并发执行。
// 用 GORM query callback 在 SELECT 之后、UPDATE 之前插入一次续租,确定性复现
// 「先查后改」窗口;修复后 reaper 的 UPDATE 重新复核 lease_until ⇒ 0 行命中。
//
// 复现基建:文件库 + 双连接(:memory: 单连接下回调内的跨语句写入在
// glebarez 驱动上不可靠),回调里用外层 db 句柄(独立连接)续租——
// 模拟「另一条连接上的 worker 恰在 reaper 的查改之间续租成功」。
func TestExecutionJob_ReaperSkipsRenewedLease(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "reaper-race.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ExecutionJob{}, &model.ExecutionTaskState{}))
	// 与生产对齐的活动槽唯一索引(migration v8,见 migrate.go)。
	require.NoError(t, db.Exec(
		"CREATE UNIQUE INDEX IF NOT EXISTS uk_execution_jobs_active_slot ON execution_jobs (active_key)").Error)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	d := NewExecutionJobRepository(db)

	_, err = d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, err := d.ListPendingIDs(1)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	ok, err := d.Claim(ids[0], "inst-A", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	// 让 reaper 的候选查询选中它(租约过期),但 UPDATE 之前会被续租救回
	require.NoError(t, db.Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
		Update("lease_until", time.Now().Add(-time.Second)).Error)

	var renewed int32
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("p037_renew", func(tx *gorm.DB) {
		if !atomic.CompareAndSwapInt32(&renewed, 0, 1) {
			return
		}
		// SELECT 之后、UPDATE 之前:worker 续租成功(lease 重新有效)。
		res := db.Model(&model.ExecutionJob{}).Where("id = ? AND owner = ?", ids[0], "inst-A").
			Update("lease_until", time.Now().Add(time.Minute))
		require.NoError(t, res.Error)
	}))
	defer func() {
		require.NoError(t, db.Callback().Query().Remove("p037_renew"))
	}()

	requeued, poisoned, err := d.RequeueExpired(time.Now())
	require.NoError(t, err)
	assert.Zero(t, requeued, "续租成功(lease 仍有效)的作业不得被 reaper 放回 pending(#37)")
	assert.Zero(t, poisoned)
	job, err := d.FindByID(ids[0])
	require.NoError(t, err)
	assert.Equal(t, model.JobStatusClaimed, job.Status, "作业必须仍在执行中")
	assert.Equal(t, "inst-A", job.Owner, "属主不得被 reaper 清掉")
	require.NotNil(t, job.LeaseUntil)
	assert.True(t, job.LeaseUntil.After(time.Now()), "续租后的租约必须保持有效")
}

// TestExecutionJob_RenewLeaseWithStaleEpochFails 旧纪元的续租必须失锁(#37):
// 同 owner 字符串的先后两次认领(实例重启复用 id / 配置撞 owner 等)之间,
// 旧纪元的持有者不得续租新纪元的作业——owner+status 校验拦不住这种重叠,
// 必须靠 lease_version(fencing token)按纪元排除。
func TestExecutionJob_RenewLeaseWithStaleEpochFails(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "inst-A", time.Minute)
	require.True(t, ok)
	staleEpoch := leaseVersion(t, d, ids[0])

	// 接管重排 → 同一 owner 字符串再次认领(新纪元)
	_, _, err = d.RequeueExpired(time.Now().Add(time.Hour))
	require.NoError(t, err)
	ok, _ = d.Claim(ids[0], "inst-A", time.Minute)
	require.True(t, ok)

	err = d.RenewLease(ids[0], "inst-A", staleEpoch, time.Minute)
	require.ErrorIs(t, err, ErrLeaseLost,
		"旧纪元的续租必须失锁(仅靠 owner 判据拦不住同 owner 的先后两次认领,#37)")
}

// TestExecutionJob_RescheduleRejectedAfterLeaseExpired 租约过期后 Reschedule
// 必须失锁(#37):退避重排是属主对自己租约的处置,租约已过期说明执行权
// 存疑(随时可能被接管),不得再放回 pending。
func TestExecutionJob_RescheduleRejectedAfterLeaseExpired(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "inst-A", time.Minute)
	require.True(t, ok)
	require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
		Update("lease_until", time.Now().Add(-time.Second)).Error)

	err = d.Reschedule(ids[0], "inst-A", leaseVersion(t, d, ids[0]), 30*time.Second)
	require.ErrorIs(t, err, ErrLeaseLost, "租约过期后 Reschedule 必须失锁(#37)")
	job, _ := d.FindByID(ids[0])
	assert.Equal(t, model.JobStatusClaimed, job.Status, "退避重排不得生效")
}

// TestExecutionJob_ClaimConcurrentSingleWinner 并发认领单胜者(-race):
// 多实例同时认领同一作业,恰好一个胜出,其余 0 行命中。
func TestExecutionJob_ClaimConcurrentSingleWinner(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	require.Len(t, ids, 1)

	const n = 8
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := d.Claim(ids[0], fmt.Sprintf("inst-%d", idx), time.Minute)
			require.NoError(t, err)
			wins[idx] = ok
		}(i)
	}
	wg.Wait()
	total := 0
	for _, ok := range wins {
		if ok {
			total++
		}
	}
	assert.Equal(t, 1, total, "并发认领必须恰好单胜者(双属主即脑裂)")
}

// TestExecutionJob_EnqueueActiveSlotUnique 入队去重由 active_key 唯一索引
// 兜底(不再依赖裸 SQL 的 WHERE NOT EXISTS):同任务第二次入队合并。
func TestExecutionJob_EnqueueActiveSlotUnique(t *testing.T) {
	d := setupJobTestDB(t)

	enq, err := d.Enqueue("task-quote", model.TriggerWebhook, nil)
	require.NoError(t, err)
	assert.True(t, enq)

	enq, err = d.Enqueue("task-quote", model.TriggerWebhook, nil)
	require.NoError(t, err)
	assert.False(t, enq, "活动槽唯一索引必须拦住第二个活动作业")
}

// TestExecutionJob_RenewLeaseLost 失锁哨兵(#14):
// 属主被 reaper 换掉后,旧属主续租/完成都必须收到 ErrLeaseLost;
// 且已过期但尚未被接管的租约不得借续租复活(对齐 lock 包语义)。
func TestExecutionJob_RenewLeaseLost(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "inst-A", time.Minute)
	require.True(t, ok)

	// 对照:仍是属主时续租正常生效(lease_until 前推,纪元不变——续租是
	// 同一租约的延长,不换 fencing token)
	job, _ := d.FindByID(ids[0])
	before := *job.LeaseUntil
	beforeVersion := job.LeaseVersion
	require.NoError(t, d.RenewLease(ids[0], "inst-A", beforeVersion, 5*time.Minute))
	job, _ = d.FindByID(ids[0])
	assert.True(t, job.LeaseUntil.After(before), "有效续租必须前推 lease_until")
	assert.Equal(t, beforeVersion, job.LeaseVersion, "续租不得换纪元(fencing token 稳定)")

	// 失锁路径:reaper 回收 → B 认领 → A 的续租与完成都要报 ErrLeaseLost
	_, _, err = d.RequeueExpired(time.Now().Add(time.Hour))
	require.NoError(t, err)
	ok, _ = d.Claim(ids[0], "inst-B", time.Minute)
	require.True(t, ok)

	require.ErrorIs(t, d.RenewLease(ids[0], "inst-A", leaseVersion(t, d, ids[0]), time.Minute), ErrLeaseLost,
		"旧属主 A 续租必须收到失锁哨兵(0 行命中不得吞成 nil)")
	require.ErrorIs(t, d.Complete(ids[0], "inst-A", leaseVersion(t, d, ids[0]), 1, nil), ErrLeaseLost,
		"旧属主 A 完成必须收到失锁哨兵(0 行命中不得吞成 nil)")

	// DB 现状仍是 B 的,A 什么都没写成
	job, _ = d.FindByID(ids[0])
	assert.Equal(t, "inst-B", job.Owner)
	assert.Nil(t, job.RunID, "A 的 Complete 不得写 run_id")

	// 已过期但尚未被接管的租约不得借续租复活
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
	ok, _ = d.Claim(bID, "inst-A", -time.Minute) // TTL 负数 ⇒ 立即过期
	require.True(t, ok)
	require.ErrorIs(t, d.RenewLease(bID, "inst-A", leaseVersion(t, d, bID), time.Minute), ErrLeaseLost,
		"已过期租约不得借续租复活(执行权可能已被接管)")
}

// TestExecutionJob_RescheduleBenignBusy 良性繁忙退避(#13):
// Reschedule 放回 pending、attempt 不增、DeferCount 单独记账;
// 反复退避不得烧光毒丸预算(此前 5 次退避后作业被误判 failed 丢弃)。
func TestExecutionJob_RescheduleBenignBusy(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerWebhook, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "inst-A", time.Minute)
	require.True(t, ok)

	// 连续 6 次「执行权被占」退避(超过 MaxJobAttempts=5):
	// 每轮 = 认领者退避(被占)→ 退避窗口内不得认领 → 窗口到期换下家认领。
	owner := "inst-A"
	for i := 0; i < 6; i++ {
		require.NoError(t, d.Reschedule(ids[0], owner, leaseVersion(t, d, ids[0]), 30*time.Second))
		job, err := d.FindByID(ids[0])
		require.NoError(t, err)
		assert.Equal(t, model.JobStatusPending, job.Status, "退避后应放回 pending 等待重试")
		assert.Zero(t, job.Attempt, "良性繁忙退避不得累加 attempt(毒丸预算)")
		assert.Equal(t, i+1, job.DeferCount, "退避次数单独记账")
		require.NotNil(t, job.LeaseUntil)
		assert.True(t, job.LeaseUntil.After(time.Now()), "退避窗口内不得被提前认领")

		// not-before:退避窗口内 Claim 必须失败
		ok, err := d.Claim(ids[0], "inst-B", time.Minute)
		require.NoError(t, err)
		assert.False(t, ok, "退避未到期不得认领(防派发器忙旋)")

		// 退避到期后可正常认领(reaper 不参与,attempt 始终 0)
		require.NoError(t, d.db.Model(&model.ExecutionJob{}).Where("id = ?", ids[0]).
			Update("lease_until", time.Now().Add(-time.Second)).Error)
		ok, err = d.Claim(ids[0], "inst-B", time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		job, _ = d.FindByID(ids[0])
		assert.Equal(t, model.JobStatusClaimed, job.Status)
		owner = "inst-B"
	}

	// reaper 只接管 claimed 的过期作业;退避中的 pending 作业不得被毒丸化
	requeued, poisoned, err := d.RequeueExpired(time.Now())
	require.NoError(t, err)
	assert.Zero(t, requeued)
	assert.Zero(t, poisoned, "良性繁忙退避不得触发毒丸")
	job, _ := d.FindByID(ids[0])
	assert.NotEqual(t, model.JobStatusFailed, job.Status,
		"连续 6 次退避后作业必须还活着(pending),不得被判 failed")
	assert.Zero(t, job.Attempt)
}

// TestExecutionJob_RescheduleLost 退避重排的失锁哨兵:
// 作业已被接管(不再归本人)时 Reschedule 返回 ErrLeaseLost。
func TestExecutionJob_RescheduleLost(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	ok, _ := d.Claim(ids[0], "inst-A", -time.Minute) // 立即过期
	require.True(t, ok)

	// reaper 回收 → B 认领 → A 再退避 = 失锁
	_, _, err = d.RequeueExpired(time.Now().Add(time.Hour))
	require.NoError(t, err)
	ok, _ = d.Claim(ids[0], "inst-B", time.Minute)
	require.True(t, ok)
	require.ErrorIs(t, d.Reschedule(ids[0], "inst-A", leaseVersion(t, d, ids[0]), 30*time.Second), ErrLeaseLost)

	// B 的作业未被 A 的退避波及
	job, _ := d.FindByID(ids[0])
	assert.Equal(t, "inst-B", job.Owner)
	assert.Equal(t, model.JobStatusClaimed, job.Status)
}

// TestExecutionJob_CleanupFinished 保留期外清理。
func TestExecutionJob_CleanupFinished(t *testing.T) {
	d := setupJobTestDB(t)
	_, err := d.Enqueue("task-a", model.TriggerManual, nil)
	require.NoError(t, err)
	ids, _ := d.ListPendingIDs(1)
	_, _ = d.Claim(ids[0], "i1", time.Minute)
	require.NoError(t, d.Complete(ids[0], "i1", leaseVersion(t, d, ids[0]), 0, nil))

	// 把 finished_at 拨回 8 天前
	d.db.Model(&model.ExecutionJob{}).Where("id=?", ids[0]).
		Update("finished_at", time.Now().Add(-8*24*time.Hour))

	n, err := d.CleanupFinished(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}
