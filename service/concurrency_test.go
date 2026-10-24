package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/lock"
)

// TestLocalGuard_TaskMutexAndConcurrency 验证进程内 guard:同 taskKey 互斥 + 全局并发上限。
func TestLocalGuard_TaskMutexAndConcurrency(t *testing.T) {
	g := newLocalGuard(2) // max 2 并发
	ctx := context.Background()

	// 同 taskKey 第二次获取应返回 ErrTaskRunning
	rel, err := g.Acquire(ctx, "taskA")
	require.NoError(t, err, "acquire taskA")

	_, err = g.Acquire(ctx, "taskA")
	require.ErrorIs(t, err, ErrTaskRunning, "second acquire taskA should be ErrTaskRunning")

	// 占满并发(taskB),再取 taskC 应 ErrTooManyConcurrent
	relB, err := g.Acquire(ctx, "taskB")
	require.NoError(t, err, "acquire taskB")

	_, err = g.Acquire(ctx, "taskC")
	require.ErrorIs(t, err, ErrTooManyConcurrent, "acquire taskC should be ErrTooManyConcurrent")

	// 释放 taskA 后空出 1 槽,taskC 可以拿到(占住)
	rel()
	relC, err := g.Acquire(ctx, "taskC")
	require.NoError(t, err, "taskC should acquire after taskA released")

	// 现在 taskB + taskC 占满,taskD 应被拦
	_, err = g.Acquire(ctx, "taskD")
	require.ErrorIs(t, err, ErrTooManyConcurrent, "taskD should be ErrTooManyConcurrent")

	relB()
	relC()
}

// TestLocalGuard_ConcurrentStress 并发场景下不应超卖并发槽。
func TestLocalGuard_ConcurrentStress(t *testing.T) {
	g := newLocalGuard(3)
	ctx := context.Background()
	var wg sync.WaitGroup
	var acquired, running, maxRunning int32
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "task"
			// 大部分用同一 key(被互斥拦),少部分用独立 key(吃并发槽)
			if i%5 == 0 {
				key = "task-" + itoa(i)
			}
			rel, err := g.Acquire(ctx, key)
			if err != nil {
				return
			}
			defer rel()
			mu.Lock()
			acquired++
			running++
			if running > maxRunning {
				maxRunning = running
			}
			mu.Unlock()
			// 模拟一点工作
			// hold briefly
			mu.Lock()
			running--
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	_ = acquired
	require.LessOrEqual(t, maxRunning, int32(3), "concurrency exceeded: maxRunning=%d > 3", maxRunning)
}

// TestRedisGuard_CrossInstanceMutex 是多实例核心测试:
// 两个 guard(模拟两个实例)共享同一 redis,同 taskKey 必须跨实例互斥。
func TestRedisGuard_CrossInstanceMutex(t *testing.T) {
	mr := miniredis.RunT(t)
	g1, err := newRedisGuard(mr.Addr(), "", 0, 2, lock.RedisPoolOptions{})
	require.NoError(t, err, "newRedisGuard g1")
	defer func() { _ = g1.Close() }()

	g2, err := newRedisGuard(mr.Addr(), "", 0, 2, lock.RedisPoolOptions{})
	require.NoError(t, err, "newRedisGuard g2")
	defer func() { _ = g2.Close() }()

	ctx := context.Background()

	// g1 拿到 taskA
	rel1, err := g1.Acquire(ctx, "taskA")
	require.NoError(t, err, "g1 acquire taskA")

	// g2(另一个实例)再拿 taskA 必须失败 —— 跨实例互斥
	_, err = g2.Acquire(ctx, "taskA")
	require.ErrorIs(t, err, ErrTaskRunning, "g2 acquire taskA should be ErrTaskRunning (cross-instance)")

	// g1 释放后 g2 能拿到
	rel1()
	rel2, err := g2.Acquire(ctx, "taskA")
	require.NoError(t, err, "g2 acquire taskA after release")
	rel2()
}

// TestRedisGuard_CrossInstanceConcurrencyCap 跨实例并发上限:
// g1 占满槽,g2 拿不到(即便不同 taskKey)。
func TestRedisGuard_CrossInstanceConcurrencyCap(t *testing.T) {
	mr := miniredis.RunT(t)
	g1, err := newRedisGuard(mr.Addr(), "", 0, 1, lock.RedisPoolOptions{}) // max 1
	require.NoError(t, err, "g1")
	defer func() { _ = g1.Close() }()

	g2, err := newRedisGuard(mr.Addr(), "", 0, 1, lock.RedisPoolOptions{})
	require.NoError(t, err, "g2")
	defer func() { _ = g2.Close() }()

	ctx := context.Background()

	rel1, err := g1.Acquire(ctx, "taskA")
	require.NoError(t, err, "g1 acquire")

	// 不同 taskKey,但全局槽已满 → ErrTooManyConcurrent
	_, err = g2.Acquire(ctx, "taskB")
	require.ErrorIs(t, err, ErrTooManyConcurrent, "g2 should be ErrTooManyConcurrent")

	rel1()
	// 释放后 g2 能拿
	rel2, err := g2.Acquire(ctx, "taskB")
	require.NoError(t, err, "g2 after release")
	rel2()
}

// itoa 轻量整数转字符串,避免引入 strconv 仅为此。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// TestCronLockTTL_ExceedsRenewInterval 钉住 cron 锁的 TTL 必须大于续期间隔。
//
// 回归背景:distlock 的续期间隔是**锁级固定值**(WithRenewInterval),
// 不随每把锁的 TTL 变化。曾经 cron 锁 TTL=10s 而续期间隔也是 10s ⇒
// 第一次续期正好撞上 key 过期,续期必败("lock lost or not owned"),
// 锁从未被续上 ⇒ cron 锁只在开头 10 秒内有效,之后另一实例可以拿到同一把锁
// 并发跑同一个任务(进程内的 "task is already running" 拦不住跨实例)。
func TestCronLockTTL_ExceedsRenewInterval(t *testing.T) {
	require.Greater(t, cronLockTTL, 2*renewInterval,
		"cron 锁 TTL 必须 > 2×续期间隔,否则第一次续期就撞上 key 过期,锁从未被续上")
}

// TestRedisGuard_TryCronLock_KeyTTLOutlivesRenewInterval 从 Redis 侧验证:
// cron 锁的 key TTL 必须长于续期间隔(否则续期在物理上不可能成功)。
func TestRedisGuard_TryCronLock_KeyTTLOutlivesRenewInterval(t *testing.T) {
	mr := miniredis.RunT(t)
	g1, err := newRedisGuard(mr.Addr(), "", 0, 2, lock.RedisPoolOptions{})
	require.NoError(t, err, "newRedisGuard g1")
	defer func() { _ = g1.Close() }()

	ctx := context.Background()
	release, ok, err := g1.TryCronLock(ctx, "taskX", cronLockTTL)
	require.NoError(t, err)
	require.True(t, ok, "cron 锁应当拿到")
	defer release()

	keys := mr.Keys()
	require.NotEmpty(t, keys, "cron 锁应当在 Redis 里留痕")
	found := false
	for _, k := range keys {
		if !strings.Contains(k, "taskX") {
			continue
		}
		found = true
		ttl := mr.TTL(k)
		require.Greater(t, ttl, renewInterval,
			"cron 锁的 key TTL(%s)必须长于续期间隔(%s),否则续期必然失败", ttl, renewInterval)
	}
	require.True(t, found, "未找到 cron 锁的 key:%v", keys)
}

// TestRedisGuard_TryCronLock_ReleaseAllowsNextTick cron 锁必须在本次执行结束后释放。
//
// 回归背景:cron 锁的续期 goroutine 会把 TTL 一直续下去(TTL > 续期间隔时续期是成功的),
// 而旧实现**不释放**这把锁 ⇒ 它变成一把永久锁 ⇒ 之后每次 tick 都拿不到锁,
// task 再也不会被触发(cron 静默停摆)。实测踩过:改完 TTL 后 cron 从每分钟一次变成完全不动。
func TestRedisGuard_TryCronLock_ReleaseAllowsNextTick(t *testing.T) {
	mr := miniredis.RunT(t)
	g1, err := newRedisGuard(mr.Addr(), "", 0, 2, lock.RedisPoolOptions{})
	require.NoError(t, err, "newRedisGuard g1")
	defer func() { _ = g1.Close() }()
	g2, err := newRedisGuard(mr.Addr(), "", 0, 2, lock.RedisPoolOptions{})
	require.NoError(t, err, "newRedisGuard g2")
	defer func() { _ = g2.Close() }()

	ctx := context.Background()
	release, ok, err := g1.TryCronLock(ctx, "taskY", cronLockTTL)
	require.NoError(t, err)
	require.True(t, ok, "g1 应当拿到 cron 锁")

	// 锁被持有时,另一个实例拿不到
	_, ok2, err := g2.TryCronLock(ctx, "taskY", cronLockTTL)
	require.NoError(t, err)
	require.False(t, ok2, "锁被持有时别的实例拿不到")

	// g1 释放后,g2 应当能拿到(否则下一次 tick 永远被跳过 ⇒ 任务停摆)
	release()
	_, ok3, err := g2.TryCronLock(ctx, "taskY", cronLockTTL)
	require.NoError(t, err)
	require.True(t, ok3, "释放后别的实例必须能拿到 cron 锁,否则下一次 tick 会被永远跳过")
}
