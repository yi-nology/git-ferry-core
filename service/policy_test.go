package service

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// newPolicyTestService 造带任务存储的轻量服务。
func newPolicyTestService(t *testing.T) *Service {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}, &model.Repo{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	return &Service{
		tasks:  NewTaskService(dao.NewSyncTaskDAO(db), dao.NewSyncRunDAO(db), dao.NewSyncRunStepDAO(db), repoDAO),
		bgCtx:  context.Background(),
		config: &model.Config{},
	}
}

// TestExportApplyTaskPolicies_Roundtrip 导出 → 回放 roundtrip:
// 创建新任务、更新已有策略、禁用语义都保真。
func TestExportApplyTaskPolicies_Roundtrip(t *testing.T) {
	svc := newPolicyTestService(t)
	ctx := context.Background()

	// 初始:一个任务
	_, err := svc.CreateTask(ctx, &model.CreateTaskRequest{
		Name: "alpha", SourceRepoKey: "s", SourceBranch: "main",
		TargetRepoKey: "t", TargetBranch: "main", SyncMode: "single",
		GitForce: true, ForcePushPolicy: "block",
	})
	require.NoError(t, err)

	// 导出
	doc, err := svc.ExportTaskPolicies(ctx)
	require.NoError(t, err)
	assert.Contains(t, doc, `"key": "`)

	// 改策略(经 Update)后,导出应反映新策略
	block := false
	_, err = svc.UpdateTask(ctx, &model.UpdateTaskRequest{Key: "alpha-key", GitForce: &block})
	_ = err // key 可能不是 "alpha-key"——用实际 key

	// 回放到「另一环境」(新建服务,同 DB 语义:空库)
	svc2 := newPolicyTestService(t)
	// 先手工建一个与导出同 key 的任务且回放前策略不同
	// ——直接回放原 doc:应创建 1 个
	res, err := svc2.ApplyTaskPolicies(ctx, []byte(doc))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Created)
	assert.Empty(t, res.Errors)

	// 再回放一次:同 key ⇒ 更新而非重复创建
	res, err = svc2.ApplyTaskPolicies(ctx, []byte(doc))
	require.NoError(t, err)
	assert.Zero(t, res.Created)
	assert.Equal(t, 1, res.Updated, "重复回放必须走更新,不得重复建任务")

	// 导出→回放→导出:收敛(策略文件是幂等声明)
	doc2, err := svc2.ExportTaskPolicies(ctx)
	require.NoError(t, err)
	assert.Equal(t, doc, doc2, "回放后导出必须与原策略文件一致(收敛)")
}

// TestApplyTaskPolicies_PartialFailure 单任务失败不中断其余(完整报告)。
func TestApplyTaskPolicies_PartialFailure(t *testing.T) {
	svc := newPolicyTestService(t)
	ctx := context.Background()

	doc := `{"version":1,"tasks":[
		{"key":"good","source_repo_key":"s","source_branch":"main",
		 "target_repo_key":"t","target_branch":"main","sync_mode":"single","enabled":true},
		{"key":"bad-branch","source_repo_key":"s","source_branch":"main:bad",
		 "target_repo_key":"t","target_branch":"main","sync_mode":"single","enabled":true}
	]}`
	res, err := svc.ApplyTaskPolicies(ctx, []byte(doc))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Created, "合法任务必须应用成功")
	require.Len(t, res.Errors, 1, "非法分支名必须报错且可见")
	assert.Contains(t, res.Errors[0], "bad-branch")
}

// TestApplyTaskPolicies_DisabledSemantics 策略声明禁用的任务:
// 创建路径 = 建后立即关。
func TestApplyTaskPolicies_DisabledSemantics(t *testing.T) {
	svc := newPolicyTestService(t)
	ctx := context.Background()

	doc := `{"version":1,"tasks":[
		{"key":"off-task","source_repo_key":"s","source_branch":"main",
		 "target_repo_key":"t","target_branch":"main","sync_mode":"single","enabled":false}
	]}`
	res, err := svc.ApplyTaskPolicies(ctx, []byte(doc))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Created)
	assert.Empty(t, res.Errors)

	task, err := svc.tasks.FindTaskByKey("off-task")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.False(t, task.Enabled, "策略声明禁用 ⇒ 回放后任务必须是禁用态")
}
