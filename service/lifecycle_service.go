package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/oekit/pkg/pathx"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// DiscoveryReport 自动发现结果。
type DiscoveryReport struct {
	PlatformKey string    `json:"platform_key"`
	ScannedAt   time.Time `json:"scanned_at"`
	Found       int       `json:"found"`
	Existing    int       `json:"existing"`
	NewRepos    []string  `json:"new_repos"`
	Imported    int       `json:"imported"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// AutoDiscoverOptions 自动发现参数。
type AutoDiscoverOptions struct {
	// ImportNew=true 时直接导入新仓库,否则只报告
	ImportNew bool
	Filter    *RepoImportFilter
}

// AutoDiscover 扫描平台仓库,找出本地尚未登记的(可选自动导入)。
// 借鉴 gitea-mirror 的 auto-discovery:新仓库自动入册,无需人工点选。
func (s *Service) AutoDiscover(ctx context.Context, platformKey string, opts AutoDiscoverOptions) (*DiscoveryReport, error) {
	rep := &DiscoveryReport{
		PlatformKey: platformKey,
		ScannedAt:   time.Now().UTC(),
		NewRepos:    []string{},
	}

	filter := opts.Filter
	if filter == nil {
		filter = &RepoImportFilter{ExcludeArchived: true, ExcludeForks: true}
	}

	if opts.ImportNew {
		n, err := s.SyncPlatformReposFiltered(ctx, platformKey, filter)
		if err != nil {
			return rep, err
		}
		rep.Imported = n
	}

	local, err := s.ListReposByPlatform(ctx, platformKey)
	if err != nil {
		return rep, err
	}
	localSet := map[string]bool{}
	for _, r := range local {
		localSet[strings.ToLower(r.PlatformOwner+"/"+r.PlatformRepo)] = true
	}
	rep.Existing = len(local)

	plat, err := s.GetPlatform(ctx, platformKey)
	if err != nil || plat == nil {
		rep.Warnings = append(rep.Warnings, "platform not found")
		return rep, nil
	}
	prov, err := platformProvider(s.platforms.providerMgr, plat, s.providerHooks)
	if err != nil {
		rep.Warnings = append(rep.Warnings, "provider: "+err.Error())
		return rep, nil
	}
	remoteRepos, err := fetchAllPlatformRepos(ctx, prov)
	if err != nil {
		rep.Warnings = append(rep.Warnings, "list repos: "+err.Error())
		return rep, nil
	}
	for _, r := range remoteRepos {
		if !filter.Allow(r) {
			continue
		}
		rep.Found++
		name := r.FullName
		if name == "" {
			name = r.Name
		}
		if !localSet[strings.ToLower(name)] {
			rep.NewRepos = append(rep.NewRepos, name)
		}
	}
	return rep, nil
}

// DriftItem 单个仓库的漂移检测结果。
type DriftItem struct {
	TaskKey     string `json:"task_key"`
	RepoKey     string `json:"repo_key"`
	Branch      string `json:"branch"`
	LocalRef    string `json:"local_ref"`
	RemoteRef   string `json:"remote_ref,omitempty"`
	Drifted     bool   `json:"drifted"`
	LocalAhead  int    `json:"local_ahead"`
	RemoteAhead int    `json:"remote_ahead"`
	Message     string `json:"message"`
	// TargetBranch 目标分支(与实际比对的 SourceBranch 相对)。
	TargetBranch string `json:"target_branch,omitempty"`
}

// DriftReport 漂移检测汇总。
type DriftReport struct {
	Generated time.Time   `json:"generated"`
	Checked   int         `json:"checked"`
	Drifted   int         `json:"drifted"`
	Items     []DriftItem `json:"items"`
	Warnings  []string    `json:"warnings,omitempty"`
}

// DriftDetectBudget 漂移检测的**总**预算。
//
// 每个漂移任务都要 fetch 一次源端才能算出 ahead/behind(见 fetchDriftSource),
// 耗时与"漂移任务数 × 单次 fetch 耗时"成正比。上游慢、任务多时(200 个漂移任务
// × 30s 拨号超时)整轮能跑到几十分钟,而调用方(HTTP/健康评分)早超时了 ——
// 服务端还在空转。
//
// 预算耗尽后,剩下的任务标为"未检查"如实报出,而不是继续跑或假装检查过。
const DriftDetectBudget = 30 * time.Second

// DetectDrift 检测**源端与目标端**的分支漂移。
//
// 用于发现「镜像声称同步成功但 refs 实际不一致」的静默故障。
// taskKeys 为空时扫描全部启用任务。
//
// **语义（重写）**：漂移 = 目标端与源端不一致。
//
// 原来比的是"工作区本地分支 vs 源端远端" ⇒ 报出来的多半是"源端前进了"
// —— 那是**正常状态**(下次同步就推上去了),不是漂移;
// 而真正该报的"目标端跑偏了(目标有源端没有的提交)"**从来没被检查过**。
//
// 另外三处一并修掉：
//   - **带凭证**:原来走 `git ls-remote`,而平台的 RunRaw 固定用空凭证
//     ⇒ 私有仓(本产品的主要场景)永远 "cannot resolve refs",
//     漂移检测形同虚设;现在用带凭证的 fetch 取两端状态。
//   - **专用工作目录**:不复用 executor 的 workdir —— 检测时的 fetch 会写
//     git 锁,和正在跑的同步抢同一份目录会互相报错。
//   - **不静默截断**:任务列表被上限截断时如实报"未检查"。
func (s *Service) DetectDrift(ctx context.Context, taskKeys []string) (*DriftReport, error) {
	rep := &DriftReport{Generated: time.Now().UTC(), Items: []DriftItem{}}

	var tasks []*model.SyncTask
	if len(taskKeys) > 0 {
		for _, k := range taskKeys {
			t, err := s.GetTask(ctx, k)
			if err != nil || t == nil {
				rep.Warnings = append(rep.Warnings, "task not found: "+k)
				continue
			}
			tasks = append(tasks, t)
		}
	} else {
		// 任务列表有上限(dao 侧钳到 200)⇒ 被截断的部分**如实报"未检查"**,
		// 不能让人以为"全部都查过了、没问题"。
		list, total, err := s.ListTasks(ctx, "", 0, 0)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			if t.Enabled {
				tasks = append(tasks, t)
			}
		}
		if int64(len(list)) < total {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("任务列表超过单页上限(已检查 %d / 共 %d),其余任务未检查", len(list), total))
		}
	}

	// 整轮检测有总预算(见 DriftDetectBudget):超了就如实标"未检查"。
	ctx, cancel := context.WithTimeout(ctx, DriftDetectBudget)
	defer cancel()

	for _, t := range tasks {
		if ctx.Err() != nil {
			rep.Warnings = append(rep.Warnings,
				"漂移检测预算("+DriftDetectBudget.String()+")已用尽,剩余任务未检查")
			break
		}
		item := DriftItem{TaskKey: t.Key, Branch: t.SourceBranch, TargetBranch: t.TargetBranch, RepoKey: t.TargetRepoKey}
		repo, rerr := s.GetRepoByKey(t.TargetRepoKey)
		if rerr != nil || repo == nil {
			item.Message = "target repo not found"
			rep.Items = append(rep.Items, item)
			continue
		}
		// 比对的是**源端 vs 目标端**（这才是"漂移"）。
		//
		// 原来比的是"工作区本地分支 vs 源端远端" ⇒ 报出来的多半是"源端前进了"
		// —— 那是正常状态(下次同步就推上去了),不是漂移;
		// 而"目标端跑偏了"从来没被检查过。
		//
		// 用**专用工作目录**(不复用 executor 的 workdir):检测时的 fetch 会写
		// git 锁,和正在跑的同步抢同一份目录会互相报错。
		gitDir, derr := s.prepareDriftRepo(ctx, t, repo)
		if derr != nil {
			item.Message = "cannot prepare drift repo: " + derr.Error()
			rep.Items = append(rep.Items, item)
			continue
		}
		// 带凭证取两端状态(私有仓/自签证书必须带凭证;
		// 原来走 git ls-remote,而平台的 RunRaw 固定用空凭证 ⇒ 私有仓永远
		// "cannot resolve refs",漂移检测形同虚设)。
		srcRef, tgtRef, ferr := s.fetchDriftBothSides(ctx, gitDir, t, repo)
		if ferr != nil {
			item.Message = "cannot fetch both sides: " + ferr.Error()
			rep.Items = append(rep.Items, item)
			continue
		}
		item.LocalRef = srcRef
		item.RemoteRef = tgtRef
		switch {
		case srcRef == "" || tgtRef == "":
			item.Message = "cannot resolve refs"
		case srcRef == tgtRef:
			item.Message = "in sync"
		default:
			item.Drifted = true
			// rev-list --count A..B 要求两个 sha 都在本地对象库里;
			// 两端都 fetch 过了 ⇒ 算得出来。
			a, b, ok := revListCounts(ctx, gitDir, srcRef, tgtRef)
			item.LocalAhead, item.RemoteAhead = a, b
			switch {
			case !ok:
				// fetch 过了还算不出 ⇒ 真的算不出来(罕见),明说而不是给 0/0
				item.Message = "drifted (ahead/behind not resolvable after fetch)"
			case a > 0 && b > 0:
				item.Message = "diverged (both source and target have unique commits)"
			case a > 0:
				item.Message = "source ahead (target is behind; next sync will push)"
			case b > 0:
				item.Message = "target ahead (target has commits the source does not)"
			}
			rep.Drifted++
		}
		rep.Checked++
		rep.Items = append(rep.Items, item)
	}
	return rep, nil
}

// prepareDriftRepo 准备该任务的**漂移检测专用仓库**(克隆源端 + 配置 target 远端)。
//
// 为什么不用 executor 的 workdir:检测要 fetch,而 fetch 会写 git 锁 ——
// 与正在跑的同步抢同一份目录会互相报错("cannot lock ref")。
// 专用目录第一次克隆、之后增量 fetch。
func (s *Service) prepareDriftRepo(ctx context.Context, t *model.SyncTask, dstRepo *model.Repo) (string, error) {
	backend := lifecycleBackend()
	if backend == nil {
		return "", errors.New("git backend 不可用")
	}
	srcRepo, err := s.GetRepoByKey(t.SourceRepoKey)
	if err != nil || srcRepo == nil {
		return "", errors.New("源仓记录不存在")
	}
	var srcPlat *model.Platform
	if srcRepo.PlatformID > 0 {
		if got, gerr := s.GetPlatformByID(ctx, srcRepo.PlatformID); gerr == nil {
			srcPlat = got
		}
	}
	base := s.GetConfig().Git.TempDir
	if base == "" {
		return "", errors.New("未配置 git.temp_dir")
	}
	dir := filepath.Join(base, "drift", t.Key, "repo")
	// **仓库必须可用,不能只看 .git 在不在**:
	// git clone 会先建好 .git 再取对象,被预算/超时杀掉后目录残留 .git ⇒
	// 下次只做 os.Stat 就认定"已存在"⇒ 跳过 clone ⇒ 后续 fetch 全失败
	// ⇒ **该任务的漂移检测被永久打瞎**(而报告里 Checked 不增,看着像"没问题")。
	if !driftRepoUsable(ctx, backend, dir) {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("漂移仓库清理失败,重建", "dir", dir, "error", err)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return "", err
		}
		if err := backend.Clone(ctx, gitbackend.CloneOptions{
			URL:  srcRepo.CloneURL,
			Path: dir,
			Auth: executor.BuildRepoAuth(srcRepo, srcPlat),
		}); err != nil {
			return "", err
		}
	}
	// target 远端
	remotes, rerr := backend.GetRemotes(ctx, dir)
	if rerr != nil {
		return "", rerr
	}
	hasTarget := false
	for _, n := range remotes {
		if n == executor.RemoteTarget {
			hasTarget = true
			break
		}
	}
	if !hasTarget {
		if err := backend.AddRemote(ctx, dir, executor.RemoteTarget, dstRepo.CloneURL); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// fetchDriftBothSides 带凭证取**源端与目标端**的分支状态,返回两边的 SHA。
func (s *Service) fetchDriftBothSides(ctx context.Context, dir string, t *model.SyncTask, dstRepo *model.Repo) (string, string, error) {
	backend := lifecycleBackend()
	if backend == nil {
		return "", "", errors.New("git backend 不可用")
	}
	srcRepo, err := s.GetRepoByKey(t.SourceRepoKey)
	if err != nil || srcRepo == nil {
		return "", "", errors.New("源仓记录不存在")
	}
	var srcPlat, dstPlat *model.Platform
	if srcRepo.PlatformID > 0 {
		if got, gerr := s.GetPlatformByID(ctx, srcRepo.PlatformID); gerr == nil {
			srcPlat = got
		}
	}
	if dstRepo.PlatformID > 0 {
		if got, gerr := s.GetPlatformByID(ctx, dstRepo.PlatformID); gerr == nil {
			dstPlat = got
		}
	}
	// **glob 不能直接当 fetch 的分支参数**:"release/*" 不是合法 refspec
	// (git: invalid refspec)⇒ fetch 报错 ⇒ 多分支任务的漂移检测**永远失败**,
	// 而且失败被算作"未漂移"(Checked/Drifted 都不增,报告里与"已检查无漂移"无法区分)。
	// 这里对 glob 改成全量取回,再从远端跟踪 ref 里挑出匹配的分支。
	srcBranches, srcGlob := driftFetchBranches(t.SourceBranch)
	tgtBranches, tgtGlob := driftFetchBranches(t.TargetBranch)
	if _, ferr := backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   executor.RemoteOrigin,
		Branches: srcBranches,
		Auth:     executor.BuildRepoAuth(srcRepo, srcPlat),
	}); ferr != nil {
		return "", "", ferr
	}
	if _, ferr := backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   executor.RemoteTarget,
		Branches: tgtBranches,
		Auth:     executor.BuildRepoAuth(dstRepo, dstPlat),
	}); ferr != nil {
		return "", "", ferr
	}
	srcRef := driftRef(ctx, dir, executor.RemoteOrigin, t.SourceBranch, srcGlob)
	tgtRef := driftRef(ctx, dir, executor.RemoteTarget, t.TargetBranch, tgtGlob)
	return srcRef, tgtRef, nil
}

// driftFetchBranches glob 模式 ⇒ 全量取回(nil);否则取该分支。
func driftFetchBranches(branch string) ([]string, bool) {
	if strings.ContainsAny(branch, "*?[") {
		return nil, true
	}
	return []string{branch}, false
}

// driftRef 取远端跟踪 ref 的 SHA;glob 模式下取**匹配到的最新一个**。
func driftRef(ctx context.Context, dir, remote, branch string, isGlob bool) string {
	if !isGlob {
		return gitRevParse(ctx, dir, "refs/remotes/"+remote+"/"+branch)
	}
	// glob:列出远端跟踪 ref,挑更新时间最新的那个作为代表
	out, err := runGitQuiet(ctx, dir, "for-each-ref", "--format=%(refname:short) %(committerdate:unix)",
		"refs/remotes/"+remote+"/")
	if err != nil {
		return ""
	}
	best, bestTS := "", int64(-1)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[0], remote+"/")
		if !pathx.MatchGlob(branch, name) {
			continue
		}
		var ts int64
		if _, serr := fmt.Sscanf(fields[1], "%d", &ts); serr != nil {
			continue
		}
		if ts > bestTS {
			best, bestTS = name, ts
		}
	}
	if best == "" {
		return ""
	}
	return gitRevParse(ctx, dir, "refs/remotes/"+remote+"/"+best)
}

// driftRepoUsable 漂移仓库是否可用(不只是 .git 在不在)。
func driftRepoUsable(ctx context.Context, backend gitbackend.GitBackend, dir string) bool {
	if _, serr := os.Stat(filepath.Join(dir, ".git")); serr != nil {
		return false
	}
	if _, rerr := backend.RevParse(ctx, dir, "HEAD"); rerr != nil {
		return false
	}
	return true
}

// ForcePushPolicy 强制推送保护策略。
type ForcePushPolicy string

const (
	// ForcePushAllow 允许强制覆盖(原 keep_divergent=false 行为)
	ForcePushAllow ForcePushPolicy = "allow"
	// ForcePushBlock 遇分歧直接失败,不覆盖
	ForcePushBlock ForcePushPolicy = "block"
	// ForcePushBackupOnDemand 覆盖前自动打 bundle 快照(可回滚)
	ForcePushBackupOnDemand ForcePushPolicy = "backup_on_demand"
)

// ResolveForcePushPolicy 从任务配置解析策略。
// 优先 task.ForcePushPolicy;否则按 keep_divergent 兼容映射。
func ResolveForcePushPolicy(t *model.SyncTask) ForcePushPolicy {
	if t == nil {
		return ForcePushBlock
	}
	if t.ForcePushPolicy != "" {
		return ForcePushPolicy(t.ForcePushPolicy)
	}
	if t.KeepDivergent {
		return ForcePushBlock
	}
	return ForcePushAllow
}

// ValidForcePushPolicy 校验策略字符串。
func ValidForcePushPolicy(p string) bool {
	switch ForcePushPolicy(p) {
	case ForcePushAllow, ForcePushBlock, ForcePushBackupOnDemand:
		return true
	}
	return false
}

// gitRevParse 解析引用为完整 SHA;任何失败(ref 不存在是正常分支、真错误同理)
// 都回落空串 —— 与历史行为一致(此处并未用 IsNotFound 区分,调用方只关心
// “能不能解析出 SHA”)。
func gitRevParse(ctx context.Context, dir, ref string) string {
	backend := lifecycleBackend()
	if backend == nil {
		return ""
	}
	sha, err := backend.RevParse(ctx, dir, ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sha)
}

func gitLsRemote(ctx context.Context, dir, remote, ref string) string {
	out, err := runGitQuiet(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) >= 2 {
		return fields[0]
	}
	return ""
}

// fetchDriftSource 把源端分支 fetch 到工作区,让 ahead/behind 能算出来。
//
// 为什么要 fetch:rev-list --count <shaA>..<shaB> 要求两个 sha 都在本地对象库里;
// 远端(ls-remote 拿到的)那个 sha 若没被 fetch 过,命令直接失败 ⇒
// ahead/behind 只能报"未知"。
//
// 必须带凭证:源仓可能是私有仓,裸 git fetch 会 401。
func (s *Service) fetchDriftSource(ctx context.Context, gitDir string, t *model.SyncTask) error {
	backend := lifecycleBackend()
	if backend == nil {
		return errors.New("git backend 不可用")
	}
	repo, err := s.GetRepoByKey(t.SourceRepoKey)
	if err != nil || repo == nil {
		return errors.New("源仓记录不存在")
	}
	var p *model.Platform
	if repo.PlatformID > 0 {
		if got, gerr := s.GetPlatformByID(ctx, repo.PlatformID); gerr == nil {
			p = got
		}
	}
	_, err = backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: gitDir,
		Remote:   "origin",
		Branches: []string{t.SourceBranch},
		Auth:     executor.BuildRepoAuth(repo, p),
	})
	return err
}

// revListCounts 统计 a/b 两端各自独有的提交数。
// 调用方传入的是裸 SHA(a=本地 ref 解析结果,b=ls-remote 的远端 SHA,
// 后者未必等于本地的 refs/remotes/* 跟踪引用),GetBranchSyncInfo 只接受
// refs/heads/<branch> + refs/remotes/<upstream> 的组合、语义对不上,
// 因此保留原 `rev-list --count a..b` 命令,经 RunRaw 收口执行。
// 第三个返回值 ok = 两侧的数量都算出来了。
// 远端 SHA 来自 ls-remote,若那个提交没被 fetch 进本地对象库,rev-list 直接失败;
// 此时不能返回 0/0 —— 那会把"算不出来"说成"没有差异"。
func revListCounts(ctx context.Context, dir, a, b string) (aAhead, bAhead int, ok bool) {
	out1, err1 := runGitQuiet(ctx, dir, "rev-list", "--count", a+".."+b)
	out2, err2 := runGitQuiet(ctx, dir, "rev-list", "--count", b+".."+a)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(out1), "%d", &bAhead); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(out2), "%d", &aAhead); err != nil {
		return 0, 0, false
	}
	return aAhead, bAhead, true
}

// runGitQuiet 只读 git 命令。优先走 backend.RunRaw(与 executor 侧同一收口),
// 平台白名单之外的子命令回落裸 git;返回值与历史实现一致(stdout/stderr 合并
// 后原样返回,不额外包装错误)。
func runGitQuiet(ctx context.Context, dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("empty git args")
	}
	backend := lifecycleBackend()
	if native, ok := backend.(*gitbackend.NativeGitBackend); ok && native != nil {
		stdout, stderr, err := native.RunRaw(ctx, dir, args)
		switch {
		case err == nil:
			return stdout, nil
		case errors.Is(err, gitbackend.ErrInvalidGitArg):
			// 白名单之外 → 回落裸 git
		default:
			return stdout + stderr, err
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // 内部构造
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var (
	lifeBackendOnce sync.Once
	lifeBackend     gitbackend.GitBackend
)

// lifecycleBackend 漂移检测用的 git 后端,与 MirrorService 同款 Options{} 自动选择。
func lifecycleBackend() gitbackend.GitBackend {
	lifeBackendOnce.Do(func() {
		b, err := gitbackend.NewGitBackend(gitbackend.Options{})
		if err != nil {
			slog.Warn("init git backend for lifecycle checks failed", "error", err)
			return
		}
		lifeBackend = b
	})
	return lifeBackend
}
