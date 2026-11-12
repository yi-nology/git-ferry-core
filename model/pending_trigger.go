package model

import "time"

// PendingTrigger 待补跑触发:任务正在执行时到达的 webhook 触发被合并记录在此,
// 当前运行结束后补跑一次。
//
// **为什么落库而不是进程内 map**(P0 修复):
//  1. 多实例下任意实例都能消费——A 实例记的待补跑,B 实例执行完可认领;
//  2. 进程重启不丢;
//  3. 认领(Claim=DELETE)是原子操作,天然只有一个消费者成功。
type PendingTrigger struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	TaskKey    string    `json:"taskKey" gorm:"size:128;not null;uniqueIndex:uk_pending_triggers_task_key"`
	Generation int64     `json:"generation" gorm:"not null;default:1"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (PendingTrigger) TableName() string {
	return TablePendingTriggers
}
