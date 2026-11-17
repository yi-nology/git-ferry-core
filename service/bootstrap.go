package service

import (
	"os"
	"time"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
	"gorm.io/gorm"
)

// appRepositories 一次装配的全部 Repository(减少 NewService 样板,便于测试注入)。
type appRepositories struct {
	repo      *repository.RepoRepository
	task      *repository.SyncTaskRepository
	run       *repository.SyncRunRepository
	runStep   *repository.SyncRunStepRepository
	rule      *repository.WebhookRuleRepository
	event     *repository.WebhookEventRepository
	platform  *repository.PlatformRepository
	opLog     *repository.OperationLogRepository
	template  *repository.TemplateRepository
	approv    *repository.ForcePushApprovalRepository
	pending   *repository.PendingTriggerRepository
	outbox    *repository.OutboxEventRepository
	jobs      *repository.ExecutionJobRepository
	retry     *repository.RetryScheduleRepository
	migration *repository.MigrationMappingRepository
	reconcile *repository.ReconcileRepository
	recovery  *repository.RecoveryPointRepository
	provider  *sdkrt.Manager
}

// initDB 初始化 GORM 与连接池(gormx 接管 open/池,连接池参数在
// Open 时一次下发,避免"先开后调"的中间态)。
func initDB(cfg *Config) (*gorm.DB, error) {
	db, err := model.InitDBWithPool(cfg.Database.Driver, cfg.Database.DSN, model.DBPoolOptions{
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: time.Duration(cfg.Database.ConnMaxLifeSec) * time.Second,
		ConnMaxIdleTime: time.Duration(cfg.Database.ConnMaxIdleSec) * time.Second,
	})
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init db failed")
	}
	return db, nil
}

// initRepositories 装配 Repository 与平台 Provider 缓存。
func initRepositories(db *gorm.DB) (*appRepositories, error) {
	repoDAO, err := repository.NewRepoRepository(db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init repo Repository failed")
	}
	platformDAO, err := repository.NewPlatformRepository(db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "failed to create PlatformRepository")
	}
	return &appRepositories{
		repo:      repoDAO,
		task:      repository.NewSyncTaskRepository(db),
		run:       repository.NewSyncRunRepository(db),
		runStep:   repository.NewSyncRunStepRepository(db),
		rule:      repository.NewWebhookRuleRepository(db),
		event:     repository.NewWebhookEventRepository(db),
		platform:  platformDAO,
		opLog:     repository.NewOperationLogRepository(db),
		template:  repository.NewTemplateRepository(db),
		approv:    repository.NewForcePushApprovalRepository(db),
		pending:   repository.NewPendingTriggerRepository(db),
		outbox:    repository.NewOutboxEventRepository(db),
		jobs:      repository.NewExecutionJobRepository(db),
		retry:     repository.NewRetryScheduleRepository(db),
		migration: repository.NewMigrationMappingRepository(db),
		reconcile: repository.NewReconcileRepository(db),
		recovery:  repository.NewRecoveryPointRepository(db),
		provider:  sdkrt.NewManager(30*time.Minute, sdkrt.WithMaxSize(64)),
	}, nil
}

// ensureTempDir 创建 git 工作目录。
func ensureTempDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return oekiterrors.Wrap(err, "create temp dir failed")
	}
	return nil
}
