package repository

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func setupMigrationMappingDB(t *testing.T) *MigrationMappingRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "map.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.MigrationMapping{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewMigrationMappingRepository(db)
}

// TestMigrationMapping_IdentityIsSourceNumberNotTitle 身份键是
// 平台/仓库/类型/源编号;同标题的不同源编号各占一条映射(#41)。
func TestMigrationMapping_IdentityIsSourceNumberNotTitle(t *testing.T) {
	d := setupMigrationMappingDB(t)

	mk := func(num string) *model.MigrationMapping {
		return &model.MigrationMapping{
			SourcePlatform: "github", SourceRepo: "acme/src", ObjectType: model.MigrationObjectIssue,
			SourceNumber: num, Status: model.MigrationMappingPending,
		}
	}
	created, err := d.CreatePending(mk("1"))
	require.NoError(t, err)
	assert.True(t, created)
	created, err = d.CreatePending(mk("2"))
	require.NoError(t, err)
	assert.True(t, created, "同标题不同编号必须各自建映射")
	created, err = d.CreatePending(mk("1"))
	require.NoError(t, err)
	assert.False(t, created, "同一源编号重复占位必须被唯一键挡住")

	// 回填目标编号与状态。
	m, err := d.Find("github", "acme/src", model.MigrationObjectIssue, "1")
	require.NoError(t, err)
	require.NotNil(t, m)
	m.TargetPlatform = "gitlab"
	m.TargetRepo = "acme/dst"
	m.TargetNumber = "42"
	m.Status = model.MigrationMappingMigrated
	require.NoError(t, d.Save(m))

	got, err := d.Find("github", "acme/src", model.MigrationObjectIssue, "1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "42", got.TargetNumber)
	assert.Equal(t, model.MigrationMappingMigrated, got.Status)

	// #2 仍是 pending 且无目标编号 ⇒ 未闭环可被报告。
	unclosed, err := d.ListUnclosed("github", "acme/src", model.MigrationObjectIssue)
	require.NoError(t, err)
	require.Len(t, unclosed, 1)
	assert.Equal(t, "2", unclosed[0].SourceNumber)
}
