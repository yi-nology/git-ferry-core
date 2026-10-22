package lock

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/oekit/redisx/distlock"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// newMiniredis 启动一个进程内 redis,测试始终可运行(不依赖外部 redis)。
func newMiniredis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// TestNewLocker_KeyLayout_miniredis 钉住跨实例锁的 key 布局:distlock 必须落在
// "git-sync:lock:<key>" 下,与迁移前自写实现一致,否则滚动升级期间新老实例各持一把锁。
// 同时验证 ownerID 校验:非持有者无法解锁。
func TestNewLocker_KeyLayout_miniredis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	locker := NewLocker(client)
	ownerID, err := locker.TryLock(ctx, "task:k1", 2*time.Second)
	require.NoError(t, err, "TryLock")
	require.NotEmpty(t, ownerID)
	require.True(t, mr.Exists("git-sync:lock:task:k1"), "lock key layout must stay git-sync:lock:<key>")

	_, err = locker.TryLock(ctx, "task:k1", 2*time.Second)
	require.ErrorIs(t, err, distlock.ErrLockNotAcquired, "second holder must be rejected")

	require.NoError(t, locker.Unlock(ctx, "task:k1", "wrong-owner"), "non-owner unlock is a no-op, not an error")
	require.True(t, mr.Exists("git-sync:lock:task:k1"), "non-owner must not release the lock")

	require.NoError(t, locker.Unlock(ctx, "task:k1", ownerID))
	require.False(t, mr.Exists("git-sync:lock:task:k1"), "owner unlock releases the lock")
}

// TestSemaphore_RespectsMax_miniredis 验证不超过 max,release 后释放槽。
func TestSemaphore_RespectsMax_miniredis(t *testing.T) {
	client := newMiniredis(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	sem := NewSemaphore(client, "sem-max", 2, 5*time.Second)

	ok, _ := sem.Acquire(ctx, "a")
	require.True(t, ok, "a should acquire")

	ok, _ = sem.Acquire(ctx, "b")
	require.True(t, ok, "b should acquire")

	ok, _ = sem.Acquire(ctx, "c")
	require.False(t, ok, "c should fail (max=2)")

	err := sem.Release(ctx, "a")
	require.NoError(t, err, "release a")

	ok, _ = sem.Acquire(ctx, "c")
	require.True(t, ok, "c should acquire after a released")
}

// TestSemaphore_RenewKeepsSlot_miniredis 验证 Renew 让槽活过原始 TTL;
// 不续期则槽到期被清理。
func TestSemaphore_RenewKeepsSlot_miniredis(t *testing.T) {
	client := newMiniredis(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	sem := NewSemaphore(client, "sem-renew", 1, 300*time.Millisecond)

	ok, _ := sem.Acquire(ctx, "a")
	require.True(t, ok, "a should acquire")

	ok, _ = sem.Acquire(ctx, "b")
	require.False(t, ok, "b should fail while a holds the only slot")

	// 续期 a 几次,撑过原始 TTL
	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		renewed, err := sem.Renew(ctx, "a")
		require.NoError(t, err, "renew a")
		require.True(t, renewed, "slot must still be alive to renew")
	}
	// 此时已过 450ms > 300ms 原始 TTL,但续期后 a 仍存活
	ok, _ = sem.Acquire(ctx, "b")
	require.False(t, ok, "b should still fail while a is renewed")

	// 不再续期,等过期后 b 应能拿到
	time.Sleep(500 * time.Millisecond)
	ok, _ = sem.Acquire(ctx, "b")
	require.True(t, ok, "b should acquire after a's slot expired")
}

// TestSemaphore_KeyLayout_miniredis 钉住信号量的 Redis key 布局:
// 必须落在 "git-sync:semaphore:<key>" 下,与迁移前自写实现一致,
// 否则滚动升级期间新老实例各看各的信号量,全局并发上限翻倍。
func TestSemaphore_KeyLayout_miniredis(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	sem := NewSemaphore(client, "sync-tasks", 2, 5*time.Second)
	require.Equal(t, "git-sync:semaphore:sync-tasks", sem.key, "semaphore key layout must stay git-sync:semaphore:<key>")
	// 占一个槽才会真正建出 ZSET key
	ok, err := sem.Acquire(context.Background(), "worker-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, mr.Exists("git-sync:semaphore:sync-tasks"), "semaphore key layout must stay git-sync:semaphore:<key>")
}

// TestSemaphore_RenewDoesNotResurrectExpiredSlot_miniredis 钉住上游更严的续期语义:
// 已过期的槽不得借 Renew 复活——它的配额可能已被其他 Acquire 接管,
// 复活即超限。迁移前自写实现的 renewScript 只判「成员存在」,不判是否过期。
func TestSemaphore_RenewDoesNotResurrectExpiredSlot_miniredis(t *testing.T) {
	client := newMiniredis(t)
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	sem := NewSemaphore(client, "sem-resurrect", 1, 200*time.Millisecond)

	ok, err := sem.Acquire(ctx, "a")
	require.NoError(t, err)
	require.True(t, ok, "a should acquire")

	// 等槽过期
	time.Sleep(300 * time.Millisecond)

	renewed, err := sem.Renew(ctx, "a")
	require.NoError(t, err, "Renew on an expired slot must not error")
	require.False(t, renewed, "expired slot must not be resurrected by Renew")

	// 配额确实让出给了 b,a 不能凭 Renew 抢回
	ok, err = sem.Acquire(ctx, "b")
	require.NoError(t, err)
	require.True(t, ok, "b should acquire the freed slot")
}
