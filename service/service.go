package service

import (
	"context"
	"database/sql"
	"github.com/oekit/logx"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/core/lifecycle"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/lock"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
	sdkmodel "github.com/yi-nology/go-git-platform/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
	"gorm.io/gorm"
)

// Compile-time check: Service satisfies executor.Service interface.
var _ executor.Service = (*Service)(nil)

type Config = model.Config

type Service struct {
	config        *Config
	db            *gorm.DB
	sqlDB         *sql.DB
	ownsDB        bool
	providerHooks *sdkprov.Hooks
	// providers SDK provider 缓存(健康检查顺带刷新缓存指标)。
	providers    *sdkrt.Manager
	templates    TemplateStore
	approvals    *ForcePushStore
	retryTracker *RetryTracker
	// pending 待补跑 webhook 触发的持久化存储(pending_triggers 表,见 pending.go)。
	pending *repository.PendingTriggerRepository
	// outbox 事务外发盒(outbox_events 表,见 events.go):运行完成事件
	// 与 run 落库同事务写入,派发器保证通知至少送达一次。
	outbox *repository.OutboxEventRepository
	// jobs 持久化执行队列(execution_jobs 表,见 queue.go);instanceID 为
	// lease 属主标识。仅 durable_queue=true 时启用。
	jobs       *repository.ExecutionJobRepository
	instanceID string
	// retrySchedules 延迟重跑排程(retry_schedules,见 retry_schedule.go);
	// 冷却期判定落库,到点由本实例或其它实例认领触发(#39)。
	retrySchedules *repository.RetryScheduleRepository
	// migrationMappings 元数据迁移的源→目标身份映射(#41);
	// 标题不再作为身份键,重复执行按映射断点续迁。
	migrationMappings *repository.MigrationMappingRepository
	// reconciles 对账结果存储(reconcile_results,见 reconcile.go);
	// 仅 reconcile_interval_hours>0 时启用。
	reconciles *repository.ReconcileRepository
	// recovery 恢复点索引(recovery_points,见 recovery.go)。
	recovery *repository.RecoveryPointRepository
	// driftSubs 对账漂移订阅者(见 events.go;壳层接告警渠道)。
	driftSubs  []driftSubscriber
	driftSubID uint64
	driftSubMu sync.Mutex
	runSubs    []runSubscriber
	runSubID   uint64
	runSubMu   sync.Mutex
	repos      *RepoService
	tasks      *TaskService
	webhooks   *WebhookService
	platforms  *PlatformService
	opLogs     *OperationLogService
	// cron 共享调度状态:DB 的 sync_tasks.next_fire_at 是唯一事实源,
	// 本地只保留「循环是否已启动」与取消函数(#40)。
	cronStarted     bool
	cronCancel      context.CancelFunc
	cronMu          sync.Mutex
	executor        *executor.Executor
	mirror          *MirrorService
	history         *HistoryService
	lastTriggerTime sync.Map
	// guard 统一封装”同 taskKey 互斥 + 全局并发上限”;配 redis 时为分布式,否则进程内。
	guard    concurrencyGuard
	shutdown *lifecycle.Manager
	stopOnce sync.Once
	bgCtx    context.Context
	bgCancel context.CancelFunc
	wg       sync.WaitGroup
}

func NewService(cfg *Config, opts ...Option) (*Service, error) {
	if cfg == nil {
		return nil, oekiterrors.New("service: config must not be nil")
	}
	// 部署模式 fail-fast:显式 distributed/worker 的硬约束不满足时
	// 拒绝启动(LocalConfig 路径已在 Config.Validate 里校验过;
	// 壳层程序化构造 Config 也走同一口径)。
	if err := cfg.ValidateDeploymentMode(); err != nil {
		return nil, err
	}
	// 启动自检日志:mode / 关键开关 / 副本假设,便于运维核对。
	logx.Info("deployment mode",
		"mode", cfg.DeploymentMode(),
		"durable_queue", cfg.Sync.DurableQueue,
		"execute_jobs", cfg.Sync.ExecuteJobs == nil || *cfg.Sync.ExecuteJobs,
		"mirror_lock_fail_closed", cfg.Sync.MirrorLockFailClosed,
		"redis_configured", cfg.Redis.Addr != "",
		"replicas_assumption", map[string]string{
			model.DeployModeStandalone:  "1 (single instance)",
			model.DeployModeDistributed: ">=1 (shared MySQL/Redis, queue + fail-closed locks)",
			model.DeployModeWorker:      ">=1 (queue consumers, no management API by shell convention)",
		}[cfg.DeploymentMode()])
	so := &serviceOptions{}
	for _, opt := range opts {
		opt(so)
	}

	// 外部注入 DB 时由调用方持有生命周期(Stop 不关闭)。
	db := so.db
	ownsDB := db == nil
	if ownsDB {
		var err error
		db, err = initDB(cfg)
		if err != nil {
			return nil, err
		}
	}
	if err := ensureTempDir(cfg.Git.TempDir); err != nil {
		return nil, err
	}
	repositories, err := initRepositories(db)
	if err != nil {
		return nil, err
	}

	repoService := NewRepoService(repositories.repo, repositories.platform, repositories.provider)
	taskService := NewTaskService(repositories.task, repositories.run, repositories.runStep, repositories.repo)
	webhookService := NewWebhookService(repositories.rule, repositories.event, repositories.repo)
	platformService := NewPlatformService(repositories.platform, repositories.repo, repositories.provider)
	opLogService := NewOperationLogService(repositories.opLog)
	// provider 生命周期钩子随实例走(WithProviderHooks),构造 provider 时带上。
	repoService.hooks = so.providerHooks
	platformService.hooks = so.providerHooks

	bgCtx, bgCancel := context.WithCancel(context.Background())
	repositories.provider.StartJanitor(bgCtx, 10*time.Minute)

	svc := &Service{
		config:            cfg,
		db:                db,
		ownsDB:            ownsDB,
		providerHooks:     so.providerHooks,
		providers:         repositories.provider,
		repos:             repoService,
		tasks:             taskService,
		webhooks:          webhookService,
		platforms:         platformService,
		opLogs:            opLogService,
		bgCtx:             bgCtx,
		bgCancel:          bgCancel,
		pending:           repositories.pending,
		outbox:            repositories.outbox,
		jobs:              repositories.jobs,
		retrySchedules:    repositories.retry,
		migrationMappings: repositories.migration,
		instanceID:        newInstanceID(),
		reconciles:        repositories.reconcile,
		recovery:          repositories.recovery,
		shutdown: lifecycle.New(10*time.Second,
			lifecycle.WithLogger(slog.New(logx.SlogHandler(logx.Default())))),
	}
	// sqlDB 仅在 Close 时用到
	if sqlDB, err := db.DB(); err == nil {
		svc.sqlDB = sqlDB
	}

	guard, err := newGuard(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB, cfg.Sync.MaxConcurrent, lock.RedisPoolOptions{
		PoolSize:        cfg.Redis.PoolSize,
		MinIdleConns:    cfg.Redis.MinIdleConns,
		DialTimeoutSec:  cfg.Redis.DialTimeoutSec,
		ReadTimeoutSec:  cfg.Redis.ReadTimeoutSec,
		WriteTimeoutSec: cfg.Redis.WriteTimeoutSec,
	})
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init concurrency guard failed")
	}
	svc.guard = guard

	// 同步策略模板库：表存储（templates）；旧 data/templates.json 仅作历史迁移源。
	svc.templates = &dbTemplateStore{repo: repositories.template}

	// force-push 审批：表存储（force_push_approvals）。
	svc.approvals = NewForcePushStore(repositories.approv)
	svc.retryTracker = NewRetryTracker(0)
	// 重试预算按「连续 auto_retry 链」从 DB 派生:重启不丢、多实例一致。
	svc.retryTracker.SetChainStats(repositories.run.AutoRetryChainStats)
	// #39 冷却期不吞预算:延后判定落 retry_schedules,由延迟重跑派发器
	// 到点认领触发(重启/多实例都生效)。
	svc.retryTracker.SetRetryScheduler(svc.scheduleDeferredRetry)

	// outbox 接线:运行完成与 outbox 事件同事务落库(见 TaskService.CompleteRun*)。
	taskService.outbox = repositories.outbox

	exec, err := executor.NewExecutor(svc)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init executor failed")
	}
	svc.executor = exec
	// 默认用 core 自带的审批存储；WithForcePushApprover 可覆盖（外部审批系统）。
	if so.forcePushApprover != nil {
		exec.Approver = so.forcePushApprover
	} else {
		exec.Approver = svc.approvals
	}
	// 恢复点索引:冷备成功后记录(P3 可恢复性)。
	exec.Recovery = svc

	mirrorSvc, err := NewMirrorService(svc, db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init mirror service failed")
	}
	svc.mirror = mirrorSvc
	historySvc, err := NewHistoryService(svc, db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init history service failed")
	}
	svc.history = historySvc

	// 优雅关停注册(oekit/core/lifecycle):按注册顺序执行 ——
	// 先等后台 goroutine 收敛,再依次关闭连接类资源。
	svc.shutdown.Add("background-goroutines", func(ctx context.Context) error {
		done := make(chan struct{})
		go func() {
			svc.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, 10*time.Second)
	if mirrorSvc.lockClient != nil {
		lc := mirrorSvc.lockClient
		svc.shutdown.Add("mirror-redis-lock", func(ctx context.Context) error {
			return lc.Close(ctx)
		}, 5*time.Second)
	}
	if historySvc.lockClient != nil {
		lc := historySvc.lockClient
		svc.shutdown.Add("history-redis-lock", func(ctx context.Context) error {
			return lc.Close(ctx)
		}, 5*time.Second)
	}
	if svc.guard != nil {
		svc.shutdown.AddCloser("concurrency-guard", svc.guard, 5*time.Second)
	}
	if svc.sqlDB != nil && svc.ownsDB {
		svc.shutdown.AddCloser("database", svc.sqlDB, 5*time.Second)
	}

	// **启动时回收卡在 processing 的 webhook 事件**:
	// 进程刚起,不可能有正在处理的事件 ⇒ 此刻仍是 processing 的,
	// 都是上次进程没走完的(被 SIGKILL / 崩溃 / goroutine panic)。
	// 不回收 ⇒ 它们永远停在这个状态:既没有失败记录(看不出"这次没同步"),
	// 也没有 pending 可补 ⇒ 这次推送的提交静默丢失。
	if n, rerr := webhookService.RecoverStuckEvents(context.Background(), 0); rerr != nil {
		logx.Warn("回收卡住的 webhook 事件失败", "error", rerr)
	} else if n > 0 {
		logx.Info("已回收卡住的 webhook 事件", "count", n)
	}

	// **启动扫描待补跑触发**:上次进程退出(SIGKILL/崩溃)前没补跑的 webhook
	// 触发落库存活,启动即认领补跑 —— 不扫描就要等下一次任意执行结束才补上。
	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.sweepPendingTriggersOnStartup()
	})

	// **启动扫描 outbox**:上次进程崩溃在「run 落库后、事件投递前」的事件
	// 未标记已发布,启动即零宽限重投 —— 通知与自动补偿最终送达(at-least-once)。
	svc.publishDueOutbox(0)
	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.dispatchOutboxLoop()
	})

	// #39 延迟重跑派发器:retry_schedules 到点后由任意实例认领触发。
	// 不依赖本地定时器 ⇒ 重启后延迟重试仍生效,多实例单胜者。
	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.retryDispatchLoop()
	})

	// P2 持久化执行队列:本实例负责执行时启动派发器与 reaper;
	// execute_jobs=false 的纯 API 实例只入队(由 Worker 实例认领执行)。
	if svc.durableQueueOn() && svc.executesJobs() {
		logx.Info("durable execution queue enabled", "instance", svc.instanceID,
			"dispatch_interval", jobDispatchInterval, "lease", jobLeaseTTL)
		svc.wg.Add(1)
		async.GoSafe(func() {
			defer svc.wg.Done()
			svc.dispatchJobLoop()
		})
		svc.wg.Add(1)
		async.GoSafe(func() {
			defer svc.wg.Done()
			svc.reaperJobLoop()
		})
	}

	// P3 周期对账:interval>0 时启动(结果落库 + 漂移写 outbox)。
	if svc.reconcileEnabled() {
		svc.wg.Add(1)
		async.GoSafe(func() {
			defer svc.wg.Done()
			svc.reconcileLoop()
		})
	}

	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.cleanupTriggerTimes()
	})
	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.recoverStuckWebhookEvents()
	})
	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.cleanupOldHistory()
	})
	svc.wg.Add(1)
	async.GoSafe(func() {
		defer svc.wg.Done()
		svc.recoverStuckMirrorRuns()
	})
	svc.startLifecycleJobs()

	return svc, nil
}

func (s *Service) Start() error {
	return s.startCronJobs()
}

func (s *Service) Stop() {
	s.stopOnce.Do(s.doStop)
}

func (s *Service) doStop() {
	// 取消后台 ctx,再停调度器;资源关闭/后台收敛统一交给 lifecycle.Manager。
	s.bgCancel()
	s.stopCronJobs()
	if s.shutdown != nil {
		s.shutdown.Shutdown(context.Background())
	}
}

// backendType 返回配置的 git 后端类型(native|gogit);空=自动选择(native 优先)。
//
// 镜像发布 / 历史推送这两条路径此前硬编码 Options{} ⇒ 实际走 native,
// 与全系统其余部分(按 cfg.Git.Backend 建)不是同一个后端 ——
// 同一份配置下两套后端,行为差异(可改写判定、超时、错误类型)就不可比了。
func (s *Service) backendType() string {
	if s == nil || s.config == nil {
		return ""
	}
	return s.config.Git.Backend
}

func (s *Service) GetTempDir(taskKey string) string {
	return filepath.Join(s.config.Git.TempDir, taskKey)
}

// GetRepoDir 返回任务的 git 工作区路径(<tempDir>/<taskKey>/repo)。
//
// GetTempDir 返回的是**父目录**,真正的 git 仓库在 repo 子目录里 ——
// 把父目录当仓库用会得到 "repository does not exist"。
func (s *Service) GetRepoDir(taskKey string) string {
	return filepath.Join(s.GetTempDir(taskKey), model.RepoDirName)
}

func (s *Service) GetConfig() *model.Config {
	return s.config
}

// Mirror 返回镜像通道服务(开源发布/仓库备份)。
func (s *Service) Mirror() *MirrorService {
	return s.mirror
}

// CreateRun creates a new sync run record. Satisfies executor.RunManager.
func (s *Service) CreateRun(task *model.SyncTask, trigger string, webhookEventID *uint) (*model.SyncRun, error) {
	return s.tasks.CreateRun(task, trigger, webhookEventID)
}

// CreateRunStep creates a new sync run step record. Satisfies executor.RunManager.
func (s *Service) CreateRunStep(step *model.SyncRunStep) error {
	return s.tasks.CreateRunStep(step)
}

// UpdateRunStep updates an existing sync run step record. Satisfies executor.RunManager.
func (s *Service) UpdateRunStep(step *model.SyncRunStep) error {
	return s.tasks.UpdateRunStep(step)
}

// CompleteRun updates a sync run with final status and details. Satisfies executor.RunManager.
func (s *Service) CompleteRun(run *model.SyncRun) error {
	return s.tasks.CompleteRun(run)
}

// UpdateTaskLastRun updates the task's last run status. Satisfies executor.RunManager.
func (s *Service) UpdateTaskLastRun(task *model.SyncTask, run *model.SyncRun) error {
	return s.tasks.UpdateTaskLastRun(task, run)
}

// CompleteRunWithTaskUpdate 在同一事务中完成 run + task 更新. Satisfies executor.RunManager.
func (s *Service) CompleteRunWithTaskUpdate(run *model.SyncRun, task *model.SyncTask) error {
	return s.tasks.CompleteRunWithTaskUpdate(run, task)
}

// GetRepoByKey returns a repository by key. Satisfies executor.RepoProvider.
func (s *Service) GetRepoByKey(key string) (*model.Repo, error) {
	return s.repos.GetRepoByKey(key)
}

// GetPlatformByID returns a platform by ID. Satisfies executor.PlatformProvider.
func (s *Service) GetPlatformByID(ctx context.Context, id uint) (*model.Platform, error) {
	return s.platforms.GetPlatformByID(ctx, id)
}

// SetForcePushApprover 注入 force-push 审批器（壳层 force-push-approvals）。
// block 策略触发分歧时：已放行则继续，否则登记 pending 并拒绝。
func (s *Service) SetForcePushApprover(a executor.ForcePushApprover) {
	if s.executor != nil {
		s.executor.Approver = a
	}
}

// HealthCheck checks the health of all dependencies.
// Returns a map of component name to "ok" or error message.
// 内部对每个组件设置 5s 超时:基础设施卡住时健康检查不会无限阻塞。
func (s *Service) HealthCheck() map[string]string {
	status := map[string]string{
		"database": "ok",
		"redis":    "ok",
		"service":  "ok",
	}

	// Check database connectivity with timeout.
	// sqlDB 在 db.DB() 失败时为空 —— 这里必须显式降级，不能直接 Ping 触发 panic。
	if s.sqlDB == nil {
		status["database"] = "unhealthy: sql.DB unavailable"
	} else {
		dbDone := make(chan error, 1)
		async.GoSafe(func() { dbDone <- s.sqlDB.Ping() })
		select {
		case err := <-dbDone:
			if err != nil {
				logx.Error("healthcheck: db ping failed", "error", err)
				status["database"] = "unhealthy"
			}
		case <-time.After(5 * time.Second):
			logx.Error("healthcheck: db ping timed out")
			status["database"] = "timeout"
		}
	}

	// Check Redis connectivity (if configured) with timeout
	if s.config.Redis.Addr != "" {
		redisDone := make(chan error, 1)
		async.GoSafe(func() { redisDone <- s.guard.Ping() })
		select {
		case err := <-redisDone:
			if err != nil {
				logx.Error("healthcheck: redis ping failed", "error", err)
				status["redis"] = "unhealthy"
			}
		case <-time.After(5 * time.Second):
			logx.Error("healthcheck: redis ping timed out")
			status["redis"] = "timeout"
		}
	} else {
		status["redis"] = "not configured"
	}

	// 刷新 provider 缓存指标(命中/未命中/淘汰/条目数)。挂在健康检查上,
	// k8s 探针与 /health 调用会持续更新,不需要额外后台循环。
	if s.providers != nil {
		st := s.providers.Stats()
		providerCacheHits.Set(float64(st.Hits))
		providerCacheMisses.Set(float64(st.Misses))
		providerCacheEvictions.Set(float64(st.Evictions))
		providerCacheSize.Set(float64(st.Size))
	}

	return status
}

// Platform related methods

// CreatePlatform 创建平台
func (s *Service) CreatePlatform(ctx context.Context, platform *model.Platform) error {
	return s.platforms.CreatePlatform(ctx, platform)
}

// GetPlatform 获取平台
func (s *Service) GetPlatform(ctx context.Context, key string) (*model.Platform, error) {
	return s.platforms.GetPlatform(ctx, key)
}

// ListPlatforms 列出所有平台
func (s *Service) ListPlatforms(ctx context.Context) ([]*model.Platform, error) {
	return s.platforms.ListPlatforms(ctx)
}

// UpdatePlatform 更新平台
func (s *Service) UpdatePlatform(ctx context.Context, platform *model.Platform) error {
	return s.platforms.UpdatePlatform(ctx, platform)
}

// DeletePlatform 删除平台
func (s *Service) DeletePlatform(ctx context.Context, key string) error {
	return s.platforms.DeletePlatform(ctx, key)
}

// SetDefaultPlatform 设置默认平台
func (s *Service) SetDefaultPlatform(ctx context.Context, key string) error {
	return s.platforms.SetDefaultPlatform(ctx, key)
}

// UpdatePlatformStatus 更新平台状态
func (s *Service) UpdatePlatformStatus(ctx context.Context, key, status, testResult string) error {
	return s.platforms.UpdatePlatformStatus(ctx, key, status, testResult)
}

// TestPlatformConnection 测试平台连接
func (s *Service) TestPlatformConnection(ctx context.Context, key string) (*sdkmodel.TestConnectionResult, error) {
	return s.platforms.TestPlatformConnection(ctx, key)
}

// ListPlatformRepos 列出平台上的仓库
func (s *Service) ListPlatformRepos(ctx context.Context, key, page, perPage string) ([]*sdkmodel.PlatformRepo, error) {
	return s.platforms.ListPlatformRepos(ctx, key, page, perPage)
}

// SyncPlatformRepos 同步平台仓库到本地
func (s *Service) SyncPlatformRepos(ctx context.Context, key string) (int, error) {
	return s.platforms.SyncPlatformRepos(ctx, key)
}

// SyncPlatformReposFiltered 按过滤条件导入平台仓库。
func (s *Service) SyncPlatformReposFiltered(ctx context.Context, key string, filter *RepoImportFilter) (int, error) {
	return s.platforms.SyncPlatformReposFiltered(ctx, key, filter)
}

// ListReposByPlatform 列出平台下的仓库
func (s *Service) ListReposByPlatform(ctx context.Context, platformKey string) ([]*model.Repo, error) {
	return s.platforms.ListReposByPlatform(ctx, platformKey)
}

// CountReposByPlatform 统计平台下的仓库数量
func (s *Service) CountReposByPlatform(ctx context.Context, platformKey string) (int64, error) {
	return s.platforms.CountReposByPlatform(ctx, platformKey)
}

// Operation log (audit) related methods

// RecordOperation 记录一条审计日志。
func (s *Service) RecordOperation(ctx context.Context, entry *model.OperationLog) error {
	return s.opLogs.Record(ctx, entry)
}

// ListOperations 按过滤条件分页返回审计日志。
func (s *Service) ListOperations(ctx context.Context, offset, limit int, filter *repository.OperationLogFilter) ([]*model.OperationLog, int64, error) {
	return s.opLogs.List(ctx, offset, limit, filter)
}

// OperationStats 返回今日、本周、总操作数。
func (s *Service) OperationStats(ctx context.Context) (today, week, total int64, err error) {
	return s.opLogs.Stats(ctx)
}

// VerifyAuditChain 校验审计哈希链。
func (s *Service) VerifyAuditChain() (*AuditChainResult, error) {
	return s.opLogs.VerifyAuditChain()
}
