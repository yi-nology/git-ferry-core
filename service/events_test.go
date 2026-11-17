package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// fakeChain 造一个可控的链统计函数:返回预设的链内条数与最近结束时间。
func fakeChain(count int64, lastEnd time.Time, err error) chainStatsFunc {
	return func(taskKey string, runID uint, runIsRetry bool) (int64, time.Time, error) {
		return count, lastEnd, err
	}
}

func TestRetryTracker_ShouldRetry(t *testing.T) {
	tr := NewRetryTracker(0)
	tr.SetChainStats(fakeChain(0, time.Time{}, nil))
	p := AutoRetryPolicy{MaxAutoRetries: 2, Cooldown: 0}
	run := &model.SyncRun{ID: 1, TaskKey: "k", Status: model.StatusFailed}

	d := tr.ShouldRetry(run, p)
	require.True(t, d.Retry)
	assert.Equal(t, 1, d.Attempt)

	// 同一运行只判定一次(订阅+轮询双通道投递去重)
	d = tr.ShouldRetry(run, p)
	assert.False(t, d.Retry)
	assert.Equal(t, "run already decided", d.Reason)

	// 链内已有 1 条 auto_retry ⇒ 放行第 2 次
	tr = NewRetryTracker(0)
	tr.SetChainStats(fakeChain(1, time.Time{}, nil))
	d = tr.ShouldRetry(&model.SyncRun{ID: 2, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	require.True(t, d.Retry)
	assert.Equal(t, 2, d.Attempt)

	// 达到预算上限
	tr = NewRetryTracker(0)
	tr.SetChainStats(fakeChain(2, time.Time{}, nil))
	d = tr.ShouldRetry(&model.SyncRun{ID: 3, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	assert.False(t, d.Retry)
	assert.Equal(t, "max auto retries reached", d.Reason)
}

func TestRetryTracker_Cooldown(t *testing.T) {
	p := AutoRetryPolicy{MaxAutoRetries: 5, Cooldown: time.Minute}

	// 链内最近一条重试刚结束 ⇒ 冷却中:不是「永久不重试」,而是
	// Deferred + 剩余等待时间(冷却只推迟、不吞预算,#39)。
	tr := NewRetryTracker(0)
	tr.SetChainStats(fakeChain(1, time.Now(), nil))
	d := tr.ShouldRetry(&model.SyncRun{ID: 1, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	assert.False(t, d.Retry)
	assert.True(t, d.Deferred, "冷却期必须标记为可延后,而不是永久拒绝")
	assert.Positive(t, d.RetryAfter, "必须给出剩余等待时间")
	assert.Equal(t, "cooldown", d.Reason)
	assert.Equal(t, 2, d.Attempt, "冷却内失败不消耗预算,下次仍是第 2 次重跑")

	// 冷却已过 ⇒ 放行(冷却以链内最近一条重试的**结束时间**起算,
	// 用开始时间会让"失败后立即判定"永远撞上冷却,链被冻死)
	tr = NewRetryTracker(0)
	tr.SetChainStats(fakeChain(1, time.Now().Add(-2*time.Minute), nil))
	d = tr.ShouldRetry(&model.SyncRun{ID: 2, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	assert.True(t, d.Retry)
}

// TestRetryTracker_CooldownDoesNotSwallowBudget 验收(#39):
// Cooldown>0、MaxAutoRetries=3、每次失败都立即返回 ⇒ 冷却只在两次重跑
// 之间推迟,最终恰好吃满 3 次重跑预算,第 4 次明确 "max auto retries reached"。
func TestRetryTracker_CooldownDoesNotSwallowBudget(t *testing.T) {
	p := AutoRetryPolicy{MaxAutoRetries: 3, Cooldown: time.Minute}
	chainLen := int64(0)
	lastEnd := time.Time{}

	scheduled := 0
	tr := NewRetryTracker(0)
	tr.SetChainStats(func(string, uint, bool) (int64, time.Time, error) {
		return chainLen, lastEnd, nil
	})
	tr.SetRetryScheduler(func(run *model.SyncRun, d Decision) error {
		scheduled++
		assert.True(t, d.Deferred)
		return nil
	})

	// R1:链头失败,链为空 ⇒ 立即重跑(第 1 次)。
	d := tr.ShouldRetry(&model.SyncRun{ID: 1, TaskKey: "k", Status: model.StatusFailed}, p)
	require.True(t, d.Retry)
	assert.Equal(t, 1, d.Attempt)

	// R2 立即失败:冷却中 ⇒ 延后,预算仍为 0/3。
	chainLen, lastEnd = 1, time.Now()
	d = tr.ShouldRetry(&model.SyncRun{ID: 2, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	require.True(t, d.Deferred)
	assert.Equal(t, 2, d.Attempt, "R2 之后下一次是第 2 次重跑")
	assert.Equal(t, 1, scheduled)

	// R3 立即失败:仍是冷却延期,下一次是第 3 次重跑。
	chainLen, lastEnd = 2, time.Now()
	d = tr.ShouldRetry(&model.SyncRun{ID: 3, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	require.True(t, d.Deferred)
	assert.Equal(t, 3, d.Attempt)
	assert.Equal(t, 2, scheduled)

	// R4 失败:3 次重跑已执行完 ⇒ 明确预算耗尽(不是 cooldown 假死)。
	chainLen, lastEnd = 3, time.Now()
	d = tr.ShouldRetry(&model.SyncRun{ID: 4, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}, p)
	assert.False(t, d.Retry)
	assert.False(t, d.Deferred)
	assert.Equal(t, "max auto retries reached", d.Reason)
	assert.Equal(t, 2, scheduled, "预算耗尽不得再排程")
}

// TestRetryTracker_CooldownScheduleFailureKeepsRetryable 排程落库失败时
// 不得标记 decided(否则该失败运行永久失去重试机会)。
func TestRetryTracker_CooldownScheduleFailureKeepsRetryable(t *testing.T) {
	p := AutoRetryPolicy{MaxAutoRetries: 3, Cooldown: time.Minute}
	tr := NewRetryTracker(0)
	tr.SetChainStats(fakeChain(1, time.Now(), nil))
	tr.SetRetryScheduler(func(*model.SyncRun, Decision) error { return assert.AnError })
	run := &model.SyncRun{ID: 9, TaskKey: "k", TriggerSource: model.TriggerAutoRetry, Status: model.StatusFailed}
	d := tr.ShouldRetry(run, p)
	assert.False(t, d.Retry)
	assert.Equal(t, "schedule retry failed", d.Reason)
	// 未标记 decided ⇒ 下一次仍会尝试落库(而不是 "run already decided")。
	d = tr.ShouldRetry(run, p)
	assert.Equal(t, "schedule retry failed", d.Reason)
}

func TestRetryTracker_ChainUnavailableRefuses(t *testing.T) {
	// 链事实查不出来 ⇒ fail-closed:宁可少重试,不可超预算
	tr := NewRetryTracker(0)
	tr.SetChainStats(fakeChain(0, time.Time{}, assert.AnError))
	d := tr.ShouldRetry(&model.SyncRun{ID: 1, TaskKey: "k", Status: model.StatusFailed},
		AutoRetryPolicy{MaxAutoRetries: 2, Cooldown: 0})
	assert.False(t, d.Retry)
	assert.Equal(t, "chain stats unavailable", d.Reason)
}

func TestRetryTracker_SkipsNonFailedAndDisabled(t *testing.T) {
	tr := NewRetryTracker(0)
	tr.SetChainStats(fakeChain(0, time.Time{}, nil))
	p := AutoRetryPolicy{MaxAutoRetries: 3, Cooldown: time.Millisecond}

	d := tr.ShouldRetry(&model.SyncRun{ID: 2, Status: model.StatusSuccess}, p)
	assert.False(t, d.Retry)
	assert.Equal(t, "run not failed", d.Reason)

	d = tr.ShouldRetry(&model.SyncRun{ID: 2, Status: model.StatusFailed}, AutoRetryPolicy{})
	assert.False(t, d.Retry)
	assert.Equal(t, "auto retry disabled", d.Reason)

	d = tr.ShouldRetry(nil, p)
	assert.False(t, d.Retry)
}

func TestRetryTracker_Prune(t *testing.T) {
	tr := NewRetryTracker(3)
	tr.SetChainStats(fakeChain(0, time.Time{}, nil))
	p := AutoRetryPolicy{MaxAutoRetries: 5, Cooldown: 0}
	for id := uint(1); id <= 10; id++ {
		tr.ShouldRetry(&model.SyncRun{ID: id, Status: model.StatusFailed}, p)
	}
	tr.mu.Lock()
	n := len(tr.decided)
	tr.mu.Unlock()
	assert.LessOrEqual(t, n, 3, "超过 maxKeep 应淘汰最老条目")
}

func TestSubscribeRuns_DeliversAndCancels(t *testing.T) {
	svc := &Service{}
	var mu sync.Mutex
	var got []uint

	cancel := svc.SubscribeRuns(func(ev RunEvent) {
		mu.Lock()
		got = append(got, ev.Run.ID)
		mu.Unlock()
	})

	svc.emitRunCompleted("k1", "manual", &model.SyncRun{ID: 7, Status: model.StatusSuccess}, nil)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1 && got[0] == 7
	}, time.Second, 5*time.Millisecond, "应收到完成事件")

	cancel()
	svc.emitRunCompleted("k1", "manual", &model.SyncRun{ID: 8}, nil)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Len(t, got, 1, "取消后不应再收到事件")
	mu.Unlock()

	// 取消幂等
	cancel()
	svc.runSubMu.Lock()
	assert.Empty(t, svc.runSubs)
	svc.runSubMu.Unlock()
}

func TestSubscribeRuns_NilRunAndPanic(t *testing.T) {
	svc := &Service{}
	done := make(chan struct{}, 1)
	svc.SubscribeRuns(func(ev RunEvent) {
		// Run 为 nil（CreateRun 失败）时也应送达，由订阅方自行判空
		if ev.Run == nil {
			done <- struct{}{}
		}
		panic("subscriber boom") // panic 不应影响其它订阅者/主流程
	})
	second := make(chan struct{}, 1)
	svc.SubscribeRuns(func(ev RunEvent) { second <- struct{}{} })

	svc.emitRunCompleted("k", "cron", nil, assert.AnError)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("nil run 事件未送达")
	}
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("panic 的订阅者不应影响其他订阅者")
	}
}

func TestSubscribeRuns_NoSubscribersNoPanic(t *testing.T) {
	svc := &Service{}
	svc.emitRunCompleted("k", "manual", &model.SyncRun{ID: 1}, nil) // 无订阅者直接返回
}

// TestFinishRun_ReleasesBeforeNotifying finishRun 必须先释放执行权再广播事件。
//
// 回归背景(P0):旧 RunTaskAsync 在 release 之前 emit —— 订阅方(runwatch)
// 收到失败完成事件后立即触发自动重跑,执行权还没放 ⇒ 重跑撞上
// ErrTaskRunning 被拒,且 RetryTracker 已记账 ⇒ 预算白烧、重试未发生。
// 用例是反向验证:把 finishRun 里 emit 挪回 release 之前,本用例立刻红。
func TestFinishRun_ReleasesBeforeNotifying(t *testing.T) {
	const key = "finish-order"
	svc, release := newPendingTestService(t, key)

	acquired := make(chan error, 1)
	svc.SubscribeRuns(func(ev RunEvent) {
		// 订阅回调里立刻抢同任务执行权:此时 finishRun 必须已 release。
		r, err := svc.guard.Acquire(context.Background(), key)
		if err == nil {
			r()
		}
		acquired <- err
	})

	svc.finishRun(key, model.TriggerManual, &model.SyncRun{ID: 9, TaskKey: key}, nil, release)
	select {
	case err := <-acquired:
		assert.NoError(t, err, "订阅回调收到事件时必须已释放执行权(否则自动重跑会撞忙)")
	case <-time.After(2 * time.Second):
		t.Fatal("未收到完成事件")
	}
}
