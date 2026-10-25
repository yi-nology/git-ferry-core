package service

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestRunTaskAsync_SameTaskKeyMutualExclusive 手动触发也必须受**同 taskKey 互斥**约束。
//
// 回归背景(P1):cron / webhook 走 RunTaskWithTrigger ⇒ 有互斥;
// 而手动触发 / 重试 / 重建 / 自动补偿全走 RunTaskAsync ⇒ 原先**完全没有互斥**
// (函数体里根本没有 guard 调用,注释里写的"并发快速失败"从未发生)。
// workdir 是按 taskKey 共享的 ⇒ 同一个任务被并发跑多份时,两个 executor
// 同时 fetch/update-ref/push 同一目录 ⇒ ref 锁竞争、推送时序失控。
//
// 这里不依赖"任务跑得够久":先把同 taskKey 的执行权占住,再触发 ⇒
// 必须**立刻**被拒(而不是排队跑第二份)。
func TestRunTaskAsync_SameTaskKeyMutualExclusive(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SyncTask{}, &model.SyncRun{}, &model.SyncRunStep{}, &model.Repo{}))

	taskDAO := dao.NewSyncTaskDAO(db)
	runDAO := dao.NewSyncRunDAO(db)
	runStepDAO := dao.NewSyncRunStepDAO(db)
	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)

	svc := &Service{
		tasks:  NewTaskService(taskDAO, runDAO, runStepDAO, repoDAO),
		guard:  newLocalGuard(2),
		bgCtx:  context.Background(),
		config: &model.Config{},
	}

	const key = "t-mutex"
	require.NoError(t, taskDAO.Create(&model.SyncTask{
		Key: key, Name: "mutex", Enabled: true, SourceBranch: "main", TargetBranch: "main",
	}))

	// 先占住同 taskKey 的执行权(模拟"已经有一个在跑")
	release, err := svc.guard.Acquire(context.Background(), key)
	require.NoError(t, err)
	defer release()

	// 手动触发必须被拒
	err = svc.RunTaskAsync(key, "manual", nil)
	require.Error(t, err, "同 taskKey 已在跑时手动触发必须被拒")
	require.ErrorIs(t, err, ErrTaskRunning)
}
