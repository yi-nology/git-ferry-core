package service

import (
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

// RetryTracker 自动重跑计数器（进程内；多实例部署时各实例只认自己触发的重跑，
// 与 runwatch 历史行为一致）。
type RetryTracker struct {
	mu       sync.Mutex
	counts   map[uint]int       // runID -> 已自动重跑次数
	lastAuto map[uint]time.Time // runID -> 上次自动重跑时间
	// maxKeep 淘汰阈值，防止 run ID 无限增长
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
		counts:   map[uint]int{},
		lastAuto: map[uint]time.Time{},
		maxKeep:  maxKeep,
	}
}

// Decision 一次判定结果。
type Decision struct {
	// Retry 是否应触发自动重跑
	Retry bool
	// Attempt 本次将要进行的第几次重跑（1 起）
	Attempt int
	// Reason 不重跑时的原因（可日志输出）
	Reason string
}

// ShouldRetry 判定该失败运行是否应自动重跑，并在放行时记账。
// 只对 Status=failed 的运行放行；成功运行直接跳过。
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
	defer t.mu.Unlock()
	n := t.counts[run.ID]
	if n >= p.MaxAutoRetries {
		return Decision{Reason: "max auto retries reached"}
	}
	last := t.lastAuto[run.ID]
	if !last.IsZero() && time.Since(last) < p.Cooldown {
		return Decision{Reason: "cooldown not elapsed"}
	}
	t.counts[run.ID] = n + 1
	t.lastAuto[run.ID] = time.Now()
	t.pruneLocked()
	return Decision{Retry: true, Attempt: n + 1}
}

// pruneLocked 超过 maxKeep 时按 run ID 升序淘汰最老条目（run ID 单调递增）。
func (t *RetryTracker) pruneLocked() {
	if len(t.counts) <= t.maxKeep {
		return
	}
	ids := make([]uint, 0, len(t.counts))
	for id := range t.counts {
		ids = append(ids, id)
	}
	// 简单选择：只在超限时排序一次
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	extra := len(ids) - t.maxKeep
	for _, id := range ids[:extra] {
		delete(t.counts, id)
		delete(t.lastAuto, id)
	}
}
