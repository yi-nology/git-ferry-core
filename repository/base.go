package repository

import (
	"context"

	"github.com/oekit/gormx"
	"github.com/oekit/gormx/crud"
	"github.com/oekit/gormx/gormrepo"
	"github.com/oekit/pkg/contextx"
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
	return newBaseExcluding[T, ID](db)
}

// newBaseExcluding 在 Update 列白名单里额外排除实体特有列
// (如 sync_tasks.next_fire_at 由调度器条件更新独占)。
func newBaseExcluding[T any, ID comparable](db *gorm.DB, exclude ...string) base[T, ID] {
	return base[T, ID]{
		db:         db,
		g:          gormrepo.NewGormRepository[T, ID](db),
		updateCols: writableColumns[T](db, exclude...),
	}
}

// writableColumns 返回实体除主键/审计列外的全部业务列。
//
// Update 走 gormrepo 的 SELECT 列白名单形态(而非 gorm 的 Save):
//   - 不整行覆写 id/created_at/updated_at/deleted_at;
//   - 0 行命中即结束,不再触发 Save 的 upsert 旁路(主键不存在时不会凭空插入)。
func writableColumns[T any](db *gorm.DB, exclude ...string) []string {
	var probe T
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(probe); err != nil || stmt.Schema == nil {
		return nil
	}
	skip := map[string]struct{}{
		"created_at": {}, "updated_at": {}, "deleted_at": {},
	}
	// 主键列名从 schema 取,不能硬编码 "id":ReconcileResult 的主键是
	// task_key、SchemaMigration 的是 version,硬编码会把主键写进 Update。
	if pk, err := gormx.PrimaryKeyColumn(db, probe); err == nil && pk != "" {
		skip[pk] = struct{}{}
	} else {
		skip["id"] = struct{}{} // 无主键模型沿用旧行为
	}
	for _, c := range exclude {
		skip[c] = struct{}{}
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
// errors.Is(err, repository.ErrNotFound) 判断(不再有 (nil, nil) 双形态)。
var ErrNotFound = crud.ErrNotFound

func (b *base[T, ID]) create(ctx context.Context, e *T) error {
	return b.g.Create(contextx.OrBackground(ctx), e)
}

func (b *base[T, ID]) batchCreate(ctx context.Context, entities []*T, batchSize int) error {
	return b.g.BatchCreate(contextx.OrBackground(ctx), entities, batchSize)
}

func (b *base[T, ID]) getByID(ctx context.Context, id ID) (*T, error) {
	return b.g.GetByID(contextx.OrBackground(ctx), id)
}

func (b *base[T, ID]) getByCondition(ctx context.Context, query any, args ...any) (*T, error) {
	return b.g.GetByCondition(contextx.OrBackground(ctx), query, args...)
}

func (b *base[T, ID]) update(ctx context.Context, e *T) error {
	_, err := b.g.Update(contextx.OrBackground(ctx), e, crud.UpdateOption{Columns: b.updateCols})
	return err
}

// updateInTx 同 update,但在调用方事务内执行(同一列白名单口径)。
func (b *base[T, ID]) updateInTx(ctx context.Context, tx *gorm.DB, e *T) error {
	ctx = contextx.OrBackground(ctx)
	_, err := gormrepo.NewGormRepository[T, ID](tx.WithContext(ctx)).
		Update(ctx, e, crud.UpdateOption{Columns: b.updateCols})
	return err
}

// updateFields 按字段更新(天然列白名单),返回受影响行数。
func (b *base[T, ID]) updateFields(ctx context.Context, id ID, fields map[string]any) (int64, error) {
	return b.g.UpdateFields(contextx.OrBackground(ctx), id, fields)
}

func (b *base[T, ID]) deleteByID(ctx context.Context, id ID) error {
	_, err := b.g.Delete(contextx.OrBackground(ctx), id)
	return err
}
