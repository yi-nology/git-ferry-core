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
	// v7 (2026-11-17, #36):generation-based coalescing 三步走(数据/DDL
	// 各自独立事务——glebarez sqlite 实测同一事务里 DDL 之后的 DML 会在
	// 提交时被静默丢弃,见 migrate_test.go 的 v7 回填用例注释;MySQL 无此
	// 问题,拆分对两种后端都安全):
	//   v7 数据:同任务重复活动作业去重(旧 WHERE NOT EXISTS 在 MySQL RR
	//          下的竞态遗留)——必须先于唯一索引建立(顺序不可换);
	//   v8 DDL:execution_task_states 表 + execution_jobs 的
	//          generation/active_key 列 + 活动槽唯一索引(索引**不挂模型
	//          tag**:baseline/v2 的 AutoMigrate 先于 v8,挂了 tag 存量重复
	//          会让索引建立失败);
	//   v9 数据:回填存量活动作业(generation=1/active_key/任务状态行)。
	{Version: 7, Name: "dedup execution_jobs active slots (generation coalescing pre-step)",
		Apply: migrateV7DedupActiveSlots},
	{Version: 8, Name: "execution task states + job generation/active slot (generation coalescing)",
		Apply: migrateV8CoalescingSchema},
	{Version: 9, Name: "backfill job generation/active slot and task states",
		Apply: migrateV9BackfillCoalescing},
	// v10 (2026-11-17, #37):execution_jobs.lease_version —— 租约纪元
	// fencing token。Complete/RenewLease/Reschedule 的条件更新带
	// lease_version=?,RequeueExpired 单语句条件更新(重新复核 lease_until)
	// 并 +1 纪元:毒丸终态不可被旧属主覆写、续租中的作业不被 reaper 重放。
	// 存量行默认 0,下一次认领/接管自动 +1,向后兼容无需回填。
	// 显式 ALTER ADD(不走 AutoMigrate):它的整表重建会用加列前的列清单
	// 拷贝数据,把 v9 的回填重置为默认值(见 migrateV8CoalescingSchema 注)。
	{Version: 10, Name: "add lease_version to execution_jobs (lease fencing token)",
		Apply: func(tx *gorm.DB) error {
			return addColumnsIfMissing(tx, map[string]string{
				"lease_version": "integer NOT NULL DEFAULT 0",
			})
		}},
}

// activeSlotIndexName execution_jobs 活动槽唯一索引(每任务至多一条
// pending/claimed 作业的数据库级硬保证,#36)。
const activeSlotIndexName = "uk_execution_jobs_active_slot"

// activeJobStatuses 活动作业状态集合(去重/回填共用)。
var activeJobStatuses = []string{JobStatusPending, JobStatusClaimed}

// migrateV7DedupActiveSlots v7(纯数据变更):同任务多条活动作业只保留一条
// (claimed 优先——它可能有 worker 在跑;同态取更早 id),其余判 failed。
// 这是 v8 唯一索引的先行条件。
func migrateV7DedupActiveSlots(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("execution_jobs") {
		return nil
	}
	var actives []ExecutionJob
	if err := tx.Where("status IN ?", activeJobStatuses).Order("id").Find(&actives).Error; err != nil {
		return err
	}
	keep := make(map[string]uint, len(actives))
	var dupes []uint
	for i := range actives {
		j := actives[i]
		curID, ok := keep[j.TaskKey]
		if !ok {
			keep[j.TaskKey] = j.ID
			continue
		}
		var cur ExecutionJob
		for k := range actives {
			if actives[k].ID == curID {
				cur = actives[k]
				break
			}
		}
		replace := cur.Status != JobStatusClaimed && j.Status == JobStatusClaimed
		if replace {
			dupes = append(dupes, cur.ID)
			keep[j.TaskKey] = j.ID
		} else {
			dupes = append(dupes, j.ID)
		}
	}
	if len(dupes) == 0 {
		return nil
	}
	return tx.Model(&ExecutionJob{}).Where("id IN ?", dupes).Updates(map[string]any{
		"status":      JobStatusFailed,
		"error":       "deduplicated by migration v7 (multiple active jobs per task)",
		"finished_at": time.Now(),
	}).Error
}

// migrateV8CoalescingSchema v8(纯 DDL):补 execution_task_states 表与
// execution_jobs 的 generation/active_key 列,显式建活动槽唯一索引(幂等:
// 迁移失败重试时跳过已存在的索引)。
//
// **为什么不用 AutoMigrate 加列**(glebarez sqlite 实测):AutoMigrate 对
// 已有数据的表会走「建 __temp → INSERT...SELECT 拷贝 → DROP → RENAME」的
// 整表重建路径,而拷贝的列清单可能取自加列前的缓存快照 ⇒ 新加列的数据
// 被静默重置为默认值(v9 的回填曾被 v10 的 AutoMigrate 这样回滚)。
// Migrator().AddColumn 是纯增量 ALTER ADD,不重建、不触碰既有列,两种
// 后端语义一致;模型 tag 上不带这些索引/列的重建需求。
func migrateV8CoalescingSchema(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&ExecutionTaskState{}); err != nil {
		return err
	}
	if err := addColumnsIfMissing(tx, map[string]string{
		"generation": "integer NOT NULL DEFAULT 0",
		"active_key": "text",
	}); err != nil {
		return err
	}
	if !tx.Migrator().HasIndex(&ExecutionJob{}, activeSlotIndexName) {
		if err := tx.Exec("CREATE UNIQUE INDEX " + activeSlotIndexName +
			" ON " + TableExecutionJobs + " (active_key)").Error; err != nil {
			return err
		}
	}
	return nil
}

// addColumnsIfMissing 显式补列(ALTER TABLE ADD COLUMN,纯增量):
// glebarez sqlite 的整表重建会把新列数据重置为默认值(见 v8 注),
// 迁移里对已有数据的 execution_jobs 一律用显式加列,不走 AutoMigrate。
// 列型 DDL 与 gorm 对同模型加列的输出形态一致(MySQL/sqlite 均兼容,
// 反引号标识符两种后端都接受)。
func addColumnsIfMissing(tx *gorm.DB, cols map[string]string) error {
	for col, def := range cols {
		if tx.Migrator().HasColumn(&ExecutionJob{}, col) {
			continue
		}
		if err := tx.Exec("ALTER TABLE `" + TableExecutionJobs + "` ADD `" + col + "` " + def).Error; err != nil {
			return err
		}
	}
	return nil
}

// migrateV9BackfillCoalescing v9(纯数据变更):回填存量活动作业——
// generation=1、active_key=task_key,任务状态行 desired=1/completed=0/
// active_job_id=该作业。已代次化,收尾 CAS 语义无缝接管(不会为升级前的
// 合并丢弃补跑,那部分已丢失无从恢复)。
func migrateV9BackfillCoalescing(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("execution_jobs") {
		return nil
	}
	var actives []ExecutionJob
	if err := tx.Where("status IN ?", activeJobStatuses).Order("id").Find(&actives).Error; err != nil {
		return err
	}
	for i := range actives {
		j := actives[i]
		if err := tx.Model(&ExecutionJob{}).Where("id = ?", j.ID).Updates(map[string]any{
			"generation": 1,
			"active_key": j.TaskKey,
		}).Error; err != nil {
			return err
		}
		id := j.ID
		st := ExecutionTaskState{TaskKey: j.TaskKey, DesiredGeneration: 1, ActiveJobID: &id}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_key"}},
			DoNothing: true,
		}).Create(&st).Error; err != nil {
			return err
		}
	}
	return nil
}

// ensureSchema 启动时的 schema 收敛入口(见文件头策略)。
//
// 多实例串行化:MySQL 走 gormx.RunWithAdvisoryLock——GET_LOCK/RELEASE_LOCK
// 与迁移语句钉在同一条连接上(会话级锁的连接池坑由此闭合);sqlite 单写者
// 天然串行,不取锁——但**必须钉在单条连接上**:#37 期间实测(glebarez
// sqlite)连接池里两条连接交叉读写同一张表时,后连接的 AutoMigrate 表重建
// 会读到旧快照并把它写回(迁移数据被静默回滚)。整个收敛包进一个事务
// (sqlite DDL 可事务),迁移 SQL 全部落到同一条连接,代价是整个 schema
// 收敛原子提交(sqlite 单写者,无并发收益可牺牲)。
func ensureSchema(db *gorm.DB, driver string) error {
	// 版本表自身幂等建表(先于锁,锁不需要它)。
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		return err
	}
	if driver != DriverMySQL {
		return db.Transaction(func(tx *gorm.DB) error {
			return ensureSchemaLocked(tx)
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), schemaMigrateLockTimeout)
	defer cancel()
	return gormx.RunWithAdvisoryLock(ctx, db, schemaMigrateLockKey(), ensureSchemaLocked)
}

// ensureSchemaLocked 在迁移互斥区内执行版本收敛。传入的 db 是**持锁/钉住
// 的连接**(MySQL: gormx 钉住的 conn;sqlite: 上述事务),迁移语句全部走它
// (避免 DDL/DML 落到池里其它连接造成跨连接可见性事故)。内部再次开事务
// (嵌套 = SAVEPOINT),版本记录与迁移语句同事务。
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
