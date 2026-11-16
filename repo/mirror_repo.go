package repo

import (
	"time"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// MirrorChannelRepo 镜像通道与目标。
type MirrorChannelRepo struct {
	base[model.MirrorChannel, uint]
	targets base[model.MirrorTarget, uint]
}

func NewMirrorChannelRepo(db *gorm.DB) *MirrorChannelRepo {
	return &MirrorChannelRepo{
		base:    newBase[model.MirrorChannel, uint](db),
		targets: newBase[model.MirrorTarget, uint](db),
	}
}

func (d *MirrorChannelRepo) Create(ch *model.MirrorChannel) error {
	return d.create(ch)
}

func (d *MirrorChannelRepo) Update(ch *model.MirrorChannel) error {
	return d.update(ch)
}

func (d *MirrorChannelRepo) FindAll(page Pagination) ([]*model.MirrorChannel, int64, error) {
	var channels []*model.MirrorChannel
	total, err := Paginate(d.db.Model(&model.MirrorChannel{}), page, &channels)
	return channels, total, err
}

func (d *MirrorChannelRepo) FindByID(id uint) (*model.MirrorChannel, error) {
	var ch model.MirrorChannel
	if err := d.db.First(&ch, id).Error; err != nil {
		return nil, err
	}
	return &ch, nil
}

// Delete 软删通道及其目标。
func (d *MirrorChannelRepo) Delete(id uint) error {
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&model.MirrorChannel{}, id).Error; err != nil {
			return err
		}
		return tx.Where("channel_id = ?", id).Delete(&model.MirrorTarget{}).Error
	})
}

func (d *MirrorChannelRepo) CreateTarget(t *model.MirrorTarget) error {
	return d.targets.create(t)
}

func (d *MirrorChannelRepo) UpdateTarget(t *model.MirrorTarget) error {
	return d.targets.update(t)
}

func (d *MirrorChannelRepo) DeleteTarget(id uint) error {
	return d.targets.deleteByID(id)
}

func (d *MirrorChannelRepo) FindTargetByID(id uint) (*model.MirrorTarget, error) {
	var t model.MirrorTarget
	if err := d.db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *MirrorChannelRepo) FindTargets(channelID uint) ([]*model.MirrorTarget, error) {
	var targets []*model.MirrorTarget
	err := d.db.Where("channel_id = ?", channelID).Order("id").Find(&targets).Error
	return targets, err
}

// MirrorRunRepo 镜像执行记录。
type MirrorRunRepo struct {
	base[model.MirrorRun, uint]
}

func NewMirrorRunRepo(db *gorm.DB) *MirrorRunRepo {
	return &MirrorRunRepo{base: newBase[model.MirrorRun, uint](db)}
}

func (d *MirrorRunRepo) Create(run *model.MirrorRun) error {
	return d.create(run)
}

func (d *MirrorRunRepo) Update(run *model.MirrorRun) error {
	return d.update(run)
}

func (d *MirrorRunRepo) FindByID(id uint) (*model.MirrorRun, error) {
	var run model.MirrorRun
	if err := d.db.First(&run, id).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

func (d *MirrorRunRepo) FindByChannel(channelID uint, page Pagination) ([]*model.MirrorRun, int64, error) {
	var runs []*model.MirrorRun
	total, err := Paginate(d.db.Model(&model.MirrorRun{}).Where("channel_id = ?", channelID), page, &runs)
	return runs, total, err
}

// FindAllByChannel 不分页拉全量(版本矩阵合并用;单通道记录量可控)。
func (d *MirrorRunRepo) FindAllByChannel(channelID uint) ([]*model.MirrorRun, error) {
	var runs []*model.MirrorRun
	err := d.db.Where("channel_id = ?", channelID).Order("id DESC").Find(&runs).Error
	return runs, err
}

// FindByChannelAndTargetAndTags 精确定位某个 tag 最近一次执行(矩阵合并)。
// tags 是逗号分隔列表;用边界匹配替代旧的 %tag% 模糊匹配——
// 否则 tag "v1" 会误匹配 "v10"、"revert-v1" 等不相关记录。
func (d *MirrorRunRepo) FindByChannelAndTargetAndTags(channelID, targetID uint, tag string) ([]*model.MirrorRun, error) {
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
func (d *MirrorRunRepo) HasRunningByChannel(channelID uint) (bool, error) {
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
func (d *MirrorRunRepo) FindStuckRuns(olderThan time.Duration) ([]*model.MirrorRun, error) {
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
