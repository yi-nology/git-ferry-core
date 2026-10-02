package dao

import (
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// ForcePushApprovalDAO force-push 审批记录。
type ForcePushApprovalDAO struct {
	db *gorm.DB
}

func NewForcePushApprovalDAO(db *gorm.DB) *ForcePushApprovalDAO {
	return &ForcePushApprovalDAO{db: db}
}

// All 按创建时间升序（与历史文件 append 顺序一致）。
func (d *ForcePushApprovalDAO) All() ([]model.ForcePushApproval, error) {
	var list []model.ForcePushApproval
	err := d.db.Order("created_at, id").Find(&list).Error
	return list, err
}

// Get 按 ID 取；不存在返回 (nil, nil)。
func (d *ForcePushApprovalDAO) Get(id string) (*model.ForcePushApproval, error) {
	var a model.ForcePushApproval
	err := d.db.Where("id = ?", id).First(&a).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Count 记录数（迁移判定用）。
func (d *ForcePushApprovalDAO) Count() (int64, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).Count(&n).Error
	return n, err
}

func (d *ForcePushApprovalDAO) Create(a *model.ForcePushApproval) error {
	return d.db.Create(a).Error
}

func (d *ForcePushApprovalDAO) Update(a *model.ForcePushApproval) error {
	return d.db.Save(a).Error
}

// PendingExists 同 task+branch 是否已有未放行记录（幂等判定）。
func (d *ForcePushApprovalDAO) PendingExists(taskKey, branch string) (bool, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ?", taskKey, branch, false).
		Count(&n).Error
	return n > 0, err
}

// IsApproved 同 task+branch 是否已放行。
func (d *ForcePushApprovalDAO) IsApproved(taskKey, branch string) (bool, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ?", taskKey, branch, true).
		Count(&n).Error
	return n > 0, err
}

// PendingCount 未放行条数。
func (d *ForcePushApprovalDAO) PendingCount() (int64, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).Where("approved = ?", false).Count(&n).Error
	return n, err
}
