package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/robfig/cron/v3"
	"github.com/yi-nology/git-ferry-core/model"
)

// ListTasks returns a paginated list of sync tasks.
func (s *Service) ListTasks(ctx context.Context, repoKey string, offset, limit int) ([]*model.SyncTask, int64, error) {
	return s.tasks.ListTasks(ctx, repoKey, offset, limit)
}

// GetTask returns a sync task by key.
func (s *Service) GetTask(ctx context.Context, key string) (*model.SyncTask, error) {
	return s.tasks.GetTask(ctx, key)
}

func (s *Service) CreateTask(ctx context.Context, req *model.CreateTaskRequest) (*model.SyncTask, error) {
	// 预校验 cron 表达式,避免写入 DB 后才发现无效
	if req.Cron != "" {
		if _, err := cron.ParseStandard(req.Cron); err != nil {
			return nil, fmt.Errorf("invalid cron expression %q: %w", req.Cron, err)
		}
	}

	task, err := s.tasks.CreateTask(ctx, req)
	if err != nil {
		return nil, err
	}

	if task.Cron != "" {
		if err := s.addCronJob(task); err != nil {
			return nil, fmt.Errorf("create cron job failed: %w", err)
		}
	}

	return task, nil
}

func (s *Service) UpdateTask(ctx context.Context, req *model.UpdateTaskRequest) (*model.SyncTask, error) {
	// 预校验 cron 表达式,避免写入 DB 后才发现无效
	if req.Cron != "" {
		if _, err := cron.ParseStandard(req.Cron); err != nil {
			return nil, fmt.Errorf("invalid cron expression %q: %w", req.Cron, err)
		}
	}

	task, err := s.tasks.UpdateTask(ctx, req)
	if err != nil {
		return nil, err
	}

	if task.Cron != "" && task.Enabled {
		if err := s.addCronJob(task); err != nil {
			return nil, fmt.Errorf("update cron job failed: %w", err)
		}
	} else {
		s.removeCronJob(task.Key)
	}

	return task, nil
}

func (s *Service) DeleteTask(ctx context.Context, key string) error {
	s.removeCronJob(key)
	if err := s.tasks.DeleteTask(ctx, key); err != nil {
		return err
	}
	// 清理该任务残留的 workdir(成功执行后保留的增量 fetch 工作区),避免磁盘泄漏
	if err := os.RemoveAll(s.GetTempDir(key)); err != nil {
		slog.Error("failed to cleanup workdir on task delete", "error", err, "taskKey", key)
	}
	return nil
}

func (s *Service) RunTask(ctx context.Context, taskKey string) error {
	return s.RunTaskWithTrigger(ctx, taskKey, model.TriggerManual, nil)
}

// RunTaskAsync 异步触发一次同步:校验与并发快速失败在调用方 goroutine 内完成,
// 实际执行纳入 WaitGroup + bgCtx(优雅关停时被等待/取消),适合 HTTP 手动触发路径,
// 避免 handler 被分钟级同步任务挂住。返回 nil 仅代表"已受理",结果见执行历史。
func (s *Service) RunTaskAsync(taskKey, trigger string, webhookEventID *uint) error {
	task, err := s.tasks.FindTaskByKey(taskKey)
	if err != nil {
		return err
	}
	if task == nil {
		return ErrTaskNotFound
	}
	if !task.Enabled {
		return ErrTaskDisabled
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		run, err := s.executor.Execute(s.bgCtx, task, trigger, webhookEventID)
		if err != nil {
			// 结果已落 run 记录,这里仅留痕
			slog.Warn("async task run failed", "task", taskKey, "error", err)
		}
		s.emitRunCompleted(taskKey, trigger, run, err)
	}()
	return nil
}

// CountTasksByStatus returns task counts grouped by last_status (key "total" = overall).
func (s *Service) CountTasksByStatus() (map[string]int64, error) {
	return s.tasks.CountTasksByStatus()
}

func (s *Service) RunTaskWithTrigger(ctx context.Context, taskKey, trigger string, webhookEventID *uint) error {
	task, err := s.tasks.FindTaskByKey(taskKey)
	if err != nil {
		return err
	}
	if task == nil {
		return ErrTaskNotFound
	}

	// 已禁用的任务不执行(cron/webhook/手动均拦截),需要运行请先启用
	if !task.Enabled {
		return ErrTaskDisabled
	}

	// 并发控制(配 redis 时为分布式,多实例安全):全局并发上限 + 同 taskKey 互斥。
	// 非阻塞快速失败:全局满返回 ErrTooManyConcurrent,taskKey 已在跑返回 ErrTaskRunning。
	release, err := s.guard.Acquire(ctx, taskKey)
	if err != nil {
		return err
	}
	defer release()

	run, err := s.executor.Execute(ctx, task, trigger, webhookEventID)
	s.emitRunCompleted(taskKey, trigger, run, err)
	return err
}

// PreviewSync previews a sync operation.
func (s *Service) PreviewSync(ctx context.Context, req *model.PreviewSyncRequest) (*model.PreviewSyncResult, error) {
	return s.tasks.PreviewSync(ctx, req)
}

// ListHistory returns a paginated list of sync run history.
func (s *Service) ListHistory(ctx context.Context, taskKey string, offset, limit int) ([]*model.SyncRun, int64, error) {
	return s.tasks.ListHistory(ctx, taskKey, offset, limit)
}

// DeleteHistory deletes a sync run by ID.
func (s *Service) DeleteHistory(ctx context.Context, id uint) error {
	return s.tasks.DeleteHistory(ctx, id)
}
