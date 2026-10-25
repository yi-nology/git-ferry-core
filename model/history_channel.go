package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// 历史同步执行状态
const (
	HistoryRunPending = "pending"
	HistoryRunRunning = "running"
	HistoryRunSuccess = "success"
	HistoryRunFailed  = "failed"
)

// 推送目标凭据类型
const (
	HistoryCredNone   = "none"
	HistoryCredToken  = "token"
	HistoryCredSSHKey = "ssh_key"
)

// HistoryChannel 历史同步通道:两个各自有完整历史的仓库之间的单向内容同步。
//
// 与 MirrorChannel(镜像通道/身份镜像)并存,语义不同:
//   - MirrorChannel:同一仓库的多个身份,每 tag 一个孤儿快照;
//   - HistoryChannel:两个有完整历史的仓库,按内容差异叠加,
//     目标树的既有提交原样保留。
//
// **通道是历史同步的唯一配置入口**:规则只存这里,
// 执行时按通道解析,不再由调用方每次重传一整套规则。
type HistoryChannel struct {
	ID   uint   `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:128;not null;uniqueIndex" json:"name"`

	// SourceRepoKey / TargetRepoKey:两侧仓库的 key(取自 Repo 表)。
	// 为空则退回 SourceDir / TargetDir(本机已有克隆时直连)。
	SourceRepoKey string `gorm:"size:128;index" json:"sourceRepoKey"`
	TargetRepoKey string `gorm:"size:128;index" json:"targetRepoKey"`
	// SourceDir / TargetDir:本地路径直连。
	SourceDir string `gorm:"size:512" json:"sourceDir"`
	TargetDir string `gorm:"size:512" json:"targetDir"`

	// 身份映射:module 必填(改写 import 与 go.mod),host 可选。
	ModuleSource string `gorm:"size:255" json:"moduleSource"`
	ModuleTarget string `gorm:"size:255" json:"moduleTarget"`
	HostSource   string `gorm:"size:255" json:"hostSource"`
	HostTarget   string `gorm:"size:255" json:"hostTarget"`

	// AuthorName / AuthorEmail 目标侧新提交的作者身份(公开形态)。
	//
	// **不继承源提交的作者**:源侧是内网仓,作者是内网开发者
	// (姓名 + 企业邮箱)—— 直接继承等于把内网人员信息发布到公开仓。
	// 留空则用中性的公开身份(见 history.DefaultPublicAuthor)。
	AuthorName  string `gorm:"size:128" json:"authorName"`
	AuthorEmail string `gorm:"size:255" json:"authorEmail"`

	// LastEntryHash 链尾锚点:该通道**最后一条已入链**执行记录的哈希。
	//
	// 作用:检出「链尾被截断」。链本身只能证明「前后接得上」,
	// 删掉末尾几条记录,剩下的链照样接得上 —— 有了这个锚点,
	// 锚点指向的记录不在了就说明链被截断了。
	LastEntryHash string `gorm:"size:64;index" json:"lastEntryHash"`

	// PathRules 路径规则 JSON:[{"from":"docs/internal","to":"docs/archive"}]。
	// to 为空表示排除该路径。
	PathRules string `gorm:"type:text" json:"pathRules"`
	// ScrubRules 痕迹规则 JSON:
	// [{"name":"...","category":"...","pattern":"...","severity":"auto|mandatory","replacement":"..."}]
	ScrubRules string `gorm:"type:text" json:"scrubRules"`
	// ConflictRules 冲突自动判据 JSON:
	// [{"kind":"internal_trace|placeholder_domain|public_asset","enabled":true,"patterns":["..."]}]
	//
	// 空 = 不启用,所有冲突都交人工/AI。启用后,确定性判定解不了的冲突
	// 会先试这些判据(内网形态 → 保留目标侧)。
	ConflictRules string `gorm:"type:text" json:"conflictRules"`

	// SourceBranch 源分支,默认 HEAD。
	SourceBranch string `gorm:"size:128" json:"sourceBranch"`
	// TargetBranch 目标分支,默认 main。
	TargetBranch string `gorm:"size:128" json:"targetBranch"`

	// Enabled 关闭的通道不参与执行。
	//
	// **不用 gorm default:true** —— GORM 遇到零值 false 会省略该字段,
	// 让数据库默认值 true 生效,于是「显式禁用」会被悄悄改成「启用」。
	// 默认值由服务层负责(创建时未指定即启用)。
	Enabled bool `gorm:"not null" json:"enabled"`

	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`

	// Targets 响应投影。
	Targets []*HistoryTarget `gorm:"-" json:"targets,omitempty"`
}

// HistoryTarget 通道的推送目标。
//
// 顺序即优先级:**第一个是留底(本地裸镜像),其后才是发布(GitHub 真上游)**。
// 留底必须先落地,否则发布失败就没有回滚依据。
type HistoryTarget struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	ChannelID uint `gorm:"index;not null" json:"channelId"`
	// Name 目标名(backup / publish)。
	Name string `gorm:"size:64;not null" json:"name"`
	// RepoURL 是目标仓库的 git 地址(https/ssh/本地路径)。
	RepoURL string `gorm:"size:512;not null" json:"repoUrl"`

	// 凭据:Credential 整体加密存储,响应只回 HasCredential。
	CredType      string `gorm:"size:16;not null;default:none" json:"credType"`
	Credential    string `gorm:"size:2048" json:"-"`
	Username      string `gorm:"size:128" json:"username"`
	HasCredential bool   `gorm:"-" json:"hasCredential"`
	// CredentialUpdatedAt 凭据最后更新时间(轮换时刷新)。
	CredentialUpdatedAt *time.Time `json:"credentialUpdatedAt,omitempty"`
	// CredentialExpiresAt 凭据到期时间(可选);预检时会对过期/临期告警。
	CredentialExpiresAt *time.Time `json:"credentialExpiresAt,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// 历史同步执行记录的类别。
const (
	// HistoryRunKindApply 内容移植:产出新提交。
	HistoryRunKindApply = "apply"
	// HistoryRunKindPush 推送:把某次移植的产物推到远端。
	HistoryRunKindPush = "push"
	// HistoryRunKindRollback 回滚:把目标分支退回某个 ref。
	HistoryRunKindRollback = "rollback"
)

// HistoryRun 历史同步的一次执行记录。
//
// 它同时承担三个角色:
//   - **产物指针**:WorkRepoPath 记下移植产出的工作仓库路径,
//     推送时服务端自己取,调用方不必回传服务端的临时路径;
//   - **审计留痕**:计划统计 / commit 映射 / 检测报告 / 门禁结果 / 推送前后 ref 对照;
//   - **回滚锚点**:BaseHead 与 Head 界定了「这次同步改了什么」。
type HistoryRun struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// ChannelID 通道 ID。
	//
	// 复合索引 idx_history_runs_channel_kind 覆盖
	// 「取通道最近一次某类别的执行记录」这类查询
	// (FindLatestByChannelKind:channel_id + kind)。
	ChannelID uint `gorm:"index;not null;index:idx_history_runs_channel_kind,priority:1" json:"channelId"`
	// Kind 记录类别:apply / push / rollback。
	//
	// **一次动作一条记录**:push 不修改已入链的 apply 记录,
	// 而是新建一条 —— 已入链的记录字段一旦被改动,哈希就与入链时算的不符,
	// 链会立刻报「被篡改」(实际是正常流程改了它)。
	//
	// 复合索引 idx_history_runs_kind_status 覆盖按类别/状态的查询
	// (待推送产物清单:kind=apply;已推送判定:kind=push + status=success)。
	Kind string `gorm:"size:16;not null;default:apply;index:idx_history_runs_kind_status,priority:1;index:idx_history_runs_channel_kind,priority:2" json:"kind"`
	// PushedRunID push/rollback 记录引用被操作的 apply 记录。
	PushedRunID uint `gorm:"index" json:"pushedRunId"`
	// Status 执行状态:pending|running|success|failed。
	Status string `gorm:"size:16;not null;default:pending;index:idx_history_runs_kind_status,priority:2" json:"status"`

	// BaseHead 移植前的目标 HEAD(既有提交原样保留的基线)。
	BaseHead string `gorm:"size:64" json:"baseHead"`
	// Head 移植后的 HEAD(新提交链的顶端)。
	Head string `gorm:"size:64" json:"head"`
	// WorkRepoPath 移植产出的工作仓库路径(推送用)。
	WorkRepoPath string `gorm:"size:512" json:"-"`

	// PlanSummary 计划统计 JSON。
	PlanSummary string `gorm:"type:text" json:"planSummary"`
	// Commits commit 映射表 JSON:[{source,target,subject,files}]。
	Commits string `gorm:"type:text" json:"commits"`
	// Files 应用的文件 JSON。
	Files string `gorm:"type:text" json:"files"`
	// Skipped 未应用的文件 JSON。
	Skipped string `gorm:"type:text" json:"skipped"`
	// ScrubReport 痕迹检测报告 JSON。
	ScrubReport string `gorm:"type:text" json:"scrubReport"`
	// GateReport 门禁结果 JSON(gofmt + 类型检查)。
	GateReport string `gorm:"type:text" json:"gateReport"`
	// RefsBefore / RefsAfter 推送前后的 ref 对照 JSON。
	RefsBefore string `gorm:"type:text" json:"refsBefore"`
	RefsAfter  string `gorm:"type:text" json:"refsAfter"`

	Error string `gorm:"type:text" json:"error"`

	// PrevHash 上一条执行记录的 EntryHash(**该通道内**的链)。
	PrevHash string `gorm:"size:64;index" json:"prevHash"`
	// EntryHash 本条哈希链值。
	//
	// 链在**通道内**:不同通道的同步记录混在一条链里没有意义,
	// 而且链会随通道数无限增长。
	EntryHash string `gorm:"size:64;index" json:"entryHash"`

	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	DurationMs int64      `json:"durationMs"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// BeforeSave 归一时间精度。
//
// **哈希链覆盖时间字段** → 写进库里的时间必须与算哈希时用的时间逐字一致。
// 亚毫秒精度写进 MySQL(datetime(3))会被四舍五入,读回来就不是同一个值,
// 整条链都会被报「被篡改」—— 实测过:每一条都误报。
//
// 必须在这里截断,而不是只在算哈希时截断:算哈希时截断只能改哈希输入,
// 改不了库里已经存进去的值,两边照样对不上。
//
// CreatedAt 缺省时在这里补:GORM 的自动时间戳是在 BeforeSave **之后**
// 才写入的,那时写进去的是纳秒值,已经来不及截断。
func (r *HistoryRun) BeforeSave(tx *gorm.DB) error {
	// 带数据库默认值的列要在**入库前**补成最终值:
	// GORM 对零值字段会省略该列,让数据库默认值生效 ——
	// 于是「算哈希时看到的空串」与「库里读出来的 'apply'」不是同一个值,
	// 链会误报被篡改。
	r.applyDefaults()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = nowFunc(tx)
	}
	r.CreatedAt = TruncateDBTime(r.CreatedAt)
	r.StartedAt = TruncateDBTimePtr(r.StartedAt)
	r.FinishedAt = TruncateDBTimePtr(r.FinishedAt)
	return nil
}

// applyDefaults 归一「带数据库默认值的列」。
//
// 哈希计算与入库都要用同一份口径(见 ComputeHistoryRunHash),
// 否则「算哈希时的值」与「库里的值」不是同一个东西。
func (r *HistoryRun) applyDefaults() {
	if r.Kind == "" {
		r.Kind = HistoryRunKindApply
	}
	if r.Status == "" {
		r.Status = HistoryRunPending
	}
}

// VerifyHistoryRunHash 校验一条执行记录的哈希。
//
// **只认当前载荷算法**:旧版载荷的哈希算法已删除 ——
// 留着它等于"升级前的记录,哪怕 kind / pushed_run_id 被改成任何值,链都照样报完好",
// 而这两个字段正是「这个产物推没推送」的判定依据。
func VerifyHistoryRunHash(prev string, r *HistoryRun, stored string) bool {
	return stored == ComputeHistoryRunHash(prev, r)
}

// ComputeHistoryRunHash 计算执行记录哈希。
//
// 与既有审计链同构:EntryHash = SHA256(PrevHash || payload),
// 篡改任一审计材料(基线 HEAD / commit 映射 / 检测报告 / ref 对照)都能检出。
//
// 只在**终态**计算:执行中的记录字段还会变,提前算出的哈希必然失配。
//
// 时间字段按**毫秒**归一(hashTimeLayout):与数据库的存储精度对齐,
// 否则写库时被四舍五入、读库后重算的哈希对不上(MySQL 上每条都误报)。
func ComputeHistoryRunHash(prev string, r *HistoryRun) string {
	// 用归一后的副本算:哈希不能依赖「入库钩子跑没跑」,
	// 否则调用方先算哈希再入库(顺序反了)就会算出另一个值。
	norm := *r
	norm.applyDefaults()
	payload, _ := json.Marshal(map[string]any{
		"channel_id":    norm.ChannelID,
		"kind":          norm.Kind,
		"pushed_run_id": norm.PushedRunID,
		"status":        norm.Status,
		"base_head":     norm.BaseHead,
		"head":          norm.Head,
		"plan_summary":  norm.PlanSummary,
		"commits":       norm.Commits,
		"files":         norm.Files,
		"skipped":       norm.Skipped,
		"scrub_report":  norm.ScrubReport,
		"gate_report":   norm.GateReport,
		"refs_before":   norm.RefsBefore,
		"refs_after":    norm.RefsAfter,
		"error":         norm.Error,
		"created_at":    HashTimeString(norm.CreatedAt),
		"finished_at":   HashTimeStringPtr(norm.FinishedAt),
	})
	h := sha256.Sum256(append([]byte(prev), payload...))
	return hex.EncodeToString(h[:])
}
