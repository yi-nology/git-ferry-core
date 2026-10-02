package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// ===== 组织导入 / 组织镜像编排 =====
//
// 壳 biz/handler/git_sync/{org_import,org_mirror}_service.go 的编排循环下沉：
// 列举平台仓库、按组织过滤、owner 映射、批量建仓/建任务与统计。
// HTTP 绑定、平台存在性校验文案、审计与响应包装留在壳层。

// OrgRepoItem 公开组织仓预览项。
type OrgRepoItem struct {
	FullName string
	CloneURL string
	Fork     bool
	Archived bool
	Stars    int
}

// PublicOrgImportRequest 公开组织仓导入请求（入参已由壳层校验/归一化）。
type PublicOrgImportRequest struct {
	PlatformKey    string
	Org            string
	CreateTasks    bool
	TargetPlatform string
	TargetOrg      string
	Filter         *RepoImportFilter
	DryRun         bool
	Max            int
}

// PublicOrgImportResult 导入结果。
type PublicOrgImportResult struct {
	Items        []OrgRepoItem
	Warnings     []string
	Imported     int
	CreatedTasks int
}

// maxPublicOrgPages 平台忽略 page 参数时的安全阀（沿用壳层既定值）。
const maxPublicOrgPages = 100

// taskScanLimit 批量建任务时扫描的仓库上限。
const taskScanLimit = 200

// ImportPublicOrg 列出组织公开仓并可选建同步任务（默认只导入仓库记录）。
//
// 顺序与历史壳层一致：列仓（仅 GitHub）→ 导入仓库记录 → 批量建任务；
// DryRun 时跳过后两步（由壳层追加 dry_run 警告）。
func (s *Service) ImportPublicOrg(ctx context.Context, req PublicOrgImportRequest) (*PublicOrgImportResult, error) {
	plat, err := s.GetPlatform(ctx, req.PlatformKey)
	if err != nil || plat == nil {
		return nil, ErrPlatformNotFound
	}

	res := &PublicOrgImportResult{Items: []OrgRepoItem{}, Warnings: []string{}}

	// GitHub 走列仓+客户端过滤公开仓；其它平台回落全量导入
	if plat.Type == model.PlatformTypeGitHub {
		items, warnings, lerr := listPublicOrgRepos(ctx, s, plat, req.Org, req.Max)
		if lerr != nil {
			return nil, lerr
		}
		res.Items = items
		res.Warnings = append(res.Warnings, warnings...)
	}

	if !req.DryRun {
		n, ierr := s.SyncPlatformReposFiltered(ctx, req.PlatformKey, req.Filter)
		if ierr != nil {
			return nil, ierr
		}
		res.Imported = n
	}

	if !req.DryRun && req.CreateTasks {
		res.CreatedTasks = s.createOrgMirrorTasks(ctx, req)
	}
	return res, nil
}

// listPublicOrgRepos 分页拉取组织公开仓（RepoManager.ListRepos(Owner=org) 无
// type=public 参数，客户端过滤私有仓；拉到 max 条为止）。
func listPublicOrgRepos(ctx context.Context, s *Service, plat *model.Platform, org string, max int) ([]OrgRepoItem, []string, error) {
	prov, err := s.ProviderForPlatform(plat, "")
	if err != nil {
		return nil, nil, fmt.Errorf("create provider: %w", err)
	}
	perPage := sdkprov.MaxPerPage
	maxPages := maxPublicOrgPages
	publics := []*sdkprov.PlatformRepo{}
	for page := 1; len(publics) < max && page <= maxPages; page++ {
		batch, lerr := prov.ListRepos(ctx, sdkprov.ListRepoOptions{Owner: org, Page: page, PerPage: perPage})
		if lerr != nil {
			return nil, nil, lerr
		}
		for _, r := range batch {
			if r.Private {
				continue
			}
			publics = append(publics, r)
		}
		if len(batch) < perPage {
			break
		}
	}
	if len(publics) > max {
		publics = publics[:max]
	}
	items := make([]OrgRepoItem, 0, len(publics))
	for _, r := range publics {
		items = append(items, OrgRepoItem{
			FullName: r.FullName, CloneURL: r.CloneURL,
			Fork: r.Fork, Archived: r.Archived, Stars: r.Stars,
		})
	}
	warnings := []string{"按平台凭证列取组织公开仓；大组织可能只拉到部分元数据"}
	return items, warnings, nil
}

// createOrgMirrorTasks 扫描已导入仓库，按组织过滤后批量建同步任务。
// 扫描失败静默跳过（与历史壳层一致）。
func (s *Service) createOrgMirrorTasks(ctx context.Context, req PublicOrgImportRequest) int {
	repos, _, lerr := s.ListRepos(ctx, 0, taskScanLimit)
	if lerr != nil {
		return 0
	}
	created := 0
	for _, r := range repos {
		if !strings.Contains(r.Key, "/"+req.Org+"/") && !strings.HasPrefix(r.Key, req.PlatformKey+"/"+req.Org+"/") {
			continue
		}
		target := r.Key
		if req.TargetOrg != "" {
			if out, merr := ResolveOrgTarget("", "", OrgMapOptions{
				Strategy: OrgMapSingle, TargetOrg: req.TargetOrg,
				RequireTarget: true, SourceKey: r.Key, TargetPlatform: req.TargetPlatform,
			}); merr == nil {
				target = out.Key
				if target == "" {
					target = out.Owner + "/" + out.Repo
				}
			}
		}
		name := r.Name + "-mirror"
		if _, cerr := s.CreateTask(ctx, &model.CreateTaskRequest{
			Name:          name,
			SourceRepoKey: r.Key,
			SourceBranch:  "*",
			TargetRepoKey: target,
			TargetBranch:  "*",
			SyncMode:      "all",
		}); cerr == nil {
			created++
		}
	}
	return created
}

// ===== 组织镜像 =====

// BulkMirrorRequest 组织镜像请求（平台存在性校验由壳层完成并保留其文案）。
type BulkMirrorRequest struct {
	SourcePlatformKey string
	SourceOrg         string
	// TargetPlatform 已校验存在的目标平台（建仓用 ID / InstanceURL）。
	TargetPlatform *model.Platform
	Strategy       OrgMapStrategy
	TargetOrg      string
	TargetUser     string
	ImportNew      bool
	CreateTasks    bool
	DryRun         bool
}

// BulkMirrorItem 单仓映射结果。Action: planned|imported|task_created|skipped|failed
type BulkMirrorItem struct {
	Source  string
	Target  string
	Action  string
	Message string
}

// BulkMirrorResult 组织镜像结果。
type BulkMirrorResult struct {
	Planned      int
	Imported     int
	TasksCreated int
	Items        []*BulkMirrorItem
	Warnings     []string
}

// BulkMirrorOrg 按策略把源组织仓库映射到目标命名空间，并可选建同步任务。
// 顺序：（可选）同步源平台仓库 → 过滤源组织 → 映射 → 建仓 → 建任务。
func (s *Service) BulkMirrorOrg(ctx context.Context, req BulkMirrorRequest) (*BulkMirrorResult, error) {
	// 1) 导入源平台仓库（可选）
	if req.ImportNew {
		_, _ = s.SyncPlatformReposFiltered(ctx, req.SourcePlatformKey, &RepoImportFilter{
			ExcludeArchived: true,
		})
	}

	// 2) 过滤出源 org 下的仓库
	all, err := s.ListReposByPlatform(ctx, req.SourcePlatformKey)
	if err != nil {
		return nil, err
	}

	res := &BulkMirrorResult{Items: []*BulkMirrorItem{}, Warnings: []string{}}
	for _, r := range all {
		if !strings.EqualFold(r.PlatformOwner, req.SourceOrg) {
			continue
		}
		srcName := r.PlatformOwner + "/" + r.PlatformRepo
		tOwner, tRepo := mirrorOrgTarget(req.Strategy, r.PlatformOwner, r.PlatformRepo, req.TargetOrg, req.TargetUser)
		tName := tOwner + "/" + tRepo
		item := &BulkMirrorItem{Source: srcName, Target: tName}
		res.Planned++

		if req.DryRun {
			item.Action = "planned"
			res.Items = append(res.Items, item)
			continue
		}

		// 3) 目标仓登记（按 clone URL；已存在则跳过创建）
		tURL := RewriteRepoURL(r.CloneURL, tOwner, tRepo)
		if tURL == "" {
			tURL = RewriteRepoURL(req.TargetPlatform.InstanceURL, tOwner, tRepo)
		}
		tRepoRec, terr := s.CreateRepo(ctx, &model.CreateRepoRequest{
			Name:        tName,
			RemoteURL:   tURL,
			PlatformID:  req.TargetPlatform.ID,
			AccessToken: r.AccessToken,
		})
		if terr != nil {
			// 已存在等错误：尝试按名称找
			item.Action = "skipped"
			item.Message = terr.Error()
			res.Items = append(res.Items, item)
			res.Warnings = append(res.Warnings, tName+": "+terr.Error())
			continue
		}
		res.Imported++

		if !req.CreateTasks {
			item.Action = "imported"
			res.Items = append(res.Items, item)
			continue
		}

		// 4) 建同步任务
		srcKey := r.Key
		if srcKey == "" {
			item.Action = "failed"
			item.Message = "source repo key empty"
			res.Items = append(res.Items, item)
			continue
		}
		taskName := "org-mirror-" + tRepo
		_, terr = s.CreateTask(ctx, &model.CreateTaskRequest{
			Name:          taskName,
			SourceRepoKey: srcKey,
			SourceBranch:  "*",
			TargetRepoKey: tRepoRec.Key,
			TargetBranch:  "*",
			SyncMode:      "all",
			GitTags:       true,
		})
		if terr != nil {
			item.Action = "failed"
			item.Message = terr.Error()
			res.Warnings = append(res.Warnings, taskName+": "+terr.Error())
		} else {
			item.Action = "task_created"
			res.TasksCreated++
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}

// mirrorOrgTarget 组织镜像语义的目标 owner/repo：mixed 组织仓落 target_org、
// 缺落点回落源 owner（等价于 OrgMapOptions{MixedOrgToTarget:true} + IsPersonalOwner）。
func mirrorOrgTarget(strategy OrgMapStrategy, sourceOwner, sourceRepo, targetOrg, targetUser string) (owner, repo string) {
	res, err := ResolveOrgTarget(sourceOwner, sourceRepo, OrgMapOptions{
		Strategy:         strategy,
		TargetOrg:        targetOrg,
		TargetUser:       targetUser,
		SourceIsPersonal: IsPersonalOwner(sourceOwner, targetUser, nil),
		MixedOrgToTarget: true,
	})
	if err != nil {
		return sourceOwner, sourceRepo
	}
	return res.Owner, res.Repo
}

// RewriteRepoURL 把 clone URL 中的 owner/repo 换成目标（支持 https 与 ssh）。
func RewriteRepoURL(raw, owner, repo string) string {
	if raw == "" || owner == "" || repo == "" {
		return raw
	}
	// https://host/old/oldrepo.git
	if i := strings.Index(raw, "://"); i > 0 {
		rest := raw[i+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return raw
		}
		prefix := raw[:i+3+slash+1]
		return prefix + owner + "/" + repo + ".git"
	}
	// git@host:old/oldrepo.git
	if i := strings.Index(raw, ":"); i > 0 && strings.Contains(raw[:i], "@") {
		return raw[:i+1] + owner + "/" + repo + ".git"
	}
	return raw
}

// ===== Starred 列举 =====

// ErrStarredUnsupported 平台不支持 starred 列举（壳层回 400，文案为对外契约）。
// 以哨兵形式放 errors.go 归类，此处仅注释说明归属。

// ListStarredRepos 拉取平台 starred 仓库（能力门控 + 分页拉到 max 条）。
func (s *Service) ListStarredRepos(ctx context.Context, platformKey string, max int) ([]*sdkprov.PlatformRepo, error) {
	plat, err := s.GetPlatform(ctx, platformKey)
	if err != nil || plat == nil {
		return nil, ErrPlatformNotFound
	}
	prov, perr := s.ProviderForPlatform(plat, "")
	if perr != nil {
		return nil, fmt.Errorf("create provider: %w", perr)
	}
	if !prov.Capabilities().Starred {
		return nil, ErrStarredUnsupported
	}
	sm := prov.(sdkprov.StarredManager)

	// 分页拉到 max 条为止(页不足一页即末页)。
	perPage := sdkprov.MaxPerPage
	starred := []*sdkprov.PlatformRepo{}
	for page := 1; len(starred) < max; page++ {
		batch, serr := sm.ListStarred(ctx, page, perPage)
		if serr != nil {
			return nil, serr
		}
		starred = append(starred, batch...)
		if len(batch) < perPage {
			break
		}
	}
	if len(starred) > max {
		starred = starred[:max]
	}
	return starred, nil
}
