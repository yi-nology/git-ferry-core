package model

import "time"

// 自动重跑延迟调度状态(见 model.RetrySchedule)。
const (
	// RetrySchedulePending 已排程,等待 next_attempt_at 到点被任意实例认领。
	RetrySchedulePending = "pending"
	// RetryScheduleDispatched 已由调度器发起重跑(触发已入队/已执行)。
	RetryScheduleDispatched = "dispatched"
	// RetryScheduleCancelled 因任务删除/禁用等原因不再重跑。
	RetryScheduleCancelled = "cancelled"
)

// RetrySchedule 失败运行的延迟重跑排程(数据库驱动的定时器)。
//
// **为什么落库**:冷却期内的失败不能直接丢弃(#39)——
// `RetryTracker.ShouldRetry` 只负责「现在能不能跑」的判断,真正的
// 「什么时候再跑」必须由持久化事实表达:
//   - 进程重启后延迟重试仍然生效(内存 time.AfterFunc 会随进程消失);
//   - 多实例部署时只有一个实例认领并触发(条件更新,天然单胜者);
//   - 预算按 run 链从 DB 派生,排程本身不消耗预算(冷却只推迟,不吞预算)。
//
// RunID 唯一:同一条失败运行最多排程一次(订阅+轮询双通道、
// 进程重启后的重复判定都不会产生重复重跑)。
type RetrySchedule struct {
	ID uint `json:"id" gorm:"primaryKey"`
	// RunID 触发排程的失败运行(唯一身份键)。
	RunID uint `json:"runId" gorm:"not null;uniqueIndex:uk_retry_schedules_run_id"`
	// TaskKey 重跑目标任务。
	TaskKey string `json:"taskKey" gorm:"size:128;not null;index"`
	// RootRunID 该重试链的链头(第一个非 auto_retry 运行),审计/预算核对用。
	RootRunID uint `json:"rootRunId" gorm:"not null;default:0"`
	// Attempt 本次将执行的第几次重跑(1 起,与 RetryTracker.Decision.Attempt 同口径)。
	Attempt int `json:"attempt" gorm:"not null"`
	// AttemptLimit 当时的自动重跑预算上限(策略快照,便于事后解释)。
	AttemptLimit int `json:"attemptLimit" gorm:"not null"`
	// Reason 排程原因(当前恒为 cooldown;留字段给后续退避策略)。
	Reason string `json:"reason" gorm:"size:64"`
	// NextAttemptAt 最早可触发时间(not-before)。调度器到点后条件认领。
	NextAttemptAt time.Time `json:"nextAttemptAt" gorm:"not null;index"`
	// Status ∈ RetrySchedule* 常量。
	Status string `json:"status" gorm:"size:20;not null;index"`
	// LastError 最近一次触发/排程失败信息(观测面)。
	LastError string    `json:"lastError" gorm:"type:text"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (RetrySchedule) TableName() string { return TableRetrySchedules }
