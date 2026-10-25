package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// mysqlRoundTime 模拟 MySQL 的 datetime(3) 存储:
// 只保留毫秒,且**四舍五入**(MySQL 的行为,不是截断)。
func mysqlRoundTime(t time.Time) time.Time {
	return t.UTC().Round(time.Millisecond)
}

// TestHistoryRunTimestampSurvivesMySQL 回归:执行记录的时间戳必须以
// 「MySQL 能原样存回」的精度落库。
//
// 背景:GORM 的 MySQL 驱动默认 datetime(3)(毫秒),纳秒时间写进去会被
// **四舍五入**;而哈希是写库前算的、校验时从库里读回来重算 ——
// 精度对不上,整条链的每一条记录都会被报「被篡改」。
// SQLite 存得下纳秒,所以单测全绿,换 MySQL 就全红。
func TestHistoryRunTimestampSurvivesMySQL(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "precision", SourceDir: strPtr(historyRepoDir(t, "s")), TargetDir: strPtr(historyRepoDir(t, "t")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	now := time.Now()
	run := &model.HistoryRun{
		ChannelID: ch.ID, Kind: model.HistoryRunKindApply,
		Status: model.HistoryRunSuccess, BaseHead: "b1", Head: "h1",
		CreatedAt: now, FinishedAt: &now,
	}
	run.EntryHash = model.ComputeHistoryRunHash("", run)
	require.NoError(t, h.runs.Create(run))

	stored, err := h.runs.FindByID(run.ID)
	require.NoError(t, err)

	// 落库的值必须没有亚毫秒成分 —— 有的话 MySQL 存不回去。
	require.Zero(t, stored.CreatedAt.Nanosecond()%int(time.Millisecond),
		"created_at 必须以毫秒精度落库,否则 MySQL 会四舍五入")
	require.NotNil(t, stored.FinishedAt)
	require.Zero(t, stored.FinishedAt.Nanosecond()%int(time.Millisecond),
		"finished_at 必须以毫秒精度落库,否则 MySQL 会四舍五入")

	// 模拟 MySQL 往返:按 datetime(3) 的规则(四舍五入到毫秒)再取一次。
	mysqlStored, err := h.runs.FindByID(run.ID)
	require.NoError(t, err)
	mysqlStored.CreatedAt = mysqlRoundTime(mysqlStored.CreatedAt)
	f := mysqlRoundTime(*mysqlStored.FinishedAt)
	mysqlStored.FinishedAt = &f

	require.Equal(t, run.EntryHash, model.ComputeHistoryRunHash("", mysqlStored),
		"经 MySQL 精度往返后哈希必须仍能复算")

	// 整条链仍然完整。
	res, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, res.OK, res.Message)
}

// TestOperationLogChainSurvivesMySQL 回归:既有审计链(OperationLog)
// 同样受时间精度影响 —— 写库前就要把时间截到毫秒。
//
// 注意:这条链是**既有**的,历史数据不能因为改哈希算法而失效,
// 所以哈希算法保持不变,只把「落库前的值」归一到毫秒。
func TestOperationLogChainSurvivesMySQL(t *testing.T) {
	svc := setupOperationLogTestService(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		entry := &model.OperationLog{
			Action: "sync", ResourceType: "repo",
			ResourceKey: "k", Resource: "r", Actor: "u", IP: "127.0.0.1",
			Status: model.StatusSuccess, Detail: "d",
		}
		require.NoError(t, svc.Record(ctx, entry))
	}

	// 落库值必须没有亚毫秒成分。
	stored, err := svc.opLogDAO.Latest()
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Zero(t, stored.CreatedAt.Nanosecond()%int(time.Millisecond),
		"审计日志的 created_at 必须以毫秒精度落库")

	// 模拟 MySQL 往返:datetime(3) 只存毫秒且**四舍五入**。
	// 若落库值带亚毫秒成分,这一步就会改变它 → 哈希对不上 → 链报被篡改。
	mysqlStored := *stored
	mysqlStored.CreatedAt = mysqlRoundTime(mysqlStored.CreatedAt)
	require.Equal(t, stored.EntryHash, model.ComputeEntryHash(stored.PrevHash, &mysqlStored),
		"经 MySQL 精度往返后哈希必须仍能复算")

	res, err := svc.VerifyAuditChain()
	require.NoError(t, err)
	require.True(t, res.OK, res.Message)
	require.Equal(t, 3, res.Checked)
}

// TestHistoryChainDetectsTailTruncation 回归:链尾被截断必须能检出。
//
// 链本身只能证明「前后接得上」—— 删掉末尾几条记录,剩下的链照样接得上。
// 靠通道上的**链尾锚点**才检得出:锚点指向的记录不在了,就是被截断了。
func TestHistoryChainDetectsTailTruncation(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "truncate", SourceDir: strPtr(historyRepoDir(t, "src")),
		TargetDir:    strPtr(historyRepoDir(t, "tgt")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	prev := ""
	for i := 0; i < 3; i++ {
		now := time.Now()
		run := &model.HistoryRun{
			ChannelID: ch.ID, Kind: model.HistoryRunKindApply,
			Status:   model.HistoryRunSuccess,
			BaseHead: fmt.Sprintf("base-%d", i), Head: fmt.Sprintf("head-%d", i),
			CreatedAt: now, FinishedAt: &now, PrevHash: prev,
		}
		run.EntryHash = model.ComputeHistoryRunHash(prev, run)
		require.NoError(t, h.runs.Create(run))
		require.NoError(t, h.channels.UpdateLastEntryHash(ch.ID, run.EntryHash))
		prev = run.EntryHash
	}

	res, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, res.OK, res.Message)
	require.Equal(t, 3, res.Checked)

	// 删掉链尾一条(模拟链尾被截断)。
	runs, err := h.runs.ListByChannelAscending(ch.ID)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	require.NoError(t, h.runs.Delete(runs[2].ID))

	res, err = h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.False(t, res.OK, "链尾被截断必须被检出")
	require.Contains(t, res.Message, "截断")
}

// TestHistoryChainDetectsAllRecordsDeleted 回归:执行记录被**删光**
// 也必须被检出。
//
// 链尾锚点就是为了这个。原先锚点核对排在「Checked == 0」的早退**之后**,
// 于是「把一个通道的记录全删光」反而报「尚无已入链的执行记录」+ OK=true ——
// 证据被删光,审计链却显示完好,比不校验更糟。
func TestHistoryChainDetectsAllRecordsDeleted(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "wiped", SourceDir: strPtr(historyRepoDir(t, "src")),
		TargetDir:    strPtr(historyRepoDir(t, "tgt")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	now := time.Now()
	run := &model.HistoryRun{
		ChannelID: ch.ID, Kind: model.HistoryRunKindApply,
		Status: model.HistoryRunSuccess, BaseHead: "b1", Head: "h1",
		CreatedAt: now, FinishedAt: &now,
	}
	run.EntryHash = model.ComputeHistoryRunHash("", run)
	require.NoError(t, h.runs.Create(run))
	require.NoError(t, h.channels.UpdateLastEntryHash(ch.ID, run.EntryHash))

	require.NoError(t, h.runs.Delete(run.ID))

	res, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.False(t, res.OK, "记录被删光必须被检出(不能报完好)")
	require.Contains(t, res.Message, "截断")
}
