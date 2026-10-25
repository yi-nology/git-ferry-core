package model

import (
	"time"

	"gorm.io/gorm"
)

type WebhookRule struct {
	ID            uint              `json:"id" gorm:"primaryKey"`
	Name          string            `json:"name" gorm:"size:100;not null"`
	RepoKey       string            `json:"repoKey" gorm:"size:255;not null;index"`
	EventType     string            `json:"eventType" gorm:"size:100;default:push"`
	BranchPattern string            `json:"branchPattern" gorm:"size:255"`
	Action        string            `json:"action" gorm:"size:50;default:sync"`
	MinInterval   int               `json:"minInterval" gorm:"not null"`
	Enabled       bool              `json:"enabled" gorm:"not null;index"`
	Description   string            `json:"description" gorm:"type:text"`
	Tasks         []WebhookRuleTask `json:"tasks,omitempty" gorm:"foreignKey:RuleID"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
	DeletedAt     gorm.DeletedAt    `json:"-" gorm:"index"`
}

func (WebhookRule) TableName() string {
	return TableWebhookRules
}

type WebhookRuleTask struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	RuleID    uint      `json:"ruleId" gorm:"not null;index;uniqueIndex:idx_rule_task"`
	TaskKey   string    `json:"taskKey" gorm:"size:36;not null;uniqueIndex:idx_rule_task"`
	CreatedAt time.Time `json:"createdAt"`
}

func (WebhookRuleTask) TableName() string {
	return TableWebhookRuleTasks
}

type CreateRuleRequest struct {
	Name          string   `json:"name"`
	RepoKey       string   `json:"repoKey"`
	EventType     string   `json:"eventType"`
	BranchPattern string   `json:"branchPattern"`
	Action        string   `json:"action"`
	TaskKeys      []string `json:"taskKeys"`
	// MinInterval 最小触发间隔(秒)。**指针**:nil=用默认值(60),
	// 显式 0=不限流(与"没传"必须能区分 —— 普通 int 做不到)。
	MinInterval *int `json:"minInterval"`
	// Enabled 是否启用。**指针**:nil=默认启用,显式 false=建出来就是禁用的。
	Enabled     *bool  `json:"enabled"`
	Description string `json:"description"`
}

type UpdateRuleRequest struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	EventType string `json:"eventType"`
	// BranchPattern 分支匹配模式。**指针**:nil=不修改,显式 ""=匹配全部分支。
	BranchPattern *string  `json:"branchPattern"`
	Action        string   `json:"action"`
	TaskKeys      []string `json:"taskKeys"`
	// MinInterval 最小触发间隔(秒)。**指针**:nil=不修改,显式 0=不限流。
	MinInterval *int `json:"minInterval"`
	// Enabled 是否启用。**指针**:nil=不修改,显式 false=禁用。
	// 以前是普通 bool ⇒ 局部更新(只带 id+enabled)会把 enabled 写成 false
	// ⇒ 规则被静默禁用;反过来建规则时 enabled=false 又会被 GORM 的默认值吞掉。
	Enabled     *bool   `json:"enabled"`
	Description *string `json:"description"`
}

// GetTaskKeys returns the task keys from the Tasks relationship
func (r *WebhookRule) GetTaskKeys() []string {
	keys := make([]string, len(r.Tasks))
	for i, t := range r.Tasks {
		keys[i] = t.TaskKey
	}
	return keys
}

// SetTaskKeys updates the Tasks relationship
func (r *WebhookRule) SetTaskKeys(keys []string) {
	tasks := make([]WebhookRuleTask, len(keys))
	for i, key := range keys {
		tasks[i] = WebhookRuleTask{
			RuleID:  r.ID,
			TaskKey: key,
		}
	}
	r.Tasks = tasks
}
