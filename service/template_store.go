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
