package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"
)

// ===== 测试基建 =====

// newMetadataServiceFixture 构造带 sqlite DAO 的 Service（BackupMetadata /
// RestoreMetadata 的 DB 查询路径用）；backupDir 直接注入 Sync 配置。
func newMetadataServiceFixture(t *testing.T, backupDir string) (*Service, *gorm.DB) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err, "open test db")
	require.NoError(t, db.AutoMigrate(&model.Repo{}, &model.Platform{}), "migrate")

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	mgr := sdkprov.NewManager(0)

	svc := &Service{
		config:    &Config{Sync: model.SyncConfig{BackupDir: backupDir}},
		repos:     NewRepoService(repoDAO, platformDAO, mgr),
		platforms: NewPlatformService(platformDAO, repoDAO, mgr),
	}
	return svc, db
}

// fakeEngineProv 覆盖元数据备份/回灌用到的全部可选能力；嵌入 nil Provider，
// 未覆写的核心方法被触达时立刻 panic（测试即失败）。
type fakeEngineProv struct {
	sdkprov.Provider
	caps sdkprov.CapabilitySet

	// 备份采集侧
	labels        []*sdkprov.IssueLabel
	labelsErr     error
	milestones    []sdkprov.Milestone
	milestonesErr error
	issues        []*sdkprov.Issue
	issuesErr     error
	comments      map[string][]*sdkprov.IssueComment
	prs           []*sdkprov.ChangeRequest
	prsErr        error
	releases      []*sdkprov.ReleaseInfo
	releasesErr   error
	archives      map[string][]byte
	archiveErrs   map[string]error
	gistPages     map[int][]*sdkprov.Gist
	gistCalls     []int
	assetData     map[int64][]byte
	assetErrs     map[int64]error
	assetCalls    []int64

	// 回灌侧（existing = 目标仓已有，复用上面同名字段；created* = 本次写入记录）
	existLabels      []*sdkprov.Label
	createdLabels    []string
	updatedLabels    []string
	createdMilestone []string
	createdIssues    []sdkprov.CreateIssueOptions
	createdComments  []string
	closedIssues     []string
	createdRelease   []sdkprov.CreateReleaseOptions
	labelErrs        map[string]error
	milestoneErrs    map[string]error
	issueErrs        map[string]error
	releaseErrs      map[string]error
}

func (f *fakeEngineProv) Capabilities() sdkprov.CapabilitySet { return f.caps }

// --- IssueManager ---

func (f *fakeEngineProv) ListIssues(_ context.Context, opts sdkprov.ListIssuesOptions) ([]*sdkprov.Issue, int, error) {
	if f.issuesErr != nil {
		return nil, 0, f.issuesErr
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}
	pp := opts.PerPage
	if pp <= 0 {
		pp = 30
	}
	start := (opts.Page - 1) * pp
	if start >= len(f.issues) {
		return nil, len(f.issues), nil
	}
	end := min(start+pp, len(f.issues))
	return f.issues[start:end], len(f.issues), nil
}

func (f *fakeEngineProv) ListIssueComments(_ context.Context, _, _, number string) ([]*sdkprov.IssueComment, error) {
	return f.comments[number], nil
}

func (f *fakeEngineProv) ListIssueLabels(_ context.Context, _, _ string) ([]*sdkprov.IssueLabel, error) {
	if f.labelsErr != nil {
		return nil, f.labelsErr
	}
	return f.labels, nil
}

func (f *fakeEngineProv) CreateIssue(_ context.Context, opts sdkprov.CreateIssueOptions) (*sdkprov.Issue, error) {
	if err := f.issueErrs[opts.Title]; err != nil {
		return nil, err
	}
	f.createdIssues = append(f.createdIssues, opts)
	return &sdkprov.Issue{Number: strconv.Itoa(len(f.createdIssues) + 100), Title: opts.Title}, nil
}

func (f *fakeEngineProv) CreateIssueComment(_ context.Context, _, _, _, body string) (*sdkprov.IssueComment, error) {
	f.createdComments = append(f.createdComments, body)
	return &sdkprov.IssueComment{Body: body}, nil
}

func (f *fakeEngineProv) CloseIssue(_ context.Context, _, _, number string) (*sdkprov.Issue, error) {
	f.closedIssues = append(f.closedIssues, number)
	return &sdkprov.Issue{Number: number, State: "closed"}, nil
}

func (f *fakeEngineProv) GetIssue(context.Context, string, string, string) (*sdkprov.Issue, error) {
	panic("fakeEngineProv: GetIssue not expected")
}
func (f *fakeEngineProv) UpdateIssue(context.Context, string, string, string, sdkprov.UpdateIssueOptions) (*sdkprov.Issue, error) {
	panic("fakeEngineProv: UpdateIssue not expected")
}
func (f *fakeEngineProv) ReopenIssue(context.Context, string, string, string) (*sdkprov.Issue, error) {
	panic("fakeEngineProv: ReopenIssue not expected")
}
func (f *fakeEngineProv) UpdateIssueComment(context.Context, string, string, string, int64, string) (*sdkprov.IssueComment, error) {
	panic("fakeEngineProv: UpdateIssueComment not expected")
}
func (f *fakeEngineProv) AddIssueLabels(context.Context, string, string, string, []string) error {
	panic("fakeEngineProv: AddIssueLabels not expected")
}
func (f *fakeEngineProv) RemoveIssueLabel(context.Context, string, string, string, string) error {
	panic("fakeEngineProv: RemoveIssueLabel not expected")
}

// --- MilestoneManager ---

func (f *fakeEngineProv) ListMilestones(_ context.Context, _, _ string, _ sdkprov.ListMilestonesOptions) ([]sdkprov.Milestone, error) {
	if f.milestonesErr != nil {
		return nil, f.milestonesErr
	}
	return f.milestones, nil
}

func (f *fakeEngineProv) CreateMilestone(_ context.Context, _, _ string, opts sdkprov.CreateMilestoneOptions) (*sdkprov.Milestone, error) {
	if err := f.milestoneErrs[opts.Title]; err != nil {
		return nil, err
	}
	f.createdMilestone = append(f.createdMilestone, opts.Title)
	return &sdkprov.Milestone{Title: opts.Title}, nil
}

func (f *fakeEngineProv) GetMilestone(context.Context, string, string, string) (*sdkprov.Milestone, error) {
	panic("fakeEngineProv: GetMilestone not expected")
}
func (f *fakeEngineProv) UpdateMilestone(context.Context, string, string, string, sdkprov.UpdateMilestoneOptions) (*sdkprov.Milestone, error) {
	panic("fakeEngineProv: UpdateMilestone not expected")
}
func (f *fakeEngineProv) DeleteMilestone(context.Context, string, string, string) error {
	panic("fakeEngineProv: DeleteMilestone not expected")
}

// --- ChangeRequestManager ---

func (f *fakeEngineProv) ListCRs(_ context.Context, _ sdkprov.ListCROptions) ([]*sdkprov.ChangeRequest, int, error) {
	if f.prsErr != nil {
		return nil, 0, f.prsErr
	}
	return f.prs, len(f.prs), nil
}

func (f *fakeEngineProv) CreateCR(context.Context, sdkprov.CreateCROptions) (*sdkprov.ChangeRequest, error) {
	panic("fakeEngineProv: CreateCR not expected")
}
func (f *fakeEngineProv) GetCR(context.Context, string, string, string) (*sdkprov.ChangeRequest, error) {
	panic("fakeEngineProv: GetCR not expected")
}
func (f *fakeEngineProv) MergeCR(context.Context, string, string, string, sdkprov.MergeCROptions) (*sdkprov.ChangeRequest, error) {
	panic("fakeEngineProv: MergeCR not expected")
}
func (f *fakeEngineProv) CloseCR(context.Context, string, string, string) (*sdkprov.ChangeRequest, error) {
	panic("fakeEngineProv: CloseCR not expected")
}
func (f *fakeEngineProv) ReopenCR(context.Context, string, string, string) (*sdkprov.ChangeRequest, error) {
	panic("fakeEngineProv: ReopenCR not expected")
}
func (f *fakeEngineProv) UpdateCR(context.Context, string, string, string, sdkprov.UpdateCROptions) (*sdkprov.ChangeRequest, error) {
	panic("fakeEngineProv: UpdateCR not expected")
}
func (f *fakeEngineProv) UpdateCRLabels(context.Context, string, string, string, []string) error {
	panic("fakeEngineProv: UpdateCRLabels not expected")
}

// --- ReleaseManager / ReleaseAssetManager ---

func (f *fakeEngineProv) ListReleases(_ context.Context, _, _ string) ([]*sdkprov.ReleaseInfo, error) {
	if f.releasesErr != nil {
		return nil, f.releasesErr
	}
	return f.releases, nil
}

func (f *fakeEngineProv) GetArchive(_ context.Context, _, _, ref, _ string) ([]byte, error) {
	if err := f.archiveErrs[ref]; err != nil {
		return nil, err
	}
	return f.archives[ref], nil
}

func (f *fakeEngineProv) CreateRelease(_ context.Context, _, _ string, opts sdkprov.CreateReleaseOptions) (*sdkprov.ReleaseInfo, error) {
	if err := f.releaseErrs[opts.TagName]; err != nil {
		return nil, err
	}
	f.createdRelease = append(f.createdRelease, opts)
	return &sdkprov.ReleaseInfo{TagName: opts.TagName}, nil
}

func (f *fakeEngineProv) DownloadReleaseAsset(_ context.Context, _, _ string, assetID int64, w io.Writer) error {
	if err := f.assetErrs[assetID]; err != nil {
		return err
	}
	f.assetCalls = append(f.assetCalls, assetID)
	_, err := w.Write(f.assetData[assetID])
	return err
}

func (f *fakeEngineProv) ListTags(context.Context, string, string) ([]*sdkprov.TagInfo, error) {
	panic("fakeEngineProv: ListTags not expected")
}
func (f *fakeEngineProv) GetReleaseByTag(context.Context, string, string, string) (*sdkprov.ReleaseInfo, error) {
	panic("fakeEngineProv: GetReleaseByTag not expected")
}
func (f *fakeEngineProv) UpdateRelease(context.Context, string, string, string, sdkprov.UpdateReleaseOptions) (*sdkprov.ReleaseInfo, error) {
	panic("fakeEngineProv: UpdateRelease not expected")
}
func (f *fakeEngineProv) DeleteRelease(context.Context, string, string, string) error {
	panic("fakeEngineProv: DeleteRelease not expected")
}

// --- GistManager ---

func (f *fakeEngineProv) ListMyGists(_ context.Context, page, _ int) ([]*sdkprov.Gist, error) {
	f.gistCalls = append(f.gistCalls, page)
	return f.gistPages[page], nil
}

// --- LabelManager ---

func (f *fakeEngineProv) ListLabels(_ context.Context, _, _ string, _ sdkprov.ListLabelsOptions) ([]*sdkprov.Label, error) {
	return f.existLabels, nil
}

func (f *fakeEngineProv) CreateLabel(_ context.Context, _, _ string, opts sdkprov.CreateLabelOptions) (*sdkprov.Label, error) {
	if err := f.labelErrs[opts.Name]; err != nil {
		return nil, err
	}
	f.createdLabels = append(f.createdLabels, opts.Name)
	return &sdkprov.Label{Name: opts.Name, Color: opts.Color}, nil
}

func (f *fakeEngineProv) UpdateLabel(_ context.Context, _, _, name string, _ sdkprov.UpdateLabelOptions) (*sdkprov.Label, error) {
	if err := f.labelErrs[name]; err != nil {
		return nil, err
	}
	f.updatedLabels = append(f.updatedLabels, name)
	return &sdkprov.Label{Name: name}, nil
}

func (f *fakeEngineProv) DeleteLabel(context.Context, string, string, string) error {
	panic("fakeEngineProv: DeleteLabel not expected")
}

// writeSnapshotManifest 在 dir 下写一份 manifest.json（fixture）。
func writeSnapshotManifest(t *testing.T, dir, repoKey string, files []string, counts map[string]int32) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o750))
	snap := MetadataSnapshot{
		RepoKey: repoKey, Platform: "github", Owner: "o", Repo: "r",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Dir: dir,
		Counts: counts, Files: files,
		Archives: []string{}, Assets: []string{}, Warnings: []string{},
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600))
}

// ===== 目录布局 / 列表 =====

func TestMetadataBackupDir(t *testing.T) {
	svc := &Service{config: &Config{Sync: model.SyncConfig{BackupDir: "/data/backup"}}}
	assert.Equal(t, filepath.Join("/data/backup", "metadata"), svc.MetadataBackupDir())
}

func TestClampMaxItems(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 500}, {0, 500}, {1, 1}, {500, 500}, {2000, 2000}, {2001, 2000},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, clampMaxItems(c.in), "clampMaxItems(%d)", c.in)
	}
}

func TestListMetadataBackups(t *testing.T) {
	root := t.TempDir()
	svc := &Service{config: &Config{Sync: model.SyncConfig{BackupDir: root}}}
	writeSnapshotManifest(t, filepath.Join(svc.MetadataBackupDir(), "r1", "20260101-000000"), "r1",
		[]string{"labels.json"}, map[string]int32{"labels": 2})
	writeSnapshotManifest(t, filepath.Join(svc.MetadataBackupDir(), "r1", "20260102-120000"), "r1",
		[]string{"issues.json"}, map[string]int32{"issues": 3})
	writeSnapshotManifest(t, filepath.Join(svc.MetadataBackupDir(), "r2", "20260101-000000"), "r2",
		[]string{}, map[string]int32{})
	// 坏 manifest：静默跳过
	bad := filepath.Join(svc.MetadataBackupDir(), "r3", "20260101-000000")
	require.NoError(t, os.MkdirAll(bad, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(bad, "manifest.json"), []byte("{"), 0o600))

	all, err := svc.ListMetadataBackups(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, all, 3)

	filtered, err := svc.ListMetadataBackups(t.Context(), "r1")
	require.NoError(t, err)
	require.Len(t, filtered, 2)
	for _, it := range filtered {
		assert.Equal(t, "r1", it.RepoKey)
	}

	none, err := svc.ListMetadataBackups(t.Context(), "nope")
	require.NoError(t, err)
	assert.Empty(t, none)
	assert.NotNil(t, none, "空结果必须是非 nil 切片（响应 JSON 为 []）")
}

func TestListMetadataBackups_EmptyBackupDir(t *testing.T) {
	svc := &Service{config: &Config{}}
	items, err := svc.ListMetadataBackups(t.Context(), "")
	require.NoError(t, err)
	assert.Empty(t, items)
}

// ===== 抽样校验 =====

func TestSampleMetadataVerify_NoBackupDir(t *testing.T) {
	svc := &Service{config: &Config{}}
	rep, err := svc.SampleMetadataVerify(t.Context(), "", 5)
	require.NoError(t, err)
	assert.False(t, rep.OK)
	assert.Equal(t, "backup_dir not configured", rep.Reason)
	assert.Zero(t, rep.Checked)
}

func TestSampleMetadataVerify_OKAndMissingIssues(t *testing.T) {
	root := t.TempDir()
	svc := &Service{config: &Config{Sync: model.SyncConfig{BackupDir: root}}}

	good := filepath.Join(svc.MetadataBackupDir(), "r1", "20260101-000000")
	writeSnapshotManifest(t, good, "r1", []string{"issues.json"}, map[string]int32{"issues": 1})
	require.NoError(t, os.WriteFile(filepath.Join(good, "issues.json"), []byte("[]"), 0o600))

	bad := filepath.Join(svc.MetadataBackupDir(), "r1", "20260102-000000")
	writeSnapshotManifest(t, bad, "r1", []string{}, map[string]int32{"issues": 5})

	rep, err := svc.SampleMetadataVerify(t.Context(), "", 5)
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Checked)
	assert.False(t, rep.OK)
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "issues.json missing")
	assert.Empty(t, rep.Reason)
}

func TestSampleMetadataVerify_CapAndFilter(t *testing.T) {
	root := t.TempDir()
	svc := &Service{config: &Config{Sync: model.SyncConfig{BackupDir: root}}}
	for _, key := range []string{"r1", "r2"} {
		for _, ts := range []string{"20260101-000000", "20260102-000000"} {
			writeSnapshotManifest(t, filepath.Join(svc.MetadataBackupDir(), key, ts), key,
				[]string{}, map[string]int32{})
		}
	}

	// n 上限
	rep, err := svc.SampleMetadataVerify(t.Context(), "", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Checked)
	assert.True(t, rep.OK)

	// repoKey 过滤（目录布局 metadata/<sanitized key>/）
	rep2, err := svc.SampleMetadataVerify(t.Context(), "r1", 5)
	require.NoError(t, err)
	assert.Equal(t, 2, rep2.Checked)
	assert.True(t, rep2.OK)
}

// ===== BackupMetadata 入口错误路径 =====

func TestBackupMetadata_RepoNotFound(t *testing.T) {
	svc, _ := newMetadataServiceFixture(t, t.TempDir())
	_, err := svc.BackupMetadata(t.Context(), MetadataBackupOptions{RepoKey: "nope"})
	require.ErrorIs(t, err, ErrRepoNotFound)
	assert.Equal(t, ErrorClassNotFound, Classify(err))
	assert.Equal(t, "repo not found", err.Error())
}

func TestBackupMetadata_BackupDirNotConfigured(t *testing.T) {
	svc, db := newMetadataServiceFixture(t, "")
	plat := &model.Platform{Key: "gh", Name: "GitHub", Type: model.PlatformTypeGitHub, APIURL: "https://api.github.com"}
	require.NoError(t, db.Create(plat).Error)
	repo := &model.Repo{
		Key: "gh/acme/app", Name: "app", PlatformID: plat.ID, Platform: model.PlatformTypeGitHub,
		PlatformOwner: "acme", PlatformRepo: "app", AccessToken: "tok", Status: model.RepoStatusActive,
	}
	require.NoError(t, db.Create(repo).Error)

	_, err := svc.BackupMetadata(t.Context(), MetadataBackupOptions{RepoKey: "gh/acme/app"})
	require.Error(t, err)
	assert.Equal(t, "sync.backup_dir not configured", err.Error())
	assert.Equal(t, ErrorClassValidation, Classify(err))
}

func TestBackupMetadata_PlatformNotFound(t *testing.T) {
	svc, db := newMetadataServiceFixture(t, t.TempDir())
	repo := &model.Repo{
		Key: "orphan/app", Name: "app", PlatformID: 99999, Platform: model.PlatformTypeGitHub,
		PlatformOwner: "o", PlatformRepo: "app", Status: model.RepoStatusActive,
	}
	require.NoError(t, db.Create(repo).Error)

	_, err := svc.BackupMetadata(t.Context(), MetadataBackupOptions{RepoKey: "orphan/app"})
	require.ErrorIs(t, err, ErrPlatformNotFound)
	assert.Equal(t, ErrorClassNotFound, Classify(err))
}

// ===== 采集引擎（桩 provider，无网络） =====

func TestCollectMetadataSnapshot_Full(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{config: &Config{Sync: model.SyncConfig{BackupDir: dir}}}
	repo := &model.Repo{PlatformOwner: "acme", PlatformRepo: "app", Platform: model.PlatformTypeGitHub}
	plat := &model.Platform{Type: model.PlatformTypeGitHub}
	prov := &fakeEngineProv{
		caps:       sdkprov.CapabilitySet{Gists: true, ReleaseAssets: true},
		labels:     []*sdkprov.IssueLabel{{Name: "bug", Color: "f00"}},
		milestones: []sdkprov.Milestone{{Title: "v1"}},
		issues:     []*sdkprov.Issue{{Number: "1", Title: "i1", State: "open"}},
		comments:   map[string][]*sdkprov.IssueComment{"1": {{Body: "hi"}}},
		prs:        []*sdkprov.ChangeRequest{{Number: "7", Title: "p1", SourceBranch: "feat", TargetBranch: "main"}},
		releases:   []*sdkprov.ReleaseInfo{{TagName: "rel/../v1", Assets: []*sdkprov.ReleaseAsset{{ID: 7, Name: "a/b.bin"}}}},
		archives:   map[string][]byte{"rel/../v1": []byte("arch")},
		gistPages:  map[int][]*sdkprov.Gist{1: {{ID: "evil/../id"}}},
		assetData:  map[int64][]byte{7: []byte("payload")},
	}

	res, err := svc.collectMetadataSnapshot(t.Context(), MetadataBackupOptions{
		RepoKey:         "gh/acme/app",
		IncludeIssues:   true,
		IncludePRs:      true,
		IncludeReleases: true,
		IncludeSource:   true,
		IncludeAssets:   true,
		IncludeGists:    true,
	}, repo, plat, prov)
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	assert.False(t, res.Truncated)

	// 计数（键名与历史 manifest 一致）
	for k, want := range map[string]int32{
		"labels": 1, "milestones": 1, "issues": 1, "pull_requests": 1,
		"releases": 1, "archives": 1, "release_assets": 1, "gists": 1,
	} {
		assert.Equal(t, want, res.Counts[k], "count[%s]", k)
	}

	// 目录布局：BackupDir/metadata/<sanitized repoKey>/<20060102-150405>
	wantPrefix := filepath.Join(svc.MetadataBackupDir(), "gh_acme_app") + string(filepath.Separator)
	assert.True(t, strings.HasPrefix(res.Dir, wantPrefix), "dir=%s", res.Dir)
	assert.Regexp(t, `^\d{8}-\d{6}$`, filepath.Base(res.Dir))

	// 分片登记：manifest.json 在响应 Files 里，但不进落盘 manifest 的 files
	require.Equal(t, []string{
		"labels.json", "milestones.json", "issues.json",
		"pull_requests.json", "releases.json", "manifest.json",
	}, res.Files)

	data, err := os.ReadFile(filepath.Join(res.Dir, "manifest.json"))
	require.NoError(t, err)
	var disk MetadataSnapshot
	require.NoError(t, json.Unmarshal(data, &disk))
	assert.Equal(t, "gh/acme/app", disk.RepoKey)
	assert.NotContains(t, disk.Files, "manifest.json", "落盘 manifest 的 files 不含自身")
	assert.Len(t, disk.Files, 5)

	// issues 分片带评论
	var issues []*issueRow
	require.NoError(t, json.Unmarshal(mustRead(t, filepath.Join(res.Dir, "issues.json")), &issues))
	require.Len(t, issues, 1)
	require.Len(t, issues[0].CommentList, 1)
	assert.Equal(t, "hi", issues[0].CommentList[0].Body)

	// 归档 / 附件 / gists：文件名全部经 SanitizePathToken
	assert.Equal(t, []string{"rel___v1.tar.gz"}, res.Archives)
	assert.Equal(t, []byte("arch"), mustRead(t, filepath.Join(res.Dir, "archives", "rel___v1.tar.gz")))
	assert.Equal(t, []string{"rel___v1__a_b.bin"}, res.Assets)
	assert.Equal(t, []byte("payload"), mustRead(t, filepath.Join(res.Dir, "release-assets", "rel___v1__a_b.bin")))
	assert.FileExists(t, filepath.Join(res.Dir, "gists", "evil___id.json"))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // 测试内部路径
	require.NoError(t, err)
	return data
}

func TestCollectMetadataSnapshot_WarningsAndTruncate(t *testing.T) {
	dir := t.TempDir()
	svc := &Service{config: &Config{Sync: model.SyncConfig{BackupDir: dir}}}
	repo := &model.Repo{PlatformOwner: "acme", PlatformRepo: "app", Platform: model.PlatformTypeGitHub}
	plat := &model.Platform{Type: model.PlatformTypeGitHub}
	prov := &fakeEngineProv{
		labels:    []*sdkprov.IssueLabel{{Name: "bug", Color: "f00"}},
		labelsErr: errors.New("boom"),
		prs: []*sdkprov.ChangeRequest{
			{Number: "1", Title: "p1"}, {Number: "2", Title: "p2"},
		},
		archiveErrs: map[string]error{"v9": errors.New("404")},
		releases:    []*sdkprov.ReleaseInfo{{TagName: "v9"}},
	}

	res, err := svc.collectMetadataSnapshot(t.Context(), MetadataBackupOptions{
		RepoKey:         "r1",
		MaxPRs:          1,
		IncludePRs:      true,
		IncludeReleases: true,
		IncludeSource:   true,
	}, repo, plat, prov)
	require.NoError(t, err)

	// warning 文案逐字保留
	require.Len(t, res.Warnings, 2)
	assert.Equal(t, "labels: boom", res.Warnings[0])
	assert.Equal(t, "archive v9: 404", res.Warnings[1])
	_, hasLabels := res.Counts["labels"]
	assert.False(t, hasLabels, "labels 失败时不写计数")

	// PR 截断语义：> MaxPRs 才截，且 Truncated 标记
	assert.Equal(t, int32(1), res.Counts["pull_requests"])
	assert.True(t, res.Truncated)

	// 能力门控：无 Gists/ReleaseAssets 能力时即使 Include* 打开也不调用、无 warning
	prov2 := &fakeEngineProv{releases: []*sdkprov.ReleaseInfo{{TagName: "v1", Assets: []*sdkprov.ReleaseAsset{{ID: 1, Name: "a"}}}}}
	res2, err := svc.collectMetadataSnapshot(t.Context(), MetadataBackupOptions{
		RepoKey:       "r2",
		IncludeAssets: true,
		IncludeGists:  true,
	}, repo, plat, prov2)
	require.NoError(t, err)
	assert.Empty(t, res2.Warnings)
	assert.Empty(t, res2.Assets)
	_, hasGists := res2.Counts["gists"]
	assert.False(t, hasGists)
	assert.Empty(t, prov2.assetCalls)
}

func TestListIssues_CapsAtMax(t *testing.T) {
	issues := make([]*sdkprov.Issue, 0, 55)
	for i := 0; i < 55; i++ {
		issues = append(issues, &sdkprov.Issue{Number: strconv.Itoa(i), Title: "t" + strconv.Itoa(i)})
	}
	prov := &fakeEngineProv{issues: issues}
	repo := &model.Repo{PlatformOwner: "o", PlatformRepo: "r", Platform: model.PlatformTypeGitHub}

	all, err := listIssues(t.Context(), prov, repo, "all", 100)
	require.NoError(t, err)
	assert.Len(t, all, 55)

	capped, err := listIssues(t.Context(), prov, repo, "all", 52)
	require.NoError(t, err)
	assert.Len(t, capped, 52, "到 max 必须提前收束")
	assert.Equal(t, "t0", capped[0].Title)
}

func TestFilterIssuesSince(t *testing.T) {
	issues := []*issueRow{
		{Title: "old", UpdatedAt: "2026-01-01T00:00:00Z"},
		{Title: "new", UpdatedAt: "2026-06-01T00:00:00Z"},
	}
	got := filterIssuesSince(issues, "2026-03-01T00:00:00Z")
	if len(got) != 1 || got[0].Title != "new" {
		t.Fatalf("got %+v", got)
	}
	// invalid since → 原样返回
	if len(filterIssuesSince(issues, "bad")) != 2 {
		t.Fatal("invalid since should pass through")
	}
}
