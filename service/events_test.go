package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

func TestRetryTracker_ShouldRetry(t *testing.T) {
	tr := NewRetryTracker(0)
	p := AutoRetryPolicy{MaxAutoRetries: 2, Cooldown: time.Minute}
	run := &model.SyncRun{ID: 1, Status: model.StatusFailed}

	d := tr.ShouldRetry(run, p)
	require.True(t, d.Retry)
	assert.Equal(t, 1, d.Attempt)

	// 冷却期内不重跑
	d = tr.ShouldRetry(run, p)
	assert.False(t, d.Retry)
	assert.Equal(t, "cooldown not elapsed", d.Reason)

	// 冷却过期后放行第二次
	tr.mu.Lock()
	tr.lastAuto[1] = time.Now().Add(-2 * time.Minute)
	tr.mu.Unlock()
	d = tr.ShouldRetry(run, p)
	require.True(t, d.Retry)
	assert.Equal(t, 2, d.Attempt)

	// 超过上限
	tr.mu.Lock()
	tr.lastAuto[1] = time.Now().Add(-2 * time.Minute)
	tr.mu.Unlock()
	d = tr.ShouldRetry(run, p)
	assert.False(t, d.Retry)
	assert.Equal(t, "max auto retries reached", d.Reason)
}

func TestRetryTracker_SkipsNonFailedAndDisabled(t *testing.T) {
	tr := NewRetryTracker(0)
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
	p := AutoRetryPolicy{MaxAutoRetries: 5, Cooldown: 0}
	for id := uint(1); id <= 10; id++ {
		tr.ShouldRetry(&model.SyncRun{ID: id, Status: model.StatusFailed}, p)
	}
	tr.mu.Lock()
	n := len(tr.counts)
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
