package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// newApprovalDB 建一个带审批表的内存库。
func newApprovalDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "approval.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ForcePushApproval{}))
	return db
}

// TestForcePushStore_RoundTrip 请求 → 未放行 → 放行 → 已放行。
func TestForcePushStore_RoundTrip(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)), "")

	assert.Empty(t, st.List())
	assert.False(t, st.IsForcePushApproved("t1", "main"))
	assert.Equal(t, 0, st.PendingCount())

	item, err := st.Request("t1", "main", "divergent")
	require.NoError(t, err)
	require.NotEmpty(t, item.ID)
	assert.True(t, len(item.ID) > 3 && item.ID[:3] == "fp-", "id 应为 fp- 前缀: %s", item.ID)

	got := st.List()
	require.Len(t, got, 1)
	assert.Equal(t, "t1", got[0].TaskKey)
	assert.Equal(t, "main", got[0].Branch)
	assert.Equal(t, 1, st.PendingCount())
	assert.False(t, st.IsForcePushApproved("t1", "main"))

	found, err := st.Approve(item.ID, "admin")
	require.NoError(t, err)
	assert.True(t, found)
	assert.True(t, st.IsForcePushApproved("t1", "main"))
	assert.Equal(t, 0, st.PendingCount())

	found, err = st.Approve("nope", "admin")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestForcePushStore_ApproverInterface 审批存储即执行器默认 approver（幂等 pending）。
func TestForcePushStore_ApproverInterface(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)), "")
	var approver interface {
		IsForcePushApproved(taskKey, branch string) bool
		RequestApproval(taskKey, branch, reason string)
	} = st

	approver.RequestApproval("k", "main", "r1")
	approver.RequestApproval("k", "main", "r2")
	require.Len(t, st.List(), 1, "重复 RequestApproval 只留一条 pending")
	assert.False(t, st.IsForcePushApproved("k", "main"))

	// 放行是一次性的：之后可再产生新 pending（与历史文件实现一致）
	id := st.List()[0].ID
	found, err := st.Approve(id, "admin")
	require.NoError(t, err)
	require.True(t, found)
	approver.RequestApproval("k", "main", "r3")
	require.Len(t, st.List(), 2, "放行后应允许新的 pending 申请")
	assert.False(t, st.List()[1].Approved)
}

// TestForcePushStore_LegacyImport 旧 JSON 文件在空表时一次性导入，非空表不再导入。
func TestForcePushStore_LegacyImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "force-push-approvals.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	legacy := `[
	  {"id":"fp-9","task_key":"legacy","branch":"dev","reason":"r",
	   "created_at":"2026-01-02T03:04:05Z","approved":true,
	   "approved_by":"admin","approved_at":"2026-01-02T03:05:00Z"}
	]`
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))

	db := newApprovalDB(t)
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(db), path)
	require.Len(t, st.List(), 1, "空表应导入旧文件")
	assert.Equal(t, "fp-9", st.List()[0].ID)
	assert.True(t, st.IsForcePushApproved("legacy", "dev"), "JSON 字段名与历史文件兼容")

	// 表非空 + 旧文件变化 → 不再导入
	require.NoError(t, os.WriteFile(path, []byte(`[{"id":"fp-10","task_key":"x","branch":"y","created_at":"2026-01-03T00:00:00Z"}]`), 0o600))
	st2 := NewForcePushStore(dao.NewForcePushApprovalDAO(db), path)
	require.Len(t, st2.List(), 1, "表非空时不应重复导入")
}

// TestForcePushStore_JSONContract 锁定序列化字段名（HTTP 响应与历史文件契约）。
func TestForcePushStore_JSONContract(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
	b, err := json.Marshal(ForcePushApproval{
		ID: "fp-1", TaskKey: "t", Branch: "b", Reason: "r",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
		Approved:  true, ApprovedBy: "admin", ApprovedAt: &at,
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	for _, k := range []string{"id", "task_key", "branch", "reason", "created_at", "approved", "approved_by", "approved_at"} {
		assert.Contains(t, m, k, "缺少字段 %s", k)
	}
	// omitempty：未填 approved_by 不应出现
	b2, err := json.Marshal(ForcePushApproval{ID: "fp-2", TaskKey: "t", Branch: "b"})
	require.NoError(t, err)
	assert.NotContains(t, string(b2), "approved_by")
}
