package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

func TestPlanRepoUpsert_CreateAndUpdate(t *testing.T) {
	plat := &model.Platform{ID: 1, Type: "github", InstanceURL: "github.com"}
	repos := []*sdkprov.PlatformRepo{
		{FullName: "o/new", Name: "new", CloneURL: "https://github.com/o/new.git"},
		{FullName: "o/old", Name: "old2", CloneURL: "https://github.com/o/old2.git"},
	}
	existing := map[string]*model.Repo{
		"o/old": {Key: "o/old", Name: "old", CloneURL: "https://github.com/o/old.git", PlatformOwner: "o", PlatformRepo: "old"},
	}
	plan := planRepoUpsert(plat, repos, existing, &RepoImportFilter{})
	require.Len(t, plan.ToCreate, 1)
	assert.Equal(t, "o/new", plan.ToCreate[0].Key)
	require.Len(t, plan.ToUpdate, 1)
	assert.Equal(t, "old2", plan.ToUpdate[0].Name)
}

func TestPlanRepoUpsert_FilterSkips(t *testing.T) {
	plat := &model.Platform{ID: 1, Type: "github"}
	repos := []*sdkprov.PlatformRepo{
		{FullName: "o/a", Fork: true},
		{FullName: "o/b"},
	}
	plan := planRepoUpsert(plat, repos, nil, &RepoImportFilter{ExcludeForks: true})
	assert.Len(t, plan.ToCreate, 1)
	assert.Equal(t, "o/b", plan.ToCreate[0].Key)
}

func TestBuildVersionCellsAndMatrix(t *testing.T) {
	runs := []*model.MirrorRun{
		{ID: 2, TargetID: 10, Kind: model.MirrorKindPublish, StartedAt: nil,
			TagStatuses: `{"v1":{"status":"success","commit":"c1","tree":"t1"}}`},
	}
	cells := buildVersionCells(runs)
	require.NotNil(t, cells["v1"])
	assert.Equal(t, "success", cells["v1"][10].state)

	targets := []*model.MirrorTarget{{ID: 10, TargetModule: "github.com/x/y"}}
	versions := assembleVersionMatrix(model.MirrorModePublish, targets,
		map[string]string{"v1": "c1", "v2": "c2"}, cells)
	require.Len(t, versions, 2)
	// v2 在前(倒序)
	assert.Equal(t, "v2", versions[0].Tag)
	assert.Equal(t, "v1", versions[1].Tag)
	assert.Len(t, versions[1].Targets, 1)
	assert.Equal(t, "success", versions[1].Targets[0].State)
}

func TestIndexExistingByKey(t *testing.T) {
	m := indexExistingByKey([]*model.Repo{{Key: "a/b"}, {Key: "c/d"}})
	assert.Len(t, m, 2)
	assert.Equal(t, "a/b", m["a/b"].Key)
}
