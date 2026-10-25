package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
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
	// writeErr 记录分片写失败(任一分片写不出来 ⇒ 这次备份不完整)。
	// 未导出:不进 manifest/响应,只在本次调用内传递。
	writeErr error

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

	// 任一分片写失败 ⇒ 这次备份不完整 ⇒ 必须让调用方拿到失败。
	if snap.writeErr != nil {
		return nil, snap.writeErr
	}
	// manifest 写失败必须**让调用方拿到失败**:
	// manifest.json 是回灌定位快照的唯一依据(没有它,这份快照在列表接口里
	// 查不到、回灌也找不到)⇒ 写失败却返回 success,等于"备份成功但对灾备不可用"。
	if err := writeSnapshotJSON(snap, snapDir); err != nil {
		return nil, err
	}
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
	// **一个快照都没抽到 ⇒ 不是"校验通过"**:
	// 原来 OK 只看 warns,checked=0 时也成立 ⇒ 没有快照时返回
	// {checked:0, ok:true},调用方(如 DR 演练响应)原样透出成
	// "metadata_check.ok=true" —— 看着像健康,实际什么都没查。
	ok := len(warns) == 0 && checked > 0
	if checked == 0 {
		warns = append(warns, "没有可校验的元数据快照(备份目录里没有 manifest.json)")
	}
	return VerifyReport{Checked: checked, OK: ok, Warnings: warns}, nil
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
	// **增量模式要按 updated 排、并扫得更深**。
	//
	// 平台默认按 created 倒序 ⇒ "上次备份后被改动的**老** issue"正好被挤在
	// 第一页之外,客户端再按 updated 过滤也捞不到(捞到的永远是"最新创建"的
	// 那批)⇒ 增量备份漏数据,而 truncated 标记又不提示具体漏了什么。
	//
	// ① 请求排序:支持 updated 排序的平台(GitHub)直接把要的排在前面;
	// ② 扫描更深:不支持排序的平台(Gitea/Forgejo 家族当前 SDK 不暴露该参数)
	//    只能靠多扫 —— 增量模式把扫描上限放大到增量的若干倍;
	// ③ 扫完仍没覆盖到列表末尾 ⇒ 如实告警("可能还有未扫到的更新"),
	//    不能让人以为"这次增量是全的"。
	sortBy, sortDir := "", ""
	scanMax := 0
	if since != "" {
		sortBy, sortDir = "updated", "desc"
		scanMax = maxItems * incrementalScanFactor
	}
	issues, scanned, prSkipped, ierr := listIssues(ctx, prov, repo, "all", maxItems, scanMax, sortBy, sortDir)
	if ierr != nil {
		snap.Warnings = append(snap.Warnings, "issues: "+ierr.Error())
		return
	}
	if since != "" && scanned >= scanMax {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf(
			"issues: 增量扫描已达上限(扫描 %d 条),可能还有未扫到的更新(平台若不支持按 updated 排序,增量可能不完整)",
			scanned))
	}
	// **剔除 PR**:GitHub 的 issue 列表端点把 PR 一起返回(同一条记录既是
	// issue 也是 PR)⇒ 不剔除的话同一个 PR 既进 issues 快照又进
	// pull_requests 快照(重复),而且**占掉 issue 的数量预算**(默认 500)
	// ⇒ PR 多的仓里真实 issue 可能整批落在预算之外,快照缺项且无任何告警。
	//
	// 剔除数量如实告警:调用方要能知道"issue 快照少了多少"。
	if prSkipped > 0 {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("issues: 剔除了 %d 条 PR(平台的 issue 列表包含 PR;它们不占 issue 额度)", prSkipped))
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

// incrementalScanFactor 增量模式把**扫描上限**放大到增量的多少倍。
//
// 平台不支持按 updated 排序时,只能靠多扫几页来覆盖"被改过的老 issue";
// 放大倍数越大越全,但 API 调用也越多。
const incrementalScanFactor = 10

// countPullRequests 数一数里有多少条其实是 PR。
func countPullRequests(issues []*issueRow) int {
	n := 0
	for _, iss := range issues {
		if iss != nil && iss.IsPullRequest {
			n++
		}
	}
	return n
}

// withoutPullRequests 去掉 PR,只留真正的 issue。
func withoutPullRequests(issues []*issueRow) []*issueRow {
	out := issues[:0]
	for _, iss := range issues {
		if iss != nil && iss.IsPullRequest {
			continue
		}
		out = append(out, iss)
	}
	return out
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
	// **open 与 closed 都要捞回来**。
	//
	// 列表端点按状态过滤,不传状态 = 只返回 open(GitHub/Gitea/Forgejo 的默认),
	// 于是**已合并/已关闭的 PR 被静默丢弃** —— 真实仓里那是绝大多数,
	// 快照因此既不完整(灾备回灌缺历史),也让回灌侧的「PR 状态跟着回灌」
	// 根本无从触发(快照里压根没有 closed/merged 的 PR)。
	//
	// 平台没有 "all" 这个过滤值(SDK 的 mapListState 只映射 open/closed),
	// 所以分两次取再合并;closed 一侧在 GitHub/Gitea 上同时覆盖"已关闭"与"已合并"
	// (合并态在数据模型里就是 state=closed + merged=true)。
	var all []*sdkprov.ChangeRequest
	seen := map[string]bool{}
	for _, st := range []sdkprov.CRState{sdkprov.CRStateOpened, sdkprov.CRStateClosed} {
		prs, lerr := listCRs(ctx, cm, repo, st, maxItems)
		if lerr != nil {
			// 一侧失败要**告警**:静默少一半比报错更坏(快照看着是成功的)。
			snap.Warnings = append(snap.Warnings, "pull_requests("+string(st)+"): "+lerr.Error())
			continue
		}
		for _, pr := range prs {
			if pr == nil || pr.Number == "" || seen[pr.Number] {
				continue
			}
			seen[pr.Number] = true
			all = append(all, pr)
		}
	}
	// 两侧各自按各自的顺序返回 ⇒ 合并后按更新时间倒序再截断,
	// 否则「open 侧就顶满上限」会把 closed 侧整批挤掉。
	sort.SliceStable(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	if maxItems > 0 && len(all) > maxItems {
		all = all[:maxItems]
	}
	writeSnapshotPart(snap, "pull_requests.json", all)
	snap.Counts["pull_requests"] = int32(len(all))
}

// listCRs 分页拉取指定状态的 PR(上限 max 条)。
//
// **必须分页**:列表端点的每页上限是 100(SDK 的 MaxPerPage),
// 而 max_items 默认 500 —— 一次性 PerPage=max 会被截成 100 且只拿第 1 页,
// 于是"上限 500"实际只拿到 100 条。
func listCRs(ctx context.Context, cm sdkprov.ChangeRequestManager, repo *model.Repo,
	state sdkprov.CRState, max int) ([]*sdkprov.ChangeRequest, error) {
	const perPage = 50
	// 页预算 = 拉满 max 所需页 +1 页空页观测(空页终止语义)。
	maxPages := (max + perPage - 1) / perPage
	if maxPages < 1 {
		maxPages = 1
	}
	var batch []*sdkprov.ChangeRequest
	err := sdkprov.EachBounded(ctx,
		func(ctx context.Context, page int) ([]*sdkprov.ChangeRequest, error) {
			items, _, lerr := cm.ListCRs(ctx, sdkprov.ListCROptions{
				Owner:   repo.PlatformOwner,
				Repo:    repo.PlatformRepo,
				State:   state,
				Page:    page,
				PerPage: perPage,
			})
			return items, lerr
		}, maxPages+1,
		func(pr *sdkprov.ChangeRequest) error {
			if len(batch) >= max {
				return sdkprov.ErrStopIteration
			}
			batch = append(batch, pr)
			return nil
		})
	if err != nil {
		return nil, err
	}
	return batch, nil
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
// writeSnapshotPart 写一个分片文件。
//
// 返回 error:**写失败是"这次备份不完整"**,不能只记 warning 就当成功返回
// (调用方要能区分"备份成功"与"备份了一半")。分片写失败仍继续写其它分片
// (尽量多留材料),但错误会一路返回给调用方。
func writeSnapshotPart(snap *MetadataSnapshot, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		snap.Warnings = append(snap.Warnings, name+": marshal: "+err.Error())
		return errors.Wrap(err, "marshal "+name)
	}
	if err := os.WriteFile(filepath.Join(snap.Dir, name), data, 0o600); err != nil {
		snap.Warnings = append(snap.Warnings, name+": write: "+err.Error())
		if snap.writeErr == nil {
			snap.writeErr = errors.Wrap(err, "write "+name)
		}
		return errors.Wrap(err, "write "+name)
	}
	snap.Files = append(snap.Files, name)
	return nil
}

// writeSnapshotJSON 写 manifest.json;失败返回错误(调用方据此判失败)。
func writeSnapshotJSON(snap *MetadataSnapshot, snapDir string) error {
	return writeSnapshotPart(snap, "manifest.json", snap)
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
	// IsPullRequest 标记这条"issue"其实是 PR(平台的 issue 列表含 PR)。
	// **不落进 JSON**:PR 在采集阶段就被剔除了,快照里不该出现。
	IsPullRequest bool `json:"-"`
}

// listIssues 分页拉取 issue 行（空页终止 + 到 max 即 ErrStopIteration 的
// 页预算语义与壳 issues-export 一致）。
// listIssues 拉取 issue 行。
//
// max 是**产出条数**上限;scanMax 是**扫描条数**上限(增量模式要扫得更深,
// 见 incrementalScanFactor);scanMax<=0 时与 max 相同。
func listIssues(ctx context.Context, p sdkprov.Provider, repo *model.Repo, state string, max, scanMax int,
	sortBy, sortDir string) ([]*issueRow, int, int, error) {
	// 平台不一定实现 IssueManager
	im, ok := p.(sdkprov.IssueManager)
	if !ok {
		return nil, 0, 0, fmt.Errorf("platform %s does not support issue export", repo.Platform)
	}
	// 空 state 交平台默认(通常 open+closed 或 open)
	st := sdkprov.IssueState(state)
	const perPage = 50
	// 页预算 = 拉满扫描上限所需页 +1 页空页观测(空页终止语义);
	// fn 内到扫描上限即 ErrStopIteration 提前收束。
	scanLimit := max
	if scanMax > scanLimit {
		scanLimit = scanMax
	}
	maxPages := (scanLimit + perPage - 1) / perPage
	if maxPages < 1 {
		maxPages = 1
	}
	var batch []*sdkprov.Issue
	scanned := 0
	prSkipped := 0
	err := sdkprov.EachBounded(ctx,
		func(ctx context.Context, page int) ([]*sdkprov.Issue, error) {
			items, _, lerr := im.ListIssues(ctx, sdkprov.ListIssuesOptions{
				Owner:   repo.PlatformOwner,
				Repo:    repo.PlatformRepo,
				State:   st,
				Page:    page,
				PerPage: perPage,
				// 增量模式要按 updated 排(见 collectIssues 的说明)。
				Sort:      sortBy,
				Direction: sortDir,
			})
			return items, lerr
		}, maxPages+1,
		func(iss *sdkprov.Issue) error {
			scanned++
			// **PR 不占 issue 的预算**:平台的 issue 列表端点(GitHub)把 PR 一起返回,
			// 若让它们占额度,PR 多的仓里真实 issue 会被整批挤出预算(而快照看着是成功的)。
			// 这里直接跳过并继续翻页 ⇒ 真实 issue 补进预算。
			if iss != nil && iss.IsPullRequest {
				prSkipped++
				return nil
			}
			if len(batch) >= max {
				return sdkprov.ErrStopIteration
			}
			batch = append(batch, iss)
			return nil
		})
	if err != nil {
		return nil, scanned, prSkipped, err
	}
	out := make([]*issueRow, 0, len(batch))
	for _, iss := range batch {
		out = append(out, toIssueRow(iss))
	}
	return out, scanned, prSkipped, nil
}

func toIssueRow(iss *sdkprov.Issue) *issueRow {
	author := ""
	if iss.Author != nil {
		author = iss.Author.Username
	}
	return &issueRow{
		Number:        iss.Number,
		Title:         iss.Title,
		State:         string(iss.State),
		IsPullRequest: iss.IsPullRequest,
		Author:        author,
		Labels:        iss.Labels,
		Assignees:     iss.Assignees,
		Body:          iss.Body,
		WebURL:        iss.WebURL,
		CreatedAt:     iss.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     iss.UpdatedAt.Format(time.RFC3339),
	}
}
