package repo

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 丢更新防护回归(2026-11-17 repo 层收编) =====
//
// 背景:各 Repo 的 Update 原先是 db.Save(整行)。Save 有两个事故面:
//  1. 0 行命中会 upsert(主键不存在时凭空插入);
//  2. 整行回写会把主键/审计列(created_at/updated_at/deleted_at)一起写回,
//     并发修改会被旧快照悄悄冲掉。
// 现在 Update 走 gormrepo 的列白名单形态,以下测试钉住这两条契约。

func setupSaveProtectionDB(t *testing.T) (*gorm.DB, *RepoRepo) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Repo{}, &model.ForcePushApproval{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	d, err := NewRepoRepo(db)
	require.NoError(t, err)
	return db, d
}

// TestRepoUpdate_DoesNotUpsert 主键不存在时 Update 不得凭空插入(Save 会)。
func TestRepoUpdate_DoesNotUpsert(t *testing.T) {
	db, d := setupSaveProtectionDB(t)

	require.NoError(t, d.Create(&model.Repo{Key: "k1", Name: "n"}))

	// 主键 999 不存在:gormrepo.Update 命中 0 行即结束。
	require.NoError(t, d.Update(&model.Repo{ID: 999, Key: "ghost", Name: "ghost"}))

	var n int64
	require.NoError(t, db.Model(&model.Repo{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "Update 不得走 Save 的 upsert 旁路")
}

// TestRepoUpdate_PreservesCreatedAt 列白名单不写回审计列。
func TestRepoUpdate_PreservesCreatedAt(t *testing.T) {
	db, d := setupSaveProtectionDB(t)

	require.NoError(t, d.Create(&model.Repo{Key: "k1", Name: "n"}))
	loaded, err := d.FindByKey("k1")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	origCreated := loaded.CreatedAt

	loaded.Name = "renamed"
	loaded.CreatedAt = time.Unix(0, 0) // 伪造旧快照:白名单必须忽略它
	require.NoError(t, d.Update(loaded))

	var got model.Repo
	require.NoError(t, db.First(&got, loaded.ID).Error)
	assert.Equal(t, "renamed", got.Name)
	assert.True(t, got.CreatedAt.Equal(origCreated), "created_at 不得被整行回写覆盖")
}

// TestConsumeApproved_OnlyOnce 放行是一次性的:第二次消费必须 false。
func TestConsumeApproved_OnlyOnce(t *testing.T) {
	db, _ := setupSaveProtectionDB(t)
	d := NewForcePushApprovalRepo(db)

	now := time.Now()
	require.NoError(t, db.Create(&model.ForcePushApproval{
		ID: "a1", TaskKey: "t", Branch: "main", Approved: true,
		TargetRef: "sha-1", CreatedAt: now,
	}).Error)

	ok, err := d.ConsumeApproved("t", "main", "sha-1", now)
	require.NoError(t, err)
	assert.True(t, ok, "第一次消费应命中")

	ok, err = d.ConsumeApproved("t", "main", "sha-1", now)
	require.NoError(t, err)
	assert.False(t, ok, "同一条放行不得被消费第二次(一次性语义)")
}

// TestRevoke_DoesNotUndoConsume 撤销只写 revoked_* 两列,不得把并发的
// consumed_at 覆盖回旧值。
func TestRevoke_DoesNotUndoConsume(t *testing.T) {
	db, _ := setupSaveProtectionDB(t)
	d := NewForcePushApprovalRepo(db)

	now := time.Now()
	require.NoError(t, db.Create(&model.ForcePushApproval{
		ID: "a1", TaskKey: "t", Branch: "main", Approved: true,
		TargetRef: "sha-1", CreatedAt: now,
	}).Error)

	ok, err := d.ConsumeApproved("t", "main", "sha-1", now)
	require.NoError(t, err)
	require.True(t, ok)

	found, err := d.Revoke("a1", "admin", now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, found)

	got, err := d.Get("a1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.ConsumedAt, "撤销不得把已消费状态改回未消费")
	assert.NotNil(t, got.RevokedAt)
	assert.Equal(t, "admin", got.RevokedBy)
}
