package service

import (
	"os"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"
)

// appDAOs 一次装配的全部 DAO(减少 NewService 样板,便于测试注入)。
type appDAOs struct {
	repo     *dao.RepoDAO
	task     *dao.SyncTaskDAO
	run      *dao.SyncRunDAO
	runStep  *dao.SyncRunStepDAO
	rule     *dao.WebhookRuleDAO
	event    *dao.WebhookEventDAO
	platform *dao.PlatformDAO
	opLog    *dao.OperationLogDAO
	template *dao.TemplateDAO
	approv   *dao.ForcePushApprovalDAO
	provider *sdkprov.Manager
}

// initDB 初始化 GORM 与连接池。
func initDB(cfg *Config) (*gorm.DB, error) {
	db, err := model.InitDB(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		return nil, errors.Wrap(err, "init db failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.Database.ConnMaxLifeSec) * time.Second)
	sqlDB.SetConnMaxIdleTime(time.Duration(cfg.Database.ConnMaxIdleSec) * time.Second)
	return db, nil
}

// initDAOs 装配 DAO 与平台 Provider 缓存。
func initDAOs(db *gorm.DB) (*appDAOs, error) {
	repoDAO, err := dao.NewRepoDAO(db)
	if err != nil {
		return nil, errors.Wrap(err, "init repo DAO failed")
	}
	platformDAO, err := dao.NewPlatformDAO(db)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create PlatformDAO")
	}
	return &appDAOs{
		repo:     repoDAO,
		task:     dao.NewSyncTaskDAO(db),
		run:      dao.NewSyncRunDAO(db),
		runStep:  dao.NewSyncRunStepDAO(db),
		rule:     dao.NewWebhookRuleDAO(db),
		event:    dao.NewWebhookEventDAO(db),
		platform: platformDAO,
		opLog:    dao.NewOperationLogDAO(db),
		template: dao.NewTemplateDAO(db),
		approv:   dao.NewForcePushApprovalDAO(db),
		provider: sdkprov.NewManager(30*time.Minute, sdkprov.WithMaxSize(64)),
	}, nil
}

// ensureTempDir 创建 git 工作目录。
func ensureTempDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return errors.Wrap(err, "create temp dir failed")
	}
	return nil
}
