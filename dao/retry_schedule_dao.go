package dao

import (
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RetryScheduleDAO 失败运行的延迟重跑排程(#39)。
//
// 语义与 execution_jobs 的 lease_until not-before 一致:
//   - Create 以 run_id 唯一键幂等(重复判定/重启重放不会产生重复排程);
//   - ClaimDue 以条件更新「租」出到点排程(把 next_attempt_at 推后一个
//     lease 窗口),多实例只有一个认领者;触发成功 MarkDispatched,
//     失败可 Reschedule 回 pending;
//   - 认领后进程崩溃 ⇒ lease 到期后其它实例再次认领(at-least-once,
//     执行侧仍有 taskKey 互斥/队列合并兜底)。
type RetryScheduleDAO struct {
	db *gorm.DB
}

func NewRetryScheduleDAO(db *gorm.DB) *RetryScheduleDAO {
	return &RetryScheduleDAO{db: db}
}

// Create 幂等登记一条排程;同一 RunID 已存在时返回 created=false。
func (d *RetryScheduleDAO) Create(s *model.RetrySchedule) (bool, error) {
	if s.Status == "" {
		s.Status = model.RetrySchedulePending
	}
	res := d.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "run_id"}},
		DoNothing: true,
	}).Create(s)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ClaimDue 认领一组到点排程(单条条件更新,多实例单胜者)。
// 认领即把 next_attempt_at 推后 lease,避免同一排程被并发重复触发;
// 触发成功后由 MarkDispatched 关闭,失败则 Reschedule 明确下一次时间。
func (d *RetryScheduleDAO) ClaimDue(now time.Time, limit int, lease time.Duration) ([]model.RetrySchedule, error) {
	if limit <= 0 {
		limit = 10
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	var candidates []model.RetrySchedule
	if err := d.db.Where("status = ? AND next_attempt_at <= ?", model.RetrySchedulePending, now).
		Order("next_attempt_at").Order("id").Limit(limit).Find(&candidates).Error; err != nil {
		return nil, err
	}
	claimed := make([]model.RetrySchedule, 0, len(candidates))
	for i := range candidates {
		res := d.db.Model(&model.RetrySchedule{}).
			Where("id = ? AND status = ? AND next_attempt_at <= ?",
				candidates[i].ID, model.RetrySchedulePending, now).
			Update("next_attempt_at", now.Add(lease))
		if res.Error != nil {
			return claimed, res.Error
		}
		if res.RowsAffected == 1 {
			claimed = append(claimed, candidates[i])
		}
	}
	return claimed, nil
}

// MarkDispatched 排程已成功发起重跑,关闭为终态。
func (d *RetryScheduleDAO) MarkDispatched(id uint) error {
	return d.db.Model(&model.RetrySchedule{}).
		Where("id = ? AND status = ?", id, model.RetrySchedulePending).
		Updates(map[string]any{
			"status":     model.RetryScheduleDispatched,
			"last_error": "",
		}).Error
}

// Reschedule 触发失败:保留 pending,指定下一次最早时间并记录原因。
func (d *RetryScheduleDAO) Reschedule(id uint, next time.Time, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return d.db.Model(&model.RetrySchedule{}).
		Where("id = ? AND status = ?", id, model.RetrySchedulePending).
		Updates(map[string]any{
			"next_attempt_at": next,
			"last_error":      msg,
		}).Error
}

// Cancel 取消排程(任务删除/禁用等重试无意义的情形)。
func (d *RetryScheduleDAO) Cancel(id uint, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return d.db.Model(&model.RetrySchedule{}).
		Where("id = ? AND status = ?", id, model.RetrySchedulePending).
		Updates(map[string]any{
			"status":     model.RetryScheduleCancelled,
			"last_error": msg,
		}).Error
}

// CancelForTask 取消某任务的全部待触发排程(任务被删除/禁用时调用),
// 返回取消条数。删除是物理删除,残留排程不应再把已删任务反复拉起。
func (d *RetryScheduleDAO) CancelForTask(taskKey string, cause error) (int64, error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	res := d.db.Model(&model.RetrySchedule{}).
		Where("task_key = ? AND status = ?", taskKey, model.RetrySchedulePending).
		Updates(map[string]any{
			"status":     model.RetryScheduleCancelled,
			"last_error": msg,
		})
	return res.RowsAffected, res.Error
}

// CleanupTerminal 清理超过保留期的终态排程(观测表有限增长)。
func (d *RetryScheduleDAO) CleanupTerminal(olderThan time.Time) (int64, error) {
	res := d.db.Where("status IN ? AND updated_at < ?",
		[]string{model.RetryScheduleDispatched, model.RetryScheduleCancelled}, olderThan).
		Limit(1000).Delete(&model.RetrySchedule{})
	return res.RowsAffected, res.Error
}

// DB 暴露底层句柄(测试用)。
func (d *RetryScheduleDAO) DB() *gorm.DB { return d.db }
