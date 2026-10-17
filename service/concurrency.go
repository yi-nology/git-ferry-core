package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/redisx/distlock"
	"github.com/redis/go-redis/v9"
	"github.com/yi-nology/git-ferry-core/lock"
	"github.com/yi-nology/git-ferry-core/model"
)

// releaseFunc 释放 Acquire 拿到的执行权(任务互斥锁 + 全局并发槽),由调用方 defer。
type releaseFunc func()

// concurrencyGuard 统一封装“同 taskKey 互斥 + 全局并发上限”。
// 单实例(无 redis)用 localGuard;多实例(配 redis)用 redisGuard。
type concurrencyGuard interface {
	// Acquire 尝试获取 taskKey 的执行权 + 一个全局并发槽。
	// 成功返回 release;taskKey 已在跑返回 ErrTaskRunning;全局满返回 ErrTooManyConcurrent。
	Acquire(ctx context.Context, taskKey string) (releaseFunc, error)
	// TryCronLock 尝试获取 cron 分布式锁(短 TTL),防止多实例重复触发。
	// 本地模式始终返回 true;Redis 模式用 SetNX 实现。
	TryCronLock(ctx context.Context, taskKey string, ttl time.Duration) (bool, error)
	// Ping 检查后端连通性(本地实现始终返回 nil)。
	Ping() error
	Close() error
}

const (
	taskLockTTL    = 30 * time.Second // 任务锁 TTL,watchdog 周期续期
	taskLockKeyPfx = "task:"          // redis 任务锁 key 前缀
	semSlotKey     = "sync-tasks"     // 全局并发信号量 key
	semSlotTTL     = 30 * time.Second // 信号量槽 TTL,watchdog 续期
	renewInterval  = 10 * time.Second // watchdog 续期间隔(< TTL/2)
)

// ---------------- 本地(进程内)实现 ----------------

// localGuard 单实例并发控制:sync.Map 做 taskKey 互斥,buffered channel 做全局并发上限。
type localGuard struct {
	runningTasks sync.Map
	sem          chan struct{}
}

func newLocalGuard(maxConcurrent int) *localGuard {
	if maxConcurrent <= 0 {
		maxConcurrent = model.DefaultMaxConcurrent
	}
	return &localGuard{sem: make(chan struct{}, maxConcurrent)}
}

func (g *localGuard) Acquire(_ context.Context, taskKey string) (releaseFunc, error) {
	select {
	case g.sem <- struct{}{}:
	default:
		return nil, ErrTooManyConcurrent
	}
	// 只有成功 store 才注册 Delete,避免误删别的执行流的标记
	if _, loaded := g.runningTasks.LoadOrStore(taskKey, struct{}{}); loaded {
		<-g.sem
		return nil, ErrTaskRunning
	}
	return func() {
		g.runningTasks.Delete(taskKey)
		<-g.sem
	}, nil
}

func (g *localGuard) Ping() error  { return nil }
func (g *localGuard) Close() error { return nil }
func (g *localGuard) TryCronLock(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil // 本地模式始终成功
}

// ---------------- 分布式(redis)实现 ----------------

// redisGuard 多实例并发控制:distlock 做 taskKey 互斥(库内自动续期),ZSET 信号量做全局并发上限
// (watchdog 续期)。进程崩溃后锁/槽在 TTL 内自动释放。
type redisGuard struct {
	client *redis.Client
	locker *distlock.RedisLocker
	sem    *lock.Semaphore
}

func newRedisGuard(addr, password string, db, maxConcurrent int, poolOpts lock.RedisPoolOptions) (*redisGuard, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = model.DefaultMaxConcurrent
	}
	client := lock.NewRedisClient(addr, password, db, lock.WithPoolOptions(poolOpts))
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	// WithMaxAutoRenew(-1) 取消 distlock 默认的 10×TTL 续期上限:同步任务可能跑数小时
	// (大仓库克隆),上限一到锁会在任务中途静默过期,被别的实例重复执行。
	// 锁的生命周期由 release 里的 Unlock 显式终止。
	locker := lock.NewLocker(client,
		distlock.WithAutoRenew(),
		distlock.WithRenewInterval(renewInterval),
		distlock.WithMaxAutoRenew(-1),
	)
	sem := lock.NewSemaphore(client, semSlotKey, maxConcurrent, semSlotTTL)
	return &redisGuard{client: client, locker: locker, sem: sem}, nil
}

func (g *redisGuard) Acquire(ctx context.Context, taskKey string) (releaseFunc, error) {
	// 1) 全局并发槽
	semID := uuid.NewString()
	ok, err := g.sem.Acquire(ctx, semID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrTooManyConcurrent
	}

	// 2) 同 taskKey 互斥锁:timeout=0 即单次尝试,被占直接返回而不排队
	lockKey := taskLockKeyPfx + taskKey
	ownerID, err := g.locker.Lock(ctx, lockKey, taskLockTTL, 0)
	if err != nil {
		_ = g.sem.Release(ctx, semID)
		if errors.Is(err, distlock.ErrLockTimeout) {
			return nil, ErrTaskRunning
		}
		return nil, err
	}

	// 3) watchdog 续期信号量槽,避免长任务期间槽过期被回收(任务锁由 distlock 自动续期)
	stop := make(chan struct{})
	async.GoSafe(func() { g.watchdog(semID, stop) })

	return func() {
		close(stop)
		// 用带超时的独立 context 释放,避免 Redis 不可响应时 goroutine 永久挂死
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		if err := g.locker.Unlock(releaseCtx, lockKey, ownerID); err != nil {
			slog.Warn("release: failed to unlock", "lockKey", lockKey, "error", err)
		}
		if err := g.sem.Release(releaseCtx, semID); err != nil {
			slog.Warn("release: failed to release semaphore slot", "semID", semID, "error", err)
		}
	}, nil
}

// watchdog 周期性续期持有的信号量槽。stop 关闭即退出。
// 使用 context.Background() 是有意为之:续期需独立于请求上下文运行,直到 stop 被关闭。
func (g *redisGuard) watchdog(semID string, stop <-chan struct{}) {
	t := time.NewTicker(renewInterval)
	defer t.Stop()
	ctx := context.Background() //nolint:gosec // watchdog goroutine intentionally uses background context
	failures := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := g.sem.Renew(ctx, semID); err != nil {
				failures++
				if failures%3 == 1 { // 每 3 次失败记一条日志,避免刷屏
					slog.Warn("watchdog: failed to renew semaphore slot", "semID", semID, "error", err, "consecutive_failures", failures)
				}
				continue
			}
			failures = 0
		}
	}
}

func (g *redisGuard) Ping() error {
	return g.client.Ping(context.Background()).Err()
}

// TryCronLock 用短 TTL 的 cron 锁防止多实例重复触发;锁不显式释放,到期自然过期。
func (g *redisGuard) TryCronLock(ctx context.Context, taskKey string, ttl time.Duration) (bool, error) {
	if _, err := g.locker.TryLock(ctx, "cron:"+taskKey, ttl); err != nil {
		if errors.Is(err, distlock.ErrLockNotAcquired) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (g *redisGuard) Close() error {
	return g.client.Close()
}

// newGuard 按是否配置 redis 选择实现。配了 redis 用分布式,否则用进程内。
func newGuard(redisAddr, redisPassword string, redisDB, maxConcurrent int, poolOpts lock.RedisPoolOptions) (concurrencyGuard, error) {
	if redisAddr != "" {
		rg, err := newRedisGuard(redisAddr, redisPassword, redisDB, maxConcurrent, poolOpts)
		if err != nil {
			// redis 连不上不应让整个服务起不来:回退到本地模式并告警
			return nil, fmt.Errorf("init redis guard failed (check redis config or remove redis.addr to use single-instance mode): %w", err)
		}
		return rg, nil
	}
	return newLocalGuard(maxConcurrent), nil
}
