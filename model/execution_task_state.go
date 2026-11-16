package model

import "time"

// ExecutionTaskState 任务级队列状态行(每 task_key 恰一行,#36 generation
// coalescing 的核心账本):
//
//   - DesiredGeneration 已受理的触发代次:每次触发(含执行中被合并的)在同一
//     事务里 +1 —— 即使本次触发因活动槽被占而合并,这次「提交」也以代次形式
//     留痕,不会像旧实现那样被 WHERE NOT EXISTS 静默丢弃;
//   - CompletedGeneration 已被某次执行覆盖到的代次(作业收尾 CAS 上推);
//   - ActiveJobID 活动槽:当前唯一 pending/claimed 作业的 ID(NULL=空闲)。
//
// 收尾契约:作业完成时若 CompletedGeneration < DesiredGeneration,必须再入
// 一个追赶作业(携带最新 DesiredGeneration)——执行中连发 N 次 webhook 最终
// 全部被承接,最后一次触发对应的提交一定被执行。
//
// 与 pending_triggers(非队列模式的同类补偿)的关系:两者**不是两套待办机制**
// ——pending_triggers 服务直跑模式(guard 撞忙的 ErrTaskRunning 分支),
// 本表的代次递增服务队列模式(入队合并);durable_queue=true 时触发走
// enqueueTaskRun,永远到不了 guard 撞忙分支,两条路径互斥、各管一种模式。
type ExecutionTaskState struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	TaskKey string `json:"taskKey" gorm:"size:128;not null;uniqueIndex:uk_execution_task_states_task_key"`
	// DesiredGeneration 已受理触发代次(单调递增,不回退)。
	DesiredGeneration int64 `json:"desiredGeneration" gorm:"not null;default:0"`
	// CompletedGeneration 已被执行覆盖到的代次(收尾 CAS:
	// completed = max(completed, job.generation))。
	CompletedGeneration int64 `json:"completedGeneration" gorm:"not null;default:0"`
	// ActiveJobID 活动槽:当前唯一 pending/claimed 作业;NULL=空闲。
	// 槽的唯一性另由 execution_jobs.active_key 的唯一索引兜底(见
	// ExecutionJob.ActiveKey)——状态行是账本,唯一索引是硬保证。
	ActiveJobID *uint     `json:"activeJobId"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (ExecutionTaskState) TableName() string { return TableExecutionTaskStates }
