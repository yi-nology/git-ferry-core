package repository

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OperationLogFilter 审计日志过滤条件。
type OperationLogFilter struct {
	Search    string // LIKE resource / detail
	Action    string // 精确匹配
	Actor     string // 精确匹配
	StartDate string // YYYY-MM-DD（含，按 created_at 起算）
	EndDate   string // YYYY-MM-DD（含，按 created_at 止算）
}

type OperationLogRepository struct {
	base[model.OperationLog, uint]
}

func NewOperationLogRepository(db *gorm.DB) *OperationLogRepository {
	return &OperationLogRepository{base: newBase[model.OperationLog, uint](db)}
}

// Create 写入一条审计日志。
func (d *OperationLogRepository) Create(log *model.OperationLog) error {
	return d.create(log)
}

// CreateChained 事务内续接哈希链并写入。
//
// 「读最新一条 → 算 prev_hash → 插入」是读改写序列:并发写会各自读到
// 同一个链尾,产生两条 prev_hash 相同的分叉,校验时被误报为篡改。
// 这里对最新一条做锁定读(MySQL `SELECT ... FOR UPDATE`),跨实例串行化;
// sqlite 由单写者事务 + 服务层互斥兜底。
func (d *OperationLogRepository) CreateChained(ctx context.Context, entry *model.OperationLog) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var last model.OperationLog
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Order("id DESC").First(&last).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			entry.PrevHash = ""
		case err != nil:
			return err
		default:
			entry.PrevHash = last.EntryHash
		}
		entry.EntryHash = model.ComputeEntryHash(entry.PrevHash, entry)
		return tx.Create(entry).Error
	})
}

// Latest 返回最新一条审计(用于哈希链续接);无记录时返回 nil,nil。
func (d *OperationLogRepository) Latest() (*model.OperationLog, error) {
	var log model.OperationLog
	err := d.db.Order("id DESC").First(&log).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &log, nil
}

// ListAscending 按 id 升序返回全部审计(哈希链校验用)。
func (d *OperationLogRepository) ListAscending() ([]*model.OperationLog, error) {
	var logs []*model.OperationLog
	err := d.db.Order("id ASC").Find(&logs).Error
	return logs, err
}

// List 按过滤条件分页查询审计日志，返回列表与总数。
func (d *OperationLogRepository) List(page Pagination, filter *OperationLogFilter) ([]*model.OperationLog, int64, error) {
	var logs []*model.OperationLog
	var total int64

	query := d.db.Model(&model.OperationLog{})

	if filter.Search != "" {
		like := "%" + EscapeLikePattern(filter.Search) + "%"
		query = query.Where("(resource LIKE ? OR detail LIKE ?)", like, like)
	}
	if filter.Action != "" {
		query = query.Where("action = ?", filter.Action)
	}
	if filter.Actor != "" {
		query = query.Where("actor = ?", filter.Actor)
	}
	if filter.StartDate != "" {
		query = query.Where("created_at >= ?", filter.StartDate+" 00:00:00")
	}
	if filter.EndDate != "" {
		query = query.Where("created_at <= ?", filter.EndDate+" 23:59:59")
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if err := query.Offset(page.Offset).Limit(page.Limit).Order("id DESC").Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

// CountSince 统计 created_at >= t 的记录数。
func (d *OperationLogRepository) CountSince(t time.Time) (int64, error) {
	var count int64
	err := d.db.Model(&model.OperationLog{}).Where("created_at >= ?", t).Count(&count).Error
	return count, err
}

// CountAll 统计全部审计日志数。
func (d *OperationLogRepository) CountAll() (int64, error) {
	var count int64
	err := d.db.Model(&model.OperationLog{}).Count(&count).Error
	return count, err
}

// StatsResult 审计日志统计结果。
type StatsResult struct {
	Today int64
	Week  int64
	Total int64
}

// StatsOnce 一次查询返回今日、近 7 天、总操作数(替代三次串行 COUNT)。
func (d *OperationLogRepository) StatsOnce(startOfToday time.Time) (*StatsResult, error) {
	var r StatsResult
	weekStart := startOfToday.AddDate(0, 0, -6)
	err := d.db.Model(&model.OperationLog{}).
		Select(
			"COUNT(*) AS total",
			"SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END) AS today",
			"SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END) AS week",
			startOfToday, weekStart,
		).
		Scan(&r).Error
	return &r, err
}
