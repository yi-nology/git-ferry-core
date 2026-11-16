package repo

import (
	"context"

	"github.com/oekit/gormx/crud"
	"github.com/oekit/gormx/gormrepo"
	"gorm.io/gorm"
)

// base 是所有 Repo 的 gormrepo 基座:泛型 T 为实体,ID 为主键类型。
//
// 只收编**通用**按主键 CRUD(建/读/改/删);领域查询(条件列表、CAS 状态机、
// 聚合统计)仍走各 Repo 自己的 *gorm.DB——收编目标是不再手写样板,
// 不是把业务查询塞进泛型层。
type base[T any, ID comparable] struct {
	db *gorm.DB
	g  *gormrepo.GormRepository[T, ID]
	// updateCols 是 Update 的列白名单(主键/审计列除外,见 writableColumns)。
	updateCols []string
}

func newBase[T any, ID comparable](db *gorm.DB) base[T, ID] {
	return base[T, ID]{
		db:         db,
		g:          gormrepo.NewGormRepository[T, ID](db),
		updateCols: writableColumns[T](db),
	}
}

// writableColumns 返回实体除主键/审计列外的全部业务列。
//
// Update 走 gormrepo 的 SELECT 列白名单形态(而非 gorm 的 Save):
//   - 不整行覆写 id/created_at/updated_at/deleted_at;
//   - 0 行命中即结束,不再触发 Save 的 upsert 旁路(主键不存在时不会凭空插入)。
func writableColumns[T any](db *gorm.DB) []string {
	var probe T
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(probe); err != nil || stmt.Schema == nil {
		return nil
	}
	skip := map[string]struct{}{
		"id": {}, "created_at": {}, "updated_at": {}, "deleted_at": {},
	}
	cols := make([]string, 0, len(stmt.Schema.Fields))
	for _, f := range stmt.Schema.Fields {
		if f.DBName == "" {
			continue
		}
		if _, ok := skip[f.DBName]; ok {
			continue
		}
		cols = append(cols, f.DBName)
	}
	return cols
}

// ErrNotFound 查无此条。与 gormrepo/crud 同一哨兵值,调用方统一用
// errors.Is(err, repo.ErrNotFound) 判断(不再有 (nil, nil) 双形态)。
var ErrNotFound = crud.ErrNotFound

func (b *base[T, ID]) create(e *T) error {
	return b.g.Create(context.Background(), e)
}

func (b *base[T, ID]) batchCreate(entities []*T, batchSize int) error {
	return b.g.BatchCreate(context.Background(), entities, batchSize)
}

func (b *base[T, ID]) getByID(id ID) (*T, error) {
	return b.g.GetByID(context.Background(), id)
}

func (b *base[T, ID]) getByCondition(query any, args ...any) (*T, error) {
	return b.g.GetByCondition(context.Background(), query, args...)
}

func (b *base[T, ID]) update(e *T) error {
	_, err := b.g.Update(context.Background(), e, crud.UpdateOption{Columns: b.updateCols})
	return err
}

// updateFields 按字段更新(天然列白名单),返回受影响行数。
func (b *base[T, ID]) updateFields(id ID, fields map[string]any) (int64, error) {
	return b.g.UpdateFields(context.Background(), id, fields)
}

func (b *base[T, ID]) deleteByID(id ID) error {
	_, err := b.g.Delete(context.Background(), id)
	return err
}
