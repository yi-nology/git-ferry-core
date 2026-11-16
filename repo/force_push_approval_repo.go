package repo

import (
	"errors"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// ForcePushApprovalRepo force-push 审批记录。
type ForcePushApprovalRepo struct {
	base[model.ForcePushApproval, string]
}

func NewForcePushApprovalRepo(db *gorm.DB) *ForcePushApprovalRepo {
	return &ForcePushApprovalRepo{base: newBase[model.ForcePushApproval, string](db)}
}

// All 按创建时间升序（与历史文件 append 顺序一致）。
func (d *ForcePushApprovalRepo) All() ([]model.ForcePushApproval, error) {
	var list []model.ForcePushApproval
	err := d.db.Order("created_at, id").Find(&list).Error
	return list, err
}

// Get 按 ID 取；不存在返回 (nil, nil)。
func (d *ForcePushApprovalRepo) Get(id string) (*model.ForcePushApproval, error) {
	return d.getByID(id)
}

// Count 记录数（迁移判定用）。
func (d *ForcePushApprovalRepo) Count() (int64, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).Count(&n).Error
	return n, err
}

func (d *ForcePushApprovalRepo) Create(a *model.ForcePushApproval) error {
	return d.create(a)
}

// Update 回写审批记录(列白名单,不写回主键/审计列)。
// 状态迁移请走 Revoke / ConsumeApproved —— 它们只写字段并带条件。
func (d *ForcePushApprovalRepo) Update(a *model.ForcePushApproval) error {
	return d.update(a)
}

// PendingExists 同 task+branch 是否已有未放行记录（幂等判定）。
func (d *ForcePushApprovalRepo) PendingExists(taskKey, branch string) (bool, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ?", taskKey, branch, false).
		Count(&n).Error
	return n > 0, err
}

// FindPending 找同 task+branch 的未放行记录(最老的一条)。
func (d *ForcePushApprovalRepo) FindPending(taskKey, branch string) (*model.ForcePushApproval, error) {
	a := &model.ForcePushApproval{}
	q := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ?", taskKey, branch, false).
		Order("created_at ASC").First(a)
	if q.Error != nil {
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, q.Error
	}
	return a, nil
}

// IsApproved 同 task+branch 是否已放行**且尚未被用掉**。
//
// 放行是一次性的:用掉之后(consumed_at 非空)不再算数 ——
// 否则一次人工放行会变成该任务+分支上的常驻后门。
func (d *ForcePushApprovalRepo) IsApproved(taskKey, branch, targetRef string) (bool, error) {
	q := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ? AND consumed_at IS NULL AND revoked_at IS NULL",
			taskKey, branch, true)
	// 绑定:审批只对"申请时的那个目标状态"有效。
	// target_ref 为空的(早期版本创建的)视为未绑定,仍然适用。
	if targetRef != "" {
		q = q.Where("target_ref = ? OR target_ref = ''", targetRef)
	}
	var n int64
	err := q.Count(&n).Error
	return n > 0, err
}

// Revoke 撤销一条放行(标记为已撤销,放行不再生效)。
// 已经用掉(consumed)的放行也允许撤销 —— 撤销是"收回授权",两者都让放行失效。
//
// 只写 revoked_at/revoked_by 两列:不整行回写,避免把并发的
// consumed_at 覆盖回旧值(撤销与消费并发时的典型丢更新)。
func (d *ForcePushApprovalRepo) Revoke(id string, by string, at time.Time) (found bool, err error) {
	rows, err := d.updateFields(id, map[string]any{"revoked_at": &at, "revoked_by": by})
	if err != nil {
		return false, err
	}
	if rows > 0 {
		return true, nil
	}
	// 0 行:要么不存在,要么值未变化(MySQL 对 no-op 更新报 0)。
	var n int64
	if err := d.db.Model(&model.ForcePushApproval{}).Where("id = ?", id).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// ConsumeApproved 把同 task+branch 的放行标记为已用掉(一次性放行的兑现)。
// 没有可用放行时返回 found=false。
//
// **必须按 target_ref 精确消费**:判定"能不能推"走 IsApproved(它绑定 target_ref),
// 这里若不绑定,消费掉的可能是**另一条**放行 ——
// 真正被授权的那条仍然 approved 且未消费 ⇒ 同一次人工放行可以反复授权覆盖,
// "一次性"变成常驻后门。
//
// 同时排除**已撤销**的放行:撤销 = 授权被收回,消费它等于把"已收回"记成"已用掉",
// 两条记录的状态就都乱了(实测:撤销的记录被消费成 consumed=true)。
func (d *ForcePushApprovalRepo) ConsumeApproved(taskKey, branch, targetRef string, at time.Time) (found bool, err error) {
	// 先选候选行,再用「id + consumed_at IS NULL + revoked_at IS NULL」条件
	// 更新兑现;0 行命中 = 被并发消费/撤销 ⇒ 重新选下一条(最多 3 轮)。
	// 不能 SELECT 后整行 Save:两个消费者会同时读到同一行并各自写回,
	// 同一次人工放行被兑现两次(一次性语义失效)。
	for attempt := 0; attempt < 3; attempt++ {
		a := &model.ForcePushApproval{}
		q := d.db.Model(&model.ForcePushApproval{}).
			Where("task_key = ? AND branch = ? AND approved = ? AND consumed_at IS NULL AND revoked_at IS NULL",
				taskKey, branch, true)
		// 绑定语义与 IsApproved 完全一致:
		// target_ref 为空的(早期版本创建的)视为未绑定,同样适用。
		if targetRef != "" {
			q = q.Where("target_ref = ? OR target_ref = ''", targetRef)
		}
		q = q.Order("created_at DESC").First(a)
		if q.Error != nil {
			if errors.Is(q.Error, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, q.Error
		}
		res := d.db.Model(&model.ForcePushApproval{}).
			Where("id = ? AND consumed_at IS NULL AND revoked_at IS NULL", a.ID).
			Updates(map[string]any{"consumed_at": &at})
		if res.Error != nil {
			return false, res.Error
		}
		if res.RowsAffected == 1 {
			return true, nil
		}
	}
	return false, nil
}

// PendingCount 未放行条数。
func (d *ForcePushApprovalRepo) PendingCount() (int64, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).Where("approved = ?", false).Count(&n).Error
	return n, err
}
