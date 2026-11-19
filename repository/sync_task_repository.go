package repository

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type SyncTaskRepository struct {
	base[model.SyncTask, uint]
}

func NewSyncTaskRepository(db *gorm.DB) *SyncTaskRepository {
	return &SyncTaskRepository{
		base: newBaseExcluding[model.SyncTask, uint](db, "next_fire_at"),
	}
}

func (d *SyncTaskRepository) FindByRepoKey(ctx context.Context, repoKey string, page Pagination) ([]*model.SyncTask, int64, error) {
	var tasks []*model.SyncTask
	query := d.db.WithContext(ctx).Where("source_repo_key = ? OR target_repo_key = ?", repoKey, repoKey)
	total, err := Paginate(query, page, &tasks)
	return tasks, total, err
}

func (d *SyncTaskRepository) FindByKey(ctx context.Context, key string) (*model.SyncTask, error) {
	return d.getByCondition(ctx, "`key` = ?", key)
}

func (d *SyncTaskRepository) FindAllEnabled() ([]*model.SyncTask, error) {
	var tasks []*model.SyncTask
	err := d.db.Where("enabled = ? AND cron IS NOT NULL AND cron != ?", true, "").Find(&tasks).Error
	return tasks, err
}

// SeedNextFire 为尚未排程的任务播种 next_fire_at(#40)。
// 条件 next_fire_at IS NULL ⇒ 多实例同时启动只有一个写入者;
// 播种不触发执行(下一次到点才触发),避免升级/重启瞬间全量误跑。
func (d *SyncTaskRepository) SeedNextFire(key string, next time.Time) (bool, error) {
	res := d.db.Model(&model.SyncTask{}).
		Where("`key` = ? AND enabled = ? AND cron IS NOT NULL AND cron <> ? AND next_fire_at IS NULL",
			key, true, "").
		Update("next_fire_at", next)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ClaimDueFire 抢占一次到点触发(#40):条件更新 next_fire_at 到下一次
// 触发时间;只有 RowsAffected=1 的实例负责触发。条件自带
// next_fire_at <= now 的重新复核 ⇒ 多实例并发扫描时单一胜者,
// 且持有者退出后任意实例下一轮都能接手(不需要 leader election)。
func (d *SyncTaskRepository) ClaimDueFire(key string, now, next time.Time) (bool, error) {
	res := d.db.Model(&model.SyncTask{}).
		Where("`key` = ? AND enabled = ? AND cron IS NOT NULL AND cron <> ? AND next_fire_at IS NOT NULL AND next_fire_at <= ?",
			key, true, "", now).
		Update("next_fire_at", next)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ResetNextFire 清空排程(cron/启用状态变更后由调度器重新播种)。
func (d *SyncTaskRepository) ResetNextFire(key string) error {
	return d.db.Model(&model.SyncTask{}).
		Where("`key` = ?", key).
		Update("next_fire_at", nil).Error
}

func (d *SyncTaskRepository) Create(task *model.SyncTask) error {
	return d.create(context.Background(), task)
}

// Update 更新任务配置(列白名单,且排除 next_fire_at)。
//
// next_fire_at 是共享调度器的事实源,由 SeedNextFire/ClaimDueFire 条件
// 更新推进;若这里整行写回,一个普通的改名请求会把执行开始时读到的旧
// next_fire_at 写回,导致同一到点周期重复触发(或把已推进的排程倒退)。
func (d *SyncTaskRepository) Update(task *model.SyncTask) error {
	return d.update(context.Background(), task)
}

// UpdateLastRun 只更新"最近一次执行"这两个字段。
//
// **不能用 Save(整行)**:Save 会把调用方手里的**整份快照**写回去。
// 同步任务执行期间,运维改了 force_push_policy / cron / 开关,
// 任务结束时用执行开始时的旧快照整行写回 ⇒ **把并发修改悄悄改回去**
// (实测:一次 cron 执行把刚改的 force_push_policy=allow 还原成 block)。
func (d *SyncTaskRepository) UpdateLastRun(key string, lastRunAt *time.Time, lastStatus string) error {
	return d.db.Model(&model.SyncTask{}).
		Where("`key` = ?", key).
		Updates(map[string]any{
			"last_run_at": lastRunAt,
			"last_status": lastStatus,
		}).Error
}

func (d *SyncTaskRepository) Delete(key string) error {
	return d.db.Where("`key` = ?", key).Delete(&model.SyncTask{}).Error
}

// FindByRoute 按「源仓+源分支+目标仓+目标分支」查任务(批量镜像幂等用)。
// 查无此条返回 ErrNotFound。
func (d *SyncTaskRepository) FindByRoute(sourceRepoKey, sourceBranch, targetRepoKey, targetBranch string) (*model.SyncTask, error) {
	return d.getByCondition(context.Background(),
		"source_repo_key = ? AND source_branch = ? AND target_repo_key = ? AND target_branch = ?",
		sourceRepoKey, sourceBranch, targetRepoKey, targetBranch,
	)
}

func (d *SyncTaskRepository) FindAll(ctx context.Context, page Pagination) ([]*model.SyncTask, int64, error) {
	var tasks []*model.SyncTask
	total, err := Paginate(d.db.WithContext(ctx), page, &tasks)
	return tasks, total, err
}

// FindAllForExport 全量任务(不分页)。
//
// 策略导出必须是完整集合:走列表分页会被 MaxPageSize(200)钳住,
// 导出成功却静默少任务,回放到另一环境后缺失任务永远建不起来。
func (d *SyncTaskRepository) FindAllForExport(ctx context.Context) ([]*model.SyncTask, error) {
	var tasks []*model.SyncTask
	err := d.db.WithContext(ctx).Order("`key` ASC").Find(&tasks).Error
	return tasks, err
}

// FindFiltered 按仓库与最近一次执行状态过滤任务(空串=该维度不过滤)。
//
// status 匹配的是 `last_status`(最近一次执行的状态),不是"是否启用"。
// 之前 CLI 的 --status 传过来但服务端没有这个能力 ⇒ 静默返回全量,
// 用户以为筛过了。
func (d *SyncTaskRepository) FindFiltered(ctx context.Context, repoKey, status string, page Pagination) ([]*model.SyncTask, int64, error) {
	var tasks []*model.SyncTask
	query := d.db.WithContext(ctx).Model(&model.SyncTask{})
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
func (d *SyncTaskRepository) CountByStatus() (map[string]int64, error) {
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
