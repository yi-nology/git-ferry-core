package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestRecoverStuckMirrorRuns_FreesChannel 僵尸 run 必须被回收,否则通道永久占用。
//
// 回归背景(P0,"通道永久占用"):ExecuteMirrorRun 受理时会查
// HasRunningByChannel(DB 里有没有在跑的 run)—— 进程被 SIGKILL / 崩溃时
// 留下的 run 永远停在 pending|running ⇒ 该通道**再也触发不了**任何
// 发布/校验(一直报"该通道有执行中的任务"),且历史里看不出"这次没跑完"。
//
// 这条用例是**反向验证**:去掉 RecoverStuckMirrorRuns,本用例会变红。
func TestRecoverStuckMirrorRuns_FreesChannel(t *testing.T) {
	m, _, db := setupMirrorServiceTest(t)
	ctx := context.Background()

	// 造一条僵尸 run:状态 running,started_at 是 3 小时前。
	ch := &model.MirrorChannel{Name: "zombie", Mode: model.MirrorModePublish}
	require.NoError(t, db.Create(ch).Error)

	old := time.Now().Add(-3 * time.Hour)
	zombie := &model.MirrorRun{
		ChannelID: ch.ID, TargetID: 1, Kind: model.MirrorKindPublish,
		Tags: "v1.0.0", Status: model.MirrorRunRunning,
		StartedAt: &old,
	}
	require.NoError(t, db.Create(zombie).Error)

	// 僵尸 run 确实占着通道
	busy, err := m.runs.HasRunningByChannel(ch.ID)
	require.NoError(t, err)
	require.True(t, busy, "僵尸 run 应被判定为进行中")

	n, err := m.RecoverStuckMirrorRuns(ctx, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "必须回收 1 条僵尸 run")

	// 通道不再被占用
	busy, err = m.runs.HasRunningByChannel(ch.ID)
	require.NoError(t, err)
	assert.False(t, busy, "回收后通道必须能被再次触发")

	// 记录要能看出"这次没跑完"
	got, err := m.runs.FindByID(zombie.ID)
	require.NoError(t, err)
	assert.Equal(t, model.MirrorRunFailed, got.Status)
	assert.NotEmpty(t, got.Error, "失败原因要写清楚")
	assert.NotNil(t, got.FinishedAt, "回收后要有结束时间")
}

// TestRecoverStuckMirrorRuns_KeepsFreshRuns 正在跑的任务不能被误伤。
func TestRecoverStuckMirrorRuns_KeepsFreshRuns(t *testing.T) {
	m, _, db := setupMirrorServiceTest(t)
	ctx := context.Background()

	ch := &model.MirrorChannel{Name: "live", Mode: model.MirrorModePublish}
	require.NoError(t, db.Create(ch).Error)

	now := time.Now()
	live := &model.MirrorRun{
		ChannelID: ch.ID, TargetID: 1, Kind: model.MirrorKindPublish,
		Tags: "v1.0.0", Status: model.MirrorRunRunning,
		StartedAt: &now,
	}
	require.NoError(t, db.Create(live).Error)

	n, err := m.RecoverStuckMirrorRuns(ctx, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "1 小时内启动的执行不能被回收")

	got, err := m.runs.FindByID(live.ID)
	require.NoError(t, err)
	assert.Equal(t, model.MirrorRunRunning, got.Status, "正在跑的执行必须保持 running")
}

// TestRecoverStuckMirrorRuns_RecoversStuckPending 从未开跑就卡住的 pending
// 记录(进程死在受理与执行之间)同样要回收。
func TestRecoverStuckMirrorRuns_RecoversStuckPending(t *testing.T) {
	m, _, db := setupMirrorServiceTest(t)
	ctx := context.Background()

	ch := &model.MirrorChannel{Name: "stuck-pending", Mode: model.MirrorModePublish}
	require.NoError(t, db.Create(ch).Error)

	// started_at 为 nil、created_at 是 3 小时前 ⇒ pending 僵尸
	stuck := &model.MirrorRun{
		ChannelID: ch.ID, TargetID: 1, Kind: model.MirrorKindPublish,
		Tags: "v1.0.0", Status: model.MirrorRunPending,
	}
	require.NoError(t, db.Create(stuck).Error)
	old := time.Now().Add(-3 * time.Hour)
	require.NoError(t, db.Model(&model.MirrorRun{}).Where("id = ?", stuck.ID).
		Update("created_at", old).Error)

	n, err := m.RecoverStuckMirrorRuns(ctx, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "pending 僵尸也必须回收(否则通道同样永久占用)")

	busy, err := m.runs.HasRunningByChannel(ch.ID)
	require.NoError(t, err)
	assert.False(t, busy, "回收后通道必须能被再次触发")
}
