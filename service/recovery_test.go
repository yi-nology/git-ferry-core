package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// newRecoveryTestService 带恢复点存储的轻量服务。
func newRecoveryTestService(t *testing.T, backupDir string) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.RecoveryPoint{}))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return &Service{
		recovery: dao.NewRecoveryPointDAO(db),
		config:   &model.Config{Sync: model.SyncConfig{BackupDir: backupDir}},
		bgCtx:    context.Background(),
	}
}

// TestRecordAndListRecoveryPoints 落库 + 按任务最新优先列出。
func TestRecordAndListRecoveryPoints(t *testing.T) {
	backupDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "a.bundle"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "b.bundle"), []byte("y"), 0o600))
	svc := newRecoveryTestService(t, backupDir)

	require.NoError(t, svc.RecordRecoveryPoint("t1", "a.bundle", "bundle", "sha-a", 1))
	require.NoError(t, svc.RecordRecoveryPoint("t1", "b.bundle", "bundle", "sha-b", 2))
	require.NoError(t, svc.RecordRecoveryPoint("t2", "a.bundle", "bundle", "sha-c", 1))

	rps, err := svc.ListRecoveryPoints("t1", 10)
	require.NoError(t, err)
	require.Len(t, rps, 2)
	assert.Equal(t, "sha-b", rps[0].SourceSHA, "最新优先")

	latest, err := svc.LatestRecoveryPoint("t1")
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, "b.bundle", latest.FileName)
}

// TestRecoveryPoint_LazyPrune 文件被轮转删除 ⇒ 惰性清理,
// 索引不留假锚(对应评审「可恢复性独立验证」)。
func TestRecoveryPoint_LazyPrune(t *testing.T) {
	backupDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "gone.bundle"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "kept.bundle"), []byte("y"), 0o600))
	svc := newRecoveryTestService(t, backupDir)

	require.NoError(t, svc.RecordRecoveryPoint("t1", "gone.bundle", "bundle", "sha-g", 1))
	require.NoError(t, svc.RecordRecoveryPoint("t1", "kept.bundle", "bundle", "sha-k", 2))

	// 删掉文件模拟轮转
	require.NoError(t, os.Remove(filepath.Join(backupDir, "gone.bundle")))

	rps, err := svc.ListRecoveryPoints("t1", 10)
	require.NoError(t, err)
	require.Len(t, rps, 1, "文件没了的条目必须被清理")
	assert.Equal(t, "kept.bundle", rps[0].FileName)

	// Latest 也走同样语义:重建文件→记录(成为最新)→再删文件→Latest 应回退到 kept
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "gone.bundle"), []byte("z"), 0o600))
	require.NoError(t, svc.RecordRecoveryPoint("t1", "gone.bundle", "bundle", "sha-g2", 1))
	require.NoError(t, os.Remove(filepath.Join(backupDir, "gone.bundle")))
	latest, err := svc.LatestRecoveryPoint("t1")
	require.NoError(t, err)
	// gone 是后来记的(最新),文件没了 ⇒ 清理后返回下一条或 nil
	if latest != nil {
		assert.Equal(t, "kept.bundle", latest.FileName)
	}
}
