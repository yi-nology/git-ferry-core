package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yi-nology/git-ferry-core/executor"
)

// BundleInfo bundle 文件信息(executor 同构,便于壳层直接用)。
type BundleInfo = executor.BundleInfo

// ListBundles 列出冷备 bundle。
func (s *Service) ListBundles(taskKey string) ([]BundleInfo, error) {
	dir := s.config.Sync.BackupDir
	return executor.ListBundles(dir, taskKey)
}

// VerifyBundle 校验 bundle 完整性。
// 自动识别 .enc 加密冷备:先解密到临时目录再校验。
func (s *Service) VerifyBundle(ctx context.Context, name string) (*BundleInfo, error) {
	dir := s.config.Sync.BackupDir
	path := filepath.Join(dir, name)
	if executor.IsEncryptedFile(name) {
		tmp, err := executor.DecryptToTemp(path, s.config.Sync.BackupEncryptKey)
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(filepath.Dir(tmp)) }()
		return executor.VerifyBundle(ctx, tmp)
	}
	return executor.VerifyBundle(ctx, path)
}

// RestoreBundle 从 bundle 恢复到 destDir。
// 支持 .bundle 与 .bundle.enc(需配置 backup_encrypt_key)。
func (s *Service) RestoreBundle(ctx context.Context, name, destDir string) error {
	dir := s.config.Sync.BackupDir
	path := filepath.Join(dir, name)
	if executor.IsEncryptedFile(name) {
		tmp, err := executor.DecryptToTemp(path, s.config.Sync.BackupEncryptKey)
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(filepath.Dir(tmp)) }()
		return executor.RestoreBundle(ctx, tmp, destDir)
	}
	return executor.RestoreBundle(ctx, path, destDir)
}

// BackupDir 返回配置的冷备目录。
func (s *Service) BackupDir() string {
	return s.config.Sync.BackupDir
}

// CleanupExpiredBackups 按 retention 天数清理过期冷备(legal_hold 时拒绝执行)。
// 返回删除数量。与 BackupKeep 轮转互补:keep 管数量,retention 管时效。
func (s *Service) CleanupExpiredBackups() (removed int, err error) {
	if s.config.Sync.LegalHold {
		return 0, errLegalHold
	}
	days := s.config.Sync.BackupRetentionDays
	if days <= 0 {
		return 0, nil
	}
	dir := s.config.Sync.BackupDir
	if dir == "" {
		return 0, errBackupDisabled
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	all, err := executor.ListBundles(dir, "")
	if err != nil {
		return 0, err
	}
	for _, b := range all {
		if b.ModTime.Before(cutoff) {
			if os.Remove(b.Path) == nil {
				removed++
			}
		}
	}
	// 同步清理过期元数据快照
	metaRoot := filepath.Join(dir, "metadata")
	_ = filepath.WalkDir(metaRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d == nil || !d.IsDir() {
			return nil
		}
		// 形如 metadata/<repo>/<ts>/
		base := d.Name()
		if len(base) == 15 && strings.Count(base, "-") == 1 { // 20060102-150405
			if t, perr := time.Parse("20060102-150405", base); perr == nil && t.Before(cutoff) {
				_ = os.RemoveAll(path)
				removed++
				return filepath.SkipDir
			}
		}
		return nil
	})
	return removed, nil
}

// LegalHold 当前是否处于合规冻结。
func (s *Service) LegalHold() bool {
	return s.config.Sync.LegalHold
}
