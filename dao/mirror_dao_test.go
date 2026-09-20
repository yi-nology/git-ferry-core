package dao

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
)

func setupMirrorTestDB(t *testing.T) (*gorm.DB, *MirrorChannelDAO, *MirrorRunDAO) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.MirrorChannel{}, &model.MirrorTarget{}, &model.MirrorRun{}))
	return db, NewMirrorChannelDAO(db), NewMirrorRunDAO(db)
}

func TestMirrorChannelCRUD(t *testing.T) {
	db, d, _ := setupMirrorTestDB(t)

	ch := &model.MirrorChannel{Name: "agentkit", Mode: model.MirrorModePublish, RepoKey: "agentkit", Module: "git.enjoye.top/enjoydream/agentkit"}
	require.NoError(t, d.Create(ch))

	require.NoError(t, d.CreateTarget(&model.MirrorTarget{ChannelID: ch.ID, Remote: "github", RepoURL: "https://github.com/yi-nology/agentkit.git", TargetModule: "github.com/yi-nology/agentkit"}))

	got, err := d.FindByID(ch.ID)
	require.NoError(t, err)
	assert.Equal(t, "publish", got.Mode)

	targets, err := d.FindTargets(ch.ID)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, "github.com/yi-nology/agentkit", targets[0].TargetModule)

	require.NoError(t, d.Delete(ch.ID))
	_, err = d.FindByID(ch.ID)
	assert.Error(t, err, "软删后不应可查")
	// 目标级联删除(hard delete)
	targets, err = d.FindTargets(ch.ID)
	require.NoError(t, err)
	assert.Empty(t, targets)

	_ = db
}

func TestMirrorRunLifecycle(t *testing.T) {
	_, d, r := setupMirrorTestDB(t)

	ch := &model.MirrorChannel{Name: "c", Mode: model.MirrorModePublish, RepoKey: "k"}
	require.NoError(t, d.Create(ch))

	run := &model.MirrorRun{ChannelID: ch.ID, TargetID: 1, Kind: model.MirrorKindPublish, Tags: `"v1.0.0"`, Status: model.MirrorRunPending, TagStatuses: "{}", Steps: "[]"}
	require.NoError(t, r.Create(run))

	ok, err := r.HasRunningByChannel(ch.ID)
	require.NoError(t, err)
	assert.True(t, ok, "pending 应视为进行中")

	run.Status = model.MirrorRunSuccess
	require.NoError(t, r.Update(run))
	ok, err = r.HasRunningByChannel(ch.ID)
	require.NoError(t, err)
	assert.False(t, ok)

	runs, err := r.FindAllByChannel(ch.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
}
