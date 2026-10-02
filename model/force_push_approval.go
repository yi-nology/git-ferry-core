package model

import "time"

// ForcePushApproval force-push 审批记录（block-and-approve 模式）。
// JSON 字段与历史文件 <backup_dir>/force-push-approvals.json 逐字兼容，
// 供既有部署迁移导入与 HTTP 响应复用。
type ForcePushApproval struct {
	ID         string     `gorm:"primaryKey;size:64" json:"id"`
	TaskKey    string     `gorm:"size:128;index" json:"task_key"`
	Branch     string     `gorm:"size:255" json:"branch"`
	Reason     string     `gorm:"size:512" json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	Approved   bool       `json:"approved"`
	ApprovedBy string     `gorm:"size:128" json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}
