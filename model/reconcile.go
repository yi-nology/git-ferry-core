package model

import "time"

// 对账状态(P3 跨平台一致性对账)。
const (
	ReconcileInSync        = "in_sync"        // 两端 SHA 一致
	ReconcileDrift         = "drift"          // 两端 SHA 不一致
	ReconcileSourceMissing = "source_missing" // 源分支不存在
	ReconcileTargetMissing = "target_missing" // 目标分支不存在
)

// ReconcileResult 一次任务对账结果(只读,不碰任何一端)。
type ReconcileResult struct {
	TaskKey      string `json:"taskKey"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	SourceSHA    string `json:"sourceSha"`
	TargetSHA    string `json:"targetSha"`
	// Status ∈ Reconcile* 常量。
	Status string `json:"status"`
	// SourceAhead 目标端缺少的提交数(CompareCommits ahead_by);
	// 正常同步场景应为 0,>0 = 源有新提交未同步。
	SourceAhead int `json:"sourceAhead"`
	// TargetAhead 目标端多出的提交数(behind_by);>0 = 目标被旁路写入,异常漂移方向。
	TargetAhead int       `json:"targetAhead"`
	Message     string    `json:"message"`
	CheckedAt   time.Time `json:"checkedAt"`
}

// OutboxTopicReconcileDrift 「对账漂移」事件主题:周期对账发现 drift/
// 分支缺失时写入 outbox,订阅方(壳层通知/octopus 告警)按需消费。
const OutboxTopicReconcileDrift = "reconcile.drift"

// ReconcileRecord 每任务最新一次对账结果(落库,观测/告警的事实源)。
// TaskKey 为主键:只留最新,历史由 outbox/日志承担。
type ReconcileRecord struct {
	TaskKey     string    `json:"taskKey" gorm:"size:128;primaryKey"`
	Status      string    `json:"status" gorm:"size:20;not null;index"`
	SourceSHA   string    `json:"sourceSha" gorm:"size:64"`
	TargetSHA   string    `json:"targetSha" gorm:"size:64"`
	SourceAhead int       `json:"sourceAhead"`
	TargetAhead int       `json:"targetAhead"`
	Message     string    `json:"message" gorm:"type:text"`
	CheckedAt   time.Time `json:"checkedAt"`
}

func (ReconcileRecord) TableName() string { return TableReconcileResults }
