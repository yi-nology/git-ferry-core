package model

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/oekit/gormx"
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
	&ExecutionJob{},
	&ReconcileRecord{},
	&RecoveryPoint{},
}

// DBPoolOptions 连接池配置(零值 = database/sql 缺省;生产由 Config.Validate
// 填好默认值后传入)。
type DBPoolOptions struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

func InitDB(driver, dsn string) (*gorm.DB, error) {
	return InitDBWithPool(driver, dsn, DBPoolOptions{})
}

// InitDBWithPool 用 oekit/gormx 打开数据库:连接池配置由 gormx.Open 统一
// 接管(WithConnectionPool 逐项 set,未设置的项不下发零值;见上游 #229)。
func InitDBWithPool(driver, dsn string, pool DBPoolOptions) (*gorm.DB, error) {
	// 慢查询日志:超过 500ms 的查询记录为 Warn 级别。
	// Logger 仍用 gorm 原生适配器(输出到 stdout,不依赖 logx 默认后端),
	// gormx 只接管连接与池。
	customLogger := logger.New(log.New(os.Stdout, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold: 500 * time.Millisecond,
		LogLevel:      logger.Warn,
		Colorful:      false,
	})

	var dialector gorm.Dialector
	switch driver {
	case DriverMySQL:
		dialector = mysql.Open(dsn)
	case DriverSQLite:
		dialector = sqlite.Open(dsn)
	default:
		return nil, oekiterrors.New(fmt.Sprintf("unsupported driver: %s", driver))
	}

	opts := []gormx.OpenOption{gormx.WithLogger(customLogger)}
	if pool.MaxOpenConns > 0 || pool.MaxIdleConns > 0 || pool.ConnMaxLifetime > 0 {
		opts = append(opts, gormx.WithConnectionPool(pool.MaxOpenConns, pool.MaxIdleConns, pool.ConnMaxLifetime))
	}
	if pool.ConnMaxIdleTime > 0 {
		opts = append(opts, gormx.WithConnMaxIdleTime(pool.ConnMaxIdleTime))
	}

	db, err := gormx.Open(dialector, opts...)
	if err != nil {
		return nil, err
	}

	// schema 迁移:基线(AutoMigrate)+ 版本化迁移,见 migrate.go。
	if err := ensureSchema(db, driver); err != nil {
		return nil, oekiterrors.Wrap(err, "schema migration failed")
	}
	return db, nil
}
