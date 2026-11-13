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
	// Updated 是"覆盖更新"的条目数，**与 Created 分开记**。
	//
	// 原来 overwrite 走的是 UpdateLabel，却把计数记成 created ⇒
	// 报告说"新建了 N 条"，实际是"改写了 N 条已存在的东西" ——
	// 报告与事实相反，比失败本身更危险（运维以为在补建，其实在覆盖）。
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// RestoreItem 回灌逐条明细。
type RestoreItem struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action"` // created | updated | skipped | failed（dry-run 下 created/updated 表示"将会创建/将会覆盖"）
	Error  string `json:"error,omitempty"`
	// Note 补充说明这次到底改了什么（如"颜色 f00→0f0"）。
	// 覆盖是破坏性动作，不说清楚就没法审。
	Note string `json:"note,omitempty"`
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
	r.addDetailNote(kind, name, action, "", err)
}

// addDetailNote 登记逐条明细（带补充说明）。
func (r *RestoreResult) addDetailNote(kind, name, action, note string, err error) {
	item := RestoreItem{Kind: kind, Name: name, Action: action, Note: note}
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
	prov, err := s.ProviderForPlatform(plat, "")
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
		result.Applied += st.Created + st.Updated
		result.Skipped += st.Skipped
		result.Failed += st.Failed
	}

	// 历史文件在 dry_run 提示追加之前写入（与历史行为一致：提示不进历史）。
	if herr := writeRestoreHistory(s.BackupDir(), req.RepoKey, result); herr != nil {
		// 历史没落盘 ⇒ 审计留痕缺失,必须让调用方看到(不能"看着成功其实没留痕")。
		result.Warnings = append(result.Warnings, "回灌历史落盘失败:"+herr.Error())
	}
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

// warnOverwriteUnsupported 在"要求覆盖但该 kind 不支持覆盖"时告警。
//
// overwrite 目前只对 labels 生效(milestones / issues / releases 命中同名一律跳过)。
// **静默跳过 = 调用方以为覆盖了、实际没覆盖** —— 从调用方视角看就是数据丢失,
// 而 stats 里只体现为 skipped,没有任何提示。
func (t *restoreTarget) warnOverwriteUnsupported(result *RestoreResult, kind, name string) {
	if !t.overwrite {
		return
	}
	result.Warnings = append(result.Warnings,
		"overwrite 不支持该类型("+kind+"),同名条目已跳过(未覆盖):"+name)
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
	// 记录目标侧 label 的**当前颜色**:覆盖与否必须拿它比对着色,
	// 只记名字的话既判断不了"要不要写",也说不清"改了什么"。
	existing := map[string]string{}
	if cur, err := lm.ListLabels(ctx, t.owner, t.repo, sdkprov.ListLabelsOptions{PerPage: 100}); err == nil {
		for _, l := range cur {
			existing[l.Name] = l.Color
		}
	}
	var colorChanges []string
	for _, lb := range labels {
		stat.Planned++
		if lb == nil || lb.Name == "" {
			stat.Skipped++
			result.addDetail("labels", "", "skipped", nil)
			continue
		}
		cur, exists := existing[lb.Name]
		if exists {
			if !t.overwrite {
				stat.Skipped++
				result.addDetail("labels", lb.Name, "skipped", nil)
				continue
			}
			// **颜色一致就不写**:
			// 一次无意义的 UpdateLabel 会改动 label 的 updated_at,
			// 白白触发下游 webhook(label 事件),还让"这次改了 N 个"变成假账。
			if sameColor(cur, lb.Color) {
				stat.Skipped++
				result.addDetailNote("labels", lb.Name, "skipped", "颜色一致,无需覆盖", nil)
				continue
			}
			note := "颜色 " + cur + "→" + lb.Color
			if t.dryRun {
				stat.Updated++
				result.addDetailNote("labels", lb.Name, "updated", "将覆盖:"+note, nil)
				colorChanges = append(colorChanges, lb.Name+"("+cur+"→"+lb.Color+")")
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
			stat.Updated++
			result.addDetailNote("labels", lb.Name, "updated", note, nil)
			colorChanges = append(colorChanges, lb.Name+"("+cur+"→"+lb.Color+")")
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
	// **覆盖必须留痕**:目标侧的颜色可能是人手工改过的,
	// overwrite 会把它改回快照值 —— 这是破坏性动作,不能悄悄发生。
	if len(colorChanges) > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"overwrite 覆盖了 %d 个 label 的颜色:%s",
			len(colorChanges), strutil.Truncate(strings.Join(colorChanges, ", "), 500)))
	}
}

// sameColor 判断两个颜色是否等价。
//
// 各平台的颜色写法不一样(GitHub 用 "f00f00"、GitLab 用 "#f00f00"),
// 只差一个 "#" 或大小写的差异**不该触发一次写入**:
// 写了要么无意义,要么把目标平台不认的格式塞进去。
func sameColor(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(a), "#"),
		strings.TrimPrefix(strings.TrimSpace(b), "#"))
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
			t.warnOverwriteUnsupported(result, "milestones", m.Title)
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

// listExistingIssueTitles 拉取目标仓**全部** issue 的标题(去重用)。
//
// 必须翻页:只取第 1 页会让第 2 页以后的同名 issue 漏出去 ⇒ 重跑回灌产生重复。
// 一侧失败时如实告警(调用方要能知道"去重表不完整")。
func listExistingIssueTitles(ctx context.Context, im sdkprov.IssueManager, owner, repo string) (map[string]bool, []string) {
	out := map[string]bool{}
	var warns []string
	const perPage = 100
	page := 1
	for {
		items, _, err := im.ListIssues(ctx, sdkprov.ListIssuesOptions{
			Owner: owner, Repo: repo, State: sdkprov.IssueState("all"),
			Page: page, PerPage: perPage,
		})
		if err != nil {
			warns = append(warns, "list target issues for dedup failed: "+err.Error())
			return out, warns
		}
		for _, iss := range items {
			if iss != nil {
				out[iss.Title] = true
			}
		}
		if len(items) < perPage {
			return out, warns
		}
		page++
		if page > 50 {
			// 兜底:目标仓异常大时别无限翻页。
			warns = append(warns, "list target issues for dedup stopped at 50 pages")
			return out, warns
		}
	}
}

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
	// 去重表必须**翻页拉全量**:
	// 原来只取第 1 页(100 条)⇒ 目标仓 issue 超过 100 条后,
	// 第二页以后的同名 issue 不在去重表里 ⇒ 灾备演练**重跑会重复创建**同名 issue
	// (幂等性被破坏,DR 重演变脏数据);overwrite=false 也拦不住
	// (同名检查根本没看到)。
	existingTitles, dedupWarn := listExistingIssueTitles(ctx, im, t.owner, t.repo)
	result.Warnings = append(result.Warnings, dedupWarn...)

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
				t.warnOverwriteUnsupported(result, kind, pr.Title)
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
			// **PR 的状态要跟着回灌**:已合并/已关闭的 PR 回灌成"开着的 issue",
			// 看着像一条待处理的请求(issues 分支本来就处理了 closed 状态)。
			if pr.State == sdkprov.CRStateMerged || pr.State == sdkprov.CRStateClosed {
				if _, cerr := im.CloseIssue(ctx, t.owner, t.repo, created.Number); cerr != nil {
					result.Warnings = append(result.Warnings, "close pr-as-issue "+pr.Title+": "+cerr.Error())
				}
			}
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
			t.warnOverwriteUnsupported(result, kind, iss.Title)
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
			t.warnOverwriteUnsupported(result, "releases", rel.TagName)
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

// writeRestoreHistory 落回灌历史(灾备演练的审计留痕)。
//
// **失败必须让调用方知道**:原来目录建不出来、序列化失败、写文件失败
// 全部静默 return ⇒ 回灌结果看起来是成功的,而"回灌了多少、改了什么"
// 这条唯一留痕**无痕消失**。
//
// 文件名用**毫秒**而不是秒:同一秒内两次回灌会写同一个文件名,
// 后者把前者覆盖掉(实测:两次回灌只剩 1 个文件)。
func writeRestoreHistory(backupDir, repoKey string, result *RestoreResult) error {
	dir := filepath.Join(backupDir, "metadata-restore", strutil.SanitizePathToken(repoKey))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
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
		return err
	}
	name := time.Now().UTC().Format("20060102-150405.000") + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return err
	}
	return nil
}
