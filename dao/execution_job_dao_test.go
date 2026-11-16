package dao

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oekit/pkg/ptrx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func setupJobTestDB(t *testing.T) *ExecutionJobDAO {
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
	return NewExecutionJobDAO(db)
}

// taskState 读某任务的队列状态行(测试断言代次账本用)。
func taskState(t *testing.T, d *ExecutionJobDAO, taskKey string) *model.ExecutionTaskState {
	t.Helper()
	var st model.ExecutionTaskState
	require.NoError(t, d.db.Where("task_key = ?", taskKey).First(&st).Error)
	return &st
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
	require.ErrorIs(t, d.Complete(bID, "i2", 0, assert.AnError), ErrLeaseLost,
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

	require.NoError(t, d.Complete(ids[0], "worker-A", 42, nil))

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
	require.NoError(t, d.Complete(ids2[0], "worker-A", 43, nil))
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

// TestExecutionJob_EnqueueSQLQuotesTrigger 入队裸 SQL 的列名必须反引号包裹:
// `trigger` 是 MySQL 保留字,裸写在 MySQL 下每条 INSERT 都 1064 语法错误
// (sqlite 宽容保留字作标识符,既有单测全绿测不出来)。钉住语句文本,
// 防止方言坑回归(#12)。
func TestExecutionJob_EnqueueSQLQuotesTrigger(t *testing.T) {
	d := setupJobTestDB(t)

	// 功能面:sqlite(接受反引号标识符)入队语义不回归
	enq, err := d.Enqueue("task-quote", model.TriggerWebhook, nil)
	require.NoError(t, err)
	assert.True(t, enq)

	// 语句面:enqueue 使用的裸 SQL 不含裸写的 trigger 列
	sql := enqueueJobSQL()
	assert.Contains(t, sql, "`trigger`", "trigger 列必须反引号包裹(MySQL 保留字)")
	assert.NotRegexp(t, `(?m)[^`+"`"+`]trigger[,)]`, sql,
		"不得出现未加引号的 trigger 列名")
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

	// 对照:仍是属主时续租正常生效(lease_until 前推)
	job, _ := d.FindByID(ids[0])
	before := *job.LeaseUntil
	require.NoError(t, d.RenewLease(ids[0], "inst-A", 5*time.Minute))
	job, _ = d.FindByID(ids[0])
	assert.True(t, job.LeaseUntil.After(before), "有效续租必须前推 lease_until")

	// 失锁路径:reaper 回收 → B 认领 → A 的续租与完成都要报 ErrLeaseLost
	_, _, err = d.RequeueExpired(time.Now().Add(time.Hour))
	require.NoError(t, err)
	ok, _ = d.Claim(ids[0], "inst-B", time.Minute)
	require.True(t, ok)

	require.ErrorIs(t, d.RenewLease(ids[0], "inst-A", time.Minute), ErrLeaseLost,
		"旧属主 A 续租必须收到失锁哨兵(0 行命中不得吞成 nil)")
	require.ErrorIs(t, d.Complete(ids[0], "inst-A", 1, nil), ErrLeaseLost,
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
	require.ErrorIs(t, d.RenewLease(bID, "inst-A", time.Minute), ErrLeaseLost,
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
		require.NoError(t, d.Reschedule(ids[0], owner, 30*time.Second))
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
	require.ErrorIs(t, d.Reschedule(ids[0], "inst-A", 30*time.Second), ErrLeaseLost)

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
	require.NoError(t, d.Complete(ids[0], "i1", 0, nil))

	// 把 finished_at 拨回 8 天前
	d.db.Model(&model.ExecutionJob{}).Where("id=?", ids[0]).
		Update("finished_at", time.Now().Add(-8*24*time.Hour))

	n, err := d.CleanupFinished(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}
