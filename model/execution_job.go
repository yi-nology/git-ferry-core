package model

import "time"

// 执行任务状态机(P2 持久化执行队列)。
// pending → claimed → succeeded / failed;(lease 过期)claimed → pending 接管重试。
const (
	JobStatusPending   = "pending"
	JobStatusClaimed   = "claimed"
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
)

// ExecutionJob 持久化执行任务(P2 分布式地基):
//
// 触发不再直接执行,而是先入队;派发器按 lease 认领执行 —— 实例崩溃/
// K8s 驱逐后,lease 过期由 reaper 放回 pending,其它实例安全接管;
// 同一任务同时最多一条 pending/claimed 作业(入队 WHERE NOT EXISTS 去重),
// 重复触发(并发 webhook)自动合并 ⇒ 不会重复发布。
type ExecutionJob struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	TaskKey string `json:"taskKey" gorm:"size:128;not null;index"`
	Trigger string `json:"trigger" gorm:"size:20;not null"`
	// WebhookEventID 触发对应的 webhook 事件(非 webhook 触发为 nil)。
	WebhookEventID *uint  `json:"webhookEventId"`
	Status         string `json:"status" gorm:"size:20;not null;index"`
	// Attempt 已认领执行次数(reaper 每次接管 +1);超 MaxJobAttempts 判失败(毒丸)。
	Attempt int `json:"attempt" gorm:"not null;default:0"`
	// RunID 完成作业对应的运行记录。
	RunID *uint `json:"runId"`
	// Owner 认领实例标识;LeaseUntil 执行权租约到期时间(过期可被接管)。
	Owner      string     `json:"owner" gorm:"size:64"`
	LeaseUntil *time.Time `json:"leaseUntil" gorm:"index"`
	Error      string     `json:"error" gorm:"type:text"`
	QueuedAt   time.Time  `json:"queuedAt"`
	ClaimedAt  *time.Time `json:"claimedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

func (ExecutionJob) TableName() string { return TableExecutionJobs }

// MaxJobAttempts 作业最大认领执行次数:超过即判 failed(毒丸保护,
// 防无限接管重试把故障放大成风暴)。
const MaxJobAttempts = 5
