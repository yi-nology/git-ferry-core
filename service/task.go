package service

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/oekit/concurrency/async"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/robfig/cron/v3"
	"github.com/yi-nology/git-ferry-core/model"
)

// ListTasks returns a paginated list of sync tasks.
func (s *Service) ListTasks(ctx context.Context, repoKey string, offset, limit int) ([]*model.SyncTask, int64, error) {
	return s.tasks.ListTasks(ctx, repoKey, offset, limit)
}

// ListTasksFiltered 按仓库与最近一次执行状态过滤任务(status 空串=不过滤)。
//
// 存在的理由:CLI 的 `task +list --status` 一直在发这个过滤条件,
// 但服务端没有对应能力 ⇒ hertz 静默忽略未知 query 参数 ⇒
// 用户拿到全量却以为筛过了(契约漂移表现成"过滤没用")。
func (s *Service) ListTasksFiltered(ctx context.Context, repoKey, status string, offset, limit int) ([]*model.SyncTask, int64, error) {
	return s.tasks.ListTasksFiltered(ctx, repoKey, status, offset, limit)
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
			return nil, oekiterrors.Errorf("invalid cron expression %q: %w", req.Cron, err)
		}
	}

	task, err := s.tasks.CreateTask(ctx, req)
	if err != nil {
		return nil, err
	}

	if task.Cron != "" {
		if err := s.addCronJob(task); err != nil {
			return nil, oekiterrors.Errorf("create cron job failed: %w", err)
		}
	}

	return task, nil
}

func (s *Service) UpdateTask(ctx context.Context, req *model.UpdateTaskRequest) (*model.SyncTask, error) {
	// 预校验 cron 表达式,避免写入 DB 后才发现无效
	// (nil=不修改;空串=清空定时,不需要校验)
	if req.Cron != nil && *req.Cron != "" {
		if _, err := cron.ParseStandard(*req.Cron); err != nil {
			return nil, oekiterrors.Errorf("invalid cron expression %q: %w", *req.Cron, err)
		}
	}

	task, err := s.tasks.UpdateTask(ctx, req)
	if err != nil {
		return nil, err
	}

	if task.Cron != "" && task.Enabled {
		if err := s.addCronJob(task); err != nil {
			return nil, oekiterrors.Errorf("update cron job failed: %w", err)
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

	// P2 持久化队列:开启后触发改为入队(同任务去重),由派发器认领执行。
	if s.durableQueueOn() {
		return s.enqueueTaskRun(taskKey, trigger, webhookEventID)
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
		// rebuild 语义:拿到执行权之后再清工作目录(见 service/rebuild.go)。
		if err := s.runWithRebuildPrep(taskKey, trigger); err != nil {
			release()
			slog.Error("rebuild: 清空工作目录失败,本次执行中止", "task", taskKey, "error", err)
			return
		}
		run, err := s.executor.Execute(s.bgCtx, task, trigger, webhookEventID)
		if err != nil {
			// 结果已落 run 记录,这里仅留痕
			slog.Warn("async task run failed", "task", taskKey, "error", err)
		}
		s.finishRun(taskKey, trigger, run, err, release)
	})
	return nil
}

// finishRun 执行收尾:先释放执行权,再广播完成事件,最后消费待补跑触发。
//
// **顺序不能反**:此前 emit 在 release 之前 —— 订阅方(runwatch)收到失败
// 完成事件后立即触发自动重跑,而执行权还没放 ⇒ 重跑撞上 ErrTaskRunning
// 被拒绝,且 RetryTracker 已记账 ⇒ 预算白白烧掉、重试未发生(P0)。
// 先放行执行权,补偿/重跑才能立刻接上。
func (s *Service) finishRun(taskKey, trigger string, run *model.SyncRun, err error, release func()) {
	release()
	delivered := s.emitRunCompleted(taskKey, trigger, run, err)
	// 快速路径**真的投递了**才闭环 outbox 行(派发器宽限后不会重投)。
	// 无订阅者时不得标记 —— 否则事件被标记已发布却从未投递,
	// at-least-once 退化成 at-most-once;留在 outbox 由派发器重投。
	// 崩溃在标记之前 ⇒ 派发器/启动扫描按 at-least-once 重投,订阅方幂等吸收。
	if delivered && run != nil {
		s.markOutboxPublishedByRun(run.ID)
	}
	// RunTaskAsync 路径(手动/重试/auto_retry)在这里消费待补跑 —— 旧实现
	// 只有 RunTaskWithTrigger 消费,手动执行期间积压的 webhook 永远补不上(P0)。
	s.runPendingWebhook(taskKey)
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

	// P2 持久化队列:开启后触发改为入队(同任务去重),由派发器认领执行。
	if s.durableQueueOn() {
		return s.enqueueTaskRun(taskKey, trigger, webhookEventID)
	}

	// 并发控制(配 redis 时为分布式,多实例安全):全局并发上限 + 同 taskKey 互斥。
	// 非阻塞快速失败:全局满返回 ErrTooManyConcurrent,taskKey 已在跑返回 ErrTaskRunning。
	release, err := s.guard.Acquire(ctx, taskKey)
	if err != nil {
		// 任务正在跑 ⇒ 这次 webhook 触发的提交**不能丢**:记一笔待补跑(落库,
		// 双路径/跨实例/重启都可消费),等当前运行结束、释放执行权后补跑一次
		// (合并:积压多次只补一次)。不记的话这次推送的提交要等下一次推送或 cron 才补上。
		if errors.Is(err, ErrTaskRunning) && trigger == model.TriggerWebhook {
			s.markPendingWebhook(taskKey)
		}
		return err
	}

	run, execErr := func() (*model.SyncRun, error) {
		defer release()
		// rebuild 语义:拿到执行权之后再清工作目录(见 service/rebuild.go)。
		if err := s.runWithRebuildPrep(taskKey, trigger); err != nil {
			return nil, err
		}
		return s.executor.Execute(ctx, task, trigger, webhookEventID)
	}()
	s.emitRunCompleted(taskKey, trigger, run, execErr)
	if run != nil {
		// 快速路径已投递 ⇒ 闭环 outbox 行(与 finishRun 同语义)。
		s.markOutboxPublishedByRun(run.ID)
	}
	// **释放执行权之后**再补跑:否则补跑会立刻又撞上同一个执行权。
	s.runPendingWebhook(taskKey)
	return execErr
}

// PreviewSync previews a sync operation.
func (s *Service) PreviewSync(ctx context.Context, req *model.PreviewSyncRequest) (*model.PreviewSyncResult, error) {
	return s.tasks.PreviewSync(ctx, req)
}

// ListHistory returns a paginated list of sync run history.
func (s *Service) ListHistory(ctx context.Context, taskKey string, offset, limit int) ([]*model.SyncRun, int64, error) {
	return s.tasks.ListHistory(ctx, taskKey, offset, limit)
}

// GetSyncRunByID 按执行记录 ID 精确取某一条(带步骤)。
//
// 命名说明:不叫 GetHistoryRun —— 那个名字已经被**历史同步通道**的
// GetHistoryRun(ctx, id) 占了(两套完全不同的东西)。
func (s *Service) GetSyncRunByID(ctx context.Context, taskKey string, runID uint) (*model.SyncRun, error) {
	return s.tasks.GetHistoryRun(ctx, taskKey, runID)
}

// DeleteHistory deletes a sync run by ID.
func (s *Service) DeleteHistory(ctx context.Context, id uint) error {
	return s.tasks.DeleteHistory(ctx, id)
}
