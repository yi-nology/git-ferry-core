package model

import (
	"time"

	"gorm.io/gorm"
)

type SyncTask struct {
	ID            uint           `json:"id" gorm:"primaryKey"`
	Key           string         `json:"key" gorm:"uniqueIndex;size:36;not null;default:''"`
	Name          string         `json:"name" gorm:"size:100;not null;default:''"`
	SourceRepoKey string         `json:"source_repo_key" gorm:"size:255;not null;index;default:''"`
	SourceBranch  string         `json:"source_branch" gorm:"size:255;not null;default:''"`
	TargetRepoKey string         `json:"target_repo_key" gorm:"size:255;not null;index;default:''"`
	TargetBranch  string         `json:"target_branch" gorm:"size:255;not null;default:''"`
	SyncMode      string         `json:"sync_mode" gorm:"size:20;default:single"`
	Cron          string         `json:"cron" gorm:"size:100"`
	WebhookToken  string         `json:"webhook_token" gorm:"uniqueIndex;size:36"`
	Enabled       bool           `json:"enabled" gorm:"default:true;index"`
	GitTags       bool           `json:"git_tags" gorm:"default:false"`
	GitForce      bool           `json:"git_force" gorm:"default:false"`
	GitPrune      bool           `json:"git_prune" gorm:"default:false"`
	// GitLFS 同步 Git LFS 对象(需要环境安装 git-lfs)
	GitLFS bool `json:"git_lfs" gorm:"default:false"`
	// SyncWiki 同步 wiki 仓库(源/目标 URL 按 .wiki.git 规则推导)
	SyncWiki bool `json:"sync_wiki" gorm:"default:false"`
	// GitBundle 同步成功后生成 git bundle 冷备(需配置 Sync.BackupDir)
	GitBundle bool `json:"git_bundle" gorm:"default:false"`
	// GitPushPrune 推送后删除目标上源已不存在的同名分支(refspec prune)
	GitPushPrune bool `json:"git_push_prune" gorm:"default:false"`
	// KeepDivergent true=目标分支有源没有的提交时拒绝 force 覆盖(防丢代码);
	// false=允许强制覆盖分歧分支。默认 true(安全)。
	KeepDivergent bool `json:"keep_divergent" gorm:"default:true"`
	LastRunAt     *time.Time     `json:"last_run_at"`
	LastStatus    string         `json:"last_status" gorm:"size:20"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	DeletedAt     gorm.DeletedAt `json:"-" gorm:"index"`
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
	GitTags        bool `json:"git_tags"`
	GitForce       bool `json:"git_force"`
	GitPrune       bool `json:"git_prune"`
	GitLFS       bool `json:"git_lfs"`
	SyncWiki     bool `json:"sync_wiki"`
	GitBundle    bool `json:"git_bundle"`
	GitPushPrune bool `json:"git_push_prune"`
	// KeepDivergent nil/缺省=true(安全);显式 false 才允许覆盖分歧
	KeepDivergent *bool `json:"keep_divergent"`
}

type UpdateTaskRequest struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	// SourceBranch/TargetBranch/SyncMode/Cron:空字符串=不修改
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SyncMode     string `json:"sync_mode"`
	Cron         string `json:"cron"`
	// bool 字段用指针:nil=不修改,false=显式关闭。
	// 替代旧 bool 零值语义——此前客户端只改 name 也会把任务静默禁用。
	Enabled  *bool `json:"enabled"`
	GitTags        *bool `json:"git_tags"`
	GitForce       *bool `json:"git_force"`
	GitPrune       *bool `json:"git_prune"`
	GitLFS         *bool `json:"git_lfs"`
	SyncWiki       *bool `json:"sync_wiki"`
	GitBundle      *bool `json:"git_bundle"`
	GitPushPrune   *bool `json:"git_push_prune"`
	KeepDivergent  *bool `json:"keep_divergent"`
}
