package model

import "time"

// OutboxTopicRunCompleted 「同步运行完成」事件主题。
// 主题字符串是 outbox 消费者的界定符,改值=断兼容。
const OutboxTopicRunCompleted = "run.completed"

// OutboxEvent 事务外发盒(outbox)事件:与业务写入同一事务落库,
// 由后台派发器在提交后投递 —— 保证「运行记录落库」与「通知可恢复」
// 之间的持久性,修掉「落库后、回调前崩溃 ⇒ 通知与自动补偿丢失」(P1)。
//
// 投递语义 = **at-least-once**:快速路径(进程内订阅)先投,
// 派发器只重投「未标记已发布且已过宽限期」的事件;
// 崩溃在「投递后、标记前」会产生一次重复,订阅方必须幂等
// (runwatch 的 RetryTracker 有 decided 去重 + DB 链计数,天然幂等)。
type OutboxEvent struct {
	ID      uint   `json:"id" gorm:"primaryKey"`
	Topic   string `json:"topic" gorm:"size:50;not null;index:idx_outbox_publish,priority:1"`
	TaskKey string `json:"taskKey" gorm:"size:128;not null"`
	RunID   uint   `json:"runId" gorm:"not null"`
	// Trigger/Status 冗余自 run 记录:重投时不用再 join 即可过滤。
	Trigger string `json:"trigger" gorm:"size:20"`
	Status  string `json:"status" gorm:"size:20"`
	// Payload JSON 附加上下文(如 error_message),重投构造事件时 enrich 用。
	Payload string `json:"payload" gorm:"type:text"`
	// PublishedAt 非 nil = 快速路径已投递;nil = 待派发器处理(或曾崩溃)。
	PublishedAt *time.Time `json:"publishedAt" gorm:"index:idx_outbox_publish,priority:2"`
	CreatedAt   time.Time  `json:"createdAt"`
}

func (OutboxEvent) TableName() string {
	return TableOutboxEvents
}
