package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestRecoverStuckEvents_MarksFailed 卡在 processing 的事件必须被回收成 failed。
//
// 回归背景(P1):processing 只在"正在处理"期间存在 ⇒ 进程被 SIGKILL / 崩溃 /
// goroutine panic 时,事件永远停在这个状态 ⇒
// ① 既没有失败记录(看不出"这次没同步");
// ② 也没有 pending 可补 ⇒ 这次推送的提交**静默丢失**。
// ⇒ 回收成 failed ⇒ 可见、可人工重试(壳层有 POST /api/v1/webhook/event/retry)。
//
// 这条用例是**反向验证**:去掉 RecoverStuckEvents 的回收,本用例变红。
func TestRecoverStuckEvents_MarksFailed(t *testing.T) {
	db, svc := setupWebhookServiceTestDB(t)
	ctx := context.Background()

	// 卡住的事件:processing + 创建于很久以前
	stuck := &model.WebhookEvent{
		EventID: "evt-stuck", RepoKey: "test-repo", EventType: "push",
		Branch: "main", Status: model.StatusProcessing,
	}
	require.NoError(t, db.Create(stuck).Error)
	past := time.Now().Add(-3 * time.Hour)
	require.NoError(t, db.Model(&model.WebhookEvent{}).Where("id = ?", stuck.ID).
		Update("created_at", past).Error)

	// 刚收到的正常事件:不该被动
	fresh := &model.WebhookEvent{
		EventID: "evt-fresh", RepoKey: "test-repo", EventType: "push",
		Branch: "main", Status: model.StatusReceived,
	}
	require.NoError(t, db.Create(fresh).Error)

	n, err := svc.RecoverStuckEvents(ctx, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "只应回收超时的那一条")

	got, err := svc.FindEventByID(stuck.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.StatusFailed, got.Status, "卡住的事件必须落 failed 终态")
	assert.Contains(t, got.ErrorMessage, "中断", "失败原因要写清楚(否则无法判断发生了什么)")

	got2, err := svc.FindEventByID(fresh.ID)
	require.NoError(t, err)
	require.NotNil(t, got2)
	assert.Equal(t, model.StatusReceived, got2.Status, "未超时的正常事件不受影响")
}
