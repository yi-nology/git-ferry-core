package model

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// sqliteFileDB 用文件 DSN(:memory: 多连接会各自独立)打开库。
func sqliteFileDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	return db
}

// TestEnsureSchema_FreshInstallBaseline 全新安装:AutoMigrate 建齐 +
// 记录 baseline,二次启动幂等(不重复记录、不报错)。
func TestEnsureSchema_FreshInstallBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db := sqliteFileDB(t, path)

	require.NoError(t, ensureSchema(db, DriverSQLite))

	// 基线后全部业务表存在。
	for _, m := range allModels {
		assert.True(t, db.Migrator().HasTable(m), "fresh install 缺表: %T", m)
	}
	// baseline 已记录。
	var rows []SchemaMigration
	require.NoError(t, db.Order("version").Find(&rows).Error)
	// baseline + v2(execution_jobs) + v3(reconcile_results) + v4(recovery_points)
	require.Len(t, rows, 4)
	assert.Equal(t, BaselineVersion, rows[0].Version)
	assert.Equal(t, uint64(2), rows[1].Version)
	assert.Equal(t, uint64(3), rows[2].Version)
	assert.Equal(t, uint64(4), rows[3].Version)
	assert.True(t, db.Migrator().HasTable(&ExecutionJob{}), "v2 迁移必须建 execution_jobs")
	assert.True(t, db.Migrator().HasTable(&ReconcileRecord{}), "v3 迁移必须建 reconcile_results")
	assert.True(t, db.Migrator().HasTable(&RecoveryPoint{}), "v4 迁移必须建 recovery_points")

	// 二次启动幂等:不再记录、不再 AutoMigrate 也不报错。
	require.NoError(t, ensureSchema(db, DriverSQLite))
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 4, "已应用版本只能记录一次")
}

// TestEnsureSchema_LegacyConverge 存量库(无 schema_migrations,只有部分旧表):
// 首次收敛 = AutoMigrate 补齐 + 记 baseline,与旧版本每次启动行为等价(零风险)。
func TestEnsureSchema_LegacyConverge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db := sqliteFileDB(t, path)

	// 模拟旧库:只有早期几张表。
	require.NoError(t, db.AutoMigrate(&SyncTask{}, &SyncRun{}))
	require.NoError(t, db.Exec("INSERT INTO sync_tasks (key, name, source_branch, target_branch, enabled) VALUES ('legacy-task', 'legacy', 'main', 'main', 1)").Error)

	require.NoError(t, ensureSchema(db, DriverSQLite))

	// 旧数据还在,新表补齐。
	assert.True(t, db.Migrator().HasTable(&PendingTrigger{}))
	assert.True(t, db.Migrator().HasTable(&OutboxEvent{}))
	var cnt int64
	require.NoError(t, db.Model(&SyncTask{}).Where("key='legacy-task'").Count(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "存量数据必须原样保留")

	var rows []SchemaMigration
	require.NoError(t, db.Order("version").Find(&rows).Error)
	// baseline + v2 + v3 + v4 在存量库上补建
	require.Len(t, rows, 4)
	assert.Equal(t, BaselineVersion, rows[0].Version)
	assert.True(t, db.Migrator().HasTable(&ExecutionJob{}), "存量库必须经 v2 迁移补建 execution_jobs")
	assert.True(t, db.Migrator().HasTable(&ReconcileRecord{}), "存量库必须经 v3 迁移补建 reconcile_results")
	assert.True(t, db.Migrator().HasTable(&RecoveryPoint{}), "存量库必须经 v4 迁移补建 recovery_points")
}

// TestEnsureSchema_AppliesPendingMigrations baseline 之后只补跑未应用的显式迁移,
// 逐条记录版本;重复启动跳过已应用版本。
func TestEnsureSchema_AppliesPendingMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mig.db")
	db := sqliteFileDB(t, path)
	require.NoError(t, ensureSchema(db, DriverSQLite)) // baseline

	// 登记两条测试迁移(v2 加索引、v3 加列),用后即焚。
	orig := migrations
	defer func() { migrations = orig }()
	migrations = append(migrations,
		Migration{Version: 90001, Name: "test: add idx on sync_runs.status",
			Apply: func(tx *gorm.DB) error {
				return tx.Exec("CREATE INDEX idx_mig_test_status ON sync_runs (status)").Error
			}},
		Migration{Version: 90002, Name: "test: add mig_test_col",
			Apply: func(tx *gorm.DB) error {
				return tx.Exec("ALTER TABLE sync_runs ADD COLUMN mig_test_col TEXT").Error
			}},
	)

	require.NoError(t, ensureSchema(db, DriverSQLite))

	assert.True(t, db.Migrator().HasIndex(&SyncRun{}, "idx_mig_test_status"), "v2 迁移未生效")
	hasCol, err := columnExists(db, "sync_runs", "mig_test_col")
	require.NoError(t, err)
	assert.True(t, hasCol, "v3 迁移未生效")

	var rows []SchemaMigration
	require.NoError(t, db.Order("version").Find(&rows).Error)
	require.Len(t, rows, 6, "baseline + 真实v2/v3/v4 + 测试v90001/v90002 都应记录")
	assert.Equal(t, uint64(90002), rows[5].Version)

	// 重复启动:v2/v3 不得重放(幂等;重放 ALTER TABLE 会炸,天然可验)。
	require.NoError(t, ensureSchema(db, DriverSQLite))
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 6, "已应用迁移不得重复记录/重复执行")
}

// TestEnsureSchema_MigrationFailureStops 迁移失败:该版本不记录,
// 下次启动会重试(不静默跳过)。
func TestEnsureSchema_MigrationFailureStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migfail.db")
	db := sqliteFileDB(t, path)
	require.NoError(t, ensureSchema(db, DriverSQLite))

	orig := migrations
	defer func() { migrations = orig }()
	migrations = append(migrations,
		Migration{Version: 90009, Name: "test: always fail",
			Apply: func(tx *gorm.DB) error {
				return gorm.ErrInvalidTransaction // 任意错误
			}},
		Migration{Version: 90010, Name: "test: after failure",
			Apply: func(tx *gorm.DB) error {
				return tx.Exec("CREATE INDEX idx_mig_after ON sync_runs (status)").Error
			}},
	)

	require.Error(t, ensureSchema(db, DriverSQLite))

	var rows []SchemaMigration
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 4, "失败的迁移不得记录版本(baseline+真实v2/v3/v4 在)")
	assert.False(t, db.Migrator().HasIndex(&SyncRun{}, "idx_mig_after"),
		"失败迁移之后的版本不得继续执行(顺序性)")
}

// columnExists sqlite pragma 查列(跨方言测试辅助)。
func columnExists(db *gorm.DB, table, col string) (bool, error) {
	type colInfo struct {
		Name string `gorm:"column:name"`
	}
	var cols []colInfo
	if err := db.Raw("PRAGMA table_info(" + table + ")").Scan(&cols).Error; err != nil {
		return false, err
	}
	for _, c := range cols {
		if c.Name == col {
			return true, nil
		}
	}
	return false, nil
}

// TestRecordMigration_Idempotent 版本记录幂等(并发/重入安全)。
func TestRecordMigration_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.db")
	db := sqliteFileDB(t, path)
	require.NoError(t, db.AutoMigrate(&SchemaMigration{}))

	require.NoError(t, recordMigration(db, 1, "a"))
	require.NoError(t, recordMigration(db, 1, "a"))
	var n int64
	require.NoError(t, db.Model(&SchemaMigration{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}
