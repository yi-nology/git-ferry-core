package model

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/oekit/gormx"
	oekiterrors "github.com/oekit/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ===== 版本化 schema 迁移 =====
//
// 背景(技术评审 P1):AutoMigrate 适合轻量/单实例部署,但多实例同时启动
// 会并发 DDL(GALTER 抢锁/竞态),且历史变更不可审计、不可重放。
//
// 策略(与评审推荐的取舍一致):
//  1. **全新安装**:AutoMigrate(allModels) 一次建齐当前 schema = 基线,
//     记入 schema_migrations(version=1),此后启动不再 AutoMigrate;
//  2. **存量库**(无 schema_migrations 记录):首次启动 AutoMigrate 收敛到
//     当前模型态(等价于过去的每次启动行为,零风险),记 baseline,之后同上;
//  3. **baseline 之后**:启动只按 schema_migrations 补跑未应用的显式迁移;
//  4. **多实例串行化**:MySQL 用 GET_LOCK 命名锁;sqlite 单写者天然串行。
//
// 新增 schema 变更的纪律(违反会在存量库上缺列):
//   a. allModels 的模型上加字段(新装/代码可用);
//   b. migrations 里追加一条 Migration{Version: 递增, Apply: 显式 DDL};
//   c. **已发布的 Migration 禁止修改**——只追加,不改写(重放安全性)。

// SchemaMigration 已应用的迁移记录(版本表)。
type SchemaMigration struct {
	Version   uint64    `json:"version" gorm:"primaryKey"`
	Name      string    `json:"name" gorm:"size:128;not null"`
	AppliedAt time.Time `json:"appliedAt"`
}

func (SchemaMigration) TableName() string { return "schema_migrations" }

// Migration 一次显式 schema 变更。Apply 在单事务内执行,成功后同事务记录版本。
type Migration struct {
	Version uint64
	Name    string
	Apply   func(*gorm.DB) error
}

// BaselineVersion = 1:AutoMigrate 时代结束、版本化时代的基线标记。
// 全新安装与存量库首次收敛都记到此版本;真实 DDL 迁移从 2 开始编号。
const BaselineVersion uint64 = 1

// schemaMigrateLockName 迁移互斥的业务键:串行化多实例启动时的迁移执行
// (sqlite 单写者无此问题)。
//
// **破坏性变更**:gormx 的咨询锁键是 int64(MySQL 侧即十进制串),
// 不再是旧版本的命名锁 "gitferry_schema_migrate" ⇒ 新旧版本在滚动升级
// 窗口内不互斥。升级时先停旧副本再启新副本(迁移本身幂等,风险可控)。
const schemaMigrateLockName = "gitferry_schema_migrate"

// schemaMigrateLockTimeout 拿锁等待上限:等不到 = 另一实例正在迁移,
// fail-closed 拒绝启动(宁可起不来,不可并发 DDL)。
const schemaMigrateLockTimeout = 60 * time.Second

// schemaMigrateLockKey 把业务键折成 gormx 会话级咨询锁的 int64 键
// (FNV-1a 64,与 gormx/migrate 的 LockKey 口径一致;同一字符串跨副本必得同一键)。
func schemaMigrateLockKey() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(schemaMigrateLockName))
	return int64(h.Sum64())
}

// migrations 已登记的版本化迁移(按 Version 升序;**只追加,不改写**)。
var migrations = []Migration{
	// v2 (2026-11-12, P2):持久化执行队列 execution_jobs。
	// 已在 baseline(v1)之前建成的库只记了 v1,baseline 之后启动不再
	// AutoMigrate ⇒ 必须显式补建(模型+迁移缺一不可的纪律实例)。
	{Version: 2, Name: "create execution_jobs (durable queue)",
		Apply: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&ExecutionJob{})
		}},
	// v3 (2026-11-12, P3):对账结果表 reconcile_results(每任务最新一条)。
	{Version: 3, Name: "create reconcile_results (drift records)",
		Apply: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&ReconcileRecord{})
		}},
	// v4 (2026-11-12, P3):恢复点索引 recovery_points(可恢复性独立验证)。
	{Version: 4, Name: "create recovery_points (restore index)",
		Apply: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&RecoveryPoint{})
		}},
	// v5 (2026-11-13, P3):sync_runs.policy_snapshot(执行时策略快照)。
	{Version: 5, Name: "add policy_snapshot to sync_runs",
		Apply: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&SyncRun{})
		}},
	// v6 (2026-11-13, #13):execution_jobs.defer_count —— 良性繁忙退避次数,
	// 与接管次数(attempt)分离记账(毒丸预算只留给崩溃接管)。已 baseline 的
	// 存量库没有该列,而 Enqueue/Reschedule 的 SQL 直接引用它 ⇒ 必须显式补列
	// (模型+迁移缺一不可的纪律)。
	{Version: 6, Name: "add defer_count to execution_jobs (benign busy backoff counter)",
		Apply: func(tx *gorm.DB) error {
			return tx.AutoMigrate(&ExecutionJob{})
		}},
}

// ensureSchema 启动时的 schema 收敛入口(见文件头策略)。
//
// 多实例串行化:MySQL 走 gormx.RunWithAdvisoryLock——GET_LOCK/RELEASE_LOCK
// 与迁移语句钉在同一条连接上(会话级锁的连接池坑由此闭合);sqlite 单写者
// 天然串行,不取锁。
func ensureSchema(db *gorm.DB, driver string) error {
	// 版本表自身幂等建表(先于锁,锁不需要它)。
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		return err
	}
	if driver != DriverMySQL {
		return ensureSchemaLocked(db)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schemaMigrateLockTimeout)
	defer cancel()
	return gormx.RunWithAdvisoryLock(ctx, db, schemaMigrateLockKey(), ensureSchemaLocked)
}

// ensureSchemaLocked 在迁移互斥区内执行版本收敛。传入的 db 在 MySQL 路径下
// 是**持锁连接**(gormx 钉住的 conn),迁移语句全部走它(避免 DDL 落到池里
// 其它连接)。
func ensureSchemaLocked(db *gorm.DB) error {
	fresh := !db.Migrator().HasTable(&SyncTask{})

	// 已应用版本集合。
	applied, err := appliedVersions(db)
	if err != nil {
		return err
	}

	switch {
	case fresh:
		// 全新安装:模型即基线。
		if err := db.AutoMigrate(allModels...); err != nil {
			return err
		}
		if err := recordMigration(db, BaselineVersion, "baseline: automigrate-era schema (fresh install)"); err != nil {
			return err
		}
		applied[BaselineVersion] = struct{}{}
	case len(applied) == 0:
		// 存量库首次跑本代码:AutoMigrate 收敛(与旧版本每次启动等价),
		// 记 baseline 后进入版本化时代。
		if err := db.AutoMigrate(allModels...); err != nil {
			return err
		}
		if err := recordMigration(db, BaselineVersion, "legacy baseline: converged by automigrate"); err != nil {
			return err
		}
		applied[BaselineVersion] = struct{}{}
	}

	// 补跑未应用的显式迁移(逐条单事务)。
	for _, m := range migrations {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := m.Apply(tx); err != nil {
				return oekiterrors.Wrap(err, fmt.Sprintf("migration %d (%s) failed", m.Version, m.Name))
			}
			return tx.Create(&SchemaMigration{Version: m.Version, Name: m.Name, AppliedAt: time.Now()}).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// appliedVersions 已应用版本集合。
func appliedVersions(db *gorm.DB) (map[uint64]struct{}, error) {
	var rows []SchemaMigration
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[uint64]struct{}, len(rows))
	for _, r := range rows {
		m[r.Version] = struct{}{}
	}
	return m, nil
}

// recordMigration 记录已应用版本(不在迁移事务内时也保证幂等:主键冲突忽略)。
// OnConflict DoNothing 兼容 MySQL/sqlite;不用 INSERT IGNORE(方言专属)。
func recordMigration(db *gorm.DB, version uint64, name string) error {
	return db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&SchemaMigration{Version: version, Name: name, AppliedAt: time.Now()}).Error
}
