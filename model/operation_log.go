package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// OperationLog 记录用户对系统发起的写操作（审计日志）。
// 哈希链:EntryHash = SHA256(PrevHash || payload);篡改任意条目可被 VerifyAuditChain 检出。
type OperationLog struct {
	ID           uint   `json:"id" gorm:"primaryKey"`
	Action       string `json:"action" gorm:"size:32;not null;index"`  // create/update/delete/run/retry/sync
	ResourceType string `json:"resource_type" gorm:"size:32;not null"` // repo/task/rule/platform/event
	ResourceKey  string `json:"resource_key" gorm:"size:255;index"`    // 资源标识（key/name/id）
	Resource     string `json:"resource" gorm:"size:500"`              // 中文摘要，如 "创建仓库 repo-main"
	Actor        string `json:"actor" gorm:"size:128;index"`           // 操作者
	IP           string `json:"ip" gorm:"size:64"`
	Status       string `json:"status" gorm:"size:16;default:success"` // success/failed
	Detail       string `json:"detail" gorm:"type:text"`
	// PrevHash 上一条审计的 EntryHash(首条为空)
	PrevHash string `json:"prev_hash" gorm:"size:64;index"`
	// EntryHash 本条哈希链值
	EntryHash string    `json:"entry_hash" gorm:"size:64;index"`
	CreatedAt time.Time `json:"created_at" gorm:"index"`
}

func (OperationLog) TableName() string {
	return TableOperationLogs
}

// ComputeEntryHash 计算审计条目哈希(prev 可为空)。
func ComputeEntryHash(prev string, e *OperationLog) string {
	payload, _ := json.Marshal(map[string]any{
		"action":        e.Action,
		"resource_type": e.ResourceType,
		"resource_key":  e.ResourceKey,
		"resource":      e.Resource,
		"actor":         e.Actor,
		"ip":            e.IP,
		"status":        e.Status,
		"detail":        e.Detail,
		"created_at":    e.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
	h := sha256.Sum256(append([]byte(prev), payload...))
	return hex.EncodeToString(h[:])
}

// NormalizeAuditHashInput 归一化字符串用于稳定哈希(去首尾空白)。
func NormalizeAuditHashInput(s string) string {
	return strings.TrimSpace(s)
}
