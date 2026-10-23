package dao

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func newHistoryDAOTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/history.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.HistoryChannel{}))
	return db
}

func TestHistoryChannelDAO(t *testing.T) {
	db := newHistoryDAOTestDB(t)
	d := NewHistoryChannelDAO(db)

	ch := &model.HistoryChannel{
		Name:         "code-review",
		SourceDir:    "/tmp/src",
		TargetDir:    "/tmp/tgt",
		ModuleSource: "git.enjoye.top/enjoydream/code-review-service",
		ModuleTarget: "github.com/yi-nology/code-review-service",
		PathRules:    `[{"from":"docs/internal","to":"docs/archive"}]`,
		Enabled:      true,
	}
	require.NoError(t, d.Create(ch))
	require.NotZero(t, ch.ID)

	got, err := d.FindByID(ch.ID)
	require.NoError(t, err)
	require.Equal(t, "code-review", got.Name)
	require.Equal(t, "github.com/yi-nology/code-review-service", got.ModuleTarget)

	byName, err := d.FindByName("code-review")
	require.NoError(t, err)
	require.NotNil(t, byName)
	require.Equal(t, ch.ID, byName.ID)

	missing, err := d.FindByName("不存在")
	require.NoError(t, err)
	require.Nil(t, missing, "不存在的名称应返回 nil 而不是错误")

	channels, total, err := d.FindAll(Pagination{Offset: 0, Limit: 50})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, channels, 1)

	got.Enabled = false
	require.NoError(t, d.Update(got))
	reloaded, err := d.FindByID(ch.ID)
	require.NoError(t, err)
	require.False(t, reloaded.Enabled)

	require.NoError(t, d.Delete(ch.ID))
	_, err = d.FindByID(ch.ID)
	require.Error(t, err, "软删后不应再能按 ID 取到")
}
