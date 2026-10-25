package service

import (
	"encoding/json"
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
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	assert.Empty(t, st.List())
	assert.False(t, st.IsForcePushApproved("t1", "main", ""))
	assert.Equal(t, 0, st.PendingCount())

	item, err := st.Request("t1", "main", "divergent", "")
	require.NoError(t, err)
	require.NotEmpty(t, item.ID)
	assert.True(t, len(item.ID) > 3 && item.ID[:3] == "fp-", "id 应为 fp- 前缀: %s", item.ID)

	got := st.List()
	require.Len(t, got, 1)
	assert.Equal(t, "t1", got[0].TaskKey)
	assert.Equal(t, "main", got[0].Branch)
	assert.Equal(t, 1, st.PendingCount())
	assert.False(t, st.IsForcePushApproved("t1", "main", ""))

	found, err := st.Approve(item.ID, "admin")
	require.NoError(t, err)
	assert.True(t, found)
	assert.True(t, st.IsForcePushApproved("t1", "main", ""))
	assert.Equal(t, 0, st.PendingCount())

	found, err = st.Approve("nope", "admin")
	require.NoError(t, err)
	assert.False(t, found)
}

// TestForcePushStore_ApproverInterface 审批存储即执行器默认 approver（幂等 pending）。
func TestForcePushStore_ApproverInterface(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))
	var approver interface {
		IsForcePushApproved(taskKey, branch, targetRef string) bool
		RequestApproval(taskKey, branch, reason, targetRef string)
	} = st

	approver.RequestApproval("k", "main", "r1", "")
	approver.RequestApproval("k", "main", "r2", "")
	require.Len(t, st.List(), 1, "重复 RequestApproval 只留一条 pending")
	assert.False(t, st.IsForcePushApproved("k", "main", ""))

	// 放行是一次性的：之后可再产生新 pending（与历史文件实现一致）
	id := st.List()[0].ID
	found, err := st.Approve(id, "admin")
	require.NoError(t, err)
	require.True(t, found)
	approver.RequestApproval("k", "main", "r3", "")
	require.Len(t, st.List(), 2, "放行后应允许新的 pending 申请")
	assert.False(t, st.List()[1].Approved)
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

// TestForcePushStore_ConsumeForcePushApproval 放行是**一次性**的:
// 用掉之后同任务同分支不再算数。
//
// 回归背景:契约写的是"Admin 一次性放行后允许**本次** force",
// 但实现只查 approved 标志 ⇒ 一次人工放行变成该任务+分支上的**常驻后门**,
// 之后每次覆盖目标侧独有提交都无人复核。
func TestForcePushStore_ConsumeForcePushApproval(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	st.RequestApproval("t1", "main", "divergent target has 1 unique commits", "sha-1")
	id := st.List()[0].ID
	found, err := st.Approve(id, "admin")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, st.IsForcePushApproved("t1", "main", ""), "放行后应当生效")

	// 用掉
	st.ConsumeForcePushApproval("t1", "main", "sha-1")
	assert.False(t, st.IsForcePushApproved("t1", "main", ""),
		"放行被用掉后不再算数(否则一次人工放行 = 常驻后门)")

	// 记录保留(审计线索:谁批的、什么时候批的、什么时候用的)
	list := st.List()
	require.Len(t, list, 1, "记录要保留,不能删")
	assert.NotNil(t, list[0].ConsumedAt, "要标出放行被用掉的时间")
	assert.Equal(t, "admin", list[0].ApprovedBy, "谁批的要留着")
	assert.True(t, list[0].Approved, "放行事实不变")

	// 用掉之后,同任务同分支可以重新申请并重新走审批
	st.RequestApproval("t1", "main", "again divergent", "sha-2")
	list = st.List()
	require.Len(t, list, 2, "用掉之后应能再产生新 pending")
	assert.False(t, list[1].Approved)
}

// TestForcePushStore_Revoke 撤销后放行不再生效,且记录保留可查。
//
// 为什么需要撤销:放行是"人工授权覆盖目标侧独有提交",批错了要在**兑现之前**收回;
// 没有撤销口子的话唯一的取消办法是等它被用掉 —— 而那正是最不该覆盖的时刻。
func TestForcePushStore_Revoke(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	st.RequestApproval("t1", "main", "divergent", "sha-1")
	id := st.List()[0].ID
	found, err := st.Approve(id, "admin")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, st.IsForcePushApproved("t1", "main", ""), "放行后应当生效")

	// 撤销
	found, err = st.Revoke(id, "admin2")
	require.NoError(t, err)
	require.True(t, found, "撤销应当找到这条记录")
	assert.False(t, st.IsForcePushApproved("t1", "main", ""), "撤销后放行不再生效")

	// 记录保留(审计线索:批了、又撤了)
	list := st.List()
	require.Len(t, list, 1, "记录要保留,不能删")
	assert.Equal(t, "admin", list[0].ApprovedBy, "谁批的要留着")
	assert.Equal(t, "admin2", list[0].RevokedBy, "谁撤的要留着")
	assert.NotNil(t, list[0].RevokedAt, "要标出撤销时间")
	assert.True(t, list[0].Approved, "放行事实不变(只是被撤销了)")

	// 撤销不存在的记录
	found, err = st.Revoke("no-such-id", "admin2")
	require.NoError(t, err)
	assert.False(t, found, "撤销不存在的记录应当 found=false")
}

// TestForcePushStore_ApprovalBoundToTargetState 放行必须绑定到"申请时的目标状态"。
//
// 回归背景:审批是"针对这一次分歧"的授权。目标侧若在审批之后又前进
// (独有提交从 1 个变成 50 个),旧审批不该继续适用 ——
// 否则一次"批 1 个独有提交"会变成"授权覆盖此后的一切"。
func TestForcePushStore_ApprovalBoundToTargetState(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	st.RequestApproval("t1", "main", "divergent target has 1 unique commits", "sha-1")
	id := st.List()[0].ID
	found, err := st.Approve(id, "admin")
	require.NoError(t, err)
	require.True(t, found)

	// 目标还是申请时那个状态 ⇒ 放行有效
	assert.True(t, st.IsForcePushApproved("t1", "main", "sha-1"), "目标未变时放行应有效")

	// 目标变了 ⇒ 放行不再适用(必须重新审批)
	assert.False(t, st.IsForcePushApproved("t1", "main", "sha-2"),
		"目标侧变了之后,旧放行不该继续适用")

	// 记录里能看到绑定的是哪个 SHA(可审计)
	list := st.List()
	require.Len(t, list, 1)
	assert.Equal(t, "sha-1", list[0].TargetRef, "要记下放行是针对哪个目标状态批的")
}

// TestForcePushStore_PendingRefreshesToCurrentState 已存在的 pending 必须**刷新**
// 到当前状态,而不是一直挂着旧的。
//
// 回归背景:pending 是"针对目标某个状态"的申请。目标侧此后又变了
// (独有提交从 1 个变成 50 个)时,旧的 pending 仍挂着、绑的还是**旧状态**
// ⇒ 运维批的是一份过期的描述,批完的放行也对应旧状态。
func TestForcePushStore_PendingRefreshesToCurrentState(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	st.RequestApproval("t1", "main", "divergent target has 1 unique commits", "sha-1")
	require.Len(t, st.List(), 1)
	firstID := st.List()[0].ID

	// 目标侧又变了,重新申请 ⇒ 仍是**同一条** pending,但内容刷新到当前状态
	st.RequestApproval("t1", "main", "divergent target has 50 unique commits", "sha-2")
	require.Len(t, st.List(), 1, "同一 task+branch 只留一条 pending")
	pending := st.List()[0]
	assert.Equal(t, firstID, pending.ID, "应是同一条记录(不重复建)")
	assert.Equal(t, "sha-2", pending.TargetRef, "pending 必须绑到当前目标状态")
	assert.Contains(t, pending.Reason, "50", "pending 的描述必须反映当前分歧")
}

// TestForcePushStore_ConsumeBindsTargetRef 消费必须**精确落在判定通过的那条**放行上。
//
// 回归背景(P0):判定"能不能推"走 IsApproved(它按 target_ref 绑定),
// 而"用掉哪条放行"走 ConsumeApproved —— 原实现**不绑定 target_ref**,
// 且取"最新一条 approved" ⇒ 被消费掉的可能是**另一条**放行,
// 真正被授权的那条仍然 approved 且未消费 ⇒ 同一次人工放行可以反复授权覆盖,
// "一次性"变成常驻后门。
func TestForcePushStore_ConsumeBindsTargetRef(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	// 两条已放行记录:A 绑 SHA_X(较早),B 绑 SHA_Y(较新)。
	st.RequestApproval("t1", "main", "divergent at sha-x", "sha-x")
	idA := approvalIDByTargetRef(t, st, "sha-x")
	found, err := st.Approve(idA, "admin")
	require.NoError(t, err)
	require.True(t, found)

	st.RequestApproval("t1", "main", "divergent at sha-y", "sha-y")
	idB := approvalIDByTargetRef(t, st, "sha-y")
	found, err = st.Approve(idB, "admin")
	require.NoError(t, err)
	require.True(t, found)

	// 目标侧此刻在 SHA_X ⇒ 判定命中的是 A。
	require.True(t, st.IsForcePushApproved("t1", "main", "sha-x"), "A 应生效")

	// 消费这次授权(必须落在 A 上)。
	st.ConsumeForcePushApproval("t1", "main", "sha-x")

	// 关键断言:A 已被用掉 —— 否则同一次放行还能再用。
	assert.False(t, st.IsForcePushApproved("t1", "main", "sha-x"),
		"被授权的那条放行必须被用掉(否则一次放行 = 常驻后门)")
	// B 绑的是别的目标状态,不该被动到。
	assert.True(t, st.IsForcePushApproved("t1", "main", "sha-y"),
		"另一条绑不同目标状态的放行不该被这次消费误伤")
}

// TestForcePushStore_ConsumeSkipsRevoked 已撤销的放行不得被消费。
//
// 撤销 = 授权被收回;消费它等于把"已收回"记成"已用掉",
// 两条记录的状态都乱了(而且撤销的授权本就不该兑现)。
func TestForcePushStore_ConsumeSkipsRevoked(t *testing.T) {
	st := NewForcePushStore(dao.NewForcePushApprovalDAO(newApprovalDB(t)))

	st.RequestApproval("t1", "main", "divergent at sha-r", "sha-r")
	idR := approvalIDByTargetRef(t, st, "sha-r")
	found, err := st.Approve(idR, "admin")
	require.NoError(t, err)
	require.True(t, found)

	found, err = st.Revoke(idR, "admin")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, st.IsForcePushApproved("t1", "main", "sha-r"), "撤销后不该再算已放行")

	// 消费时不能选中已撤销的那条(它没有 consumed_at,但已被收回)。
	st.ConsumeForcePushApproval("t1", "main", "sha-r")

	for _, a := range st.List() {
		if a.ID == idR {
			assert.Nil(t, a.ConsumedAt, "已撤销的放行不该被标记成已用掉")
		}
	}
}

// approvalIDByTargetRef 找出绑定了指定目标状态的放行 id。
func approvalIDByTargetRef(t *testing.T, st *ForcePushStore, targetRef string) string {
	t.Helper()
	for _, a := range st.List() {
		if a.TargetRef == targetRef {
			return a.ID
		}
	}
	t.Fatalf("没找到 target_ref=%q 的放行(现有:%+v)", targetRef, st.List())
	return ""
}
