package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestForcePushStore_RoundTrip 自壳 forcepush_approval_test.go 迁移：
// 写入 → 未放行 → 放行 → 已放行；并锁定 JSON 字段名与历史文件兼容。
func TestForcePushStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "force-push-approvals.json")
	st := NewForcePushStore(path)

	// 空存储读取不报错
	assert.Empty(t, st.List())
	assert.False(t, st.IsForcePushApproved("t1", "main"))

	item, err := st.Request("t1", "main", "divergent")
	require.NoError(t, err)
	require.NotEmpty(t, item.ID)

	require.FileExists(t, path)
	got := st.List()
	require.Len(t, got, 1)
	assert.True(t, len(got[0].ID) > 3 && got[0].ID[:3] == "fp-", "id 应为 fp- 前缀: %s", got[0].ID)
	assert.Equal(t, "t1", got[0].TaskKey)
	assert.Equal(t, "main", got[0].Branch)
	assert.False(t, st.IsForcePushApproved("t1", "main"))

	found, err := st.Approve(item.ID, "admin")
	require.NoError(t, err)
	assert.True(t, found)
	assert.True(t, st.IsForcePushApproved("t1", "main"))
	assert.Equal(t, 0, st.PendingCount())

	// 未找到的 ID
	found, err = st.Approve("nope", "admin")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestForcePushStore_ApproverInterface 审批存储即执行器默认 approver。
func TestForcePushStore_ApproverInterface(t *testing.T) {
	st := NewForcePushStore(filepath.Join(t.TempDir(), "a.json"))
	var approver interface {
		IsForcePushApproved(taskKey, branch string) bool
		RequestApproval(taskKey, branch, reason string)
	} = st

	// 幂等：重复 RequestApproval 只留一条 pending
	approver.RequestApproval("k", "main", "r1")
	approver.RequestApproval("k", "main", "r2")
	list := st.List()
	require.Len(t, list, 1)
	assert.False(t, st.IsForcePushApproved("k", "main"))
}

// TestForcePushStore_JSONCompat 锁定序列化字段名（既有部署文件必须还能读）。
func TestForcePushStore_JSONCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "force-push-approvals.json")
	legacy := `[
	  {"id":"fp-9","task_key":"legacy","branch":"dev","reason":"r",
	   "created_at":"2026-01-02T03:04:05Z","approved":true,
	   "approved_by":"admin","approved_at":"2026-01-02T03:05:00Z"}
	]`
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
	st := NewForcePushStore(path)
	got := st.List()
	require.Len(t, got, 1)
	assert.Equal(t, "fp-9", got[0].ID)
	assert.Equal(t, "legacy", got[0].TaskKey)
	assert.True(t, st.IsForcePushApproved("legacy", "dev"))
}
