package model

import (
	"time"

	"gorm.io/gorm"
)

// 镜像通道模式
const (
	MirrorModePublish = "publish" // 开源公开发布:身份改写 + 门禁 + 快照推送
	MirrorModeBackup  = "backup"  // 仓库备份:原样 ref 推送(P1)
)

// 镜像执行状态
const (
	MirrorRunPending   = "pending"
	MirrorRunRunning   = "running"
	MirrorRunSuccess   = "success"
	MirrorRunFailed    = "failed"
	MirrorRunDivergent = "divergent" // 远端同名 tag 内容不一致,默认拒绝覆盖
)

// 镜像执行类别
const (
	MirrorKindPublish = "publish"
	MirrorKindVerify  = "verify"
)

// 凭据类型
const (
	MirrorCredNone   = "none"
	MirrorCredToken  = "token"
	MirrorCredSSHKey = "ssh_key"
)

// MirrorChannel 镜像通道:源仓库 → N 个远端目标的配置。
type MirrorChannel struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	Name    string `gorm:"size:128;not null" json:"name"`
	Mode    string `gorm:"size:16;not null;default:publish" json:"mode"`
	RepoKey string `gorm:"size:128;not null;index" json:"repoKey"`
	// Module 是源 module 身份(publish 模式,创建时从 go.mod 读出)。
	Module string `gorm:"size:255" json:"module"`
	// ScrubRules 痕迹清理规则(JSON 串):发布前把内容里的内网痕迹清掉。
	//
	// 身份改写只管 module path;内网 registry 地址、部署代号、令牌
	// 这些**不在 module path 里**的标识,原样进公开 module 就是泄漏。
	// 规则形状与历史同步侧一致{name,category,pattern,severity,replacement}。
	ScrubRules string `gorm:"type:text" json:"scrubRules,omitempty"`

	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	// Targets 响应投影(GetMirrorChannel 组装)。
	Targets []*MirrorTarget `gorm:"-" json:"targets,omitempty"`
}

// MirrorTarget 通道目标:publish 模式为身份映射,backup 模式为备份远端。
type MirrorTarget struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	ChannelID uint `gorm:"index;not null" json:"channelId"`
	// Remote 是目标仓库在本地克隆里配置的远端名(如 github)。
	Remote string `gorm:"size:64;not null" json:"remote"`
	// RepoURL 是目标仓库的 git 地址(https/ssh)。
	RepoURL string `gorm:"size:512;not null" json:"repoUrl"`
	// TargetModule 是目标 module 身份(publish 模式)。
	TargetModule string `gorm:"size:255" json:"targetModule"`

	// 凭据:Credential 整体加密存储,响应只回 HasCredential。
	CredType      string `gorm:"size:16;not null;default:none" json:"credType"`
	Credential    string `gorm:"size:2048" json:"-"`
	Username      string `gorm:"size:128" json:"username"`
	HasCredential bool   `gorm:"-" json:"hasCredential"`

	// **SSH 主机密钥钉扎**(与源仓侧同一套字段):
	// 之前目标侧没法钉 —— 于是「检查路径」无 known_hosts 就硬错(fail-closed)、
	// 「推送路径」却用 accept-new(TOFU),同一次发布里两套策略相反。
	// 现在两边都从同一份 AuthConfig 走,配了指纹就都校验。
	//
	// **必须落库**:这一项是「放宽安全」的开关,丢了等于悄悄放宽。
	InsecureSkipTLS    bool   `gorm:"not null;default:false" json:"insecureSkipTLS,omitempty"`
	HostKeyFingerprint string `gorm:"size:255" json:"hostKeyFingerprint,omitempty"`
	KnownHostsPath     string `gorm:"size:512" json:"knownHostsPath,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MirrorStep 执行步骤(JSON 存储)。
type MirrorStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // running|success|failed|skipped
	Detail string `json:"detail,omitempty"`
}

// MirrorRun 执行记录:一次发布/备份/验证动作。
type MirrorRun struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	ChannelID uint   `gorm:"index:idx_mirror_channel;not null" json:"channelId"`
	TargetID  uint   `gorm:"index" json:"targetId"`
	Kind      string `gorm:"size:16;not null;default:publish" json:"kind"`
	// Tags 逗号分隔的本批 tag。
	Tags string `gorm:"size:1024" json:"tags"`
	// Status 整批状态:pending|running|success|failed|divergent。
	Status string `json:"status"`
	// TagStatuses 行级结果 JSON:{tag: {status, detail, commit, tree}}。
	TagStatuses string `gorm:"type:text" json:"tagStatuses"`
	// Steps 步骤时间线 JSON。
	Steps string `gorm:"type:text" json:"steps"`
	// Report 最后一次 core 报告 JSON(参考 mirror.TagReport)。
	Report string `gorm:"type:text" json:"report"`
	Error  string `gorm:"type:text" json:"error"`

	AllowOverwrite bool       `json:"allowOverwrite"`
	StartedAt      *time.Time `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt"`
	DurationMs     int64      `json:"durationMs"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TagStatus 行级发布结果(MirrorRun.TagStatuses 的元素)。
type TagStatus struct {
	Tag    string `json:"tag"`
	Status string `json:"status"` // success|failed|divergent
	Detail string `json:"detail,omitempty"`
	Commit string `json:"commit,omitempty"`
	Tree   string `json:"tree,omitempty"`
}
