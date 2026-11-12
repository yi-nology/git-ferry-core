package dao

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReconcileDAO 对账结果存储(reconcile_results,每任务最新一条)。
type ReconcileDAO struct {
	db *gorm.DB
}

func NewReconcileDAO(db *gorm.DB) *ReconcileDAO {
	return &ReconcileDAO{db: db}
}

// Upsert 记录一次对账结果(task_key 主键,重复对账覆盖最新)。
func (d *ReconcileDAO) Upsert(r *model.ReconcileRecord) error {
	return d.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "task_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"status", "source_sha", "target_sha", "source_ahead", "target_ahead",
			"message", "checked_at",
		}),
	}).Create(r).Error
}

// Get 查某任务最新对账结果。
func (d *ReconcileDAO) Get(taskKey string) (*model.ReconcileRecord, error) {
	var r model.ReconcileRecord
	if err := d.db.Where("task_key = ?", taskKey).First(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

// ListDrift 全部非 in_sync 的记录(告警/看板用)。
func (d *ReconcileDAO) ListDrift() ([]model.ReconcileRecord, error) {
	var rs []model.ReconcileRecord
	err := d.db.Where("status <> ?", model.ReconcileInSync).
		Order("checked_at DESC").Find(&rs).Error
	return rs, err
}

// ListAll 全部记录(观测用)。
func (d *ReconcileDAO) ListAll() ([]model.ReconcileRecord, error) {
	var rs []model.ReconcileRecord
	err := d.db.Order("task_key").Find(&rs).Error
	return rs, err
}

// Cleanup 删除 checked_at 超过保留期的旧记录(每任务只留最新,此清理
// 主要面向已删除任务的残留)。
func (d *ReconcileDAO) Cleanup(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res := d.db.WithContext(ctx).
		Where("checked_at < ?", cutoff).
		Limit(1000).Delete(&model.ReconcileRecord{})
	return res.RowsAffected, res.Error
}
