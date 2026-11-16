package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
	"github.com/robfig/cron/v3"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== cron 共享调度(#40) =====
//
// 旧实现:每个实例启动时把 DB 里的 cron 任务灌进**本地 robfig 表**,
// Create/Update 只改本实例 ⇒ 跨实例配置不一致;持有某条 entry 的实例
// 退出后,没有其它实例重建它 ⇒ 任务永久停摆。
//
// 现实现:DB 的 sync_tasks.next_fire_at 是唯一事实源。
//   - 每个实例周期扫 DB(不缓存本地调度表);
//   - 到点触发用 **单条条件更新** 抢占:
//     UPDATE sync_tasks SET next_fire_at=? WHERE key=? AND next_fire_at<=now
//     RowsAffected=1 的实例触发,多实例天然单胜者;
//   - 持有者退出不影响排程:next_fire_at 已在库里,其它实例下一轮即可
//     接手,不需要 leader election;
//   - 新建/修改/删除 cron 只改 DB,所有实例下一轮看到同一计划。
//
// 代价是一次周期 DB 查询(默认 15s),换来「有效调度计划」可被运维
// 直接查询(sync_tasks.next_fire_at),不再有各实例各一份的幽灵表。

const (
	// cronScanInterval 共享调度扫描周期:配置变更(新建/改 cron/禁用)
	// 在 N 秒内被所有实例看到;到点触发的最大延迟同量级。
	cronScanInterval = 15 * time.Second
)

// addCronJob 保持旧的调用点语义,但不再操作本地 robfig 表:
// 校验表达式 + 清空 next_fire_at,让调度器下一轮按新表达式重新播种。
func (s *Service) addCronJob(task *model.SyncTask) error {
	if task == nil {
		return fmt.Errorf("cron: nil task")
	}
	if task.Cron != "" {
		if _, err := cron.ParseStandard(task.Cron); err != nil {
			return err
		}
	}
	if s.tasks == nil {
		return nil
	}
	return s.tasks.taskDAO.ResetNextFire(task.Key)
}

// removeCronJob 清空排程(禁用/删除/清空 cron 后不再触发)。
func (s *Service) removeCronJob(taskKey string) {
	if s.tasks == nil {
		return
	}
	if err := s.tasks.taskDAO.ResetNextFire(taskKey); err != nil {
		slog.Error("cron: 清空排程失败", "taskKey", taskKey, "error", err)
	}
}

// startCronJobs 启动共享调度循环(幂等)。
// 首次扫描同步播种缺失的 next_fire_at,随后由后台循环周期抢占到期任务。
func (s *Service) startCronJobs() error {
	s.cronMu.Lock()
	if s.cronStarted {
		s.cronMu.Unlock()
		return nil
	}
	s.cronStarted = true
	base := s.bgCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	s.cronCancel = cancel
	s.cronMu.Unlock()

	if err := s.runCronTick(ctx); err != nil {
		slog.Error("cron: 首次扫描失败", "error", err)
	}

	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()
		loop.Run(ctx, loop.Config{
			Interval:   cronScanInterval,
			FirstDelay: cronScanInterval,
			Name:       "cron-shared-scheduler",
		}, func(ctx context.Context) {
			if err := s.runCronTick(ctx); err != nil {
				slog.Error("cron: 扫描失败", "error", err)
			}
		}).Wait()
	})
	return nil
}

// stopCronJobs 停止调度循环(已写库的 next_fire_at 保留,由其它实例接手)。
func (s *Service) stopCronJobs() {
	s.cronMu.Lock()
	cancel := s.cronCancel
	s.cronCancel = nil
	s.cronStarted = false
	s.cronMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// runCronTick 扫描一轮:
//   - next_fire_at 为空 ⇒ 按当前时间播种(不触发);
//   - 已到点 ⇒ 条件更新抢占并异步触发(只触发一次)。
func (s *Service) runCronTick(ctx context.Context) error {
	if s.tasks == nil {
		return nil
	}
	tasks, err := s.tasks.FindAllEnabledTasks()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, task := range tasks {
		if task == nil || task.Cron == "" {
			continue
		}
		sched, perr := cron.ParseStandard(task.Cron)
		if perr != nil {
			// 写路径已校验;这里兜底手工改库的非法表达式,不拖垮其它任务。
			slog.Error("cron: 非法表达式,跳过", "taskKey", task.Key, "cron", task.Cron, "error", perr)
			continue
		}
		if task.NextFireAt == nil {
			next := sched.Next(now)
			if next.IsZero() {
				continue
			}
			claimed, serr := s.tasks.taskDAO.SeedNextFire(task.Key, next)
			if serr != nil {
				slog.Error("cron: 播种 next_fire_at 失败", "taskKey", task.Key, "error", serr)
			} else if claimed {
				slog.Info("cron: 已播种下一次触发", "taskKey", task.Key, "next_fire_at", next)
			}
			continue
		}
		if task.NextFireAt.After(now) {
			continue
		}
		next := sched.Next(now)
		if !next.After(now) {
			next = sched.Next(now.Add(time.Second))
		}
		claimed, cerr := s.tasks.taskDAO.ClaimDueFire(task.Key, now, next)
		if cerr != nil {
			slog.Error("cron: 抢占到期任务失败", "taskKey", task.Key, "error", cerr)
			continue
		}
		if !claimed {
			continue // 另一个实例已抢占并推进排程
		}
		slog.Info("cron: 本实例抢占到期任务", "taskKey", task.Key, "next_fire_at", next)
		s.triggerCronTask(ctx, task.Key)
	}
	return nil
}

// triggerCronTask 异步触发一次同步(cron 触发)。
// 触发失败不回滚 next_fire_at:下一次排程已推进,失败会在执行历史/outbox
// 可见;若执行权被占,这次触发按既有语义跳过(下一周期继续)。
func (s *Service) triggerCronTask(ctx context.Context, taskKey string) {
	async.GoSafe(func() {
		if err := s.RunTaskWithTrigger(ctx, taskKey, model.TriggerCron, nil); err != nil {
			slog.Error("cron task failed", "taskKey", taskKey, "error", err)
		}
	})
}
