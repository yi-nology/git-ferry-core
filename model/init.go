package model

import (
	"log"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/tpl"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// allModels 全部业务模型的 schema 基线(AutoMigrate 时代的事实源)。
//
// **纪律**:版本化迁移(见 migrate.go)启用后,新 schema 变更 = 先在这里
// 登记模型字段(代码可用)+ 在 migrations 里写显式 DDL(存量库生效),
// 两者缺一不可——只改模型会让存量库缺列,只写迁移会让新装模型代码对不上。
var allModels = []any{
	&Platform{},
	&Repo{},
	&SyncTask{},
	&SyncRun{},
	&SyncRunStep{},
	&WebhookRule{},
	&WebhookRuleTask{},
	&WebhookEvent{},
	&OperationLog{},
	&MirrorChannel{},
	&MirrorTarget{},
	&MirrorRun{},
	&HistoryChannel{},
	&HistoryTarget{},
	&HistoryRun{},
	&HistoryOverride{},
	&tpl.Template{},
	&ForcePushApproval{},
	&PendingTrigger{},
	&OutboxEvent{},
}

func InitDB(driver, dsn string) (*gorm.DB, error) {
	// 慢查询日志:超过 500ms 的查询记录为 Warn 级别
	customLogger := logger.New(log.New(os.Stdout, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold: 500 * time.Millisecond,
		LogLevel:      logger.Warn,
		Colorful:      false,
	})
	gormCfg := &gorm.Config{Logger: customLogger}

	var db *gorm.DB
	var err error
	switch driver {
	case DriverMySQL:
		db, err = gorm.Open(mysql.Open(dsn), gormCfg)
	case DriverSQLite:
		db, err = gorm.Open(sqlite.Open(dsn), gormCfg)
	default:
		return nil, oekiterrors.Errorf("unsupported driver: %s", driver)
	}
	if err != nil {
		return nil, err
	}

	// schema 迁移:基线(AutoMigrate)+ 版本化迁移,见 migrate.go。
	if err := ensureSchema(db, driver); err != nil {
		return nil, oekiterrors.Wrap(err, "schema migration failed")
	}
	return db, nil
}
