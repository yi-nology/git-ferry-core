package repo

import (
	"context"

	"github.com/oekit/gormx/crud"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

type WebhookRuleRepo struct {
	base[model.WebhookRule, uint]
}

func NewWebhookRuleRepo(db *gorm.DB) *WebhookRuleRepo {
	return &WebhookRuleRepo{base: newBase[model.WebhookRule, uint](db)}
}

func (d *WebhookRuleRepo) FindByRepoKey(repoKey string) ([]*model.WebhookRule, error) {
	var rules []*model.WebhookRule
	err := d.db.Preload("Tasks").Where("repo_key = ?", repoKey).Find(&rules).Error
	return rules, err
}

// FindByID 按主键取规则并预加载任务关联(gormrepo 查询构建器,不再手写 Preload)。
func (d *WebhookRuleRepo) FindByID(id uint) (*model.WebhookRule, error) {
	return d.g.GetOne(context.Background(), crud.NewQuery().Where("id", "=", id).Preload("Tasks"))
}

func (d *WebhookRuleRepo) Create(rule *model.WebhookRule) error {
	return d.create(rule)
}

// Update 事务内替换任务关联，并按业务列白名单回写规则本身。
//
// 不用 Save：Save 在 0 行命中时会 upsert，且整行覆写会把并发修改与审计列
// 一并写回；Select(updateCols) 只写业务列。
func (d *WebhookRuleRepo) Update(rule *model.WebhookRule) error {
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(rule).Association("Tasks").Replace(rule.Tasks); err != nil {
			return err
		}
		return tx.Model(rule).Select(d.updateCols).Updates(rule).Error
	})
}

func (d *WebhookRuleRepo) Delete(id uint) error {
	return d.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("rule_id = ?", id).Delete(&model.WebhookRuleTask{}).Error; err != nil {
			return err
		}
		return tx.Delete(&model.WebhookRule{}, id).Error
	})
}

func (d *WebhookRuleRepo) FindTasksByRuleID(ruleID uint) ([]model.WebhookRuleTask, error) {
	var tasks []model.WebhookRuleTask
	err := d.db.Where("rule_id = ?", ruleID).Find(&tasks).Error
	return tasks, err
}
