package model

import (
	"context"
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
	// + v5(policy_snapshot) + v6(defer_count) + v7/v8/v9(generation coalescing
	// 三步走) + v10(lease_version) + v11(next_fire_at) + v12(retry_schedules)
	// + v13(migration_mappings)
	require.Len(t, rows, 15)
	assert.Equal(t, BaselineVersion, rows[0].Version)
	assert.Equal(t, uint64(2), rows[1].Version)
	assert.Equal(t, uint64(3), rows[2].Version)
	assert.Equal(t, uint64(4), rows[3].Version)
	assert.Equal(t, uint64(5), rows[4].Version)
	assert.Equal(t, uint64(6), rows[5].Version)
	assert.Equal(t, uint64(7), rows[6].Version)
	assert.Equal(t, uint64(8), rows[7].Version)
	assert.Equal(t, uint64(9), rows[8].Version)
	assert.Equal(t, uint64(10), rows[9].Version)
	assert.True(t, db.Migrator().HasTable(&ExecutionJob{}), "v2 迁移必须建 execution_jobs")
	assert.True(t, db.Migrator().HasTable(&ReconcileRecord{}), "v3 迁移必须建 reconcile_results")
	assert.True(t, db.Migrator().HasTable(&RecoveryPoint{}), "v4 迁移必须建 recovery_points")
	assert.True(t, db.Migrator().HasColumn(&SyncRun{}, "policy_snapshot"), "v5 迁移必须加 policy_snapshot 列")
	assert.True(t, db.Migrator().HasColumn(&ExecutionJob{}, "defer_count"), "v6 迁移必须加 defer_count 列")
	assert.True(t, db.Migrator().HasTable(&ExecutionTaskState{}), "v7 迁移必须建 execution_task_states")
	assert.True(t, db.Migrator().HasColumn(&ExecutionJob{}, "generation"), "v7 迁移必须加 generation 列")
	assert.True(t, db.Migrator().HasColumn(&ExecutionJob{}, "active_key"), "v7 迁移必须加 active_key 列")
	assert.True(t, db.Migrator().HasIndex(&ExecutionJob{}, "uk_execution_jobs_active_slot"),
		"v7 迁移必须建活动槽唯一索引")
	assert.True(t, db.Migrator().HasColumn(&ExecutionJob{}, "lease_version"), "v10 迁移必须加 lease_version 列")
	assert.True(t, db.Migrator().HasColumn(&SyncTask{}, "next_fire_at"), "v11 迁移必须加 next_fire_at 列")
	assert.True(t, db.Migrator().HasTable(&RetrySchedule{}), "v12 迁移必须建 retry_schedules")
	assert.True(t, db.Migrator().HasTable(&MigrationMapping{}), "v13 迁移必须建 migration_mappings")
	assert.True(t, db.Migrator().HasIndex(&ExecutionJob{}, "idx_execution_jobs_status_lease"),
		"v14 迁移必须建 execution_jobs(status,lease_until) 索引")
	assert.True(t, db.Migrator().HasIndex(&OutboxEvent{}, "idx_outbox_events_pending"),
		"v14 迁移必须建 outbox_events(published_at,created_at) 索引")
	assert.True(t, db.Migrator().HasIndex(&RetrySchedule{}, "idx_retry_schedules_due"),
		"v14 迁移必须建 retry_schedules(status,next_attempt_at) 索引")

	// 二次启动幂等:不再记录、不再 AutoMigrate 也不报错。
	require.NoError(t, ensureSchema(db, DriverSQLite))
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 15, "已应用版本只能记录一次")
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
	// baseline + v2..v15 在存量库上补建
	require.Len(t, rows, 15)
	assert.Equal(t, BaselineVersion, rows[0].Version)
	assert.True(t, db.Migrator().HasTable(&ExecutionJob{}), "存量库必须经 v2 迁移补建 execution_jobs")
	assert.True(t, db.Migrator().HasTable(&ReconcileRecord{}), "存量库必须经 v3 迁移补建 reconcile_results")
	assert.True(t, db.Migrator().HasTable(&RecoveryPoint{}), "存量库必须经 v4 迁移补建 recovery_points")
	assert.True(t, db.Migrator().HasColumn(&SyncRun{}, "policy_snapshot"), "存量库必须经 v5 迁移加 policy_snapshot")
	assert.True(t, db.Migrator().HasColumn(&ExecutionJob{}, "defer_count"), "存量库必须经 v6 迁移加 defer_count")
	assert.True(t, db.Migrator().HasTable(&ExecutionTaskState{}), "存量库必须经 v7 迁移补建 execution_task_states")
	assert.True(t, db.Migrator().HasColumn(&ExecutionJob{}, "lease_version"), "存量库必须经 v10 迁移加 lease_version")
	assert.True(t, db.Migrator().HasColumn(&SyncTask{}, "next_fire_at"), "存量库必须经 v11 迁移加 next_fire_at")
	assert.True(t, db.Migrator().HasTable(&RetrySchedule{}), "存量库必须经 v12 迁移补建 retry_schedules")
	assert.True(t, db.Migrator().HasTable(&MigrationMapping{}), "存量库必须经 v13 迁移补建 migration_mappings")
	assert.True(t, db.Migrator().HasIndex(&ExecutionJob{}, "idx_execution_jobs_status_lease"),
		"存量库必须经 v14 迁移补建热路径索引")
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
	require.Len(t, rows, 17, "baseline + 真实v2..v15 + 测试v90001/v90002 都应记录")
	assert.Equal(t, uint64(90001), rows[15].Version)
	assert.Equal(t, uint64(90002), rows[16].Version)

	// 重复启动:v2/v3 不得重放(幂等;重放 ALTER TABLE 会炸,天然可验)。
	require.NoError(t, ensureSchema(db, DriverSQLite))
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 17, "已应用迁移不得重复记录/重复执行")
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
	assert.Len(t, rows, 15, "失败的迁移不得记录版本(baseline+真实v2..v15 在)")
	assert.False(t, db.Migrator().HasIndex(&SyncRun{}, "idx_mig_after"),
		"失败迁移之后的版本不得继续执行(顺序性)")
}

// TestEnsureSchema_V7BackfillsGenerationCoalescing 存量库升级(#36):
// 旧 schema 的 execution_jobs(无 generation/active_key 列、无状态行表)上有
// 活动作业(含同任务重复活动作业——旧 WHERE NOT EXISTS 在 MySQL RR 下的竞态
// 遗留),v7 必须:去重(多余活动作业判 failed)→ 建表/加列/建唯一索引 →
// 回填 generation=1、active_key=task_key、任务状态行(desired=1,活动槽指向
// 存活作业)。升级后队列语义无缝接管。
func TestEnsureSchema_V7BackfillsGenerationCoalescing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7legacy.db")
	db := sqliteFileDB(t, path)

	// 旧 schema(基线时代的 execution_jobs,没有新列)。
	require.NoError(t, db.Exec(`CREATE TABLE execution_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_key TEXT NOT NULL,
		"trigger" TEXT NOT NULL,
		webhook_event_id INTEGER,
		status TEXT NOT NULL,
		attempt INTEGER NOT NULL DEFAULT 0,
		defer_count INTEGER NOT NULL DEFAULT 0,
		run_id INTEGER,
		owner TEXT,
		lease_until DATETIME,
		error TEXT,
		queued_at DATETIME,
		claimed_at DATETIME,
		finished_at DATETIME
	)`).Error)
	// task-a:两条 pending 活动作业(竞态遗留,重复);task-b:一条 claimed。
	require.NoError(t, db.Exec(`INSERT INTO execution_jobs
		(task_key, "trigger", status, attempt, defer_count, owner, queued_at) VALUES
		('task-a', 'webhook', 'pending', 0, 0, '', '2026-01-01'),
		('task-a', 'webhook', 'pending', 0, 0, '', '2026-01-02'),
		('task-b', 'cron', 'claimed', 0, 0, 'inst-1', '2026-01-03')`).Error)
	require.NoError(t, db.AutoMigrate(&SchemaMigration{}))
	require.NoError(t, recordMigration(db, BaselineVersion, "legacy baseline"))

	require.NoError(t, ensureSchema(db, DriverSQLite))

	// 去重:task-a 只剩一条活动作业(id=1),id=2 判 failed。
	var dup ExecutionJob
	require.NoError(t, db.First(&dup, 2).Error)
	assert.Equal(t, JobStatusFailed, dup.Status)
	assert.Contains(t, dup.Error, "deduplicated by migration v7")

	// 回填:存活活动作业 generation=1、active_key=task_key。
	var a, b ExecutionJob
	require.NoError(t, db.First(&a, 1).Error)
	require.NoError(t, db.First(&b, 3).Error)
	assert.Equal(t, JobStatusPending, a.Status)
	assert.Equal(t, int64(1), a.Generation)
	require.NotNil(t, a.ActiveKey)
	assert.Equal(t, "task-a", *a.ActiveKey)
	assert.Equal(t, JobStatusClaimed, b.Status)
	assert.Equal(t, int64(1), b.Generation)
	require.NotNil(t, b.ActiveKey)
	assert.Equal(t, "task-b", *b.ActiveKey)

	// 任务状态行:desired=1、completed=0、活动槽指向存活作业。
	var sa, sb ExecutionTaskState
	require.NoError(t, db.Where("task_key = 'task-a'").First(&sa).Error)
	require.NoError(t, db.Where("task_key = 'task-b'").First(&sb).Error)
	assert.Equal(t, int64(1), sa.DesiredGeneration)
	assert.Zero(t, sa.CompletedGeneration)
	require.NotNil(t, sa.ActiveJobID)
	assert.Equal(t, uint(1), *sa.ActiveJobID)
	assert.Equal(t, int64(1), sb.DesiredGeneration)
	require.NotNil(t, sb.ActiveJobID)
	assert.Equal(t, uint(3), *sb.ActiveJobID)

	// 活动槽唯一索引已建,二次启动幂等。
	assert.True(t, db.Migrator().HasIndex(&ExecutionJob{}, "uk_execution_jobs_active_slot"))
	require.NoError(t, ensureSchema(db, DriverSQLite))
	var rows []SchemaMigration
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 15, "重复启动不得重放迁移")
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

// TestEnsureSchema_MySQLAdvisoryLockWrapsAllDDL #43 回归:
// MySQL 路径必须把**全部 DDL(含版本表自身)包进 RunWithAdvisoryLock
// 的回调**,而不是在锁外/池里另发 GET_LOCK/RELEASE_LOCK——后者会在
// 另一条池连接上解锁,锁只能等连接关闭才释放。
// 用 sqlite 引擎 + 伪造 driver 参数验证调用形态;真正的同连接语义由
// gormx.RunWithAdvisoryLock(db.Connection/*sql.Conn) 保证。
func TestEnsureSchema_MySQLAdvisoryLockWrapsAllDDL(t *testing.T) {
	db := sqliteFileDB(t, filepath.Join(t.TempDir(), "locked.db"))

	orig := runWithSchemaAdvisoryLock
	defer func() { runWithSchemaAdvisoryLock = orig }()

	called := 0
	runWithSchemaAdvisoryLock = func(_ context.Context, raw *gorm.DB, key int64, fn func(*gorm.DB) error) error {
		called++
		assert.Equal(t, schemaMigrateLockKey(), key, "锁键必须稳定(FNV-1a 业务键)")
		assert.False(t, raw.Migrator().HasTable(&SchemaMigration{}),
			"版本表 AutoMigrate 也必须在互斥区内(锁外 DDL 会与迁移抢表锁)")
		return fn(raw)
	}

	require.NoError(t, ensureSchema(db, DriverMySQL))
	assert.Equal(t, 1, called, "MySQL 路径必须且只能走一次 advisory lock 包裹")
	assert.True(t, db.Migrator().HasTable(&SchemaMigration{}))

	var rows []SchemaMigration
	require.NoError(t, db.Find(&rows).Error)
	assert.Len(t, rows, 15, "锁回调内必须完成 baseline + v2..v15")
}
