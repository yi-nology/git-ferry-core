package repository

import (
	"context"
	"time"

	"github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrLeaseLost 失锁哨兵:条件更新(续租/完成/退避重排)0 行命中 —— 作业已
// 不再归本实例所有(lease 过期被 reaper 放回、被其它实例接管等)。调用方
// 必须把它当作「我已失去执行权,该停手」的信号:立即停止本次执行,不得再跑
// executor/推送(防脑裂分叉推送)。用 errors.Is 判定。
var ErrLeaseLost = errors.New("execution job lease lost: no longer owned by this instance")

// ExecutionJobRepository 持久化执行队列(见 model.ExecutionJob)。
//
// #36 generation coalescing:每任务一条状态行(model.ExecutionTaskState)
// 记 desired/completed 代次与活动槽;入队在**同一事务**里递增 desired ——
// 执行中到达的触发即使被合并,也以代次形式留痕,作业收尾 CAS 发现
// completed < desired 必须再入追赶作业 ⇒ 被合并的触发不丢。
type ExecutionJobRepository struct {
	base[model.ExecutionJob, uint]
	// lockingReads MySQL 方言下用 SELECT ... FOR UPDATE 锁定读状态行:
	// REPEATABLE READ 的快照读看不到并发事务的代次递增,不锁定读会让两个
	// 并发入队都判「活动槽空闲」而双插(唯一索引能拦住,但整个事务回滚、
	// 触发方报错)。锁定读把同任务的并发入队串行化在状态行上。
	// sqlite 单写者天然串行(整库一写锁),且不支持 FOR UPDATE 语法。
	lockingReads bool
}

func NewExecutionJobRepository(db *gorm.DB) *ExecutionJobRepository {
	return &ExecutionJobRepository{base: newBase[model.ExecutionJob, uint](db), lockingReads: isMySQLDialect(db)}
}

// isMySQLDialect 报告连接的方言是否 MySQL:FOR UPDATE 锁定读只有 MySQL 支持
// (sqlite 单写者天然串行,不支持该语法)。显式过一道局部变量而不直接
// db.Dialector.Name() —— 嵌入字段链上直呼会被 staticcheck QF1008 要求
// 消去嵌入选择器,而 db.Name() 把「方言名」藏得太深。
func isMySQLDialect(db *gorm.DB) bool {
	dialector := db.Dialector
	return dialector != nil && dialector.Name() == "mysql"
}

// Enqueue 入队(#36 generation coalescing):
//
//  1. 同事务把任务状态行的 desired_generation +1 —— **每次触发都递增**,
//     包括将被合并的这次:合并不再等于丢弃,代次就是待办承接标记;
//  2. 活动槽空闲 ⇒ 插入携带 generation 的作业并占槽(WHERE NOT EXISTS 仅
//     快路径;active_key 唯一索引兜底并发竞态,竞态插入失败=整体回滚=
//     触发方报错重试,at-least-once);
//  3. 活动槽被占 ⇒ 合并(返回 enqueued=false),代次已递增,作业收尾 CAS
//     据此补跑 —— 执行中连发 N 次 webhook 最终全部被承接,最后一次触发
//     对应的提交一定被执行。
func (d *ExecutionJobRepository) Enqueue(taskKey, trigger string, webhookEventID *uint) (enqueued bool, err error) {
	err = d.db.Transaction(func(tx *gorm.DB) error {
		// (1) 同事务代次递增(upsert;首次受理 desired=1)。
		st := model.ExecutionTaskState{TaskKey: taskKey, DesiredGeneration: 1}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "task_key"}},
			DoUpdates: clause.Assignments(map[string]any{
				"desired_generation": gorm.Expr("desired_generation + 1"),
				"updated_at":         time.Now(),
			}),
		}).Create(&st).Error; err != nil {
			return err
		}

		// (2) 锁定读状态行(见 lockingReads 注):代次与活动槽一致快照。
		var state model.ExecutionTaskState
		q := tx.Where("task_key = ?", taskKey)
		if d.lockingReads {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&state).Error; err != nil {
			return err
		}

		// (3) 活动槽被占 ⇒ 合并(不重复发布);待办承接 = 步骤 (1) 的代次递增。
		if state.ActiveJobID != nil {
			enqueued = false
			return nil
		}

		// (4) 活动槽空闲 ⇒ 插入携带代次的作业。
		// ON CONFLICT DO NOTHING 由 active_key 唯一索引兜底并发竞态
		// (迁移 v8 建立;0 行命中 = 另一实例已占槽,按合并处理)。
		active := taskKey
		job := &model.ExecutionJob{
			TaskKey:        taskKey,
			Trigger:        trigger,
			WebhookEventID: webhookEventID,
			Status:         model.JobStatusPending,
			Generation:     state.DesiredGeneration,
			ActiveKey:      &active,
			QueuedAt:       time.Now(),
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(job)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 状态行说空闲而唯一索引拒绝(账本与作业表不一致):
			// 不插作业,按合并处理 —— 在跑作业的收尾 CAS 会据代次补跑。
			enqueued = false
			return nil
		}

		// (5) 占活动槽(指向刚插入的作业;唯一索引保证只有一条可指)。
		if err := tx.Model(&model.ExecutionTaskState{}).
			Where("task_key = ? AND active_job_id IS NULL", taskKey).
			Update("active_job_id", job.ID).Error; err != nil {
			return err
		}
		enqueued = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return enqueued, nil
}

// ListPendingIDs 取**当前可认领**的作业 ID(按入队序 = 因果序)。
//
// pending 态的 lease_until 是退避重排的 not-before 标记(见 Reschedule):
// 未到点的作业混在队首会让派发器每轮空试 Claim,后面的可认领作业被
// 队头阻塞。这里直接按时间过滤,队列语义与 Claim 的条件保持一致。
func (d *ExecutionJobRepository) ListPendingIDs(limit int) ([]uint, error) {
	var ids []uint
	err := d.db.Model(&model.ExecutionJob{}).
		Where("status = ? AND (lease_until IS NULL OR lease_until <= ?)",
			model.JobStatusPending, time.Now()).
		Order("id").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}

// FindByID 查单条。
func (d *ExecutionJobRepository) FindByID(id uint) (*model.ExecutionJob, error) {
	return d.getByID(id)
}

// Claim 条件认领:仅当仍是 pending 才翻为 claimed(原子,多实例单胜者)。
// owner 通常为实例标识;ttl 为执行权租约;认领开启新的租约纪元
// (lease_version+1,fencing token 见 model.ExecutionJob.LeaseVersion)。
//
// lease_until 在 pending 态下复用作「最早可认领时间」(not-before,见
// Reschedule):退避中的作业不得提前认领,防派发器对同一作业忙旋。
func (d *ExecutionJobRepository) Claim(id uint, owner string, ttl time.Duration) (bool, error) {
	now := time.Now()
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND status = ? AND (lease_until IS NULL OR lease_until <= ?)",
			id, model.JobStatusPending, now).
		Updates(map[string]any{
			"status":        model.JobStatusClaimed,
			"owner":         owner,
			"lease_until":   now.Add(ttl),
			"claimed_at":    now,
			"lease_version": gorm.Expr("lease_version + 1"),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// RenewLease 执行中续租。仅「本人 + 仍 claimed + 租约未过期 + 纪元一致」
// 才生效:已过期的租约不得借续租复活(对齐 lock 包「已过期槽不得借 Renew
// 复活」的语义——它的执行权可能已被其他实例接管,复活即脑裂);旧纪元的
// 持有者不得续租新纪元的作业(同 owner 字符串的先后两次认领,owner+status
// 判据拦不住,靠 lease_version 按纪元排除,#37)。续租不换纪元。
// 0 行命中 ⇒ 返回 ErrLeaseLost(旧属主据此立即停手),不再静默吞掉。
func (d *ExecutionJobRepository) RenewLease(id uint, owner string, leaseVersion int64, ttl time.Duration) error {
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ? AND status = ? AND lease_until > ? AND lease_version = ?",
			id, owner, model.JobStatusClaimed, time.Now(), leaseVersion).
		Update("lease_until", time.Now().Add(ttl))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Reschedule 执行权被占(良性繁忙)的退避重排:作业放回 pending,
// **attempt 不增**(毒丸预算只留给「lease 过期被接管」的崩溃接管场景,
// 见 RequeueExpired——良性繁忙烧光毒丸预算会把作业误判成毒丸丢弃);
// lease_until 在 pending 态下复用作「最早可认领时间」(not-before)推迟
// delay,防派发器忙旋;DeferCount 单独记账,与接管次数(Attempt)分离。
// 条件补 status=claimed AND lease_until>now AND lease_version=?(#37):
// 租约已过期的属主不得退避重排(执行权存疑,随时可能被接管);
// 重排关闭本纪元(lease_version+1)。
// 0 行命中(作业已被接管,不再归本人)⇒ ErrLeaseLost。
func (d *ExecutionJobRepository) Reschedule(id uint, owner string, leaseVersion int64, delay time.Duration) error {
	res := d.db.Model(&model.ExecutionJob{}).
		Where("id = ? AND owner = ? AND status = ? AND lease_until > ? AND lease_version = ?",
			id, owner, model.JobStatusClaimed, time.Now(), leaseVersion).
		Updates(map[string]any{
			"status":        model.JobStatusPending,
			"owner":         "",
			"lease_until":   time.Now().Add(delay),
			"defer_count":   gorm.Expr("defer_count + 1"),
			"lease_version": gorm.Expr("lease_version + 1"),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Complete 完成作业:写终态与运行记录 ID,并在**同一事务**里结算代次账本
// (#36):上推 completed_generation、释放活动槽,若 completed < desired
// 则再入一个追赶作业 —— 被合并进这次执行的触发不丢。
// 条件补 status=claimed AND lease_until>now AND lease_version=?(#37):
// 毒丸终态不可被旧属主覆写(reaper 已判 failed,WHERE 不再命中),
// 租约过期/被接管的属主不得写终态。0 行命中 ⇒ ErrLeaseLost。
func (d *ExecutionJobRepository) Complete(id uint, owner string, leaseVersion int64, runID uint, execErr error) error {
	status := model.JobStatusSucceeded
	errMsg := ""
	if execErr != nil {
		status = model.JobStatusFailed
		errMsg = execErr.Error()
	}
	return d.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.ExecutionJob{}).
			Where("id = ? AND owner = ? AND status = ? AND lease_until > ? AND lease_version = ?",
				id, owner, model.JobStatusClaimed, time.Now(), leaseVersion).
			Updates(map[string]any{
				"status":      status,
				"run_id":      runID,
				"error":       errMsg,
				"finished_at": time.Now(),
				"active_key":  nil,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrLeaseLost
		}
		var job model.ExecutionJob
		if err := tx.First(&job, id).Error; err != nil {
			return err
		}
		return d.settleCompletedGeneration(tx, &job)
	})
}

// settleCompletedGeneration 作业**执行完**(成功或失败,执行真实发生过)的
// 代次结算(#36,调用方必须在事务内):completed_generation CAS 上推 →
// 释放活动槽 → completed < desired ⇒ **必须**再入一个追赶作业。
//
// 追赶作业 generation=最新 desired,继承 task_key 与 trigger —— 重建语义
// 不丢;被合并的多个 webhook_event_id 无法对应到具体事件,置 nil,与
// pending_trigger 补跑路径口径一致。
func (d *ExecutionJobRepository) settleCompletedGeneration(tx *gorm.DB, job *model.ExecutionJob) error {
	// 代次上推:仅当本作业携带的代次更新(CAS,幂等)。
	if err := tx.Model(&model.ExecutionTaskState{}).
		Where("task_key = ? AND completed_generation < ?", job.TaskKey, job.Generation).
		Update("completed_generation", job.Generation).Error; err != nil {
		return err
	}
	return d.releaseSlotAndCatchUp(tx, job)
}

// releaseSlotAndCatchUp 释放状态行活动槽并在仍有未消费代次时补追赶作业
// (#36,调用方必须在事务内)。毒丸路径(作业从未执行)也走这里——
// 它**不上推 completed_generation**:代次没被消费,追赶作业必须承接。
func (d *ExecutionJobRepository) releaseSlotAndCatchUp(tx *gorm.DB, job *model.ExecutionJob) error {
	// 释放活动槽:仅当仍指向本作业(防误伤接管方新占的槽)。
	res := tx.Model(&model.ExecutionTaskState{}).
		Where("task_key = ? AND active_job_id = ?", job.TaskKey, job.ID).
		Update("active_job_id", nil)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// 槽不指向本作业(账本异常):不补追赶,避免双槽双执行。
		return nil
	}
	// 追赶判断(锁定读:与并发入队的代次递增交错时取最新值,防漏判)。
	var state model.ExecutionTaskState
	q := tx.Where("task_key = ?", job.TaskKey)
	if d.lockingReads {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&state).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // 无状态行(理论上不可达):无事可结
		}
		return err
	}
	if state.CompletedGeneration >= state.DesiredGeneration {
		return nil // 全部代次已覆盖,无需追赶
	}
	next := model.ExecutionJob{
		TaskKey:    job.TaskKey,
		Trigger:    job.Trigger,
		Status:     model.JobStatusPending,
		Generation: state.DesiredGeneration,
		ActiveKey:  &job.TaskKey,
		QueuedAt:   time.Now(),
	}
	if err := tx.Create(&next).Error; err != nil {
		return err
	}
	return tx.Model(&model.ExecutionTaskState{}).
		Where("task_key = ? AND active_job_id IS NULL", job.TaskKey).
		Update("active_job_id", next.ID).Error
}

// RequeueExpired 接管过期租约(reaper):claimed 且 lease_until 已过 ⇒
// 放回 pending(attempt+1);attempt 超限 ⇒ 判 failed(毒丸)。
// 返回 (重放条数, 毒丸条数, 错误)。
//
// **单语句条件更新,去掉「先查后改」窗口**(#37):UPDATE 自带
// lease_until < now 的重新复核——候选选出之后、UPDATE 之前 worker 续租
// 成功(lease 重新有效)的作业不再被放回 pending,随后可被第三个实例
// 认领的双属主窗口由此闭合;重排/毒丸各自 +1 纪元(lease_version),
// 旧属主的续租/收尾立即失锁。
//
// 毒丸作业从未执行完 ⇒ 它携带的代次未消费:判毒丸的**同一事务**里释放
// 活动槽并补追赶作业(#36),否则执行中被合并的触发会随毒丸一起蒸发。
func (d *ExecutionJobRepository) RequeueExpired(now time.Time) (requeued, poisoned int64, err error) {
	// 候选只取毒丸阈值已到的作业:重排分支是单条集合更新,不需要候选;
	// 把全部过期作业(含 error 全文)读进内存只会在大面积故障时放大 reaper
	// 的内存开销。一切生效仍以条件 UPDATE 为准。
	var expired []model.ExecutionJob
	if err := d.db.Where("status = ? AND lease_until IS NOT NULL AND lease_until < ? AND attempt >= ?",
		model.JobStatusClaimed, now, model.MaxJobAttempts).Find(&expired).Error; err != nil {
		return 0, 0, err
	}

	// 重排分支:单语句条件更新覆盖全部「attempt 未超限」的过期作业。
	res := d.db.Model(&model.ExecutionJob{}).
		Where("status = ? AND lease_until IS NOT NULL AND lease_until < ? AND attempt < ?",
			model.JobStatusClaimed, now, model.MaxJobAttempts).
		Updates(map[string]any{
			"status":        model.JobStatusPending,
			"attempt":       gorm.Expr("attempt + 1"),
			"owner":         "",
			"lease_until":   nil,
			"lease_version": gorm.Expr("lease_version + 1"),
		})
	if res.Error != nil {
		return 0, 0, res.Error
	}
	requeued = res.RowsAffected

	// 毒丸分支:逐条条件更新(每条都重新复核 lease_until),命中者在
	// 同一事务里做代次善后。
	for i := range expired {
		if expired[i].Attempt < model.MaxJobAttempts {
			continue // 重排分支已覆盖
		}
		var hit int64
		if err := d.db.Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&model.ExecutionJob{}).
				Where("id = ? AND status = ? AND lease_until IS NOT NULL AND lease_until < ?",
					expired[i].ID, model.JobStatusClaimed, now).
				Updates(map[string]any{
					"status":        model.JobStatusFailed,
					"attempt":       gorm.Expr("attempt + 1"),
					"error":         "max attempts exceeded (lease expired repeatedly)",
					"finished_at":   time.Now(),
					"active_key":    nil,
					"lease_version": gorm.Expr("lease_version + 1"),
				})
			if res.Error != nil {
				return res.Error
			}
			hit = res.RowsAffected
			if hit != 1 {
				return nil
			}
			job := expired[i]
			return d.releaseSlotAndCatchUp(tx, &job)
		}); err != nil {
			return requeued, poisoned, err
		}
		poisoned += hit
	}
	return requeued, poisoned, nil
}

// CleanupFinished 清理已完成且超过保留期的作业。
func (d *ExecutionJobRepository) CleanupFinished(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res := d.db.WithContext(ctx).
		Where("status IN (?, ?) AND finished_at < ?", model.JobStatusSucceeded, model.JobStatusFailed, cutoff).
		Limit(1000).Delete(&model.ExecutionJob{})
	return res.RowsAffected, res.Error
}

// CountPending 观测用。
func (d *ExecutionJobRepository) CountPending() (int64, error) {
	var n int64
	err := d.db.Model(&model.ExecutionJob{}).
		Where("status = ?", model.JobStatusPending).Count(&n).Error
	return n, err
}

// DB 暴露底层句柄(测试/事务用)。
func (d *ExecutionJobRepository) DB() *gorm.DB { return d.db }
