package dao

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func setupReconcileTestDB(t *testing.T) *ReconcileDAO {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ReconcileRecord{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewReconcileDAO(db)
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

	require.NoError(t, d.Upsert(mkRec("t1", model.ReconcileDrift, 3)))
	require.NoError(t, d.Upsert(mkRec("t1", model.ReconcileInSync, 0)))

	got, err := d.Get("t1")
	require.NoError(t, err)
	assert.Equal(t, model.ReconcileInSync, got.Status, "upsert 必须覆盖最新结果")
	assert.Zero(t, got.SourceAhead)

	n, err := d.ListAll()
	require.NoError(t, err)
	assert.Len(t, n, 1, "每任务只留一条")
}

// TestReconcileDAO_ListDrift 只列非 in_sync(告警面)。
func TestReconcileDAO_ListDrift(t *testing.T) {
	d := setupReconcileTestDB(t)
	require.NoError(t, d.Upsert(mkRec("ok", model.ReconcileInSync, 0)))
	require.NoError(t, d.Upsert(mkRec("bad", model.ReconcileDrift, 2)))
	require.NoError(t, d.Upsert(mkRec("gone", model.ReconcileTargetMissing, 0)))

	drift, err := d.ListDrift()
	require.NoError(t, err)
	require.Len(t, drift, 2)
	keys := map[string]bool{}
	for _, r := range drift {
		keys[r.TaskKey] = true
	}
	assert.True(t, keys["bad"] && keys["gone"])
	assert.False(t, keys["ok"], "in_sync 不得出现在告警面")
}
