package service

import (
	"os"
	"time"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repo"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
	"gorm.io/gorm"
)

// appRepos 一次装配的全部 Repo(减少 NewService 样板,便于测试注入)。
type appRepos struct {
	repo      *repo.RepoRepo
	task      *repo.SyncTaskRepo
	run       *repo.SyncRunRepo
	runStep   *repo.SyncRunStepRepo
	rule      *repo.WebhookRuleRepo
	event     *repo.WebhookEventRepo
	platform  *repo.PlatformRepo
	opLog     *repo.OperationLogRepo
	template  *repo.TemplateRepo
	approv    *repo.ForcePushApprovalRepo
	pending   *repo.PendingTriggerRepo
	outbox    *repo.OutboxEventRepo
	jobs      *repo.ExecutionJobRepo
	reconcile *repo.ReconcileRepo
	recovery  *repo.RecoveryPointRepo
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

// initRepos 装配 Repo 与平台 Provider 缓存。
func initRepos(db *gorm.DB) (*appRepos, error) {
	repoRepo, err := repo.NewRepoRepo(db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init repo Repo failed")
	}
	platformRepo, err := repo.NewPlatformRepo(db)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "failed to create PlatformRepo")
	}
	return &appRepos{
		repo:      repoRepo,
		task:      repo.NewSyncTaskRepo(db),
		run:       repo.NewSyncRunRepo(db),
		runStep:   repo.NewSyncRunStepRepo(db),
		rule:      repo.NewWebhookRuleRepo(db),
		event:     repo.NewWebhookEventRepo(db),
		platform:  platformRepo,
		opLog:     repo.NewOperationLogRepo(db),
		template:  repo.NewTemplateRepo(db),
		approv:    repo.NewForcePushApprovalRepo(db),
		pending:   repo.NewPendingTriggerRepo(db),
		outbox:    repo.NewOutboxEventRepo(db),
		jobs:      repo.NewExecutionJobRepo(db),
		reconcile: repo.NewReconcileRepo(db),
		recovery:  repo.NewRecoveryPointRepo(db),
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
