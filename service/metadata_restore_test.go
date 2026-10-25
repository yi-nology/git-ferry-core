package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	// 「更新」必须与「新建」分开记:原来把 update 记成 created ⇒ 报告说"新建了 2 条",
	// 实际是"新建 1 条 + 覆盖 1 条" —— 报告与事实相反。
	assert.Equal(t, RestoreKindStat{Planned: 2, Created: 1, Updated: 1}, *res2.Stats["labels"])
	assert.Equal(t, []string{"bug"}, prov2.updatedLabels, "overwrite=true 时同名 label 走 UpdateLabel")
	assert.Equal(t, []string{"feat"}, prov2.createdLabels)
}

// TestRunRestore_OverwriteLabelColorHonest overwrite 覆盖 label 颜色必须**如实且可见**。
//
// 回归背景(P1-6):
//   - 覆盖走的是 UpdateLabel,却把计数记成 created ⇒ 报告与事实相反;
//   - 颜色**一致**时也照写一次 ⇒ 无意义写入(还会改动 label 的 updated_at、
//     白白触发下游 label 事件);
//   - 覆盖了目标侧的颜色(可能是人工改过的)却**一声不吭**。
func TestRunRestore_OverwriteLabelColorHonest(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	// 目标侧:bug 颜色与快照一致(f00);feat 颜色被人工改成 00f
	prov := &fakeEngineProv{existLabels: []*sdkprov.Label{
		{Name: "bug", Color: "f00"},
		{Name: "feat", Color: "00f"},
	}}
	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", Overwrite: true, Kinds: []string{"labels"}},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	// ① 计数如实:1 覆盖 + 1 跳过,没有"新建"
	st := res.Stats["labels"]
	require.NotNil(t, st)
	assert.Equal(t, 0, st.Created, "没有新建 label")
	assert.Equal(t, 1, st.Updated, "1 个 label 被覆盖更新")
	assert.Equal(t, 1, st.Skipped, "颜色一致的 label 应跳过")

	// ② 颜色一致 ⇒ 不写
	assert.Equal(t, []string{"feat"}, prov.updatedLabels, "只应覆盖颜色确实变了的 label")

	// ③ 覆盖内容留痕:明细说清动作与变化,warning 汇总点名
	byName := map[string]RestoreItem{}
	for _, d := range res.Details {
		byName[d.Name] = d
	}
	feat, ok := byName["feat"]
	require.True(t, ok)
	assert.Equal(t, "updated", feat.Action, "覆盖必须记成 updated,不能记成 created")
	assert.Contains(t, feat.Note, "00f→0f0", "明细要说清颜色从什么改成什么")
	bug, ok := byName["bug"]
	require.True(t, ok)
	assert.Equal(t, "skipped", bug.Action)
	assert.Contains(t, bug.Note, "颜色一致")

	warns := strings.Join(res.Warnings, "|")
	assert.Contains(t, warns, "overwrite 覆盖了 1 个 label 的颜色", "覆盖必须留痕")
	assert.Contains(t, warns, "feat(00f→0f0)", "warning 要点名改了哪个、怎么改的")
}

// TestRunRestore_OverwriteLabelColorDryRunVisible dry_run 必须**提前**把
// "会覆盖哪个 label 的颜色"说清楚 —— 运维要在动手前看到破坏面。
func TestRunRestore_OverwriteLabelColorDryRunVisible(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	prov := &fakeEngineProv{existLabels: []*sdkprov.Label{{Name: "feat", Color: "00f"}}}
	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", Overwrite: true, DryRun: true, Kinds: []string{"labels"}},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	assert.Empty(t, prov.updatedLabels, "dry_run 不得真的写")
	st := res.Stats["labels"]
	require.NotNil(t, st)
	assert.Equal(t, 1, st.Updated, "dry_run 也要把覆盖数出来")
	assert.Equal(t, 1, st.Created, "目标侧缺失的 bug 属于新建")

	var note string
	for _, d := range res.Details {
		if d.Name == "feat" {
			note = d.Note
			assert.Equal(t, "updated", d.Action)
		}
	}
	assert.Contains(t, note, "将覆盖", "dry_run 的明细要写明这是覆盖")
	assert.Contains(t, note, "00f→0f0")
	assert.Contains(t, strings.Join(res.Warnings, "|"), "overwrite 覆盖了 1 个 label 的颜色")
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

// numberingProv 在 fakeEngineProv 之上记录「标题 → 服务端分配的编号」。
//
// 关闭 PR 回灌出来的 issue 时,调用方拿到的是 **CreateIssue 的返回值**里的编号,
// 而 CreateIssueOptions 只是请求参数、本身没有编号字段 ⇒ 必须在 provider 侧留痕。
type numberingProv struct {
	*fakeEngineProv
	numbers map[string]string
}

func (p *numberingProv) CreateIssue(ctx context.Context, opts sdkprov.CreateIssueOptions) (*sdkprov.Issue, error) {
	iss, err := p.fakeEngineProv.CreateIssue(ctx, opts)
	if err != nil {
		return nil, err
	}
	if p.numbers == nil {
		p.numbers = map[string]string{}
	}
	p.numbers[opts.Title] = iss.Number
	return iss, nil
}

// TestRunRestore_PRStatePreserved 已合并/已关闭的 PR 回灌后对应 issue 必须是**关掉**的。
//
// 回归背景:PR 分支只调 CreateIssue、**从不看 state** ⇒ 早已合并的 PR 回灌成
// 一个"开着的 issue",看着像一条待处理的请求。
// (issues 分支本来就处理了 closed 状态。)
func TestRunRestore_PRStatePreserved(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	// 把 PR 改成已合并
	prJSON := `[{"number":"7","title":"p1","state":"merged","source_branch":"feat","target_branch":"main","description":"desc"}]`
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "pull_requests.json"), []byte(prJSON), 0o600))
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	prov := &numberingProv{fakeEngineProv: &fakeEngineProv{}}
	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", DryRun: false},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	// 3 个 issue + 1 个 PR-as-issue
	require.Len(t, prov.createdIssues, 4)
	prNumber := prov.numbers["p1"]
	require.NotEmpty(t, prNumber, "PR 应回灌成一个 issue")
	// 夹具里本来就有一个 closed issue(i2)⇒ 关闭列表里应有 2 个,
	// 其一必须是 PR 回灌出来的那个。
	require.GreaterOrEqual(t, len(prov.closedIssues), 2)
	assert.Contains(t, prov.closedIssues, prNumber,
		"已合并的 PR 回灌出的 issue 必须被关掉(实际关闭: %v)", prov.closedIssues)
	assert.Zero(t, res.Failed)
}

// TestRunRestore_OverwriteUnsupportedWarns overwrite 对不支持覆盖的类型
// 必须**告警**,不能静默跳过。
//
// 回归背景:overwrite 只对 labels 生效,milestones/issues/releases 命中同名一律跳过
// 且零告警 ⇒ 调用方以为覆盖了、实际没覆盖(从调用方视角就是数据丢失)。
func TestRunRestore_OverwriteUnsupportedWarns(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	// 目标侧已有一个同名 milestone
	prov := &fakeEngineProv{
		milestones: []sdkprov.Milestone{{Number: "1", Title: "v1", State: "open"}},
	}
	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", DryRun: false, Overwrite: true},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	require.NotEmpty(t, res.Warnings, "overwrite 不支持该类型时必须告警")
	joined := ""
	for _, w := range res.Warnings {
		joined += w + "\n"
	}
	assert.Contains(t, joined, "overwrite", "告警要说清是 overwrite 不支持该类型")
	assert.Contains(t, joined, "milestones", "告警要点名具体类型")
}

// TestRunRestore_DedupSeesAllPages 去重表必须**翻页拉全量**。
//
// 回归背景(P1):去重只取第 1 页(100 条)⇒ 目标仓 issue 超过 100 条后,
// 第二页以后的同名 issue 不在去重表里 ⇒ 灾备演练**重跑会重复创建**同名 issue
// (幂等性被破坏,DR 重演变脏数据);overwrite=false 也拦不住(同名检查根本没看到)。
func TestRunRestore_DedupSeesAllPages(t *testing.T) {
	backupRoot := t.TempDir()
	svc := newRestoreTestService(backupRoot)
	snapDir := t.TempDir()
	writeRestoreFixture(t, snapDir)
	// 快照里只有一条 issue,标题与目标侧**第 2 页**的一条同名
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "issues.json"),
		[]byte(`[{"number":"120","title":"target-issue-120","state":"open","body":"b"}]`), 0o600))
	snap, err := loadMetadataSnapshot(snapDir)
	require.NoError(t, err)

	// 目标侧:150 条 issue,第 120 条标题与快照同名(在第 2 页)
	issues := make([]*sdkprov.Issue, 0, 150)
	for i := 1; i <= 150; i++ {
		issues = append(issues, &sdkprov.Issue{
			Number: strconv.Itoa(i), Title: "target-issue-" + strconv.Itoa(i), State: "open",
		})
	}
	prov := &fakeEngineProv{issues: issues}

	res := svc.runRestore(t.Context(), RestoreRequest{RepoKey: "r1", Kinds: []string{"issues"}},
		snapDir, snap, &model.Platform{Type: "github"}, "acme", "app", prov)

	st := res.Stats["issues"]
	require.NotNil(t, st)
	assert.Equal(t, 0, st.Created, "同名 issue 已存在(第 2 页)⇒ 不得重复创建")
	assert.Equal(t, 1, st.Skipped, "应识别为已存在并跳过")
	assert.Empty(t, prov.createdIssues, "不得真的创建重复 issue")
}
