package service

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 恢复点索引(P3 可恢复性,见 model.RecoveryPoint) =====

// RecordRecoveryPoint 实现 executor.RecoveryRecorder:冷备成功后落索引。
// executor 侧已保证「观测面失败不毁冷备」,这里只负责落库。
func (s *Service) RecordRecoveryPoint(taskKey, fileName, format, sourceSHA string, sizeBytes int64) error {
	if s.recovery == nil {
		return nil // 未接线:静默跳过(裸构造测试路径)
	}
	return s.recovery.Create(&model.RecoveryPoint{
		TaskKey:   taskKey,
		FileName:  fileName,
		Format:    format,
		SourceSHA: sourceSHA,
		SizeBytes: sizeBytes,
	})
}

// ListRecoveryPoints 按任务列恢复点(最新优先),并惰性清理文件已不存在的
// 条目(轮转删了 bundle ⇒ 索引跟着走,不留假锚)。limit<=0 用默认 50。
func (s *Service) ListRecoveryPoints(taskKey string, limit int) ([]model.RecoveryPoint, error) {
	if s.recovery == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rps, err := s.recovery.ListByTask(taskKey, limit)
	if err != nil {
		return nil, err
	}
	// 惰性清理:文件不在了的条目删除(保留仍有效的)。
	backupDir := ""
	if s.config != nil {
		backupDir = s.config.Sync.BackupDir
	}
	var stale []uint
	alive := make([]model.RecoveryPoint, 0, len(rps))
	for _, rp := range rps {
		if backupDir == "" {
			// backup_dir 未配置 ⇒ 无法验证存在性,原样返回(不误删)。
			alive = append(alive, rp)
			continue
		}
		if _, err := os.Stat(filepath.Join(backupDir, rp.FileName)); err != nil {
			stale = append(stale, rp.ID)
			continue
		}
		alive = append(alive, rp)
	}
	if len(stale) > 0 {
		if _, err := s.recovery.DeleteByIDs(stale); err != nil {
			slog.Warn("recovery: 惰性清理失败", "stale", len(stale), "error", err)
		}
	}
	return alive, nil
}

// LatestRecoveryPoint 某任务最新恢复点(带同样的惰性清理)。
func (s *Service) LatestRecoveryPoint(taskKey string) (*model.RecoveryPoint, error) {
	if s.recovery == nil {
		return nil, nil
	}
	rp, err := s.recovery.GetLatest(taskKey)
	if err != nil || rp == nil {
		return nil, err
	}
	if s.config != nil && s.config.Sync.BackupDir != "" {
		if _, serr := os.Stat(filepath.Join(s.config.Sync.BackupDir, rp.FileName)); serr != nil {
			// 文件已轮转删除:清理并返回 nil(没有可用恢复点)。
			if _, derr := s.recovery.DeleteByFileName(rp.FileName); derr != nil {
				return nil, derr
			}
			return nil, nil
		}
	}
	return rp, nil
}
