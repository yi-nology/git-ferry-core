package service

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/tpl"
)

// TemplateStore 模板库访问接口。
// 生产用 DB 实现（dbTemplateStore）；测试/过渡可注入文件实现（*tpl.Store），
// 两者共用 tpl 的类型与继承链解析（tpl.ResolveSelf）。
type TemplateStore interface {
	List() []tpl.Template
	ListByTag(tag string) []tpl.Template
	Get(id string) (*tpl.Template, error)
	Upsert(t *tpl.Template) (*tpl.Template, error)
	Delete(id string) error
	Resolve(id string) (tpl.Spec, []string, error)
}

// dbTemplateStore 表 backing 的模板库（表 templates）。
type dbTemplateStore struct {
	dao *dao.TemplateDAO
}

func (s *dbTemplateStore) List() []tpl.Template {
	list, err := s.dao.All()
	if err != nil {
		slog.Error("template list failed", "error", err)
		return []tpl.Template{}
	}
	return list
}

func (s *dbTemplateStore) ListByTag(tag string) []tpl.Template {
	out := []tpl.Template{}
	for _, t := range s.List() {
		for _, x := range t.Tags {
			if x == tag {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

func (s *dbTemplateStore) Get(id string) (*tpl.Template, error) {
	t, err := s.dao.Get(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, tpl.ErrNotFound
	}
	return t, nil
}

// Upsert 语义与 tpl.Store.Upsert 一致：空 ID 生成 tpl-<unixnano>，
// 更新保留 CreatedAt、刷新 UpdatedAt。
func (s *dbTemplateStore) Upsert(t *tpl.Template) (*tpl.Template, error) {
	if t.ID == "" {
		t.ID = fmt.Sprintf("tpl-%d", time.Now().UnixNano())
	}
	now := time.Now()
	existing, err := s.dao.Get(t.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		t.CreatedAt = existing.CreatedAt
		t.UpdatedAt = now
	} else {
		t.CreatedAt, t.UpdatedAt = now, now
	}
	if err := s.dao.Save(t); err != nil {
		return nil, err
	}
	out := *t
	return &out, nil
}

func (s *dbTemplateStore) Delete(id string) error {
	ok, err := s.dao.Delete(id)
	if err != nil {
		return err
	}
	if !ok {
		return tpl.ErrNotFound
	}
	return nil
}

func (s *dbTemplateStore) Resolve(id string) (tpl.Spec, []string, error) {
	list, err := s.dao.All()
	if err != nil {
		return tpl.Spec{}, nil, err
	}
	return tpl.ResolveSelf(list, id)
}

// migrateLegacyTemplates 旧 data/templates.json 在**表为空**时一次性导入。
// 幂等：只在空表执行，重复启动不再导入；失败仅告警不阻断启动。
func migrateLegacyTemplates(d *dao.TemplateDAO, path string) {
	n, err := d.Count()
	if err != nil || n > 0 {
		return
	}
	legacy, lerr := tpl.Open(path) // 只读解析旧文件
	if lerr != nil {
		slog.Warn("legacy templates unreadable, skip import", "path", path, "error", lerr)
		return
	}
	items := legacy.List()
	if len(items) == 0 {
		return
	}
	imported := 0
	for i := range items {
		if items[i].ID == "" || items[i].Name == "" {
			continue
		}
		if err := d.Save(&items[i]); err != nil {
			slog.Warn("import legacy template failed", "id", items[i].ID, "error", err)
			continue
		}
		imported++
	}
	if imported > 0 {
		slog.Info("imported legacy templates into db", "count", imported, "path", path)
	}
}
