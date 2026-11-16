package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	sdkmodel "github.com/yi-nology/go-git-platform/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
)

// fakeMappingStore 内存版迁移映射(#41):身份键四元组,标题不参与。
type fakeMappingStore struct {
	rows map[string]*model.MigrationMapping
}

func newFakeMappingStore() *fakeMappingStore {
	return &fakeMappingStore{rows: map[string]*model.MigrationMapping{}}
}

func mappingKey(platform, repo, objType, number string) string {
	return fmt.Sprintf("%s|%s|%s|%s", platform, repo, objType, number)
}

func (f *fakeMappingStore) Find(p, r, o, n string) (*model.MigrationMapping, error) {
	m, ok := f.rows[mappingKey(p, r, o, n)]
	if !ok {
		return nil, nil
	}
	cp := *m
	return &cp, nil
}

func (f *fakeMappingStore) CreatePending(m *model.MigrationMapping) (bool, error) {
	k := mappingKey(m.SourcePlatform, m.SourceRepo, m.ObjectType, m.SourceNumber)
	if _, ok := f.rows[k]; ok {
		return false, nil
	}
	cp := *m
	f.rows[k] = &cp
	return true, nil
}

func (f *fakeMappingStore) Save(m *model.MigrationMapping) error {
	cp := *m
	f.rows[mappingKey(m.SourcePlatform, m.SourceRepo, m.ObjectType, m.SourceNumber)] = &cp
	return nil
}

func (f *fakeMappingStore) MarkFailed(p, r, o, n string, cause error) error {
	k := mappingKey(p, r, o, n)
	if m, ok := f.rows[k]; ok {
		m.Status = model.MigrationMappingFailed
		if cause != nil {
			m.LastError = cause.Error()
		}
	}
	return nil
}

// fakeMigrator 可控的源/目标 provider(读写都走内存)。
type fakeMigrator struct {
	issues          []*sdkmodel.Issue
	comments        map[string][]*sdkmodel.IssueComment // issue number -> comments
	crs             []*sdkmodel.ChangeRequest
	createdIs       []*sdkprov.CreateIssueOptions
	createdCm       []string // comment bodies
	createdCr       []*sdkprov.CreateCROptions
	closedIs        []string
	closedCr        []string
	failCreateIssue bool
	// failDraft 目标平台无 draft 写面(模拟 SDK #160 ErrDraftNotSupported)
	failDraft bool
	// Ensure 收敛可控返回
	ensureStateOutcome sdkrt.MutationOutcome
	ensureStateErr     error
	ensureLabelsErr    error
	// ensuredState 记录 EnsureIssueState 调用(want)
	ensuredState []sdkmodel.IssueState
}

func (f *fakeMigrator) ListIssues(_ context.Context, opts sdkprov.ListIssuesOptions) ([]*sdkmodel.Issue, int, error) {
	// 单页返回全部(测试无需真分页;limit 由上层截断)
	return f.issues, len(f.issues), nil
}

func (f *fakeMigrator) CreateIssue(_ context.Context, opts sdkprov.CreateIssueOptions) (*sdkmodel.Issue, error) {
	if f.failCreateIssue {
		return nil, errors.New("create boom")
	}
	f.createdIs = append(f.createdIs, &opts)
	return &sdkmodel.Issue{Number: "n-created", Title: opts.Title, State: sdkmodel.IssueStateOpen}, nil
}

func (f *fakeMigrator) CloseIssue(_ context.Context, _, _, number string) (*sdkmodel.Issue, error) {
	f.closedIs = append(f.closedIs, number)
	return &sdkmodel.Issue{Number: number, State: sdkmodel.IssueStateClosed}, nil
}

func (f *fakeMigrator) ListIssueComments(_ context.Context, _, _, number string) ([]*sdkmodel.IssueComment, error) {
	return f.comments[number], nil
}

func (f *fakeMigrator) CreateIssueComment(_ context.Context, _, _, _, body string) (*sdkmodel.IssueComment, error) {
	f.createdCm = append(f.createdCm, body)
	return &sdkmodel.IssueComment{Body: body}, nil
}

func (f *fakeMigrator) ListCRs(_ context.Context, _ sdkprov.ListCROptions) ([]*sdkmodel.ChangeRequest, *int, error) {
	n := len(f.crs)
	return f.crs, &n, nil
}

func (f *fakeMigrator) CreateCR(_ context.Context, opts sdkprov.CreateCROptions) (*sdkmodel.ChangeRequest, error) {
	f.createdCr = append(f.createdCr, &opts)
	if opts.Draft && f.failDraft {
		return nil, sdkprov.ErrDraftNotSupported
	}
	return &sdkmodel.ChangeRequest{Number: "cr-created", Title: opts.Title, State: sdkmodel.CRStateOpened}, nil
}

func (f *fakeMigrator) CloseCR(_ context.Context, _, _, number string) (*sdkmodel.ChangeRequest, error) {
	f.closedCr = append(f.closedCr, number)
	return &sdkmodel.ChangeRequest{Number: number, State: sdkmodel.CRStateClosed}, nil
}

// --- issueEnsurer(SDK 2B Ensure* 可注入缝) ---
// ensureStateOutcome/ensureLabelsOutcome 可控返回(测试按需设置)。
func (f *fakeMigrator) EnsureIssueState(_ context.Context, _, _, _ string, want sdkmodel.IssueState) (sdkrt.MutationOutcome, error) {
	f.ensuredState = append(f.ensuredState, want)
	if f.ensureStateErr != nil {
		return "", f.ensureStateErr
	}
	if f.ensureStateOutcome != "" {
		return f.ensureStateOutcome, nil
	}
	return sdkrt.OutcomeUnchanged, nil
}

func (f *fakeMigrator) EnsureIssueLabels(_ context.Context, _, _, _ string, _ []string) (sdkrt.MutationOutcome, error) {
	if f.ensureLabelsErr != nil {
		return "", f.ensureLabelsErr
	}
	return sdkrt.OutcomeUnchanged, nil
}

// TestMigrateIssues_WithCommentsAndClose Issue 迁移:标题/正文/labels 搬运、
// 源 closed 同步关闭、评论逐条搬运。
func TestMigrateIssues_WithCommentsAndClose(t *testing.T) {
	src := &fakeMigrator{
		issues: []*sdkmodel.Issue{
			{Number: "1", Title: "open issue", Body: "b1", Labels: []string{"bug"}, State: sdkmodel.IssueStateOpen},
			{Number: "2", Title: "closed issue", Body: "b2", State: sdkmodel.IssueStateClosed},
			{Number: "3", Title: "pr disguised", IsPullRequest: true}, // GitHub 混入的 PR
		},
		comments: map[string][]*sdkmodel.IssueComment{
			"1": {{Body: "first"}, {Body: "second"}},
		},
	}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{Notes: metadataFidelityNotes}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, nil,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true, Comments: true}, rep))

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
	src := &fakeMigrator{issues: []*sdkmodel.Issue{{Number: "1", Title: "t1"}}}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, nil,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true, DryRun: true}, rep))

	assert.Empty(t, tgt.createdIs, "dry-run 不得写目标")
	require.Len(t, rep.DryRunPlanned, 1)
	assert.Contains(t, rep.DryRunPlanned[0], "issue: t1")
	assert.Zero(t, rep.CreatedIssues)
}

// TestMigrateDryRun_ConvergePlan dry-run 依据持久化映射看到收敛面:
// 源 #1 已映射到目标 #9 ⇒ 计划标 converge:(不再靠目标侧标题猜)。
func TestMigrateDryRun_ConvergePlan(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkmodel.Issue{{Number: "1", Title: "t1"}}}
	tgt := &fakeMigrator{}
	maps := newFakeMappingStore()
	require.NoError(t, maps.Save(&model.MigrationMapping{
		SourcePlatform: "srcplat", SourceRepo: "so/sr", ObjectType: model.MigrationObjectIssue,
		SourceNumber: "1", TargetPlatform: "tgtplat", TargetRepo: "to/tr", TargetNumber: "9",
		Status: model.MigrationMappingMigrated,
	}))
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, maps,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true, DryRun: true}, rep))

	require.Len(t, rep.DryRunPlanned, 1)
	assert.Contains(t, rep.DryRunPlanned[0], "converge: t1")
	assert.Empty(t, tgt.createdIs, "dry-run 不得写目标")
}

// TestMigrateSameTitleDistinctSources 同标题的两个源 Issue 不得归并:
// 身份键是源编号 ⇒ 各创建一个目标对象、各写一条映射(#41)。
func TestMigrateSameTitleDistinctSources(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkmodel.Issue{
		{Number: "1", Title: "same title", Body: "first"},
		{Number: "2", Title: "same title", Body: "second"},
	}}
	tgt := &fakeMigrator{}
	maps := newFakeMappingStore()
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, maps,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))

	assert.Equal(t, 2, rep.CreatedIssues, "同标题必须各自创建,不得归并")
	require.Len(t, tgt.createdIs, 2)
	assert.Equal(t, []string{"same title", "same title"}, []string{tgt.createdIs[0].Title, tgt.createdIs[1].Title})
	require.Len(t, maps.rows, 2, "每个源编号一条映射(不按标题)")
	m1, err := maps.Find("srcplat", "so/sr", model.MigrationObjectIssue, "1")
	require.NoError(t, err)
	require.NotNil(t, m1)
	assert.NotEmpty(t, m1.TargetNumber)
	assert.Equal(t, model.MigrationMappingMigrated, m1.Status)
}

// TestMigrateUnclosedPendingMappingIsReported 创建与回填之间崩溃留下的
// pending 映射:显式报告并跳过创建(绝不静默再建一份),断点可见。
func TestMigrateUnclosedPendingMappingIsReported(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkmodel.Issue{{Number: "1", Title: "half"}}}
	tgt := &fakeMigrator{}
	maps := newFakeMappingStore()
	_, err := maps.CreatePending(&model.MigrationMapping{
		SourcePlatform: "srcplat", SourceRepo: "so/sr", ObjectType: model.MigrationObjectIssue,
		SourceNumber: "1", Status: model.MigrationMappingPending,
	})
	require.NoError(t, err)
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, maps,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))

	assert.Zero(t, rep.CreatedIssues, "未闭环映射不得重复创建")
	assert.Equal(t, 1, rep.UnclosedMappings)
	require.NotEmpty(t, rep.Errors)
	assert.Contains(t, rep.Errors[0], "unclosed migration mapping")
}

// TestMigrateCreateFailureDoesNotAbort 单条失败不中断其余(完整报告)。
func TestMigrateCreateFailureDoesNotAbort(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkmodel.Issue{
		{Number: "1", Title: "a"},
		{Number: "2", Title: "b"},
	}}
	tgt := &fakeMigrator{}
	rep := &MetadataMigrateReport{}
	// 第一条就失败(fake 恒失败)——仍要处理完第二条的语义由循环保证,
	// 这里用 failCreateIssue 恒失败验证:报告两条错误、不 panic。
	tgt.failCreateIssue = true

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, nil,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))
	assert.Zero(t, rep.CreatedIssues)
	assert.Len(t, rep.Errors, 2, "每条失败都要可见")
}

// TestMigrateCRs PR 搬运:创建 + 源 closed 同步关闭;merged 不重放(只创建)。
func TestMigrateCRs(t *testing.T) {
	src := &fakeMigrator{crs: []*sdkmodel.ChangeRequest{
		{Number: "1", Title: "open pr", SourceBranch: "feat", TargetBranch: "main"},
		{Number: "2", Title: "closed pr", State: sdkmodel.CRStateClosed},
		{Number: "3", Title: "merged pr", State: sdkmodel.CRStateMerged},
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
	src := &fakeMigrator{crs: []*sdkmodel.ChangeRequest{
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
	src := &fakeMigrator{crs: []*sdkmodel.ChangeRequest{
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
// 持久化映射命中 ⇒ 不重复创建,EnsureIssueState/EnsureIssueLabels 收敛;
// Unchanged/Updated 按 outcome 分类——迁移可安全重跑。
func TestMigrateIssues_IdempotentConverge(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkmodel.Issue{
		{Number: "1", Title: "已存在", State: sdkmodel.IssueStateClosed, Labels: []string{"bug"}},
		{Number: "2", Title: "新建", State: sdkmodel.IssueStateOpen},
	}}
	tgt := &fakeMigrator{ensureStateOutcome: sdkrt.OutcomeUnchanged}
	maps := newFakeMappingStore()
	require.NoError(t, maps.Save(&model.MigrationMapping{
		SourcePlatform: "srcplat", SourceRepo: "so/sr", ObjectType: model.MigrationObjectIssue,
		SourceNumber: "1", TargetPlatform: "tgtplat", TargetRepo: "to/tr", TargetNumber: "9",
		Status: model.MigrationMappingMigrated,
	}))
	rep := &MetadataMigrateReport{}

	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, maps,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))

	// 「已存在」走映射收敛:不重复创建 + Ensure 被调(状态 open→closed want)
	assert.Equal(t, 1, rep.CreatedIssues, "只有「新建」被创建")
	assert.Equal(t, 1, rep.UnchangedIssues, "已达标 zero-write 计入 unchanged")
	require.Len(t, tgt.ensuredState, 1, "收敛路径必须调 EnsureIssueState")
	assert.Equal(t, sdkmodel.IssueStateClosed, tgt.ensuredState[0], "want=源状态 closed")
	assert.Empty(t, rep.Errors)
}

// TestMigrateIssues_UpdatedOutcome 收敛发生写入 ⇒ Updated 计数。
func TestMigrateIssues_UpdatedOutcome(t *testing.T) {
	src := &fakeMigrator{issues: []*sdkmodel.Issue{
		{Number: "1", Title: "t", State: sdkmodel.IssueStateClosed},
	}}
	tgt := &fakeMigrator{ensureStateOutcome: sdkrt.OutcomeUpdated}
	maps := newFakeMappingStore()
	require.NoError(t, maps.Save(&model.MigrationMapping{
		SourcePlatform: "srcplat", SourceRepo: "so/sr", ObjectType: model.MigrationObjectIssue,
		SourceNumber: "1", TargetPlatform: "tgtplat", TargetRepo: "to/tr", TargetNumber: "9",
		Status: model.MigrationMappingMigrated,
	}))
	rep := &MetadataMigrateReport{}
	require.NoError(t, migrateIssues(context.Background(), src, tgt, tgt, maps,
		"srcplat", "so", "sr", "tgtplat", "to", "tr", 500, MetadataMigrateOptions{Issues: true}, rep))
	assert.Equal(t, 1, rep.UpdatedIssues)
	assert.Zero(t, rep.UnchangedIssues)
	assert.Zero(t, rep.CreatedIssues, "收敛路径不得创建")
}
