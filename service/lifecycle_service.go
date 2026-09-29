package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
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
	prov, err := platformProvider(s.platforms.providerMgr, plat)
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
}

// DriftReport 漂移检测汇总。
type DriftReport struct {
	Generated time.Time   `json:"generated"`
	Checked   int         `json:"checked"`
	Drifted   int         `json:"drifted"`
	Items     []DriftItem `json:"items"`
	Warnings  []string    `json:"warnings,omitempty"`
}

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

	for _, t := range tasks {
		item := DriftItem{TaskKey: t.Key, Branch: t.TargetBranch, RepoKey: t.TargetRepoKey}
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
		localRef := gitRevParse(ctx, gitDir, "refs/heads/"+t.TargetBranch)
		item.LocalRef = localRef
		remoteRef := gitLsRemote(ctx, gitDir, "origin", "refs/heads/"+t.TargetBranch)
		item.RemoteRef = remoteRef
		switch {
		case localRef == "" || remoteRef == "":
			item.Message = "cannot resolve refs"
		case localRef == remoteRef:
			item.Message = "in sync"
		default:
			item.Drifted = true
			item.Message = "diverged"
			item.LocalAhead, item.RemoteAhead = revListCounts(ctx, gitDir, localRef, remoteRef)
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

func gitRevParse(ctx context.Context, dir, ref string) string {
	out, err := runGitQuiet(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
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

func revListCounts(ctx context.Context, dir, a, b string) (aAhead, bAhead int) {
	if out, err := runGitQuiet(ctx, dir, "rev-list", "--count", a+".."+b); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &bAhead)
	}
	if out, err := runGitQuiet(ctx, dir, "rev-list", "--count", b+".."+a); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &aAhead)
	}
	return
}

func runGitQuiet(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // 内部构造
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
