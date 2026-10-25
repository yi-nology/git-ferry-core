package dao

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type WebhookEventDAO struct {
	db *gorm.DB
}

func NewWebhookEventDAO(db *gorm.DB) *WebhookEventDAO {
	return &WebhookEventDAO{db: db}
}

func (d *WebhookEventDAO) FindByEventID(eventID string) (*model.WebhookEvent, error) {
	var event model.WebhookEvent
	err := d.db.Where("event_id = ?", eventID).First(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &event, err
}

func (d *WebhookEventDAO) FindByRepoKey(repoKey string, page Pagination) ([]*model.WebhookEvent, int64, error) {
	var events []*model.WebhookEvent
	// 列表场景不需要原始 payload(可达 10MB),单独的详情路径再取全文
	query := d.db.Omit("payload").Where("repo_key = ?", repoKey)
	total, err := Paginate(query, page, &events)
	return events, total, err
}

func (d *WebhookEventDAO) Create(event *model.WebhookEvent) error {
	return d.db.Create(event).Error
}

func (d *WebhookEventDAO) Update(event *model.WebhookEvent) error {
	return d.db.Save(event).Error
}

// UpdateStatus 仅更新 status 和 processed_at 字段,避免全字段回写(payload 可能很大)。
func (d *WebhookEventDAO) UpdateStatus(id uint, status string, processedAt *time.Time) error {
	return d.db.Model(&model.WebhookEvent{}).Where("id = ?", id).
		Updates(map[string]interface{}{"status": status, "processed_at": processedAt}).Error
}

// UpdateStatusAndError 更新事件状态与失败原因(失败终态)。
func (d *WebhookEventDAO) UpdateStatusAndError(id uint, status string, processedAt *time.Time, reason string) error {
	return d.db.Model(&model.WebhookEvent{}).Where("id = ?", id).
		Updates(map[string]interface{}{"status": status, "processed_at": processedAt, "error_message": reason}).Error
}

func (d *WebhookEventDAO) FindByID(id uint) (*model.WebhookEvent, error) {
	return FindByID[model.WebhookEvent](d.db, id)
}

func (d *WebhookEventDAO) FindRecent(repoKey string, page Pagination) ([]*model.WebhookEvent, error) {
	var events []*model.WebhookEvent
	query := d.db.Offset(page.Offset).Limit(page.Limit).Order("id DESC")
	if repoKey != "" {
		query = query.Where("repo_key = ?", repoKey)
	}
	err := query.Find(&events).Error
	return events, err
}

func (d *WebhookEventDAO) CleanupOlderThan(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	var total int64
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		result := d.db.WithContext(ctx).Where("created_at < ?", cutoff).Limit(1000).Delete(&model.WebhookEvent{})
		if result.Error != nil {
			return total, result.Error
		}
		total += result.RowsAffected
		if result.RowsAffected == 0 {
			break
		}
	}
	return total, nil
}

// FindStuckProcessing 找出卡在 processing 的事件。
//
// processing 只在"正在处理"期间存在 ⇒ 进程被 SIGKILL / 崩溃 / goroutine panic
// 时,事件会永远停在这个状态 ⇒ 既不失败也不重试 ⇒ 这次推送的提交静默丢失。
func (d *WebhookEventDAO) FindStuckProcessing(ctx context.Context, olderThan time.Duration) ([]*model.WebhookEvent, error) {
	cutoff := time.Now().Add(-olderThan)
	var events []*model.WebhookEvent
	err := d.db.WithContext(ctx).
		Where("status = ? AND created_at < ?", model.StatusProcessing, cutoff).
		Order("id ASC").
		Find(&events).Error
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (d *WebhookEventDAO) CountByRepoKey(repoKey string) (int64, error) {
	var count int64
	err := d.db.Model(&model.WebhookEvent{}).Where("repo_key = ?", repoKey).Count(&count).Error
	return count, err
}
