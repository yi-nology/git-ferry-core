package service

import (
	errors "github.com/cockroachdb/errors"

	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oekit/pkg/pathx"
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
	// 文件名安全校验(纵深防御):core 是给多个壳用的库,
	// 不能假设每个壳都校验过;名字里带 ../ 就能把路径拼到备份目录之外。
	if !pathx.IsSafeFileName(name) {
		return nil, errInvalidName
	}
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
	if !pathx.IsSafeFileName(name) {
		return errInvalidName
	}
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
		return 0, ErrBackupDisabled
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	all, err := executor.ListBundles(dir, "")
	if err != nil {
		return 0, err
	}
	// 删除失败**如实报错**,不计入 removed。
	//
	// 原来 `_ = os.Remove(...)` 静默丢弃错误 ⇒ 日志打出
	// "expired backups cleaned: removed=N",运维以为保留策略在生效,
	// 实际旧产物原地不动(磁盘持续涨),而且没有任何信号提示需要人工介入。
	var failures []string
	for _, b := range all {
		if b.ModTime.Before(cutoff) {
			if rerr := os.Remove(b.Path); rerr != nil {
				failures = append(failures, b.Name+": "+rerr.Error())
				continue
			}
			removed++
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
				if rerr := os.RemoveAll(path); rerr != nil {
					failures = append(failures, base+": "+rerr.Error())
					return filepath.SkipDir
				}
				removed++
				return filepath.SkipDir
			}
		}
		return nil
	})
	// 同步清理过期的**回灌历史**(metadata-restore/<repo>/*.json)。
	//
	// 原来只清 metadata/ ⇒ 回灌历史目录无限增长(每次回灌一个文件,永不清理)。
	// 它和元数据快照是同一类东西(灾备材料),按同一保留期清即可
	// (legal_hold 时整个清理都不执行 ✔)。
	restoreRoot := filepath.Join(dir, "metadata-restore")
	_ = filepath.WalkDir(restoreRoot, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d == nil || d.IsDir() {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil || !fi.ModTime().Before(cutoff) {
			return nil
		}
		if rerr := os.Remove(path); rerr != nil {
			failures = append(failures, path+": "+rerr.Error())
			return nil
		}
		removed++
		return nil
	})
	if len(failures) > 0 {
		return removed, errors.Errorf("清理未完成:%d 项删除失败(%s)",
			len(failures), strings.Join(failures, "; "))
	}
	return removed, nil
}

// LegalHold 当前是否处于合规冻结。
func (s *Service) LegalHold() bool {
	return s.config.Sync.LegalHold
}
