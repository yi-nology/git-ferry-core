package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/tpl"
)

func newTemplateStore(t *testing.T) *dbTemplateStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "tpl.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tpl.Template{}))
	return &dbTemplateStore{dao: dao.NewTemplateDAO(db)}
}

func TestDBTemplateStore_CRUD(t *testing.T) {
	st := newTemplateStore(t)
	assert.Empty(t, st.List())

	// 空 ID 自动生成
	created, err := st.Upsert(&tpl.Template{Name: "base", Tags: []string{"core"}})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	assert.False(t, created.CreatedAt.IsZero())
	assert.False(t, created.UpdatedAt.IsZero())

	got, err := st.Get(created.ID)
	require.NoError(t, err)
	assert.Equal(t, "base", got.Name)
	createdAt := got.CreatedAt

	// 更新保留 CreatedAt、刷新 UpdatedAt
	got.Name = "base-v2"
	updated, err := st.Upsert(got)
	require.NoError(t, err)
	assert.Equal(t, createdAt.Unix(), updated.CreatedAt.Unix(), "CreatedAt 应保留")
	assert.Len(t, st.List(), 1)
	assert.Len(t, st.ListByTag("core"), 1)
	assert.Empty(t, st.ListByTag("nope"))

	// 未找到
	_, err = st.Get("missing")
	require.ErrorIs(t, err, tpl.ErrNotFound)
	require.ErrorIs(t, st.Delete("missing"), tpl.ErrNotFound)

	require.NoError(t, st.Delete(created.ID))
	assert.Empty(t, st.List())
}

func TestDBTemplateStore_ResolveExtends(t *testing.T) {
	st := newTemplateStore(t)
	_, err := st.Upsert(&tpl.Template{
		ID: "parent", Name: "parent",
		Spec: tpl.Spec{Cron: "0 2 * * *", TimeoutSeconds: 60},
	})
	require.NoError(t, err)
	_, err = st.Upsert(&tpl.Template{
		ID: "child", Name: "child", Extends: "parent",
		Spec: tpl.Spec{Cron: "0 3 * * *"},
	})
	require.NoError(t, err)

	eff, chain, err := st.Resolve("child")
	require.NoError(t, err)
	assert.Equal(t, "0 3 * * *", eff.Cron, "子覆盖父")
	assert.Equal(t, 60, eff.TimeoutSeconds, "未覆盖字段继承父")
	assert.Equal(t, []string{"child", "parent"}, chain)

	// 成环
	_, err = st.Upsert(&tpl.Template{ID: "a", Name: "a", Extends: "b"})
	require.NoError(t, err)
	_, err = st.Upsert(&tpl.Template{ID: "b", Name: "b", Extends: "a"})
	require.NoError(t, err)
	_, _, err = st.Resolve("a")
	require.ErrorIs(t, err, tpl.ErrCycle)

	_, _, err = st.Resolve("missing")
	require.ErrorIs(t, err, tpl.ErrNotFound)
}

// TestMigrateLegacyTemplates 旧 json 在空表时导入；表非空不导入。
func TestMigrateLegacyTemplates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	legacy := `[{"id":"tpl-1","name":"legacy","spec":{"cron":"0 1 * * *"},"tags":["old"]}]`
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(dir, "tpl.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tpl.Template{}))
	d := dao.NewTemplateDAO(db)

	migrateLegacyTemplates(d, path)
	list, err := d.All()
	require.NoError(t, err)
	require.Len(t, list, 1, "空表应导入旧文件")
	assert.Equal(t, "tpl-1", list[0].ID)
	assert.Equal(t, "0 1 * * *", list[0].Spec.Cron)

	// 表非空 + 旧文件新增 → 不再导入
	require.NoError(t, os.WriteFile(path, []byte(`[{"id":"tpl-2","name":"new","spec":{}}]`), 0o600))
	migrateLegacyTemplates(d, path)
	list, err = d.All()
	require.NoError(t, err)
	require.Len(t, list, 1, "表非空时不应重复导入")
}
