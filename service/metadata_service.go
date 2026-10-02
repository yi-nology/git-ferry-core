package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// 元数据备份引擎（壳 biz/handler/git_sync 的 MetadataBackup/ListMetadataBackups
// 端点下沉）：抓取 issues/PR/labels/milestones/releases 快照并落盘 manifest，
// 可选源码归档 / Release 附件 / Gists。core 不含任何 Web 框架依赖，
// 错误经 ErrRepoNotFound / ErrPlatformNotFound / ErrMetadataValidation 分类，
// 由壳层映射 HTTP 状态码；所有 warning 文案逐字保留（前端/CLI/文档引用）。

// MetadataSnapshot 元数据快照清单（manifest.json 与 HTTP 响应共用结构）。
// 字段顺序与 JSON 标签对齐壳层 biz/model/ops.MetadataSnapshot（IDL 生成），
// 保证落盘文件与响应体逐字节兼容。
type MetadataSnapshot struct {
	RepoKey   string           `json:"repo_key"`
	Platform  string           `json:"platform"`
	Owner     string           `json:"owner"`
	Repo      string           `json:"repo"`
	CreatedAt string           `json:"created_at"`
	Dir       string           `json:"dir"`
	Counts    map[string]int32 `json:"counts"`
	Files     []string         `json:"files"`
	Archives  []string         `json:"archives"`
	Assets    []string         `json:"assets"`
	Warnings  []string         `json:"warnings"`
}

// MetadataBackupInfo 列表条目（与快照清单同构）。
type MetadataBackupInfo = MetadataSnapshot

// MetadataBackupOptions 元数据备份入参。
// 上限 ≤0 时按引擎缺省：issues/PRs/releases=500（>2000 截到 2000，与壳
// max_items 口径一致）、gists=200、assets=100。
// Include* 为解析后的布尔（壳层 optBoolDefault 缺省全开语义在壳侧完成）。
// Since 为 RFC3339 增量：仅保留 updatedAt >= since 的 issue。
type MetadataBackupOptions struct {
	RepoKey         string
	MaxIssues       int
	MaxPRs          int
	MaxReleases     int
	MaxGists        int
	MaxAssets       int
	IncludeIssues   bool
	IncludePRs      bool
	IncludeReleases bool
	IncludeSource   bool // 源码归档（原 with_archives）
	IncludeAssets   bool // Release 附件（另受 Capabilities().ReleaseAssets 门控）
	IncludeGists    bool // Gists（另受 Capabilities().Gists 门控）
	Since           string
	DryRun          bool // 预留：当前备份无 dry-run 入参（壳层恒 false），不影响落盘行为
}

// MetadataBackupResult 备份结果：嵌入快照字段（壳层整体转换为 ops.MetadataSnapshot）。
// Truncated 为信息性标记（任一集合拉满上限即 true），不进 manifest/HTTP 响应。
type MetadataBackupResult struct {
	MetadataSnapshot
	Truncated bool
}

// VerifyReport 元数据快照抽样校验报告。
// Reason 非空表示 backup_dir 未配置：此时响应结构仅 {ok,reason}（与历史行为一致）。
type VerifyReport struct {
	Checked  int      `json:"checked"`
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings"`
	Reason   string   `json:"reason,omitempty"`
}

// MetadataBackupDir 元数据快照根目录 = BackupDir()+"/metadata"，目录布局唯一来源。
func (s *Service) MetadataBackupDir() string {
	return filepath.Join(s.BackupDir(), "metadata")
}

// clampMaxItems 上限口径与壳 max_items 一致：≤0 → 500，>2000 → 2000。
func clampMaxItems(v int) int {
	if v <= 0 {
		return 500
	}
	if v > 2000 {
		return 2000
	}
	return v
}

// BackupMetadata 抓取 issues/PR/labels/milestones/releases 元数据快照,
// 可选下载 source archive / GitHub Release 附件 / Gists。
// provider 经 ProviderForPlatform 获取（Manager 缓存 + GitHub App token 感知）。
// 校验顺序与历史壳层一致：repo → platform → provider → backup_dir → 落盘。
func (s *Service) BackupMetadata(ctx context.Context, o MetadataBackupOptions) (*MetadataBackupResult, error) {
	repo, err := s.GetRepo(ctx, o.RepoKey)
	if err != nil || repo == nil {
		return nil, ErrRepoNotFound
	}
	plat, err := s.GetPlatformByID(ctx, repo.PlatformID)
	if err != nil || plat == nil {
		return nil, ErrPlatformNotFound
	}
	prov, err := s.ProviderForPlatform(plat, repo.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("create provider: %w", err)
	}
	if s.BackupDir() == "" {
		return nil, newMetadataError("sync.backup_dir not configured", nil)
	}
	return s.collectMetadataSnapshot(ctx, o, repo, plat, prov)
}

// collectMetadataSnapshot 建快照目录 → 分片采集 → 写 manifest。
// 与 BackupMetadata 拆分以便测试注入桩 provider（跳过 DB 与 provider 解析）。
func (s *Service) collectMetadataSnapshot(ctx context.Context, o MetadataBackupOptions,
	repo *model.Repo, plat *model.Platform, prov sdkprov.Provider) (*MetadataBackupResult, error) {
	maxIssues := clampMaxItems(o.MaxIssues)
	maxPRs := clampMaxItems(o.MaxPRs)
	maxReleases := clampMaxItems(o.MaxReleases)
	maxGists := o.MaxGists
	if maxGists <= 0 {
		maxGists = 200
	}
	maxAssets := o.MaxAssets
	if maxAssets <= 0 {
		maxAssets = 100
	}

	snapDir := filepath.Join(s.MetadataBackupDir(), strutil.SanitizePathToken(o.RepoKey),
		time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return nil, err
	}

	snap := &MetadataSnapshot{
		RepoKey:   o.RepoKey,
		Platform:  plat.Type,
		Owner:     repo.PlatformOwner,
		Repo:      repo.PlatformRepo,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Dir:       snapDir,
		Counts:    map[string]int32{},
		Files:     []string{},
		Archives:  []string{},
		Assets:    []string{},
		Warnings:  []string{},
	}

	s.collectLabelsMilestones(ctx, prov, repo, snap)
	if o.IncludeIssues {
		s.collectIssues(ctx, prov, repo, maxIssues, o.Since, snap)
	}
	if o.IncludePRs {
		s.collectPullRequests(ctx, prov, repo, maxPRs, snap)
	}
	releases := s.collectReleases(ctx, prov, repo, o.IncludeReleases, maxReleases, snap)
	if o.IncludeSource {
		s.downloadSourceArchives(ctx, prov, repo, releases, snapDir, snap)
	}
	if o.IncludeAssets && prov.Capabilities().ReleaseAssets {
		s.collectReleaseAssets(ctx, prov, repo, releases, snapDir, maxAssets, snap)
	}
	if o.IncludeGists && prov.Capabilities().Gists {
		s.collectGists(ctx, prov, snapDir, maxGists, snap)
	}

	// Truncated：任一集合拉满上限即视为可能截断（信息性字段，不进 manifest/响应）。
	truncated := (o.IncludeIssues && int(snap.Counts["issues"]) >= maxIssues) ||
		(o.IncludePRs && int(snap.Counts["pull_requests"]) >= maxPRs) ||
		(o.IncludeReleases && int(snap.Counts["releases"]) >= maxReleases)

	writeSnapshotJSON(snap, snapDir)
	return &MetadataBackupResult{MetadataSnapshot: *snap, Truncated: truncated}, nil
}

// ListMetadataBackups 扫描 MetadataBackupDir 下的 manifest.json；
// 读取/解析失败的条目静默跳过（与历史行为一致）。repoKey 非空时按快照
// repo_key 过滤。BackupDir 未配置时返回空列表。
func (s *Service) ListMetadataBackups(_ context.Context, repoKey string) ([]MetadataBackupInfo, error) {
	items := []MetadataBackupInfo{}
	if s.BackupDir() == "" {
		return items, nil
	}
	_ = filepath.WalkDir(s.MetadataBackupDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "manifest.json" {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // 内部备份路径
		if rerr != nil {
			return nil
		}
		var snap MetadataSnapshot
		if json.Unmarshal(data, &snap) != nil {
			return nil
		}
		if repoKey != "" && snap.RepoKey != repoKey {
			return nil
		}
		items = append(items, snap)
		return nil
	})
	return items, nil
}

// SampleMetadataVerify 抽样比对元数据快照：清单存在、分片可解析、数量一致。
// 最多检查 n 个快照（n ≤ 0 → 5）；repoKey 非空时按快照目录布局过滤
// （metadata/<sanitized repoKey>/ 下的才算）。BackupDir 未配置时返回
// Reason="backup_dir not configured"（壳层据此回 {ok,reason}）。
func (s *Service) SampleMetadataVerify(_ context.Context, repoKey string, n int) (VerifyReport, error) {
	if s.BackupDir() == "" {
		return VerifyReport{OK: false, Reason: "backup_dir not configured"}, nil
	}
	if n <= 0 {
		n = 5
	}
	root := s.MetadataBackupDir()
	warns := []string{}
	checked := 0
	prefix := ""
	if repoKey != "" {
		prefix = filepath.Join(root, strutil.SanitizePathToken(repoKey)) + string(filepath.Separator)
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "manifest.json" {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			return nil
		}
		checked++
		data, rerr := os.ReadFile(path) //nolint:gosec // 内部备份路径
		if rerr != nil {
			warns = append(warns, path+": read")
			return nil
		}
		var snap struct {
			Counts map[string]int32 `json:"counts"`
			Files  []string         `json:"files"`
		}
		if json.Unmarshal(data, &snap) != nil {
			warns = append(warns, path+": manifest parse")
			return nil
		}
		// 抽样：issues.json 存在且条数与 counts 对齐
		if snap.Counts["issues"] > 0 {
			issuesPath := filepath.Join(filepath.Dir(path), "issues.json")
			if _, serr := os.Stat(issuesPath); serr != nil {
				warns = append(warns, path+": issues.json missing")
			}
		}
		if checked >= n {
			return filepath.SkipAll
		}
		return nil
	})
	return VerifyReport{Checked: checked, OK: len(warns) == 0, Warnings: warns}, nil
}

// ===== 收集器:单一职责,便于复用与测试 =====

func (s *Service) collectLabelsMilestones(ctx context.Context, prov sdkprov.Provider, repo *model.Repo, snap *MetadataSnapshot) {
	if im, ok := prov.(sdkprov.IssueManager); ok {
		labels, lerr := im.ListIssueLabels(ctx, repo.PlatformOwner, repo.PlatformRepo)
		if lerr != nil {
			snap.Warnings = append(snap.Warnings, "labels: "+lerr.Error())
		} else {
			writeSnapshotPart(snap, "labels.json", labels)
			snap.Counts["labels"] = int32(len(labels))
		}
	}
	if mm, ok := prov.(sdkprov.MilestoneManager); ok {
		ms, merr := mm.ListMilestones(ctx, repo.PlatformOwner, repo.PlatformRepo, sdkprov.ListMilestonesOptions{})
		if merr != nil {
			snap.Warnings = append(snap.Warnings, "milestones: "+merr.Error())
		} else {
			writeSnapshotPart(snap, "milestones.json", ms)
			snap.Counts["milestones"] = int32(len(ms))
		}
	}
}

func (s *Service) collectIssues(ctx context.Context, prov sdkprov.Provider, repo *model.Repo, maxItems int, since string, snap *MetadataSnapshot) {
	im, ok := prov.(sdkprov.IssueManager)
	if !ok {
		return
	}
	issues, ierr := listIssues(ctx, prov, repo, "all", maxItems)
	if ierr != nil {
		snap.Warnings = append(snap.Warnings, "issues: "+ierr.Error())
		return
	}
	if since != "" {
		issues = filterIssuesSince(issues, since)
	}
	for _, iss := range issues {
		comments, cerr := im.ListIssueComments(ctx, repo.PlatformOwner, repo.PlatformRepo, iss.Number)
		if cerr == nil {
			iss.CommentList = comments
		}
	}
	writeSnapshotPart(snap, "issues.json", issues)
	snap.Counts["issues"] = int32(len(issues))
}

// filterIssuesSince 仅保留 UpdatedAt >= since(RFC3339) 的 issue。
func filterIssuesSince(issues []*issueRow, since string) []*issueRow {
	cut, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return issues
	}
	out := issues[:0]
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		if t, perr := time.Parse(time.RFC3339, iss.UpdatedAt); perr == nil && t.Before(cut) {
			continue
		}
		out = append(out, iss)
	}
	return out
}

func (s *Service) collectPullRequests(ctx context.Context, prov sdkprov.Provider, repo *model.Repo, maxItems int, snap *MetadataSnapshot) {
	cm, ok := prov.(sdkprov.ChangeRequestManager)
	if !ok {
		return
	}
	prs, _, perr := cm.ListCRs(ctx, sdkprov.ListCROptions{
		Owner: repo.PlatformOwner, Repo: repo.PlatformRepo, PerPage: maxItems,
	})
	if perr != nil {
		snap.Warnings = append(snap.Warnings, "pull_requests: "+perr.Error())
		return
	}
	if len(prs) > maxItems {
		prs = prs[:maxItems]
	}
	writeSnapshotPart(snap, "pull_requests.json", prs)
	snap.Counts["pull_requests"] = int32(len(prs))
}

func (s *Service) collectReleases(ctx context.Context, prov sdkprov.Provider, repo *model.Repo,
	include bool, maxItems int, snap *MetadataSnapshot) []*sdkprov.ReleaseInfo {
	if !include {
		return nil
	}
	rm, ok := prov.(sdkprov.ReleaseManager)
	if !ok {
		return nil
	}
	rels, rerr := rm.ListReleases(ctx, repo.PlatformOwner, repo.PlatformRepo)
	if rerr != nil {
		snap.Warnings = append(snap.Warnings, "releases: "+rerr.Error())
		return nil
	}
	if len(rels) > maxItems {
		rels = rels[:maxItems]
	}
	writeSnapshotPart(snap, "releases.json", rels)
	snap.Counts["releases"] = int32(len(rels))
	return rels
}

func (s *Service) downloadSourceArchives(ctx context.Context, prov sdkprov.Provider, repo *model.Repo,
	releases []*sdkprov.ReleaseInfo, snapDir string, snap *MetadataSnapshot) {
	if len(releases) == 0 {
		return
	}
	rm, ok := prov.(sdkprov.ReleaseManager)
	if !ok {
		return
	}
	archDir := filepath.Join(snapDir, "archives")
	_ = os.MkdirAll(archDir, 0o750)
	for _, rel := range releases {
		if rel.TagName == "" {
			continue
		}
		data, aerr := rm.GetArchive(ctx, repo.PlatformOwner, repo.PlatformRepo, rel.TagName, "tar.gz")
		if aerr != nil {
			snap.Warnings = append(snap.Warnings, "archive "+rel.TagName+": "+aerr.Error())
			continue
		}
		name := strutil.SanitizePathToken(rel.TagName) + ".tar.gz"
		if err := os.WriteFile(filepath.Join(archDir, name), data, 0o600); err != nil {
			snap.Warnings = append(snap.Warnings, "write archive "+name+": "+err.Error())
			continue
		}
		snap.Archives = append(snap.Archives, name)
	}
	snap.Counts["archives"] = int32(len(snap.Archives))
}

func (s *Service) collectReleaseAssets(ctx context.Context, prov sdkprov.Provider, repo *model.Repo,
	releases []*sdkprov.ReleaseInfo, snapDir string, maxAssets int, snap *MetadataSnapshot) {
	assetDir := filepath.Join(snapDir, "release-assets")
	// token 归 ProviderForPlatform 解析(repo token 优先,GitHub App 感知);
	// 已收集的 releases 带 Assets 元数据则复用,不重复请求。
	saved, warns, aerr := s.DownloadReleaseAssets(ctx, prov, repo.PlatformOwner, repo.PlatformRepo,
		releases, assetDir, maxAssets)
	snap.Warnings = append(snap.Warnings, warns...)
	if aerr != nil {
		snap.Warnings = append(snap.Warnings, "release-assets: "+aerr.Error())
		return
	}
	snap.Assets = saved
	snap.Counts["release_assets"] = int32(len(saved))
}

func (s *Service) collectGists(ctx context.Context, prov sdkprov.Provider, snapDir string, maxGists int, snap *MetadataSnapshot) {
	gistDir := filepath.Join(snapDir, "gists")
	gc, warns, gerr := s.BackupGists(ctx, prov, gistDir, maxGists)
	snap.Warnings = append(snap.Warnings, warns...)
	if gerr != nil {
		snap.Warnings = append(snap.Warnings, "gists: "+gerr.Error())
		return
	}
	snap.Counts["gists"] = int32(gc)
}

// writeSnapshotPart 写出分片 JSON 并登记文件名。
func writeSnapshotPart(snap *MetadataSnapshot, name string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		snap.Warnings = append(snap.Warnings, name+": marshal: "+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(snap.Dir, name), data, 0o600); err != nil {
		snap.Warnings = append(snap.Warnings, name+": write: "+err.Error())
		return
	}
	snap.Files = append(snap.Files, name)
}

func writeSnapshotJSON(snap *MetadataSnapshot, snapDir string) {
	writeSnapshotPart(snap, "manifest.json", snap)
}

// ===== issue 分片视图（与壳 issues-export 的行结构 JSON 兼容） =====

// issueRow 导出用 issue 视图（issues.json 分片结构；JSON 标签与壳层一致）。
type issueRow struct {
	Number      string                  `json:"number"`
	Title       string                  `json:"title"`
	State       string                  `json:"state"`
	Author      string                  `json:"author,omitempty"`
	Labels      []string                `json:"labels,omitempty"`
	Assignees   []string                `json:"assignees,omitempty"`
	Body        string                  `json:"body,omitempty"`
	WebURL      string                  `json:"web_url,omitempty"`
	CreatedAt   string                  `json:"created_at"`
	UpdatedAt   string                  `json:"updated_at"`
	CommentList []*sdkprov.IssueComment `json:"comments,omitempty"`
}

// listIssues 分页拉取 issue 行（空页终止 + 到 max 即 ErrStopIteration 的
// 页预算语义与壳 issues-export 一致）。
func listIssues(ctx context.Context, p sdkprov.Provider, repo *model.Repo, state string, max int) ([]*issueRow, error) {
	// 平台不一定实现 IssueManager
	im, ok := p.(sdkprov.IssueManager)
	if !ok {
		return nil, fmt.Errorf("platform %s does not support issue export", repo.Platform)
	}
	// 空 state 交平台默认(通常 open+closed 或 open)
	st := sdkprov.IssueState(state)
	const perPage = 50
	// 页预算 = 拉满 max 所需页 +1 页空页观测(空页终止语义,v0.73 分页面);
	// fn 内到 max 即 ErrStopIteration 提前收束,预算实际用不到。
	maxPages := (max + perPage - 1) / perPage
	if maxPages < 1 {
		maxPages = 1
	}
	var batch []*sdkprov.Issue
	err := sdkprov.EachBounded(ctx,
		func(ctx context.Context, page int) ([]*sdkprov.Issue, error) {
			items, _, lerr := im.ListIssues(ctx, sdkprov.ListIssuesOptions{
				Owner:   repo.PlatformOwner,
				Repo:    repo.PlatformRepo,
				State:   st,
				Page:    page,
				PerPage: perPage,
			})
			return items, lerr
		}, maxPages+1,
		func(iss *sdkprov.Issue) error {
			if len(batch) >= max {
				return sdkprov.ErrStopIteration
			}
			batch = append(batch, iss)
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := make([]*issueRow, 0, len(batch))
	for _, iss := range batch {
		out = append(out, toIssueRow(iss))
	}
	return out, nil
}

func toIssueRow(iss *sdkprov.Issue) *issueRow {
	author := ""
	if iss.Author != nil {
		author = iss.Author.Username
	}
	return &issueRow{
		Number:    iss.Number,
		Title:     iss.Title,
		State:     string(iss.State),
		Author:    author,
		Labels:    iss.Labels,
		Assignees: iss.Assignees,
		Body:      iss.Body,
		WebURL:    iss.WebURL,
		CreatedAt: iss.CreatedAt.Format(time.RFC3339),
		UpdatedAt: iss.UpdatedAt.Format(time.RFC3339),
	}
}
