package dao

import (
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PendingTriggerDAO 待补跑触发的持久化存储(见 model.PendingTrigger)。
type PendingTriggerDAO struct {
	db *gorm.DB
}

func NewPendingTriggerDAO(db *gorm.DB) *PendingTriggerDAO {
	return &PendingTriggerDAO{db: db}
}

// Upsert 记一笔待补跑。同一任务重复记 ⇒ generation+1(合并语义:只补跑一次,
// 但积压次数留痕,排查「丢了多少个推送」时可见)。
func (d *PendingTriggerDAO) Upsert(taskKey string) error {
	pt := model.PendingTrigger{TaskKey: taskKey, Generation: 1}
	return d.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "task_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"generation": gorm.Expr("generation + 1"),
			"updated_at": time.Now(),
		}),
	}).Create(&pt).Error
}

// Claim 认领一笔待补跑(原子删除,返回是否命中)。
// 跨实例安全:同时认领只有一个实例的 DELETE 命中; loser 返回 false 不动作。
func (d *PendingTriggerDAO) Claim(taskKey string) (bool, error) {
	res := d.db.Where("task_key = ?", taskKey).Delete(&model.PendingTrigger{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ClaimAll 认领全部待补跑(启动扫描用)。
// 与 Claim 一样靠 DELETE 原子性防双消费;多实例同时启动双认领的窗口由
// 补跑侧的执行权互斥兜底(撞忙会重新记账,最终收敛)。
func (d *PendingTriggerDAO) ClaimAll() ([]string, error) {
	var keys []string
	if err := d.db.Model(&model.PendingTrigger{}).Pluck("task_key", &keys).Error; err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return keys, nil
	}
	if err := d.db.Where("task_key IN ?", keys).Delete(&model.PendingTrigger{}).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// List 当前全部待补跑(观测/诊断用)。
func (d *PendingTriggerDAO) List() ([]model.PendingTrigger, error) {
	var pts []model.PendingTrigger
	err := d.db.Order("id").Find(&pts).Error
	return pts, err
}
