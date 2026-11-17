package service

import (
	"context"
	"github.com/oekit/logx"
	"time"

	"github.com/oekit/concurrency/loop"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
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
	created, err := s.retrySchedules.Create(&model.RetrySchedule{
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
	due, err := s.retrySchedules.ClaimDue(time.Now(), retryDispatchBatch, retryDispatchLease)
	if err != nil {
		logx.Error("auto retry: claim due schedules failed", "error", err)
		return
	}
	for i := range due {
		sch := due[i]
		err := s.RunTaskAsync(sch.TaskKey, model.TriggerAutoRetry, nil)
		switch {
		case err == nil:
			if merr := s.retrySchedules.MarkDispatched(sch.ID); merr != nil {
				logx.Error("auto retry: mark schedule dispatched failed",
					"schedule_id", sch.ID, "error", merr)
			}
			logx.Info("auto retry: deferred retry dispatched",
				"task", sch.TaskKey, "run_id", sch.RunID, "attempt", sch.Attempt)
		case oekiterrors.Is(err, ErrTaskNotFound), oekiterrors.Is(err, ErrTaskDisabled):
			// 任务已删除/禁用 ⇒ 重试无意义,关掉排程防止反复拉起。
			if cerr := s.retrySchedules.Cancel(sch.ID, err); cerr != nil {
				logx.Error("auto retry: cancel schedule failed", "schedule_id", sch.ID, "error", cerr)
			}
		default:
			// 任务忙/队列暂不可用等可恢复原因:保留 pending,稍后再来。
			if rerr := s.retrySchedules.Reschedule(sch.ID, time.Now().Add(retryDispatchBackoff), err); rerr != nil {
				logx.Error("auto retry: reschedule failed", "schedule_id", sch.ID, "error", rerr)
			}
			logx.Warn("auto retry: deferred retry not dispatched, will retry",
				"task", sch.TaskKey, "run_id", sch.RunID, "error", err)
		}
	}
}
