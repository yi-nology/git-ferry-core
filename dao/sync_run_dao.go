package dao

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type SyncRunDAO struct {
	db *gorm.DB
}

func NewSyncRunDAO(db *gorm.DB) *SyncRunDAO {
	return &SyncRunDAO{db: db}
}

// DB 返回底层 gorm.DB,供跨 DAO 事务使用。
func (d *SyncRunDAO) DB() *gorm.DB {
	return d.db
}

// FindByTaskKey 列表查询,**预加载 Steps**。
//
// 以前刻意不加载(注释写的是"避免 N+1")—— 但 GORM 的 Preload 是**一条**聚合查询
// (`WHERE run_id IN (...)`),不是逐行查 ⇒ 本来就不存在 N+1。
// 不加载的代价大得多:健康评分要的「步骤级失败」信号恒为 0(结构性失效),
// 运维/AI 都看不到失败发生在哪一步。
func (d *SyncRunDAO) FindByTaskKey(taskKey string, page Pagination) ([]*model.SyncRun, int64, error) {
	var runs []*model.SyncRun
	query := d.db.Preload("Steps").Where("task_key = ?", taskKey)
	total, err := Paginate(query, page, &runs)
	return runs, total, err
}

// FindByTaskKeyAndRunID 精确查某个任务下的某一条执行记录。
//
// 之前 CLI 的 `history +detail --run-id` 只能靠"翻页找到那条"——
// 服务端没有按 run_id 过滤的能力 ⇒ 翻不到就静默返回空列表,
// 用户以为"这条记录不存在"。
func (d *SyncRunDAO) FindByTaskKeyAndRunID(taskKey string, runID uint) (*model.SyncRun, error) {
	var run model.SyncRun
	q := d.db.Preload("Steps").Where("id = ?", runID)
	// taskKey 为空 = 不按任务限定(按 ID 全局查唯一一条)。
	if taskKey != "" {
		q = q.Where("task_key = ?", taskKey)
	}
	if err := q.First(&run).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// FindByIDWithSteps 单条详情查询,加载 Steps 子记录。
func (d *SyncRunDAO) FindByIDWithSteps(id uint) (*model.SyncRun, error) {
	var run model.SyncRun
	err := d.db.Preload("Steps").First(&run, id).Error
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (d *SyncRunDAO) Create(run *model.SyncRun) error {
	return d.db.Create(run).Error
}

func (d *SyncRunDAO) Update(run *model.SyncRun) error {
	return d.db.Save(run).Error
}

func (d *SyncRunDAO) Delete(id uint) error {
	return d.db.Delete(&model.SyncRun{}, id).Error
}

func (d *SyncRunDAO) FindRecent(page Pagination) ([]*model.SyncRun, error) {
	var runs []*model.SyncRun
	err := d.db.Offset(page.Offset).Limit(page.Limit).Order("id DESC").Find(&runs).Error
	return runs, err
}

func (d *SyncRunDAO) CleanupOlderThan(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	var total int64
	for {
		// 每轮检查 context 是否已取消,支持优雅关闭时中断清理
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		result := d.db.WithContext(ctx).Where("created_at < ?", cutoff).Limit(1000).Delete(&model.SyncRun{})
		if result.Error != nil {
			return total, result.Error
		}
		total += result.RowsAffected
		if result.RowsAffected == 0 {
			break
		}
	}
	return total, nil
}

func (d *SyncRunDAO) CountByTaskKey(taskKey string) (int64, error) {
	var count int64
	err := d.db.Model(&model.SyncRun{}).Where("task_key = ?", taskKey).Count(&count).Error
	return count, err
}
