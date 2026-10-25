package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestUpdateTaskLastRun_DoesNotRevertConcurrentChanges 钉住"执行结束回写状态"
// 不得把执行期间的并发修改悄悄改回去。
//
// 回归背景:回写用的是整行 Save(task) —— task 是**执行开始时**加载的快照。
// 于是执行期间运维改了 force_push_policy / cron / 布尔开关,任务一结束就被
// 旧快照整行写回 ⇒ 并发修改丢失(实测:一次 cron 执行把刚改的
// force_push_policy=allow 还原成 block,表现为"改了没生效")。
//
// 正确语义:回写只碰它自己拥有的两列(last_run_at / last_status)。
func TestUpdateTaskLastRun_DoesNotRevertConcurrentChanges(t *testing.T) {
	svc, db := setupCronTestService(t)

	keep := true
	created, err := svc.tasks.CreateTask(t.Context(), &model.CreateTaskRequest{
		Name: "lost-update", SourceRepoKey: "src/repo", TargetRepoKey: "dst/repo",
		SourceBranch: "main", TargetBranch: "main", SyncMode: "single",
		KeepDivergent: &keep, ForcePushPolicy: "block",
	})
	require.NoError(t, err)

	// 执行开始时的快照(= 会被整行写回的那份)
	staleSnapshot, err := svc.tasks.GetTask(t.Context(), created.Key)
	require.NoError(t, err)
	require.Equal(t, "block", staleSnapshot.ForcePushPolicy)

	// 执行期间:运维把策略改成 allow
	allow := "allow"
	_, err = svc.tasks.UpdateTask(t.Context(), &model.UpdateTaskRequest{
		Key: created.Key, ForcePushPolicy: &allow,
	})
	require.NoError(t, err)

	// 执行结束:回写状态(必须只碰 last_run_at / last_status)
	now := time.Now()
	require.NoError(t, svc.tasks.UpdateTaskLastRun(staleSnapshot, &model.SyncRun{
		TaskKey: created.Key, Status: model.StatusSuccess, EndTime: &now,
	}))

	after, err := svc.tasks.GetTask(t.Context(), created.Key)
	require.NoError(t, err)
	require.Equal(t, "allow", after.ForcePushPolicy,
		"回写执行状态不得把执行期间的并发修改(force_push_policy)改回去")
	require.Equal(t, model.StatusSuccess, after.LastStatus, "last_status 应当被更新")
	require.NotNil(t, after.LastRunAt, "last_run_at 应当被更新")

	// 顺带确认其它列也没被快照带回
	require.Equal(t, created.Name, after.Name)
	require.Equal(t, "src/repo", after.SourceRepoKey)
	_ = db
}

// TestUpdateTask_CronCanBeCleared 设过的 cron 必须能**清空**。
//
// 回归背景:UpdateTaskReq.cron 曾是普通 string,空串与"没传"不可区分
// (core 侧 "req.Cron != ” ⇒ 不修改")⇒ 一旦设了 cron 就再也改不掉,
// 只能删任务重建。改成 optional string(nil=不改 / 空串=清空)后才修好。
func TestUpdateTask_CronCanBeCleared(t *testing.T) {
	svc, _ := setupCronTestService(t)

	created, err := svc.tasks.CreateTask(t.Context(), &model.CreateTaskRequest{
		Name: "cron-clear", SourceRepoKey: "src/repo", TargetRepoKey: "dst/repo",
		SourceBranch: "main", TargetBranch: "main", SyncMode: "single", Cron: "* * * * *",
	})
	require.NoError(t, err)
	require.Equal(t, "* * * * *", created.Cron)

	// 空串 = 清空定时
	empty := ""
	updated, err := svc.tasks.UpdateTask(t.Context(), &model.UpdateTaskRequest{
		Key: created.Key, Cron: &empty,
	})
	require.NoError(t, err)
	require.Empty(t, updated.Cron, "空串必须能把 cron 清空")

	after, err := svc.tasks.GetTask(t.Context(), created.Key)
	require.NoError(t, err)
	require.Empty(t, after.Cron, "cron 必须真正被清空(落库校验)")

	// nil = 不修改
	other := "0 3 * * *"
	updated2, err := svc.tasks.UpdateTask(t.Context(), &model.UpdateTaskRequest{
		Key: created.Key, Cron: &other,
	})
	require.NoError(t, err)
	require.Equal(t, "0 3 * * *", updated2.Cron)

	// nil 时不修改其它字段(名字保持)
	updated3, err := svc.tasks.UpdateTask(t.Context(), &model.UpdateTaskRequest{
		Key: created.Key, Name: "renamed",
	})
	require.NoError(t, err)
	require.Equal(t, "0 3 * * *", updated3.Cron, "不传 cron 时 cron 不得被清掉")
	require.Equal(t, "renamed", updated3.Name)
}

// TestUpdateTask_CanClearFields 这三个字段必须能**清空**。
//
// 回归背景:它们是普通 string,空串与"没传"不可区分 ⇒ 一旦设过就再也改不回去
// (只能删任务重建)。与之前修好的 Cron 同一类问题。
func TestUpdateTask_CanClearFields(t *testing.T) {
	svc, _ := setupCronTestService(t)

	keep := true
	policy := "block"
	include := "release/*"
	exclude := "refs/pull/*"
	created, err := svc.tasks.CreateTask(t.Context(), &model.CreateTaskRequest{
		Name: "clear-fields", SourceRepoKey: "src/repo", TargetRepoKey: "dst/repo",
		SourceBranch: "main", TargetBranch: "main", SyncMode: "single",
		KeepDivergent:      &keep,
		ForcePushPolicy:    policy,
		IncludeBranches:    include,
		ExcludeRefPatterns: exclude,
	})
	require.NoError(t, err)

	empty := ""
	_, err = svc.tasks.UpdateTask(t.Context(), &model.UpdateTaskRequest{
		Key: created.Key,
		// 三个都显式给空串 ⇒ 应当被清空
		ForcePushPolicy:    &empty,
		IncludeBranches:    &empty,
		ExcludeRefPatterns: &empty,
	})
	require.NoError(t, err)

	got, err := svc.tasks.GetTask(t.Context(), created.Key)
	require.NoError(t, err)
	assert.Empty(t, got.ForcePushPolicy, "force_push_policy 应被清空")
	assert.Empty(t, got.IncludeBranches, "include_branches 应被清空")
	assert.Empty(t, got.ExcludeRefPatterns, "exclude_ref_patterns 应被清空")

	// 不传(nil)⇒ 不修改
	_, err = svc.tasks.UpdateTask(t.Context(), &model.UpdateTaskRequest{Key: created.Key, Name: "renamed"})
	require.NoError(t, err)
	got, err = svc.tasks.GetTask(t.Context(), created.Key)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Empty(t, got.ForcePushPolicy, "不传 ⇒ 不该被改动")
}
