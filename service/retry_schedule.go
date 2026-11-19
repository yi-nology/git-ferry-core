package service

import (
	"context"
	"time"

	"github.com/oekit/logx"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
)

// ===== 延迟重跑派发器(#39) =====
//
// 冷却期只是「时间未到」,不是「不再重试」。RetryTracker 判定为
// Deferred 时把排程落进 retry_schedules(run_id 唯一),本派发器周期认领
// 到点排程并触发 RunTaskAsync(trigger=auto_retry):
//
//	失败 run → ShouldRetry(冷却中) → retry_schedules(pending,next_attempt_at)
//	   → 任意实例 ClaimDue(条件更新,单胜者) → RunTaskAsync → dispatched
//
// 收益:
//   - 冷却内失败不再吞预算(预算仍按 DB 里的 run 链长度计,排程不计数);
//   - 进程重启后排程仍在,到点继续;
//   - 多实例只有一个实例认领(条件更新),无需额外 leader election。
const (
	// retryDispatchInterval 到点排程扫描周期。
	retryDispatchInterval = 5 * time.Second
	// retryDispatchLease 认领租约:认领后把 next_attempt_at 推后这么久,
	// 防止同一排程被并发重复触发;进程崩溃后租约到期可被其它实例重试。
	retryDispatchLease = 30 * time.Second
	// retryDispatchBatch 单轮认领上限。
	retryDispatchBatch = 10
	// retryDispatchBackoff 触发失败(任务忙等可恢复原因)后的重试间隔。
	retryDispatchBackoff = 30 * time.Second
)

// scheduleDeferredRetry 把冷却期判定落成持久化排程(RetryTracker 注入)。
// 同一失败 run 只允许一条排程(run_id 唯一),重复判定/重启重放幂等。
func (s *Service) scheduleDeferredRetry(run *model.SyncRun, d Decision) error {
	if s.retrySchedules == nil {
		return oekiterrors.New("retry schedule store not wired")
	}
	root := run.ID
	if s.tasks != nil && s.tasks.runRepository != nil {
		r, err := s.tasks.runRepository.RetryChainRoot(run.TaskKey, run.ID)
		if err != nil {
			return err
		}
		root = r
	}
	next := time.Now().Add(d.RetryAfter)
	if d.RetryAfter <= 0 {
		next = time.Now()
	}
	created, err := s.retrySchedules.Create(s.bgCtx, &model.RetrySchedule{
		RunID:         run.ID,
		TaskKey:       run.TaskKey,
		RootRunID:     root,
		Attempt:       d.Attempt,
		AttemptLimit:  d.AttemptLimit,
		Reason:        d.Reason,
		NextAttemptAt: next,
		Status:        model.RetrySchedulePending,
	})
	if err != nil {
		return err
	}
	if !created {
		logx.Debug("auto retry: deferred schedule already exists (idempotent)",
			"run_id", run.ID, "task", run.TaskKey)
	}
	return nil
}

// dispatchRetrySynchronously 非队列模式:同步执行一次重跑,结束后关闭排程。
//
// 预检失败(任务不存在/禁用/忙)按原语义取消或退避;真正的执行失败说明
// 重试已发生(run 已落库),关闭排程,后续预算由 runwatch 按 run 链继续。
func (s *Service) dispatchRetrySynchronously(sch model.RetrySchedule) {
	ctx := s.bgCtx
	if ctx == nil {
		ctx = context.Background()
	}
	err := s.RunTaskWithTrigger(ctx, sch.TaskKey, model.TriggerAutoRetry, nil)
	switch {
	case err == nil:
		s.markRetryScheduleDispatched(sch)
	case oekiterrors.Is(err, ErrTaskNotFound), oekiterrors.Is(err, ErrTaskDisabled):
		if cerr := s.retrySchedules.Cancel(s.bgCtx, sch.ID, err); cerr != nil &&
			!oekiterrors.Is(cerr, repository.ErrScheduleLost) {
			logx.Error("auto retry: cancel schedule failed", "schedule_id", sch.ID, "error", cerr)
		}
	case oekiterrors.Is(err, ErrTaskRunning), oekiterrors.Is(err, ErrTooManyConcurrent):
		if rerr := s.retrySchedules.Reschedule(s.bgCtx, sch.ID, time.Now().Add(retryDispatchBackoff), err); rerr != nil &&
			!oekiterrors.Is(rerr, repository.ErrScheduleLost) {
			logx.Error("auto retry: reschedule failed", "schedule_id", sch.ID, "error", rerr)
		}
	default:
		// 执行已发生(run 已落库)但结果失败:排程使命完成,交给 runwatch 链。
		s.markRetryScheduleDispatched(sch)
	}
}

func (s *Service) markRetryScheduleDispatched(sch model.RetrySchedule) {
	if merr := s.retrySchedules.MarkDispatched(s.bgCtx, sch.ID); merr != nil {
		if oekiterrors.Is(merr, repository.ErrScheduleLost) {
			logx.Debug("auto retry: schedule already closed by another actor", "schedule_id", sch.ID)
			return
		}
		logx.Error("auto retry: mark schedule dispatched failed", "schedule_id", sch.ID, "error", merr)
		return
	}
	logx.Info("auto retry: deferred retry dispatched",
		"task", sch.TaskKey, "run_id", sch.RunID, "attempt", sch.Attempt)
}

// retryDispatchLoop 周期派发到点排程(启动即扫一轮,重启后延迟重试不丢)。
func (s *Service) retryDispatchLoop() {
	loop.Run(s.bgCtx, loop.Config{
		Interval:   retryDispatchInterval,
		FirstDelay: 0,
		Name:       "dispatch-retry-schedules",
	}, func(context.Context) {
		s.retryDispatchOnce()
	}).Wait()
}

// retryDispatchOnce 认领并触发一批到点排程;单条失败不影响其它排程。
func (s *Service) retryDispatchOnce() {
	if s.retrySchedules == nil {
		return
	}
	due, err := s.retrySchedules.ClaimDue(s.bgCtx, time.Now(), retryDispatchBatch, retryDispatchLease)
	if err != nil {
		logx.Error("auto retry: claim due schedules failed", "error", err)
		return
	}
	for i := range due {
		sch := due[i]
		// **非队列模式不能"先标 dispatched 再异步跑"**:
		// RunTaskAsync 返回不代表 run 已落库,进程在 goroutine 创建 run 前崩溃
		// 会让排程已关闭、重试永久丢失。这里改为同步执行(独立 goroutine,
		// 不阻塞派发器),执行完/确认已受理后才关闭排程;租约窗口内不会重复认领。
		if !s.durableQueueOn() {
			schCopy := sch
			async.GoSafe(func() { s.dispatchRetrySynchronously(schCopy) })
			continue
		}
		err := s.RunTaskAsync(sch.TaskKey, model.TriggerAutoRetry, nil)
		switch {
		case err == nil:
			if merr := s.retrySchedules.MarkDispatched(s.bgCtx, sch.ID); merr != nil {
				if oekiterrors.Is(merr, repository.ErrScheduleLost) {
					logx.Debug("auto retry: schedule already closed by another actor",
						"schedule_id", sch.ID)
				} else {
					logx.Error("auto retry: mark schedule dispatched failed",
						"schedule_id", sch.ID, "error", merr)
				}
			}
			logx.Info("auto retry: deferred retry dispatched",
				"task", sch.TaskKey, "run_id", sch.RunID, "attempt", sch.Attempt)
		case oekiterrors.Is(err, ErrTaskNotFound), oekiterrors.Is(err, ErrTaskDisabled):
			// 任务已删除/禁用 ⇒ 重试无意义,关掉排程防止反复拉起。
			if cerr := s.retrySchedules.Cancel(s.bgCtx, sch.ID, err); cerr != nil &&
				!oekiterrors.Is(cerr, repository.ErrScheduleLost) {
				logx.Error("auto retry: cancel schedule failed", "schedule_id", sch.ID, "error", cerr)
			}
		default:
			// 任务忙/队列暂不可用等可恢复原因:保留 pending,稍后再来。
			if rerr := s.retrySchedules.Reschedule(s.bgCtx, sch.ID, time.Now().Add(retryDispatchBackoff), err); rerr != nil {
				if oekiterrors.Is(rerr, repository.ErrScheduleLost) {
					logx.Debug("auto retry: schedule already closed, skip reschedule",
						"schedule_id", sch.ID)
				} else {
					logx.Error("auto retry: reschedule failed", "schedule_id", sch.ID, "error", rerr)
				}
			}
			logx.Warn("auto retry: deferred retry not dispatched, will retry",
				"task", sch.TaskKey, "run_id", sch.RunID, "error", err)
		}
	}
}
