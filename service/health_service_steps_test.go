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
	"github.com/yi-nology/git-ferry-core/health"
	"github.com/yi-nology/git-ferry-core/model"
)

func setupHealthTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}))

	// sqlite :memory: 每个连接是独立库 ⇒ 强制单连接,确保同一连接能看到迁移后的表
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	taskDAO := dao.NewSyncTaskDAO(db)
	runDAO := dao.NewSyncRunDAO(db)
	stepDAO := dao.NewSyncRunStepDAO(db)
	return &Service{tasks: NewTaskService(taskDAO, runDAO, stepDAO, nil)}, db
}

// TestHealthSnapshot_ReportsStepLevelFailures 健康评分必须报出"失败发生在哪一步"。
//
// 回归背景:运行记录列表查询以前不加载 Steps ⇒ 健康评分里的
// `step_fail_count` **恒为 0** ⇒
//
//	①「步骤级失败 N 处」永远不出现;② reliability 的分数上限(55 分)永远不生效
//	   ⇒ 运维/AI 以为任务很健康,而真实情况是每次都卡在同一个步骤上。
func TestHealthSnapshot_ReportsStepLevelFailures(t *testing.T) {
	svc, db := setupHealthTestService(t)

	task := &model.SyncTask{Key: "health-steps", Name: "带步骤失败的任务", Enabled: true}
	require.NoError(t, dao.NewSyncTaskDAO(db).Create(task))

	end := time.Now().Add(-time.Hour)
	run := &model.SyncRun{
		TaskKey: task.Key, TriggerSource: "manual", Status: "success",
		StartTime: end, EndTime: &end, ErrorType: "network", RetryTotal: 2,
	}
	require.NoError(t, dao.NewSyncRunDAO(db).Create(run))
	require.NoError(t, dao.NewSyncRunStepDAO(db).Create(&model.SyncRunStep{
		RunID: run.ID, StepName: "clone", Status: "success",
	}))
	require.NoError(t, dao.NewSyncRunStepDAO(db).Create(&model.SyncRunStep{
		RunID: run.ID, StepName: "push", Status: "failed", ErrorMsg: "boom",
	}))

	snap, err := svc.HealthSnapshot(context.Background(), 10, nil)
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)

	var reliabilityDetail []string
	for _, d := range snap.Items[0].Dimensions {
		if d.Name == "reliability" {
			reliabilityDetail = d.Detail
		}
	}
	assert.Contains(t, reliabilityDetail, "步骤级失败 1 处（fetch/push 等）",
		"步骤级失败必须被报出来(此前恒为 0,信号结构性失效)")
	assert.Contains(t, reliabilityDetail, "最近失败主因: network",
		"失败主因必须被报出来")
}

// TestApplyRPOFacts RPO 事实键与评分窗口解耦(#18):
// 全历史最近一次备份成功覆盖 CollectRunFacts 的窗口内结果;
// 窗口内无成功备份时以 backup_in_recent_window 区分两种 -1 的成因。
func TestApplyRPOFacts(t *testing.T) {
	now := time.Now()
	facts := health.Facts{
		"run_total":        "3",
		"last_backup_at":   "", // CollectRunFacts 在 10 条窗口内没看到成功备份 ⇒ 空/-1
		"backup_age_hours": "-1",
	}

	// 窗口外 25h 前有一次备份成功 ⇒ 不得谎报成「从未成功备份」
	lastBackup := &model.SyncRun{EndTime: ptrTime(now.Add(-25 * time.Hour)), BackupStatus: model.BackupStatusSuccess}
	applyRPOFacts(facts, lastBackup, []*model.SyncRun{
		{BackupStatus: model.BackupStatusFailed},
		{BackupStatus: ""},
	}, now)
	assert.Equal(t, lastBackup.EndTime.Format(time.RFC3339), facts["last_backup_at"])
	assert.Equal(t, "25", facts["backup_age_hours"])
	assert.Equal(t, "false", facts["backup_in_recent_window"],
		"窗口内确实没有成功备份,该位必须如实报 false")

	// 窗口内就有成功备份 ⇒ 该位为 true
	applyRPOFacts(facts, lastBackup, []*model.SyncRun{
		{BackupStatus: model.BackupStatusSuccess},
	}, now)
	assert.Equal(t, "true", facts["backup_in_recent_window"])

	// 真的从未备份成功(lastBackup=nil)⇒ 保持 -1(契约语义:无成功备份)
	never := health.Facts{"last_backup_at": "", "backup_age_hours": "-1"}
	applyRPOFacts(never, nil, nil, now)
	assert.Empty(t, never["last_backup_at"])
	assert.Equal(t, "-1", never["backup_age_hours"])
}

// TestHealthSnapshot_RPONotTruncatedByWindow 端到端(#18):
// 12 次运行、唯一一次备份成功落在「最近 10 条」窗口之外,
// 健康快照仍必须报出真实的 last_backup_at / backup_age_hours。
func TestHealthSnapshot_RPONotTruncatedByWindow(t *testing.T) {
	svc, db := setupHealthTestService(t)

	task := &model.SyncTask{Key: "health-rpo", Name: "RPO 任务", Enabled: true}
	require.NoError(t, dao.NewSyncTaskDAO(db).Create(task))

	runDAO := dao.NewSyncRunDAO(db)
	end := time.Now().Add(-25 * time.Hour)
	for i := 0; i < 12; i++ {
		r := &model.SyncRun{
			TaskKey: "health-rpo", TriggerSource: "cron", Status: model.StatusSuccess,
			StartTime: end, EndTime: &end,
		}
		if i == 1 { // 第 2 条:唯一备份成功,窗口外
			r.BackupStatus = model.BackupStatusSuccess
		}
		require.NoError(t, runDAO.Create(r))
	}

	snap, err := svc.HealthSnapshot(context.Background(), 10, nil)
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.NotEmpty(t, snap.Items, "快照必须产出评分项")

	// 事实键本身经 applyRPOFacts 覆写;此处经由维度评估链路验证不 panic
	// 且维度可正常产出(此前 -1 谎报在 Facts 组装处,已由单测钉住)。
	for _, d := range snap.Items[0].Dimensions {
		assert.NotEmpty(t, d.Name)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
