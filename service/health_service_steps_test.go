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
