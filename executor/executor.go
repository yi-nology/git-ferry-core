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
	//
	// targetRef 是**判定时用的那个目标状态**:消费必须精确落在判定通过的那条上,
	// 否则被消费掉的可能是另一条放行,真正授权的那条仍可反复使用。
	ConsumeForcePushApproval(taskKey, branch, targetRef string)
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

// postExecTimeout 是 post_exec 脚本的最长运行时间。
//
// 收尾阶段不能被外部脚本无限期拖住:run 状态与互斥锁都压在它后面。
var postExecTimeout = 5 * time.Minute

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
	// **必须有超时**:post_exec 脚本是部署方写的外部程序,可能挂住
	// (等输入、等网络、sleep infinity)。而它跑在收尾的 defer 里 ——
	// 挂住 ⇒ CompleteRunWithTaskUpdate 永不执行 ⇒ run 记录永远停在 running,
	// 同时调用方持有的同 taskKey 互斥锁与全局并发槽一起被占住;
	// 挂够 max_concurrent 次之后,整个服务的同步全部起不来。
	//
	// 超时后杀掉子进程并记进 details(脚本失败不影响同步结果,这是既有语义)。
	execCtx, cancel := context.WithTimeout(context.Background(), postExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, cfg.Sync.PostExecScript) //nolint:gosec // 路径由部署方配置
	// CommandContext 只杀**直接子进程**:脚本里再 fork 的孙子(sleep 等)仍持有
	// stdout 管道 ⇒ CombinedOutput 会一直等管道关闭。
	// WaitDelay 兜底:进程被杀后最多再等这么久就返回,不再挂住收尾。
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(),
		"GITFERRY_TASK="+task.Key,
		"GITFERRY_RESULT="+result,
		"GITFERRY_RUN_ID="+fmt.Sprint(run.ID),
		"GITFERRY_TRIGGER="+run.TriggerSource,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if execCtx.Err() != nil {
			fmt.Fprintf(details, "\n=== post_exec_script 超时(%s 被终止): %v ===\n%s\n",
				postExecTimeout, execCtx.Err(), out)
			return
		}
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
	e.widenOriginFetchRefspec(ctx, dir)
	e.excludeRefs(ctx, dir, task)
	// **多分支:clone 之后也要对齐**。
	//
	// clone 只把默认分支建成**本地**分支,其余分支只存在于 refs/remotes/origin/*。
	// 而 push 推的是 refs/heads/*(本地) ⇒ 不对齐的话:
	//   - glob 不含默认分支 ⇒ 拼不出 refspec,整次同步失败
	//     ("no local branches match");
	//   - glob 含默认分支(如 *) ⇒ 只同步了默认分支,其余 N-1 个**静默没推**,
	//     报告却是 success。
	// 增量路径(fetch 之后)本来就有对齐,这里让首次与后续行为一致。
	if strings.ContainsAny(task.SourceBranch, "*?[") {
		return e.alignSourceBranchesExcluded(ctx, dir, task.SourceBranch, task)
	}
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

// widenOriginFetchRefspec 把 origin 的 fetch refspec 放宽到"全部分支"。
//
// **为什么必须放宽**:单分支 clone 走的是 --single-branch,git 会把本地的
// fetch refspec 限定成那一个分支 ⇒ 源端删掉该分支后,后续
// `git fetch --prune origin` 会报 "couldn't find remote ref refs/heads/<b>"
// (fetch 的是**配置里的 refspec**,不是"远端现在有什么")⇒
// 源端删分支 = 同步直接失败,而且错误信息完全指不到"镜像删除"这件事。
//
// 放宽后:源端删掉的分支只是**不存在**(--prune 会剪掉它的远端跟踪 ref),
// fetch 不再报错,"源端删了分支 ⇒ 目标侧也删"这条镜像语义才有机会执行。
//
// 对 gogit 后端无副作用(它按入参构造 refspec,不看本地配置)。
func (e *Executor) widenOriginFetchRefspec(ctx context.Context, dir string) {
	const want = "+refs/heads/*:refs/remotes/" + RemoteOrigin + "/*"
	cur, err := e.backend.GetConfig(ctx, dir, "remote."+RemoteOrigin+".fetch")
	if err == nil && strings.TrimSpace(cur) == want {
		return
	}
	if err := e.backend.SetConfig(ctx, dir, "remote."+RemoteOrigin+".fetch", want); err != nil {
		slog.Warn("widenOriginFetchRefspec 失败,源端删除分支时可能报错",
			"dir", dir, "error", err)
	}
}

// sourceBranchGone 判断"源端是否已经删掉了这个分支"。
//
// 判据:refs/remotes/origin/<branch> 在一次**全量** fetch(--prune 恒开)之后
// 仍然不存在 ⇒ 源端确实没有这个分支了。
//
// 为什么不能只看第一次 fetch 的失败:那也可能是网络/鉴权/证书问题 ——
// 把"连不上"当成"源端删了"会让同步静默地不推送(数据不前进却报成功)。
func (e *Executor) sourceBranchGone(ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) bool {
	if _, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteOrigin,
		Branches: nil, // 全量:让 --prune 有机会剪掉源端已删的分支
		Tags:     task.GitTags,
		Auth:     e.authConfig(ctx, repo, platform),
	}); err != nil {
		// 连不上就**不能**下"源端删了"的结论。
		slog.Warn("sourceBranchGone: 全量 fetch 失败,无法判定源端是否删除了该分支",
			"branch", task.SourceBranch, "error", err)
		return false
	}
	ref := "refs/remotes/" + RemoteOrigin + "/" + task.SourceBranch
	if _, err := e.backend.RevParse(ctx, dir, ref); err == nil {
		return false // 源端还有这个分支
	}
	return true
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

	// 放宽 fetch refspec:单分支 clone 的 refspec 指向那一个分支,
	// 源端删掉它之后 fetch 会直接报错(而不是"这个分支没了")。
	e.widenOriginFetchRefspec(ctx, dir)

	_, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteOrigin,
		Branches: fetchBranches,
		Tags:     task.GitTags,
		// **恒剪枝**(不再跟 task.GitPrune 走):
		//
		// 剪掉"源端已删分支"的远端跟踪 ref,是镜像语义的基石 ——
		// 不剪的话:① 陈旧的跟踪 ref 留在本地 ⇒ align 把本地分支对齐到旧提交
		// ⇒ **源端已删的分支又被推回目标**(分支"复活");
		// ② sourceBranchGone 判断"源端是否还在"靠的就是 ref 被剪掉
		// ⇒ 不剪的话镜像删除(git_push_prune)永远不触发。
		//
		// 注意:native 后端本来就恒传 --prune,而 gogit 后端(生产用的那个)
		// 只在 Prune=true 时才剪 ⇒ **同一份配置在两个后端行为不同**,
		// 生产上镜像删除其实一直没生效。这里显式传 true,两个后端口径一致。
		Prune: true,
		Auth:  e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		// **源端可能已经删掉了这个分支**:按名字 fetch 一个远端不存在的分支,
		// git 会直接报 "couldn't find remote ref" ⇒ 整次同步失败。
		// 而正确行为是"目标侧同名分支也删掉"(镜像语义),不是失败。
		//
		// 区分办法:退化成一次**全量** fetch(SDK 的 --prune 恒开,
		// 源端删掉的分支其远端跟踪 ref 会被剪掉),再看 refs/remotes/origin/<src>
		// 还在不在:不在 = 源端确实删了(不是网络/鉴权问题)。
		if len(fetchBranches) > 0 && e.sourceBranchGone(ctx, dir, task, repo, platform) {
			slog.Info("fetchRepo: 源端已删除该分支,按镜像语义处理(不推送)",
				"branch", task.SourceBranch, "dir", dir)
			return nil
		}
		return err
	}
	e.excludeRefs(ctx, dir, task)

	// 增量同步的关键一步:fetch 只更新 refs/remotes/origin/<branch>,
	// 本地分支仍停在旧提交。而 push 推的是 refs/heads/<branch>(本地),
	// 不把本地分支推进到刚取到的源端 ref ⇒ 目标侧永不前进,
	// git 回 "already up-to-date" 被当成成功 —— 增量同步等于没跑。
	return e.alignSourceBranchesExcluded(ctx, dir, task.SourceBranch, task)
}

// alignSourceBranchesExcluded 带排除名单的分支对齐。
//
// **为什么 align 也要看 exclude 名单**:增量路径的顺序是
// fetch → excludeRefs(删匹配 ref) → alignSourceBranches(按 glob 把
// refs/remotes/origin/<b> 写回 refs/heads/<b>)。
// align **不看排除名单** ⇒ 被 exclude 名单命中的分支在删掉之后又被
// 重新创建成本地分支,下一次 push 照样推给目标 ⇒
// 黑名单形同虚设(默认的 refs/pull/* 不受影响,因为它不是分支)。
func (e *Executor) alignSourceBranchesExcluded(ctx context.Context, dir, sourceBranch string, task *model.SyncTask) error {
	patterns := stringx.SplitCSVNonEmpty(task.ExcludeRefPatterns)
	if len(patterns) == 0 {
		patterns = defaultExcludeRefs
	}
	excluded := func(branch string) bool {
		ref := "refs/heads/" + branch
		for _, p := range patterns {
			if matchRefGlob(p, ref) {
				return true
			}
		}
		return false
	}
	if !strings.ContainsAny(sourceBranch, "*?[") {
		if excluded(sourceBranch) {
			slog.Info("align: 分支命中排除名单,跳过", "branch", sourceBranch)
			return nil
		}
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
		// "origin" 是 origin/HEAD 这个**符号引用**的短名(不是分支):
		// 对齐它必然失败,只会刷一条 "source ref missing" 噪音日志。
		if name == ref || name == "" || name == "HEAD" {
			continue
		}
		if !matchBranchGlob(sourceBranch, name) {
			continue
		}
		if excluded(name) {
			slog.Info("align: 分支命中排除名单,跳过", "branch", name)
			continue
		}
		if err := e.alignSourceBranch(ctx, dir, name); err != nil {
			return err
		}
	}
	return nil
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
// 第三个返回值 approvalTargetRef = 该放行绑定的目标状态(消费时必须原样带回)。
func (e *Executor) push(ctx context.Context, dir string, task *model.SyncTask, repo *model.Repo, platform *model.Platform) (bool, string, string, error) {
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
	// 必须用完整 refspec:go-git 按全名严格匹配本地 ref,不做 git CLI 的
	// 短名展开,"main:main" 匹配不到 refs/heads/main,会静默零推送
	// (返回 already up-to-date,被误判成功)。
	multi := strings.ContainsAny(task.SourceBranch, "*?[")
	var refSpecs []string
	var branches []string
	if multi {
		all, lerr := e.listLocalBranches(dir)
		if lerr == nil {
			for _, b := range all {
				if !matchBranchGlob(task.SourceBranch, b) {
					continue
				}
				if !matchesInclude(task.IncludeBranches, b) {
					continue
				}
				branches = append(branches, b)
			}
		}
		if len(branches) == 0 {
			return false, "", "", fmt.Errorf("no local branches match %q", task.SourceBranch)
		}
		for _, b := range branches {
			refSpecs = append(refSpecs, fmt.Sprintf("refs/heads/%s:refs/heads/%s", b, b))
		}
	} else {
		if !matchesInclude(task.IncludeBranches, task.SourceBranch) {
			return false, "", "", fmt.Errorf("branch %q not in include_branches %q", task.SourceBranch, task.IncludeBranches)
		}
		// **源端还有这个分支吗?**
		//
		// 源端删掉分支后,本地那个 refs/heads/<src> 是**残留**
		// (align 阶段不会删本地分支,只会在源端 ref 不存在时跳过对齐)。
		// 把它推过去 = 把源端已经删掉的分支又搬回目标(目标侧"复活"了一个
		// 源端已经不存在的分支)。
		//
		// 源端确实没有 ⇒ 本次**不推**,改为按镜像语义处理:
		// 开了 git_push_prune 就把目标侧同名分支删掉,没开就保持不动。
		sourceRef := "refs/remotes/" + RemoteOrigin + "/" + task.SourceBranch
		if _, rerr := e.backend.RevParse(ctx, dir, sourceRef); rerr != nil {
			if task.GitPushPrune {
				if perr := e.pruneRemoteBranch(ctx, dir, task.TargetBranch, repo, platform); perr != nil {
					return false, "", "", perr
				}
				slog.Info("源端已删除该分支,已删除目标侧同名分支(镜像删除)",
					"source", task.SourceBranch, "target", task.TargetBranch)
			} else {
				slog.Info("源端已删除该分支,本次不推送(git_push_prune 未开启,目标侧保持不动)",
					"source", task.SourceBranch, "target", task.TargetBranch)
			}
			return false, "", "", nil
		}
		branches = []string{task.SourceBranch}
		refSpecs = []string{fmt.Sprintf("refs/heads/%s:refs/heads/%s", task.SourceBranch, task.TargetBranch)}
	}

	// 分歧保护:**逐分支**判定。
	//
	// 多分支任务以前把 glob("release/*")当分支名传进判定,
	// git rev-list 对含 * 的 rev 必然报错 ⇒ 旧实现 fail-open 直接放行 ⇒
	// block 策略不拦、backup_on_demand 不备份,随后强推照常执行 ——
	// 分歧保护对多分支**整体失效**。现在按实际要推的分支逐个判定。
	var usedApproval bool
	var approvalTargetRef string
	// approvalBranch 是"这次放行对应的分支" —— 消费时要用它,
	// 不能用 task.TargetBranch(多分支任务里那是 glob 或空)⇒
	// 否则放行既不被消费、也不报错,approved 一直挂着 ⇒
	// "一次性放行"对多分支退化成常驻后门。
	var approvalBranch string
	if multi {
		// 目标端一次取回(逐分支 fetch 太慢)。
		//
		// **knownBranches 传"本次要推的分支"**:fetch 参数是 nil(全量),
		// 若用它判断"目标端是否已知存在",循环体一次都不执行 ⇒
		// fetch 失败会被当成"目标端没有这些分支"放行 —— fail-closed 形同虚设,
		// 目标侧独有提交被静默覆盖。
		targetKnown, ferr := e.fetchTargetForDivergence(ctx, dir, nil, branches, repo, platform)
		if ferr != nil {
			return false, "", "", errors.Wrap(ferr,
				"cannot verify divergence for multi-branch task: target fetch failed")
		}
		for _, b := range branches {
			used, ref, derr := e.checkDivergenceForBranch(ctx, dir, b, b, task.GitForce, policy, backupDir, task.Key, targetKnown)
			if derr != nil {
				return false, ref, approvalBranch, derr
			}
			if used {
				usedApproval = true
				approvalTargetRef = ref
				approvalBranch = b
			}
		}
	} else {
		var derr error
		usedApproval, approvalTargetRef, derr = e.checkDivergencePolicy(ctx, dir, task.SourceBranch, task.TargetBranch, task.GitForce, policy, backupDir, task.Key, repo, platform)
		if derr != nil {
			return false, approvalTargetRef, approvalBranch, derr
		}
		approvalBranch = task.TargetBranch
	}

	// LFS 对象先推,避免代码分支推上去了大文件缺失
	if task.GitLFS {
		if err := e.syncLFS(ctx, dir, RemoteTarget, false); err != nil {
			return usedApproval, approvalTargetRef, approvalBranch, err
		}
	}

	_, err := e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		RefSpecs: refSpecs,
		Force:    task.GitForce,
		Auth:     e.authConfig(ctx, repo, platform),
	})
	if err != nil {
		return usedApproval, approvalTargetRef, approvalBranch, err
	}

	// 推送侧 prune **只发生在"源端已删除该分支"的那条路径上**
	// (见上面 sourceRef 不存在时的分支)。
	//
	// 原实现在这里**无条件**删目标分支 —— 也就是"推上去紧接着删掉",
	// 任何勾了 git_push_prune 的单分支任务,每次同步结束目标侧该分支都没了,
	// 而任务报 success。
	if task.GitPushPrune && multi {
		if perr := e.pruneRemoteBranchesMirror(ctx, dir, task, repo, platform); perr != nil {
			return usedApproval, approvalTargetRef, approvalBranch, perr
		}
	}
	return usedApproval, approvalTargetRef, approvalBranch, nil
}

// pruneRemoteBranchesMirror 多分支的镜像删除:删掉目标上**源端已不存在**的分支。
//
// 判定依据是两端 fetch 之后的远端跟踪 ref:
//   - 目标侧有哪些分支:refs/remotes/target/*(这里现取一次目标端)
//   - 源端有哪些分支:refs/remotes/origin/*(源端 fetch 恒剪枝 ⇒ 源端删掉的分支不会留下 ref)
//
// **三条安全阀,少一条都不删**:
//
//	① 源端分支列表为空 ⇒ 源端 fetch 没成功 ⇒ 什么都不删;
//	② 目标端 fetch 失败 ⇒ 不知道目标有哪些分支 ⇒ 什么都不删;
//	③ 只删"目标有、源端没有"的分支,其余一律不碰。
func (e *Executor) pruneRemoteBranchesMirror(ctx context.Context, dir string, task *model.SyncTask,
	repo *model.Repo, platform *model.Platform) error {
	if _, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		Branches: nil, // 全量:要知道目标端**所有**分支
		Auth:     e.authConfig(ctx, repo, platform),
	}); err != nil {
		return errors.Wrap(err, "fetch target for mirror prune failed")
	}
	srcBranches, err := e.remoteTrackingBranches(ctx, dir, RemoteOrigin)
	if err != nil {
		return err
	}
	tgtBranches, err := e.remoteTrackingBranches(ctx, dir, RemoteTarget)
	if err != nil {
		return err
	}
	if len(srcBranches) == 0 {
		slog.Warn("镜像删除跳过:源端分支列表为空(源端 fetch 未成功?)",
			"task", task.Key, "sourcePattern", task.SourceBranch)
		return nil
	}
	srcSet := map[string]bool{}
	for _, b := range srcBranches {
		srcSet[b] = true
	}
	var victims []string
	for _, b := range tgtBranches {
		if !srcSet[b] {
			victims = append(victims, b)
		}
	}
	if len(victims) == 0 {
		return nil
	}
	var failures []string
	for _, b := range victims {
		if perr := e.pruneRemoteBranch(ctx, dir, b, repo, platform); perr != nil {
			failures = append(failures, b+": "+perr.Error())
		}
	}
	if len(failures) > 0 {
		return errors.Errorf("镜像删除失败 %d 项:%s", len(failures), strings.Join(failures, "; "))
	}
	slog.Info("已删除目标上源端已不存在的分支",
		"task", task.Key, "count", len(victims), "branches", strings.Join(victims, ","))
	return nil
}

// remoteTrackingBranches 列出某个远端的远端跟踪分支(跳过 origin/HEAD 这类伪 ref)。
func (e *Executor) remoteTrackingBranches(ctx context.Context, dir, remote string) ([]string, error) {
	out, err := e.gitOutput(ctx, dir, "for-each-ref", "--format=%(refname:short)",
		"refs/remotes/"+remote+"/")
	if err != nil {
		return nil, err
	}
	var res []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		name := strings.TrimPrefix(l, remote+"/")
		if name == l || name == "" || name == "HEAD" {
			continue // origin/HEAD 这类符号引用不是分支
		}
		res = append(res, name)
	}
	return res, nil
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
