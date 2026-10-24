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
	"github.com/oekit/pkg/pathx"
	"github.com/oekit/pkg/ptrx"
	"github.com/oekit/pkg/stringx"
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
	// IsForcePushApproved 查询放行是否生效。
	// targetRef 是**当前**目标分支的 HEAD SHA:放行只对它被批准时的目标状态有效
	// (目标侧此后又变了 ⇒ 旧放行不再适用,必须重新审批)。
	IsForcePushApproved(taskKey, branch, targetRef string) bool
	// RequestApproval 产生一条 pending 审批（best-effort，失败不影响主流程）。
	// targetRef 记录申请时的目标分支 HEAD SHA,用于上面的绑定判定。
	RequestApproval(taskKey, branch, reason, targetRef string)
	// ConsumeForcePushApproval 消费掉一次放行。
	// 放行是**一次性**的:一次成功的强推之后就要用掉,下次同任务同分支
	// 再出现分歧必须重新审批(否则一次人工放行变成常驻后门)。
	ConsumeForcePushApproval(taskKey, branch string)
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
		run.EndTime = ptrx.Of(time.Now())
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
		if run.Status == model.StatusSuccess {
			return
		}
		// 失败时**不总是**删掉工作区:
		//  - 工作区本身是坏的(克隆没成功/被中断)⇒ 留着只会让下一次执行
		//    在坏仓库上继续失败,直接删;
		//  - 工作区是好的(失败发生在推送/鉴权/网络这类与本地状态无关的环节)
		//    ⇒ 保留,下一次走增量 fetch —— 全量克隆是大仓的主要成本,
		//    "失败一次就退回全量克隆"等于把增量收益清零。
		if e.workdirUsable(ctx, rc.RepoDir) {
			slog.Info("keep workdir after failed run; next run will fetch incrementally", "dir", rc.RepoDir)
			return
		}
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			slog.Error("failed to cleanup temp dir", "error", rmErr, "dir", workDir)
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
	cloneBranch := task.SourceBranch
	if multi {
		cloneBranch = ""
	}
	var partial string
	if cfg := e.service.GetConfig(); cfg != nil {
		partial = cfg.Sync.PartialClone
	}
	err := e.backend.Clone(ctx, gitbackend.CloneOptions{
		URL:  repo.CloneURL,
		Path: dir,
		// glob 多分支:--branch 只接受具体分支名,给 glob 会让 git clone 直接失败
		// ("Remote branch release/* not found in upstream origin")。
		// 多分支走全量克隆,再由 alignSourceBranches 按 glob 建出本地分支。
		Branch:       cloneBranch,
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
	patterns := stringx.SplitCSVNonEmpty(task.ExcludeRefPatterns)
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

// matchRefGlob 用统一 globMatch（`*` 跨 `/`），匹配完整 ref 名。
func matchRefGlob(pattern, ref string) bool {
	return pathx.MatchGlob(pattern, ref)
}

// matchesInclude 是否在 include_branches 白名单内(空=全部)。
func matchesInclude(include, branch string) bool {
	list := stringx.SplitCSVNonEmpty(include)
	if len(list) == 0 {
		return true
	}
	for _, p := range list {
		if p == branch {
			return true
		}
		if pathx.MatchGlob(p, branch) {
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

	// glob 多分支:fetch 的 branch 参数要求完整 refspec 形态,
	// "release/*" 不是合法 refspec(git: invalid refspec 'release/*')。
	// 多分支改为全量 fetch,再由 alignSourceBranches 按 glob 建出本地分支。
	fetchBranches := []string{task.SourceBranch}
	if strings.ContainsAny(task.SourceBranch, "*?[") {
		fetchBranches = nil
	}

	_, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteOrigin,
		Branches: fetchBranches,
		Tags:     task.GitTags,
		Prune:    task.GitPrune,
		Auth:     e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		return err
	}
	e.excludeRefs(ctx, dir, task)

	// 增量同步的关键一步:fetch 只更新 refs/remotes/origin/<branch>,
	// 本地分支仍停在旧提交。而 push 推的是 refs/heads/<branch>(本地),
	// 不把本地分支推进到刚取到的源端 ref ⇒ 目标侧永不前进,
	// git 回 "already up-to-date" 被当成成功 —— 增量同步等于没跑。
	return e.alignSourceBranches(ctx, dir, task.SourceBranch)
}

// alignSourceBranches 把本地源分支对齐到刚 fetch 到的远端跟踪 ref。
// 单分支任务对齐一个分支;glob 多分支任务对齐所有匹配的远端跟踪分支
// (源端新建的分支本地还没有,只对齐"已存在的本地分支"会漏掉它)。
func (e *Executor) alignSourceBranches(ctx context.Context, dir, sourceBranch string) error {
	if !strings.ContainsAny(sourceBranch, "*?[") {
		return e.alignSourceBranch(ctx, dir, sourceBranch)
	}
	out, err := e.gitOutput(ctx, dir, "for-each-ref", "--format=%(refname:short)",
		"refs/remotes/"+RemoteOrigin+"/")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		name := strings.TrimPrefix(ref, RemoteOrigin+"/")
		if !matchBranchGlob(sourceBranch, name) {
			continue
		}
		if err := e.alignSourceBranch(ctx, dir, name); err != nil {
			return err
		}
	}
	return nil
}

// alignSourceBranch 把单个本地分支对齐到 refs/remotes/origin/<branch>。
//
// 源端是唯一真相(镜像语义):本地分支必须等于 origin/<branch>,
// 源端改写历史(force push)时本地也跟着走。
//
// 两条命令按"HEAD 在不在该分支上"分流:
//   - HEAD 在该分支上 ⇒ reset --hard(ref 与工作区一起更新,不留脏状态);
//   - 否则 ⇒ update-ref 只动 ref(不动 HEAD 与工作区,避免把别的分支带偏)。
//
// 两者都不在平台 git 子命令白名单里,gitOutput 会回落到裸 git ——
// 与 excludeRefs 用 update-ref 的做法一致。
func (e *Executor) alignSourceBranch(ctx context.Context, dir, branch string) error {
	ref := "refs/remotes/" + RemoteOrigin + "/" + branch
	srcSHA, err := e.backend.RevParse(ctx, dir, ref)
	if err != nil {
		// 源端已无此分支(被删)⇒ 本地分支留在原地,push 阶段按需 prune。
		slog.Warn("alignSourceBranch: source ref missing, skip", "branch", branch, "ref", ref, "dir", dir, "error", err)
		return nil
	}
	if curSHA, err := e.backend.RevParse(ctx, dir, "refs/heads/"+branch); err == nil && curSHA == srcSHA {
		return nil
	}
	cur, curErr := e.backend.GetCurrentBranch(ctx, dir)
	if curErr != nil {
		slog.Warn("alignSourceBranch: GetCurrentBranch failed", "error", curErr, "dir", dir)
	}
	if cur == branch {
		if _, err := e.gitOutput(ctx, dir, "reset", "--hard", ref); err != nil {
			return errors.Wrapf(err, "align source branch %q to %s failed", branch, ref)
		}
		return nil
	}
	if _, err := e.gitOutput(ctx, dir, "update-ref", "refs/heads/"+branch, srcSHA); err != nil {
		return errors.Wrapf(err, "align source branch %q to %s failed", branch, srcSHA)
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

// workdirUsable 工作区是不是一个可用的 git 仓库(有 .git 且 HEAD 能解析)。
// 用于失败后的清理决策:坏工作区要删,好工作区要留(留给下一次增量 fetch)。
func (e *Executor) workdirUsable(ctx context.Context, dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	if _, err := e.backend.RevParse(ctx, dir, "HEAD"); err != nil {
		return false
	}
	return true
}

// push 推送分支到目标端。
// 第二个返回值 usedApproval = 本次是否用掉了一次人工放行(调用方负责在成功后兑现"一次性")。
func (e *Executor) push(ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) (bool, error) {
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
	usedApproval, err := e.checkDivergencePolicy(ctx, dir, task.SourceBranch, task.TargetBranch, task.GitForce, policy, backupDir, task.Key, repo, platform)
	if err != nil {
		return false, err
	}

	// LFS 对象先推,避免代码分支推上去了大文件缺失
	if task.GitLFS {
		if err := e.syncLFS(ctx, dir, RemoteTarget, false); err != nil {
			return usedApproval, err
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
			return usedApproval, fmt.Errorf("no local branches match %q", task.SourceBranch)
		}
	} else {
		if !matchesInclude(task.IncludeBranches, task.SourceBranch) {
			return usedApproval, fmt.Errorf("branch %q not in include_branches %q", task.SourceBranch, task.IncludeBranches)
		}
		refSpecs = []string{fmt.Sprintf("refs/heads/%s:refs/heads/%s", task.SourceBranch, task.TargetBranch)}
	}

	_, err = e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		RefSpecs: refSpecs,
		Force:    task.GitForce,
		Auth:     e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		return usedApproval, err
	}

	// 推送侧 prune:目标上源已删除的同名分支一并删掉(仅单分支任务,语义清晰)
	if task.GitPushPrune && !multi {
		return usedApproval, e.pruneRemoteBranch(ctx, dir, task.TargetBranch, repo, platform)
	}
	return usedApproval, nil
}

// pushTags 把源端的 tag 推到目标端。
//
// 主推送只带分支 refspec,tag 必须**单独推**:语义等同 `git push --tags`
// —— 全量推、**不覆盖**已存在的 tag(同名不同 SHA 会被拒绝)。
// 单独推还有一层好处:tag 冲突不会把**已经成功的分支推送**一起拖成失败。
// 目标端本来就没有同名 tag 时,tag 就补齐了(git_tags=true 的既有语义)。
func (e *Executor) pushTags(ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) error {
	if !task.GitTags {
		return nil
	}
	auth := e.authConfig(ctx, repo, platform)
	// 批量推(常见情况一次到位)
	bulkErr := error(nil)
	if _, err := e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		RefSpecs: []string{"refs/tags/*:refs/tags/*"},
		Auth:     auth,
	}); err != nil {
		bulkErr = err
	} else {
		return nil
	}
	// 批量失败:多半是**某一个** tag 冲突(同名不同 SHA)。
	// git 的批量 push 是**全有全无** ⇒ 一个冲突会把其它本来没问题的 tag 一起挡住
	// (实测:目标端有一个同名不同 SHA 的 tag,源端**所有** tag 都没同步上)。
	// 退化成逐个推:冲突的那个单独报错,其它照常送达。
	tags, err := e.backend.GetTagList(ctx, dir)
	if err != nil {
		return bulkErr
	}
	var failed []string
	for _, t := range tags {
		if err := e.backend.PushTag(ctx, dir, RemoteTarget, t.Name, auth); err != nil {
			failed = append(failed, t.Name)
		}
	}
	if len(failed) > 0 {
		return errors.Wrapf(bulkErr, "push tags failed for %d/%d tag(s): %s",
			len(failed), len(tags), strings.Join(failed, ","))
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
