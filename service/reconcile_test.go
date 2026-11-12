package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// fakeQuerier 可控的分支 HEAD/Compare fake。
type fakeQuerier struct {
	head   map[string]string // "owner/repo:branch" → sha;缺省 = 分支不存在
	cmpErr error
}

func (f *fakeQuerier) ListCommits(_ context.Context, owner, repo string, opts sdkprov.ListCommitsOptions) ([]*sdkprov.CommitInfo, error) {
	sha, ok := f.head[owner+"/"+repo+":"+opts.Branch]
	if !ok {
		return nil, nil // 分支不存在
	}
	return []*sdkprov.CommitInfo{{SHA: sha}}, nil
}

func (f *fakeQuerier) CompareCommits(_ context.Context, _, _, base, head string) (*sdkprov.CompareResult, error) {
	if f.cmpErr != nil {
		return nil, f.cmpErr
	}
	// 简化语义:head="src:NN" base="tgt:MM" —— 从 sha 尾部数字推导 ahead/behind。
	return &sdkprov.CompareResult{AheadBy: shaNum(head) - shaNum(base), BehindBy: 0}, nil
}

// shaNum 从 "sha-7" 这类测试 sha 取数字。
func shaNum(sha string) int {
	n := 0
	for _, c := range sha {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func TestReconcileBranches(t *testing.T) {
	ctx := context.Background()

	// 两端一致
	src := &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}}
	tgt := &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}}
	res := reconcileBranches(ctx, src, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileInSync, res.Status)
	assert.Equal(t, "sha-5", res.SourceSHA)
	assert.Zero(t, res.SourceAhead)

	// 漂移:源 ahead 2(sha-7 vs sha-5)
	src = &fakeQuerier{head: map[string]string{"o/r:main": "sha-7"}}
	tgt = &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}}
	res = reconcileBranches(ctx, src, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileDrift, res.Status)
	assert.Equal(t, 2, res.SourceAhead)
	assert.Equal(t, "sha-7", res.SourceSHA)
	assert.Equal(t, "sha-5", res.TargetSHA)

	// 源分支不存在
	src = &fakeQuerier{head: map[string]string{}}
	tgt = &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}}
	res = reconcileBranches(ctx, src, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileSourceMissing, res.Status)

	// 目标分支不存在
	src = &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}}
	tgt = &fakeQuerier{head: map[string]string{}}
	res = reconcileBranches(ctx, src, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileTargetMissing, res.Status)

	// 查询出错 ≠ 分支不存在:错误要留在 Message 里可见,而不是静默当 missing
	brokenQ := &fakeErrQuerier{err: errors.New("api 500")}
	res = reconcileBranches(ctx, brokenQ, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileSourceMissing, res.Status)
	assert.Contains(t, res.Message, "api 500")
}

// fakeErrQuerier ListCommits 恒错(区分「查询失败」与「分支不存在」)。
type fakeErrQuerier struct{ err error }

func (f *fakeErrQuerier) ListCommits(context.Context, string, string, sdkprov.ListCommitsOptions) ([]*sdkprov.CommitInfo, error) {
	return nil, f.err
}
func (f *fakeErrQuerier) CompareCommits(context.Context, string, string, string, string) (*sdkprov.CompareResult, error) {
	return nil, f.err
}

// TestReconcileBranches_CompareFailure 漂移但 Compare 挂:仍报 drift,
// 不得 panic/误判 in_sync。
func TestReconcileBranches_CompareFailure(t *testing.T) {
	src := &fakeQuerier{head: map[string]string{"o/r:main": "sha-7"}}
	tgt := &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}, cmpErr: errors.New("compare boom")}
	res := reconcileBranches(context.Background(), src, tgt, "o", "r", "main", "o", "r", "main")
	require.Equal(t, model.ReconcileDrift, res.Status)
	assert.Contains(t, res.Message, "CompareCommits 失败")
}

// TestBranchHeadSHA 单测:空列表 = 分支不存在("");错误上抛。
func TestBranchHeadSHA(t *testing.T) {
	q := &fakeQuerier{head: map[string]string{"o/r:dev": "abc123"}}
	sha, err := branchHeadSHA(context.Background(), q, "o", "r", "dev")
	require.NoError(t, err)
	assert.Equal(t, "abc123", sha)

	sha, err = branchHeadSHA(context.Background(), q, "o", "r", "ghost")
	require.NoError(t, err)
	assert.Empty(t, sha, "空列表 = 分支不存在")
}

// newReconcileTestService 带对账存储的轻量服务。
func newReconcileTestService(t *testing.T, cfg *model.Config) *Service {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{},
		&model.Repo{}, &model.ReconcileRecord{}, &model.OutboxEvent{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	return &Service{
		tasks:      NewTaskService(dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db), dao.NewSyncRunStepDAO(db), repoDAO),
		reconciles: dao.NewReconcileDAO(db),
		outbox:     dao.NewOutboxEventDAO(db),
		bgCtx:      context.Background(),
		config:     cfg,
	}
}

// TestReconcileEnabled_ConfigSwitch 间隔配置开关(默认关=零变化)。
func TestReconcileEnabled_ConfigSwitch(t *testing.T) {
	svc := newReconcileTestService(t, &model.Config{})
	assert.False(t, svc.reconcileEnabled(), "默认不启用周期对账")
	svc.config.Sync.ReconcileIntervalHours = 6
	assert.True(t, svc.reconcileEnabled())
}

// TestRecordReconcile_DriftWritesOutbox 对账结果落库;漂移时写
// reconcile.drift outbox 事件;in_sync 不得写事件(告警面不被噪音淹没)。
func TestRecordReconcile_DriftWritesOutbox(t *testing.T) {
	svc := newReconcileTestService(t, &model.Config{})

	// in_sync:落库,不写事件
	require.NoError(t, svc.recordReconcile(&model.ReconcileResult{
		TaskKey: "t-ok", Status: model.ReconcileInSync, SourceSHA: "s", TargetSHA: "s",
		CheckedAt: time.Now(),
	}))
	evs, err := svc.outbox.ListDue(10, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, evs, "in_sync 不得写漂移事件")

	rec, err := svc.reconciles.Get("t-ok")
	require.NoError(t, err)
	assert.Equal(t, model.ReconcileInSync, rec.Status)

	// drift:落库 + 写事件
	require.NoError(t, svc.recordReconcile(&model.ReconcileResult{
		TaskKey: "t-drift", Status: model.ReconcileDrift, SourceSHA: "s1", TargetSHA: "s2",
		SourceAhead: 3, Message: "两端漂移", CheckedAt: time.Now(),
	}))
	evs, err = svc.outbox.ListDue(10, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, evs, 1, "漂移必须写 reconcile.drift 事件")
	assert.Equal(t, model.OutboxTopicReconcileDrift, evs[0].Topic)
	assert.Equal(t, "t-drift", evs[0].TaskKey)
	assert.Contains(t, evs[0].Payload, "\"source_ahead\":3")

	rec, err = svc.reconciles.Get("t-drift")
	require.NoError(t, err)
	assert.Equal(t, 3, rec.SourceAhead)
}

// TestRecordReconcile_NoStoreDegrades 对账存储未接线时不 panic(fail-open 于观测面)。
func TestRecordReconcile_NoStoreDegrades(t *testing.T) {
	svc := &Service{bgCtx: context.Background(), config: &model.Config{}}
	require.NoError(t, svc.recordReconcile(&model.ReconcileResult{
		TaskKey: "t", Status: model.ReconcileDrift, CheckedAt: time.Now(),
	}), "reconciles/outbox 为 nil 时应静默跳过")
}
