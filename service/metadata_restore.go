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

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// 元数据回灌引擎（壳 MetadataRestore 端点下沉）：将 metadata-backup 快照
// 回灌到目标仓 labels → milestones → issues → PRs(以 issue 形态) → releases。
// dry_run 缺省 true（安全默认，壳层按 IDL optional 解析）；overwrite=false
// 时同名跳过。所有 warning 文案逐字保留（前端/CLI/文档引用）。

// RestoreRequest 元数据回灌入参。Kinds 空时按快照内存在的分片推导
// （labels|milestones|issues|prs|releases）。
type RestoreRequest struct {
	RepoKey        string
	SnapshotDir    string
	Kinds          []string
	DryRun         bool
	Overwrite      bool
	TargetPlatform string // 平台 key，空 = repo 关联平台
	TargetOwner    string // 空 = repo.PlatformOwner
	TargetRepo     string // 空 = repo.PlatformRepo
}

// RestoreKindStat 单类回灌统计（JSON 标签与壳 ops.RestoreKindStat 一致）。
type RestoreKindStat struct {
	Planned int `json:"planned"`
	Created int `json:"created"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// RestoreItem 回灌逐条明细。
type RestoreItem struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action"` // created | skipped | failed（dry-run 下 created 表示"将会创建"）
	Error  string `json:"error,omitempty"`
}

// RestoreResult 回灌结果。SnapshotDir/Target/Stats/Warnings/DryRun/StartedAt/
// FinishedAt 与壳 ops.MetadataRestoreResult 同构（历史落盘走 restoreHistoryRecord，
// 文件格式不含扩展字段）；Applied/Skipped/Failed 是 Stats 的聚合视图，
// Details 为逐条明细。
type RestoreResult struct {
	SnapshotDir string                      `json:"snapshot_dir"`
	Target      string                      `json:"target"`
	Stats       map[string]*RestoreKindStat `json:"stats"`
	Warnings    []string                    `json:"warnings"`
	DryRun      bool                        `json:"dry_run"`
	StartedAt   string                      `json:"started_at"`
	FinishedAt  string                      `json:"finished_at"`
	Applied     int                         `json:"applied"`
	Skipped     int                         `json:"skipped"`
	Failed      int                         `json:"failed"`
	Details     []RestoreItem               `json:"details"`
}

// addDetail 登记逐条明细。
func (r *RestoreResult) addDetail(kind, name, action string, err error) {
	item := RestoreItem{Kind: kind, Name: name, Action: action}
	if err != nil {
		item.Error = err.Error()
	}
	r.Details = append(r.Details, item)
}

// RestoreMetadata 回灌快照到目标仓。校验顺序与历史壳层一致：
// backup_dir → 最新快照解析 → manifest 加载 → repo → 目标平台 → provider。
// 错误文案是对外契约：快照类经 ErrMetadataValidation 归 400，
// repo/平台缺失归 404，provider 类回落 500。
func (s *Service) RestoreMetadata(ctx context.Context, req RestoreRequest) (*RestoreResult, error) {
	backupDir := s.BackupDir()
	if backupDir == "" {
		return nil, newMetadataError("sync.backup_dir not configured", nil)
	}

	snapDir := req.SnapshotDir
	if snapDir == "" {
		latest, lerr := latestMetadataSnapshot(backupDir, req.RepoKey)
		if lerr != nil {
			return nil, newMetadataError(lerr.Error(), lerr)
		}
		snapDir = latest
	}
	snap, err := loadMetadataSnapshot(snapDir)
	if err != nil {
		return nil, newMetadataError("load snapshot: "+err.Error(), err)
	}

	repo, err := s.GetRepo(ctx, req.RepoKey)
	if err != nil || repo == nil {
		return nil, ErrRepoNotFound
	}
	plat := resolveTargetPlatform(ctx, s, repo, req.TargetPlatform)
	if plat == nil {
		return nil, ErrTargetPlatformNotFound
	}
	owner := req.TargetOwner
	if owner == "" {
		owner = repo.PlatformOwner
	}
	repoName := req.TargetRepo
	if repoName == "" {
		repoName = repo.PlatformRepo
	}
	prov, err := s.ProviderForPlatform(plat, repo.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("create provider: %w", err)
	}
	return s.runRestore(ctx, req, snapDir, snap, plat, owner, repoName, prov), nil
}

// runRestore kind 归一 → 分发回灌 → 统计聚合 → 历史落盘 → dry_run 提示。
// 与入口（DB/provider 解析）拆分，测试可注入桩 provider。
func (s *Service) runRestore(ctx context.Context, req RestoreRequest, snapDir string,
	snap *MetadataSnapshot, plat *model.Platform, owner, repoName string, prov sdkprov.Provider) *RestoreResult {
	kinds := normalizeRestoreKinds(req.Kinds, snap)
	result := &RestoreResult{
		SnapshotDir: snapDir,
		Target:      fmt.Sprintf("%s/%s/%s", plat.Type, owner, repoName),
		Stats:       map[string]*RestoreKindStat{},
		Warnings:    []string{},
		Details:     []RestoreItem{},
		DryRun:      req.DryRun,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	for _, k := range kinds {
		result.Stats[k] = &RestoreKindStat{}
	}

	target := restoreTarget{owner: owner, repo: repoName, plat: plat, prov: prov,
		overwrite: req.Overwrite, dryRun: req.DryRun}
	for _, kind := range kinds {
		stat := result.Stats[kind]
		switch kind {
		case "labels":
			restoreLabels(ctx, target, snapDir, stat, result)
		case "milestones":
			restoreMilestones(ctx, target, snapDir, stat, result)
		case "issues":
			restoreIssues(ctx, target, snapDir, stat, result, false)
		case "prs":
			restoreIssues(ctx, target, snapDir, stat, result, true)
		case "releases":
			restoreReleases(ctx, target, snapDir, stat, result)
		default:
			result.Warnings = append(result.Warnings, "unknown kind: "+kind)
		}
	}
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	for _, st := range result.Stats {
		if st == nil {
			continue
		}
		result.Applied += st.Created
		result.Skipped += st.Skipped
		result.Failed += st.Failed
	}

	// 历史文件在 dry_run 提示追加之前写入（与历史行为一致：提示不进历史）。
	writeRestoreHistory(s.BackupDir(), req.RepoKey, result)
	if req.DryRun {
		result.Warnings = append(result.Warnings, "dry_run=true：未写入目标；去掉 dry_run 执行")
	}
	return result
}

type restoreTarget struct {
	owner     string
	repo      string
	plat      *model.Platform
	prov      sdkprov.Provider
	overwrite bool
	dryRun    bool
}

func resolveTargetPlatform(ctx context.Context, s *Service, repo *model.Repo, key string) *model.Platform {
	if key == "" {
		plat, err := s.GetPlatformByID(ctx, repo.PlatformID)
		if err != nil {
			return nil
		}
		return plat
	}
	plat, err := s.GetPlatform(ctx, key)
	if err != nil {
		return nil
	}
	return plat
}

func latestMetadataSnapshot(backupDir, repoKey string) (string, error) {
	root := filepath.Join(backupDir, "metadata", strutil.SanitizePathToken(repoKey))
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("no snapshot for %s: %w", repoKey, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("no snapshot for %s", repoKey)
	}
	sort.Strings(dirs)
	return filepath.Join(root, dirs[len(dirs)-1]), nil
}

func loadMetadataSnapshot(dir string) (*MetadataSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:gosec // 内部备份路径
	if err != nil {
		return nil, err
	}
	var snap MetadataSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func normalizeRestoreKinds(kinds []string, snap *MetadataSnapshot) []string {
	available := map[string]bool{}
	for _, f := range snap.Files {
		switch f {
		case "labels.json":
			available["labels"] = true
		case "milestones.json":
			available["milestones"] = true
		case "issues.json":
			available["issues"] = true
		case "pull_requests.json":
			available["prs"] = true
		case "releases.json":
			available["releases"] = true
		}
	}
	if len(kinds) == 0 {
		out := []string{"labels", "milestones", "issues", "prs", "releases"}
		filtered := out[:0]
		for _, k := range out {
			if available[k] {
				filtered = append(filtered, k)
			}
		}
		return filtered
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		k = strings.TrimSpace(strings.ToLower(k))
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func readSnapshotJSON(dir, name string, v any) error {
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // 内部备份路径
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func restoreLabels(ctx context.Context, t restoreTarget, dir string, stat *RestoreKindStat, result *RestoreResult) {
	var labels []*sdkprov.IssueLabel
	if err := readSnapshotJSON(dir, "labels.json", &labels); err != nil {
		result.Warnings = append(result.Warnings, "labels.json: "+err.Error())
		return
	}
	lm, ok := t.prov.(sdkprov.LabelManager)
	if !ok {
		result.Warnings = append(result.Warnings, "target does not support LabelManager")
		return
	}
	existing := map[string]bool{}
	if cur, err := lm.ListLabels(ctx, t.owner, t.repo, sdkprov.ListLabelsOptions{PerPage: 100}); err == nil {
		for _, l := range cur {
			existing[l.Name] = true
		}
	}
	for _, lb := range labels {
		stat.Planned++
		if lb == nil || lb.Name == "" {
			stat.Skipped++
			result.addDetail("labels", "", "skipped", nil)
			continue
		}
		if existing[lb.Name] {
			if !t.overwrite {
				stat.Skipped++
				result.addDetail("labels", lb.Name, "skipped", nil)
				continue
			}
			if t.dryRun {
				stat.Created++
				result.addDetail("labels", lb.Name, "created", nil)
				continue
			}
			if _, err := lm.UpdateLabel(ctx, t.owner, t.repo, lb.Name, sdkprov.UpdateLabelOptions{
				Color: &lb.Color,
			}); err != nil {
				stat.Failed++
				result.addDetail("labels", lb.Name, "failed", err)
				result.Warnings = append(result.Warnings, "update label "+lb.Name+": "+err.Error())
				continue
			}
			stat.Created++
			result.addDetail("labels", lb.Name, "created", nil)
			continue
		}
		if t.dryRun {
			stat.Created++
			result.addDetail("labels", lb.Name, "created", nil)
			continue
		}
		if _, err := lm.CreateLabel(ctx, t.owner, t.repo, sdkprov.CreateLabelOptions{
			Name:  lb.Name,
			Color: lb.Color,
		}); err != nil {
			stat.Failed++
			result.addDetail("labels", lb.Name, "failed", err)
			result.Warnings = append(result.Warnings, "create label "+lb.Name+": "+err.Error())
			continue
		}
		stat.Created++
		result.addDetail("labels", lb.Name, "created", nil)
	}
}

func restoreMilestones(ctx context.Context, t restoreTarget, dir string, stat *RestoreKindStat, result *RestoreResult) {
	var ms []*sdkprov.Milestone
	if err := readSnapshotJSON(dir, "milestones.json", &ms); err != nil {
		result.Warnings = append(result.Warnings, "milestones.json: "+err.Error())
		return
	}
	mm, ok := t.prov.(sdkprov.MilestoneManager)
	if !ok {
		result.Warnings = append(result.Warnings, "target does not support MilestoneManager")
		return
	}
	existing := map[string]bool{}
	if cur, err := mm.ListMilestones(ctx, t.owner, t.repo, sdkprov.ListMilestonesOptions{PerPage: 100}); err == nil {
		for _, m := range cur {
			existing[m.Title] = true
		}
	}
	for _, m := range ms {
		stat.Planned++
		if m == nil || m.Title == "" {
			stat.Skipped++
			result.addDetail("milestones", "", "skipped", nil)
			continue
		}
		if existing[m.Title] {
			stat.Skipped++
			result.addDetail("milestones", m.Title, "skipped", nil)
			continue
		}
		if t.dryRun {
			stat.Created++
			result.addDetail("milestones", m.Title, "created", nil)
			continue
		}
		opts := sdkprov.CreateMilestoneOptions{Title: m.Title, Description: m.Description, DueOn: m.DueOn}
		if _, err := mm.CreateMilestone(ctx, t.owner, t.repo, opts); err != nil {
			stat.Failed++
			result.addDetail("milestones", m.Title, "failed", err)
			result.Warnings = append(result.Warnings, "create milestone "+m.Title+": "+err.Error())
			continue
		}
		stat.Created++
		result.addDetail("milestones", m.Title, "created", nil)
	}
}

// restoreIssues 回灌 issues；asPR=true 时读 pull_requests.json 并以 issue 形态落盘（标注来源）。
func restoreIssues(ctx context.Context, t restoreTarget, dir string, stat *RestoreKindStat, result *RestoreResult, asPR bool) {
	kind := "issues"
	if asPR {
		kind = "prs"
	}
	im, ok := t.prov.(sdkprov.IssueManager)
	if !ok {
		result.Warnings = append(result.Warnings, "target does not support IssueManager")
		return
	}
	existingTitles := map[string]bool{}
	if cur, _, err := im.ListIssues(ctx, sdkprov.ListIssuesOptions{
		Owner: t.owner, Repo: t.repo, State: sdkprov.IssueState("all"), PerPage: 100,
	}); err == nil {
		for _, iss := range cur {
			existingTitles[iss.Title] = true
		}
	}

	if asPR {
		var prs []*sdkprov.ChangeRequest
		if err := readSnapshotJSON(dir, "pull_requests.json", &prs); err != nil {
			result.Warnings = append(result.Warnings, "pull_requests.json: "+err.Error())
			return
		}
		for _, pr := range prs {
			stat.Planned++
			if pr == nil || pr.Title == "" {
				stat.Skipped++
				result.addDetail(kind, "", "skipped", nil)
				continue
			}
			if existingTitles[pr.Title] {
				stat.Skipped++
				result.addDetail(kind, pr.Title, "skipped", nil)
				continue
			}
			body := prBody(pr)
			if t.dryRun {
				stat.Created++
				result.addDetail(kind, pr.Title, "created", nil)
				continue
			}
			created, err := im.CreateIssue(ctx, sdkprov.CreateIssueOptions{
				Owner: t.owner, Repo: t.repo, Title: pr.Title, Body: body,
			})
			if err != nil {
				stat.Failed++
				result.addDetail(kind, pr.Title, "failed", err)
				result.Warnings = append(result.Warnings, "create pr-as-issue "+pr.Title+": "+err.Error())
				continue
			}
			stat.Created++
			result.addDetail(kind, pr.Title, "created", nil)
			existingTitles[pr.Title] = true
			_ = created
		}
		return
	}

	var issues []*issueRow
	if err := readSnapshotJSON(dir, "issues.json", &issues); err != nil {
		result.Warnings = append(result.Warnings, "issues.json: "+err.Error())
		return
	}
	for _, iss := range issues {
		stat.Planned++
		if iss == nil || iss.Title == "" {
			stat.Skipped++
			result.addDetail(kind, "", "skipped", nil)
			continue
		}
		if existingTitles[iss.Title] {
			stat.Skipped++
			result.addDetail(kind, iss.Title, "skipped", nil)
			continue
		}
		body := issueBody(iss)
		if t.dryRun {
			stat.Created++
			result.addDetail(kind, iss.Title, "created", nil)
			continue
		}
		created, err := im.CreateIssue(ctx, sdkprov.CreateIssueOptions{
			Owner: t.owner, Repo: t.repo, Title: iss.Title, Body: body,
			Labels: iss.Labels,
		})
		if err != nil {
			stat.Failed++
			result.addDetail(kind, iss.Title, "failed", err)
			result.Warnings = append(result.Warnings, "create issue "+iss.Title+": "+err.Error())
			continue
		}
		stat.Created++
		result.addDetail(kind, iss.Title, "created", nil)
		existingTitles[iss.Title] = true
		for _, cm := range iss.CommentList {
			if cm == nil || cm.Body == "" {
				continue
			}
			if _, cerr := im.CreateIssueComment(ctx, t.owner, t.repo, created.Number, cm.Body); cerr != nil {
				result.Warnings = append(result.Warnings, "comment on "+iss.Title+": "+cerr.Error())
			}
		}
		if strings.EqualFold(iss.State, "closed") {
			if _, cerr := im.CloseIssue(ctx, t.owner, t.repo, created.Number); cerr != nil {
				result.Warnings = append(result.Warnings, "close issue "+iss.Title+": "+cerr.Error())
			}
		}
	}
}

func issueBody(iss *issueRow) string {
	var b strings.Builder
	b.WriteString("<!-- gitferry-restore:issue -->\n")
	if iss.WebURL != "" {
		b.WriteString("> 源：" + iss.WebURL + "\n\n")
	}
	if iss.Author != "" {
		b.WriteString("原作者：@" + iss.Author + "  ·  原编号：" + iss.Number + "\n\n")
	}
	b.WriteString(iss.Body)
	return b.String()
}

func prBody(pr *sdkprov.ChangeRequest) string {
	var b strings.Builder
	b.WriteString("<!-- gitferry-restore:pr -->\n")
	b.WriteString("> 以 issue 形态从 PR 快照回灌（不创建真实 PR）\n\n")
	_, _ = fmt.Fprintf(&b, "原编号：#%s  ·  `%s` → `%s`\n\n", pr.Number, pr.SourceBranch, pr.TargetBranch)
	b.WriteString(pr.Description)
	return b.String()
}

func restoreReleases(ctx context.Context, t restoreTarget, dir string, stat *RestoreKindStat, result *RestoreResult) {
	var rels []*sdkprov.ReleaseInfo
	if err := readSnapshotJSON(dir, "releases.json", &rels); err != nil {
		result.Warnings = append(result.Warnings, "releases.json: "+err.Error())
		return
	}
	rm, ok := t.prov.(sdkprov.ReleaseManager)
	if !ok {
		result.Warnings = append(result.Warnings, "target does not support ReleaseManager")
		return
	}
	existing := map[string]bool{}
	if cur, err := rm.ListReleases(ctx, t.owner, t.repo); err == nil {
		for _, r := range cur {
			existing[r.TagName] = true
		}
	}
	for _, rel := range rels {
		stat.Planned++
		if rel == nil || rel.TagName == "" {
			stat.Skipped++
			result.addDetail("releases", "", "skipped", nil)
			continue
		}
		if existing[rel.TagName] {
			stat.Skipped++
			result.addDetail("releases", rel.TagName, "skipped", nil)
			continue
		}
		if t.dryRun {
			stat.Created++
			result.addDetail("releases", rel.TagName, "created", nil)
			continue
		}
		_, err := rm.CreateRelease(ctx, t.owner, t.repo, sdkprov.CreateReleaseOptions{
			TagName:    rel.TagName,
			Title:      rel.Title,
			Body:       rel.Body,
			Draft:      rel.Draft,
			Prerelease: rel.Prerelease,
		})
		if err != nil {
			stat.Failed++
			result.addDetail("releases", rel.TagName, "failed", err)
			result.Warnings = append(result.Warnings, "create release "+rel.TagName+": "+err.Error())
			continue
		}
		stat.Created++
		result.addDetail("releases", rel.TagName, "created", nil)
	}
}

// restoreHistoryRecord 历史落盘结构：字段与顺序同壳 ops.MetadataRestoreResult，
// 不含 Applied/Skipped/Failed/Details 扩展视图（保持 metadata-restore/ 下
// 历史 JSON 文件格式不变）。
type restoreHistoryRecord struct {
	SnapshotDir string                      `json:"snapshot_dir"`
	Target      string                      `json:"target"`
	Stats       map[string]*RestoreKindStat `json:"stats"`
	Warnings    []string                    `json:"warnings"`
	DryRun      bool                        `json:"dry_run"`
	StartedAt   string                      `json:"started_at"`
	FinishedAt  string                      `json:"finished_at"`
}

func writeRestoreHistory(backupDir, repoKey string, result *RestoreResult) {
	dir := filepath.Join(backupDir, "metadata-restore", strutil.SanitizePathToken(repoKey))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return
	}
	data, err := json.Marshal(restoreHistoryRecord{
		SnapshotDir: result.SnapshotDir,
		Target:      result.Target,
		Stats:       result.Stats,
		Warnings:    result.Warnings,
		DryRun:      result.DryRun,
		StartedAt:   result.StartedAt,
		FinishedAt:  result.FinishedAt,
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, time.Now().UTC().Format("20060102-150405")+".json"), data, 0o600)
}
