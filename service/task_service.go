package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/stringx"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
	"gorm.io/gorm"
)

// validBranchName 匹配合法的分支名/分支规格:字母数字._/-,不含冒号/双点/波浪号等危险字符。
// 额外允许 glob 元字符 * ? [ ]：executor 以 strings.ContainsAny(spec, "*?[") 识别
// 多分支规格（org 导入/镜像编排用 "*" 表示同步全部分支），校验必须放行同一套字符。
var validBranchName = regexp.MustCompile(`^[a-zA-Z0-9._/\-*?\[\]]+$`)

func validateBranchName(branch string) error {
	if branch == "" {
		return nil // 空分支名由调用方处理(通常表示使用默认分支)
	}
	if !validBranchName.MatchString(branch) {
		return errors.New(fmt.Sprintf("invalid branch name %q: only alphanumeric, '.', '_', '/', '-' are allowed", branch))
	}
	if branch == "." || branch == ".." {
		return errors.New(fmt.Sprintf("invalid branch name %q", branch))
	}
	return nil
}

// TaskService handles sync task-related operations.
type TaskService struct {
	taskRepository    *repository.SyncTaskRepository
	runRepository     *repository.SyncRunRepository
	runStepRepository *repository.SyncRunStepRepository
	repoRepository    *repository.RepoRepository
	// outbox 可选:接好后,运行完成会**同事务**写 outbox 事件
	// (transactional outbox,见 model.OutboxEvent);nil = 不写(裸构造的测试路径)。
	outbox *repository.OutboxEventRepository
}

// NewTaskService creates a new TaskService instance.
func NewTaskService(taskRepository *repository.SyncTaskRepository, runRepository *repository.SyncRunRepository, runStepRepository *repository.SyncRunStepRepository, repoRepository *repository.RepoRepository) *TaskService {
	return &TaskService{
		taskRepository:    taskRepository,
		runRepository:     runRepository,
		runStepRepository: runStepRepository,
		repoRepository:    repoRepository,
	}
}

// ListTasks returns a paginated list of sync tasks.
func (ts *TaskService) ListTasks(ctx context.Context, repoKey string, offset, limit int) ([]*model.SyncTask, int64, error) {
	page := repository.DefaultPagination(offset, limit)
	if repoKey != "" {
		return ts.taskRepository.FindByRepoKey(ctx, repoKey, page)
	}
	return ts.taskRepository.FindAll(ctx, page)
}

// ListTasksFiltered 按仓库与最近一次执行状态过滤任务。
//
// status 为空串 = 不按状态过滤(与 ListTasks 等价)。
func (ts *TaskService) ListTasksFiltered(ctx context.Context, repoKey, status string, offset, limit int) ([]*model.SyncTask, int64, error) {
	if repoKey == "" && status == "" {
		return ts.ListTasks(ctx, repoKey, offset, limit)
	}
	page := repository.DefaultPagination(offset, limit)
	return ts.taskRepository.FindFiltered(ctx, repoKey, status, page)
}

// CountTasksByStatus returns task counts grouped by last_status (key "total" = overall).
func (ts *TaskService) CountTasksByStatus() (map[string]int64, error) {
	return ts.taskRepository.CountByStatus()
}

// GetTask returns a sync task by key.
func (ts *TaskService) GetTask(ctx context.Context, key string) (*model.SyncTask, error) {
	return ts.findTask(ctx, key)
}

// findTask 查任务并归一化查无此条。
func (ts *TaskService) findTask(ctx context.Context, key string) (*model.SyncTask, error) {
	t, err := ts.taskRepository.FindByKey(ctx, key)
	if err != nil {
		return nil, mapNotFound(err, ErrTaskNotFound)
	}
	return t, nil
}

// CreateTask creates a new sync task.
func (ts *TaskService) CreateTask(ctx context.Context, req *model.CreateTaskRequest) (*model.SyncTask, error) {
	if err := validateBranchName(req.SourceBranch); err != nil {
		return nil, err
	}
	if err := validateBranchName(req.TargetBranch); err != nil {
		return nil, err
	}
	// Key:回放路径显式指定(策略文件身份),普通创建生成 UUID。
	key := req.Key
	if key == "" {
		key = stringx.UUIDv4()
	} else if !validTaskKey(key) {
		return nil, errors.New(fmt.Sprintf("invalid task key %q: only [a-zA-Z0-9-]{1,64}", key))
	}
	task := &model.SyncTask{
		Key:                key,
		Name:               req.Name,
		SourceRepoKey:      req.SourceRepoKey,
		SourceBranch:       req.SourceBranch,
		TargetRepoKey:      req.TargetRepoKey,
		TargetBranch:       req.TargetBranch,
		SyncMode:           req.SyncMode,
		Cron:               req.Cron,
		WebhookToken:       stringx.UUIDv4(),
		Enabled:            true,
		GitTags:            req.GitTags,
		GitForce:           req.GitForce,
		GitLFS:             req.GitLFS,
		SyncWiki:           req.SyncWiki,
		GitBundle:          req.GitBundle,
		Submodules:         req.Submodules,
		GitPushPrune:       req.GitPushPrune,
		ForcePushPolicy:    req.ForcePushPolicy,
		IncludeBranches:    req.IncludeBranches,
		ExcludeRefPatterns: req.ExcludeRefPatterns,
	}

	if err := ts.taskRepository.Create(task); err != nil {
		return nil, err
	}

	return task, nil
}

// UpdateTask updates an existing sync task.
func (ts *TaskService) UpdateTask(ctx context.Context, req *model.UpdateTaskRequest) (*model.SyncTask, error) {
	task, err := ts.findTask(ctx, req.Key)
	if err != nil {
		return nil, err
	}

	if req.Name != "" {
		task.Name = req.Name
	}
	if req.SourceBranch != "" {
		if err := validateBranchName(req.SourceBranch); err != nil {
			return nil, err
		}
		task.SourceBranch = req.SourceBranch
	}
	if req.TargetBranch != "" {
		if err := validateBranchName(req.TargetBranch); err != nil {
			return nil, err
		}
		task.TargetBranch = req.TargetBranch
	}
	if req.SyncMode != "" {
		task.SyncMode = req.SyncMode
	}
	// nil=不修改;空串=清空定时
	if req.Cron != nil {
		task.Cron = *req.Cron
	}
	// bool 字段:仅当请求显式携带时才更新(nil = 不修改)
	if req.Enabled != nil {
		task.Enabled = *req.Enabled
	}
	if req.GitTags != nil {
		task.GitTags = *req.GitTags
	}
	if req.GitForce != nil {
		task.GitForce = *req.GitForce
	}
	if req.GitLFS != nil {
		task.GitLFS = *req.GitLFS
	}
	if req.SyncWiki != nil {
		task.SyncWiki = *req.SyncWiki
	}
	if req.GitBundle != nil {
		task.GitBundle = *req.GitBundle
	}
	if req.Submodules != nil {
		task.Submodules = *req.Submodules
	}
	if req.GitPushPrune != nil {
		task.GitPushPrune = *req.GitPushPrune
	}
	if req.ForcePushPolicy != nil {
		task.ForcePushPolicy = *req.ForcePushPolicy
	}
	if req.IncludeBranches != nil {
		task.IncludeBranches = *req.IncludeBranches
	}
	if req.ExcludeRefPatterns != nil {
		task.ExcludeRefPatterns = *req.ExcludeRefPatterns
	}

	if err := ts.taskRepository.Update(task); err != nil {
		return nil, err
	}

	return task, nil
}

// DeleteTask deletes a sync task by key.
func (ts *TaskService) DeleteTask(ctx context.Context, key string) error {
	return ts.taskRepository.Delete(key)
}

// PreviewSync previews a sync operation.
func (ts *TaskService) PreviewSync(ctx context.Context, req *model.PreviewSyncRequest) (*model.PreviewSyncResult, error) {
	sourceRepo, err := ts.repoRepository.FindByKey(ctx, req.SourceRepoKey)
	if err != nil && !isRepositoryNotFound(err) {
		return nil, err
	}
	targetRepo, err := ts.repoRepository.FindByKey(ctx, req.TargetRepoKey)
	if err != nil && !isRepositoryNotFound(err) {
		return nil, err
	}

	result := &model.PreviewSyncResult{
		SourceExists: sourceRepo != nil,
		TargetExists: targetRepo != nil,
	}

	if sourceRepo == nil || targetRepo == nil {
		result.CanSync = false
		result.Message = "source or target repo not found"
		return result, nil
	}

	result.CanSync = true
	result.Message = "sync preview ready"
	return result, nil
}

// ListHistory returns a paginated list of sync run history.
func (ts *TaskService) ListHistory(ctx context.Context, taskKey string, offset, limit int) ([]*model.SyncRun, int64, error) {
	page := repository.DefaultPagination(offset, limit)
	return ts.runRepository.FindByTaskKey(ctx, taskKey, page)
}

// ListRecentRuns 最近一批已结束的 run(轻量列,跨任务)。
// 壳层 runwatch 轮询兜底用:替代「每个任务各查一次历史」的 N+1。
func (ts *TaskService) ListRecentRuns(ctx context.Context, limit int) ([]*model.SyncRun, error) {
	return ts.runRepository.FindRecentTerminalLite(ctx, limit)
}

// GetHistoryRun 按执行记录 ID 精确取某一条(带步骤)。
//
// taskKey 为空 = 不按任务限定(ID 全局唯一)。
//
// 之前只有 ListHistory(按任务翻页)⇒ CLI 的 `history +detail --run-id`
// 只能"翻页找那条",找不到就静默返回空列表。
func (ts *TaskService) GetHistoryRun(ctx context.Context, taskKey string, runID uint) (*model.SyncRun, error) {
	return ts.runRepository.FindByTaskKeyAndRunID(ctx, taskKey, runID)
}

// DeleteHistory deletes a sync run by ID.
func (ts *TaskService) DeleteHistory(ctx context.Context, id uint) error {
	return ts.runRepository.Delete(id)
}

// FindAllEnabledTasks returns all enabled sync tasks.
func (ts *TaskService) FindAllEnabledTasks() ([]*model.SyncTask, error) {
	return ts.taskRepository.FindAllEnabled()
}

// FindTaskByKey returns a sync task by key (internal use).
func (ts *TaskService) FindTaskByKey(key string) (*model.SyncTask, error) {
	return ts.findTask(context.Background(), key)
}

// CleanupOldRuns removes sync runs older than the specified duration.
func (ts *TaskService) CleanupOldRuns(ctx context.Context, maxAge time.Duration) (int64, error) {
	return ts.runRepository.CleanupOlderThan(ctx, maxAge)
}

// CleanupOldRunSteps removes sync run steps older than the specified duration.
func (ts *TaskService) CleanupOldRunSteps(ctx context.Context, maxAge time.Duration) (int64, error) {
	return ts.runStepRepository.CleanupOlderThan(ctx, maxAge)
}

// CreateRun creates a new sync run record for a task.
func (ts *TaskService) CreateRun(task *model.SyncTask, trigger string, webhookEventID *uint) (*model.SyncRun, error) {
	run := &model.SyncRun{
		TaskKey:        task.Key,
		TriggerSource:  trigger,
		Status:         model.StatusRunning,
		StartTime:      time.Now(),
		WebhookEventID: webhookEventID,
	}
	if err := ts.runRepository.Create(run); err != nil {
		return nil, err
	}
	return run, nil
}

// CreateRunStep creates a new sync run step record.
func (ts *TaskService) CreateRunStep(step *model.SyncRunStep) error {
	return ts.runStepRepository.Create(step)
}

// UpdateRunStep updates an existing sync run step record.
func (ts *TaskService) UpdateRunStep(step *model.SyncRunStep) error {
	return ts.runStepRepository.Update(step)
}

// CompleteRun updates a sync run with final status and details.
// runCompletedOutbox 由完成的 run 构造「运行完成」outbox 事件。
// 冗余 trigger/status 供重投过滤;payload 带 error_message 便于排查。
func runCompletedOutbox(run *model.SyncRun) *model.OutboxEvent {
	payload, _ := json.Marshal(map[string]any{"error_message": run.ErrorMessage})
	return &model.OutboxEvent{
		Topic:   model.OutboxTopicRunCompleted,
		TaskKey: run.TaskKey,
		RunID:   run.ID,
		Trigger: run.TriggerSource,
		Status:  run.Status,
		Payload: string(payload),
	}
}

func (ts *TaskService) CompleteRun(run *model.SyncRun) error {
	if ts.outbox == nil {
		return ts.runRepository.Update(run)
	}
	// run 更新 + outbox 事件同一事务:要么都成立,要么都回滚 ——
	// 否则「run 已完成但事件丢了」的崩溃窗口原样存在(outbox 就白做了)。
	return ts.runRepository.DB().Transaction(func(tx *gorm.DB) error {
		if err := ts.runRepository.UpdateInTx(tx, run); err != nil {
			return err
		}
		return ts.outbox.CreateInTx(context.Background(), tx, runCompletedOutbox(run))
	})
}

// UpdateTaskLastRun updates the task's last run time and status.
//
// 只写这两列(不是整行 Save):执行期间的并发修改(策略/cron/开关)不能被
// 执行开始时的旧快照悄悄改回去。
func (ts *TaskService) UpdateTaskLastRun(task *model.SyncTask, run *model.SyncRun) error {
	return ts.taskRepository.UpdateLastRun(task.Key, run.EndTime, run.Status)
}

// CompleteRunWithTaskUpdate 在同一事务中完成 run 更新 + task 状态更新 + outbox 事件,
// 保证原子性。
//
// task 状态**只更新 last_run_at / last_status 两列**:整行 Save 会把
// 执行开始时的旧快照写回,抹掉执行期间的并发修改(实测:一次 cron 执行
// 把刚改的 force_push_policy=allow 还原成 block)。
func (ts *TaskService) CompleteRunWithTaskUpdate(run *model.SyncRun, task *model.SyncTask) error {
	return ts.runRepository.DB().Transaction(func(tx *gorm.DB) error {
		if err := ts.runRepository.UpdateInTx(tx, run); err != nil {
			return err
		}
		if err := tx.Model(&model.SyncTask{}).
			Where("`key` = ?", task.Key).
			Updates(map[string]any{
				"last_run_at": run.EndTime,
				"last_status": run.Status,
			}).Error; err != nil {
			return err
		}
		// outbox 与完成状态同事务落库:派发器/重启扫描保证通知至少送达一次。
		if ts.outbox != nil {
			return ts.outbox.CreateInTx(context.Background(), tx, runCompletedOutbox(run))
		}
		return nil
	})
}

// validTaskKey 显式 key 的合法性(回放路径):[a-zA-Z0-9-]{1,64}。
// 普通创建不受影响(key 为空走 UUID)。
func validTaskKey(key string) bool {
	if len(key) == 0 || len(key) > 64 {
		return false
	}
	for _, c := range key {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
