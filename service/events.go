package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 运行完成事件 =====
//
// 此前壳层只能用 ListHistory 轮询当事件源（runwatch 自述「core 不暴露完成回调」）；
// 本文件提供进程内订阅：RunTaskAsync / RunTaskWithTrigger 执行收尾时广播。

// RunEvent 一次运行结束的事件。
type RunEvent struct {
	TaskKey string
	// Run 完成态快照（CreateRun 失败时为 nil）。
	Run     *model.SyncRun
	Trigger string
	// Err Execute 返回的错误（结果本身以 Run.Status 为准）。
	Err error
	At  time.Time
}

// runSubscriber 订阅者：每订阅者独立 goroutine 分发，慢回调不阻塞同步主流程。
type runSubscriber struct {
	id uint64
	fn func(RunEvent)
}

// SubscribeRuns 注册运行完成回调，返回取消函数（幂等）。
// 回调在独立 goroutine 中执行并 recover panic；调用方需自行保证非阻塞语义
// （要串行处理就自己起 worker）。Stop/进程退出不等待订阅者。
func (s *Service) SubscribeRuns(fn func(RunEvent)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	s.runSubMu.Lock()
	s.runSubID++
	id := s.runSubID
	s.runSubs = append(s.runSubs, runSubscriber{id: id, fn: fn})
	s.runSubMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.runSubMu.Lock()
			defer s.runSubMu.Unlock()
			for i := range s.runSubs {
				if s.runSubs[i].id == id {
					s.runSubs = append(s.runSubs[:i], s.runSubs[i+1:]...)
					break
				}
			}
		})
	}
}

// emitRunCompleted 广播一次运行结束（快照订阅者后异步分发）。
//
// 这是**快速路径**:outbox 行已在 CompleteRun*(同一事务)落库,
// 这里投递进程内订阅,随后由 finishRun 标记已发布;
// 崩溃在标记之前 ⇒ 后台派发器/启动扫描重投(at-least-once)。
func (s *Service) emitRunCompleted(taskKey, trigger string, run *model.SyncRun, err error) {
	subs := s.snapshotRunSubs()
	if len(subs) == 0 {
		return
	}
	s.dispatchRunEvent(subs, RunEvent{TaskKey: taskKey, Run: run, Trigger: trigger, Err: err, At: time.Now()})
}

// snapshotRunSubs 快照当前订阅者列表(持锁时间最短化)。
func (s *Service) snapshotRunSubs() []runSubscriber {
	s.runSubMu.Lock()
	defer s.runSubMu.Unlock()
	if len(s.runSubs) == 0 {
		return nil
	}
	subs := make([]runSubscriber, len(s.runSubs))
	copy(subs, s.runSubs)
	return subs
}

// dispatchRunEvent 把事件分发给给定订阅者(每订阅者独立 goroutine,
// 慢回调不阻塞派发;panic 由 GoSafe recover)。
func (s *Service) dispatchRunEvent(subs []runSubscriber, ev RunEvent) {
	for _, sub := range subs {
		fn := sub.fn
		async.GoSafe(func() { fn(ev) })
	}
}

// deliverOutboxEvent 把一条 outbox 事件重投给当前订阅者。
// run 从 DB 重新加载(状态以库为准,不用事件里的快照);
// 重投事件的 Err 恒为 nil —— 错误信息已落在 run.ErrorMessage,
// 订阅方应以 Run.Status / Run.ErrorMessage 为准。
func (s *Service) deliverOutboxEvent(ev model.OutboxEvent) {
	subs := s.snapshotRunSubs()
	if len(subs) == 0 {
		return
	}
	run, err := s.tasks.runDAO.FindByTaskKeyAndRunID(ev.TaskKey, ev.RunID)
	if err != nil || run == nil {
		// run 记录已被清理(保留期) ⇒ 事件失去意义,直接放弃投递。
		slog.Warn("outbox: 重投时 run 记录不存在,跳过", "run_id", ev.RunID, "task", ev.TaskKey)
		return
	}
	s.dispatchRunEvent(subs, RunEvent{TaskKey: ev.TaskKey, Run: run, Trigger: ev.Trigger, At: time.Now()})
}

// publishDueOutbox 派发「已过宽限期仍未标记已发布」的 outbox 事件,
// 并顺手清理过期已发布事件。快速路径正常闭环时这里应恒为空转 ——
// 不为空转 = 有崩溃/异常发生,这正是它存在的意义。
func (s *Service) publishDueOutbox(grace time.Duration) {
	if s.outbox == nil {
		return
	}
	evs, err := s.outbox.ListDue(100, time.Now().Add(-grace))
	if err != nil {
		slog.Error("outbox: 查询待派发事件失败", "error", err)
		return
	}
	published := make([]uint, 0, len(evs))
	for i := range evs {
		s.deliverOutboxEvent(evs[i])
		published = append(published, evs[i].ID)
	}
	if err := s.outbox.MarkPublished(published); err != nil {
		slog.Error("outbox: 标记已发布失败(下轮可能重复投递,订阅方需幂等)", "error", err)
	}
	if _, err := s.outbox.CleanupPublished(context.Background(), outboxRetention); err != nil {
		slog.Warn("outbox: 清理过期事件失败", "error", err)
	}
}

// outboxRetention 已发布事件的保留期:重投窗口远小于它,保留是为了可审计。
const outboxRetention = 24 * time.Hour

// dispatchOutboxLoop 周期派发器:宽限给快速路径留足标记时间,
// 周期扫尾兜底崩溃恢复;启动时由 NewService 先做一次零宽限扫描。
func (s *Service) dispatchOutboxLoop() {
	loop.Run(s.bgCtx, loop.Config{
		Interval:   outboxDispatchInterval,
		FirstDelay: outboxDispatchInterval,
		Name:       "dispatch-outbox-events",
	}, func(ctx context.Context) {
		s.publishDueOutbox(outboxFastPathGrace)
	}).Wait()
}

// outboxFastPathGrace 快速路径标记宽限:事件落库后这段时间内
// finishRun 应已完成「投递 + 标记」;超过仍未标记 ⇒ 按崩溃处理重投。
const outboxFastPathGrace = 30 * time.Second

const outboxDispatchInterval = 10 * time.Second

// markOutboxPublishedByRun 快速路径投递成功后闭环 outbox 行。
func (s *Service) markOutboxPublishedByRun(runID uint) {
	if s.outbox == nil || runID == 0 {
		return
	}
	if err := s.outbox.MarkPublishedByRun(runID); err != nil {
		slog.Warn("outbox: 标记已发布失败(派发器宽限后会重投一次)", "run_id", runID, "error", err)
	}
}
