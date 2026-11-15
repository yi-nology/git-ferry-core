package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// fakeMigrator 可控的源/目标 provider(读写都走内存)。
type fakeMigrator struct {
	issues          []*sdkprov.Issue
	comments        map[string][]*sdkprov.IssueComment // issue number -> comments
	crs             []*sdkprov.ChangeRequest
	createdIs       []*sdkprov.CreateIssueOptions
	createdCm       []string // comment bodies
	createdCr       []*sdkprov.CreateCROptions
	closedIs        []string
	closedCr        []string
	failCreateIssue bool
	// failDraft 目标平台无 draft 写面(模拟 SDK #160 ErrDraftNotSupported)
	failDraft bool
	// Ensure 收敛可控返回
	ensureStateOutcome sdkprov.MutationOutcome
	ensureStateErr     error
	ensureLabelsErr    error
	// ensuredState 记录 EnsureIssueState 调用(want)
	ensuredState []sdkprov.IssueState
}

func (f *fakeMigrator) ListIssues(_ context.Context, opts sdkprov.ListIssuesOptions) ([]*sdkprov.Issue, int, error) {
	// 单页返回全部(测试无需真分页;limit 由上层截断)
	return f.issues, len(f.issues), nil
}

func (f *fakeMigrator) CreateIssue(_ context.Context, opts sdkprov.CreateIssueOptions) (*sdkprov.Issue, error) {
	if f.failCreateIssue {
		return nil, errors.New("create boom")
	}
	f.createdIs = append(f.createdIs, &opts)
	return &sdkprov.Issue{Number: "n-created", Title: opts.Title, State: sdkprov.IssueStateOpen}, nil
}

func (f *fakeMigrator) CloseIssue(_ context.Context, _, _, number string) (*sdkprov.Issue, error) {
	f.closedIs = append(f.closedIs, number)
	return &sdkprov.Issue{Number: number, State: sdkprov.IssueStateClosed}, nil
}

func (f *fakeMigrator) ListIssueComments(_ context.Context, _, _, number string) ([]*sdkprov.IssueComment, error) {
	return f.comments[number], nil
}

func (f *fakeMigrator) CreateIssueComment(_ context.Context, _, _, _, body string) (*sdkprov.IssueComment, error) {
	f.createdCm = append(f.createdCm, body)
	return &sdkprov.IssueComment{Body: body}, nil
}

func (f *fakeMigrator) ListCRs(_ context.Context, _ sdkprov.ListCROptions) ([]*sdkprov.ChangeRequest, *int, error) {
	n := len(f.crs)
	return f.crs, &n, nil
}

func (f *fakeMigrator) CreateCR(_ context.Context, opts sdkprov.CreateCROptions) (*sdkprov.ChangeRequest, error) {
	f.createdCr = append(f.createdCr, &opts)
	if opts.Draft && f.failDraft {
		return nil, sdkprov.ErrDraftNotSupported
	}
	return &sdkprov.ChangeRequest{Number: "cr-created", Title: opts.Title, State: sdkprov.CRStateOpened}, nil
}

func (f *fakeMigrator) CloseCR(_ context.Context, _, _, number string) (*sdkprov.ChangeRequest, error) {
	f.closedCr = append(f.closedCr, number)
	return &sdkprov.ChangeRequest{Number: number, State: sdkprov.CRStateClosed}, nil
}

// --- issueEnsurer(SDK 2B Ensure* 可注入缝) ---
// ensureStateOutcome/ensureLabelsOutcome 可控返回(测试按需设置)。
func (f *fakeMigrator) EnsureIssueState(_ context.Context, _, _, _ string, want sdkprov.IssueState) (sdkprov.MutationOutcome, error) {
	f.ensuredState = append(f.ensuredState, want)
	if f.ensureStateErr != nil {
		return "", f.ensureStateErr
	}
	if f.ensureStateOutcome != "" {
		return f.ensureStateOutcome, nil
	}
	return sdkprov.OutcomeUnchanged, nil
}

func (f *fakeMigrator) EnsureIssueLabels(_ context.Context, _, _, _ string, _ []string) (sdkprov.MutationOutcome, error) {
	if f.ensureLabelsErr != nil {
		return "", f.ensureLabelsErr
	}
	return sdkprov.OutcomeUnchanged, nil
}

// TestMigrateIssues_WithCommentsAndClose Issue 迁移:标题/正文/labels 搬运、
// 源 closed 同步关闭、评论逐条搬运。
func TestMigrateIssues_WithCommentsAndClose(t *testing.T) {
	src := &fakeMigrator{
		issues: []*sdkprov.Issue{
			{Number: "1", Title: "open issue", Body: "b1", Labels: []string{"bug"}, State: sdkprov.IssueStateOpen},
			{Number: "2", Title: "closed issue", Body: "b2", State: sdkprov.IssueStateClosed},
			{Number: "3", Title: "pr disguised", IsPullRequest: true}, // GitHub 混入的 PR
		},
		comments: map[string][]*sdkprov.IssueComment{
			"1": {{Body: "first"}, {Body: "second"}},
		},
	}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{Notes: metadataFidelityNotes}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{Issues: true, Comments: true}, rep))

	assert.Equal(t, 3, rep.SourceIssues)
	assert.Equal(t, 2, rep.CreatedIssues, "IsPullRequest 的必须被过滤(PR 通道搬)")
	require.Len(t, tgt.createdIs, 2)
	assert.Equal(t, "open issue", tgt.createdIs[0].Title)
	assert.Equal(t, []string{"bug"}, tgt.createdIs[0].Labels)
	assert.Equal(t, 1, rep.ClosedIssues, "源 closed ⇒ 目标同步关闭")
	assert.Equal(t, 2, rep.CreatedComments, "评论逐条搬运")
	assert.Equal(t, []string{"first", "second"}, tgt.createdCm)
	assert.Empty(t, rep.Errors)
}

// TestMigrateDryRun 盘点不写:Planned 有、目标零写入。
func TestMigrateDryRun(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkprov.Issue{{Number: "1", Title: "t1"}}}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{Issues: true, DryRun: true}, rep))

	assert.Empty(t, tgt.createdIs, "dry-run 不得写目标")
	require.Len(t, rep.DryRunPlanned, 1)
	assert.Contains(t, rep.DryRunPlanned[0], "issue: t1")
	assert.Zero(t, rep.CreatedIssues)
}

// TestMigrateCreateFailureDoesNotAbort 单条失败不中断其余(完整报告)。
func TestMigrateCreateFailureDoesNotAbort(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkprov.Issue{
		{Number: "1", Title: "a"},
		{Number: "2", Title: "b"},
	}}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{}
	// 第一条就失败(fake 恒失败)——仍要处理完第二条的语义由循环保证,
	// 这里用 failCreateIssue 恒失败验证:报告两条错误、不 panic。
	tgt.failCreateIssue = true

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))
	assert.Zero(t, rep.CreatedIssues)
	assert.Len(t, rep.Errors, 2, "每条失败都要可见")
}

// TestMigrateCRs PR 搬运:创建 + 源 closed 同步关闭;merged 不重放(只创建)。
func TestMigrateCRs(t *testing.T) {
	src := &fakeMigrator{crs: []*sdkprov.ChangeRequest{
		{Number: "1", Title: "open pr", SourceBranch: "feat", TargetBranch: "main"},
		{Number: "2", Title: "closed pr", State: sdkprov.CRStateClosed},
		{Number: "3", Title: "merged pr", State: sdkprov.CRStateMerged},
	}}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateCRs(context.Background(), src, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{PRs: true}, rep))

	assert.Equal(t, 3, rep.SourcePRs)
	assert.Equal(t, 3, rep.CreatedPRs, "merged 也创建(状态不重放,见 Notes)")
	assert.Equal(t, 1, rep.ClosedPRs, "仅源 closed 同步关闭;merged 不重放")
	assert.Equal(t, "feat", tgt.createdCr[0].SourceBranch, "分支映射要带")
}

// TestMigrateCRs_DraftPreserved SDK #160 写侧:源 draft 透传目标
// (平台创建面可表达时原样下发)。
func TestMigrateCRs_DraftPreserved(t *testing.T) {
	src := &fakeMigrator{crs: []*sdkprov.ChangeRequest{
		{Number: "1", Title: "wip", SourceBranch: "feat", TargetBranch: "main", Draft: true},
		{Number: "2", Title: "ready", SourceBranch: "feat2", TargetBranch: "main"},
	}}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateCRs(context.Background(), src, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{PRs: true}, rep))

	require.Len(t, tgt.createdCr, 2)
	assert.True(t, tgt.createdCr[0].Draft, "源 draft 必须透传")
	assert.False(t, tgt.createdCr[1].Draft)
	assert.Zero(t, rep.DraftDegraded)
	assert.Equal(t, 2, rep.CreatedPRs)
}

// TestMigrateCRs_DraftDegraded 目标平台创建面无 draft 表达
// (ErrDraftNotSupported)⇒ 降级非 draft 创建 + 计数留痕,绝不静默。
func TestMigrateCRs_DraftDegraded(t *testing.T) {
	src := &fakeMigrator{crs: []*sdkprov.ChangeRequest{
		{Number: "1", Title: "wip", SourceBranch: "feat", TargetBranch: "main", Draft: true},
	}}
	tgt := &fakeMigrator{failDraft: true}
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateCRs(context.Background(), src, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{PRs: true}, rep))

	require.Len(t, tgt.createdCr, 2, "先 draft 再降级,两次调用都要可见")
	assert.True(t, tgt.createdCr[0].Draft)
	assert.False(t, tgt.createdCr[1].Draft, "降级路径必须非 draft")
	assert.Equal(t, 1, rep.DraftDegraded)
	assert.Equal(t, 1, rep.CreatedPRs, "降级成功后仍计 created")
	assert.Empty(t, rep.Errors)
}

// TestMetadataFidelityNotes 保真边界必须随报告携带(诚实声明)。
func TestMetadataFidelityNotes(t *testing.T) {
	assert.NotEmpty(t, metadataFidelityNotes)
	joined := ""
	for _, n := range metadataFidelityNotes {
		joined += n
	}
	assert.Contains(t, joined, "重置", "作者/时间重置必须声明")
	assert.Contains(t, joined, "merged", "merged 不重放必须声明")
}

// TestMigrateIssues_IdempotentConverge 幂等收敛(SDK 2B Ensure* 采用):
// 目标已有同标题 issue ⇒ 不重复创建,EnsureIssueState/EnsureIssueLabels
// 收敛;Unchanged/Updated 按 outcome 分类——迁移可安全重跑。
func TestMigrateIssues_IdempotentConverge(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkprov.Issue{
		{Number: "1", Title: "已存在", State: sdkprov.IssueStateClosed, Labels: []string{"bug"}},
		{Number: "2", Title: "新建", State: sdkprov.IssueStateOpen},
	}}
	tgt := &fakeMigrator{
		// 目标已有「已存在」同标题 issue
		issues: []*sdkprov.Issue{
			{Number: "9", Title: "已存在", State: sdkprov.IssueStateOpen, Labels: []string{"old"}},
		},
		ensureStateOutcome: sdkprov.OutcomeUnchanged,
	}
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))

	// 「已存在」走收敛:不重复创建 + Ensure 被调(状态 open→closed want)
	assert.Equal(t, 1, rep.CreatedIssues, "只有「新建」被创建")
	assert.Equal(t, 1, rep.UnchangedIssues, "已达标 zero-write 计入 unchanged")
	require.Len(t, tgt.ensuredState, 1, "收敛路径必须调 EnsureIssueState")
	assert.Equal(t, sdkprov.IssueStateClosed, tgt.ensuredState[0], "want=源状态 closed")
	assert.Empty(t, rep.Errors)
}

// TestMigrateIssues_UpdatedOutcome 收敛发生写入 ⇒ Updated 计数。
func TestMigrateIssues_UpdatedOutcome(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkprov.Issue{
		{Number: "1", Title: "t", State: sdkprov.IssueStateClosed},
	}}
	tgt := &fakeMigrator{
		issues:             []*sdkprov.Issue{{Number: "9", Title: "t", State: sdkprov.IssueStateOpen}},
		ensureStateOutcome: sdkprov.OutcomeUpdated,
	}
	rep := &MetadataMigrateReport{}
	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt,
		"so", "sr", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))
	assert.Equal(t, 1, rep.UpdatedIssues)
	assert.Zero(t, rep.UnchangedIssues)
	assert.Zero(t, rep.CreatedIssues, "收敛路径不得创建")
}
