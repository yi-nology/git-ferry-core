package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// writeRestoreFixture 在 dir 写一份含全部分片的快照（labels×2 / milestones×1 /
// issues×3（含评论与 closed）/ prs×1 / releases×1）。
func writeRestoreFixture(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o750))
	files := []string{"labels.json", "milestones.json", "issues.json", "pull_requests.json", "releases.json"}
	counts := map[string]int32{"labels": 2, "milestones": 1, "issues": 3, "pull_requests": 1, "releases": 1}
	writeSnapshotManifest(t, dir, "r1", files, counts)

	parts := map[string]string{
		"labels.json":     `[{"name":"bug","color":"f00"},{"name":"feat","color":"0f0"}]`,
		"milestones.json": `[{"number":"1","title":"v1","description":"d","state":"open"}]`,
		"issues.json": `[{"number":"1","title":"i1","state":"open","author":"alice","labels":["bug"],` +
			`"body":"b1","web_url":"https://x/1","created_at":"2026-01-01T00:00:00Z",` +
			`"updated_at":"2026-01-02T00:00:00Z","comments":[{"body":"c1"}]},` +
			`{"number":"2","title":"i2","state":"closed","body":"b2",` +
			`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"},` +
			`{"number":"3","title":"i3","state":"open","body":"b3",` +
			`"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}]`,
		"pull_requests.json": `[{"number":"7","title":"p1","source_branch":"feat","target_branch":"main","description":"desc"}]`,
		"releases.json":      `[{"tag_name":"v1","title":"R1","body":"rb","draft":false,"prerelease":false}]`,
	}
	for name, body := range parts {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
}

func newRestoreTestService(backupDir string) *Service {
	return &Service{config: &Config{Sync: model.SyncConfig{BackupDir: backupDir}}}
}

// ===== 迁自壳层的纯逻辑断言 =====

func TestNormalizeRestoreKinds_EmptyUsesAvailable(t *testing.T) {
	snap := &MetadataSnapshot{Files: []string{"labels.json", "issues.json", "releases.json"}}
	got := normalizeRestoreKinds(nil, snap)
	if len(got) != 3 {
		t.Fatalf("got %v want 3 kinds", got)
	}
	want := map[string]bool{"labels": true, "issues": true, "releases": true}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("unexpected kind %s", k)
		}
	}
}

func TestNormalizeRestoreKinds_Explicit(t *testing.T) {
	snap := &MetadataSnapshot{Files: []string{"labels.json", "issues.json", "pull_requests.json"}}
	got := normalizeRestoreKinds([]string{"Labels", " prs ", ""}, snap)
	if len(got) != 2 || got[0] != "labels" || got[1] != "prs" {
		t.Fatalf("got %v", got)
	}
}

func TestLatestMetadataSnapshot_PicksNewest(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "metadata", "r1")
	for _, d := range []string{"20260101-000000", "20260102-120000", "20260102-080000"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	got, err := latestMetadataSnapshot(root, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "20260102-120000" {
		t.Fatalf("got %s", got)
	}
}

func TestLatestMetadataSnapshot_Missing(t *testing.T) {
	if _, err := latestMetadataSnapshot(t.TempDir(), "nope"); err == nil {
		t.Fatal("want error")
	}
}

func TestLoadMetadataSnapshot(t *testing.T) {
	dir := t.TempDir()
	body := `{"repo_key":"r1","platform":"github","owner":"o","repo":"r","counts":{"labels":1},"files":["labels.json"]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := loadMetadataSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RepoKey != "r1" || snap.Counts["labels"] != 1 {
		t.Fatalf("snap=%+v", snap)
	}
}

func TestIssueBodyHasRestoreMarker(t *testing.T) {
	b := issueBody(&issueRow{Number: "12", Title: "t", Body: "hello", Author: "alice", WebURL: "https://x/1"})
	for _, want := range []string{"gitferry-restore:issue", "hello", "alice", "https://x/1"} {
		if !strings.Contains(b, want) {
			t.Fatalf("body missing %q: %s", want, b)
		}
	}
}

func TestPRBodyHasRestoreMarker(t *testing.T) {
	b := prBody(&sdkprov.ChangeRequest{
		Number: "3", SourceBranch: "feat", TargetBranch: "main", Description: "desc",
	})
	for _, want := range []string{"gitferry-restore:pr", "feat", "main", "desc"} {
		if !strings.Contains(b, want) {
			t.Fatalf("body missing %q: %s", want, b)
		}
	}
}

// ===== RestoreMetadata 入口错误路径 =====

func TestRestoreMetadata_BackupDirNotConfigured(t *testing.T) {
	svc := newRestoreTestService("")
	_, err := svc.RestoreMetadata(t.Context(), RestoreRequest{RepoKey: "r1"})
	require.Error(t, err)
	assert.Equal(t, "sync.backup_dir not configured", err.Error())
	assert.Equal(t, ErrorClassValidation, Classify(err))
}

func TestRestoreMetadata_NoSnapshot(t *testing.T) {
	svc := newRestoreTestService(t.TempDir())
	_, err := svc.RestoreMetadata(t.Context(), RestoreRequest{RepoKey: "nope"})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "no snapshot for nope"), "got %q", err)
	assert.Equal(t, ErrorClassValidation, Classify(err))
}

func TestRestoreMetadata_LoadSnapshotFail(t *testing.T) {
	svc := newRestoreTestService(t.TempDir())
	// 快照目录存在但没有 manifest.json → latest 通过、load 失败
	require.NoError(t, os.MkdirAll(filepath.Join(svc.MetadataBackupDir(), "r1", "20260101-000000"), 0o750))
	_, err := svc.RestoreMetadata(t.Context(), RestoreRequest{RepoKey: "r1"})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "load snapshot: "), "got %q", err)
	assert.Equal(t, ErrorClassValidation, Classify(err))
}

func TestRestoreMetadata_RepoNotFound(t *testing.T) {
	backupRoot := t.TempDir()
	svc, _ := newMetadataServiceFixture(t, backupRoot)
	writeRestoreFixture(t, filepath.Join(svc.MetadataBackupDir(), "r1", "20260101-000000"))

	_, err := svc.RestoreMetadata(t.Context(), RestoreRequest{RepoKey: "r1"})
	require.ErrorIs(t, err, ErrRepoNotFound)
	assert.Equal(t, ErrorClassNotFound, Classify(err))
}

// ===== 回灌分发（桩 provider） =====

func TestRunRestore_DryRunAndHistory(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	prov := &fakeEngineProv{}
	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", DryRun: true},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	assert.Equal(t, "github/acme/app", res.Target)
	assert.Equal(t, snapDir, res.SnapshotDir)
	assert.True(t, res.DryRun)

	// dry-run：全部计入 created，不触达 provider 写接口
	require.Contains(t, res.Stats, "labels")
	assert.Equal(t, RestoreKindStat{Planned: 2, Created: 2}, *res.Stats["labels"])
	assert.Equal(t, RestoreKindStat{Planned: 1, Created: 1}, *res.Stats["milestones"])
	assert.Equal(t, RestoreKindStat{Planned: 3, Created: 3}, *res.Stats["issues"])
	assert.Equal(t, RestoreKindStat{Planned: 1, Created: 1}, *res.Stats["prs"])
	assert.Equal(t, RestoreKindStat{Planned: 1, Created: 1}, *res.Stats["releases"])
	assert.Equal(t, 8, res.Applied)
	assert.Zero(t, res.Failed)
	assert.Empty(t, prov.createdLabels)
	assert.Empty(t, prov.createdIssues)
	assert.Empty(t, prov.createdRelease)

	// dry_run 提示追加在最后，文案逐字保留
	require.NotEmpty(t, res.Warnings)
	assert.Equal(t, "dry_run=true：未写入目标；去掉 dry_run 执行", res.Warnings[len(res.Warnings)-1])

	// 历史文件：结构与 ops.MetadataRestoreResult 一致，且不含 dry_run 提示与扩展字段
	histDir := filepath.Join(backupRoot, "metadata-restore", "r1")
	entries, err := os.ReadDir(histDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	var rec map[string]any
	data, err := os.ReadFile(filepath.Join(histDir, entries[0].Name()))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &rec))
	assert.ElementsMatch(t,
		[]string{"snapshot_dir", "target", "stats", "warnings", "dry_run", "started_at", "finished_at"},
		keysOf(rec), "历史文件字段集必须与 ops.MetadataRestoreResult 一致")
	assert.Equal(t, true, rec["dry_run"], "历史记录 dry_run 取实际值")
	assert.Equal(t, []any{}, rec["warnings"], "dry_run 提示不得进历史文件")
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRunRestore_CreateSkipFail(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	// run A：overwrite=false —— 同名跳过、注入失败、未知 kind
	prov := &fakeEngineProv{
		existLabels: []*sdkprov.Label{{Name: "bug"}},
		milestones:  []sdkprov.Milestone{{Title: "v1"}},
		labelErrs:   map[string]error{"feat": assert.AnError},
		issueErrs:   map[string]error{"i3": assert.AnError},
		releaseErrs: map[string]error{"v1": assert.AnError},
	}
	res := svc.runRestore(t.Context(), RestoreRequest{
		RepoKey: "r1",
		Kinds:   []string{"labels", "milestones", "issues", "prs", "releases", "bogus"},
	}, snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	assert.Equal(t, RestoreKindStat{Planned: 2, Skipped: 1, Failed: 1}, *res.Stats["labels"])
	assert.Equal(t, RestoreKindStat{Planned: 1, Skipped: 1}, *res.Stats["milestones"])
	assert.Equal(t, RestoreKindStat{Planned: 3, Created: 2, Failed: 1}, *res.Stats["issues"])
	assert.Equal(t, RestoreKindStat{Planned: 1, Created: 1}, *res.Stats["prs"])
	assert.Equal(t, RestoreKindStat{Planned: 1, Failed: 1}, *res.Stats["releases"])
	assert.Equal(t, 3, res.Applied)
	assert.Equal(t, 2, res.Skipped)
	assert.Equal(t, 3, res.Failed)

	// warning 文案逐字保留
	warns := strings.Join(res.Warnings, "|")
	assert.Contains(t, warns, "create label feat: ")
	assert.Contains(t, warns, "create issue i3: ")
	assert.Contains(t, warns, "create release v1: ")
	assert.Contains(t, warns, "unknown kind: bogus")

	// issue 侧副作用：评论落、closed 关
	assert.Equal(t, []string{"c1"}, prov.createdComments)
	assert.Equal(t, []string{"102"}, prov.closedIssues)
	require.Len(t, prov.createdIssues, 3, "i1+i2 两个 issue + p1 以 issue 形态")
	assert.Equal(t, "i1", prov.createdIssues[0].Title)
	assert.Equal(t, "i2", prov.createdIssues[1].Title)
	assert.Equal(t, []string{"bug"}, prov.createdIssues[0].Labels)

	// pr 以 issue 形态回灌，body 带来源标注
	var prOpts *sdkprov.CreateIssueOptions
	for i := range prov.createdIssues {
		if prov.createdIssues[i].Title == "p1" {
			prOpts = &prov.createdIssues[i]
		}
	}
	require.NotNil(t, prOpts, "p1 应以 issue 形态创建")
	assert.Contains(t, prOpts.Body, "gitferry-restore:pr")
	assert.Contains(t, prOpts.Body, "feat")
	assert.Empty(t, prOpts.Labels, "PR 回灌不带 labels")

	// run B：overwrite=true —— 已存在 label 走更新而非跳过
	prov2 := &fakeEngineProv{existLabels: []*sdkprov.Label{{Name: "bug"}}}
	res2 := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", Overwrite: true, Kinds: []string{"labels"}},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov2)
	assert.Equal(t, RestoreKindStat{Planned: 2, Created: 2}, *res2.Stats["labels"])
	assert.Equal(t, []string{"bug"}, prov2.updatedLabels, "overwrite=true 时同名 label 走 UpdateLabel")
	assert.Equal(t, []string{"feat"}, prov2.createdLabels)
}

func TestRunRestore_MissingPartWarning(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	// manifest 声明 labels.json 但分片缺失 → warning 前缀逐字
	writeSnapshotManifest(t, snapDir, "r1", []string{"labels.json"}, map[string]int32{"labels": 1})
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1"},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", &fakeEngineProv{})
	require.Len(t, res.Warnings, 1)
	assert.True(t, strings.HasPrefix(res.Warnings[0], "labels.json: "), "got %q", res.Warnings[0])
}
