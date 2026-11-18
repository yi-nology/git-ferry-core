package repository

import (
	"github.com/yi-nology/git-ferry-core/tpl"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TemplateRepository 同步策略模板（表模板 templates）。
type TemplateRepository struct {
	base[tpl.Template, string]
}

func NewTemplateRepository(db *gorm.DB) *TemplateRepository {
	return &TemplateRepository{base: newBase[tpl.Template, string](db)}
}

// All 全量返回（模板量级小，继承链解析在 service 侧做）。
func (d *TemplateRepository) All() ([]tpl.Template, error) {
	var list []tpl.Template
	err := d.db.Order("id").Find(&list).Error
	return list, err
}

// Get 按 ID 取；不存在返回 (nil, nil)。
func (d *TemplateRepository) Get(id string) (*tpl.Template, error) {
	return d.getByID(id)
}

// Save 按主键 upsert（列白名单：只覆盖业务列，不写回 id/created_at）。
func (d *TemplateRepository) Save(t *tpl.Template) error {
	return d.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "description", "extends", "match", "spec", "tags", "updated_at"}),
	}).Create(t).Error
}

// Delete 按 ID 删除；返回是否命中。
func (d *TemplateRepository) Delete(id string) (bool, error) {
	res := d.db.Where("id = ?", id).Delete(&tpl.Template{})
	return res.RowsAffected > 0, res.Error
}
