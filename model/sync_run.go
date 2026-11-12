package model

import "time"

type SyncRun struct {
	ID             uint       `json:"id" gorm:"primaryKey"`
	TaskKey        string     `json:"taskKey" gorm:"size:36;not null;index:idx_sync_runs_task_key_created,priority:1"`
	TriggerSource  string     `json:"triggerSource" gorm:"size:20;not null"`
	Status         string     `json:"status" gorm:"size:20;not null;index"`
	StartTime      time.Time  `json:"startTime"`
	EndTime        *time.Time `json:"endTime"`
	CommitRange    string     `json:"commitRange" gorm:"size:255"`
	Details        string     `json:"details" gorm:"type:text"`
	ErrorMessage   string     `json:"errorMessage" gorm:"type:text"`
	WebhookEventID *uint      `json:"webhookEventId" gorm:"index"`
	DurationMs     int64      `json:"durationMs"`
	ErrorType      string     `json:"errorType" gorm:"size:30"`
	RetryTotal     int        `json:"retryTotal"`
	// BackupStatus 冷备/备份远端阶段状态,与 Status(同步)分离:
	// 空 = 未启用或未执行;BackupStatusSuccess / BackupStatusFailed。
	// **为什么分离**(P1):同步成功 + 备份失败 ⇒ 灾备目标未达成,
	// 旧实现把备份异常只写进 details,run 恒 success,用户看到的是「健康」。
	// 两个可选阶段(bundle/backup_remote)都会写;failed 粘滞——
	// 任一备份环节失败即整体 failed,成功要求全程无失败。
	BackupStatus string `json:"backupStatus" gorm:"size:20"`
	// PolicySnapshot 执行开始时固化的任务策略快照(确定性 JSON,
	// 见 BuildTaskPolicySnapshot):审计「这次执行是在什么策略下跑的」,
	// 策略后续被改不影响对历史执行的复核。
	PolicySnapshot string        `json:"policySnapshot" gorm:"type:text"`
	Steps          []SyncRunStep `json:"steps" gorm:"foreignKey:RunID"`
	CreatedAt      time.Time     `json:"createdAt" gorm:"index:idx_sync_runs_created_at;index:idx_sync_runs_task_key_created,priority:2"`
}

func (SyncRun) TableName() string {
	return TableSyncRuns
}
