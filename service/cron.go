package service

import (
	"context"
	"log/slog"

	"github.com/yi-nology/git-ferry-core/model"
)

func (s *Service) addCronJob(task *model.SyncTask) error {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()

	if entryID, ok := s.cronEntryIDs[task.Key]; ok {
		s.cron.Remove(entryID)
	}

	entryID, err := s.cron.AddFunc(task.Cron, func() {
		ctx := context.Background()
		// 多实例部署时用分布式锁防止重复触发:只有一个实例能拿到锁
		release, locked, err := s.guard.TryCronLock(ctx, task.Key, cronLockTTL)
		if err != nil {
			slog.Error("cron lock check failed", "taskKey", task.Key, "error", err)
			return
		}
		if !locked {
			slog.Debug("cron task skipped (another instance holds lock)", "taskKey", task.Key)
			return
		}
		// **必须释放**:cron 锁会被续期 goroutine 一直续着,不释放 = 永久锁,
		// 之后每次 tick 都拿不到 ⇒ 任务静默停摆(实测踩过)。
		defer release()
		if err := s.RunTaskWithTrigger(ctx, task.Key, model.TriggerCron, nil); err != nil {
			slog.Error("cron task failed", "taskKey", task.Key, "error", err)
		}
	})
	if err != nil {
		return err
	}

	s.cronEntryIDs[task.Key] = entryID
	return nil
}

func (s *Service) removeCronJob(taskKey string) {
	s.cronMu.Lock()
	defer s.cronMu.Unlock()

	if entryID, ok := s.cronEntryIDs[taskKey]; ok {
		s.cron.Remove(entryID)
		delete(s.cronEntryIDs, taskKey)
	}
}

func (s *Service) startCronJobs() error {
	tasks, err := s.tasks.FindAllEnabledTasks()
	if err != nil {
		return err
	}

	for _, task := range tasks {
		if task.Cron != "" {
			if err := s.addCronJob(task); err != nil {
				slog.Error("add cron job failed", "taskKey", task.Key, "error", err)
			}
		}
	}

	s.cron.Start()
	return nil
}

func (s *Service) stopCronJobs() {
	s.cron.Stop()
}
