package repo

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type SyncRunStepRepo struct {
	base[model.SyncRunStep, uint]
}

func NewSyncRunStepRepo(db *gorm.DB) *SyncRunStepRepo {
	return &SyncRunStepRepo{base: newBase[model.SyncRunStep, uint](db)}
}

func (d *SyncRunStepRepo) Create(step *model.SyncRunStep) error {
	return d.create(step)
}

// Update 回写步骤状态(列白名单,不写回主键/审计列)。
func (d *SyncRunStepRepo) Update(step *model.SyncRunStep) error {
	return d.update(step)
}

func (d *SyncRunStepRepo) FindByRunID(runID uint) ([]*model.SyncRunStep, error) {
	var steps []*model.SyncRunStep
	err := d.db.Where("run_id = ?", runID).Order("id ASC").Find(&steps).Error
	return steps, err
}

func (d *SyncRunStepRepo) CleanupOlderThan(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	var total int64
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		result := d.db.WithContext(ctx).Where("created_at < ?", cutoff).Limit(1000).Delete(&model.SyncRunStep{})
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
