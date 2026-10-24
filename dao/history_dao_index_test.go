package dao

import (
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

// newHistoryRunDAOTestDB 建一个带 history_runs 表的测试库。
func newHistoryRunDAOTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/runs.db"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.HistoryRun{}))
	return db
}

// TestListPendingProductDirs 回归:待推送产物清单的**语义**必须不变。
//
// 「待推送」= apply 记录,且没有成功的 push 记录指向它。
// 这条清单保护的是「apply 与 push 之间的产物」—— 清理逻辑一旦把它算错,
// 还没推送的产物就会被删掉,推送必然失败(而且产物不可恢复)。
func TestListPendingProductDirs(t *testing.T) {
	db := newHistoryRunDAOTestDB(t)
	d := NewHistoryRunDAO(db)

	mk := func(kind, status string, pushedRunID uint, path string) *model.HistoryRun {
		run := &model.HistoryRun{
			ChannelID: 1, Kind: kind, Status: status,
			PushedRunID: pushedRunID, WorkRepoPath: path,
		}
		require.NoError(t, d.Create(run))
		return run
	}

	pending := mk(model.HistoryRunKindApply, model.HistoryRunSuccess, 0, "/tmp/p1")
	mk(model.HistoryRunKindApply, model.HistoryRunSuccess, 0, "/tmp/p2")
	pushed := mk(model.HistoryRunKindApply, model.HistoryRunSuccess, 0, "/tmp/done")
	mk(model.HistoryRunKindPush, model.HistoryRunSuccess, pushed.ID, "")
	// 失败的 push 不算已推送:apply 产物仍待推送。
	mk(model.HistoryRunKindPush, model.HistoryRunFailed, pending.ID, "")
	// 非 apply 记录不参与。
	mk(model.HistoryRunKindRollback, model.HistoryRunSuccess, 0, "/tmp/rb")

	dirs, err := d.ListPendingProductDirs()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"/tmp/p1", "/tmp/p2"}, dirs,
		"已推送成功的 apply 产物不该在待推送清单里")
}

// TestListPendingProductDirsUsesIndex 回归:待推送清单**不能是全表扫描**。
//
// 背景(第四轮实测):这条查询挂在**每次 apply 与每次 push 的热路径上**
// (apply 调一次、push 调两次),而 history_runs 每动作涨一行 ——
// 1 万行时 EXPLAIN QUERY PLAN 给出 `SCAN history_runs`,
// 也就是说「最频繁的查询」恰好是全表扫描。
//
// 这里用 EXPLAIN 钉住:必须走索引(SEARCH ... USING INDEX),
// 不能退回 SCAN。索引不是优化,是这条查询能撑住的前提。
func TestListPendingProductDirsUsesIndex(t *testing.T) {
	db := newHistoryRunDAOTestDB(t)
	d := NewHistoryRunDAO(db)

	// 造一批数据,让查询计划的选择有意义。
	for i := 0; i < 200; i++ {
		run := &model.HistoryRun{
			ChannelID: 1, Kind: model.HistoryRunKindApply,
			Status: model.HistoryRunSuccess, WorkRepoPath: "/tmp/p",
		}
		require.NoError(t, d.Create(run))
	}

	plan := explainQueryPlan(t, db,
		"SELECT work_repo_path FROM history_runs "+
			"WHERE kind = 'apply' AND work_repo_path <> '' "+
			"AND id NOT IN (SELECT pushed_run_id FROM history_runs "+
			"WHERE kind = 'push' AND status = 'success' AND pushed_run_id <> 0)")
	t.Logf("查询计划:%s", plan)

	assert.NotContains(t, plan, "SCAN history_runs",
		"待推送清单不能是全表扫描(热路径上的查询):%s", plan)
	assert.Contains(t, plan, "USING INDEX",
		"待推送清单应走索引:%s", plan)
}

// explainQueryPlan 返回 sqlite 的查询计划文本。
func explainQueryPlan(t *testing.T, db *gorm.DB, query string) string {
	t.Helper()
	rows, err := db.Raw("EXPLAIN QUERY PLAN " + query).Rows()
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		out += detail + " | "
	}
	return out
}
