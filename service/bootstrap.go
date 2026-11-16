package service

import (
	"os"
	"time"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
	"gorm.io/gorm"
)

// appDAOs 一次装配的全部 DAO(减少 NewService 样板,便于测试注入)。
type appDAOs struct {
	repo      *dao.RepoDAO
	task      *dao.SyncTaskDAO
	run       *dao.SyncRunDAO
	runStep   *dao.SyncRunStepDAO
	rule      *dao.WebhookRuleDAO
	event     *dao.WebhookEventDAO
	platform  *dao.PlatformDAO
	opLog     *dao.OperationLogDAO
	template  *dao.TemplateDAO
	approv    *dao.ForcePushApprovalDAO
	pending   *dao.PendingTriggerDAO
	outbox    *dao.OutboxEventDAO
	jobs      *dao.ExecutionJobDAO
	retry     *dao.RetryScheduleDAO
	migration *dao.MigrationMappingDAO
	reconcile *dao.ReconcileDAO
	recovery  *dao.RecoveryPointDAO
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

// initDAOs 装配 DAO 与平台 Provider 缓存。
func initDAOs(db *gorm.DB) (*appDAOs, error) {
	repoDAO, err := dao.NewRepoDAO(db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init repo DAO failed")
	}
	platformDAO, err := dao.NewPlatformDAO(db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "failed to create PlatformDAO")
	}
	return &appDAOs{
		repo:      repoDAO,
		task:      dao.NewSyncTaskDAO(db),
		run:       dao.NewSyncRunDAO(db),
		runStep:   dao.NewSyncRunStepDAO(db),
		rule:      dao.NewWebhookRuleDAO(db),
		event:     dao.NewWebhookEventDAO(db),
		platform:  platformDAO,
		opLog:     dao.NewOperationLogDAO(db),
		template:  dao.NewTemplateDAO(db),
		approv:    dao.NewForcePushApprovalDAO(db),
		pending:   dao.NewPendingTriggerDAO(db),
		outbox:    dao.NewOutboxEventDAO(db),
		jobs:      dao.NewExecutionJobDAO(db),
		retry:     dao.NewRetryScheduleDAO(db),
		migration: dao.NewMigrationMappingDAO(db),
		reconcile: dao.NewReconcileDAO(db),
		recovery:  dao.NewRecoveryPointDAO(db),
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
