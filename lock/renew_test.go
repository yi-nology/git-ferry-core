package lock

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oekit/redisx/distlock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDistlockLockDoesNotLeakRenewGoroutine 钉住上游 issue #33 的修复。
//
// `Lock()` 曾经**重复启动**自动续期 goroutine(TryLock 内部已启一次),
// 而 `l.locks` 只保留后一个 state → Unlock 只能停掉其中一个,
// 另一个成孤儿:Unlock 后还会打出误导性的「锁丢了」WARN。
//
// 上游在 v0.10.2 修复。本仓锁的是 v0.10.0 —— 这条测试就是升级的守门人:
// 一旦回退到旧版本,这里会立刻变红。
func TestDistlockLockDoesNotLeakRenewGoroutine(t *testing.T) {
	client := getTestRedisClient(t)
	defer closeClient(t, client)
	ctx := context.Background()

	var logs []string
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&renewLogWriter{&logs}, nil)))
	defer slog.SetDefault(old)

	locker := distlock.NewRedisLocker(client,
		distlock.WithPrefix("git-sync:lock:issue33:"),
		distlock.WithAutoRenew())

	key := "issue33-" + time.Now().Format("150405.000000")
	owner, err := locker.Lock(ctx, key, 2*time.Second, time.Second)
	require.NoError(t, err)

	before := runtime.NumGoroutine()
	require.NoError(t, locker.Unlock(ctx, key, owner))
	// 等一个续期间隔(ttl/2):孤儿 goroutine 会在这一轮 tick 里发现锁已不属于自己。
	time.Sleep(1500 * time.Millisecond)
	after := runtime.NumGoroutine()

	var warns int
	for _, l := range logs {
		if strings.Contains(l, "auto renew failed") {
			warns++
		}
	}
	assert.Equal(t, 0, warns,
		"Unlock 后不该再打出「锁丢了」的 WARN(孤儿续期 goroutine 还在)——上游 v0.10.2 已修")
	assert.LessOrEqual(t, after, before-1,
		"Unlock 后续期 goroutine 应已退出(前=%d 后=%d)", before, after)
}

// renewLogWriter 收集 slog 输出。
type renewLogWriter struct{ out *[]string }

func (w *renewLogWriter) Write(p []byte) (int, error) {
	*w.out = append(*w.out, string(p))
	return len(p), nil
}
