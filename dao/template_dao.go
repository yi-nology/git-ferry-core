package dao

import (
	"github.com/yi-nology/git-ferry-core/tpl"
	"gorm.io/gorm"
)

// TemplateDAO 同步策略模板（表模板 templates）。
type TemplateDAO struct {
	db *gorm.DB
}

func NewTemplateDAO(db *gorm.DB) *TemplateDAO { return &TemplateDAO{db: db} }

// All 全量返回（模板量级小，继承链解析在 service 侧做）。
func (d *TemplateDAO) All() ([]tpl.Template, error) {
	var list []tpl.Template
	err := d.db.Order("id").Find(&list).Error
	return list, err
}

// Get 按 ID 取；不存在返回 (nil, nil)。
func (d *TemplateDAO) Get(id string) (*tpl.Template, error) {
	var t tpl.Template
	err := d.db.Where("id = ?", id).First(&t).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Count 记录数（迁移判定用）。
func (d *TemplateDAO) Count() (int64, error) {
	var n int64
	err := d.db.Model(&tpl.Template{}).Count(&n).Error
	return n, err
}

// Save 按主键 upsert。
func (d *TemplateDAO) Save(t *tpl.Template) error { return d.db.Save(t).Error }

// Delete 按 ID 删除；返回是否命中。
func (d *TemplateDAO) Delete(id string) (bool, error) {
	res := d.db.Where("id = ?", id).Delete(&tpl.Template{})
	return res.RowsAffected > 0, res.Error
}
