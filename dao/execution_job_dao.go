package dao

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

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
func (d *ExecutionJobDAO) Enqueue(taskKey, trigger string, webhookEventID *uint) (enqueued bool, err error) {
	var weID any
	if webhookEventID != nil {
		weID = *webhookEventID
	}
	res := d.db.Exec(`INSERT INTO execution_jobs
	(task_key, trigger, webhook_event_id, status, attempt, queued_at)
	SELECT ?, ?, ?, ?, 0, ?
	WHERE NOT EXISTS (
		SELECT 1 FROM execution_jobs
		WHERE task_key = ? AND status IN ('pending', 'claimed')
	)`,
		taskKey, trigger, weID, model.JobStatusPending, time.Now(), taskKey)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
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
func (d *ExecutionJobDAO) Claim(id uint, owner string, ttl time.Duration) (bool, error) {
	lease := time.Now().Add(ttl)
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND status = ?", id, model.JobStatusPending).
		Updates(map[string]any{
			"status":      model.JobStatusClaimed,
			"owner":       owner,
			"lease_until": lease,
			"claimed_at":  time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RenewLease 执行中续租(仅本人且仍 claimed 才生效)。
func (d *ExecutionJobDAO) RenewLease(id uint, owner string, ttl time.Duration) error {
	return d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ? AND status = ?", id, owner, model.JobStatusClaimed).
		Update("lease_until", time.Now().Add(ttl)).Error
}

// Reschedule 执行权被占等原因:保持 claimed,把租约推迟 delay 后重试
// (不经过 reaper,attempt 不增;退避防派发忙旋)。
func (d *ExecutionJobDAO) Reschedule(id uint, owner string, delay time.Duration) error {
	return d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ? AND status = ?", id, owner, model.JobStatusClaimed).
		Update("lease_until", time.Now().Add(delay)).Error
}

// Complete 完成作业:写终态与运行记录 ID。
func (d *ExecutionJobDAO) Complete(id uint, owner string, runID uint, execErr error) error {
	status := model.JobStatusSucceeded
	errMsg := ""
	if execErr != nil {
		status = model.JobStatusFailed
		errMsg = execErr.Error()
	}
	return d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ?", id, owner).
		Updates(map[string]any{
			"status":      status,
			"run_id":      runID,
			"error":       errMsg,
			"finished_at": time.Now(),
		}).Error
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
