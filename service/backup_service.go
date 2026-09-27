package service

import (
	"context"

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
func (s *Service) VerifyBundle(ctx context.Context, name string) (*BundleInfo, error) {
	dir := s.config.Sync.BackupDir
	path := dir + "/" + name
	return executor.VerifyBundle(ctx, path)
}

// RestoreBundle 从 bundle 恢复到 destDir。
func (s *Service) RestoreBundle(ctx context.Context, name, destDir string) error {
	dir := s.config.Sync.BackupDir
	return executor.RestoreBundle(ctx, dir+"/"+name, destDir)
}

// BackupDir 返回配置的冷备目录。
func (s *Service) BackupDir() string {
	return s.config.Sync.BackupDir
}
