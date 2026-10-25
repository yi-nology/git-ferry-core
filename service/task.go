package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/oekit/concurrency/async"
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
	// (创建路径:cron 是普通字符串,空串=不设定时)
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
	// (nil=不修改;空串=清空定时,不需要校验)
	if req.Cron != nil && *req.Cron != "" {
		if _, err := cron.ParseStandard(*req.Cron); err != nil {
			return nil, fmt.Errorf("invalid cron expression %q: %w", *req.Cron, err)
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

	// **并发互斥必须在这里拿**(而不是只在 RunTaskWithTrigger 里):
	//
	// cron / webhook 走 RunTaskWithTrigger ⇒ 有同 taskKey 互斥;
	// 而手动触发 / 重试 / 重建 / 自动补偿全走本函数 ⇒ 原先**完全没有互斥**,
	// 同一个任务可以被并发跑多份 ——
	// 而 workdir 是按 taskKey 共享的(service.GetTempDir),两个 executor
	// 同时 fetch/update-ref/push 同一目录 ⇒ ref 锁竞争、推送时序失控。
	//
	// 注释里写的"并发快速失败"以前从未发生(函数体里根本没有 guard 调用)。
	//
	// 锁在**受理时**抢(同步),不是执行时抢 —— 否则两个请求都会先返回"已受理",
	// 再排队跑同一份,那不叫互斥。
	release, err := s.guard.Acquire(s.bgCtx, taskKey)
	if err != nil {
		return err
	}
	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()
		defer release()
		run, err := s.executor.Execute(s.bgCtx, task, trigger, webhookEventID)
		if err != nil {
			// 结果已落 run 记录,这里仅留痕
			slog.Warn("async task run failed", "task", taskKey, "error", err)
		}
		s.emitRunCompleted(taskKey, trigger, run, err)
	})
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
		// 任务正在跑 ⇒ 这次 webhook 触发的提交**不能丢**:记一笔待补跑,
		// 等当前运行结束、释放执行权后补跑一次(合并:积压多次只补一次)。
		// 不记的话这次推送的提交要等下一次推送或 cron 才补上。
		if errors.Is(err, ErrTaskRunning) && trigger == model.TriggerWebhook {
			s.pendingWebhook.Mark(taskKey)
		}
		return err
	}

	run, execErr := func() (*model.SyncRun, error) {
		defer release()
		return s.executor.Execute(ctx, task, trigger, webhookEventID)
	}()
	s.emitRunCompleted(taskKey, trigger, run, execErr)
	// **释放执行权之后**再补跑:否则补跑会立刻又撞上同一个执行权。
	s.runPendingWebhook(taskKey)
	return execErr
}

// runPendingWebhook 补跑被合并掉的 webhook 触发。
//
// 合并语义:同一任务积压多次只补跑一次 —— 补跑是"再同步一次",
// 会把期间到达的所有提交一起带上。
func (s *Service) runPendingWebhook(taskKey string) {
	if !s.pendingWebhook.Take(taskKey) {
		return
	}
	ctx := s.bgCtx
	if ctx == nil {
		ctx = context.Background()
	}
	async.GoSafe(func() {
		if err := s.RunTaskWithTrigger(ctx, taskKey, model.TriggerWebhook, nil); err != nil {
			slog.Error("补跑被合并的 webhook 触发失败", "taskKey", taskKey, "error", err)
		}
	})
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
