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

// newOutboxTestService 造带 outbox 的轻量服务(不跑 executor)。
func newOutboxTestService(t *testing.T) *Service {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{},
		&model.Repo{}, &model.OutboxEvent{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	taskSvc := NewTaskService(
		dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db), dao.NewSyncRunStepDAO(db), repoDAO)
	taskSvc.outbox = dao.NewOutboxEventDAO(db)

	return &Service{
		tasks:  taskSvc,
		outbox: dao.NewOutboxEventDAO(db),
		bgCtx:  context.Background(),
		config: &model.Config{},
	}
}

// TestOutbox_CompletionWritesEventInTx 运行完成必须**同事务**写 outbox 事件:
// 否则「run 已落库、事件丢失」的崩溃窗口原样存在,outbox 白做。
func TestOutbox_CompletionWritesEventInTx(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-tx", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskDAO.Create(task))
	end := time.Now()
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: end.Add(-time.Second), EndTime: &end,
		ErrorMessage: "boom"}
	require.NoError(t, svc.tasks.runDAO.Create(run))

	require.NoError(t, svc.tasks.CompleteRunWithTaskUpdate(run, task))

	evs, err := svc.outbox.ListDue(10, time.Now())
	require.NoError(t, err)
	require.Len(t, evs, 1, "完成运行必须同事务写一条 outbox 事件")
	ev := evs[0]
	assert.Equal(t, model.OutboxTopicRunCompleted, ev.Topic)
	assert.Equal(t, run.ID, ev.RunID)
	assert.Equal(t, model.StatusFailed, ev.Status)
	assert.Equal(t, model.TriggerCron, ev.Trigger)
	assert.Contains(t, ev.Payload, "boom")
	assert.Nil(t, ev.PublishedAt, "刚落库未投递,不得预标记已发布")

	// CompleteRun(不带 task 更新)路径同样写事件
	run2 := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerManual,
		Status: model.StatusSuccess, StartTime: time.Now(), EndTime: &end}
	require.NoError(t, svc.tasks.runDAO.Create(run2))
	require.NoError(t, svc.tasks.CompleteRun(run2))
	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

// TestOutbox_CrashRecoveryRedelivers 崩溃场景:事件落库但未标记已发布
// (进程死在「落库后、投递前」) ⇒ 派发器重投给订阅者并闭环。
// 对应评审验收:「运行结果落库后立即 SIGKILL ⇒ 恢复后通知与补偿最终完成」。
func TestOutbox_CrashRecoveryRedelivers(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-crash", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskDAO.Create(task))
	end := time.Now().Add(-time.Minute)
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: end.Add(-time.Second), EndTime: &end}
	require.NoError(t, svc.tasks.runDAO.Create(run))

	// 模拟崩溃残留:未发布、已过宽限期的事件
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runDAO.DB(), &model.OutboxEvent{
		Topic: model.OutboxTopicRunCompleted, TaskKey: task.Key, RunID: run.ID,
		Trigger: model.TriggerCron, Status: model.StatusFailed,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}))

	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })

	svc.publishDueOutbox(0) // 零宽限 = 启动扫描语义

	select {
	case ev := <-got:
		assert.Equal(t, task.Key, ev.TaskKey)
		require.NotNil(t, ev.Run)
		assert.Equal(t, run.ID, ev.Run.ID)
		assert.Equal(t, model.StatusFailed, ev.Run.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("崩溃残留事件未被重投")
	}

	// 重投后必须闭环:再扫不得重复投递(at-least-once 不放大成无限重复)
	svc.publishDueOutbox(0)
	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n, "重投后事件必须标记已发布")
	select {
	case <-got:
		t.Fatal("已发布事件被重复重投")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestOutbox_GraceSkipsFreshEvents 宽限内的事件(快速路径正在处理中)
// 不得被派发器抢投 —— 否则每条事件都投两遍。
func TestOutbox_GraceSkipsFreshEvents(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-grace", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskDAO.Create(task))
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: time.Now()}
	require.NoError(t, svc.tasks.runDAO.Create(run))
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runDAO.DB(), &model.OutboxEvent{
		Topic: model.OutboxTopicRunCompleted, TaskKey: task.Key, RunID: run.ID,
		Status: model.StatusFailed, CreatedAt: time.Now(), // 刚落库
	}))

	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })

	svc.publishDueOutbox(30 * time.Second) // 宽限 30s,事件是新的 ⇒ 不投

	select {
	case <-got:
		t.Fatal("宽限内的事件被派发器抢投(会退化成每条事件双投)")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestOutbox_FastPathClosesEvent 快速路径投递成功后标记已发布,
// 派发器宽限过后不会重复投递。
func TestOutbox_FastPathClosesEvent(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-fast", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskDAO.Create(task))
	end := time.Now().Add(-time.Minute)
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerWebhook,
		Status: model.StatusSuccess, StartTime: end, EndTime: &end}
	require.NoError(t, svc.tasks.runDAO.Create(run))
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runDAO.DB(), &model.OutboxEvent{
		Topic: model.OutboxTopicRunCompleted, TaskKey: task.Key, RunID: run.ID,
		Status: model.StatusSuccess, CreatedAt: time.Now().Add(-2 * time.Minute),
	}))

	svc.markOutboxPublishedByRun(run.ID)

	due, err := svc.outbox.ListDue(10, time.Now())
	require.NoError(t, err)
	assert.Empty(t, due, "快速路径闭环后派发器不得再见到该事件")
}

// TestOutbox_RunCleanedSkipDelivery run 记录已被保留期清理时,
// 重投 gracefully 跳过(不 panic、不阻塞派发器)。
func TestOutbox_RunCleanedSkipDelivery(t *testing.T) {
	svc := newOutboxTestService(t)

	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runDAO.DB(), &model.OutboxEvent{
		Topic: model.OutboxTopicRunCompleted, TaskKey: "gone", RunID: 999,
		Status: model.StatusSuccess, CreatedAt: time.Now().Add(-time.Hour),
	}))

	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })

	assert.NotPanics(t, func() { svc.publishDueOutbox(0) })

	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n, "run 缺失的事件也要闭环标记,否则会每轮重试")
	select {
	case <-got:
		t.Fatal("run 不存在时不应投递")
	case <-time.After(100 * time.Millisecond):
	}
}
