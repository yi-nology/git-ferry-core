package dao

import (
	"errors"
	"time"

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

// IsApproved 同 task+branch 是否已放行**且尚未被用掉**。
//
// 放行是一次性的:用掉之后(consumed_at 非空)不再算数 ——
// 否则一次人工放行会变成该任务+分支上的常驻后门。
func (d *ForcePushApprovalDAO) IsApproved(taskKey, branch string) (bool, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ? AND consumed_at IS NULL AND revoked_at IS NULL",
			taskKey, branch, true).
		Count(&n).Error
	return n > 0, err
}

// Revoke 撤销一条放行(标记为已撤销,放行不再生效)。
// 已经用掉(consumed)的放行也允许撤销 —— 撤销是"收回授权",两者都让放行失效。
func (d *ForcePushApprovalDAO) Revoke(id string, by string, at time.Time) (found bool, err error) {
	a := &model.ForcePushApproval{}
	q := d.db.Model(&model.ForcePushApproval{}).Where("id = ?", id).First(a)
	if q.Error != nil {
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, q.Error
	}
	a.RevokedAt = &at
	a.RevokedBy = by
	if err := d.db.Save(a).Error; err != nil {
		return false, err
	}
	return true, nil
}

// ConsumeApproved 把同 task+branch 的放行标记为已用掉(一次性放行的兑现)。
// 没有可用放行时返回 found=false。
func (d *ForcePushApprovalDAO) ConsumeApproved(taskKey, branch string, at time.Time) (found bool, err error) {
	a := &model.ForcePushApproval{}
	q := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ? AND consumed_at IS NULL", taskKey, branch, true).
		Order("created_at DESC").First(a)
	if q.Error != nil {
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, q.Error
	}
	a.ConsumedAt = &at
	if err := d.db.Save(a).Error; err != nil {
		return false, err
	}
	return true, nil
}

// PendingCount 未放行条数。
func (d *ForcePushApprovalDAO) PendingCount() (int64, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).Where("approved = ?", false).Count(&n).Error
	return n, err
}
