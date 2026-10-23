package service

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/yi-nology/git-ferry-core/mirror"
	"github.com/yi-nology/git-ferry-core/mirror/history"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// GateReport 是发布前的门禁结果。
type GateReport struct {
	// OK 全部门禁通过。
	OK bool `json:"ok"`
	// GofmtViolations 物化结果中违反 gofmt 的文件。
	GofmtViolations []string `json:"gofmt_violations,omitempty"`
	// TypeCheckErr 类型门禁的错误信息。
	TypeCheckErr string `json:"type_check_err,omitempty"`
	// Note 结论说明。
	Note string `json:"note"`
}

// TargetImpact 是一个推送目标的影响面。
type TargetImpact struct {
	// Name 目标名。
	Name string `json:"name"`
	// RepoURL 目标地址。
	RepoURL string `json:"repo_url"`
	// RemoteHead 远端当前的 HEAD(空 = 空仓库)。
	RemoteHead string `json:"remote_head"`
	// BaseHead 本地基线 HEAD(移植前的目标树 HEAD)。
	BaseHead string `json:"base_head"`
	// NewHead 推送后远端将变成的 HEAD。
	NewHead string `json:"new_head"`
	// FastForward 推送是快进(不需要强推)。
	FastForward bool `json:"fast_forward"`
	// Diverged 远端与本地基线已分叉(需要强推,且要人工确认)。
	Diverged bool `json:"diverged"`
	// Unknown 远端不可达或状态未知。
	Unknown bool `json:"unknown"`
	// Note 说明。
	Note string `json:"note"`
}

// PreviewReport 是预检报告。
//
// 预检 = **把这次同步的全部影响面摊开**,但不推送:
// 计划统计、将产生的提交、将改动的文件、门禁结果(推出去能不能编译)、
// 每个推送目标的 ref 变化与是否需要强推、待人工裁决的冲突。
type PreviewReport struct {
	// ChannelID 通道 ID。
	ChannelID uint `json:"channel_id"`
	// ChannelName 通道名。
	ChannelName string `json:"channel_name"`
	// Summary 计划统计。
	Summary history.Summary `json:"summary"`
	// BaseHead 目标侧当前 HEAD(既有提交原样保留的基线)。
	BaseHead string `json:"base_head"`
	// NewHead 预演后的 HEAD(推送后会变成这样)。
	NewHead string `json:"new_head"`
	// Commits 将要产生的提交。
	Commits []history.AppliedCommit `json:"commits"`
	// AppliedFiles 将要应用的文件。
	AppliedFiles []string `json:"applied_files"`
	// SkippedFiles 不会带过去的文件(待人工裁决/排除)。
	SkippedFiles []string `json:"skipped_files"`
	// ProtectedCount 受保护文件数(不会动)。
	ProtectedCount int `json:"protected_count"`
	// PendingConflicts 待人工裁决的冲突。
	PendingConflicts []history.ConflictDossier `json:"pending_conflicts,omitempty"`
	// ScrubHits 痕迹命中。
	ScrubHits []history.ScrubHit `json:"scrub_hits,omitempty"`
	// Gate 门禁结果。
	Gate GateReport `json:"gate"`
	// Targets 各推送目标的影响面。
	Targets []TargetImpact `json:"targets"`
	// Credentials 各推送目标的凭据状态。
	Credentials []CredentialStatus `json:"credentials,omitempty"`
	// NeedsForce 是否需要强推。
	NeedsForce bool `json:"needs_force"`
	// Warnings 警告(不阻断,但要让人看见)。
	Warnings []string `json:"warnings,omitempty"`
	// PlanMarkdown 计划的 Markdown 视图(便于人读)。
	PlanMarkdown string `json:"plan_markdown,omitempty"`
}

// HistoryPreview 执行预检。
//
// **不推送、不改原始目标仓**:物化与门禁都在临时工作副本上进行。
// 预检的结论必须与真正执行时的结论一致 —— 两者跑的是同一套代码路径。
func (h *HistoryService) HistoryPreview(ctx context.Context, channelID uint) (*PreviewReport, error) {
	ch, opts, err := h.channelAndOptions(ctx, channelID)
	if err != nil {
		return nil, err
	}
	plan, err := history.BuildPlan(ctx, *opts)
	if err != nil {
		return nil, err
	}
	if err := history.Resolve(ctx, *opts, plan); err != nil {
		return nil, err
	}
	if err := h.applyOverrides(ctx, channelID, plan); err != nil {
		return nil, err
	}

	report := &PreviewReport{
		ChannelID:        channelID,
		ChannelName:      ch.Name,
		Summary:          plan.Summary,
		PendingConflicts: history.Dossiers(plan),
		PlanMarkdown:     plan.Markdown(),
	}

	// 物化 + 门禁:回答「这次推出去能不能编译」。
	workDir, err := os.MkdirTemp("", "history-preview-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	applyRes, err := history.Apply(ctx, history.ApplyOptions{
		SourceDir:  opts.SourceDir,
		TargetDir:  opts.TargetDir,
		TargetRef:  opts.TargetRef,
		Rules:      opts.Rules,
		PathRules:  opts.PathRules,
		ScrubRules: opts.ScrubRules,
		Plan:       plan,
		WorkDir:    workDir,
	})
	if err != nil {
		return nil, fmt.Errorf("预演移植失败: %w", err)
	}
	report.BaseHead = applyRes.BaseHead
	report.NewHead = applyRes.Head
	report.Commits = applyRes.Commits
	report.AppliedFiles = applyRes.AppliedFileList()
	report.SkippedFiles = applyRes.Skipped
	report.ScrubHits = applyRes.MandatoryHits
	for _, f := range plan.Files {
		if f.Disposition == history.DispositionPreserve {
			report.ProtectedCount++
		}
	}

	// 门禁:物化出来的那棵树,gofmt 干净吗?类型检查过吗?
	report.Gate = h.gateCheck(ctx, applyRes.RepoPath)

	// 推送影响面:各远端现在的 HEAD、与基线的关系、推送后会变成什么。
	targets, err := h.targets.FindByChannel(channelID)
	if err != nil {
		return nil, err
	}
	branch := ch.TargetBranch
	if branch == "" {
		branch = "main"
	}
	for _, t := range targets {
		impact, err := h.checkTargetImpact(ctx, applyRes.RepoPath, applyRes.BaseHead, applyRes.Head, branch, t)
		if err != nil {
			return nil, err
		}
		report.Targets = append(report.Targets, impact)
		if impact.Diverged {
			report.NeedsForce = true
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("远端 %s 与本地基线已分叉,推送需要强推", t.Name))
		}
		if impact.Unknown {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("远端 %s 状态未知(不可达或空仓库),推送前请确认", t.Name))
		}
	}
	report.Credentials = h.credentialStatus(targets)
	for _, c := range report.Credentials {
		if c.Expired || c.ExpiringSoon {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("目标 %s 的凭据%s", c.Name, c.Note))
		}
	}
	if len(report.PendingConflicts) > 0 {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("有 %d 个冲突待人工裁决,未裁决的文件不会带过去", len(report.PendingConflicts)))
	}
	if !report.Gate.OK {
		report.Warnings = append(report.Warnings, "门禁未通过,推送会被中止")
	}
	return report, nil
}

// gateCheck 对物化出来的树跑门禁(gofmt + 全量类型检查)。
func (h *HistoryService) gateCheck(ctx context.Context, dir string) GateReport {
	g := GateReport{OK: true}
	g.GofmtViolations = history.CheckGofmt(dir)
	if len(g.GofmtViolations) > 0 {
		g.OK = false
	}
	if err := mirror.TypeCheckGate(ctx, dir); err != nil {
		g.OK = false
		g.TypeCheckErr = err.Error()
	}
	switch {
	case !g.OK && g.TypeCheckErr != "":
		g.Note = "格式或类型门禁未通过,不能推送"
	case !g.OK:
		g.Note = "格式门禁未通过,不能推送"
	default:
		g.Note = "门禁通过:物化结果 gofmt 干净且类型检查通过"
	}
	return g
}

// checkTargetImpact 检查一个推送目标的影响面。
//
// 关键:远端当前 HEAD 与本地基线的关系 ——
//   - 相同 → 推送是快进,安全;
//   - 远端 HEAD 是基线的后代 → 远端领先,本地要先 rebase(重新计划);
//   - 否则 → 已分叉,推送需要强推,必须人工确认。
func (h *HistoryService) checkTargetImpact(ctx context.Context, workRepoPath, baseHead, newHead, branch string,
	t *model.HistoryTarget) (TargetImpact, error) {

	imp := TargetImpact{
		Name: t.Name, RepoURL: t.RepoURL,
		BaseHead: baseHead, NewHead: newHead,
	}
	auth, err := h.targetAuth(t)
	if err != nil {
		return imp, err
	}
	repo, err := git.PlainOpen(workRepoPath)
	if err != nil {
		return imp, fmt.Errorf("打开工作仓库失败: %w", err)
	}
	if _, err := ensureRemote(repo, t.Name, t.RepoURL); err != nil {
		return imp, err
	}
	// 拉取远端该分支的最新状态。
	if _, err := h.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: workRepoPath,
		Remote:   t.Name,
		Branches: []string{branch},
		Auth:     auth,
	}); err != nil {
		imp.Unknown = true
		if isEmptyRemoteErr(err) {
			// 空仓库是首次发布的正常状态,不是异常。
			imp.Note = "远端是空仓库(首次发布),推送会创建该分支"
		} else {
			imp.Note = "拉取远端状态失败:" + err.Error()
		}
		return imp, nil
	}
	remoteRef := plumbing.NewRemoteReferenceName(t.Name, branch)
	ref, err := repo.Reference(remoteRef, true)
	if err != nil {
		imp.Unknown = true
		imp.Note = "远端没有分支 " + branch + "(空仓库?)"
		return imp, nil
	}
	imp.RemoteHead = ref.Hash().String()

	switch {
	case imp.RemoteHead == baseHead:
		imp.FastForward = true
		imp.Note = "远端与本地基线一致,推送是快进"
	case isAncestorCommit(repo, baseHead, imp.RemoteHead):
		// 远端领先:本地基线是远端的祖先 → 推送会覆盖远端的新提交。
		imp.Diverged = true
		imp.Note = "远端已领先于本地基线,直接推送会覆盖远端的新提交,需重新计划或强推"
	default:
		imp.Diverged = true
		imp.Note = "远端与本地基线已分叉,推送需要强推"
	}
	return imp, nil
}

// isEmptyRemoteErr 判断拉取失败是否因为远端是空仓库(分支不存在)。
func isEmptyRemoteErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "couldn't find remote ref") ||
		strings.Contains(msg, "remote ref does not exist") ||
		strings.Contains(msg, "not found") && strings.Contains(msg, "refs/heads/")
}

// isAncestorCommit 判断 anc 是否是 desc 的祖先。
func isAncestorCommit(repo *git.Repository, anc, desc string) bool {
	ancHash := plumbing.NewHash(anc)
	descHash := plumbing.NewHash(desc)
	if ancHash.IsZero() || descHash.IsZero() {
		return false
	}
	seen := make(map[plumbing.Hash]bool)
	queue := []plumbing.Hash{descHash}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if cur == ancHash {
			return true
		}
		commit, err := repo.CommitObject(cur)
		if err != nil {
			continue
		}
		queue = append(queue, commit.ParentHashes...)
	}
	return false
}

// historyPreviewMarkdown 把预检报告渲染成可读文档。
func historyPreviewMarkdown(r *PreviewReport) string {
	var b strings.Builder
	b.WriteString("# 历史同步预检报告\n\n")
	fmt.Fprintf(&b, "- 通道:%s(id=%d)\n", r.ChannelName, r.ChannelID)
	fmt.Fprintf(&b, "- 目标树 HEAD:%s → 预演后:%s\n", shortHash(r.BaseHead), shortHash(r.NewHead))
	fmt.Fprintf(&b, "- 统计:same=%d port=%d hold=%d(已裁决 %d) preserve=%d 痕迹命中=%d\n",
		r.Summary.Same, r.Summary.Port, r.Summary.Hold, r.Summary.Resolved,
		r.Summary.Preserve, r.Summary.ScrubHits)

	b.WriteString("\n## 门禁\n\n")
	fmt.Fprintf(&b, "- 结论:%s\n", r.Gate.Note)
	if len(r.Gate.GofmtViolations) > 0 {
		fmt.Fprintf(&b, "- gofmt 违规:%d 个\n", len(r.Gate.GofmtViolations))
	}
	if r.Gate.TypeCheckErr != "" {
		fmt.Fprintf(&b, "- 类型门禁:%s\n", r.Gate.TypeCheckErr)
	}

	b.WriteString("\n## 推送影响面\n\n")
	b.WriteString("| 目标 | 远端当前 HEAD | 快进 | 说明 |\n|---|---|---|---|\n")
	for _, t := range r.Targets {
		ff := "是"
		if !t.FastForward {
			ff = "否"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n",
			t.Name, shortHash(t.RemoteHead), ff, t.Note)
	}
	if r.NeedsForce {
		b.WriteString("\n⚠️ **需要强推**\n")
	}

	if len(r.Commits) > 0 {
		b.WriteString("\n## 将产生的提交\n\n")
		for _, c := range r.Commits {
			fmt.Fprintf(&b, "- `%s` %s(%d 个文件)\n", shortHash(c.SourceCommit), c.SourceSubject, len(c.Files))
		}
	}
	if len(r.PendingConflicts) > 0 {
		b.WriteString("\n## 待人工裁决\n\n")
		for _, c := range r.PendingConflicts {
			fmt.Fprintf(&b, "- `%s`(%s)\n", c.Path, c.Verdict)
		}
	}
	if len(r.Warnings) > 0 {
		b.WriteString("\n## 警告\n\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	return b.String()
}

// shortHash 取 hash 前 8 位。
func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	if h == "" {
		return "(空)"
	}
	return h
}

// ===== 一次命令:预检 → 移植 → 推送 =====

// HistorySyncOptions 是一次同步的选项。
type HistorySyncOptions struct {
	// ChannelID 通道 ID。
	ChannelID uint
	// Force 有阻塞项时也继续(调用方仍应让用户确认)。
	Force bool
}

// HistorySyncReport 是一次同步的结果。
type HistorySyncReport struct {
	// Preview 预检结论。
	Preview *PreviewReport `json:"preview"`
	// Apply 移植结果(未执行时为空)。
	Apply *HistoryRunDTO `json:"apply,omitempty"`
	// Push 推送结果(未执行时为空)。
	Push *HistoryPushDTO `json:"push,omitempty"`
	// Blocked 阻塞项;非空表示**没有往下走**。
	Blocked []string `json:"blocked,omitempty"`
}

// HistorySync 一次跑完「预检 → 移植 → 推送」。
//
// 有阻塞项时**停下并报告**,不擅自往下走:
//   - 门禁不通过(推出去编译不过);
//   - 还有待人工裁决的冲突;
//   - 需要强推(可能覆盖远端别人的提交)。
//
// 这三件事都不是「程序能替用户决定的」—— 停下让人看,比推上去再回滚便宜。
func (h *HistoryService) HistorySync(ctx context.Context, opts HistorySyncOptions) (*HistorySyncReport, error) {
	pv, err := h.HistoryPreview(ctx, opts.ChannelID)
	if err != nil {
		return nil, err
	}
	report := &HistorySyncReport{Preview: pv}

	if !pv.Gate.OK {
		report.Blocked = append(report.Blocked, "门禁未通过:"+pv.Gate.Note)
	}
	if len(pv.PendingConflicts) > 0 {
		report.Blocked = append(report.Blocked,
			fmt.Sprintf("有 %d 个冲突待人工裁决", len(pv.PendingConflicts)))
	}
	if pv.NeedsForce {
		report.Blocked = append(report.Blocked, "推送需要强推(远端与本地基线已分叉)")
	}
	if len(report.Blocked) > 0 && !opts.Force {
		return report, nil
	}

	applyRes, err := h.HistoryApply(ctx, opts.ChannelID)
	if err != nil {
		return nil, err
	}
	report.Apply = applyRes

	pushRes, err := h.HistoryPush(ctx, opts.ChannelID)
	if err != nil {
		return nil, err
	}
	report.Push = pushRes
	return report, nil
}
