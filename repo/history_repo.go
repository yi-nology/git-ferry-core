package repo

import (
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// HistoryChannelRepo 历史同步通道。
type HistoryChannelRepo struct {
	base[model.HistoryChannel, uint]
}

// NewHistoryChannelRepo 构造 Repo。
func NewHistoryChannelRepo(db *gorm.DB) *HistoryChannelRepo {
	return &HistoryChannelRepo{base: newBase[model.HistoryChannel, uint](db)}
}

// Create 新建通道。
func (d *HistoryChannelRepo) Create(ch *model.HistoryChannel) error {
	return d.create(ch)
}

// Update 更新通道(列白名单,不写回主键/审计列)。
func (d *HistoryChannelRepo) Update(ch *model.HistoryChannel) error {
	return d.update(ch)
}

// UpdateLastEntryHash 更新链尾锚点(只动这一列,不动整行)。
func (d *HistoryChannelRepo) UpdateLastEntryHash(id uint, hash string) error {
	return d.db.Model(&model.HistoryChannel{}).Where("id = ?", id).
		Update("last_entry_hash", hash).Error
}

// FindAll 分页列出通道。
func (d *HistoryChannelRepo) FindAll(page Pagination) ([]*model.HistoryChannel, int64, error) {
	var channels []*model.HistoryChannel
	total, err := Paginate(d.db.Model(&model.HistoryChannel{}), page, &channels)
	return channels, total, err
}

// FindByID 按 ID 取通道。
func (d *HistoryChannelRepo) FindByID(id uint) (*model.HistoryChannel, error) {
	return d.getByID(id)
}

// Delete 软删通道。
func (d *HistoryChannelRepo) Delete(id uint) error {
	return d.deleteByID(id)
}

// FindByName 按名称取通道(名称唯一)。
func (d *HistoryChannelRepo) FindByName(name string) (*model.HistoryChannel, error) {
	return d.getByCondition("name = ?", name)
}

// HistoryTargetRepo 历史同步通道的推送目标。
type HistoryTargetRepo struct {
	base[model.HistoryTarget, uint]
}

// NewHistoryTargetRepo 构造 Repo。
func NewHistoryTargetRepo(db *gorm.DB) *HistoryTargetRepo {
	return &HistoryTargetRepo{base: newBase[model.HistoryTarget, uint](db)}
}

// Create 新建目标。
func (d *HistoryTargetRepo) Create(t *model.HistoryTarget) error {
	return d.create(t)
}

// Update 更新目标(列白名单,不写回主键/审计列)。
func (d *HistoryTargetRepo) Update(t *model.HistoryTarget) error {
	return d.update(t)
}

// Delete 删除目标。
func (d *HistoryTargetRepo) Delete(id uint) error {
	return d.deleteByID(id)
}

// FindByID 按 ID 取目标。
func (d *HistoryTargetRepo) FindByID(id uint) (*model.HistoryTarget, error) {
	return d.getByID(id)
}

// FindByChannel 列出通道的目标(按 ID 升序 = 推送优先级)。
func (d *HistoryTargetRepo) FindByChannel(channelID uint) ([]*model.HistoryTarget, error) {
	var targets []*model.HistoryTarget
	if err := d.db.Where("channel_id = ?", channelID).
		Order("id ASC").Find(&targets).Error; err != nil {
		return nil, err
	}
	return targets, nil
}

// HistoryRunRepo 历史同步执行记录。
type HistoryRunRepo struct {
	base[model.HistoryRun, uint]
}

// NewHistoryRunRepo 构造 Repo。
func NewHistoryRunRepo(db *gorm.DB) *HistoryRunRepo {
	return &HistoryRunRepo{base: newBase[model.HistoryRun, uint](db)}
}

// Create 新建执行记录。
func (d *HistoryRunRepo) Create(run *model.HistoryRun) error {
	return d.create(run)
}

// Update 更新执行记录(列白名单,不写回主键/审计列)。
func (d *HistoryRunRepo) Update(run *model.HistoryRun) error {
	return d.update(run)
}

// ListPendingProductDirs 返回「还没推送成功」的 apply 记录各自的产物路径。
//
// 产物清理要用它兜底:apply 与 push 是两次独立调用,中间可能隔很久
// (人工流程、值班交接)。只看「目录多旧」就删,会把**还没推送**的产物删掉
// —— 推送必然失败,而且产物不可恢复(重做 apply 也会因为基线变了而得到不同的 head)。
//
// **一条 SQL 搞定**:待推送 = apply 记录,且没有成功的 push 记录指向它。
//
// 旧实现先全表捞 apply 记录(整行都读出来),再捞一次 push 记录,在内存里做差集。
// 而 history_runs 每 apply/push 一次就涨一行,这条查询又挂在
// **每次 apply 与每次 push 的热路径上** —— 于是「待推送产物清单」这条
// 最频繁的查询恰好是全表扫描(实测 1 万行时 EXPLAIN 给出 SCAN history_runs)。
// 现在:一条带 NOT IN 子查询的 SQL + Pluck(只取需要的列)+ 复合索引。
func (d *HistoryRunRepo) ListPendingProductDirs() ([]string, error) {
	// 已推送成功的 apply 记录:有 push 记录指向它且状态为 success。
	pushed := d.db.Model(&model.HistoryRun{}).
		Select("pushed_run_id").
		Where("kind = ? AND status = ? AND pushed_run_id <> 0",
			model.HistoryRunKindPush, model.HistoryRunSuccess)

	var out []string
	if err := d.db.Model(&model.HistoryRun{}).
		Where("kind = ? AND work_repo_path <> ''", model.HistoryRunKindApply).
		Where("id NOT IN (?)", pushed).
		Pluck("work_repo_path", &out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// Delete 删除执行记录(链尾截断的模拟与运维清理用)。
func (d *HistoryRunRepo) Delete(id uint) error {
	return d.db.Delete(&model.HistoryRun{}, id).Error
}

// FindByID 按 ID 取执行记录。
func (d *HistoryRunRepo) FindByID(id uint) (*model.HistoryRun, error) {
	return d.getByID(id)
}

// FindByChannel 分页列出通道的执行记录(新→旧)。
func (d *HistoryRunRepo) FindByChannel(channelID uint, page Pagination) ([]*model.HistoryRun, int64, error) {
	var runs []*model.HistoryRun
	total, err := Paginate(d.db.Model(&model.HistoryRun{}).Where("channel_id = ?", channelID), page, &runs)
	return runs, total, err
}

// FindLatestByChannelKind 取通道最近一次指定类别的执行记录。
//
// push 必须找 kind=apply 的记录:apply 记录才有产物路径(WorkRepoPath),
// push/rollback 记录自己不带产物。
func (d *HistoryRunRepo) FindLatestByChannelKind(channelID uint, kind string) (*model.HistoryRun, error) {
	var run model.HistoryRun
	if err := d.db.Where("channel_id = ? AND kind = ?", channelID, kind).
		Order("id DESC").First(&run).Error; err != nil {
		if oekiterrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &run, nil
}

// FindLatestByChannel 取通道最近一次执行记录。
// FindLastHashedByChannel 取该通道最近一条**已入链**的执行记录。
//
// 执行中的记录字段还在变,哈希尚未计算 —— 续链时必须跳过它们,
// 否则新记录会把 prev 指向一条没有哈希的记录。
func (d *HistoryRunRepo) FindLastHashedByChannel(channelID uint) (*model.HistoryRun, error) {
	var run model.HistoryRun
	if err := d.db.Where("channel_id = ? AND entry_hash <> ''", channelID).
		Order("id DESC").First(&run).Error; err != nil {
		if oekiterrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &run, nil
}

// ListByChannelAscending 按 ID 升序列出通道的执行记录(链校验用)。
func (d *HistoryRunRepo) ListByChannelAscending(channelID uint) ([]*model.HistoryRun, error) {
	var runs []*model.HistoryRun
	if err := d.db.Where("channel_id = ?", channelID).
		Order("id ASC").Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// HistoryOverrideRepo 历史同步的裁决覆盖。
type HistoryOverrideRepo struct {
	base[model.HistoryOverride, uint]
}

// NewHistoryOverrideRepo 构造 Repo。
func NewHistoryOverrideRepo(db *gorm.DB) *HistoryOverrideRepo {
	return &HistoryOverrideRepo{base: newBase[model.HistoryOverride, uint](db)}
}

// Upsert 按 (channel_id, path) 覆盖写入裁决。
//
// 表上 (channel_id, path) 目前只有普通索引,没有唯一约束 ⇒ 不能用
// ON CONFLICT 表达原子 upsert,只能在「查到就按列更新 / 查不到就插入」。
// 更新分支走列白名单,避免整行覆写。
func (d *HistoryOverrideRepo) Upsert(o *model.HistoryOverride) error {
	var existing model.HistoryOverride
	err := d.db.Where("channel_id = ? AND path = ?", o.ChannelID, o.Path).First(&existing).Error
	if err == nil {
		o.ID = existing.ID
		o.CreatedAt = existing.CreatedAt
		return d.update(o)
	}
	if !oekiterrors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return d.create(o)
}

// FindByChannel 列出通道的全部裁决。
func (d *HistoryOverrideRepo) FindByChannel(channelID uint) ([]*model.HistoryOverride, error) {
	var list []*model.HistoryOverride
	if err := d.db.Where("channel_id = ?", channelID).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// FindByPath 取通道对某路径的裁决。
// Delete 删除裁决。
func (d *HistoryOverrideRepo) Delete(id uint) error {
	return d.deleteByID(id)
}
