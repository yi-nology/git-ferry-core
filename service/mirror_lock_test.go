package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/oekit/redisx/distlock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/lock"
)

const mirrorTestKey = "mirror-channel-1"

// TestMirrorLockChannel_CrossInstance 另一实例先占住 redis 锁时，本进程报忙碌；
// 对方释放后可正常获取。不同通道互不影响。
func TestMirrorLockChannel_CrossInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	m := &MirrorService{locker: lock.NewLocker(lock.NewRedisClient(mr.Addr(), "", 0)), lockTTL: time.Minute}
	ctx := context.Background()

	// 模拟"另一实例"先占住同通道
	other := lock.NewLocker(lock.NewRedisClient(mr.Addr(), "", 0))
	otherID, err := other.TryLock(ctx, mirrorTestKey, time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, otherID)

	_, err = m.lockChannel(ctx, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "其它实例")

	// 不同通道不受影响
	release2, err := m.lockChannel(ctx, 2)
	require.NoError(t, err)
	release2()

	// 对方释放后可获取
	require.NoError(t, other.Unlock(ctx, mirrorTestKey, otherID))
	release, err := m.lockChannel(ctx, 1)
	require.NoError(t, err)
	release()
	release() // release 幂等
}

// TestMirrorLockChannel_LocalOnly 未配 redis：纯进程内互斥，串行获取/释放正常。
func TestMirrorLockChannel_LocalOnly(t *testing.T) {
	m := &MirrorService{}
	ctx := context.Background()

	release, err := m.lockChannel(ctx, 7)
	require.NoError(t, err)

	acquired := make(chan struct{})
	go func() {
		r, gerr := m.lockChannel(ctx, 7)
		if gerr == nil {
			close(acquired)
			r()
		}
	}()
	select {
	case <-acquired:
		t.Fatal("进程内锁未互斥")
	case <-time.After(80 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("释放后未被获取")
	}
}

// TestMirrorLockChannel_Degraded Redis 不可用时降级为进程内互斥，不阻断任务。
func TestMirrorLockChannel_Degraded(t *testing.T) {
	// 指向不存在的地址：TryLock 出错 → 降级路径
	m := &MirrorService{locker: lock.NewLocker(lock.NewRedisClient("127.0.0.1:1", "", 0)), lockTTL: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	release, err := m.lockChannel(ctx, 3)
	require.NoError(t, err, "redis 抖动应降级而非失败")
	release()
}

// fakeRemoteLocker 可控的跨实例锁 fake:err 非 nil 时 TryLock 恒定返回它。
type fakeRemoteLocker struct{ err error }

func (f fakeRemoteLocker) TryLock(_ context.Context, _ string, _ time.Duration) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "owner", nil
}

func (f fakeRemoteLocker) Unlock(_ context.Context, _, _ string) error { return nil }

// TestMirrorLockChannel_FailClosed mirror_lock_fail_closed=true 时,
// Redis 非竞争类错误必须拒绝执行(不进入互斥区),而不是静默降级。
//
// 回归背景(P0):旧实现 Redis 异常一律降级为进程内互斥 ⇒ 多实例下两个实例
// 可同时进入本应互斥的镜像发布流程。
func TestMirrorLockChannel_FailClosed(t *testing.T) {
	down := errors.New("redis connection refused")

	// fail-closed=true:Redis 异常 ⇒ 忙碌拒绝,**不许**进入互斥区
	m := &MirrorService{locker: fakeRemoteLocker{err: down}, lockFailClosed: true, lockTTL: time.Second}
	release, err := m.lockChannel(context.Background(), 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fail_closed")
	release() // 未获取时也返回 noop release,不得 panic

	// 竞争失败(ErrLockNotAcquired)与策略无关,恒为忙碌
	busy := &MirrorService{locker: fakeRemoteLocker{err: distlock.ErrLockNotAcquired}, lockTTL: time.Second}
	_, err = busy.lockChannel(context.Background(), 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "其它实例")

	// fail-closed=false(默认):Redis 异常降级进程内互斥,可获取(旧语义不变)
	m3 := &MirrorService{locker: fakeRemoteLocker{err: down}, lockTTL: time.Second}
	release3, err := m3.lockChannel(context.Background(), 5)
	require.NoError(t, err)
	release3()
}
