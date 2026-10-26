package dao

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/model"
)

func newTaskFilterDAO(t *testing.T) *SyncTaskDAO {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return NewSyncTaskDAO(db)
}

// TestSyncTaskDAO_FindFiltered 状态过滤必须真的生效。
//
// 回归背景(P1,契约漂移):CLI 的 `task +list --status` 一直在发 status,
// 但服务端 ListTasksReq 没有这个字段 ⇒ hertz 静默忽略 ⇒ 用户拿到全量
// 却以为筛过了。
//
// 这条用例是**反向验证**:把 FindFiltered 里的 status 条件去掉,本用例变红。
func TestSyncTaskDAO_FindFiltered(t *testing.T) {
	d := newTaskFilterDAO(t)
	require.NoError(t, d.Create(&model.SyncTask{
		Key: "t-ok", Name: "ok", Enabled: true, LastStatus: model.StatusSuccess, WebhookToken: "wt-ok",
	}))
	require.NoError(t, d.Create(&model.SyncTask{
		Key: "t-bad", Name: "bad", Enabled: true, LastStatus: model.StatusFailed, WebhookToken: "wt-bad",
	}))
	require.NoError(t, d.Create(&model.SyncTask{
		Key: "t-new", Name: "new", Enabled: true, LastStatus: "", WebhookToken: "wt-new",
	}))

	list, total, err := d.FindFiltered("", model.StatusSuccess, DefaultPagination(0, 50))
	require.NoError(t, err)
	require.Equal(t, int64(1), total, "status 过滤后总数必须只算命中的那批")
	require.Len(t, list, 1)
	assert.Equal(t, "t-ok", list[0].Key)

	// 失败态同理
	list, total, err = d.FindFiltered("", model.StatusFailed, DefaultPagination(0, 50))
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, list, 1)
	assert.Equal(t, "t-bad", list[0].Key)

	// 空 status = 不过滤(全量)
	_, total, err = d.FindFiltered("", "", DefaultPagination(0, 50))
	require.NoError(t, err)
	assert.Equal(t, int64(3), total, "空 status 必须返回全量")
}

// TestSyncTaskDAO_FindFiltered_ByRepoKey 仓库 + 状态两个维度要能同时生效。
func TestSyncTaskDAO_FindFiltered_ByRepoKey(t *testing.T) {
	d := newTaskFilterDAO(t)
	require.NoError(t, d.Create(&model.SyncTask{
		Key: "t-a", Name: "a", Enabled: true, LastStatus: model.StatusSuccess, WebhookToken: "wt-a",
		SourceRepoKey: "github/x/a", TargetRepoKey: "github/y/a",
	}))
	require.NoError(t, d.Create(&model.SyncTask{
		Key: "t-b", Name: "b", Enabled: true, LastStatus: model.StatusSuccess, WebhookToken: "wt-b",
		SourceRepoKey: "github/x/b", TargetRepoKey: "github/y/b",
	}))
	require.NoError(t, d.Create(&model.SyncTask{
		Key: "t-c", Name: "c", Enabled: true, LastStatus: model.StatusFailed, WebhookToken: "wt-c",
		SourceRepoKey: "github/x/a", TargetRepoKey: "github/y/a",
	}))

	list, total, err := d.FindFiltered("github/x/a", model.StatusSuccess, DefaultPagination(0, 50))
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, list, 1)
	assert.Equal(t, "t-a", list[0].Key)
}

// TestSyncRunDAO_FindByTaskKeyAndRunID 按 run_id 精确查必须只返回那一条。
//
// 回归背景(P1):CLI 的 `history +detail --run-id` 之前只能靠翻页找那条,
// 服务端没有按 run_id 过滤的能力 ⇒ 找不到就静默返回空列表。
func TestSyncRunDAO_FindByTaskKeyAndRunID(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncRun{}, &model.SyncRunStep{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	d := NewSyncRunDAO(db)

	r1 := &model.SyncRun{TaskKey: "t1", Status: model.StatusSuccess, StartTime: time.Now()}
	r2 := &model.SyncRun{TaskKey: "t1", Status: model.StatusFailed, StartTime: time.Now()}
	r3 := &model.SyncRun{TaskKey: "t2", Status: model.StatusSuccess, StartTime: time.Now()}
	require.NoError(t, d.Create(r1))
	require.NoError(t, d.Create(r2))
	require.NoError(t, d.Create(r3))

	got, err := d.FindByTaskKeyAndRunID("t1", r2.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, r2.ID, got.ID)
	assert.Equal(t, model.StatusFailed, got.Status)

	// 别的任务的同号记录不能被串出来
	_, err = d.FindByTaskKeyAndRunID("t1", r3.ID)
	require.Error(t, err, "跨任务的记录不得被匹配")
}
