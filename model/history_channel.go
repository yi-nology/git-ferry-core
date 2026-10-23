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
	ID        uint `gorm:"primaryKey" json:"id"`
	ChannelID uint `gorm:"index;not null" json:"channelId"`
	// Kind 记录类别:apply / push / rollback。
	//
	// **一次动作一条记录**:push 不修改已入链的 apply 记录,
	// 而是新建一条 —— 已入链的记录字段一旦被改动,哈希就与入链时算的不符,
	// 链会立刻报「被篡改」(实际是正常流程改了它)。
	Kind string `gorm:"size:16;not null;default:apply" json:"kind"`
	// PushedRunID push/rollback 记录引用被操作的 apply 记录。
	PushedRunID uint `gorm:"index" json:"pushedRunId"`
	// Status 执行状态:pending|running|success|failed。
	Status string `gorm:"size:16;not null;default:pending" json:"status"`

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

// ComputeHistoryRunHash 计算执行记录哈希。
//
// 与既有审计链同构:EntryHash = SHA256(PrevHash || payload),
// 篡改任一审计材料(基线 HEAD / commit 映射 / 检测报告 / ref 对照)都能检出。
//
// 只在**终态**计算:执行中的记录字段还会变,提前算出的哈希必然失配。
func ComputeHistoryRunHash(prev string, r *HistoryRun) string {
	payload, _ := json.Marshal(map[string]any{
		"channel_id":   r.ChannelID,
		"status":       r.Status,
		"base_head":    r.BaseHead,
		"head":         r.Head,
		"plan_summary": r.PlanSummary,
		"commits":      r.Commits,
		"files":        r.Files,
		"skipped":      r.Skipped,
		"scrub_report": r.ScrubReport,
		"gate_report":  r.GateReport,
		"refs_before":  r.RefsBefore,
		"refs_after":   r.RefsAfter,
		"error":        r.Error,
		"created_at":   r.CreatedAt.UTC().Format(time.RFC3339Nano),
		"finished_at":  finishedAt(r),
	})
	h := sha256.Sum256(append([]byte(prev), payload...))
	return hex.EncodeToString(h[:])
}

// finishedAt 取终态时间(未完成时为空串)。
func finishedAt(r *HistoryRun) string {
	if r.FinishedAt == nil {
		return ""
	}
	return r.FinishedAt.UTC().Format(time.RFC3339Nano)
}
