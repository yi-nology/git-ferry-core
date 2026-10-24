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

// DetectDrift 检测本地 workdir 与目标远端的分支漂移。
// 用于发现「镜像声称同步成功但 refs 实际不一致」的静默故障。
// taskKeys 为空时扫描全部启用任务。
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
		list, _, err := s.ListTasks(ctx, "", 0, 200)
		if err != nil {
			return nil, err
		}
		for _, t := range list {
			if t.Enabled {
				tasks = append(tasks, t)
			}
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
		workDir := s.GetTempDir(t.Key)
		gitDir := filepath.Join(workDir, "repo")
		if _, serr := os.Stat(gitDir); serr != nil {
			item.Message = "workdir missing (not synced yet)"
			rep.Items = append(rep.Items, item)
			continue
		}
		// 比对的是**源**分支:工作区里的本地分支就是克隆源端时检出的那个分支
		// (executor.cloneRepo 按 task.SourceBranch 克隆)⇒ 用 TargetBranch 去找
		// refs/heads/<target> 在源/目标分支不同的任务上必然找不到("cannot resolve refs")。
		// origin 远端同样是源端(工作区的 "target" 才是目标仓)。
		localRef := gitRevParse(ctx, gitDir, "refs/heads/"+t.SourceBranch)
		item.LocalRef = localRef
		remoteRef := gitLsRemote(ctx, gitDir, "origin", "refs/heads/"+t.SourceBranch)
		item.RemoteRef = remoteRef
		switch {
		case localRef == "" || remoteRef == "":
			item.Message = "cannot resolve refs"
		case localRef == remoteRef:
			item.Message = "in sync"
		default:
			item.Drifted = true
			// rev-list --count A..B 要求两个 sha 都在本地对象库里;
			// 远端(ls-remote 拿到的)那个 sha 没被 fetch 过 ⇒ 命令直接失败,
			// ahead/behind 只能报"未知"。先把源端分支 fetch 下来(带凭证,私有仓必须),
			// 再算 —— 否则"漂移了多少"这个最该有的信息永远是空的。
			if ferr := s.fetchDriftSource(ctx, gitDir, t); ferr != nil {
				item.Message = "drifted (cannot fetch source branch to compute ahead/behind: " + ferr.Error() + ")"
				rep.Drifted++
				rep.Items = append(rep.Items, item)
				continue
			}
			a, b, ok := revListCounts(ctx, gitDir, localRef, remoteRef)
			item.LocalAhead, item.RemoteAhead = a, b
			switch {
			case !ok:
				// fetch 过了还算不出 ⇒ 真的算不出来(罕见),明说而不是给 0/0
				item.Message = "drifted (ahead/behind not resolvable after fetch)"
			case a > 0 && b > 0:
				item.Message = "diverged (both sides have unique commits)"
			case a > 0:
				item.Message = "local ahead (workdir has commits the source does not)"
			case b > 0:
				item.Message = "source ahead (workdir is behind; next sync will push)"
			}
			rep.Drifted++
		}
		rep.Checked++
		rep.Items = append(rep.Items, item)
	}
	return rep, nil
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
