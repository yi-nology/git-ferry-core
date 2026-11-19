package repository

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrationMappingRepository 源对象 → 目标对象的持久化映射(#41)。
// 身份键是 (SourcePlatform, SourceRepo, ObjectType, SourceNumber),
// 标题/正文永不参与查重。
type MigrationMappingRepository struct {
	base[model.MigrationMapping, uint]
}

func NewMigrationMappingRepository(db *gorm.DB) *MigrationMappingRepository {
	return &MigrationMappingRepository{base: newBase[model.MigrationMapping, uint](db)}
}

// Find 按源对象身份键查映射;查无此条返回 ErrNotFound。
func (d *MigrationMappingRepository) Find(sourcePlatform, sourceRepo, objectType, sourceNumber string) (*model.MigrationMapping, error) {
	return d.getByCondition(context.Background(),
		"source_platform = ? AND source_repo = ? AND object_type = ? AND source_number = ?",
		sourcePlatform, sourceRepo, objectType, sourceNumber,
	)
}

// CreatePending 先落 pending 映射(断点续迁的占位):唯一键冲突返回
// created=false —— 并发迁移同一源对象时只有一个能进入创建路径。
func (d *MigrationMappingRepository) CreatePending(m *model.MigrationMapping) (bool, error) {
	if m.Status == "" {
		m.Status = model.MigrationMappingPending
	}
	res := d.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "source_platform"}, {Name: "source_repo"}, {Name: "object_type"}, {Name: "source_number"}},
		DoNothing: true,
	}).Create(m)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// Save 回填/更新映射(按源身份键 upsert;不改变身份键)。
func (d *MigrationMappingRepository) Save(m *model.MigrationMapping) error {
	now := time.Now()
	return d.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "source_platform"}, {Name: "source_repo"}, {Name: "object_type"}, {Name: "source_number"}},
		DoUpdates: clause.Assignments(map[string]any{
			"target_platform":     m.TargetPlatform,
			"target_repo":         m.TargetRepo,
			"target_number":       m.TargetNumber,
			"source_updated_at":   m.SourceUpdatedAt,
			"content_fingerprint": m.ContentFingerprint,
			"status":              m.Status,
			"last_error":          m.LastError,
			"migrated_at":         m.MigratedAt,
			"updated_at":          now,
		}),
	}).Create(m).Error
}

// MarkFailed 创建失败:保留身份键与失败原因,下次迁移可重试。
func (d *MigrationMappingRepository) MarkFailed(sourcePlatform, sourceRepo, objectType, sourceNumber string, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	return d.db.Model(&model.MigrationMapping{}).
		Where("source_platform = ? AND source_repo = ? AND object_type = ? AND source_number = ?",
			sourcePlatform, sourceRepo, objectType, sourceNumber).
		Updates(map[string]any{
			"status":     model.MigrationMappingFailed,
			"last_error": msg,
		}).Error
}

// ListUnclosed 列出「未闭环」映射:目标编号为空、状态为 pending 的
// 记录(先写 pending 再创建,进程在创建与回填之间崩溃)。迁移报告
// 显式列出,要求人工核对;绝不静默再建一份。
// 空过滤参数 = 不过滤该维度。
func (d *MigrationMappingRepository) ListUnclosed(sourcePlatform, sourceRepo, objectType string) ([]model.MigrationMapping, error) {
	q := d.db.Where("status = ? AND (target_number IS NULL OR target_number = '')",
		model.MigrationMappingPending)
	if sourcePlatform != "" {
		q = q.Where("source_platform = ?", sourcePlatform)
	}
	if sourceRepo != "" {
		q = q.Where("source_repo = ?", sourceRepo)
	}
	if objectType != "" {
		q = q.Where("object_type = ?", objectType)
	}
	var ms []model.MigrationMapping
	err := q.Find(&ms).Error
	return ms, err
}

// DB 暴露底层句柄(测试用)。
func (d *MigrationMappingRepository) DB() *gorm.DB { return d.db }
