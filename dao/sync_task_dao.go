package dao

import (
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type SyncTaskDAO struct {
	db *gorm.DB
}

func NewSyncTaskDAO(db *gorm.DB) *SyncTaskDAO {
	return &SyncTaskDAO{db: db}
}

func (d *SyncTaskDAO) FindByRepoKey(repoKey string, page Pagination) ([]*model.SyncTask, int64, error) {
	var tasks []*model.SyncTask
	query := d.db.Where("source_repo_key = ? OR target_repo_key = ?", repoKey, repoKey)
	total, err := Paginate(query, page, &tasks)
	return tasks, total, err
}

func (d *SyncTaskDAO) FindByKey(key string) (*model.SyncTask, error) {
	return FindByKey[model.SyncTask](d.db, key)
}

func (d *SyncTaskDAO) FindAllEnabled() ([]*model.SyncTask, error) {
	var tasks []*model.SyncTask
	err := d.db.Where("enabled = ? AND cron IS NOT NULL AND cron != ?", true, "").Find(&tasks).Error
	return tasks, err
}

func (d *SyncTaskDAO) Create(task *model.SyncTask) error {
	return d.db.Create(task).Error
}

func (d *SyncTaskDAO) Update(task *model.SyncTask) error {
	return d.db.Save(task).Error
}

// UpdateLastRun 只更新"最近一次执行"这两个字段。
//
// **不能用 Save(整行)**:Save 会把调用方手里的**整份快照**写回去。
// 同步任务执行期间,运维改了 force_push_policy / cron / 开关,
// 任务结束时用执行开始时的旧快照整行写回 ⇒ **把并发修改悄悄改回去**
// (实测:一次 cron 执行把刚改的 force_push_policy=allow 还原成 block)。
func (d *SyncTaskDAO) UpdateLastRun(key string, lastRunAt *time.Time, lastStatus string) error {
	return d.db.Model(&model.SyncTask{}).
		Where("`key` = ?", key).
		Updates(map[string]any{
			"last_run_at": lastRunAt,
			"last_status": lastStatus,
		}).Error
}

func (d *SyncTaskDAO) Delete(key string) error {
	return d.db.Where("`key` = ?", key).Delete(&model.SyncTask{}).Error
}

func (d *SyncTaskDAO) FindAll(page Pagination) ([]*model.SyncTask, int64, error) {
	var tasks []*model.SyncTask
	total, err := Paginate(d.db, page, &tasks)
	return tasks, total, err
}

// FindFiltered 按仓库与最近一次执行状态过滤任务(空串=该维度不过滤)。
//
// status 匹配的是 `last_status`(最近一次执行的状态),不是"是否启用"。
// 之前 CLI 的 --status 传过来但服务端没有这个能力 ⇒ 静默返回全量,
// 用户以为筛过了。
func (d *SyncTaskDAO) FindFiltered(repoKey, status string, page Pagination) ([]*model.SyncTask, int64, error) {
	var tasks []*model.SyncTask
	query := d.db.Model(&model.SyncTask{})
	if repoKey != "" {
		query = query.Where("source_repo_key = ? OR target_repo_key = ?", repoKey, repoKey)
	}
	if status != "" {
		query = query.Where("last_status = ?", status)
	}
	total, err := Paginate(query, page, &tasks)
	return tasks, total, err
}

// CountByStatus 按_last_status 聚合计数,返回 map[status]count;
// 总数放在 "total" 键。用于面板计数,避免全表加载进内存。
func (d *SyncTaskDAO) CountByStatus() (map[string]int64, error) {
	var rows []struct {
		LastStatus string
		Cnt        int64
	}
	if err := d.db.Model(&model.SyncTask{}).
		Select("last_status, count(*) as cnt").
		Group("last_status").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows)+1)
	var total int64
	for _, r := range rows {
		out[r.LastStatus] = r.Cnt
		total += r.Cnt
	}
	out["total"] = total
	return out, nil
}
