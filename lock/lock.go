// Package lock 提供同步任务的并发控制原语:
//   - 跨实例互斥锁:oekit redisx/distlock(SetNX + Lua 原子解锁 + 自动续期),
//     本包只负责固定 key 前缀与客户端构造;
//   - Semaphore:基于 redis ZSET 的分布式信号量,限制全局并发上限,
//     实现复用 oekit redisx/distlock.Semaphore。
package lock

import (
	"time"

	"github.com/oekit/redisx/distlock"
	"github.com/redis/go-redis/v9"
)

const (
	// LockKeyPrefix 是跨实例互斥锁的 key 前缀。distlock 以 "<prefix>:<key>" 落 key,
	// 与迁移前自写实现的 "git-sync:lock:<key>" 布局一致:滚动升级期间新老实例
	// 看到的是同一把锁,不会出现两边各持一把而同时执行的窗口。
	LockKeyPrefix = "git-sync:lock"

	// SemaphoreKeyPrefix 是全局并发信号量的 key 前缀。与迁移前自写实现的
	// "git-sync:semaphore:<key>" 布局保持一致:滚动升级期间新老实例看到的是
	// 同一把信号量,不会两把各算各的配额而使实际上限翻倍。
	SemaphoreKeyPrefix = "git-sync:semaphore:"
)

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

// ⚠️ **WithMaxAutoRenew 的「最大生命周期安全网」现已可用**:
// 上游在 redisx v0.10.3 修了「计时器 Stop 绑在 startAutoRenew 函数级 defer ⇒
// 派发完 goroutine 就当场 Stop ⇒ 安全网分支是死代码」的缺陷
// (上游 issue: oekit/redisx#39)。本仓已钉 redisx v0.10.3,该安全网真实生效。
//
// 本仓的两个用法仍然不受它影响:
//   - 通道锁:要的就是「一直续期到 Unlock」(TTL 30 分钟只是兜底);
//   - 任务锁:显式传 WithMaxAutoRenew(-1)(不限制)。
//
// 降级到 v0.10.2 及更早时,这个安全网形同虚设 —— 别的地方要用它先确认版本。
//
// NewLocker 构造跨实例互斥锁,key 前缀固定为 LockKeyPrefix;续期等策略由调用方按场景追加。
//
// **默认开启自动续期**:锁的持有时长由业务决定(一次克隆+推送可能几十分钟),
// 而 TTL 是固定的 —— 不续期的话,操作进行到一半锁就过期了,
// 另一个实例能进同一通道,两边的哈希链会分叉。
// 调用方仍可追加 WithAutoRenew 之外的选项覆盖。
func NewLocker(client redis.UniversalClient, opts ...distlock.RedisLockerOption) *distlock.RedisLocker {
	all := append([]distlock.RedisLockerOption{
		distlock.WithPrefix(LockKeyPrefix),
		distlock.WithAutoRenew(),
	}, opts...)
	return distlock.NewRedisLocker(client, all...)
}

// Semaphore 是全局并发上限的分布式信号量,实现复用 oekit redisx/distlock.Semaphore
// (ZSET 槽位模型 + 原子 Lua 脚本)。
//
// 本包只做两件事:固定 key 前缀、对外暴露 key 供测试与运维观测;
// 槽位的获取/续期/释放/清理语义全部跟随上游,不再自维护 Lua 脚本。
//
// 与迁移前自写实现的差异(均为上游更严的方向):
//   - Acquire 对同一 identifier 幂等刷新(重复 Acquire 不占第二个槽);
//   - Renew 只续期**未过期**的槽,已过期槽不得借 Renew 复活——
//     它的配额可能已被其他 Acquire 接管,复活即超限;
//   - Release 幂等(未持有不报错),Cleanup 返回被清理的槽数。
type Semaphore struct {
	*distlock.Semaphore
	key string
}

// NewSemaphore 构造分布式信号量,Redis key 为 "<SemaphoreKeyPrefix><key>"。
// slotTTL 为单个槽的存活时长:持有者需周期性 Renew,进程崩溃后槽在 slotTTL 后自动释放。
func NewSemaphore(client redis.UniversalClient, key string, max int, slotTTL time.Duration) *Semaphore {
	fullKey := SemaphoreKeyPrefix + key
	return &Semaphore{
		Semaphore: distlock.NewSemaphore(client, fullKey, max, slotTTL),
		key:       fullKey,
	}
}
