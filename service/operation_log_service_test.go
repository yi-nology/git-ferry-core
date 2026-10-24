package service

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupOperationLogTestService(t *testing.T) *OperationLogService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	if err := db.AutoMigrate(&model.OperationLog{}); err != nil {
		t.Fatalf("failed to migrate test db: %v", err)
	}
	return NewOperationLogService(dao.NewOperationLogDAO(db))
}

func TestOperationLogService_RecordAndList(t *testing.T) {
	svc := setupOperationLogTestService(t)
	ctx := context.Background()

	// Record some operations
	if err := svc.Record(ctx, &model.OperationLog{Action: "create", ResourceType: "repo", Resource: "创建仓库 repo-x", Actor: "admin"}); err != nil {
		t.Fatalf("record failed: %v", err)
	}

	if err := svc.Record(ctx, &model.OperationLog{Action: "delete", ResourceType: "task", Resource: "删除任务 t1", Actor: "ops"}); err != nil {
		t.Fatalf("record failed: %v", err)
	}

	// List all
	filter := &dao.OperationLogFilter{}
	got, total, err := svc.List(ctx, 0, 50, filter)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("expected 2, got total=%d len=%d", total, len(got))
	}

	// Check ordering (newest first)
	if got[0].Resource != "删除任务 t1" {
		t.Fatalf("expected newest first, got %+v", got[0])
	}

	// Check default status
	if got[0].Status != model.StatusSuccess {
		t.Fatalf("expected default status success, got %q", got[0].Status)
	}
}

func TestOperationLogService_Stats(t *testing.T) {
	svc := setupOperationLogTestService(t)
	ctx := context.Background()

	// Add some test data
	_ = svc.Record(ctx, &model.OperationLog{Action: "create", ResourceType: "repo", Resource: "test1", Actor: "admin"})
	_ = svc.Record(ctx, &model.OperationLog{Action: "update", ResourceType: "task", Resource: "test2", Actor: "user"})

	today, week, total, err := svc.Stats(ctx)
	if err != nil {
		t.Fatalf("stats failed: %v", err)
	}

	if total < 2 {
		t.Errorf("expected total >= 2, got %d", total)
	}
	t.Logf("Stats: today=%d, week=%d, total=%d", today, week, total)
}

// TestOperationLogChainSurvivesPrecisionChange 回归:把审计日志的时间精度
// 从纳秒改成毫秒之后,**旧记录必须仍然验得过**。
//
// 改动时间精度后,新记录落库的是毫秒值,旧记录库里仍是纳秒值。
// 链的哈希是「每条记录用它自己存的值算的」—— 两批记录各自算各自的,
// 混在一起也应当接得上。
//
// 这条断言守的是「改了存储精度 ≠ 毁掉历史审计证据」。
func TestOperationLogChainSurvivesPrecisionChange(t *testing.T) {
	svc := setupOperationLogTestService(t)
	ctx := context.Background()

	// 第一条:模拟**旧记录**(纳秒精度,直接写库,绕开新的截断逻辑)。
	old := &model.OperationLog{
		Action: "sync", ResourceType: "repo", ResourceKey: "k",
		Resource: "r", Actor: "u", IP: "127.0.0.1", Status: model.StatusSuccess,
		Detail: "旧记录(纳秒)",
		// 显式给纳秒时间戳 —— 这正是升级前写进去的形态。
		CreatedAt: time.Now(),
	}
	old.EntryHash = model.ComputeEntryHash("", old)
	require.NoError(t, svc.opLogDAO.Create(old))

	// 后续记录走正常路径(毫秒精度)。
	prev := old.EntryHash
	for i := 0; i < 2; i++ {
		entry := &model.OperationLog{
			Action: "sync", ResourceType: "repo", ResourceKey: "k",
			Resource: "r", Actor: "u", IP: "127.0.0.1", Status: model.StatusSuccess,
			Detail: "新记录(毫秒)",
		}
		require.NoError(t, svc.Record(ctx, entry))
		prev = entry.EntryHash
	}
	_ = prev

	res, err := svc.VerifyAuditChain()
	require.NoError(t, err)
	assert.True(t, res.OK, "精度变更后旧记录必须仍然验得过:"+res.Message)
	assert.Equal(t, 3, res.Checked)
}
