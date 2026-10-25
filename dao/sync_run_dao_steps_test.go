package dao

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestSyncRunDAO_FindByTaskKey_LoadsSteps 列表查询必须带出步骤明细。
//
// 回归背景:FindByTaskKey 以前**不**预加载 Steps(注释写"避免 N+1",但 GORM 的
// Preload 本来就是一条聚合查询,不存在 N+1)⇒ 健康评分拿到的运行记录里步骤恒为空
// ⇒ 「步骤级失败」信号结构性恒为 0,运维/AI 都看不到失败发生在哪一步。
func TestSyncRunDAO_FindByTaskKey_LoadsSteps(t *testing.T) {
	db := setupSyncRunTestDB(t)
	d := NewSyncRunDAO(db)
	steps := NewSyncRunStepDAO(db)

	run := &model.SyncRun{
		TaskKey: "task-steps", TriggerSource: "manual", Status: "failed",
		StartTime: time.Now(), ErrorType: "divergent", RetryTotal: 2,
	}
	require.NoError(t, d.Create(run))

	require.NoError(t, steps.Create(&model.SyncRunStep{
		RunID: run.ID, StepName: "clone", Status: "success",
	}))
	require.NoError(t, steps.Create(&model.SyncRunStep{
		RunID: run.ID, StepName: "push", Status: "failed", ErrorMsg: "boom",
	}))

	runs, _, err := d.FindByTaskKey("task-steps", DefaultPagination(0, 10))
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Len(t, runs[0].Steps, 2, "列表查询必须带出步骤明细(否则失败步骤数恒为 0)")

	var failed int
	for i := range runs[0].Steps {
		if runs[0].Steps[i].Status == "failed" {
			failed++
		}
	}
	assert.Equal(t, 1, failed)
}
