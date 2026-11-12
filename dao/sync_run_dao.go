package dao

import (
	"context"
	"errors"
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

// FindLastBackupSuccess 取某任务**全历史**里最近一次备份成功的运行
// (backup_status=success,ORDER BY id DESC LIMIT 1);没有则返回 (nil, nil)。
//
// RPO 事实键专用,与评分用的「最近 N 条」窗口解耦:契约把
// backup_age_hours=-1 定义为「无成功备份」,若只看最近 10 次窗口,
// 窗口外的成功备份会被谎报成「从未成功备份」—— 恰恰在 RPO 最该被
// 关注的时刻报出最强的乐观信号。
func (d *SyncRunDAO) FindLastBackupSuccess(taskKey string) (*model.SyncRun, error) {
	var run model.SyncRun
	err := d.db.Where("task_key = ? AND backup_status = ?", taskKey, model.BackupStatusSuccess).
		Order("id DESC").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
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

// AutoRetryChainStats 返回「包含 runID 的那条连续 auto_retry 链」的统计:
// 链内 auto_retry 运行条数 + 最近一条链内运行的结束时间。
//
// 链的界定:两个相邻非 auto_retry 运行之间的 auto_retry 运行构成一条链;
// 人工/cron/webhook 触发的任意一次运行都会自然开启新链(重试预算随之归零)。
//
// runIsRetry=false(runID 本身是链头失败)时,统计 (runID, 下一个非 auto_retry]
// 区间(开区间:链头自己不算重试);runIsRetry=true 时统计
// (上一个非 auto_retry, runID](含自身——它本身就是一次已消耗预算的重试)。
//
// 计数从 DB 派生而非内存记账:进程重启、多实例竞争、订阅+轮询双通道投递
// 看到的都是同一份事实,重试预算因此是耐久的。
func (d *SyncRunDAO) AutoRetryChainStats(taskKey string, runID uint, runIsRetry bool) (int64, time.Time, error) {
	// base 构造链区间的公共过滤;每次查询新建 statement,避免 Count/Select 互相污染。
	base := func() *gorm.DB {
		q := d.db.Model(&model.SyncRun{}).
			Where("task_key = ? AND trigger_source = ?", taskKey, model.TriggerAutoRetry)
		if runIsRetry {
			return q
		}
		return q.Where("id > ?", runID)
	}
	if runIsRetry {
		var anchor uint
		// 上一个非 auto_retry 运行的 ID(0 = 链前没有边界,从头算)
		err := d.db.Model(&model.SyncRun{}).
			Where("task_key = ? AND id < ? AND trigger_source <> ?", taskKey, runID, model.TriggerAutoRetry).
			Select("COALESCE(MAX(id), 0)").Scan(&anchor).Error
		if err != nil {
			return 0, time.Time{}, err
		}
		base = func() *gorm.DB {
			return d.db.Model(&model.SyncRun{}).
				Where("task_key = ? AND trigger_source = ?", taskKey, model.TriggerAutoRetry).
				Where("id > ? AND id <= ?", anchor, runID)
		}
	} else {
		// 下一个非 auto_retry 运行的 ID(0 = 链后再无边界,算到最新)
		var next uint
		err := d.db.Model(&model.SyncRun{}).
			Where("task_key = ? AND id > ? AND trigger_source <> ?", taskKey, runID, model.TriggerAutoRetry).
			Select("COALESCE(MIN(id), 0)").Scan(&next).Error
		if err != nil {
			return 0, time.Time{}, err
		}
		if next > 0 {
			base = func() *gorm.DB {
				return d.db.Model(&model.SyncRun{}).
					Where("task_key = ? AND trigger_source = ?", taskKey, model.TriggerAutoRetry).
					Where("id > ? AND id < ?", runID, next)
			}
		}
	}

	var count int64
	if err := base().Count(&count).Error; err != nil {
		return 0, time.Time{}, err
	}
	if count == 0 {
		return 0, time.Time{}, nil
	}
	// 取链尾行的结束时间(冷却起算点)。走 First 让 GORM 按 schema 解析时间列,
	// 不用 COALESCE(MAX(...)) —— sqlite 驱动对表达式列会丢失类型,Scan 报错。
	var last model.SyncRun
	err := base().Order("id DESC").First(&last).Error
	if err != nil {
		return 0, time.Time{}, err
	}
	lastEnd := last.StartTime
	if last.EndTime != nil {
		lastEnd = *last.EndTime
	}
	return count, lastEnd, nil
}
