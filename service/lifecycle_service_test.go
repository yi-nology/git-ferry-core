package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	// 注册平台后端：测试构造 provider 时 registry 不能为空
	_ "github.com/yi-nology/go-git-platform/backends/all"
	sdkprov "github.com/yi-nology/go-git-platform/provider"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// setupLifecycleTest 造 Service + 假 GitHub（ListRepos 分页返回固定集合）。
func setupLifecycleTest(t *testing.T) (*Service, *gorm.DB, func()) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			_, _ = w.Write([]byte(`[
			  {"id":1,"full_name":"acme/existing","name":"existing","owner":{"login":"acme"},
			   "clone_url":"https://github.com/acme/existing.git","private":false,"fork":false,
			   "archived":false,"stargazers_count":1,"default_branch":"main"},
			  {"id":2,"full_name":"acme/brand-new","name":"brand-new","owner":{"login":"acme"},
			   "clone_url":"https://github.com/acme/brand-new.git","private":false,"fork":false,
			   "archived":false,"stargazers_count":0,"default_branch":"main"},
			  {"id":3,"full_name":"acme/old-archived","name":"old-archived","owner":{"login":"acme"},
			   "clone_url":"https://github.com/acme/old-archived.git","private":false,"fork":false,
			   "archived":true,"stargazers_count":0,"default_branch":"main"}
			]`))
			return
		}
		_, _ = w.Write([]byte(`[]`)) // 短页 → 终止
	}))
	t.Cleanup(srv.Close)

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "lc.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Platform{}, &model.Repo{}, &model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}))

	plat := &model.Platform{Key: "gh", Name: "GH", Type: "github", APIURL: srv.URL + "/api/v3"}
	require.NoError(t, db.Create(plat).Error)
	require.NoError(t, db.Create(&model.Repo{
		Key: "gh/acme/existing", Name: "existing", Platform: "github",
		PlatformID: plat.ID, PlatformOwner: "acme", PlatformRepo: "existing",
		CloneURL: "https://github.com/acme/existing.git",
	}).Error)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	mgr := sdkprov.NewManager(30*time.Minute, sdkprov.WithMaxSize(8))

	svc := &Service{
		config:    &model.Config{Git: model.GitConfig{TempDir: t.TempDir()}},
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
		tasks: NewTaskService(
			dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db),
			dao.NewSyncRunStepDAO(db), repoDAO,
		),
	}
	return svc, db, func() { srv.Close() }
}

// TestAutoDiscover_Diff 只读发现：本地已有不重复报、archived 被默认过滤、新增进 NewRepos。
func TestAutoDiscover_Diff(t *testing.T) {
	svc, _, cleanup := setupLifecycleTest(t)
	defer cleanup()

	rep, err := svc.AutoDiscover(context.Background(), "gh", AutoDiscoverOptions{})
	require.NoError(t, err)
	assert.Empty(t, rep.Warnings)
	assert.Equal(t, 1, rep.Existing, "本地已有 1 个仓库")
	assert.Equal(t, 2, rep.Found, "远端过滤 archived 后 2 个")
	require.Equal(t, []string{"acme/brand-new"}, rep.NewRepos)
	assert.Equal(t, 0, rep.Imported, "未开 ImportNew 不落库")

	// 平台不存在：本地列举即报错
	_, err = svc.AutoDiscover(context.Background(), "nope", AutoDiscoverOptions{})
	require.Error(t, err)
}

// TestAutoDiscover_ImportNew 开启导入：先把远端写进本地，再比对 → NewRepos 为空。
func TestAutoDiscover_ImportNew(t *testing.T) {
	svc, db, cleanup := setupLifecycleTest(t)
	defer cleanup()

	rep, err := svc.AutoDiscover(context.Background(), "gh", AutoDiscoverOptions{ImportNew: true})
	require.NoError(t, err)
	assert.Empty(t, rep.Warnings)
	assert.GreaterOrEqual(t, rep.Imported, 2, "应导入远端仓库")
	assert.Equal(t, 3, rep.Existing, "导入后本地 3 个（含 archived 也入库）")
	assert.Equal(t, 2, rep.Found, "过滤口径不因导入而改变")
	assert.Empty(t, rep.NewRepos, "全部已导入 → 无新增")

	var n int64
	require.NoError(t, db.Model(&model.Repo{}).Where("platform_id IS NOT NULL").Count(&n).Error)
	assert.GreaterOrEqual(t, n, int64(3))
}

// TestDetectDrift_NoTasks 无启用任务时返回空报告（不碰网络）。
func TestDetectDrift_NoTasks(t *testing.T) {
	svc, _, cleanup := setupLifecycleTest(t)
	defer cleanup()

	rep, err := svc.DetectDrift(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, rep.Checked)
	assert.Empty(t, rep.Items)
	assert.Zero(t, rep.Drifted)
}
