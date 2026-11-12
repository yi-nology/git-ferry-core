package dao

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// ErrLeaseLost 失锁哨兵:条件更新(续租/完成/退避重排)0 行命中 —— 作业已
// 不再归本实例所有(lease 过期被 reaper 放回、被其它实例接管等)。调用方
// 必须把它当作「我已失去执行权,该停手」的信号:立即停止本次执行,不得再跑
// executor/推送(防脑裂分叉推送)。用 errors.Is 判定。
var ErrLeaseLost = errors.New("execution job lease lost: no longer owned by this instance")

// ExecutionJobDAO 持久化执行队列(见 model.ExecutionJob)。
type ExecutionJobDAO struct {
	db *gorm.DB
}

func NewExecutionJobDAO(db *gorm.DB) *ExecutionJobDAO {
	return &ExecutionJobDAO{db: db}
}

// Enqueue 入队(同任务去重):同一 task_key 已有 pending/claimed 作业时
// 合并(返回 enqueued=false)——并发 webhook 重复触发不会重复发布。
//
// 用 INSERT ... SELECT ... WHERE NOT EXISTS 把「检查+插入」收敛为单条
// 原子语句(MySQL/sqlite 均支持),多实例并发入队也只有一个胜出。
//
// 列名一律反引号包裹:`trigger` 是 MySQL 保留字,裸写会 1064 语法错误
// (sqlite 宽容保留字作标识符,单测全绿也测不出来);sqlite 同样接受
// 反引号引用标识符,一条语句两种后端通用。
func (d *ExecutionJobDAO) Enqueue(taskKey, trigger string, webhookEventID *uint) (enqueued bool, err error) {
	var weID any
	if webhookEventID != nil {
		weID = *webhookEventID
	}
	res := d.db.Exec(enqueueJobSQL(),
		taskKey, trigger, weID, model.JobStatusPending, time.Now(), taskKey)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// enqueueJobSQL 单语句原子入队(INSERT ... SELECT ... WHERE NOT EXISTS)。
// 独立成函数供测试钉住语句文本:`trigger` 等 MySQL 保留字必须反引号包裹。
func enqueueJobSQL() string {
	return "INSERT INTO execution_jobs\n" +
		"(task_key, `trigger`, webhook_event_id, status, attempt, defer_count, queued_at)\n" +
		"SELECT ?, ?, ?, ?, 0, 0, ?\n" +
		"WHERE NOT EXISTS (\n" +
		"\tSELECT 1 FROM execution_jobs\n" +
		"\tWHERE task_key = ? AND status IN ('pending', 'claimed')\n" +
		")"
}

// ListPendingIDs 取待认领作业 ID(按入队序 = 因果序)。
func (d *ExecutionJobDAO) ListPendingIDs(limit int) ([]uint, error) {
	var ids []uint
	err := d.db.Model(&model.ExecutionJob{}).
		Where("status = ?", model.JobStatusPending).
		Order("id").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}

// FindByID 查单条。
func (d *ExecutionJobDAO) FindByID(id uint) (*model.ExecutionJob, error) {
	var j model.ExecutionJob
	if err := d.db.First(&j, id).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

// Claim 条件认领:仅当仍是 pending 才翻为 claimed(原子,多实例单胜者)。
// owner 通常为实例标识;ttl 为执行权租约。
//
// lease_until 在 pending 态下复用作「最早可认领时间」(not-before,见
// Reschedule):退避中的作业不得提前认领,防派发器对同一作业忙旋。
func (d *ExecutionJobDAO) Claim(id uint, owner string, ttl time.Duration) (bool, error) {
	now := time.Now()
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND status = ? AND (lease_until IS NULL OR lease_until <= ?)",
			id, model.JobStatusPending, now).
		Updates(map[string]any{
			"status":      model.JobStatusClaimed,
			"owner":       owner,
			"lease_until": now.Add(ttl),
			"claimed_at":  now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RenewLease 执行中续租。仅「本人 + 仍 claimed + 租约未过期」才生效:
// 已过期的租约不得借续租复活(对齐 lock 包「已过期槽不得借 Renew 复活」
// 的语义——它的执行权可能已被其他实例接管,复活即脑裂)。
// 0 行命中 ⇒ 返回 ErrLeaseLost(旧属主据此立即停手),不再静默吞掉。
func (d *ExecutionJobDAO) RenewLease(id uint, owner string, ttl time.Duration) error {
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ? AND status = ? AND lease_until > ?",
			id, owner, model.JobStatusClaimed, time.Now()).
		Update("lease_until", time.Now().Add(ttl))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Reschedule 执行权被占(良性繁忙)的退避重排:作业放回 pending,
// **attempt 不增**(毒丸预算只留给「lease 过期被接管」的崩溃接管场景,
// 见 RequeueExpired——良性繁忙烧光毒丸预算会把作业误判成毒丸丢弃);
// lease_until 在 pending 态下复用作「最早可认领时间」(not-before)推迟
// delay,防派发器忙旋;DeferCount 单独记账,与接管次数(Attempt)分离。
// 0 行命中(作业已被接管,不再归本人)⇒ ErrLeaseLost。
func (d *ExecutionJobDAO) Reschedule(id uint, owner string, delay time.Duration) error {
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ? AND status = ?", id, owner, model.JobStatusClaimed).
		Updates(map[string]any{
			"status":      model.JobStatusPending,
			"owner":       "",
			"lease_until": time.Now().Add(delay),
			"defer_count": gorm.Expr("defer_count + 1"),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Complete 完成作业:写终态与运行记录 ID。仅属主可写;
// 0 行命中 ⇒ ErrLeaseLost(此前静默返回 nil,旧 worker 无感继续跑,脑裂)。
func (d *ExecutionJobDAO) Complete(id uint, owner string, runID uint, execErr error) error {
	status := model.JobStatusSucceeded
	errMsg := ""
	if execErr != nil {
		status = model.JobStatusFailed
		errMsg = execErr.Error()
	}
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ?", id, owner).
		Updates(map[string]any{
			"status":      status,
			"run_id":      runID,
			"error":       errMsg,
			"finished_at": time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// RequeueExpired 接管过期租约(reaper):claimed 且 lease_until 已过 ⇒
// 放回 pending(attempt+1);attempt 超限 ⇒ 判 failed(毒丸)。
// 返回 (重放条数, 毒丸条数, 错误)。
func (d *ExecutionJobDAO) RequeueExpired(now time.Time) (requeued, poisoned int64, err error) {
	var expired []model.ExecutionJob
	if err := d.db.Where("status = ? AND lease_until IS NOT NULL AND lease_until < ?",
		model.JobStatusClaimed, now).Find(&expired).Error; err != nil {
		return 0, 0, err
	}
	for i := range expired {
		attempt := expired[i].Attempt + 1
		if attempt > model.MaxJobAttempts {
			res := d.db.Model(&model.ExecutionJob{}).
				Where("id = ? AND status = ?", expired[i].ID, model.JobStatusClaimed).
				Updates(map[string]any{
					"status":      model.JobStatusFailed,
					"attempt":     attempt,
					"error":       "max attempts exceeded (lease expired repeatedly)",
					"finished_at": time.Now(),
				})
			if res.Error != nil {
				return requeued, poisoned, res.Error
			}
			poisoned += res.RowsAffected
			continue
		}
		res := d.db.Model(&model.ExecutionJob{}).
			Where("id = ? AND status = ?", expired[i].ID, model.JobStatusClaimed).
			Updates(map[string]any{
				"status":      model.JobStatusPending,
				"attempt":     attempt,
				"owner":       "",
				"lease_until": nil,
			})
		if res.Error != nil {
			return requeued, poisoned, res.Error
		}
		requeued += res.RowsAffected
	}
	return requeued, poisoned, nil
}

// CleanupFinished 清理已完成且超过保留期的作业。
func (d *ExecutionJobDAO) CleanupFinished(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res := d.db.WithContext(ctx).
		Where("status IN (?, ?) AND finished_at < ?", model.JobStatusSucceeded, model.JobStatusFailed, cutoff).
		Limit(1000).Delete(&model.ExecutionJob{})
	return res.RowsAffected, res.Error
}

// CountPending 观测用。
func (d *ExecutionJobDAO) CountPending() (int64, error) {
	var n int64
	err := d.db.Model(&model.ExecutionJob{}).
		Where("status = ?", model.JobStatusPending).Count(&n).Error
	return n, err
}

// DB 暴露底层句柄(测试/事务用)。
func (d *ExecutionJobDAO) DB() *gorm.DB { return d.db }
