package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// newBackupService 构造只依赖 config 的 Service（冷备逻辑不需要 DB/executor）。
func newBackupService(dir string, retentionDays int, legalHold bool) *Service {
	return &Service{config: &model.Config{Sync: model.SyncConfig{
		BackupDir:           dir,
		BackupRetentionDays: retentionDays,
		LegalHold:           legalHold,
	}}}
}

func TestBackupDirAndLegalHold(t *testing.T) {
	s := newBackupService("/tmp/backup", 3, true)
	assert.Equal(t, "/tmp/backup", s.BackupDir())
	assert.True(t, s.LegalHold())
	assert.False(t, newBackupService("/tmp/backup", 0, false).LegalHold())
}

func TestListBundles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "taskA-20260101.bundle"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "taskB-20260101.bundle"), []byte("bb"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "metadata"), 0o750))

	s := newBackupService(dir, 0, false)

	// 按 taskKey 前缀过滤
	list, err := s.ListBundles("taskA")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "taskA-20260101.bundle", list[0].Name)
	assert.Equal(t, "taskA", list[0].TaskKey)

	// 全量：只收 .bundle，时间倒序（新在前）
	older := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "taskA-20260101.bundle"), older, older))
	all, err := s.ListBundles("")
	require.NoError(t, err)
	require.Len(t, all, 2, "notes.txt 与目录不应计入")
	assert.Equal(t, "taskB-20260101.bundle", all[0].Name)

	// 目录不存在 → 空列表而非报错
	missing := newBackupService(filepath.Join(dir, "nope"), 0, false)
	got, err := missing.ListBundles("")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestCleanupExpiredBackups(t *testing.T) {
	dir := t.TempDir()
	s := newBackupService(dir, 7, false)

	oldBundle := filepath.Join(dir, "taskA-old.bundle")
	newBundle := filepath.Join(dir, "taskA-new.bundle")
	require.NoError(t, os.WriteFile(oldBundle, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(newBundle, []byte("new"), 0o600))
	stale := time.Now().AddDate(0, 0, -10)
	require.NoError(t, os.Chtimes(oldBundle, stale, stale))

	// 过期元数据快照目录（形如 20060102-150405，15 字符 1 个横杠）
	oldMeta := filepath.Join(dir, "metadata", "repo1", "20260101-000000")
	newMeta := filepath.Join(dir, "metadata", "repo1", "20991231-000000")
	require.NoError(t, os.MkdirAll(oldMeta, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(oldMeta, "manifest.json"), []byte("{}"), 0o600))
	require.NoError(t, os.MkdirAll(newMeta, 0o750))

	removed, err := s.CleanupExpiredBackups()
	require.NoError(t, err)
	assert.Equal(t, 2, removed, "过期 bundle 与过期元数据目录各计 1")
	assert.NoFileExists(t, oldBundle)
	assert.FileExists(t, newBundle)
	assert.NoDirExists(t, oldMeta)
	assert.DirExists(t, newMeta)
}

func TestCleanupExpiredBackups_Guards(t *testing.T) {
	dir := t.TempDir()

	// 合规冻结拒绝执行
	hold := newBackupService(dir, 7, true)
	removed, err := hold.CleanupExpiredBackups()
	require.ErrorIs(t, err, errLegalHold)
	assert.Equal(t, 0, removed)

	// retention 未配置 → 不清理
	none := newBackupService(dir, 0, false)
	removed, err = none.CleanupExpiredBackups()
	require.NoError(t, err)
	assert.Equal(t, 0, removed)

	// backup_dir 未配置 → 明确报错（区别于"没到期"）
	nodir := newBackupService("", 7, false)
	removed, err = nodir.CleanupExpiredBackups()
	require.ErrorIs(t, err, errBackupDisabled)
	assert.Equal(t, 0, removed)
}
