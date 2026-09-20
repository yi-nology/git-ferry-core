package dao

import (
	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

// MirrorChannelDAO 镜像通道与目标。
type MirrorChannelDAO struct {
	db *gorm.DB
}

func NewMirrorChannelDAO(db *gorm.DB) *MirrorChannelDAO {
	return &MirrorChannelDAO{db: db}
}

func (d *MirrorChannelDAO) Create(ch *model.MirrorChannel) error {
	return d.db.Create(ch).Error
}

func (d *MirrorChannelDAO) Update(ch *model.MirrorChannel) error {
	return d.db.Save(ch).Error
}

func (d *MirrorChannelDAO) FindAll(page Pagination) ([]*model.MirrorChannel, int64, error) {
	var channels []*model.MirrorChannel
	total, err := Paginate(d.db.Model(&model.MirrorChannel{}), page, &channels)
	return channels, total, err
}

func (d *MirrorChannelDAO) FindByID(id uint) (*model.MirrorChannel, error) {
	var ch model.MirrorChannel
	if err := d.db.First(&ch, id).Error; err != nil {
		return nil, err
	}
	return &ch, nil
}

// Delete 软删通道及其目标。
func (d *MirrorChannelDAO) Delete(id uint) error {
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&model.MirrorChannel{}, id).Error; err != nil {
			return err
		}
		return tx.Where("channel_id = ?", id).Delete(&model.MirrorTarget{}).Error
	})
}

func (d *MirrorChannelDAO) CreateTarget(t *model.MirrorTarget) error {
	return d.db.Create(t).Error
}

func (d *MirrorChannelDAO) UpdateTarget(t *model.MirrorTarget) error {
	return d.db.Save(t).Error
}

func (d *MirrorChannelDAO) DeleteTarget(id uint) error {
	return d.db.Delete(&model.MirrorTarget{}, id).Error
}

func (d *MirrorChannelDAO) FindTargetByID(id uint) (*model.MirrorTarget, error) {
	var t model.MirrorTarget
	if err := d.db.First(&t, id).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *MirrorChannelDAO) FindTargets(channelID uint) ([]*model.MirrorTarget, error) {
	var targets []*model.MirrorTarget
	err := d.db.Where("channel_id = ?", channelID).Order("id").Find(&targets).Error
	return targets, err
}

// MirrorRunDAO 镜像执行记录。
type MirrorRunDAO struct {
	db *gorm.DB
}

func NewMirrorRunDAO(db *gorm.DB) *MirrorRunDAO {
	return &MirrorRunDAO{db: db}
}

func (d *MirrorRunDAO) Create(run *model.MirrorRun) error {
	return d.db.Create(run).Error
}

func (d *MirrorRunDAO) Update(run *model.MirrorRun) error {
	return d.db.Save(run).Error
}

func (d *MirrorRunDAO) FindByID(id uint) (*model.MirrorRun, error) {
	var run model.MirrorRun
	if err := d.db.First(&run, id).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

func (d *MirrorRunDAO) FindByChannel(channelID uint, page Pagination) ([]*model.MirrorRun, int64, error) {
	var runs []*model.MirrorRun
	total, err := Paginate(d.db.Model(&model.MirrorRun{}).Where("channel_id = ?", channelID), page, &runs)
	return runs, total, err
}

// FindAllByChannel 不分页拉全量(版本矩阵合并用;单通道记录量可控)。
func (d *MirrorRunDAO) FindAllByChannel(channelID uint) ([]*model.MirrorRun, error) {
	var runs []*model.MirrorRun
	err := d.db.Where("channel_id = ?", channelID).Order("id DESC").Find(&runs).Error
	return runs, err
}

// FindByChannelAndTargetAndTags 精确定位某个 tag 最近一次执行(矩阵合并)。
// tags 是逗号分隔列表;用边界匹配替代旧的 %tag% 模糊匹配——
// 否则 tag "v1" 会误匹配 "v10"、"revert-v1" 等不相关记录。
func (d *MirrorRunDAO) FindByChannelAndTargetAndTags(channelID, targetID uint, tag string) ([]*model.MirrorRun, error) {
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
func (d *MirrorRunDAO) HasRunningByChannel(channelID uint) (bool, error) {
	var count int64
	err := d.db.Model(&model.MirrorRun{}).
		Where("channel_id = ? AND status IN ?", channelID,
			[]string{model.MirrorRunPending, model.MirrorRunRunning}).
		Count(&count).Error
	if err != nil {
		return false, errors.Wrap(err, "query running mirror run failed")
	}
	return count > 0, nil
}
