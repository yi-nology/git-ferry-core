package service

import (
	"context"
	"errors"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
	"github.com/oekit/logx"
	"github.com/oekit/timertask/local"
	"github.com/yi-nology/git-ferry-core/model"
)

// cronReconcileInterval 是「DB 启用集 → 本地调度器」的对账周期:
// 其它实例上新建/改/停用的 cron 任务在本实例最多 30s 内生效(#40)。
const cronReconcileInterval = 30 * time.Second

// cronTask 构造一个 cron 触发闭包:多实例部署用分布式锁防重复触发,
// 只有一个实例能拿到锁;锁必须在本次触发结束后释放(续期 goroutine 会一直续着,
// 不释放 = 永久锁 ⇒ 任务静默停摆)。
func (s *Service) cronTask(taskKey string) local.Task {
	return func(ctx context.Context, _ time.Time) error {
		release, locked, err := s.guard.TryCronLock(ctx, taskKey, cronLockTTL)
		if err != nil {
			logx.Error("cron lock check failed", "taskKey", taskKey, "error", err)
			return nil
		}
		if !locked {
			logx.Debug("cron task skipped (another instance holds lock)", "taskKey", taskKey)
			return nil
		}
		defer release()
		if err := s.RunTaskWithTrigger(ctx, taskKey, model.TriggerCron, nil); err != nil {
			logx.Error("cron task failed", "taskKey", taskKey, "error", err)
		}
		return nil
	}
}

// addCronJob 按业务键注册/更新一条 cron(同 key 同 spec 幂等)。
func (s *Service) addCronJob(task *model.SyncTask) error {
	if s.cron == nil {
		return nil
	}
	if err := s.cron.ScheduleFor(task.Key, task.Cron, s.cronTask(task.Key)); err != nil {
		return err
	}
	s.cronMu.Lock()
	s.cronKeys[task.Key] = task.Cron
	s.cronMu.Unlock()
	return nil
}

func (s *Service) removeCronJob(taskKey string) {
	if s.cron == nil {
		return
	}
	s.cron.UnscheduleFor(taskKey)
	s.cronMu.Lock()
	delete(s.cronKeys, taskKey)
	s.cronMu.Unlock()
}

func (s *Service) startCronJobs() error {
	if s.cron == nil {
		return nil
	}
	tasks, err := s.tasks.FindAllEnabledTasks()
	if err != nil {
		return err
	}

	for _, task := range tasks {
		if task.Cron != "" {
			if err := s.addCronJob(task); err != nil {
				logx.Error("add cron job failed", "taskKey", task.Key, "error", err)
			}
		}
	}

	ctx := s.bgCtx
	if ctx == nil {
		ctx = context.Background()
	}
	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()
		if err := s.cron.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logx.Error("cron scheduler stopped", "error", err)
		}
	})
	return nil
}

func (s *Service) stopCronJobs() {
	if s.cron == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.cron.Stop(ctx); err != nil {
		logx.Warn("cron scheduler stop failed (running tasks continue until ctx timeout)", "error", err)
	}
}

// cronReconcileLoop 周期把 DB 里的启用任务集收敛到本地调度器。
// Reconcile 按业务键做增/改/删,same key same spec 为幂等空操作。
func (s *Service) cronReconcileLoop() {
	if s.cron == nil || s.bgCtx == nil {
		return
	}
	loop.Run(s.bgCtx, loop.Config{
		Interval:   cronReconcileInterval,
		FirstDelay: cronReconcileInterval,
		Name:       "cron-reconcile",
	}, func(context.Context) {
		s.reconcileCronEntries()
	}).Wait()
}

func (s *Service) reconcileCronEntries() {
	tasks, err := s.tasks.FindAllEnabledTasks()
	if err != nil {
		logx.Warn("cron reconcile: load tasks failed", "error", err)
		return
	}
	entries := make([]local.Entry, 0, len(tasks))
	keys := make(map[string]string, len(tasks))
	for _, task := range tasks {
		if task.Cron == "" {
			continue
		}
		entries = append(entries, local.Entry{
			Key:  task.Key,
			Spec: task.Cron,
			Task: s.cronTask(task.Key),
		})
		keys[task.Key] = task.Cron
	}
	if err := s.cron.Reconcile(entries); err != nil {
		logx.Error("cron reconcile failed", "error", err)
		return
	}
	s.cronMu.Lock()
	s.cronKeys = keys
	s.cronMu.Unlock()
}
