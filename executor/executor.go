package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// RunManager handles sync run lifecycle operations.
type RunManager interface {
	CreateRun(task *model.SyncTask, trigger string, webhookEventID *uint) (*model.SyncRun, error)
	CreateRunStep(step *model.SyncRunStep) error
	UpdateRunStep(step *model.SyncRunStep) error
	CompleteRun(run *model.SyncRun) error
	UpdateTaskLastRun(task *model.SyncTask, run *model.SyncRun) error
	CompleteRunWithTaskUpdate(run *model.SyncRun, task *model.SyncTask) error
}

// RepoProvider provides repository lookup by key.
type RepoProvider interface {
	GetRepoByKey(key string) (*model.Repo, error)
}

// PlatformProvider provides platform lookup by ID.
type PlatformProvider interface {
	GetPlatformByID(ctx context.Context, id uint) (*model.Platform, error)
}

// Service is the interface the executor depends on.
// It exposes only the operations the executor needs — no DAO leakage.
type Service interface {
	GetTempDir(taskKey string) string
	GetConfig() *model.Config
	RunManager
	RepoProvider
	PlatformProvider
}

type Executor struct {
	service Service
	backend gitbackend.GitBackend
	// Approver 可选：force_push_policy=block 时查询是否已获人工放行。
	// 由壳层注入（如 force-push-approvals 审批表）；nil=始终未放行。
	Approver ForcePushApprover
}

// ForcePushApprover 查询某任务/分支的强制推送是否已获放行。
type ForcePushApprover interface {
	IsForcePushApproved(taskKey, branch string) bool
	// RequestApproval 产生一条 pending 审批（best-effort，失败不影响主流程）。
	RequestApproval(taskKey, branch, reason string)
}

func NewExecutor(svc Service) (*Executor, error) {
	// git.backend 配置接线:空值由 SDK 自动选择(native 优先,回退 gogit)
	backendType := ""
	if cfg := svc.GetConfig(); cfg != nil {
		backendType = cfg.Git.Backend
	}
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{Type: backendType})
	if err != nil {
		return nil, errors.Wrap(err, "init git backend failed")
	}
	return &Executor{
		service: svc,
		backend: backend,
	}, nil
}

func (e *Executor) Execute(ctx context.Context, task *model.SyncTask, trigger string, webhookEventID *uint) (*model.SyncRun, error) {
	run, err := e.service.CreateRun(task, trigger, webhookEventID)
	if err != nil {
		return nil, err
	}
	startTime := time.Now()

	var details strings.Builder
	const maxDetailsSize = 64 * 1024
	fmt.Fprintf(&details, "=== Sync Task: %s ===\n", task.Name)
	fmt.Fprintf(&details, "Trigger: %s\n", trigger)
	fmt.Fprintf(&details, "Time: %s\n", startTime.Format(time.RFC3339))

	defer func() {
		run.EndTime = timePtr(time.Now())
		run.DurationMs = run.EndTime.Sub(startTime).Milliseconds()
		detailStr := details.String()
		if len(detailStr) > maxDetailsSize {
			detailStr = detailStr[:maxDetailsSize] + "\n... (truncated)"
		}
		run.Details = detailStr
		e.runPostExec(task, run, &details)
		// post-exec 可能追加 details，重新截断
		run.Details = details.String()
		if len(run.Details) > maxDetailsSize {
			run.Details = run.Details[:maxDetailsSize] + "\n... (truncated)"
		}
		if err := e.service.CompleteRunWithTaskUpdate(run, task); err != nil {
			slog.Error("failed to complete sync run and update task", "error", err)
		}
	}()

	// 解析仓库/平台并组装运行态(阶段只读 RunContext,不各自查库)
	rc, err := e.prepareRunContext(ctx, task, run, &details)
	if err != nil {
		return failRun(run, err)
	}

	workDir := rc.WorkDir
	defer func() {
		if run.Status != model.StatusSuccess {
			if rmErr := os.RemoveAll(workDir); rmErr != nil {
				slog.Error("failed to cleanup temp dir", "error", rmErr, "dir", workDir)
			}
		}
	}()

	timeout := e.service.GetConfig().Sync.DefaultTimeout
	if timeout <= 0 {
		timeout = 300
	}
	execCtx, execCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer execCancel()

	if err := defaultPipeline().Run(execCtx, rc); err != nil {
		return failRun(run, err)
	}

	run.Status = model.StatusSuccess
	details.WriteString("\n=== Sync completed successfully ===")
	return run, nil
}

// runPostExec 执行 sync.post_exec_script（ghorg post_exec_script 模式）。
// 失败只记 details，不影响同步结果。
func (e *Executor) runPostExec(task *model.SyncTask, run *model.SyncRun, details *strings.Builder) {
	cfg := e.service.GetConfig()
	if cfg == nil || cfg.Sync.PostExecScript == "" {
		return
	}
	result := "failed"
	if run.Status == model.StatusSuccess {
		result = "success"
	}
	cmd := exec.Command(cfg.Sync.PostExecScript) //nolint:gosec // 路径由部署方配置
	cmd.Env = append(os.Environ(),
		"GITFERRY_TASK="+task.Key,
		"GITFERRY_RESULT="+result,
		"GITFERRY_RUN_ID="+fmt.Sprint(run.ID),
		"GITFERRY_TRIGGER="+run.TriggerSource,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(details, "\n=== post_exec_script error: %v ===\n%s\n", err, out)
		return
	}
	fmt.Fprintf(details, "\n=== post_exec_script ok ===\n%s\n", out)
}

// prepareRunContext 解析仓库/平台、准备工作目录,构造流水线共享运行态。
func (e *Executor) prepareRunContext(ctx context.Context, task *model.SyncTask, run *model.SyncRun, details *strings.Builder) (*RunContext, error) {
	sourceRepo, err := e.service.GetRepoByKey(task.SourceRepoKey)
	if err != nil {
		return nil, errors.Wrap(err, "query source repo failed")
	}
	if sourceRepo == nil {
		return nil, errors.Newf("source repo not found: %s", task.SourceRepoKey)
	}
	targetRepo, err := e.service.GetRepoByKey(task.TargetRepoKey)
	if err != nil {
		return nil, errors.Wrap(err, "query target repo failed")
	}
	if targetRepo == nil {
		return nil, errors.Newf("target repo not found: %s", task.TargetRepoKey)
	}

	// 空分支回退默认分支:避免坏 refspec 静默零推送
	runTask := *task
	if runTask.SourceBranch == "" {
		runTask.SourceBranch = defaultBranchOf(sourceRepo)
	}
	if runTask.TargetBranch == "" {
		runTask.TargetBranch = defaultBranchOf(targetRepo)
	}

	platforms := e.prefetchPlatforms(ctx, sourceRepo, targetRepo)
	workDir := e.service.GetTempDir(task.Key)
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return nil, errors.Wrap(err, "create work dir failed")
	}

	return &RunContext{
		Task:       &runTask,
		SourceRepo: sourceRepo,
		TargetRepo: targetRepo,
		Platforms:  platforms,
		WorkDir:    workDir,
		RepoDir:    filepath.Join(workDir, RepoDir),
		Details:    details,
		Run:        run,
		Exec:       e,
	}, nil
}

// failRun 标记 run 失败并按错误分类填充字段,统一 Execute 的失败出口。
func failRun(run *model.SyncRun, err error) (*model.SyncRun, error) {
	run.Status = model.StatusFailed
	run.ErrorMessage = err.Error()
	run.ErrorType = ClassifyError(err)
	return run, err
}

// prefetchPlatforms 预取 source/target 的 platform 记录,返回 platformID→Platform 映射。
// 一次 Execute 内复用,避免后续每次 git 操作都查 DB。
func (e *Executor) prefetchPlatforms(ctx context.Context, repos ...*model.Repo) map[uint]*model.Platform {
	platforms := make(map[uint]*model.Platform, 2)
	for _, repo := range repos {
		if repo.PlatformID == 0 {
			continue
		}
		if _, ok := platforms[repo.PlatformID]; ok {
			continue
		}
		p, err := e.service.GetPlatformByID(ctx, repo.PlatformID)
		if err != nil {
			slog.Warn("prefetch platform failed", "platformID", repo.PlatformID, "error", err)
			continue
		}
		platforms[repo.PlatformID] = p
	}
	return platforms
}

func (e *Executor) beginStep(runID uint, stepName string) *model.SyncRunStep {
	step := &model.SyncRunStep{
		RunID:     runID,
		StepName:  stepName,
		Status:    model.StatusRunning,
		StartTime: time.Now(),
	}
	if err := e.service.CreateRunStep(step); err != nil {
		slog.Error("failed to create run step", "step", stepName, "error", err)
	}
	return step
}

// completeStep marks a step as successfully completed.
func (e *Executor) completeStep(step *model.SyncRunStep, output string) {
	now := time.Now()
	step.EndTime = &now
	step.DurationMs = now.Sub(step.StartTime).Milliseconds()
	step.Status = model.StatusSuccess
	step.Output = output
	if err := e.service.UpdateRunStep(step); err != nil {
		slog.Error("failed to update run step", "step", step.StepName, "error", err)
	}
}

// failStep marks a step as failed with error classification.
func (e *Executor) failStep(step *model.SyncRunStep, err error) {
	now := time.Now()
	step.EndTime = &now
	step.DurationMs = now.Sub(step.StartTime).Milliseconds()
	step.Status = model.StatusFailed
	step.ErrorMsg = err.Error()
	step.ErrorType = ClassifyError(err)
	if updateErr := e.service.UpdateRunStep(step); updateErr != nil {
		slog.Error("failed to update run step", "step", step.StepName, "error", updateErr)
	}
}

func (e *Executor) cloneRepo(ctx context.Context, dir string, repo *model.Repo, task *model.SyncTask, platform *model.Platform) error {
	// 全量克隆:同步要把源仓库推到目标,浅克隆(depth=1)缺完整历史,
	// 首次推送到空目标会被平台拒绝(shallow update not allowed),
	// 且 gogit 会把该拒绝误报为 already up-to-date。
	// workdir 成功后保留,后续执行走增量 fetch,全量克隆只是一次性成本。
	// 多分支(glob)同步不能 SingleBranch,否则只克隆匹配到的第一分支
	multi := strings.ContainsAny(task.SourceBranch, "*?[")
	var partial string
	if cfg := e.service.GetConfig(); cfg != nil {
		partial = cfg.Sync.PartialClone
	}
	err := e.backend.Clone(ctx, gitbackend.CloneOptions{
		URL:          repo.CloneURL,
		Path:         dir,
		Branch:       task.SourceBranch,
		SingleBranch: !multi,
		Filter:       partial,
		Submodules:   task.Submodules,
		Auth:         e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		return err
	}
	e.excludeRefs(ctx, dir, task)
	return nil
}

// defaultExcludeRefs 忽略 PR/MR 引用,避免污染目标仓。
var defaultExcludeRefs = []string{"refs/pull/*", "refs/merge-requests/*"}

// excludeRefs 按 task.ExcludeRefPatterns(空=默认)删除本地匹配 ref。
func (e *Executor) excludeRefs(ctx context.Context, dir string, task *model.SyncTask) {
	patterns := splitCSVNonEmpty(task.ExcludeRefPatterns)
	if len(patterns) == 0 {
		patterns = defaultExcludeRefs
	}
	out, err := e.gitOutput(ctx, dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		for _, p := range patterns {
			if matchRefGlob(p, ref) {
				// update-ref 不在平台 RunRaw 白名单内,gitOutput 内回落裸 git。
				_, _ = e.gitOutput(ctx, dir, "update-ref", "-d", ref)
				break
			}
		}
	}
}

func splitCSVNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// matchRefGlob 用统一 globMatch（`*` 跨 `/`），匹配完整 ref 名。
func matchRefGlob(pattern, ref string) bool {
	return globMatch(pattern, ref)
}

// matchesInclude 是否在 include_branches 白名单内(空=全部)。
func matchesInclude(include, branch string) bool {
	list := splitCSVNonEmpty(include)
	if len(list) == 0 {
		return true
	}
	for _, p := range list {
		if p == branch {
			return true
		}
		if globMatch(p, branch) {
			return true
		}
	}
	return false
}

func (e *Executor) fetchRepo(ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) error {
	// 克隆地址在 DB 里被修正(私有实例重写/仓库迁移)后,既有 workdir 的
	// origin 仍指向旧地址;不同步会一直从错误源 fetch。
	if err := e.syncRemoteURL(ctx, dir, RemoteOrigin, repo.CloneURL); err != nil {
		slog.Warn("fetchRepo: update origin URL failed", "error", err, "dir", dir)
	}

	_, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteOrigin,
		Branches: []string{task.SourceBranch},
		Tags:     task.GitTags,
		Prune:    task.GitPrune,
		Auth:     e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		return err
	}
	e.excludeRefs(ctx, dir, task)

	// 增量同步时分支已在正确位置,仅当分支不同时才 checkout
	cur, curErr := e.backend.GetCurrentBranch(ctx, dir)
	if curErr != nil {
		slog.Warn("fetchRepo: GetCurrentBranch failed, will attempt checkout", "error", curErr, "dir", dir)
	}
	if cur != task.SourceBranch {
		return e.backend.Checkout(ctx, dir, task.SourceBranch)
	}
	return nil
}

func (e *Executor) ensureRemote(ctx context.Context, dir string, repo *model.Repo) error {
	// 先看 remote 是否已配置;存在时再核对 URL(目标 clone_url 被重写后
	// 只查存在性会继续推旧地址)。native 后端 GetRemoteURL 对缺失 remote
	// 不返回 ErrRemoteNotFound,所以必须先走 GetRemotes。
	remotes, err := e.backend.GetRemotes(ctx, dir)
	if err != nil {
		return err
	}
	exists := false
	for _, name := range remotes {
		if name == RemoteTarget {
			exists = true
			break
		}
	}
	if !exists {
		return e.backend.AddRemote(ctx, dir, RemoteTarget, repo.CloneURL)
	}
	return e.syncRemoteURL(ctx, dir, RemoteTarget, repo.CloneURL)
}

// syncRemoteURL 确保 named remote 指向 wantURL(已存在时按需重建)。
func (e *Executor) syncRemoteURL(ctx context.Context, dir, name, wantURL string) error {
	if wantURL == "" {
		return nil
	}
	cur, err := e.backend.GetRemoteURL(ctx, dir, name)
	if err != nil {
		return err
	}
	if remoteURLsEqual(cur, wantURL) {
		return nil
	}
	if err := e.backend.RemoveRemote(ctx, dir, name); err != nil {
		return err
	}
	return e.backend.AddRemote(ctx, dir, name, wantURL)
}

// remoteURLsEqual 比较 remote URL:忽略末尾斜杠,避免无意义的重建。
func remoteURLsEqual(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// defaultBranchOf 仓库默认分支,空则回退 "main"。
func defaultBranchOf(repo *model.Repo) string {
	if repo != nil && repo.DefaultBranch != "" {
		return repo.DefaultBranch
	}
	return model.DefaultBranch
}

func (e *Executor) push(ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) error {
	// 分歧保护:按 ForcePushPolicy(block/backup_on_demand/allow)处理
	policy := task.ForcePushPolicy
	if policy == "" {
		if task.KeepDivergent {
			policy = "block"
		} else {
			policy = "allow"
		}
	}
	backupDir := ""
	if cfg := e.service.GetConfig(); cfg != nil {
		backupDir = cfg.Sync.BackupDir
	}
	if err := e.checkDivergencePolicy(ctx, dir, task.SourceBranch, task.TargetBranch, task.GitForce, policy, backupDir, task.Key); err != nil {
		return err
	}

	// LFS 对象先推,避免代码分支推上去了大文件缺失
	if task.GitLFS {
		if err := e.syncLFS(ctx, dir, RemoteTarget, false); err != nil {
			return err
		}
	}

	// 必须用完整 refspec:go-git 按全名严格匹配本地 ref,不做 git CLI 的
	// 短名展开,"main:main" 匹配不到 refs/heads/main,会静默零推送
	// (返回 already up-to-date,被误判成功)。
	multi := strings.ContainsAny(task.SourceBranch, "*?[")
	var refSpecs []string
	if multi {
		branches, err := e.listLocalBranches(dir)
		if err == nil {
			for _, b := range branches {
				if !matchBranchGlob(task.SourceBranch, b) {
					continue
				}
				if !matchesInclude(task.IncludeBranches, b) {
					continue
				}
				refSpecs = append(refSpecs, fmt.Sprintf("refs/heads/%s:refs/heads/%s", b, b))
			}
		}
		if len(refSpecs) == 0 {
			return fmt.Errorf("no local branches match %q", task.SourceBranch)
		}
	} else {
		if !matchesInclude(task.IncludeBranches, task.SourceBranch) {
			return fmt.Errorf("branch %q not in include_branches %q", task.SourceBranch, task.IncludeBranches)
		}
		refSpecs = []string{fmt.Sprintf("refs/heads/%s:refs/heads/%s", task.SourceBranch, task.TargetBranch)}
	}

	_, err := e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		RefSpecs: refSpecs,
		Force:    task.GitForce,
		Auth:     e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		return err
	}

	// 推送侧 prune:目标上源已删除的同名分支一并删掉(仅单分支任务,语义清晰)
	if task.GitPushPrune && !multi {
		return e.pruneRemoteBranch(ctx, dir, task.TargetBranch, repo, platform)
	}
	return nil
}

// listLocalBranches 列出本地分支短名。
// 走 backend.ListLocalBranches(等价于原 for-each-ref --format=%(refname:short)
// refs/heads/),include/exclude 过滤留在调用方的 Go 侧。
func (e *Executor) listLocalBranches(dir string) ([]string, error) {
	ctx, cancel := withTimeout(context.Background(), 15)
	defer cancel()
	return e.backend.ListLocalBranches(ctx, dir)
}

// pruneRemoteBranch 删除目标上指定分支(源已不存在时)。
func (e *Executor) pruneRemoteBranch(ctx context.Context, dir, branch string, repo *model.Repo, platform *model.Platform) error {
	if branch == "" {
		return nil
	}
	spec := fmt.Sprintf(":refs/heads/%s", branch)
	_, err := e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		RefSpecs: []string{spec},
		Auth:     e.authConfig(ctx, repo, platform),
	})
	// 目标分支本就不存在时的错误可忽略
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
		return nil
	}
	return err
}

func timePtr(t time.Time) *time.Time {
	return &t
}
