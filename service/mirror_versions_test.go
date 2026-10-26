package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestBuildVersionCells_StateComesFromNewestRun 矩阵状态必须取**最近一次**执行。
//
// 回归背景(P0,报告与事实相反):runs 按 id 倒序(首个最新),而 publish 分支
// **无条件覆盖**状态 ⇒ 最后一次迭代(最旧那条)赢了 ⇒
// ① 先失败、后修好重发成功 ⇒ 矩阵仍报 failed;
// ② 最近一次被判定 divergent ⇒ 矩阵仍报 success。
// verify 分支是"首个即最新"的 ⇒ 两个分支口径还不一致。
//
// 这条用例是**反向验证**:把 publish 分支改回无条件覆盖,本用例会变红。
func TestBuildVersionCells_StateComesFromNewestRun(t *testing.T) {
	now := time.Now()
	newer := &model.MirrorRun{
		ID: 2, ChannelID: 1, TargetID: 7, Kind: model.MirrorKindPublish,
		Status:      model.MirrorRunSuccess,
		TagStatuses: `{"v1.0.0":{"status":"success","commit":"C2","tree":"T2"}}`,
		StartedAt:   &now,
	}
	older := &model.MirrorRun{
		ID: 1, ChannelID: 1, TargetID: 7, Kind: model.MirrorKindPublish,
		Status:      model.MirrorRunFailed,
		TagStatuses: `{"v1.0.0":{"status":"failed","commit":"C1","tree":"T1"}}`,
		StartedAt:   &now,
	}
	// DAO 的顺序是 id DESC ⇒ 第一个即最新
	cells := buildVersionCells([]*model.MirrorRun{newer, older})
	require.Contains(t, cells, "v1.0.0")
	cell := cells["v1.0.0"][7]
	require.NotNil(t, cell)
	assert.Equal(t, model.MirrorRunSuccess, cell.state,
		"矩阵状态必须取最近一次执行(旧实现被最旧那条覆盖)")
	assert.Equal(t, "C2", cell.commit)
}

// TestBuildVersionCells_VerifyAlsoNewestWins verify 列同样取最近一次。
func TestBuildVersionCells_VerifyAlsoNewestWins(t *testing.T) {
	now := time.Now()
	newer := &model.MirrorRun{
		ID: 2, ChannelID: 1, TargetID: 7, Kind: model.MirrorKindVerify,
		Status:      model.MirrorRunFailed,
		TagStatuses: `{"v1.0.0":{"status":"failed"}}`,
		StartedAt:   &now,
	}
	older := &model.MirrorRun{
		ID: 1, ChannelID: 1, TargetID: 7, Kind: model.MirrorKindVerify,
		Status:      model.MirrorRunSuccess,
		TagStatuses: `{"v1.0.0":{"status":"success"}}`,
		StartedAt:   &now,
	}
	cells := buildVersionCells([]*model.MirrorRun{newer, older})
	cell := cells["v1.0.0"][7]
	require.NotNil(t, cell)
	assert.Equal(t, "failed", cell.verify, "verify 列也必须取最近一次")
}
