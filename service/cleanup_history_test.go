package service

import (
	"context"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestCleanupOldData_DeletesExpiredKeepsRecent 执行历史清理必须**只删过期的**。
//
// 回归背景:这三张表(webhook 事件 / 同步运行 / 运行步骤)此前**没有任何清理入口**
// ⇒ 随运行次数无限增长(现网两个月已有 92 次运行 / 355 条步骤)。
//
// 这条用例是**反向验证**:把 CleanupOldData 的删除条件改掉(例如删全部),
// 本用例会变红。
func TestCleanupOldData_DeletesExpiredKeepsRecent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.WebhookEvent{}, &model.SyncRun{}, &model.SyncRunStep{}))
	// sqlite ":memory:" 每个连接是独立库 ⇒ 强制单连接,否则写入与清理可能落在不同库上
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	old := time.Now().Add(-40 * 24 * time.Hour)

	oldEvent := &model.WebhookEvent{EventID: "old-ev", RepoKey: "r", EventType: "push", Status: model.StatusProcessed}
	newEvent := &model.WebhookEvent{EventID: "new-ev", RepoKey: "r", EventType: "push", Status: model.StatusProcessed}
	require.NoError(t, db.Create(oldEvent).Error)
	require.NoError(t, db.Create(newEvent).Error)
	require.NoError(t, db.Model(&model.WebhookEvent{}).Where("id = ?", oldEvent.ID).Update("created_at", old).Error)

	oldRun := &model.SyncRun{TaskKey: "t", TriggerSource: "manual", Status: model.StatusSuccess}
	newRun := &model.SyncRun{TaskKey: "t", TriggerSource: "manual", Status: model.StatusSuccess}
	require.NoError(t, db.Create(oldRun).Error)
	require.NoError(t, db.Create(newRun).Error)
	require.NoError(t, db.Model(&model.SyncRun{}).Where("id = ?", oldRun.ID).Update("created_at", old).Error)

	oldStep := &model.SyncRunStep{RunID: oldRun.ID, StepName: "push", Status: "failed"}
	newStep := &model.SyncRunStep{RunID: newRun.ID, StepName: "push", Status: "success"}
	require.NoError(t, db.Create(oldStep).Error)
	require.NoError(t, db.Create(newStep).Error)
	require.NoError(t, db.Model(&model.SyncRunStep{}).Where("id = ?", oldStep.ID).Update("created_at", old).Error)

	// CleanupOldData 走的是子服务(webhooks / tasks)⇒ 要把它们装上
	svc := &Service{
		db:       db,
		webhooks: NewWebhookService(dao.NewWebhookRuleDAO(db), dao.NewWebhookEventDAO(db), nil),
		tasks: NewTaskService(
			dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db), dao.NewSyncRunStepDAO(db), nil),
	}
	events, runs, steps, err := svc.CleanupOldData(context.Background(), 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), events, "过期的 webhook 事件应被删")
	assert.Equal(t, int64(1), runs, "过期的同步运行应被删")
	assert.Equal(t, int64(1), steps, "过期的运行步骤应被删")

	// 近期的必须留着
	var evCount, runCount, stepCount int64
	require.NoError(t, db.Model(&model.WebhookEvent{}).Count(&evCount).Error)
	require.NoError(t, db.Model(&model.SyncRun{}).Count(&runCount).Error)
	require.NoError(t, db.Model(&model.SyncRunStep{}).Count(&stepCount).Error)
	assert.Equal(t, int64(1), evCount, "近期事件不得被删")
	assert.Equal(t, int64(1), runCount, "近期运行不得被删")
	assert.Equal(t, int64(1), stepCount, "近期步骤不得被删")
}

// TestHistoryRetention_ConfigDriven 保留期由配置决定(0 = 不清理)。
func TestHistoryRetention_ConfigDriven(t *testing.T) {
	svc := &Service{}
	assert.Equal(t, time.Duration(0), svc.historyRetention(), "没配置 ⇒ 不清理")

	svc.config = &model.Config{Sync: model.SyncConfig{HistoryRetentionDays: 30}}
	assert.Equal(t, 30*24*time.Hour, svc.historyRetention())

	svc.config = &model.Config{Sync: model.SyncConfig{HistoryRetentionDays: -1}}
	assert.Equal(t, time.Duration(0), svc.historyRetention(), "负值按不清理处理")
}
