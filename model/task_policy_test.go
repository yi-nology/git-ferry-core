package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func policyTask(key string) *SyncTask {
	return &SyncTask{
		Key: key, SourceRepoKey: "src", SourceBranch: "main",
		TargetRepoKey: "tgt", TargetBranch: "main",
		SyncMode: "single", Enabled: true, GitForce: true,
		ForcePushPolicy: "block", IncludeBranches: "main,release/*",
	}
}

// TestBuildTaskPolicySnapshot_Deterministic 快照确定性:
// 同任务 ⇒ 同字节(跨 run 比对「策略变了没有」的前提)。
func TestBuildTaskPolicySnapshot_Deterministic(t *testing.T) {
	task := policyTask("t1")
	a := BuildTaskPolicySnapshot(task)
	b := BuildTaskPolicySnapshot(task)
	require.NotEmpty(t, a)
	assert.Equal(t, a, b, "同任务状态必须产生同字节快照")

	// 策略变了 ⇒ 字节变(快照要能感知变更)
	task.GitForce = false
	c := BuildTaskPolicySnapshot(task)
	assert.NotEqual(t, a, c, "策略变更必须改变快照字节")

	// nil 安全
	assert.Empty(t, BuildTaskPolicySnapshot(nil))
}

// TestExportTaskPolicies_SortedAndVersioned 导出按 key 升序 + 版本信封。
func TestExportTaskPolicies_SortedAndVersioned(t *testing.T) {
	doc, err := ExportTaskPolicies([]*SyncTask{
		policyTask("zeta"), policyTask("alpha"), nil, policyTask("mid"),
	})
	require.NoError(t, err)
	assert.Contains(t, doc, `"version": 1`)
	// key 升序:alpha 在 mid 剉,zeta 最后
	iAlpha := indexOf(doc, `"key": "alpha"`)
	iMid := indexOf(doc, `"key": "mid"`)
	iZeta := indexOf(doc, `"key": "zeta"`)
	require.True(t, iAlpha >= 0 && iMid > iAlpha && iZeta > iMid,
		"导出必须按 key 升序(确定性,变更 diff 友好)")
	// 凭据绝不进策略文件
	assert.NotContains(t, doc, "webhook_token")
	assert.NotContains(t, doc, "webhookToken")
}

// TestParseTaskPolicies_VersionGuard 版本信封校验:未知版本拒绝(fail-closed)。
func TestParseTaskPolicies_VersionGuard(t *testing.T) {
	doc, err := ExportTaskPolicies([]*SyncTask{policyTask("t1")})
	require.NoError(t, err)

	ps, err := ParseTaskPolicies([]byte(doc))
	require.NoError(t, err)
	require.Len(t, ps, 1)
	assert.Equal(t, "t1", ps[0].Key)
	assert.Equal(t, "block", ps[0].ForcePushPolicy)

	// 未知版本拒绝
	_, err = ParseTaskPolicies([]byte(`{"version": 99, "tasks": []}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported policy doc version")

	// 垃圾数据拒绝
	_, err = ParseTaskPolicies([]byte(`not json`))
	require.Error(t, err)
}

// TestTaskPolicy_ToCreateRequest 策略条目 → 建任务请求(回放创建路径)。
func TestTaskPolicy_ToCreateRequest(t *testing.T) {
	ps, err := ParseTaskPolicies([]byte(`{"version":1,"tasks":[{
		"key":"t1","source_repo_key":"s","source_branch":"main",
		"target_repo_key":"t","target_branch":"dev","sync_mode":"single",
		"enabled":false,"git_force":true,"force_push_policy":"allow"
	}]}`))
	require.NoError(t, err)
	req := ps[0].ToCreateRequest()
	assert.Equal(t, "t1", req.Name)
	assert.Equal(t, "s", req.SourceRepoKey)
	assert.Equal(t, "dev", req.TargetBranch)
	assert.True(t, req.GitForce)
	assert.Equal(t, "allow", req.ForcePushPolicy)
	// CreateTaskRequest 无 Enabled 字段——禁用语义由回放侧建后 Update
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
