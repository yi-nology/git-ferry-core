package health

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestSnapshotsFromRuns_CarriesAllSignals 钉住"运行记录 → 评分快照"的映射。
//
// 回归背景:这段映射以前在两处各写一份(core 的健康服务、壳层的 AI agent 工具),
// 壳层那份**漏了 StepFails / RetryTotal / ErrorType** ⇒ 同一个任务,AI 看到的
// 健康分比 API 给运维看的乐观。
//
// 现在收敛成 health 包的唯一实现 ⇒ 钉住它必须带全所有信号。
func TestSnapshotsFromRuns_CarriesAllSignals(t *testing.T) {
	end := time.Now().Add(-time.Hour)
	runs := []*model.SyncRun{
		{
			ID: 1, TaskKey: "k", Status: "failed", ErrorMessage: "boom",
			EndTime: &end, RetryTotal: 3, ErrorType: "divergent",
			Steps: []model.SyncRunStep{
				{RunID: 1, StepName: "clone", Status: "success"},
				{RunID: 1, StepName: "push", Status: "failed"},
				{RunID: 1, StepName: "bundle", Status: "failed"},
			},
		},
		{ID: 2, TaskKey: "k", Status: "success", EndTime: &end},
	}

	snaps := SnapshotsFromRuns(runs)
	require.Len(t, snaps, 2)

	assert.Equal(t, "failed", snaps[0].Status)
	assert.Equal(t, "boom", snaps[0].ErrorMessage)
	assert.Equal(t, end, snaps[0].EndAt)
	assert.Equal(t, 2, snaps[0].StepFails, "失败步骤数必须统计出来(此前恒为 0)")
	assert.Equal(t, 3, snaps[0].RetryTotal, "重试总数必须带上")
	assert.Equal(t, "divergent", snaps[0].ErrorType, "错误分类必须带上(失败主因靠它)")

	assert.Equal(t, 0, snaps[1].StepFails)
	assert.Equal(t, 0, snaps[1].RetryTotal)
	assert.Equal(t, "", snaps[1].ErrorType)
}

// TestSnapshotsFromRuns_NilSafe 空输入/空指针不许 panic(agent 工具拿到的历史可能有空洞)。
func TestSnapshotsFromRuns_NilSafe(t *testing.T) {
	assert.Nil(t, SnapshotsFromRuns(nil))
	assert.Nil(t, SnapshotsFromRuns([]*model.SyncRun{}))
	snaps := SnapshotsFromRuns([]*model.SyncRun{nil, {ID: 3, Status: "success"}})
	require.Len(t, snaps, 1)
	assert.Equal(t, "success", snaps[0].Status)
	assert.True(t, snaps[0].EndAt.IsZero())
}

// TestSnapshotsFromRuns_FeedDimensionSignals 端到端:快照里的信号必须真的进到评分里。
func TestSnapshotsFromRuns_FeedDimensionSignals(t *testing.T) {
	end := time.Now().Add(-time.Hour)
	runs := []*model.SyncRun{{
		ID: 1, Status: "success", EndTime: &end, ErrorType: "network",
		Steps: []model.SyncRunStep{{RunID: 1, StepName: "push", Status: "failed"}},
	}}
	facts := CollectRunFacts(SnapshotsFromRuns(runs), time.Now())

	assert.Equal(t, "1", facts["step_fail_count"], "步骤级失败数要进事实")
	assert.Equal(t, "network", facts["dominant_error"], "失败主因要进事实")

	res := EvaluateDimensions(nil, facts)
	var detail []string
	for _, d := range res.Dimensions {
		if d.Name == "reliability" {
			detail = d.Detail
		}
	}
	assert.Contains(t, detail, "步骤级失败 1 处（fetch/push 等）")
	assert.Contains(t, detail, "最近失败主因: network")
}
