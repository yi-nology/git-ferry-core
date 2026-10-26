package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oekit/concurrency/loop"
	"github.com/oekit/concurrency/syncx"
)

// cleanupTasks 列出并行清理的三张表及其执行体。
// 顺序即 FanInErr 的槽位序:events / runs / steps。
var cleanupTasks = []struct {
	name string
	run  func(ctx context.Context, s *Service, maxAge time.Duration) (int64, error)
}{
	{"CleanupOldEvents", func(ctx context.Context, s *Service, maxAge time.Duration) (int64, error) {
		return s.webhooks.CleanupOldEvents(ctx, maxAge)
	}},
	{"CleanupOldRuns", func(ctx context.Context, s *Service, maxAge time.Duration) (int64, error) {
		return s.tasks.CleanupOldRuns(ctx, maxAge)
	}},
	{"CleanupOldRunSteps", func(ctx context.Context, s *Service, maxAge time.Duration) (int64, error) {
		return s.tasks.CleanupOldRunSteps(ctx, maxAge)
	}},
}

func (s *Service) CleanupOldData(ctx context.Context, maxAge time.Duration) (events, runs, steps int64, err error) {
	// 三张表独立清理,互不依赖,并行执行缩短耗时。
	// 用 oekit concurrency/syncx.FanInErr:逐槽收编 panic(槽 Err 为 *async.PanicError,
	// 带栈),结果按槽位序返回,不必自维护 channel 与 recover 样板。
	slots := syncx.FanInErr(len(cleanupTasks), func(i int) (int64, error) {
		t := cleanupTasks[i]
		return t.run(ctx, s, maxAge)
	})

	events = slots[0].Val
	runs = slots[1].Val
	steps = slots[2].Val

	// 聚合所有错误而非只保留最后一个,方便排查多个表同时出问题的情况。
	// panic 也在这里冒泡:槽 Err 带 *async.PanicError,文案保留表名。
	var errs []error
	for i, slot := range slots {
		if slot.Err != nil {
			errs = append(errs, fmt.Errorf("cleanup %s: %w", cleanupTasks[i].name, slot.Err))
		}
	}
	slog.Info("data cleanup completed", "events_deleted", events, "runs_deleted", runs, "steps_deleted", steps)
	return events, runs, steps, errors.Join(errs...)
}

// recoverStuckWebhookEvents 周期回收卡在 processing 的 webhook 事件。
//
// 进程内的 goroutine panic(async.GoSafe 会 recover)会让事件永远停在 processing
// ⇒ 既不失败也不重试 ⇒ 这次推送的提交静默丢失。
//
// 阈值取 1 小时:一次大同步可能跑很久,不能误伤正在处理中的事件。
func (s *Service) recoverStuckWebhookEvents() {
	const interval = 10 * time.Minute
	const stuckTimeout = 1 * time.Hour
	loop.Run(s.bgCtx, loop.Config{
		Interval:   interval,
		FirstDelay: interval,
		Name:       "recover-stuck-webhook-events",
	}, func(ctx context.Context) {
		n, err := s.webhooks.RecoverStuckEvents(ctx, stuckTimeout)
		if err != nil {
			slog.Error("回收卡住的 webhook 事件失败", "error", err)
			return
		}
		if n > 0 {
			slog.Info("已回收卡住的 webhook 事件", "count", n)
		}
	}).Wait()
}

// cleanupOldHistory 周期清理过期的执行历史(webhook 事件 / 同步运行 / 运行步骤)。
//
// 这三张表此前**没有任何清理入口**(`CleanupOldData` 写好了却没人调)
// ⇒ 随运行次数无限增长(现网两个月已有 92 次运行 / 355 条步骤)。
//
// 保留期由 `sync.history_retention_days` 配置(0=不清理,默认)。
// 用 oekit concurrency/loop:自带 panic 隔离与 ctx 取消退出。
func (s *Service) cleanupOldHistory() {
	const interval = 6 * time.Hour
	loop.Run(s.bgCtx, loop.Config{
		Interval:   interval,
		FirstDelay: interval,
		Name:       "cleanup-old-history",
	}, func(ctx context.Context) {
		maxAge := s.historyRetention()
		if maxAge <= 0 {
			return
		}
		events, runs, steps, err := s.CleanupOldData(ctx, maxAge)
		if err != nil {
			slog.Error("清理过期执行历史失败", "error", err)
			return
		}
		if events+runs+steps > 0 {
			slog.Info("已清理过期执行历史",
				"events", events, "runs", runs, "steps", steps,
				"retention", maxAge)
		}
	}).Wait()
}

// cleanupTriggerTimes 周期清理过期的触发时间缓存。
// 用 oekit concurrency/loop:自带 panic 隔离(单轮失败只记日志,不中断循环)
// 与 ctx 取消退出(随 bgCancel 收敛),不再自维护 ticker + stop channel。
func (s *Service) cleanupTriggerTimes() {
	const interval = 10 * time.Minute
	loop.Run(s.bgCtx, loop.Config{
		Interval:   interval,
		FirstDelay: interval, // 原实现是「先等一个 ticker 再干活」,不立即首跑
		Name:       "cleanup-trigger-times",
	}, func(context.Context) {
		now := time.Now()
		s.lastTriggerTime.Range(func(key, value interface{}) bool {
			if t, ok := value.(time.Time); ok && now.Sub(t) > 1*time.Hour {
				s.lastTriggerTime.Delete(key)
			}
			return true
		})
	}).Wait()
}

// historyRetention 执行历史的保留期(0 = 不清理)。
//
// 由 `sync.history_retention_days` 配置决定(天)。
func (s *Service) historyRetention() time.Duration {
	cfg := s.GetConfig()
	if cfg == nil || cfg.Sync.HistoryRetentionDays <= 0 {
		return 0
	}
	return time.Duration(cfg.Sync.HistoryRetentionDays) * 24 * time.Hour
}
