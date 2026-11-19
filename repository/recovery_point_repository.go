package repository

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// RecoveryPointRepository 恢复点索引(见 model.RecoveryPoint)。
type RecoveryPointRepository struct {
	base[model.RecoveryPoint, uint]
}

func NewRecoveryPointRepository(db *gorm.DB) *RecoveryPointRepository {
	return &RecoveryPointRepository{base: newBase[model.RecoveryPoint, uint](db)}
}

// Create 记录一次成功冷备的恢复点。
func (d *RecoveryPointRepository) Create(ctx context.Context, rp *model.RecoveryPoint) error {
	return d.create(ctx, rp)
}

// ListByTask 按任务列恢复点,最新优先(可恢复性检索主入口)。
func (d *RecoveryPointRepository) ListByTask(ctx context.Context, taskKey string, limit int) ([]model.RecoveryPoint, error) {
	var rps []model.RecoveryPoint
	err := d.db.WithContext(ctx).Where("task_key = ?", taskKey).
		Order("created_at DESC").Limit(limit).Find(&rps).Error
	return rps, err
}

// GetLatest 某任务最新恢复点(快速回答「现在能恢复到哪」)。
func (d *RecoveryPointRepository) GetLatest(ctx context.Context, taskKey string) (*model.RecoveryPoint, error) {
	var rp model.RecoveryPoint
	if err := d.db.WithContext(ctx).Where("task_key = ?", taskKey).
		Order("created_at DESC").First(&rp).Error; err != nil {
		return nil, err
	}
	return &rp, nil
}

// DeleteByFileName 按文件名删除(轮转/惰性清理:文件没了,索引条目跟着走)。
func (d *RecoveryPointRepository) DeleteByFileName(ctx context.Context, fileName string) (int64, error) {
	res := d.db.WithContext(ctx).Where("file_name = ?", fileName).Delete(&model.RecoveryPoint{})
	return res.RowsAffected, res.Error
}

// PruneMissing 清理文件已不存在的恢复点(返回清理条数)。
// 批量调用方负责传入 backupDir 拼接后的存在性判断——此方法只按 id 删。
func (d *RecoveryPointRepository) DeleteByIDs(ctx context.Context, ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := d.db.WithContext(ctx).Where("id IN ?", ids).Delete(&model.RecoveryPoint{})
	return res.RowsAffected, res.Error
}

// CleanupOlderThan 删除超过保留期的恢复点(与冷备保留策略对齐)。
func (d *RecoveryPointRepository) CleanupOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res := d.db.WithContext(ctx).Where("created_at < ?", cutoff).Limit(1000).Delete(&model.RecoveryPoint{})
	return res.RowsAffected, res.Error
}
