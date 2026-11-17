package repository

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ReconcileRepository 对账结果存储(reconcile_results,每任务最新一条)。
type ReconcileRepository struct {
	db *gorm.DB
}

func NewReconcileRepository(db *gorm.DB) *ReconcileRepository {
	return &ReconcileRepository{db: db}
}

// Upsert 记录一次对账结果(task_key 主键,重复对账覆盖最新)。
func (d *ReconcileRepository) Upsert(r *model.ReconcileRecord) error {
	return d.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "task_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"status", "source_sha", "target_sha", "source_ahead", "target_ahead",
			"message", "checked_at",
		}),
	}).Create(r).Error
}

// Get 查某任务最新对账结果。
func (d *ReconcileRepository) Get(taskKey string) (*model.ReconcileRecord, error) {
	var r model.ReconcileRecord
	if err := d.db.Where("task_key = ?", taskKey).First(&r).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

// ListDrift 漂移/分支缺失记录(漂移告警/看板用)。
// unknown/unsupported **不在此列**(#44):那是「无法判断」,走
// ListUnknown 的运维告警,不污染 drift 率。
func (d *ReconcileRepository) ListDrift() ([]model.ReconcileRecord, error) {
	var rs []model.ReconcileRecord
	err := d.db.Where("status IN ?", []string{
		model.ReconcileDrift, model.ReconcileSourceMissing, model.ReconcileTargetMissing,
	}).Order("checked_at DESC").Find(&rs).Error
	return rs, err
}

// ListUnknown 无法判断的记录(unknown/unsupported):凭据/限流/平台能力
// 问题,单独计数与告警(#44)。
func (d *ReconcileRepository) ListUnknown() ([]model.ReconcileRecord, error) {
	var rs []model.ReconcileRecord
	err := d.db.Where("status IN ?", []string{
		model.ReconcileUnknown, model.ReconcileUnsupported,
	}).Order("checked_at DESC").Find(&rs).Error
	return rs, err
}

// ListAll 全部记录(观测用)。
func (d *ReconcileRepository) ListAll() ([]model.ReconcileRecord, error) {
	var rs []model.ReconcileRecord
	err := d.db.Order("task_key").Find(&rs).Error
	return rs, err
}

// Cleanup 删除 checked_at 超过保留期的旧记录(每任务只留最新,此清理
// 主要面向已删除任务的残留)。
func (d *ReconcileRepository) Cleanup(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res := d.db.WithContext(ctx).
		Where("checked_at < ?", cutoff).
		Limit(1000).Delete(&model.ReconcileRecord{})
	return res.RowsAffected, res.Error
}
