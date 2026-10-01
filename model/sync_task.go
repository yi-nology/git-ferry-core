package model

import (
	"time"

	"gorm.io/gorm"
)

type SyncTask struct {
	ID            uint   `json:"id" gorm:"primaryKey"`
	Key           string `json:"key" gorm:"uniqueIndex;size:36;not null;default:''"`
	Name          string `json:"name" gorm:"size:100;not null;default:''"`
	SourceRepoKey string `json:"source_repo_key" gorm:"size:255;not null;index;default:''"`
	SourceBranch  string `json:"source_branch" gorm:"size:255;not null;default:''"`
	TargetRepoKey string `json:"target_repo_key" gorm:"size:255;not null;index;default:''"`
	TargetBranch  string `json:"target_branch" gorm:"size:255;not null;default:''"`
	SyncMode      string `json:"sync_mode" gorm:"size:20;default:single"`
	Cron          string `json:"cron" gorm:"size:100"`
	WebhookToken  string `json:"webhook_token" gorm:"uniqueIndex;size:36"`
	Enabled       bool   `json:"enabled" gorm:"default:true;index"`
	GitTags       bool   `json:"git_tags" gorm:"default:false"`
	GitForce      bool   `json:"git_force" gorm:"default:false"`
	GitPrune      bool   `json:"git_prune" gorm:"default:false"`
	// GitLFS 同步 Git LFS 对象(需要环境安装 git-lfs)
	GitLFS bool `json:"git_lfs" gorm:"default:false"`
	// SyncWiki 同步 wiki 仓库(源/目标 URL 按 .wiki.git 规则推导)
	SyncWiki bool `json:"sync_wiki" gorm:"default:false"`
	// GitBundle 同步成功后生成 git bundle 冷备(需配置 Sync.BackupDir)
	GitBundle bool `json:"git_bundle" gorm:"default:false"`
	// Submodules 递归同步 git 子模块
	Submodules bool `json:"submodules" gorm:"default:false"`
	// GitPushPrune 推送后删除目标上源已不存在的同名分支(refspec prune)
	GitPushPrune bool `json:"git_push_prune" gorm:"default:false"`
	// KeepDivergent true=目标分支有源没有的提交时拒绝 force 覆盖(防丢代码);
	// false=允许强制覆盖分歧分支。默认 true(安全)。
	KeepDivergent bool `json:"keep_divergent" gorm:"default:true"`
	// ForcePushPolicy 强制推送保护策略:allow | block | backup_on_demand。
	// 空值按 KeepDivergent 兼容映射(keep_divergent=true → block)。
	ForcePushPolicy string `json:"force_push_policy" gorm:"size:32"`
	// IncludeBranches 逗号分隔分支 glob 白名单(如 "main,release/*")。
	// 空=不过滤；非空时推送侧仅同步匹配分支(在 SourceBranch 基础上再收窄)。
	IncludeBranches string `json:"include_branches" gorm:"size:512"`
	// ExcludeRefPatterns 逗号分隔 ref glob 黑名单(如 "refs/pull/*,refs/merge-requests/*")。
	// fetch/clone 后删除匹配 ref,避免 PR 引用污染目标。空=默认排除 refs/pull/* 与 refs/merge-requests/*。
	ExcludeRefPatterns string         `json:"exclude_ref_patterns" gorm:"size:512"`
	LastRunAt          *time.Time     `json:"last_run_at"`
	LastStatus         string         `json:"last_status" gorm:"size:20"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          gorm.DeletedAt `json:"-" gorm:"index"`
}

func (SyncTask) TableName() string {
	return TableSyncTasks
}

type CreateTaskRequest struct {
	Name          string `json:"name"`
	SourceRepoKey string `json:"source_repo_key"`
	SourceBranch  string `json:"source_branch"`
	TargetRepoKey string `json:"target_repo_key"`
	TargetBranch  string `json:"target_branch"`
	SyncMode      string `json:"sync_mode"`
	Cron          string `json:"cron"`
	GitTags       bool   `json:"git_tags"`
	GitForce      bool   `json:"git_force"`
	GitPrune      bool   `json:"git_prune"`
	GitLFS        bool   `json:"git_lfs"`
	SyncWiki      bool   `json:"sync_wiki"`
	GitBundle     bool   `json:"git_bundle"`
	Submodules    bool   `json:"submodules"`
	GitPushPrune  bool   `json:"git_push_prune"`
	// KeepDivergent nil/缺省=true(安全);显式 false 才允许覆盖分歧
	KeepDivergent *bool `json:"keep_divergent"`
	// ForcePushPolicy allow|block|backup_on_demand;空=按 KeepDivergent 映射
	ForcePushPolicy string `json:"force_push_policy"`
	// IncludeBranches 逗号分隔 glob 白名单;空=全部
	IncludeBranches string `json:"include_branches"`
	// ExcludeRefPatterns 逗号分隔 ref glob 黑名单;空=使用默认 PR refs 排除
	ExcludeRefPatterns string `json:"exclude_ref_patterns"`
}

type UpdateTaskRequest struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// SourceBranch/TargetBranch/SyncMode/Cron:空字符串=不修改
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SyncMode     string `json:"sync_mode"`
	Cron         string `json:"cron"`
	// bool 字段用指针:nil=不修改,false=显式关闭。
	// 替代旧 bool 零值语义——此前客户端只改 name 也会把任务静默禁用。
	Enabled       *bool `json:"enabled"`
	GitTags       *bool `json:"git_tags"`
	GitForce      *bool `json:"git_force"`
	GitPrune      *bool `json:"git_prune"`
	GitLFS        *bool `json:"git_lfs"`
	SyncWiki      *bool `json:"sync_wiki"`
	GitBundle     *bool `json:"git_bundle"`
	Submodules    *bool `json:"submodules"`
	GitPushPrune  *bool `json:"git_push_prune"`
	KeepDivergent *bool `json:"keep_divergent"`
	// ForcePushPolicy 空=不修改
	ForcePushPolicy string `json:"force_push_policy"`
	// IncludeBranches/ExcludeRefPatterns:空字符串=不修改
	IncludeBranches    string `json:"include_branches"`
	ExcludeRefPatterns string `json:"exclude_ref_patterns"`
}
