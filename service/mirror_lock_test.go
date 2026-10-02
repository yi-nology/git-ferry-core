package service

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/lock"
)

const mirrorTestKey = "mirror-channel-1"

// TestMirrorLockChannel_CrossInstance 另一实例先占住 redis 锁时，本进程报忙碌；
// 对方释放后可正常获取。不同通道互不影响。
func TestMirrorLockChannel_CrossInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	m := &MirrorService{redisLock: lock.NewRedisLock(mr.Addr(), "", 0), lockTTL: time.Minute}
	ctx := context.Background()

	// 模拟"另一实例"先占住同通道
	other := lock.NewRedisLock(mr.Addr(), "", 0)
	ok, err := other.TryLockWithTTL(ctx, mirrorTestKey, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	_, err = m.lockChannel(ctx, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "其它实例")

	// 不同通道不受影响
	release2, err := m.lockChannel(ctx, 2)
	require.NoError(t, err)
	release2()

	// 对方释放后可获取
	require.NoError(t, other.Unlock(ctx, mirrorTestKey))
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
	m := &MirrorService{redisLock: lock.NewRedisLock("127.0.0.1:1", "", 0), lockTTL: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	release, err := m.lockChannel(ctx, 3)
	require.NoError(t, err, "redis 抖动应降级而非失败")
	release()
}
