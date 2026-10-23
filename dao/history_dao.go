package dao

import (
	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// HistoryChannelDAO 历史同步通道。
type HistoryChannelDAO struct {
	db *gorm.DB
}

// NewHistoryChannelDAO 构造 DAO。
func NewHistoryChannelDAO(db *gorm.DB) *HistoryChannelDAO {
	return &HistoryChannelDAO{db: db}
}

// Create 新建通道。
func (d *HistoryChannelDAO) Create(ch *model.HistoryChannel) error {
	return d.db.Create(ch).Error
}

// Update 更新通道。
func (d *HistoryChannelDAO) Update(ch *model.HistoryChannel) error {
	return d.db.Save(ch).Error
}

// FindAll 分页列出通道。
func (d *HistoryChannelDAO) FindAll(page Pagination) ([]*model.HistoryChannel, int64, error) {
	var channels []*model.HistoryChannel
	total, err := Paginate(d.db.Model(&model.HistoryChannel{}), page, &channels)
	return channels, total, err
}

// FindByID 按 ID 取通道。
func (d *HistoryChannelDAO) FindByID(id uint) (*model.HistoryChannel, error) {
	var ch model.HistoryChannel
	if err := d.db.First(&ch, id).Error; err != nil {
		return nil, err
	}
	return &ch, nil
}

// Delete 软删通道。
func (d *HistoryChannelDAO) Delete(id uint) error {
	return d.db.Delete(&model.HistoryChannel{}, id).Error
}

// FindByName 按名称取通道(名称唯一)。
func (d *HistoryChannelDAO) FindByName(name string) (*model.HistoryChannel, error) {
	var ch model.HistoryChannel
	if err := d.db.Where("name = ?", name).First(&ch).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &ch, nil
}

// HistoryTargetDAO 历史同步通道的推送目标。
type HistoryTargetDAO struct {
	db *gorm.DB
}

// NewHistoryTargetDAO 构造 DAO。
func NewHistoryTargetDAO(db *gorm.DB) *HistoryTargetDAO {
	return &HistoryTargetDAO{db: db}
}

// Create 新建目标。
func (d *HistoryTargetDAO) Create(t *model.HistoryTarget) error {
	return d.db.Create(t).Error
}

// Update 更新目标。
func (d *HistoryTargetDAO) Update(t *model.HistoryTarget) error {
	return d.db.Save(t).Error
}

// Delete 删除目标。
func (d *HistoryTargetDAO) Delete(id uint) error {
	return d.db.Delete(&model.HistoryTarget{}, id).Error
}

// FindByID 按 ID 取目标。
func (d *HistoryTargetDAO) FindByID(id uint) (*model.HistoryTarget, error) {
	var t model.HistoryTarget
	if err := d.db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

// FindByChannel 列出通道的目标(按 ID 升序 = 推送优先级)。
func (d *HistoryTargetDAO) FindByChannel(channelID uint) ([]*model.HistoryTarget, error) {
	var targets []*model.HistoryTarget
	if err := d.db.Where("channel_id = ?", channelID).
		Order("id ASC").Find(&targets).Error; err != nil {
		return nil, err
	}
	return targets, nil
}

// HistoryRunDAO 历史同步执行记录。
type HistoryRunDAO struct {
	db *gorm.DB
}

// NewHistoryRunDAO 构造 DAO。
func NewHistoryRunDAO(db *gorm.DB) *HistoryRunDAO {
	return &HistoryRunDAO{db: db}
}

// Create 新建执行记录。
func (d *HistoryRunDAO) Create(run *model.HistoryRun) error {
	return d.db.Create(run).Error
}

// Update 更新执行记录。
func (d *HistoryRunDAO) Update(run *model.HistoryRun) error {
	return d.db.Save(run).Error
}

// FindByID 按 ID 取执行记录。
func (d *HistoryRunDAO) FindByID(id uint) (*model.HistoryRun, error) {
	var run model.HistoryRun
	if err := d.db.First(&run, id).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// FindByChannel 分页列出通道的执行记录(新→旧)。
func (d *HistoryRunDAO) FindByChannel(channelID uint, page Pagination) ([]*model.HistoryRun, int64, error) {
	var runs []*model.HistoryRun
	total, err := Paginate(d.db.Model(&model.HistoryRun{}).Where("channel_id = ?", channelID), page, &runs)
	return runs, total, err
}

// FindLatestByChannelKind 取通道最近一次指定类别的执行记录。
//
// push 必须找 kind=apply 的记录:apply 记录才有产物路径(WorkRepoPath),
// push/rollback 记录自己不带产物。
func (d *HistoryRunDAO) FindLatestByChannelKind(channelID uint, kind string) (*model.HistoryRun, error) {
	var run model.HistoryRun
	if err := d.db.Where("channel_id = ? AND kind = ?", channelID, kind).
		Order("id DESC").First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &run, nil
}

// FindLatestByChannel 取通道最近一次执行记录。
func (d *HistoryRunDAO) FindLatestByChannel(channelID uint) (*model.HistoryRun, error) {
	var run model.HistoryRun
	if err := d.db.Where("channel_id = ?", channelID).
		Order("id DESC").First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &run, nil
}

// FindLastHashedByChannel 取该通道最近一条**已入链**的执行记录。
//
// 执行中的记录字段还在变,哈希尚未计算 —— 续链时必须跳过它们,
// 否则新记录会把 prev 指向一条没有哈希的记录。
func (d *HistoryRunDAO) FindLastHashedByChannel(channelID uint) (*model.HistoryRun, error) {
	var run model.HistoryRun
	if err := d.db.Where("channel_id = ? AND entry_hash <> ''", channelID).
		Order("id DESC").First(&run).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &run, nil
}

// ListByChannelAscending 按 ID 升序列出通道的执行记录(链校验用)。
func (d *HistoryRunDAO) ListByChannelAscending(channelID uint) ([]*model.HistoryRun, error) {
	var runs []*model.HistoryRun
	if err := d.db.Where("channel_id = ?", channelID).
		Order("id ASC").Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// HistoryOverrideDAO 历史同步的裁决覆盖。
type HistoryOverrideDAO struct {
	db *gorm.DB
}

// NewHistoryOverrideDAO 构造 DAO。
func NewHistoryOverrideDAO(db *gorm.DB) *HistoryOverrideDAO {
	return &HistoryOverrideDAO{db: db}
}

// Upsert 按 (channel_id, path) 覆盖写入裁决。
func (d *HistoryOverrideDAO) Upsert(o *model.HistoryOverride) error {
	var existing model.HistoryOverride
	err := d.db.Where("channel_id = ? AND path = ?", o.ChannelID, o.Path).First(&existing).Error
	if err == nil {
		o.ID = existing.ID
		o.CreatedAt = existing.CreatedAt
		return d.db.Save(o).Error
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return d.db.Create(o).Error
}

// FindByChannel 列出通道的全部裁决。
func (d *HistoryOverrideDAO) FindByChannel(channelID uint) ([]*model.HistoryOverride, error) {
	var list []*model.HistoryOverride
	if err := d.db.Where("channel_id = ?", channelID).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// FindByPath 取通道对某路径的裁决。
func (d *HistoryOverrideDAO) FindByPath(channelID uint, path string) (*model.HistoryOverride, error) {
	var o model.HistoryOverride
	if err := d.db.Where("channel_id = ? AND path = ?", channelID, path).First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// Delete 删除裁决。
func (d *HistoryOverrideDAO) Delete(id uint) error {
	return d.db.Delete(&model.HistoryOverride{}, id).Error
}
