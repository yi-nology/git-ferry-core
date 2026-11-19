package repository

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// ForcePushApprovalRepository force-push 审批记录。
type ForcePushApprovalRepository struct {
	base[model.ForcePushApproval, string]
}

func NewForcePushApprovalRepository(db *gorm.DB) *ForcePushApprovalRepository {
	return &ForcePushApprovalRepository{base: newBase[model.ForcePushApproval, string](db)}
}

// All 按创建时间升序（与历史文件 append 顺序一致）。
func (d *ForcePushApprovalRepository) All() ([]model.ForcePushApproval, error) {
	var list []model.ForcePushApproval
	err := d.db.Order("created_at, id").Find(&list).Error
	return list, err
}

// Get 按 ID 取；不存在返回 (nil, nil)。
func (d *ForcePushApprovalRepository) Get(id string) (*model.ForcePushApproval, error) {
	return d.getByID(context.Background(), id)
}

func (d *ForcePushApprovalRepository) Create(a *model.ForcePushApproval) error {
	return d.create(context.Background(), a)
}

// Update 回写审批记录(列白名单,不写回主键/审计列)。
// 状态迁移请走 Revoke / ConsumeApproved —— 它们只写字段并带条件。
func (d *ForcePushApprovalRepository) Update(a *model.ForcePushApproval) error {
	return d.update(context.Background(), a)
}

// RefreshPending 只刷新 pending 记录的展示字段(reason/target_ref/created_at)。
//
// 不能用 Update:它按列白名单整行回写,会把读到的 approved/consumed_at
// 快照一起写回 —— 多实例下管理员在另一实例刚放行,这边一次刷新就把它
// 重置回未审批。这里条件锁死「仍未放行、未消费、未撤销」。
func (d *ForcePushApprovalRepository) RefreshPending(id, reason, targetRef string, createdAt time.Time) (bool, error) {
	res := d.db.Model(&model.ForcePushApproval{}).
		Where("id = ? AND approved = ? AND consumed_at IS NULL AND revoked_at IS NULL", id, false).
		Updates(map[string]any{
			"reason":     reason,
			"target_ref": targetRef,
			"created_at": createdAt,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// FindPending 找同 task+branch 的未放行记录(最老的一条)。
func (d *ForcePushApprovalRepository) FindPending(taskKey, branch string) (*model.ForcePushApproval, error) {
	a := &model.ForcePushApproval{}
	q := d.db.Model(&model.ForcePushApproval{}).
		Where("task_key = ? AND branch = ? AND approved = ?", taskKey, branch, false).
		Order("created_at ASC").First(a)
	if q.Error != nil {
		if errors.Is(q.Error, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, q.Error
	}
	return a, nil
}

// IsApproved 同 task+branch 是否已放行**且尚未被用掉**。
//
// 放行是一次性的:用掉之后(consumed_at 非空)不再算数 ——
// 否则一次人工放行会变成该任务+分支上的常驻后门。
func (d *ForcePushApprovalRepository) IsApproved(taskKey, branch, targetRef string) (bool, error) {
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
func (d *ForcePushApprovalRepository) Revoke(id string, by string, at time.Time) (found bool, err error) {
	rows, err := d.updateFields(context.Background(), id, map[string]any{"revoked_at": &at, "revoked_by": by})
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
func (d *ForcePushApprovalRepository) ConsumeApproved(taskKey, branch, targetRef string, at time.Time) (found bool, err error) {
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
func (d *ForcePushApprovalRepository) PendingCount() (int64, error) {
	var n int64
	err := d.db.Model(&model.ForcePushApproval{}).Where("approved = ?", false).Count(&n).Error
	return n, err
}
