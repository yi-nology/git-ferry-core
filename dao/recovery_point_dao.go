package dao

import (
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// RecoveryPointDAO 恢复点索引(见 model.RecoveryPoint)。
type RecoveryPointDAO struct {
	db *gorm.DB
}

func NewRecoveryPointDAO(db *gorm.DB) *RecoveryPointDAO {
	return &RecoveryPointDAO{db: db}
}

// Create 记录一次成功冷备的恢复点。
func (d *RecoveryPointDAO) Create(rp *model.RecoveryPoint) error {
	return d.db.Create(rp).Error
}

// ListByTask 按任务列恢复点,最新优先(可恢复性检索主入口)。
func (d *RecoveryPointDAO) ListByTask(taskKey string, limit int) ([]model.RecoveryPoint, error) {
	var rps []model.RecoveryPoint
	err := d.db.Where("task_key = ?", taskKey).
		Order("created_at DESC").Limit(limit).Find(&rps).Error
	return rps, err
}

// GetLatest 某任务最新恢复点(快速回答「现在能恢复到哪」)。
func (d *RecoveryPointDAO) GetLatest(taskKey string) (*model.RecoveryPoint, error) {
	var rp model.RecoveryPoint
	if err := d.db.Where("task_key = ?", taskKey).
		Order("created_at DESC").First(&rp).Error; err != nil {
		return nil, err
	}
	return &rp, nil
}

// ListAll 全部恢复点(备份覆盖情况总览)。
func (d *RecoveryPointDAO) ListAll() ([]model.RecoveryPoint, error) {
	var rps []model.RecoveryPoint
	err := d.db.Order("task_key, created_at DESC").Find(&rps).Error
	return rps, err
}

// DeleteByFileName 按文件名删除(轮转/惰性清理:文件没了,索引条目跟着走)。
func (d *RecoveryPointDAO) DeleteByFileName(fileName string) (int64, error) {
	res := d.db.Where("file_name = ?", fileName).Delete(&model.RecoveryPoint{})
	return res.RowsAffected, res.Error
}

// PruneMissing 清理文件已不存在的恢复点(返回清理条数)。
// 批量调用方负责传入 backupDir 拼接后的存在性判断——此方法只按 id 删。
func (d *RecoveryPointDAO) DeleteByIDs(ids []uint) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := d.db.Where("id IN ?", ids).Delete(&model.RecoveryPoint{})
	return res.RowsAffected, res.Error
}

// CleanupOlderThan 删除超过保留期的恢复点(与冷备保留策略对齐)。
func (d *RecoveryPointDAO) CleanupOlderThan(cutoff time.Time) (int64, error) {
	res := d.db.Where("created_at < ?", cutoff).Limit(1000).Delete(&model.RecoveryPoint{})
	return res.RowsAffected, res.Error
}
