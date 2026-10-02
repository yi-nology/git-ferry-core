package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	_ "github.com/yi-nology/go-git-platform/backends/all"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"
)

// setupOrgBulkDB 临时 sqlite 建库 + 组装 org 导入/镜像编排所需的最小 Service 装配
// （只依赖 repos / tasks / platforms 三个子服务，不起 cron 与后台协程）。
func setupOrgBulkDB(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "orgbulk.db")), &gorm.Config{})
	require.NoError(t, err, "open test db")
	require.NoError(t, db.AutoMigrate(
		&model.Platform{}, &model.Repo{},
		&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{},
	), "migrate test db")

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	mgr := sdkprov.NewManager(0)

	svc := &Service{
		config:    &Config{Git: model.GitConfig{TempDir: t.TempDir()}},
		db:        db,
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		tasks:     NewTaskService(dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db), dao.NewSyncRunStepDAO(db), repoDAO),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
	}
	return svc, db
}

func mustCreatePlatform(t *testing.T, db *gorm.DB, p *model.Platform) *model.Platform {
	t.Helper()
	require.NoError(t, db.Create(p).Error)
	require.NotZero(t, p.ID)
	return p
}

func countRows(t *testing.T, db *gorm.DB, m any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(m).Count(&n).Error)
	return n
}

// ===== rewriteRepoURL / 组织镜像映射（自壳 org_mirror_helpers_test.go 迁移，行为锁定不变） =====

func TestRewriteRepoURL(t *testing.T) {
	got := RewriteRepoURL("https://github.com/old/oldrepo.git", "new", "newrepo")
	if want := "https://github.com/new/newrepo.git"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	got = RewriteRepoURL("git@github.com:old/oldrepo.git", "new", "newrepo")
	if want := "git@github.com:new/newrepo.git"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	// 空值原样返回
	if got := RewriteRepoURL("", "a", "b"); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := RewriteRepoURL("https://h/a/b.git", "", ""); got != "https://h/a/b.git" {
		t.Fatalf("got %q", got)
	}
}

// TestMirrorOrgTarget 自壳 TestOrgMirrorResolveTarget 迁移：
// preserve 恒等、single/flat 落点、mixed 个人仓按 targetUser 判定、
// mixed 组织仓落 target_org，缺落点静默回落源 owner。
func TestMirrorOrgTarget(t *testing.T) {
	cases := []struct {
		strategy               string
		owner, repo, org, user string
		wantOwner, wantRepo    string
	}{
		{"preserve", "acme", "api", "", "", "acme", "api"},
		{"single", "acme", "api", "backup", "", "backup", "api"},
		{"flat", "acme", "api", "", "mirror", "mirror", "api"},
		{"mixed", "alice", "dot", "backup", "alice", "alice", "dot"},
		{"mixed", "acme", "api", "backup", "alice", "backup", "api"},
	}
	for i, c := range cases {
		o, r := mirrorOrgTarget(OrgMapStrategy(c.strategy), c.owner, c.repo, c.org, c.user)
		if o != c.wantOwner || r != c.wantRepo {
			t.Fatalf("case %d: got %s/%s want %s/%s", i, o, r, c.wantOwner, c.wantRepo)
		}
	}
}

// ===== ImportPublicOrg =====

// TestImportPublicOrg_PlatformNotFound 平台缺失归一化为 ErrPlatformNotFound（壳层据此回 404）。
func TestImportPublicOrg_PlatformNotFound(t *testing.T) {
	svc, _ := setupOrgBulkDB(t)
	_, err := svc.ImportPublicOrg(context.Background(), PublicOrgImportRequest{
		PlatformKey: "nope", Org: "acme", DryRun: true,
	})
	require.ErrorIs(t, err, ErrPlatformNotFound)
}

// TestImportPublicOrg_DryRunDoesNotWrite dry_run 不碰库：不导入、不建任务。
func TestImportPublicOrg_DryRunDoesNotWrite(t *testing.T) {
	svc, db := setupOrgBulkDB(t)
	mustCreatePlatform(t, db, &model.Platform{
		Key: "gl-dry", Name: "GL", Type: "gitlab", APIURL: "https://gl.example.com/api/v4",
	})

	res, err := svc.ImportPublicOrg(context.Background(), PublicOrgImportRequest{
		PlatformKey: "gl-dry", Org: "acme", CreateTasks: true, DryRun: true,
		Filter: &RepoImportFilter{ExcludeArchived: true},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Empty(t, res.Items, "非 github 平台无预览项")
	assert.Empty(t, res.Warnings, "非 github 平台无列仓警告；dry_run 文案由壳层拼装")
	assert.Zero(t, res.Imported)
	assert.Zero(t, res.CreatedTasks)
	assert.Zero(t, countRows(t, db, &model.Repo{}), "dry_run 不得写仓库")
	assert.Zero(t, countRows(t, db, &model.SyncTask{}), "dry_run 不得写任务")
}

// TestImportPublicOrg_FullRun 编排全链路：列公开仓预览 → 过滤导入 → 按 org 建任务。
//
//   - 预览只收公开仓，不套 RepoImportFilter；
//   - 导入走 SyncPlatformReposFiltered（ExcludeForks 裁掉 fork 仓）；
//   - 建任务扫描前 200 仓，按 "/<org>/" 或 "<platform>/<org>/" 命中，
//     落点 = single + TargetOrg/TargetPlatform（见 createOrgMirrorTasks）。
func TestImportPublicOrg_FullRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id": 1, "full_name": "bulksrc/pub1", "name": "pub1",
				"owner":     map[string]any{"login": "bulksrc"},
				"clone_url": "https://github.com/bulksrc/pub1.git",
				"private":   false, "fork": false, "archived": false,
				"stargazers_count": 10, "default_branch": "main",
			},
			{
				"id": 2, "full_name": "bulksrc/forked", "name": "forked",
				"owner":     map[string]any{"login": "bulksrc"},
				"clone_url": "https://github.com/bulksrc/forked.git",
				"private":   false, "fork": true, "archived": false,
				"stargazers_count": 0, "default_branch": "main",
			},
		})
	}))
	defer srv.Close()

	svc, db := setupOrgBulkDB(t)
	mustCreatePlatform(t, db, &model.Platform{
		Key: "gh-bulk", Name: "GH", Type: "github", APIURL: srv.URL + "/api/v3",
	})
	// 预置 3 段 key 的存量仓：用于命中组织过滤并验证建任务循环。
	require.NoError(t, db.Create(&model.Repo{
		Key: "github/bulksrc/app", Name: "app", Platform: "github",
		PlatformOwner: "bulksrc", PlatformRepo: "app",
		CloneURL: "https://github.com/bulksrc/app.git", Status: model.RepoStatusActive,
	}).Error)

	res, err := svc.ImportPublicOrg(context.Background(), PublicOrgImportRequest{
		PlatformKey: "gh-bulk", Org: "bulksrc",
		CreateTasks: true, TargetOrg: "backup", TargetPlatform: "gl",
		DryRun: false, Max: 100,
		Filter: &RepoImportFilter{ExcludeForks: true},
	})
	require.NoError(t, err)

	// 预览：两个公开仓（预览阶段不过滤 fork）
	require.Len(t, res.Items, 2)
	assert.Equal(t, "bulksrc/pub1", res.Items[0].FullName)
	assert.Equal(t, "https://github.com/bulksrc/pub1.git", res.Items[0].CloneURL)
	assert.Equal(t, 10, res.Items[0].Stars)
	assert.False(t, res.Items[0].Fork)
	assert.Equal(t, "bulksrc/forked", res.Items[1].FullName)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "按平台凭证列取组织公开仓；大组织可能只拉到部分元数据", res.Warnings[0])

	// 导入：ExcludeForks 裁掉 fork 仓 → 只落 1 条新记录
	assert.Equal(t, 1, res.Imported)
	assert.EqualValues(t, 2, countRows(t, db, &model.Repo{}), "存量 1 + 新导 1")

	// 建任务：只有 3 段 key 的存量仓命中组织过滤，落点 single→backup
	require.Equal(t, 1, res.CreatedTasks)
	var task model.SyncTask
	require.NoError(t, db.Where("name = ?", "app-mirror").First(&task).Error)
	assert.Equal(t, "github/bulksrc/app", task.SourceRepoKey)
	assert.Equal(t, "gl/backup/app", task.TargetRepoKey)
	assert.Equal(t, "all", task.SyncMode)
}

// ===== BulkMirrorOrg =====

// seedMirrorRepos 源组织仓：acme 两仓 + 旁组织仓（应被过滤）+ 个人仓 alice/dot。
func seedMirrorRepos(t *testing.T, db *gorm.DB, platformID uint) {
	t.Helper()
	rows := []*model.Repo{
		{Key: "m-app", Name: "app", PlatformID: platformID, Platform: "gitlab",
			PlatformOwner: "acme", PlatformRepo: "app",
			CloneURL: "https://src.example.com/acme/app.git", Status: model.RepoStatusActive},
		{Key: "m-api", Name: "api", PlatformID: platformID, Platform: "gitlab",
			PlatformOwner: "acme", PlatformRepo: "api",
			CloneURL: "git@src.example.com:acme/api.git", Status: model.RepoStatusActive},
		{Key: "m-other", Name: "x", PlatformID: platformID, Platform: "gitlab",
			PlatformOwner: "other", PlatformRepo: "x",
			CloneURL: "https://src.example.com/other/x.git", Status: model.RepoStatusActive},
		{Key: "m-dot", Name: "dot", PlatformID: platformID, Platform: "gitlab",
			PlatformOwner: "alice", PlatformRepo: "dot",
			CloneURL: "https://src.example.com/alice/dot.git", Status: model.RepoStatusActive},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error)
	}
}

func setupMirrorFixture(t *testing.T) (svc *Service, db *gorm.DB, dst *model.Platform) {
	t.Helper()
	svc, db = setupOrgBulkDB(t)
	src := mustCreatePlatform(t, db, &model.Platform{
		Key: "src-plat", Name: "SRC", Type: "gitlab", APIURL: "https://src.example.com/api/v4",
	})
	dst = mustCreatePlatform(t, db, &model.Platform{
		Key: "dst-plat", Name: "DST", Type: "gitlab",
		APIURL: "https://dst.example.com/api/v4", InstanceURL: "https://dst.example.com",
	})
	seedMirrorRepos(t, db, src.ID)
	return svc, db, dst
}

// TestBulkMirrorOrg_PlatformMissing 源平台记录缺失时上抛（壳层映射 404 文案）。
func TestBulkMirrorOrg_PlatformMissing(t *testing.T) {
	svc, db := setupOrgBulkDB(t)
	dst := mustCreatePlatform(t, db, &model.Platform{Key: "dst-x", Name: "D", Type: "gitlab"})

	_, err := svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
		SourcePlatformKey: "nope", SourceOrg: "acme",
		TargetPlatform: dst, Strategy: OrgMapSingle, TargetOrg: "backup", DryRun: true,
	})
	require.Error(t, err)
}

// TestBulkMirrorOrg_DryRun dry_run 只算落点与计数：planned、action=planned、零写入。
func TestBulkMirrorOrg_DryRun(t *testing.T) {
	svc, db, dst := setupMirrorFixture(t)

	res, err := svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
		SourcePlatformKey: "src-plat", SourceOrg: "acme",
		TargetPlatform: dst, Strategy: OrgMapSingle, TargetOrg: "backup",
		CreateTasks: true, DryRun: true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, res.Planned, "只统计源 org 的两仓，旁组织仓与个人仓被过滤")
	assert.Zero(t, res.Imported)
	assert.Zero(t, res.TasksCreated)
	require.Len(t, res.Items, 2)
	targets := map[string]string{}
	for _, it := range res.Items {
		assert.Equal(t, "planned", it.Action)
		assert.Empty(t, it.Message)
		targets[it.Source] = it.Target
	}
	assert.Equal(t, "backup/app", targets["acme/app"])
	assert.Equal(t, "backup/api", targets["acme/api"])
	assert.NotContains(t, targets, "other/x", "旁组织仓必须被过滤")
	assert.NotContains(t, targets, "alice/dot", "个人仓不在 SourceOrg 内")
	assert.Empty(t, res.Warnings, "dry_run 文案由壳层拼装")
	assert.EqualValues(t, 4, countRows(t, db, &model.Repo{}), "dry_run 不得写目标仓")
	assert.Zero(t, countRows(t, db, &model.SyncTask{}))
}

// TestBulkMirrorOrg_CreateRepoAndTask 非 dry_run：登记目标仓（clone URL 改写）+ 建同步任务。
func TestBulkMirrorOrg_CreateRepoAndTask(t *testing.T) {
	svc, db, dst := setupMirrorFixture(t)

	res, err := svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
		SourcePlatformKey: "src-plat", SourceOrg: "acme",
		TargetPlatform: dst, Strategy: OrgMapSingle, TargetOrg: "backup",
		CreateTasks: true, DryRun: false,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, res.Planned)
	assert.Equal(t, 2, res.Imported)
	assert.Equal(t, 2, res.TasksCreated)
	require.Len(t, res.Items, 2)
	for _, it := range res.Items {
		assert.Equal(t, "task_created", it.Action, "item=%+v", it)
	}
	assert.Empty(t, res.Warnings)

	// 目标仓落在目标平台，clone URL 按 owner/repo 改写（host 沿用源 clone URL）
	var dstRepos []*model.Repo
	require.NoError(t, db.Where("platform_id = ?", dst.ID).Find(&dstRepos).Error)
	require.Len(t, dstRepos, 2)
	names := map[string]string{}
	for _, r := range dstRepos {
		names[r.CloneURL] = r.Name
	}
	assert.Equal(t, "backup/app", names["https://src.example.com/backup/app.git"])
	assert.Equal(t, "backup/api", names["git@src.example.com:backup/api.git"])

	// 任务：源 key 沿用源仓记录，目标 key 是新建目标仓记录的 key
	var tasks []*model.SyncTask
	require.NoError(t, db.Find(&tasks).Error)
	require.Len(t, tasks, 2)
	for _, task := range tasks {
		assert.Contains(t, []string{"m-app", "m-api"}, task.SourceRepoKey)
		assert.True(t, task.Enabled)
		assert.Equal(t, "*", task.SourceBranch)
		assert.Equal(t, "*", task.TargetBranch)
		assert.Equal(t, "all", task.SyncMode)
		assert.True(t, task.GitTags)
	}
}

// TestBulkMirrorOrg_MappingStrategies 落点策略表驱动（壳 org-mirror 语义）：
// preserve 恒等、single 落 target_org、flat 落 target_user、mixed 组织仓落 target_org。
func TestBulkMirrorOrg_MappingStrategies(t *testing.T) {
	cases := []struct {
		name       string
		strategy   OrgMapStrategy
		targetOrg  string
		targetUser string
		wantTarget string
	}{
		{"preserve", OrgMapPreserve, "backup", "alice", "acme/app"},
		{"single", OrgMapSingle, "backup", "", "backup/app"},
		{"single_no_org", OrgMapSingle, "", "", "acme/app"},
		{"flat", OrgMapFlat, "", "mirror", "mirror/app"},
		{"flat_no_user", OrgMapFlat, "", "", "acme/app"},
		{"mixed_org", OrgMapMixed, "backup", "alice", "backup/app"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, _, dst := setupMirrorFixture(t)
			res, err := svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
				SourcePlatformKey: "src-plat", SourceOrg: "acme",
				TargetPlatform: dst, Strategy: c.strategy,
				TargetOrg: c.targetOrg, TargetUser: c.targetUser,
				DryRun: true,
			})
			require.NoError(t, err)
			require.Len(t, res.Items, 2)
			got := map[string]string{}
			for _, it := range res.Items {
				got[it.Source] = it.Target
			}
			assert.Equal(t, c.wantTarget, got["acme/app"])
		})
	}
}

// TestBulkMirrorOrg_MixedPersonalRepo mixed 下按 IsPersonalOwner(owner, targetUser)
// 判定：owner==targetUser 视为个人仓（保持落 target_user），否则按组织仓落 target_org。
func TestBulkMirrorOrg_MixedPersonalRepo(t *testing.T) {
	svc, _, dst := setupMirrorFixture(t)

	res, err := svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
		SourcePlatformKey: "src-plat", SourceOrg: "alice",
		TargetPlatform: dst, Strategy: OrgMapMixed,
		TargetOrg: "backup", TargetUser: "alice",
		DryRun: true,
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "alice/dot", res.Items[0].Source)
	assert.Equal(t, "alice/dot", res.Items[0].Target, "owner==targetUser 判为个人仓")

	// 同一 owner 不等于 target_user 时判为组织仓 → 落 target_org
	res, err = svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
		SourcePlatformKey: "src-plat", SourceOrg: "alice",
		TargetPlatform: dst, Strategy: OrgMapMixed,
		TargetOrg: "backup", TargetUser: "someoneelse",
		DryRun: true,
	})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "backup/dot", res.Items[0].Target, "mixed 组织仓落 target_org")
}

// TestBulkMirrorOrg_ImportNewToleratesFailure import_new 失败不阻断后续编排（历史行为）。
func TestBulkMirrorOrg_ImportNewToleratesFailure(t *testing.T) {
	svc, db := setupOrgBulkDB(t)
	// 未知平台类型 → provider 构造即失败，SyncPlatformReposFiltered 返回错误但被忽略
	src := mustCreatePlatform(t, db, &model.Platform{
		Key: "src-bogus", Name: "BOGUS", Type: "bogus-platform", APIURL: "https://bogus.example.com/api",
	})
	dst := mustCreatePlatform(t, db, &model.Platform{Key: "dst-bogus", Name: "D", Type: "gitlab"})
	seedMirrorRepos(t, db, src.ID)

	res, err := svc.BulkMirrorOrg(context.Background(), BulkMirrorRequest{
		SourcePlatformKey: "src-bogus", SourceOrg: "acme",
		TargetPlatform: dst, Strategy: OrgMapSingle, TargetOrg: "backup",
		ImportNew: true, DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Planned, "import_new 失败不影响过滤与统计")
}
