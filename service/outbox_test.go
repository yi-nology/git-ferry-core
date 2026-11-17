package service

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
	"gorm.io/gorm"
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

	repoDAO, err := repository.NewRepoRepository(db)
	require.NoError(t, err)
	taskSvc := NewTaskService(
		repository.NewSyncTaskRepository(db), repository.NewSyncRunRepository(db), repository.NewSyncRunStepRepository(db), repoDAO)
	taskSvc.outbox = repository.NewOutboxEventRepository(db)

	return &Service{
		tasks:  taskSvc,
		outbox: repository.NewOutboxEventRepository(db),
		bgCtx:  context.Background(),
		config: &model.Config{},
	}
}

// newDirectRunOutboxService 带**真实 executor** 的轻量服务:任务的源仓库
// 不存在 ⇒ Execute 快速失败(run 记录与 outbox 事件照常同事务落库),
// 正好走完 RunTaskWithTrigger 的直跑收尾路径。
func newDirectRunOutboxService(t *testing.T, key string) *Service {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{},
		&model.Repo{}, &model.Platform{}, &model.OutboxEvent{}, &model.PendingTrigger{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	repoDAO, err := repository.NewRepoRepository(db)
	require.NoError(t, err)
	platformDAO, err := repository.NewPlatformRepository(db)
	require.NoError(t, err)
	mgr := sdkrt.NewManager(0)

	svc := &Service{
		config:    &model.Config{Git: model.GitConfig{TempDir: t.TempDir()}},
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
		tasks: NewTaskService(repository.NewSyncTaskRepository(db), repository.NewSyncRunRepository(db),
			repository.NewSyncRunStepRepository(db), repoDAO),
		guard:   newLocalGuard(2),
		pending: repository.NewPendingTriggerRepository(db),
		bgCtx:   context.Background(),
	}
	svc.tasks.outbox = repository.NewOutboxEventRepository(db)
	svc.outbox = svc.tasks.outbox

	exec, err := executor.NewExecutor(svc)
	require.NoError(t, err)
	svc.executor = exec

	require.NoError(t, svc.tasks.taskRepository.Create(&model.SyncTask{
		Key: key, Name: "direct", Enabled: true,
		SourceRepoKey: "missing/repo", TargetRepoKey: "missing/tgt",
		SourceBranch: "main", TargetBranch: "main",
	}))
	return svc
}

// TestOutbox_DirectPathNoSubscriberKeepsEvent RunTaskWithTrigger **直跑路径**
// 同样不得「未投递先闭环」(#38):无订阅者时 emitRunCompleted 返回 false,
// 不得按 run 标记已发布 —— 旧实现丢弃 delivered 无条件 markOutboxPublishedByRun,
// 无订阅者(壳层未注册 runwatch/进程刚启动/纯 CLI 模式)时完成事件被标记
// 已发布却从未投递 ⇒ 通知与自动补偿永久丢失,且 outbox 里不可见(已标记
// published),运维无法事后补偿。#15 只修了异步路径(finishRun),这是直跑
// 路径的遗漏面。
func TestOutbox_DirectPathNoSubscriberKeepsEvent(t *testing.T) {
	svc := newDirectRunOutboxService(t, "ob-direct")

	// 直跑一次(源仓库缺失 ⇒ run 失败,但 run/outbox 事件已落库)
	err := svc.RunTaskWithTrigger(context.Background(), "ob-direct", model.TriggerWebhook, nil)
	require.Error(t, err, "仓库缺失的执行应失败")

	// 事件确实落库了(同事务 invariant,两种实现都应满足)
	var total int64
	require.NoError(t, svc.tasks.runRepository.DB().Table("outbox_events").
		Where("task_key = ?", "ob-direct").Count(&total).Error)
	require.Equal(t, int64(1), total, "直跑路径必须同事务落一条完成事件")

	// 关键断言:无订阅者时事件不得被标记已发布
	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "直跑路径无订阅者时事件不得被标记已发布(#38)")

	// 订阅者注册后派发器重投闭环(通知不丢)
	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })
	svc.publishDueOutbox(0)
	select {
	case ev := <-got:
		require.NotNil(t, ev.Run)
		assert.Equal(t, "ob-direct", ev.TaskKey)
	case <-time.After(2 * time.Second):
		t.Fatal("直跑路径未闭环的事件,派发器应重投给订阅者")
	}
	n, err = svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n, "投递后事件必须闭环")
}

// TestOutbox_DirectPathSubscriberClosesEvent 对照:有订阅者时直跑路径
// 正常闭环(投递成功 ⇒ 标记已发布,派发器宽限后不重投)。
func TestOutbox_DirectPathSubscriberClosesEvent(t *testing.T) {
	svc := newDirectRunOutboxService(t, "ob-direct-sub")

	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })

	err := svc.RunTaskWithTrigger(context.Background(), "ob-direct-sub", model.TriggerCron, nil)
	require.Error(t, err)

	select {
	case ev := <-got:
		require.NotNil(t, ev.Run)
		assert.Equal(t, "ob-direct-sub", ev.TaskKey)
	case <-time.After(2 * time.Second):
		t.Fatal("有订阅者时直跑路径必须投递完成事件")
	}
	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n, "快速路径已投递 ⇒ 事件闭环")
}

// TestOutbox_CompletionWritesEventInTx 运行完成必须**同事务**写 outbox 事件:
// 否则「run 已落库、事件丢失」的崩溃窗口原样存在,outbox 白做。
func TestOutbox_CompletionWritesEventInTx(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-tx", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	end := time.Now()
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: end.Add(-time.Second), EndTime: &end,
		ErrorMessage: "boom"}
	require.NoError(t, svc.tasks.runRepository.Create(run))

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
	require.NoError(t, svc.tasks.runRepository.Create(run2))
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
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	end := time.Now().Add(-time.Minute)
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: end.Add(-time.Second), EndTime: &end}
	require.NoError(t, svc.tasks.runRepository.Create(run))

	// 模拟崩溃残留:未发布、已过宽限期的事件
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
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

// TestOutbox_NoSubscriberNotMarkedPublished 无订阅者时事件**不得被标记
// 已发布**(#15):启动零宽限扫描先于壳层 SubscribeRuns 执行,若无订阅者
// 也照单标记,事件一个字节都没投出去就闭环 —— at-least-once 退化成
// at-most-once,通知与自动补偿永久丢失。修复后:无订阅者 ⇒ 不标记,
// 补上订阅者再扫 ⇒ 投递并闭环。
func TestOutbox_NoSubscriberNotMarkedPublished(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-nosub", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	end := time.Now().Add(-time.Minute)
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: end.Add(-time.Second), EndTime: &end}
	require.NoError(t, svc.tasks.runRepository.Create(run))
	// 模拟崩溃残留:未发布、已过宽限期的事件
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
		Topic: model.OutboxTopicRunCompleted, TaskKey: task.Key, RunID: run.ID,
		Trigger: model.TriggerCron, Status: model.StatusFailed,
		CreatedAt: time.Now().Add(-2 * time.Minute),
	}))

	// 关键差异:扫描时刻**没有订阅者**(= NewService 启动扫描的必然情形)
	svc.publishDueOutbox(0)

	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "无订阅者时事件不得被标记已发布(投递字节都没出去)")

	// 补上订阅者后再扫 ⇒ 事件被投递并闭环
	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })
	svc.publishDueOutbox(0)

	select {
	case ev := <-got:
		assert.Equal(t, task.Key, ev.TaskKey)
		require.NotNil(t, ev.Run)
		assert.Equal(t, run.ID, ev.Run.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("订阅者注册后,此前未投递的事件没有被重投")
	}
	n, err = svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n, "投递后事件必须闭环")
}

// TestOutbox_FastPathNoSubscriberKeepsEvent 快速路径同样不得「未投递先闭环」:
// finishRun 在无订阅者时不得按 run 标记已发布,事件留在 outbox 等派发器重投。
func TestOutbox_FastPathNoSubscriberKeepsEvent(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-fastnosub", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	end := time.Now()
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusSuccess, StartTime: end, EndTime: &end}
	require.NoError(t, svc.tasks.runRepository.Create(run))
	require.NoError(t, svc.tasks.CompleteRun(run)) // 同事务写 outbox 事件

	// 此刻无订阅者:finishRun 语义下不得闭环
	svc.finishRun(task.Key, model.TriggerCron, run, nil, func() {})

	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "快速路径无订阅者时事件不得被标记已发布")

	// 订阅者注册后派发器重投闭环
	got := make(chan RunEvent, 1)
	svc.SubscribeRuns(func(ev RunEvent) { got <- ev })
	svc.publishDueOutbox(0)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("快速路径未闭环的事件,派发器应重投给订阅者")
	}
	n, err = svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestOutbox_GraceSkipsFreshEvents 宽限内的事件(快速路径正在处理中)
// 不得被派发器抢投 —— 否则每条事件都投两遍。
func TestOutbox_GraceSkipsFreshEvents(t *testing.T) {
	svc := newOutboxTestService(t)

	task := &model.SyncTask{Key: "ob-grace", Name: "ob", Enabled: true,
		SourceBranch: "main", TargetBranch: "main"}
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerCron,
		Status: model.StatusFailed, StartTime: time.Now()}
	require.NoError(t, svc.tasks.runRepository.Create(run))
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
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
	require.NoError(t, svc.tasks.taskRepository.Create(task))
	end := time.Now().Add(-time.Minute)
	run := &model.SyncRun{TaskKey: task.Key, TriggerSource: model.TriggerWebhook,
		Status: model.StatusSuccess, StartTime: end, EndTime: &end}
	require.NoError(t, svc.tasks.runRepository.Create(run))
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
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

	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
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

// TestOutbox_DriftTopicRouting 漂移事件按 Topic 分发:
// 不走 run 查找(RunID=0 会被误判废弃),投给 drift 订阅者。
func TestOutbox_DriftTopicRouting(t *testing.T) {
	svc := newOutboxTestService(t)

	// 落一条 drift 事件(与 recordReconcile 同形态)
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
		Topic:   model.OutboxTopicReconcileDrift,
		TaskKey: "t-drift",
		Status:  model.ReconcileDrift,
		Payload: `{"status":"drift","source_sha":"aaa","target_sha":"bbb","source_ahead":3,"message":"两端漂移","checked_at":"2026-11-15T00:00:00Z"}`,
	}))

	got := make(chan ReconcileDrift, 1)
	svc.SubscribeReconcileDrift(func(d ReconcileDrift) { got <- d })

	svc.publishDueOutbox(0)

	select {
	case d := <-got:
		assert.Equal(t, "t-drift", d.TaskKey)
		assert.Equal(t, "drift", d.Status)
		assert.Equal(t, 3, d.SourceAhead)
		assert.Equal(t, "aaa", d.SourceSHA)
		assert.Contains(t, d.Message, "两端漂移")
	case <-time.After(2 * time.Second):
		t.Fatal("drift 事件未投递给订阅者")
	}
	// 已投递 ⇒ 闭环
	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestOutbox_DriftNoSubscriberNotMarked 无 drift 订阅者时不标记已发布
// (留待壳层注册订阅后重投——与 run 事件同语义)。
func TestOutbox_DriftNoSubscriberNotMarked(t *testing.T) {
	svc := newOutboxTestService(t)
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
		Topic:   model.OutboxTopicReconcileDrift,
		TaskKey: "t-x",
		Status:  model.ReconcileDrift,
		Payload: `{"status":"drift"}`,
	}))

	svc.publishDueOutbox(0)

	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "无订阅者不得标记已发布(否则订阅者注册后永远收不到)")
}

// TestOutbox_DriftBadPayloadCloses 坏 payload 跳过并闭环(不无限重试)。
func TestOutbox_DriftBadPayloadCloses(t *testing.T) {
	svc := newOutboxTestService(t)
	require.NoError(t, svc.outbox.CreateInTx(svc.tasks.runRepository.DB(), &model.OutboxEvent{
		Topic:   model.OutboxTopicReconcileDrift,
		TaskKey: "t-bad",
		Payload: `{not json`,
	}))
	got := make(chan ReconcileDrift, 1)
	svc.SubscribeReconcileDrift(func(d ReconcileDrift) { got <- d })

	svc.publishDueOutbox(0)

	n, err := svc.outbox.CountUnpublished()
	require.NoError(t, err)
	assert.Zero(t, n, "坏 payload 应闭环(不无限重试)")
	select {
	case <-got:
		t.Fatal("坏 payload 不应投递")
	case <-time.After(100 * time.Millisecond):
	}
}
