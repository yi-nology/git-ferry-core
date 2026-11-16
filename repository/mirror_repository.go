package repository

import (
	"time"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// MirrorChannelRepository 镜像通道与目标。
type MirrorChannelRepository struct {
	base[model.MirrorChannel, uint]
	targets base[model.MirrorTarget, uint]
}

func NewMirrorChannelRepository(db *gorm.DB) *MirrorChannelRepository {
	return &MirrorChannelRepository{
		base:    newBase[model.MirrorChannel, uint](db),
		targets: newBase[model.MirrorTarget, uint](db),
	}
}

func (d *MirrorChannelRepository) Create(ch *model.MirrorChannel) error {
	return d.create(ch)
}

func (d *MirrorChannelRepository) Update(ch *model.MirrorChannel) error {
	return d.update(ch)
}

func (d *MirrorChannelRepository) FindAll(page Pagination) ([]*model.MirrorChannel, int64, error) {
	var channels []*model.MirrorChannel
	total, err := Paginate(d.db.Model(&model.MirrorChannel{}), page, &channels)
	return channels, total, err
}

func (d *MirrorChannelRepository) FindByID(id uint) (*model.MirrorChannel, error) {
	return d.getByID(id)
}

// Delete 软删通道及其目标。
func (d *MirrorChannelRepository) Delete(id uint) error {
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&model.MirrorChannel{}, id).Error; err != nil {
			return err
		}
		return tx.Where("channel_id = ?", id).Delete(&model.MirrorTarget{}).Error
	})
}

func (d *MirrorChannelRepository) CreateTarget(t *model.MirrorTarget) error {
	return d.targets.create(t)
}

func (d *MirrorChannelRepository) UpdateTarget(t *model.MirrorTarget) error {
	return d.targets.update(t)
}

func (d *MirrorChannelRepository) DeleteTarget(id uint) error {
	return d.targets.deleteByID(id)
}

func (d *MirrorChannelRepository) FindTargetByID(id uint) (*model.MirrorTarget, error) {
	return d.targets.getByID(id)
}

func (d *MirrorChannelRepository) FindTargets(channelID uint) ([]*model.MirrorTarget, error) {
	var targets []*model.MirrorTarget
	err := d.db.Where("channel_id = ?", channelID).Order("id").Find(&targets).Error
	return targets, err
}

// MirrorRunRepository 镜像执行记录。
type MirrorRunRepository struct {
	base[model.MirrorRun, uint]
}

func NewMirrorRunRepository(db *gorm.DB) *MirrorRunRepository {
	return &MirrorRunRepository{base: newBase[model.MirrorRun, uint](db)}
}

func (d *MirrorRunRepository) Create(run *model.MirrorRun) error {
	return d.create(run)
}

func (d *MirrorRunRepository) Update(run *model.MirrorRun) error {
	return d.update(run)
}

func (d *MirrorRunRepository) FindByID(id uint) (*model.MirrorRun, error) {
	return d.getByID(id)
}

func (d *MirrorRunRepository) FindByChannel(channelID uint, page Pagination) ([]*model.MirrorRun, int64, error) {
	var runs []*model.MirrorRun
	total, err := Paginate(d.db.Model(&model.MirrorRun{}).Where("channel_id = ?", channelID), page, &runs)
	return runs, total, err
}

// FindAllByChannel 不分页拉全量(版本矩阵合并用;单通道记录量可控)。
func (d *MirrorRunRepository) FindAllByChannel(channelID uint) ([]*model.MirrorRun, error) {
	var runs []*model.MirrorRun
	err := d.db.Where("channel_id = ?", channelID).Order("id DESC").Find(&runs).Error
	return runs, err
}

// FindByChannelAndTargetAndTags 精确定位某个 tag 最近一次执行(矩阵合并)。
// tags 是逗号分隔列表;用边界匹配替代旧的 %tag% 模糊匹配——
// 否则 tag "v1" 会误匹配 "v10"、"revert-v1" 等不相关记录。
func (d *MirrorRunRepository) FindByChannelAndTargetAndTags(channelID, targetID uint, tag string) ([]*model.MirrorRun, error) {
	var runs []*model.MirrorRun
	// 覆盖四种位置:唯一、列表首、列表尾、列表中间
	pat := d.db.Where(
		"channel_id = ? AND target_id = ? AND (tags = ? OR tags LIKE ? OR tags LIKE ? OR tags LIKE ?)",
		channelID, targetID, tag, tag+",%", "%,"+tag, "%,"+tag+",%",
	)
	err := pat.Order("id DESC").Find(&runs).Error
	if err != nil {
		return nil, err
	}
	return runs, nil
}

// HasRunningByChannel 是否存在进行中的执行(同通道互斥)。
func (d *MirrorRunRepository) HasRunningByChannel(channelID uint) (bool, error) {
	var count int64
	err := d.db.Model(&model.MirrorRun{}).
		Where("channel_id = ? AND status IN ?", channelID,
			[]string{model.MirrorRunPending, model.MirrorRunRunning}).
		Count(&count).Error
	if err != nil {
		return false, oekiterrors.Wrap(err, "query running mirror run failed")
	}
	return count > 0, nil
}

// FindStuckRuns 找出卡在进行中(pending/running)且**超过 olderThan** 的执行记录。
//
// 时间基准:优先 started_at(真正开跑的时刻),没有 started_at 的 pending 记录
// 用 created_at(记录创建的时刻)—— pending 说明还没开跑,创建多久了就是卡多久。
func (d *MirrorRunRepository) FindStuckRuns(olderThan time.Duration) ([]*model.MirrorRun, error) {
	cutoff := time.Now().Add(-olderThan)
	var runs []*model.MirrorRun
	err := d.db.Where(
		"status IN ? AND (started_at IS NULL AND created_at < ? OR started_at < ?)",
		[]string{model.MirrorRunPending, model.MirrorRunRunning},
		cutoff, cutoff,
	).Find(&runs).Error
	if err != nil {
		return nil, oekiterrors.Wrap(err, "query stuck mirror runs failed")
	}
	return runs, nil
}
