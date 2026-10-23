package model

import (
	"time"

	"gorm.io/gorm"
)

// 历史同步裁决类型(人工/AI 对某个 hold 文件的结论)。
const (
	// HistoryVerdictKeepTarget 保留目标侧内容(源侧的改动是内网特有,不该公开)。
	HistoryVerdictKeepTarget = "keep_target"
	// HistoryVerdictTakeSource 取源侧内容(源侧的改动是公开树需要的)。
	HistoryVerdictTakeSource = "take_source"
	// HistoryVerdictMerged 用裁决后的内容(两侧都要保留各自的部分)。
	HistoryVerdictMerged = "merged"
)

// HistoryOverride 对某个 hold 文件的裁决结果。
//
// 存在它是为了让「AI 建议 + 人工确认」的闭环真正落地:
// resolve 给出冲突卷宗 → 裁决写回这张表 → 再次 resolve 时按裁决出解 → apply 时用裁决内容。
//
// 裁决**只对 hold 生效**:其余文件仍走确定性判定。
type HistoryOverride struct {
	ID        uint `gorm:"primaryKey" json:"id"`
	ChannelID uint `gorm:"index:idx_history_override;not null" json:"channelId"`
	// Path 目标侧路径(裁决的定位键)。
	Path string `gorm:"size:512;not null;index:idx_history_override" json:"path"`
	// SourcePath 源侧路径(回溯用)。
	SourcePath string `gorm:"size:512" json:"sourcePath"`
	// Verdict 裁决:keep_target / take_source / merged。
	Verdict string `gorm:"size:16;not null" json:"verdict"`
	// Content 裁决后的内容(仅 merged 时有值)。
	Content string `gorm:"type:text" json:"content,omitempty"`
	// Reason 裁决理由(审计用)。
	Reason string `gorm:"type:text" json:"reason"`
	// DecidedBy 裁决者(人工或 AI 标识)。
	DecidedBy string `gorm:"size:128" json:"decidedBy"`

	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
