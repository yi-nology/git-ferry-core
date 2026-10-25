package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestApplyRules_TriggerFailureIsReported 触发失败必须能被调用方知道。
//
// 回归背景(P1):ApplyRules 以前没有返回值,触发失败只进日志 ⇒
// 事件照样被标成 processed ⇒ 事件列表里与成功事件**无法区分**,
// 没有任何可观测信号说"这次没同步",也没有可重试的对象。
//
// 这条用例是**反向验证**:把 ApplyRules 的错误返回去掉,本用例变红。
func TestApplyRules_TriggerFailureIsReported(t *testing.T) {
	db, svc := setupWebhookServiceTestDB(t)
	ctx := context.Background()

	rule := &model.WebhookRule{
		Name: "触发失败", RepoKey: "test-repo", EventType: "push",
		BranchPattern: "main", Action: "sync", Enabled: true,
	}
	require.NoError(t, db.Create(rule).Error)
	require.NoError(t, db.Create(&model.WebhookRuleTask{RuleID: rule.ID, TaskKey: "task1"}).Error)

	event := &model.WebhookEvent{
		EventID: "evt-fail", RepoKey: "test-repo", EventType: "push",
		Branch: "main", Status: model.StatusReceived,
	}
	require.NoError(t, db.Create(event).Error)

	boom := errors.New("任务正在运行")
	runTaskFn := func(ctx context.Context, taskKey, trigger string, webhookEventID *uint) error {
		return boom
	}

	err := svc.ApplyRules(ctx, "test-repo", event, &sync.Map{}, runTaskFn, nil)
	require.Error(t, err, "触发失败必须返回错误(否则事件会被标成 processed)")
	assert.Contains(t, err.Error(), "task1", "错误信息应指出是哪个任务失败")
	assert.Contains(t, err.Error(), "任务正在运行", "错误信息应带上原始原因")
}

// TestMarkEventFailed 失败的事件要落 failed 终态并带上原因。
func TestMarkEventFailed(t *testing.T) {
	db, svc := setupWebhookServiceTestDB(t)

	event := &model.WebhookEvent{
		EventID: "evt-failed", RepoKey: "test-repo", EventType: "push",
		Branch: "main", Status: model.StatusProcessing,
	}
	require.NoError(t, db.Create(event).Error)

	require.NoError(t, svc.MarkEventFailed(event, "触发任务 task1 失败:任务正在运行"))

	stored, err := svc.FindEventByID(event.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, model.StatusFailed, stored.Status, "失败事件必须是 failed 终态")
	assert.Contains(t, stored.ErrorMessage, "任务正在运行", "失败原因必须落库(否则无法排查)")
}
