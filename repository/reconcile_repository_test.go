package repository

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func setupReconcileTestDB(t *testing.T) *ReconcileRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ReconcileRecord{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewReconcileRepository(db)
}

func mkRec(task, status string, ahead int) *model.ReconcileRecord {
	return &model.ReconcileRecord{
		TaskKey: task, Status: status,
		SourceSHA: "aaa", TargetSHA: "bbb",
		SourceAhead: ahead, Message: "m", CheckedAt: time.Now(),
	}
}

// TestReconcileDAO_UpsertKeepsLatest 同任务重复对账覆盖最新(每任务一条)。
func TestReconcileDAO_UpsertKeepsLatest(t *testing.T) {
	d := setupReconcileTestDB(t)

	require.NoError(t, d.Upsert(context.Background(), mkRec("t1", model.ReconcileDrift, 3)))
	require.NoError(t, d.Upsert(context.Background(), mkRec("t1", model.ReconcileInSync, 0)))

	got, err := d.Get(context.Background(), "t1")
	require.NoError(t, err)
	assert.Equal(t, model.ReconcileInSync, got.Status, "upsert 必须覆盖最新结果")
	assert.Zero(t, got.SourceAhead)

	n, err := d.ListAll(context.Background())
	require.NoError(t, err)
	assert.Len(t, n, 1, "每任务只留一条")
}

// TestReconcileDAO_ListDrift 漂移告警面只含 drift 与分支缺失;
// unknown/unsupported 走 ListUnknown,不污染 drift 率(#44)。
func TestReconcileDAO_ListDrift(t *testing.T) {
	d := setupReconcileTestDB(t)
	require.NoError(t, d.Upsert(context.Background(), mkRec("ok", model.ReconcileInSync, 0)))
	require.NoError(t, d.Upsert(context.Background(), mkRec("bad", model.ReconcileDrift, 2)))
	require.NoError(t, d.Upsert(context.Background(), mkRec("gone", model.ReconcileTargetMissing, 0)))
	require.NoError(t, d.Upsert(context.Background(), mkRec("auth", model.ReconcileUnknown, 0)))
	require.NoError(t, d.Upsert(context.Background(), mkRec("nocmp", model.ReconcileUnsupported, 0)))

	drift, err := d.ListDrift(context.Background())
	require.NoError(t, err)
	require.Len(t, drift, 2)
	keys := map[string]bool{}
	for _, r := range drift {
		keys[r.TaskKey] = true
	}
	assert.True(t, keys["bad"] && keys["gone"])
	assert.False(t, keys["ok"], "in_sync 不得出现在告警面")
	assert.False(t, keys["auth"] || keys["nocmp"], "unknown/unsupported 不得计入漂移面")

	unknown, err := d.ListUnknown(context.Background())
	require.NoError(t, err)
	require.Len(t, unknown, 2)
	uk := map[string]bool{}
	for _, r := range unknown {
		uk[r.TaskKey] = true
	}
	assert.True(t, uk["auth"] && uk["nocmp"])
}
