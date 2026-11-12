package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// TestMarkBackup_StickyFailed failed 粘滞语义:
// 任一备份环节失败 ⇒ 整体 failed;后续成功不得翻回;未执行保持空(n/a)。
func TestMarkBackup_StickyFailed(t *testing.T) {
	rc := &RunContext{Run: &model.SyncRun{}}
	// 未执行 ⇒ 空 = n/a
	assert.Empty(t, rc.Run.BackupStatus)

	rc.markBackup(true) // bundle 成功
	assert.Equal(t, model.BackupStatusSuccess, rc.Run.BackupStatus)

	rc.markBackup(false) // backup_remote 失败 ⇒ 翻 failed
	assert.Equal(t, model.BackupStatusFailed, rc.Run.BackupStatus)

	rc.markBackup(true) // 之后的成功不得把 failed 洗掉
	assert.Equal(t, model.BackupStatusFailed, rc.Run.BackupStatus,
		"failed 必须粘滞:任一备份环节失败 ⇒ 灾备目标未达成")
}

// TestPushBackupRemotes_AggregatesFailures 备份远端失败不再只沉在 details:
// 聚合错误返回,供阶段写 Run.BackupStatus。
func TestPushBackupRemotes_AggregatesFailures(t *testing.T) {
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{Type: "gogit"})
	require.NoError(t, err)
	e := &Executor{backend: backend}

	task := &model.SyncTask{Key: "bk-aggr", SourceBranch: "main", TargetBranch: "main"}
	var details strings.Builder
	err = e.pushBackupRemotes(context.Background(),
		filepath.Join(t.TempDir(), "nonexistent-repo"), // 目录不存在 ⇒ add 必失败
		task, &model.Repo{Key: "r"},
		[]model.BackupRemoteConfig{{Name: "b1", URL: "https://example.com/a.git"}},
		&details)
	require.Error(t, err, "备份远端失败必须聚合返回(不再只写 details)")
	assert.Contains(t, details.String(), "add failed")
}

// TestFanoutBundle_AggregatesFailures 多目的地部分失败 ⇒ 聚合返回。
func TestFanoutBundle_AggregatesFailures(t *testing.T) {
	// 起服务再立刻关掉 ⇒ 连接必被拒
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "a.bundle")
	require.NoError(t, os.WriteFile(bundlePath, []byte("bundle-data"), 0o600))

	e := &Executor{}
	cfg := &model.Config{Sync: model.SyncConfig{BackupDestinations: []model.BackupDestinationConfig{{
		Type: "s3", Name: "bad", Endpoint: closedURL, Bucket: "b",
		AccessKey: "A", SecretKey: "S", PathStyle: true, Region: "us-east-1",
	}}}}
	var details strings.Builder
	err := e.fanoutBundle(context.Background(), bundlePath, cfg, &details)
	require.Error(t, err, "扇出目的地失败必须聚合返回")
	assert.Contains(t, details.String(), "FAILED")
}

// stubRunService 只实现 executor.Service 中本用例需要的方法。
type stubRunService struct {
	cfg *model.Config
}

func (stubRunService) GetTempDir(string) string   { return "" }
func (s stubRunService) GetConfig() *model.Config { return s.cfg }
func (stubRunService) CreateRun(*model.SyncTask, string, *uint) (*model.SyncRun, error) {
	return &model.SyncRun{}, nil
}
func (stubRunService) CreateRunStep(*model.SyncRunStep) error { return nil }
func (stubRunService) UpdateRunStep(*model.SyncRunStep) error { return nil }
func (stubRunService) CompleteRun(*model.SyncRun) error       { return nil }
func (stubRunService) UpdateTaskLastRun(*model.SyncTask, *model.SyncRun) error {
	return nil
}
func (stubRunService) CompleteRunWithTaskUpdate(*model.SyncRun, *model.SyncTask) error {
	return nil
}
func (stubRunService) GetRepoByKey(string) (*model.Repo, error) { return nil, nil }
func (stubRunService) GetPlatformByID(context.Context, uint) (*model.Platform, error) {
	return nil, nil
}

// TestBackupRemoteStage_SetsBackupStatus 备份远端失败:
// run.Status(同步)不受可选阶段影响,但 BackupStatus 必须红 —— 两种状态分别显示。
// 对应评审验收:「备份上传 S3 失败,但 Git 同步成功 ⇒ 两种状态分别显示」。
func TestBackupRemoteStage_SetsBackupStatus(t *testing.T) {
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{Type: "gogit"})
	require.NoError(t, err)

	svc := stubRunService{cfg: &model.Config{Sync: model.SyncConfig{
		BackupRemotes: []model.BackupRemoteConfig{{Name: "b1", URL: "https://example.com/a.git"}},
	}}}
	e := &Executor{service: svc, backend: backend}

	rc := &RunContext{
		Task:    &model.SyncTask{Key: "bk-status", SourceBranch: "main", TargetBranch: "main"},
		RepoDir: filepath.Join(t.TempDir(), "nonexistent"),
		Run:     &model.SyncRun{TaskKey: "bk-status", Status: model.StatusSuccess},
		Details: &strings.Builder{},
		Exec:    e,
	}

	serr := backupRemoteStage{}.Run(context.Background(), rc)
	require.Error(t, serr, "可选阶段失败要返回(流水线记日志继续)")
	assert.Equal(t, model.StatusSuccess, rc.Run.Status, "同步状态不受可选阶段影响")
	assert.Equal(t, model.BackupStatusFailed, rc.Run.BackupStatus,
		"备份失败必须单独红:同步成功 + 备份失败 ⇒ 灾备目标未达成")
}

// TestBackupRemoteStage_SkippedNoConfig 未配置备份远端 ⇒ n/a,不写字段。
func TestBackupRemoteStage_SkippedNoConfig(t *testing.T) {
	e := &Executor{service: stubRunService{cfg: &model.Config{}}}
	rc := &RunContext{
		Task:    &model.SyncTask{Key: "bk-none"},
		Run:     &model.SyncRun{Status: model.StatusSuccess},
		Details: &strings.Builder{},
		Exec:    e,
	}
	require.NoError(t, backupRemoteStage{}.Run(context.Background(), rc))
	assert.Empty(t, rc.Run.BackupStatus, "未配置备份 ⇒ n/a(空),不得误标 success")
}
