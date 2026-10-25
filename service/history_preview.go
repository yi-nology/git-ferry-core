package service

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	errors "github.com/cockroachdb/errors"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"

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
	ch, opts, plan, err := h.planAndResolve(ctx, channelID)
	if err != nil {
		return nil, err
	}

	// 物化 + 门禁:回答「这次推出去能不能编译」。
	//
	// 预检的物化落在**临时目录**(看完即删);执行路径落在产物目录(推送要用)。
	workDir, err := os.MkdirTemp("", "history-preview-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	res, err := h.materialize(ctx, *opts, plan, workDir, nil)
	if err != nil {
		return nil, fmt.Errorf("预演移植失败: %w", err)
	}
	gate := h.gateCheck(ctx, res.RepoPath, internalIdentifiers(ch)...)
	return h.previewFrom(ctx, ch, plan, res, gate)
}

// planAndResolve 算计划、解冲突、应用人工裁决 —— **一次调用内只算一遍**。
//
// 单独调 preview / apply 时各算一次是对的:两次调用之间仓可能变了,
// 必须按当下的仓重算。但**一次调用内**绝不能算两遍 ——
// 一键同步原本 preview 算一遍、apply 又算一遍,等于把流水线跑两遍
// (实仓上 plan+resolve≈65s、物化≈50s、类型门禁≈60s)。
//
// 更要紧的是**一致性**:两遍算出的 new_head 可能不同
// (两次计算之间仓变了)⇒ 预检报告说的树不是真正要推的那棵树。
func (h *HistoryService) planAndResolve(ctx context.Context, channelID uint) (
	*model.HistoryChannel, *history.Options, *history.Plan, error) {
	ch, opts, err := h.channelAndOptions(ctx, channelID)
	if err != nil {
		return nil, nil, nil, err
	}
	plan, err := history.BuildPlan(ctx, *opts)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := history.Resolve(ctx, *opts, plan); err != nil {
		return nil, nil, nil, err
	}
	if err := h.applyOverrides(ctx, channelID, plan); err != nil {
		return nil, nil, nil, err
	}
	return ch, opts, plan, nil
}

// materialize 把计划物化成工作仓(写盘 + 生成新提交)。
//
// workDir 决定产物落在哪:预检用临时目录(看完即删),
// 执行用服务自己的产物目录(推送要用)。keepDirs 是要保留的产物目录
// (还没推送成功的那些)。
func (h *HistoryService) materialize(ctx context.Context, opts history.Options, plan *history.Plan,
	workDir string, keepDirs []string) (*history.ApplyResult, error) {
	h.materializeRuns.Add(1)
	return history.Apply(ctx, history.ApplyOptions{
		SourceDir: opts.SourceDir,
		TargetDir: opts.TargetDir,
		TargetRef: opts.TargetRef,
		// 源 ref 要与计划一致(否则计划与写盘不是同一棵树)。
		SourceRef:  opts.SourceRef,
		Rules:      opts.Rules,
		PathRules:  opts.PathRules,
		ScrubRules: opts.ScrubRules,
		Plan:       plan,
		WorkDir:    workDir,
		// **作者身份也要一致**:作者参与 commit hash ——
		// 预检用默认公开作者算出的 new_head,与真实产物(配了作者)对不上,
		// 用户按预检的 head 做事后核对必然对不上。
		AuthorName:      opts.AuthorName,
		AuthorEmail:     opts.AuthorEmail,
		KeepProductDirs: keepDirs,
	})
}

// previewFrom 从「已物化的结果 + 门禁结论」组装预检报告。
//
// **预览与执行共用同一份物化结果** —— 一键同步因此不必跑两遍流水线,
// 而且报告说的就是真正要推的那棵树(报告里的 new_head 与推送的 head 逐字一致)。
func (h *HistoryService) previewFrom(ctx context.Context, ch *model.HistoryChannel,
	plan *history.Plan, res *history.ApplyResult, gate GateReport) (*PreviewReport, error) {

	report := &PreviewReport{
		ChannelID:        ch.ID,
		ChannelName:      ch.Name,
		Summary:          plan.Summary,
		PendingConflicts: history.Dossiers(plan),
		PlanMarkdown:     plan.Markdown(),
		BaseHead:         res.BaseHead,
		NewHead:          res.Head,
		Commits:          res.Commits,
		AppliedFiles:     res.AppliedFileList(),
		SkippedFiles:     res.Skipped,
		ScrubHits:        res.ScrubHits,
		Gate:             gate,
	}
	for _, f := range plan.Files {
		if f.Disposition == history.DispositionPreserve {
			report.ProtectedCount++
		}
	}

	// 推送影响面:各远端现在的 HEAD、与基线的关系、推送后会变成什么。
	targets, err := h.targets.FindByChannel(ch.ID)
	if err != nil {
		return nil, err
	}
	branch := ch.TargetBranch
	if branch == "" {
		branch = "main"
	}
	for _, t := range targets {
		impact, err := h.checkTargetImpact(ctx, res.RepoPath, res.BaseHead, res.Head, branch, t)
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
//
// ids 是要脱敏的内网标识(见 internalIdentifiers):门禁错误会流进
// 模型上下文(agent 调工具 → 错误进 prompt)与审计记录,
// 而通道 DTO **不暴露** module_source / host_source ——
// 错误文本就成了内网标识外流的口子。
func (h *HistoryService) gateCheck(ctx context.Context, dir string, ids ...string) GateReport {
	g := GateReport{OK: true}
	g.GofmtViolations = history.CheckGofmt(dir)
	if len(g.GofmtViolations) > 0 {
		g.OK = false
	}
	if err := mirror.TypeCheckGate(ctx, dir); err != nil {
		g.OK = false
		g.TypeCheckErr = redactIdentifiers(err.Error(), ids)
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

// internalIdentifiers 列出通道的内网标识(按长度降序,长的先替换)。
//
// 为什么连「路径部分」一起脱敏:远端的报错原文里常见
// "Cannot find repository: <org>/<repo>" —— 那是 module path 去掉主机段的部分。
//
// 为什么长���先替换:否则主机名会先把完整的 module path 拦腰截断,
// 剩下 "/org/repo" 裸露在外,等于没脱敏。
func internalIdentifiers(ch *model.HistoryChannel) []string {
	if ch == nil {
		return nil
	}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		out = append(out, s)
		if i := strings.Index(s, "/"); i >= 0 && i+1 < len(s) {
			out = append(out, s[i+1:])
		}
	}
	add(ch.ModuleSource)
	add(ch.HostSource)
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// redactIdentifiers 把内网标识从文本里抹成占位符。
func redactIdentifiers(text string, ids []string) string {
	if text == "" || len(ids) == 0 {
		return text
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		text = strings.ReplaceAll(text, id, "[内网标识已脱敏]")
	}
	return text
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
		// URL 脱敏:预检报告会进 CLI 输出与模型上下文,而它可能内嵌凭据。
		Name: t.Name, RepoURL: history.RedactRepoURL(t.RepoURL),
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
	if err := history.EnsureRemote(repo, t.Name, t.RepoURL); err != nil {
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

	// 判定与推送闸门共用同一份(classifyRemote)。
	// 以前这里只认"远端领先"与"其它"两种,把**远端落后**(推送闸门明确放行)
	// 与**重复发布**都报成"已分叉需强推" ⇒ 报告与事实相反,且 NeedsForce
	// 会把一次本可安全完成的同步卡住。
	rel, note := classifyRemote(repo, imp.RemoteHead, baseHead, newHead)
	applyRemoteRelation(&imp, rel, note)
	return imp, nil
}

// isEmptyRemoteErr 判断拉取失败是否只是因为「远端没有这条分支 / 远端是空仓」
// —— 两者都是**首次发布的正常状态**,不是异常。
//
// **必须精确判空**(实测三种真实错误形态):
//
//	远端完全空(首次发布)  → "remote repository is empty"
//	                        = transport.ErrEmptyRemoteRepository
//	远端有内容但无该分支  → "couldn't find remote ref \"refs/heads/main\""
//	远端不存在            → "repository not found"
//
// 旧实现用字符串匹配三个子句,**真正的空仓一个都没命中** ——
// 于是首次发布到空远端时预检报「远端状态未知(不可达或空仓库),推送前请确认」,
// 把正常状态说成了异常。而 "not found" + "refs/heads/" 那一支反过来会把
// 「远端不存在 / 认证失败」误判成「空仓库(首次发布),推送会创建该分支」——
// 让人以为可以放心首发。
//
// 镜像发布侧早已改用 errors.Is 判同一件事(见 mirror/mirror.go),
// 这里跟着统一:**同一事实只有一处判定**。
func isEmptyRemoteErr(err error) bool {
	if err == nil {
		return false
	}
	// 远端完全空:go-git 的哨兵错误(经 gitbackend 的包装后仍可 errors.Is 命中)。
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return true
	}
	// 远端有内容但没有这条分支:git 的标准措辞。
	msg := err.Error()
	return strings.Contains(msg, "couldn't find remote ref") ||
		strings.Contains(msg, "remote ref does not exist")
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
	// ForcePush 强制推送:远端与本次移植的基线已分叉也照样覆盖。
	//
	// 与 Force 分开:「忽略阻塞项继续」和「覆盖远端已有的提交」是两件事,
	// 不该共用一个开关 —— 后者会丢掉远端别人的提交。
	ForcePush bool
	// ForcePushApprovedBy 强制推送的批准者标识(非空 = 已有人批准)。
	//
	// **强制推送必须有批准者**:入口层校验角色是 admin 后把批准者填进来,
	// core 只认「有人批准过」这个事实。放在 core 才能一劳永逸 ——
	// 入口有三条(HTTP / MCP / AI 助手),任何一条忘了判角色闸门就形同虚设。
	ForcePushApprovedBy string
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
	// 一次锁住整个通道:预检 → 移植 → 推送必须串行。
	// (锁不可重入,所以下面走的是内部实现,不再各自取锁。)
	release, err := h.lockChannel(ctx, opts.ChannelID)
	if err != nil {
		return nil, err
	}
	defer release()

	// **整条流水线只跑一遍**:计划算一次、物化一次、门禁一次。
	//
	// 旧实现是「先 HistoryPreview(临时目录物化 + 门禁)再 applyRun(产物目录物化 + 门禁)」:
	// 实仓上 plan+resolve≈65s、物化≈50s、类型门禁≈60s —— 等于把流水线跑两遍。
	// 更要紧的是**一致性**:两遍算出的 new_head 可能不同
	// (两次计算之间仓变了)⇒ 预检报告说的不是真正要推的那棵树。
	ch, hopts, plan, err := h.planAndResolve(ctx, opts.ChannelID)
	if err != nil {
		return nil, err
	}
	out, err := h.applyWithPlan(ctx, ch, hopts, plan)
	if err != nil {
		return nil, err
	}
	report := &HistorySyncReport{Apply: out.Run}

	if out.Res == nil {
		// 物化就失败了:没有可预览的树。
		report.Blocked = append(report.Blocked, "移植未成功:"+out.Run.Error)
		return report, nil
	}
	pv, err := h.previewFrom(ctx, ch, plan, out.Res, out.Gate)
	if err != nil {
		return nil, err
	}
	report.Preview = pv

	// **移植自己失败(如门禁未过)就不要再往下走**。
	//
	// applyWithPlan 把门禁结论写进执行记录,门禁不通过时状态就是 failed。
	// 此时继续往下走等于「明知编译不过还去推」—— 推送侧也会独立核对并拒绝,
	// 但没必要走到那一步才失败:报告里已经写明原因了。
	//
	// **这条不看 Force**:Force 是「忽略阻塞项继续」,不是「允许推编译不过的树」。
	if out.Run.Status != model.HistoryRunSuccess {
		report.Blocked = append(report.Blocked, "移植未成功:"+out.Run.Error)
		return report, nil
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

	// **强制推送的闸门放在 core**:没有批准者就不许强推。
	// 入口层(HTTP/MCP/AI)负责校验「谁是批准者」(admin),core 只认这个事实。
	if opts.ForcePush && opts.ForcePushApprovedBy == "" {
		return report, fmt.Errorf("强制推送需要批准者(admin 角色):没有批准者就不能覆盖远端已有的提交")
	}
	pushRes, err := h.pushChannelLatest(ctx, opts.ChannelID, opts.ForcePush)
	if err != nil {
		// **移植已经成功、推送失败**:把已完成的移植结果带回去。
		// 直接返回 nil 会让调用方以为什么都没发生,产物路径也丢了 ——
		// 而产物正是重试推送时唯一能用的东西。
		report.Push = &HistoryPushDTO{Error: err.Error()}
		return report, fmt.Errorf("移植成功但推送失败: %w", err)
	}
	report.Push = pushRes
	return report, nil
}
