// Package lock 提供同步任务的并发控制原语:
//   - 跨实例互斥锁:oekit redisx/distlock(SetNX + Lua 原子解锁 + 自动续期),
//     本包只负责固定 key 前缀与客户端构造;
//   - Semaphore:基于 redis ZSET 的分布式信号量,限制全局并发上限;
//   - LocalLock:单实例(未配 redis)时的进程内互斥。
package lock

import (
	"context"
	"fmt"
	"sync"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/oekit/redisx/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	defaultLockTTL = 30 * time.Second

	// LockKeyPrefix 是跨实例互斥锁的 key 前缀。distlock 以 "<prefix>:<key>" 落 key,
	// 与迁移前自写实现的 "git-sync:lock:<key>" 布局一致:滚动升级期间新老实例
	// 看到的是同一把锁,不会出现两边各持一把而同时执行的窗口。
	LockKeyPrefix = "git-sync:lock"
)

var semaphoreAcquireScript = redis.NewScript(`
local key = KEYS[1]
local member = ARGV[1]
local max = tonumber(ARGV[2])
local now = tonumber(ARGV[3])      -- 当前时间(毫秒)
local expire = tonumber(ARGV[4])   -- 槽过期时间(毫秒)

-- 清理已过期的成员(score = 过期毫秒时间戳 <= now 即已过期)
redis.call('ZREMRANGEBYSCORE', key, '-inf', now)

local current = redis.call('ZCARD', key)
if current >= max then
    return 0
end

-- 占用一个槽,score 设为过期时间戳,到点未续期则被清理(进程崩溃也不会永久占槽)
redis.call('ZADD', key, expire, member)
return 1
`)

// RedisPoolOptions 连接池可选配置,零值使用 go-redis 默认。
type RedisPoolOptions struct {
	PoolSize        int // 连接池大小,默认 10*GOMAXPROCS
	MinIdleConns    int // 最小空闲连接数,默认 0(冷启动无预热)
	DialTimeoutSec  int // 建连超时秒数,默认使用 OS TCP 超时
	ReadTimeoutSec  int // 读超时秒数
	WriteTimeoutSec int // 写超时秒数
}

// Option functional option for NewRedisClient.
type Option func(*redis.Options)

// WithPoolOptions 设置连接池参数。
func WithPoolOptions(pool RedisPoolOptions) Option {
	return func(o *redis.Options) {
		if pool.PoolSize > 0 {
			o.PoolSize = pool.PoolSize
		}
		if pool.MinIdleConns > 0 {
			o.MinIdleConns = pool.MinIdleConns
		}
		if pool.DialTimeoutSec > 0 {
			o.DialTimeout = time.Duration(pool.DialTimeoutSec) * time.Second
		}
		if pool.ReadTimeoutSec > 0 {
			o.ReadTimeout = time.Duration(pool.ReadTimeoutSec) * time.Second
		}
		if pool.WriteTimeoutSec > 0 {
			o.WriteTimeout = time.Duration(pool.WriteTimeoutSec) * time.Second
		}
	}
}

// NewRedisClient 按地址与连接池参数构造 go-redis 客户端,供 NewLocker / NewSemaphore 共用。
func NewRedisClient(addr, password string, db int, opts ...Option) *redis.Client {
	o := &redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	}
	for _, opt := range opts {
		opt(o)
	}
	return redis.NewClient(o)
}

// NewLocker 构造跨实例互斥锁,key 前缀固定为 LockKeyPrefix;续期等策略由调用方按场景追加。
func NewLocker(client redis.UniversalClient, opts ...distlock.RedisLockerOption) *distlock.RedisLocker {
	all := append([]distlock.RedisLockerOption{distlock.WithPrefix(LockKeyPrefix)}, opts...)
	return distlock.NewRedisLocker(client, all...)
}

type Semaphore struct {
	client  *redis.Client
	key     string
	max     int
	slotTTL time.Duration // 单个槽的存活时长,持有期间需 Renew 续期,否则到期自动释放
}

// NewSemaphore 创建一个基于 redis ZSET 的分布式信号量。
// slotTTL 为单个槽的存活时长:持有者需周期性 Renew,进程崩溃后槽在 slotTTL 后自动释放。
func NewSemaphore(client *redis.Client, key string, max int, slotTTL time.Duration) *Semaphore {
	return &Semaphore{
		client:  client,
		key:     "git-sync:semaphore:" + key,
		max:     max,
		slotTTL: slotTTL,
	}
}

func (s *Semaphore) Acquire(ctx context.Context, identifier string) (bool, error) {
	now := time.Now().UnixMilli()
	expire := time.Now().Add(s.slotTTL).UnixMilli()
	result, err := semaphoreAcquireScript.Run(ctx, s.client, []string{s.key}, identifier, s.max, now, expire).Int()
	if err != nil {
		return false, errors.Wrap(err, "semaphore acquire failed")
	}
	return result == 1, nil
}

// renewScript 仅当成员已存在于集合中才续期,防止过期后重新加入导致超限。
var renewScript = redis.NewScript(`
	if redis.call("ZSCORE", KEYS[1], ARGV[1]) then
		return redis.call("ZADD", KEYS[1], ARGV[2], ARGV[1])
	end
	return 0
`)

// Renew 续期持有的槽(把 score 推到 now+slotTTL),仅当成员已存在才续期。
func (s *Semaphore) Renew(ctx context.Context, identifier string) error {
	expire := time.Now().Add(s.slotTTL).UnixMilli()
	_, err := renewScript.Run(ctx, s.client, []string{s.key}, identifier, expire).Result()
	return err
}

func (s *Semaphore) Release(ctx context.Context, identifier string) error {
	_, err := s.client.ZRem(ctx, s.key, identifier).Result()
	return err
}

func (s *Semaphore) Cleanup(ctx context.Context, olderThan time.Duration) error {
	// 注意:槽位 score 是毫秒时间戳(Acquire 用 UnixMilli),阈值必须同为毫秒,
	// 否则 ZRemRangeByScore 永远删不到任何条目(此前用 Unix() 秒导致差 1000 倍)
	cutoff := float64(time.Now().Add(-olderThan).UnixMilli())
	_, err := s.client.ZRemRangeByScore(ctx, s.key, "-inf", fmt.Sprintf("%f", cutoff)).Result()
	return err
}

type localLockEntry struct {
	expiresAt time.Time
}

type LocalLock struct {
	mu      sync.Mutex
	entries map[string]*localLockEntry
}

func NewLocalLock() *LocalLock {
	return &LocalLock{
		entries: make(map[string]*localLockEntry),
	}
}

func (l *LocalLock) TryLock(ctx context.Context, key string) (bool, error) {
	return l.TryLockWithTTL(ctx, key, defaultLockTTL)
}

func (l *LocalLock) TryLockWithTTL(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	entry, exists := l.entries[key]
	if !exists || now.After(entry.expiresAt) {
		l.entries[key] = &localLockEntry{expiresAt: now.Add(ttl)}
		return true, nil
	}
	return false, nil
}

func (l *LocalLock) Lock(ctx context.Context, key string) error {
	backoff := 100 * time.Millisecond
	const maxBackoff = 5 * time.Second
	for {
		ok, _ := l.TryLock(ctx, key)
		if ok {
			return nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (l *LocalLock) Unlock(ctx context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, exists := l.entries[key]; !exists {
		// 双重 Unlock 防护:不存在时返回错误而非静默成功,
		// 避免调用方误以为释放成功而后续逻辑依赖错误的锁状态
		return fmt.Errorf("local lock %q: not held (double unlock?)", key)
	}
	delete(l.entries, key)
	return nil
}
