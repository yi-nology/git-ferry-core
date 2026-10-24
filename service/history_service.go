package service

import (
	"context"
	"encoding/json"

	"github.com/yi-nology/git-ferry-core/mirror/history"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 历史同步:请求/响应 DTO =====
//
// 壳层与 Agent 通过 HTTP 触达,JSON 形态与 Go 内部类型解耦:
// 规则种类用字符串、痕迹模式用正则源码,避免把内部枚举直接暴露给外部。
//
// **接口是通道优先的**:配置只在通道里,执行按 channel id 进行,
// 调用方不重传规则、也不回传服务端的临时路径。

// IdentityRuleDTO 是一条身份映射规则。
type IdentityRuleDTO struct {
	// Source 待替换的源标识。
	Source string `json:"source"`
	// Target 替换后的目标标识;为空表示剥离。
	Target string `json:"target"`
	// Kind 规则种类:module / host / literal,默认 module。
	Kind string `json:"kind"`
}

// PathRuleDTO 是一条路径规则。
type PathRuleDTO struct {
	// From 源仓路径前缀。
	From string `json:"from"`
	// To 目标仓路径前缀;为空表示排除。
	To string `json:"to"`
}

// ScrubRuleDTO 是一条痕迹检测与清理规则。
type ScrubRuleDTO struct {
	// Name 规则名。
	Name string `json:"name"`
	// Category 类别分组。
	Category string `json:"category"`
	// Pattern 正则源码。
	Pattern string `json:"pattern"`
	// Severity 处理强度:auto / mandatory,默认 auto。
	Severity string `json:"severity"`
	// Replacement 命中后的替换文本;auto 且为空表示删除。
	Replacement string `json:"replacement"`
}

// HistoryOptionsDTO 一次历史同步的**执行参数**(由通道解析而来)。
//
// 调用方不直接构造它:走 HistoryPlan/HistoryResolve 等方法时传 channel id,
// 由服务端按通道配置解析。保留它只是为了给「预演一份未入库的配置」用。
type HistoryOptionsDTO struct {
	// SourceDir 内部源仓本地路径。
	SourceDir string `json:"source_dir"`
	// TargetDir 公开目标仓本地路径。
	TargetDir string `json:"target_dir"`
	// SourceRef 源仓比对 ref,默认 HEAD。
	SourceRef string `json:"source_ref"`
	// TargetRef 目标仓分支,默认 main。
	TargetRef string `json:"target_ref"`
	// Rules 身份映射规则。
	Rules []IdentityRuleDTO `json:"rules"`
	// PathRules 路径规则。
	PathRules []PathRuleDTO `json:"path_rules"`
	// ScrubRules 痕迹规则。
	ScrubRules []ScrubRuleDTO `json:"scrub_rules"`
	// ConflictRules 冲突自动判据;空 = 不启用。
	ConflictRules []history.ConflictRuleConfig `json:"conflict_rules,omitempty"`
}

// HistoryPlanDTO 是一份同步计划的精简视图。
type HistoryPlanDTO struct {
	// ChannelID 计划所属的通道。
	ChannelID uint `json:"channel_id"`
	// Summary 统计。
	Summary history.Summary `json:"summary"`
	// Files 逐文件处置。
	Files []history.FilePlan `json:"files"`
	// Commits 按源侧提交分组的回滚视图。
	Commits []history.CommitPlan `json:"commits"`
	// Resolutions hold 文件的解法。
	Resolutions map[string]history.Resolution `json:"resolutions,omitempty"`
	// Dossiers 仍需人工裁决的卷宗(仅 resolve 时返回)。
	Dossiers []history.ConflictDossier `json:"dossiers,omitempty"`
}

// planDTO 把计划转成 DTO 视图。
func planDTO(channelID uint, plan *history.Plan, withDossiers bool) *HistoryPlanDTO {
	out := &HistoryPlanDTO{
		ChannelID:   channelID,
		Summary:     plan.Summary,
		Files:       plan.Files,
		Commits:     plan.Commits,
		Resolutions: plan.Resolutions,
	}
	if withDossiers {
		out.Dossiers = history.Dossiers(plan)
	}
	return out
}

// jsonString 序列化为 JSON 字符串(存库的列都是 TEXT)。
func jsonString(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// HistoryChannelInput 通道的创建/更新入参。
type HistoryChannelInput struct {
	// Name 通道名(唯一)。
	Name string `json:"name"`
	//
	// 下面这些字段用**指针**:区分「未指定」(nil)与「显式给值」
	// (含显式清空 —— 指向空串)。
	//
	// 为什么必须区分:更新是**部分更新**语义。用普通 string 的话,
	// 「只改一下目录」会把没提到的字段一起抹成空 ——
	// 身份映射(module_source/module_target)被抹掉后,
	// 执行时就没有任何身份规则,内网 module path 会**原样**进公开仓。
	// 这不是理论风险:只传目录的更新请求就能造成。
	//
	// 线上格式不变:指针与值序列化出来是同一个 JSON,
	// 缺字段 → nil,给了空串 → 指向空串。
	//
	// SourceRepoKey / TargetRepoKey 两侧仓库 key(取自 Repo 表)。
	SourceRepoKey *string `json:"source_repo_key"`
	TargetRepoKey *string `json:"target_repo_key"`
	// SourceDir / TargetDir 本地路径直连(与 repo key 二选一)。
	SourceDir *string `json:"source_dir"`
	TargetDir *string `json:"target_dir"`
	// ModuleSource / ModuleTarget 身份映射(创建时必填)。
	ModuleSource *string `json:"module_source"`
	ModuleTarget *string `json:"module_target"`
	// HostSource / HostTarget 主机名映射(可选)。
	HostSource *string `json:"host_source"`
	HostTarget *string `json:"host_target"`
	// AuthorName / AuthorEmail 目标侧新提交的作者身份(公开形态)。
	//
	// 留空则用中性的公开身份。**不继承源提交的作者** ——
	// 源侧作者是内网开发者,继承等于把内网人员信息发到公开仓。
	AuthorName  *string `json:"author_name"`
	AuthorEmail *string `json:"author_email"`
	// PathRules 路径规则。
	PathRules []PathRuleDTO `json:"path_rules"`
	// ScrubRules 痕迹规则。
	ScrubRules []ScrubRuleDTO `json:"scrub_rules"`
	// ConflictRules 冲突自动判据;空 = 不启用。
	ConflictRules []history.ConflictRuleConfig `json:"conflict_rules,omitempty"`
	// SourceBranch / TargetBranch 分支。
	SourceBranch *string `json:"source_branch"`
	TargetBranch *string `json:"target_branch"`
	// Enabled 是否启用。
	//
	// 用指针区分「未指定」(nil:创建时默认启用,更新时保持原值)
	// 与「显式禁用」(false)—— 用普通 bool 的话,false 与未指定无法区分。
	Enabled *bool `json:"enabled,omitempty"`
}

// HistoryChannelDTO 是通道的响应视图。
type HistoryChannelDTO struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// Targets 推送目标(顺序 = 优先级:先留底后发布)。
	Targets []*HistoryTargetDTO `json:"targets,omitempty"`
}

// HistoryTargetInput 是推送目标的创建/更新入参。
type HistoryTargetInput struct {
	// Name 目标名(backup / publish)。
	Name string `json:"name"`
	// RepoURL 远端地址。
	RepoURL string `json:"repo_url"`
	// CredType 凭据类型:none / token / ssh_key。
	CredType string `json:"cred_type"`
	// Credential 凭据明文(入参);落库时加密,响应不回显。
	Credential string `json:"credential"`
	// Username HTTPS Basic 用户名(token 类凭据用)。
	Username string `json:"username"`
}

// HistoryTargetDTO 是推送目标的响应视图(不回显凭据)。
type HistoryTargetDTO struct {
	ID            uint   `json:"id"`
	ChannelID     uint   `json:"channel_id"`
	Name          string `json:"name"`
	RepoURL       string `json:"repo_url"`
	CredType      string `json:"cred_type"`
	Username      string `json:"username"`
	HasCredential bool   `json:"has_credential"`
}

// HistoryRunDTO 是执行记录的响应视图。
type HistoryRunDTO struct {
	ID        uint `json:"id"`
	ChannelID uint `json:"channel_id"`
	// Kind 记录类别:apply / push / rollback。
	Kind string `json:"kind"`
	// PushedRunID push/rollback 引用被操作的 apply 记录。
	PushedRunID uint   `json:"pushed_run_id,omitempty"`
	Status      string `json:"status"`
	BaseHead    string `json:"base_head"`
	Head        string `json:"head"`
	PlanSummary string `json:"plan_summary"`
	Commits     string `json:"commits"`
	Files       string `json:"files"`
	Skipped     string `json:"skipped"`
	// ScrubReport 痕迹检测报告(JSON 字符串)。
	//
	// 形状:{"hits":[{rule,category,line,text,mandatory}...],
	//       "mandatory":[{...}...]}
	//   - hits:本次发布涉及的全部痕迹命中(含已自动清理的);
	//   - mandatory:其中**强制人工处理**的子集(密钥类)。
	//
	// 两者必须分开:自动清理类的命中**已随内容发布**,
	// 强制人工类的命中意味着**这个文件没有被发布**。
	// 混在一起会让人以为密钥已处理。
	ScrubReport string `json:"scrub_report"`
	GateReport  string `json:"gate_report"`
	RefsBefore  string `json:"refs_before"`
	RefsAfter   string `json:"refs_after"`
	Error       string `json:"error"`
	PrevHash    string `json:"prev_hash"`
	EntryHash   string `json:"entry_hash"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	CreatedAt   string `json:"created_at"`
}

// HistoryPushDTO 是一次推送的结果。
type HistoryPushDTO struct {
	// RunID 被推送的执行记录。
	RunID uint `json:"run_id"`
	// Pushed 成功推送的远端(按顺序)。
	Pushed []string `json:"pushed"`
	// Heads 各远端推送后的 HEAD。
	Heads map[string]string `json:"heads"`
	// Error 推送失败的原因(成功时为空)。
	//
	// 推送失败时**仍然要带回这条**:
	// 「移植成功、推送失败」是最需要人接手的局面 ——
	// 产物已经在本地,重试推送靠的就是它。
	Error string `json:"error,omitempty"`
}

// HistoryPushOptions 是推送的选项。
type HistoryPushOptions struct {
	// Force 强制推送:即使远端与本次移植的基线已分叉也照样覆盖。
	//
	// 默认不强制 —— 强推会把远端的提交**静默丢掉**
	// (实测:远端别人的提交被丢弃后只剩 0 条)。
	// 要强制,必须由人明确指定。
	Force bool `json:"force,omitempty"`
	// ForceApprovedBy 强制推送的批准者标识。
	//
	// **强制推送必须有批准者**:入口层校验角色是 admin 后把批准者填进来,
	// core 只认「有人批准过」这个事实。
	//
	// 为什么要下沉到 core:入口有三条(HTTP / MCP / AI 助手),
	// 任何一条忘了判角色,闸门就形同虚设 —— 放在 core 才能一劳永逸。
	// (实测:MCP 端点只判了「能写」,operator 就能强推覆盖远端提交。)
	ForceApprovedBy string `json:"force_approved_by,omitempty"`
}

// HistoryRollbackDTO 是回滚的结果。
type HistoryRollbackDTO struct {
	// RunID 回滚依据的执行记录。
	RunID uint `json:"run_id"`
	// Ref 回滚到的 ref。
	Ref string `json:"ref"`
	// Pushed 执行回滚的远端。
	Pushed []string `json:"pushed"`
	// Heads 回滚后各远端的 HEAD。
	Heads map[string]string `json:"heads"`
}

// ----- 通道 → 视图 -----

// toDTO 把通道转成响应视图。
func toDTO(ch *model.HistoryChannel) *HistoryChannelDTO {
	return &HistoryChannelDTO{
		ID:        ch.ID,
		Name:      ch.Name,
		Enabled:   ch.Enabled,
		CreatedAt: ch.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt: ch.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

// toTargetDTO 把推送目标转成响应视图(不回显凭据)。
func toTargetDTO(t *model.HistoryTarget) *HistoryTargetDTO {
	return &HistoryTargetDTO{
		ID:            t.ID,
		ChannelID:     t.ChannelID,
		Name:          t.Name,
		RepoURL:       t.RepoURL,
		CredType:      t.CredType,
		Username:      t.Username,
		HasCredential: t.Credential != "",
	}
}

// toRunDTO 把执行记录转成响应视图。
func toRunDTO(r *model.HistoryRun) *HistoryRunDTO {
	out := &HistoryRunDTO{
		ID:          r.ID,
		ChannelID:   r.ChannelID,
		Kind:        r.Kind,
		PushedRunID: r.PushedRunID,
		Status:      r.Status,
		BaseHead:    r.BaseHead,
		Head:        r.Head,
		PlanSummary: r.PlanSummary,
		Commits:     r.Commits,
		Files:       r.Files,
		Skipped:     r.Skipped,
		ScrubReport: r.ScrubReport,
		GateReport:  r.GateReport,
		RefsBefore:  r.RefsBefore,
		RefsAfter:   r.RefsAfter,
		Error:       r.Error,
		PrevHash:    r.PrevHash,
		EntryHash:   r.EntryHash,
		DurationMs:  r.DurationMs,
		CreatedAt:   r.CreatedAt.Format("2006-01-02 15:04:05"),
	}
	if r.StartedAt != nil {
		out.StartedAt = r.StartedAt.Format("2006-01-02 15:04:05")
	}
	if r.FinishedAt != nil {
		out.FinishedAt = r.FinishedAt.Format("2006-01-02 15:04:05")
	}
	return out
}

// ===== Service 级便捷入口 =====
//
// 壳层的 SyncService 接口由 *corebridge.Service 满足,
// 这里把历史同步的动作直接挂在 Service 上,免去调用方写 History().Xxx()。

// ListHistoryChannels 列出历史同步通道。
func (s *Service) ListHistoryChannels(ctx context.Context, offset, limit int) ([]*HistoryChannelDTO, int64, error) {
	return s.history.ListHistoryChannels(ctx, offset, limit)
}

// CreateHistoryChannel 新建历史同步通道。
func (s *Service) CreateHistoryChannel(ctx context.Context, in HistoryChannelInput) (*HistoryChannelDTO, error) {
	return s.history.CreateHistoryChannel(ctx, in)
}

// UpdateHistoryChannel 更新历史同步通道。
func (s *Service) UpdateHistoryChannel(ctx context.Context, id uint, in HistoryChannelInput) (*HistoryChannelDTO, error) {
	return s.history.UpdateHistoryChannel(ctx, id, in)
}

// GetHistoryChannel 取历史同步通道详情。
func (s *Service) GetHistoryChannel(ctx context.Context, id uint) (*HistoryChannelDTO, error) {
	return s.history.GetHistoryChannel(ctx, id)
}

// DeleteHistoryChannel 删除历史同步通道。
func (s *Service) DeleteHistoryChannel(ctx context.Context, id uint) error {
	return s.history.DeleteHistoryChannel(ctx, id)
}

// ListHistoryTargets 列出通道的推送目标。
func (s *Service) ListHistoryTargets(ctx context.Context, channelID uint) ([]*HistoryTargetDTO, error) {
	return s.history.ListHistoryTargets(ctx, channelID)
}

// CreateHistoryTarget 新建推送目标。
func (s *Service) CreateHistoryTarget(ctx context.Context, channelID uint, in HistoryTargetInput) (*HistoryTargetDTO, error) {
	return s.history.CreateHistoryTarget(ctx, channelID, in)
}

// UpdateHistoryTarget 更新推送目标。
func (s *Service) UpdateHistoryTarget(ctx context.Context, id uint, in HistoryTargetInput) (*HistoryTargetDTO, error) {
	return s.history.UpdateHistoryTarget(ctx, id, in)
}

// DeleteHistoryTarget 删除推送目标。
func (s *Service) DeleteHistoryTarget(ctx context.Context, id uint) error {
	return s.history.DeleteHistoryTarget(ctx, id)
}

// HistoryPlan 按通道生成同步计划。
func (s *Service) HistoryPlan(ctx context.Context, channelID uint) (*HistoryPlanDTO, error) {
	return s.history.HistoryPlan(ctx, channelID)
}

// HistoryResolve 按通道生成计划并计算 hold 的自动解法。
func (s *Service) HistoryResolve(ctx context.Context, channelID uint) (*HistoryPlanDTO, error) {
	return s.history.HistoryResolve(ctx, channelID)
}

// HistoryApply 按通道把计划落到目标树,产出执行记录(本地,不推送)。
func (s *Service) HistoryApply(ctx context.Context, channelID uint) (*HistoryRunDTO, error) {
	return s.history.HistoryApply(ctx, channelID)
}

// HistoryPush 按通道推送最近一次执行记录的产物。
func (s *Service) HistoryPush(ctx context.Context, channelID uint) (*HistoryPushDTO, error) {
	return s.history.HistoryPush(ctx, channelID)
}

// HistoryPushWithOptions 按通道推送(可指定强制推送)。
func (s *Service) HistoryPushWithOptions(ctx context.Context, channelID uint,
	opts HistoryPushOptions) (*HistoryPushDTO, error) {
	return s.history.HistoryPushWithOptions(ctx, channelID, opts)
}

// HistoryPushRun 推送指定执行记录的产物。
func (s *Service) HistoryPushRun(ctx context.Context, runID uint) (*HistoryPushDTO, error) {
	return s.history.HistoryPushRun(ctx, runID)
}

// ListHistoryRuns 列出通道的执行记录。
func (s *Service) ListHistoryRuns(ctx context.Context, channelID uint, offset, limit int) ([]*HistoryRunDTO, int64, error) {
	return s.history.ListHistoryRuns(ctx, channelID, offset, limit)
}

// GetHistoryRun 取执行记录详情。
func (s *Service) GetHistoryRun(ctx context.Context, id uint) (*HistoryRunDTO, error) {
	return s.history.GetHistoryRun(ctx, id)
}

// HistoryRollback 把目标树回滚到指定 ref。
func (s *Service) HistoryRollback(ctx context.Context, runID uint, ref string) (*HistoryRollbackDTO, error) {
	return s.history.HistoryRollback(ctx, runID, ref)
}

// VerifyHistoryChannelChain 校验通道内执行记录的哈希链。
func (s *Service) VerifyHistoryChannelChain(ctx context.Context, channelID uint) (*HistoryChainResult, error) {
	return s.history.VerifyHistoryChannelChain(ctx, channelID)
}

// SetHistoryOverride 写入(或覆盖)一条裁决。
func (s *Service) SetHistoryOverride(ctx context.Context, channelID uint, in HistoryOverrideInput) (*HistoryOverrideDTO, error) {
	return s.history.SetHistoryOverride(ctx, channelID, in)
}

// ListHistoryOverrides 列出通道的裁决。
func (s *Service) ListHistoryOverrides(ctx context.Context, channelID uint) ([]*HistoryOverrideDTO, error) {
	return s.history.ListHistoryOverrides(ctx, channelID)
}

// DeleteHistoryOverride 删除一条裁决。
func (s *Service) DeleteHistoryOverride(ctx context.Context, id uint) error {
	return s.history.DeleteHistoryOverride(ctx, id)
}

// RotateHistoryTargetCredential 轮换一个推送目标的凭据(只换密钥)。
func (s *Service) RotateHistoryTargetCredential(ctx context.Context, id uint, in HistoryCredentialInput) (*HistoryTargetDTO, error) {
	return s.history.RotateHistoryTargetCredential(ctx, id, in)
}

// HistorySync 一次跑完「预检 → 移植 → 推送」,有阻塞项则停下。
func (s *Service) HistorySync(ctx context.Context, opts HistorySyncOptions) (*HistorySyncReport, error) {
	return s.history.HistorySync(ctx, opts)
}

// HistoryPreview 执行预检(不推送)。
func (s *Service) HistoryPreview(ctx context.Context, channelID uint) (*PreviewReport, error) {
	return s.history.HistoryPreview(ctx, channelID)
}

// HistoryPreviewMarkdown 把预检报告渲染成可读文档。
func (s *Service) HistoryPreviewMarkdown(ctx context.Context, channelID uint) (string, error) {
	rep, err := s.history.HistoryPreview(ctx, channelID)
	if err != nil {
		return "", err
	}
	return historyPreviewMarkdown(rep), nil
}
