package service

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestNewService_WiresAllStores 全链路接线回归:
// NewService 必须把 DAO 装配到 Service 的每个存储字段。
//
// 回归背景(真实事故,随 v0.36.0 发布):svc.outbox 从未在构造函数赋值 ⇒
// 进程内 outbox 派发器空转(有 nil 守卫不 panic),事件只落库不重投、
// outbox 表无限增长。此前全仓无 NewService 测试,逐字段断言就是堵住
// 「结构体有字段、构造没接线」这类洞的闸。
func TestNewService_WiresAllStores(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	cfg := &model.Config{
		Database: model.DatabaseConfig{
			Driver: "sqlite",
			DSN:    filepath.Join(t.TempDir(), "svc.db"),
		},
		Git: model.GitConfig{TempDir: t.TempDir()},
	}
	svc, err := NewService(cfg)
	require.NoError(t, err)
	defer svc.Stop()

	assert.NotNil(t, svc.outbox, "outbox DAO 必须接线(构造函数赋值)")
	assert.NotNil(t, svc.pending, "pending DAO 必须接线")
	assert.NotNil(t, svc.jobs, "execution jobs DAO 必须接线")
	assert.NotEmpty(t, svc.instanceID, "lease 属主标识必须生成")
	assert.NotNil(t, svc.reconciles, "reconcile DAO 必须接线")
	assert.NotNil(t, svc.recovery, "recovery DAO 必须接线")
	assert.NotNil(t, svc.executor.Recovery, "executor 必须注入恢复点记录器")
	assert.NotNil(t, svc.templates, "templates store 必须接线")
	assert.NotNil(t, svc.approvals, "force-push approvals 必须接线")
	assert.NotNil(t, svc.retryTracker, "重试判定器必须接线")
	assert.NotNil(t, svc.guard, "执行权互斥必须接线")
	assert.NotNil(t, svc.executor, "executor 必须接线")
	assert.NotNil(t, svc.mirror, "mirror service 必须接线")
	assert.NotNil(t, svc.history, "history service 必须接线")

	// 重试链统计函数接线(P0-1 的关键修复点,同样只在 NewService 路径生效)。
	tr := svc.AutoRetry()
	require.NotNil(t, tr)
	tr.mu.Lock()
	hasChain := tr.chain != nil
	tr.mu.Unlock()
	assert.True(t, hasChain, "RetryTracker.chain 必须接线(重试预算按 DB 派生)")
}
