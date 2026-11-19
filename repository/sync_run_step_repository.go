package repository

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type SyncRunStepRepository struct {
	base[model.SyncRunStep, uint]
}

func NewSyncRunStepRepository(db *gorm.DB) *SyncRunStepRepository {
	return &SyncRunStepRepository{base: newBase[model.SyncRunStep, uint](db)}
}

func (d *SyncRunStepRepository) Create(step *model.SyncRunStep) error {
	return d.create(context.Background(), step)
}

// Update 回写步骤状态(列白名单,不写回主键/审计列)。
func (d *SyncRunStepRepository) Update(step *model.SyncRunStep) error {
	return d.update(context.Background(), step)
}

func (d *SyncRunStepRepository) FindByRunID(runID uint) ([]*model.SyncRunStep, error) {
	var steps []*model.SyncRunStep
	err := d.db.Where("run_id = ?", runID).Order("id ASC").Find(&steps).Error
	return steps, err
}

// FindByRunIDs 一次取多条 run 的步骤并按 run_id 分组。
// 供健康评分等批量聚合路径使用,避免逐 run 查询。
func (d *SyncRunStepRepository) FindByRunIDs(runIDs []uint) (map[uint][]*model.SyncRunStep, error) {
	out := make(map[uint][]*model.SyncRunStep, len(runIDs))
	if len(runIDs) == 0 {
		return out, nil
	}
	var steps []*model.SyncRunStep
	if err := d.db.Where("run_id IN ?", runIDs).Order("id ASC").Find(&steps).Error; err != nil {
		return nil, err
	}
	for _, s := range steps {
		out[s.RunID] = append(out[s.RunID], s)
	}
	return out, nil
}

func (d *SyncRunStepRepository) CleanupOlderThan(ctx context.Context, olderThan time.Duration) (int64, error) {
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
