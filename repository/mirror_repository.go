package repository

import (
	"context"
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
	return d.create(context.Background(), ch)
}

func (d *MirrorChannelRepository) Update(ch *model.MirrorChannel) error {
	return d.update(context.Background(), ch)
}

func (d *MirrorChannelRepository) FindAll(page Pagination) ([]*model.MirrorChannel, int64, error) {
	var channels []*model.MirrorChannel
	total, err := Paginate(d.db.Model(&model.MirrorChannel{}), page, &channels)
	return channels, total, err
}

func (d *MirrorChannelRepository) FindByID(id uint) (*model.MirrorChannel, error) {
	return d.getByID(context.Background(), id)
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
	return d.targets.create(context.Background(), t)
}

func (d *MirrorChannelRepository) UpdateTarget(t *model.MirrorTarget) error {
	return d.targets.update(context.Background(), t)
}

func (d *MirrorChannelRepository) DeleteTarget(id uint) error {
	return d.targets.deleteByID(context.Background(), id)
}

func (d *MirrorChannelRepository) FindTargetByID(id uint) (*model.MirrorTarget, error) {
	return d.targets.getByID(context.Background(), id)
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
	return d.create(context.Background(), run)
}

func (d *MirrorRunRepository) Update(run *model.MirrorRun) error {
	return d.update(context.Background(), run)
}

func (d *MirrorRunRepository) FindByID(id uint) (*model.MirrorRun, error) {
	return d.getByID(context.Background(), id)
}

func (d *MirrorRunRepository) FindByChannel(channelID uint, page Pagination) ([]*model.MirrorRun, int64, error) {
	var runs []*model.MirrorRun
	total, err := Paginate(d.db.Model(&model.MirrorRun{}).Where("channel_id = ?", channelID), page, &runs)
	return runs, total, err
}

// FindRecentByChannelLite 版本矩阵专用:只取构建矩阵需要的列,
// 不加载 Steps/Report 大 JSON(长通道历史里这两列可能占绝大部分体积);
// 按 id 倒序限制条数(矩阵只取每 tag×target 的最新一条,不需要全量历史)。
func (d *MirrorRunRepository) FindRecentByChannelLite(channelID uint, limit int) ([]*model.MirrorRun, error) {
	if limit <= 0 {
		limit = 500
	}
	var runs []*model.MirrorRun
	err := d.db.Select(
		"id, channel_id, target_id, kind, tags, status, tag_statuses, started_at, created_at",
	).Where("channel_id = ?", channelID).Order("id DESC").Limit(limit).Find(&runs).Error
	return runs, err
}

// MarkStuckFailed 批量把卡住的 pending/running 置为 failed。
// 单条 UPDATE 替代逐行读取+回写(旧实现会搬运 Steps/Report 大字段)。
func (d *MirrorRunRepository) MarkStuckFailed(olderThan time.Duration, reason string) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	now := time.Now()
	res := d.db.Model(&model.MirrorRun{}).Where(
		"status IN ? AND (started_at IS NULL AND created_at < ? OR started_at < ?)",
		[]string{model.MirrorRunPending, model.MirrorRunRunning},
		cutoff, cutoff,
	).Updates(map[string]any{
		"status":      model.MirrorRunFailed,
		"error":       reason,
		"finished_at": now,
	})
	return res.RowsAffected, res.Error
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
