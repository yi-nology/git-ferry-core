package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
)

// startLifecycleJobs 启动生命周期后台任务:
//   - 冷备 retention 清理(legal_hold 时自动跳过)
//   - 平台自动发现(sync.auto_discover_interval_minutes > 0 时)
//
// 与 cron 任务解耦:单次失败只记日志,不中断循环。
func (s *Service) startLifecycleJobs() {
	interval := s.config.Sync.AutoDiscoverIntervalMinutes
	retention := s.config.Sync.BackupRetentionDays
	if interval <= 0 && retention <= 0 {
		return
	}
	// 清理周期:与发现同周期,至少 30 分钟
	tick := 30 * time.Minute
	if interval > 0 && time.Duration(interval)*time.Minute < tick {
		tick = time.Duration(interval) * time.Minute
	}

	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()
		// 用 oekit concurrency/loop:panic 隔离(单次失败只记日志,不中断循环)
		// 与 ctx 取消退出(随 bgCancel 收敛)。FirstDelay 精确复刻「启动后先等一个
		// 周期,避免与初始化竞争」的原语义。
		loop.Run(s.bgCtx, loop.Config{
			Interval:   tick,
			FirstDelay: tick,
			Name:       "lifecycle-jobs",
		}, func(context.Context) {
			s.runLifecycleOnce()
		}).Wait()
	})
}

// runLifecycleOnce 执行一轮生命周期任务(清理 + 自动发现)。
func (s *Service) runLifecycleOnce() {
	ctx, cancel := context.WithTimeout(s.bgCtx, 5*time.Minute)
	defer cancel()

	// 1) 冷备过期清理
	if s.config.Sync.BackupRetentionDays > 0 && !s.config.Sync.LegalHold {
		if removed, err := s.CleanupExpiredBackups(); err != nil {
			slog.Warn("lifecycle: backup cleanup failed", "error", err)
		} else if removed > 0 {
			slog.Info("lifecycle: expired backups cleaned", "removed", removed)
		}
	}

	// 2) 平台自动发现
	if s.config.Sync.AutoDiscoverIntervalMinutes <= 0 {
		return
	}
	platforms, err := s.ListPlatforms(ctx)
	if err != nil {
		slog.Warn("lifecycle: list platforms failed", "error", err)
		return
	}
	for _, p := range platforms {
		if p == nil || p.Key == "" {
			continue
		}
		opts := AutoDiscoverOptions{
			ImportNew: s.config.Sync.AutoDiscoverImport,
			Filter: &RepoImportFilter{
				ExcludeArchived: true,
				ExcludeForks:    true,
			},
		}
		rep, derr := s.AutoDiscover(ctx, p.Key, opts)
		if derr != nil {
			slog.Warn("lifecycle: auto discover failed", "platform", p.Key, "error", derr)
			continue
		}
		if len(rep.NewRepos) > 0 {
			slog.Info("lifecycle: discovered new repos",
				"platform", p.Key, "new", len(rep.NewRepos), "imported", rep.Imported)
		}
	}
}
