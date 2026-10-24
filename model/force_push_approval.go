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
	// ConsumedAt 放行被**用掉**的时间。
	//
	// 放行是**一次性**的(契约:"Admin 一次性放行后允许本次 force"):
	// 用掉之后,同一任务同一分支再出现分歧必须**重新审批** ——
	// 否则一次人工放行会变成该分支上的常驻后门,之后每次覆盖都无人复核。
	// 保留记录(而不是删掉)是为了留住审计线索:谁批的、什么时候批的、什么时候用的。
	ConsumedAt *time.Time `json:"consumed_at,omitempty"`
}
