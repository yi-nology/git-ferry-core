package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmodel "github.com/yi-nology/go-git-platform/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
)

// fakeQuerier 可控的分支 HEAD/Compare fake。
type fakeQuerier struct {
	head   map[string]string // "owner/repo:branch" → sha;缺省 = 分支不存在
	cmpErr error
}

func (f *fakeQuerier) ListCommits(_ context.Context, owner, repo string, opts sdkprov.ListCommitsOptions) ([]*sdkmodel.CommitInfo, error) {
	sha, ok := f.head[owner+"/"+repo+":"+opts.Branch]
	if !ok {
		return nil, nil // 分支不存在
	}
	return []*sdkmodel.CommitInfo{{SHA: sha}}, nil
}

func (f *fakeQuerier) CompareCommits(_ context.Context, _, _, base, head string) (*sdkmodel.CompareResult, error) {
	if f.cmpErr != nil {
		return nil, f.cmpErr
	}
	// 简化语义:head="src:NN" base="tgt:MM" —— 从 sha 尾部数字推导 ahead/behind。
	return &sdkmodel.CompareResult{AheadBy: shaNum(head) - shaNum(base), BehindBy: 0}, nil
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

	// 查询出错 ≠ 分支不存在:非 404 的失败(500/403/429/超时)一律 unknown,
	// 不得伪装成 source_missing(#44)。
	brokenQ := &fakeErrQuerier{err: errors.New("api 500")}
	res = reconcileBranches(ctx, brokenQ, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileUnknown, res.Status)
	assert.Contains(t, res.Message, "api 500")

	// 平台明确 404 ⇒ 分支不存在(missing)。
	notFoundQ := &fakeErrQuerier{err: fmt.Errorf("get branch: %w", sdkprov.ErrNotFound)}
	res = reconcileBranches(ctx, notFoundQ, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileSourceMissing, res.Status)

	// 目标侧查询 403/429 同样 unknown。
	for _, qerr := range []error{
		fmt.Errorf("auth: %w", sdkprov.ErrForbidden),
		fmt.Errorf("throttle: %w", sdkprov.ErrRateLimited),
		context.DeadlineExceeded,
	} {
		badTgt := &fakeErrQuerier{err: qerr}
		src = &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}}
		res = reconcileBranches(ctx, src, badTgt, "o", "r", "main", "o", "r", "main")
		assert.Equal(t, model.ReconcileUnknown, res.Status, "err=%v 不得归 target_missing", qerr)
	}
}

// fakeErrQuerier ListCommits 恒错(区分「查询失败」与「分支不存在」)。
type fakeErrQuerier struct{ err error }

func (f *fakeErrQuerier) ListCommits(context.Context, string, string, sdkprov.ListCommitsOptions) ([]*sdkmodel.CommitInfo, error) {
	return nil, f.err
}
func (f *fakeErrQuerier) CompareCommits(context.Context, string, string, string, string) (*sdkmodel.CompareResult, error) {
	return nil, f.err
}

// TestReconcileBranches_CompareFailure 漂移但 Compare 挂:报 unknown,
// 不得 panic/误判 in_sync,也不得把「无法判断」报成「已证明漂移」(#44)。
func TestReconcileBranches_CompareFailure(t *testing.T) {
	src := &fakeQuerier{head: map[string]string{"o/r:main": "sha-7"}}
	tgt := &fakeQuerier{head: map[string]string{"o/r:main": "sha-5"}, cmpErr: errors.New("compare boom")}
	res := reconcileBranches(context.Background(), src, tgt, "o", "r", "main", "o", "r", "main")
	require.Equal(t, model.ReconcileUnknown, res.Status)
	assert.Contains(t, res.Message, "CompareCommits 失败")
	assert.Zero(t, res.SourceAhead, "无法比较时不得编造 ahead/behind")

	// 平台无 Compare 能力 ⇒ unsupported(不是 drift)。
	tgt.cmpErr = fmt.Errorf("compare: %w", sdkprov.ErrNotImplemented)
	res = reconcileBranches(context.Background(), src, tgt, "o", "r", "main", "o", "r", "main")
	assert.Equal(t, model.ReconcileUnsupported, res.Status)
	assert.Contains(t, res.Message, "不支持 CompareCommits")
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

	repoRepository, err := repository.NewRepoRepository(db)
	require.NoError(t, err)
	return &Service{
		tasks:      NewTaskService(repository.NewSyncTaskRepository(db), repository.NewSyncRunRepository(db), repository.NewSyncRunStepRepository(db), repoRepository),
		reconciles: repository.NewReconcileRepository(db),
		outbox:     repository.NewOutboxEventRepository(db),
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

// TestRecordReconcile_UnknownSeparateTopic #44:unknown/unsupported 不进
// drift 告警主题,落到 reconcile.unknown(运营按主题分流);仍可被订阅方消费。
func TestRecordReconcile_UnknownSeparateTopic(t *testing.T) {
	svc := newReconcileTestService(t, &model.Config{})
	require.NoError(t, svc.recordReconcile(&model.ReconcileResult{
		TaskKey: "t-unknown", Status: model.ReconcileUnknown, Message: "403 from api", CheckedAt: time.Now(),
	}))
	evs, err := svc.outbox.ListDue(10, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, model.OutboxTopicReconcileUnknown, evs[0].Topic, "unknown 不得进 drift 主题")
	assert.Equal(t, "unknown", evs[0].Status)

	got := make(chan ReconcileDrift, 1)
	cancel := svc.SubscribeReconcileDrift(func(d ReconcileDrift) { got <- d })
	defer cancel()
	svc.publishDueOutbox(0)
	select {
	case d := <-got:
		assert.Equal(t, "unknown", d.Status)
	case <-time.After(time.Second):
		t.Fatal("reconcile.unknown 事件必须仍可投递")
	}
}
