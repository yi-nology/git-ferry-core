package repository

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// OutboxEventRepository 事务外发盒存储(见 model.OutboxEvent)。
type OutboxEventRepository struct {
	base[model.OutboxEvent, uint]
}

func NewOutboxEventRepository(db *gorm.DB) *OutboxEventRepository {
	return &OutboxEventRepository{base: newBase[model.OutboxEvent, uint](db)}
}

// CreateInTx 在既有事务里写事件 —— **必须与业务写入同事务**,
// 否则 outbox 就退化成「先写库再写事件」的两阶段,崩溃窗口原样存在。
func (d *OutboxEventRepository) CreateInTx(tx *gorm.DB, ev *model.OutboxEvent) error {
	return tx.Create(ev).Error
}

// ListDue 取待派发事件:未标记已发布且 created_at <= before(宽限截止)。
// 按 ID 升序(落库序=因果序),limit 防一次性吞太多。
func (d *OutboxEventRepository) ListDue(limit int, before time.Time) ([]model.OutboxEvent, error) {
	var evs []model.OutboxEvent
	err := d.db.Where("published_at IS NULL AND created_at <= ?", before).
		Order("id").Limit(limit).Find(&evs).Error
	return evs, err
}

// MarkPublished 批量标记已投递(派发成功后调用)。
func (d *OutboxEventRepository) MarkPublished(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return d.db.Model(&model.OutboxEvent{}).
		Where("id IN ?", ids).
		Update("published_at", time.Now()).Error
}

// MarkPublishedByRun 按 run_id 标记已投递 —— 快速路径(进程内订阅已投)
// 用它把 outbox 行闭环,避免派发器宽限期后重复投递。
// 一个 run 只完成一次 ⇒ run_id 唯一定位一条事件。
func (d *OutboxEventRepository) MarkPublishedByRun(runID uint) error {
	return d.db.Model(&model.OutboxEvent{}).
		Where("run_id = ? AND published_at IS NULL", runID).
		Update("published_at", time.Now()).Error
}

// CleanupPublished 删除已发布且超过保留期的事件,防表无限增长。
func (d *OutboxEventRepository) CleanupPublished(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res := d.db.WithContext(ctx).
		Where("published_at IS NOT NULL AND published_at < ?", cutoff).
		Limit(1000).Delete(&model.OutboxEvent{})
	return res.RowsAffected, res.Error
}

// CountUnpublished 未发布事件数(健康观测用)。
func (d *OutboxEventRepository) CountUnpublished() (int64, error) {
	var n int64
	err := d.db.Model(&model.OutboxEvent{}).
		Where("published_at IS NULL").Count(&n).Error
	return n, err
}
