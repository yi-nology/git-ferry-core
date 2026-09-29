package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupOpLogSvc(t *testing.T) (*OperationLogService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.OperationLog{}))
	return NewOperationLogService(dao.NewOperationLogDAO(db)), db
}

func TestAuditHashChain(t *testing.T) {
	svc, db := setupOpLogSvc(t)

	e1 := &model.OperationLog{Action: "create", ResourceType: "repo", ResourceKey: "r1", Actor: "a"}
	require.NoError(t, svc.Record(t.Context(), e1))
	assert.NotEmpty(t, e1.EntryHash)
	assert.Empty(t, e1.PrevHash)

	e2 := &model.OperationLog{Action: "update", ResourceType: "repo", ResourceKey: "r1", Actor: "a"}
	require.NoError(t, svc.Record(t.Context(), e2))
	assert.Equal(t, e1.EntryHash, e2.PrevHash)

	res, err := svc.VerifyAuditChain()
	require.NoError(t, err)
	assert.True(t, res.OK, res.Message)
	assert.Equal(t, 2, res.Checked)

	// 篡改 e2
	require.NoError(t, db.Model(&model.OperationLog{}).
		Where("id = ?", e2.ID).Update("resource", "hacked").Error)
	res, err = svc.VerifyAuditChain()
	require.NoError(t, err)
	assert.False(t, res.OK)
}

func TestResolveForcePushPolicy(t *testing.T) {
	assert.Equal(t, ForcePushBlock, ResolveForcePushPolicy(&model.SyncTask{KeepDivergent: true}))
	assert.Equal(t, ForcePushAllow, ResolveForcePushPolicy(&model.SyncTask{KeepDivergent: false}))
	assert.Equal(t, ForcePushBackupOnDemand,
		ResolveForcePushPolicy(&model.SyncTask{ForcePushPolicy: "backup_on_demand"}))
	assert.Equal(t, ForcePushBlock, ResolveForcePushPolicy(nil))
	assert.True(t, ValidForcePushPolicy("allow"))
	assert.False(t, ValidForcePushPolicy("nope"))
}

func TestRPOMetricHuman(t *testing.T) {
	assert.Equal(t, "45s", humanSeconds(45))
	assert.Equal(t, "5m", humanSeconds(300))
	assert.Equal(t, "2h", humanSeconds(7200))
	assert.Equal(t, "3d", humanSeconds(3*86400))
}

func TestComputeEntryHashStable(t *testing.T) {
	e := &model.OperationLog{
		Action: "run", ResourceType: "task", ResourceKey: "t",
		Actor: "x", CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	h1 := model.ComputeEntryHash("prev", e)
	h2 := model.ComputeEntryHash("prev", e)
	assert.Equal(t, h1, h2)
	h3 := model.ComputeEntryHash("other", e)
	assert.NotEqual(t, h1, h3)
}
