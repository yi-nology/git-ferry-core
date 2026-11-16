package service

import (
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 失败自动补偿策略 =====
//
// 此前冷却/次数判定散落在壳层 runwatch（轮询驱动）；策略与计数收敛到 core，
// 壳层只负责「收到完成事件 → 问策略 → 触发重跑 + 通知」。

// AutoRetryPolicy 同一运行失败后的自动重跑策略。
type AutoRetryPolicy struct {
	// MaxAutoRetries 失败后的自动重跑次数；0=不自动重跑
	MaxAutoRetries int
	// Cooldown 两次自动重跑之间的冷却
	Cooldown time.Duration
}

// chainStatsFunc 查询「包含 runID 的连续 auto_retry 链」的统计,
// 由 Service 注入并把事实落在 DB(见 dao.SyncRunDAO.AutoRetryChainStats)。
// 返回:链内 auto_retry 条数 + 最近一条链内运行的结束时间(无链时为零值)。
type chainStatsFunc func(taskKey string, runID uint, runIsRetry bool) (int64, time.Time, error)

// RetryTracker 自动重跑判定器。
//
// 修复前的实现按 runID 内存记账——每次重跑都是一条**新 run 记录(新 ID)**,
// 预算永远从零计起 ⇒ MaxAutoRetries 形同虚设,失败链可无限延伸(P0)。
//
// 现语义:
//   - **计数从 DB 派生**(chain):「相邻两个非 auto_retry 运行之间的
//     auto_retry 运行」构成一条链,链内条数就是已消耗预算。落库的好处:
//     重启不丢、多实例一致、订阅+轮询双通道投递看到的是同一份事实。
//   - **decided 只做同进程去重**:防止订阅事件与轮询扫描对同一条失败
//     运行重复放行。它不参与预算——预算是 DB 里的链长,不是这个 map。
type RetryTracker struct {
	mu      sync.Mutex
	decided map[uint]struct{} // runID -> 已放行/已排程(同进程去重)
	chain   chainStatsFunc    // nil = 未接 DB(仅裸构造的测试路径),预算恒按 0 算
	// schedule 冷却期延迟重跑落库函数(nil = 未接 DB,只返回 Deferred 决策)。
	schedule retryScheduleFunc
	// maxKeep 淘汰阈值，防止 decided 无限增长
	maxKeep int
}

// AutoRetry 返回失败自动补偿的策略计数器（NewService 建好）。
func (s *Service) AutoRetry() *RetryTracker { return s.retryTracker }

// NewRetryTracker 创建计数器；maxKeep<=0 时用默认 1000。
func NewRetryTracker(maxKeep int) *RetryTracker {
	if maxKeep <= 0 {
		maxKeep = 1000
	}
	return &RetryTracker{
		decided: make(map[uint]struct{}),
		maxKeep: maxKeep,
	}
}

// SetChainStats 注入 DB 链统计函数(由 NewService 用 run DAO 接线)。
func (t *RetryTracker) SetChainStats(fn chainStatsFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chain = fn
}

// retryScheduleFunc 把「冷却期延后」落成持久化排程(由 Service 注入 DB)。
// 成功 ⇒ 该失败运行在排程表里恰有一条记录(run_id 唯一),多实例/双通道
// 不会重复排程;失败 ⇒ 不标记 decided,留待下次判定或重启恢复。
type retryScheduleFunc func(run *model.SyncRun, d Decision) error

// SetRetryScheduler 注入冷却期排程落库函数(由 NewService 接线)。
func (t *RetryTracker) SetRetryScheduler(fn retryScheduleFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.schedule = fn
}

// Decision 一次判定结果。
type Decision struct {
	// Retry 是否应立即触发自动重跑
	Retry bool
	// Deferred 现在不能跑、但**预算未耗尽**,应在 RetryAfter 之后再判定/调度。
	// 与 Retry=false+Reason 的「永久不重试」严格区分:冷却期不是终点,
	// 只是时间未到(#39)。
	Deferred bool
	// RetryAfter Deferred=true 时距最早可重跑时刻的剩余等待时间。
	// 冷却只推迟、不消耗预算。
	RetryAfter time.Duration
	// Attempt 本次将要进行的第几次重跑（1 起）
	Attempt int
	// AttemptLimit 本次判定时的自动重跑预算上限(排程快照用)。
	AttemptLimit int
	// Reason 不立即重跑时的原因（可日志输出;冷却为 "cooldown"）
	Reason string
}

// ShouldRetry 判定该失败运行是否应自动重跑,放行时记账(同进程去重)。
// 只对 Status=failed 的运行放行；成功运行直接跳过。
//
// #39 冷却语义:冷却未到 ⇒ Retry=false 但 **Deferred=true + RetryAfter>0**,
// 且不消耗预算。若注入了排程函数,这里会把「到点重跑」写入 retry_schedules
// (run_id 唯一),由 core 的延迟重跑派发器在到点后触发 —— 冷却只推迟,
// 不会让「冷却内失败」把剩余预算永久冻结。
func (t *RetryTracker) ShouldRetry(run *model.SyncRun, p AutoRetryPolicy) Decision {
	if run == nil {
		return Decision{Reason: "nil run"}
	}
	if p.MaxAutoRetries <= 0 {
		return Decision{Reason: "auto retry disabled"}
	}
	if run.Status != model.StatusFailed {
		return Decision{Reason: "run not failed"}
	}

	t.mu.Lock()
	if _, ok := t.decided[run.ID]; ok {
		t.mu.Unlock()
		return Decision{Reason: "run already decided"}
	}
	chain := t.chain
	schedule := t.schedule
	t.mu.Unlock()

	var already int64
	var lastEnd time.Time
	if chain != nil {
		n, end, err := chain(run.TaskKey, run.ID, run.TriggerSource == model.TriggerAutoRetry)
		if err != nil {
			// 链事实查不出来时不放行:宁可少重试,不可超预算(fail-closed)。
			slog.Warn("auto retry: chain stats unavailable, refuse to retry",
				"run_id", run.ID, "task", run.TaskKey, "error", err)
			return Decision{Reason: "chain stats unavailable"}
		}
		already, lastEnd = n, end
	}

	if int(already) >= p.MaxAutoRetries {
		return Decision{Reason: "max auto retries reached"}
	}
	// 冷却以「上一次链内重试的结束时间」起算;链为空(lastEnd 零值)不冷却。
	if p.Cooldown > 0 && !lastEnd.IsZero() {
		if elapsed := time.Since(lastEnd); elapsed < p.Cooldown {
			d := Decision{
				Deferred:     true,
				RetryAfter:   p.Cooldown - elapsed,
				Attempt:      int(already) + 1,
				AttemptLimit: p.MaxAutoRetries,
				Reason:       "cooldown",
			}
			if schedule != nil {
				if err := schedule(run, d); err != nil {
					// 排程落库失败 ≠ 永久放弃:不标记 decided,保留重判窗口。
					slog.Warn("auto retry: persist cooldown schedule failed",
						"run_id", run.ID, "task", run.TaskKey, "error", err)
					return Decision{Reason: "schedule retry failed"}
				}
				t.markDecided(run.ID)
			}
			return d
		}
	}

	t.markDecided(run.ID)
	return Decision{Retry: true, Attempt: int(already) + 1, AttemptLimit: p.MaxAutoRetries}
}

// markDecided 记录同进程已判定(调用方不得持锁)。
func (t *RetryTracker) markDecided(runID uint) {
	t.mu.Lock()
	t.decided[runID] = struct{}{}
	t.pruneLocked()
	t.mu.Unlock()
}

// pruneLocked 超过 maxKeep 时按 run ID 升序淘汰最老条目（run ID 单调递增）。
func (t *RetryTracker) pruneLocked() {
	if len(t.decided) <= t.maxKeep {
		return
	}
	ids := make([]uint, 0, len(t.decided))
	for id := range t.decided {
		ids = append(ids, id)
	}
	// 升序排序（run ID 单调递增，越小越老），淘汰最老的 extra 条。
	slices.Sort(ids)
	extra := len(ids) - t.maxKeep
	for _, id := range ids[:extra] {
		delete(t.decided, id)
	}
}
