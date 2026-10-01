package executor

import (
	"context"
	"os"
	"path/filepath"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 流水线阶段实现(每阶段单一职责) =====

// cloneFetchStage 首次 clone 或增量 fetch(含 LFS)。
type cloneFetchStage struct{}

func (cloneFetchStage) Name() string     { return stageCloneFetch }
func (cloneFetchStage) IsOptional() bool { return false }

func (cloneFetchStage) Run(ctx context.Context, rc *RunContext) error {
	repoDir := rc.RepoDir
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); os.IsNotExist(err) {
		rc.logf("initial clone of source repo...\n")
		if err := rc.Exec.cloneRepo(ctx, repoDir, rc.SourceRepo, rc.Task, rc.Platforms[rc.SourceRepo.PlatformID]); err != nil {
			return errors.Wrap(err, "clone failed")
		}
	} else {
		rc.logf("fetch updates from source repo...\n")
		if err := rc.Exec.fetchRepo(ctx, repoDir, rc.Task, rc.SourceRepo, rc.Platforms[rc.SourceRepo.PlatformID]); err != nil {
			return errors.Wrap(err, "fetch failed")
		}
	}
	if rc.Task.GitLFS {
		if err := rc.Exec.syncLFS(ctx, repoDir, RemoteOrigin, true); err != nil {
			return errors.Wrap(err, "lfs fetch failed")
		}
		rc.logf("  LFS objects fetched\n")
	}
	return nil
}

// ensureRemoteStage 保证目标 remote 存在。
type ensureRemoteStage struct{}

func (ensureRemoteStage) Name() string     { return stageEnsureRemote }
func (ensureRemoteStage) IsOptional() bool { return false }

func (ensureRemoteStage) Run(ctx context.Context, rc *RunContext) error {
	rc.logf("ensure target remote exists...\n")
	return rc.Exec.ensureRemote(ctx, rc.RepoDir, rc.TargetRepo)
}

// pushStage 推送到目标(带重试退避)。
type pushStage struct{}

func (pushStage) Name() string     { return stagePush }
func (pushStage) IsOptional() bool { return false }

func (pushStage) Run(ctx context.Context, rc *RunContext) error {
	cfg := rc.Exec.service.GetConfig()
	maxRetries := 3
	if cfg != nil && cfg.Sync.RetryCount > 0 {
		maxRetries = cfg.Sync.RetryCount
	}
	var pushErr error
	var retries int
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			retries = attempt - 1
			rc.logf("retry attempt %d/%d...\n", attempt, maxRetries)
			backoff := time.Duration(attempt*model.RetryBackoffMs) * time.Millisecond
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Wrap(ctx.Err(), "retry aborted")
			case <-timer.C:
			}
		}
		pushErr = rc.Exec.push(ctx, rc.RepoDir, rc.Task, rc.TargetRepo, rc.Platforms[rc.TargetRepo.PlatformID])
		if pushErr == nil {
			break
		}
		if attempt < maxRetries {
			rc.logf("push failed, retrying fetch...\n")
			if err := rc.Exec.fetchRepo(ctx, rc.RepoDir, rc.Task, rc.SourceRepo, rc.Platforms[rc.SourceRepo.PlatformID]); err != nil {
				rc.logf("retry fetch failed: %v\n", err)
			}
		}
	}
	rc.Run.RetryTotal = retries
	if pushErr != nil {
		return errors.Wrapf(pushErr, "push failed after %d attempts", maxRetries)
	}
	return nil
}

// wikiStage 同步 wiki(可选,失败不阻断)。
type wikiStage struct{}

func (wikiStage) Name() string     { return stageWiki }
func (wikiStage) IsOptional() bool { return true }

func (wikiStage) Run(ctx context.Context, rc *RunContext) error {
	if !rc.Task.SyncWiki {
		rc.logf("wiki: skipped (not enabled)\n")
		return nil
	}
	rc.logf("sync wiki...\n")
	return rc.Exec.syncWiki(ctx, rc.WorkDir, rc.Task, rc.SourceRepo, rc.TargetRepo,
		rc.Platforms[rc.SourceRepo.PlatformID], rc.Platforms[rc.TargetRepo.PlatformID], rc.Details)
}

// bundleStage 冷备 bundle + 多目的地扇出(可选)。
type bundleStage struct{}

func (bundleStage) Name() string     { return stageBundle }
func (bundleStage) IsOptional() bool { return true }

func (bundleStage) Run(ctx context.Context, rc *RunContext) error {
	if !rc.Task.GitBundle {
		rc.logf("bundle: skipped (not enabled)\n")
		return nil
	}
	cfg := rc.Exec.service.GetConfig()
	if cfg == nil || cfg.Sync.BackupDir == "" {
		rc.logf("bundle: skipped (sync.backup_dir not set)\n")
		return nil
	}
	rc.logf("create cold backup bundle...\n")
	path, err := rc.Exec.writeBundle(ctx, rc.WorkDir, cfg.Sync.BackupDir, rc.Task, rc.Details)
	if err != nil {
		return err
	}
	rc.Exec.fanoutBundle(ctx, path, cfg, rc.Details)
	rc.BundlesPath = path
	return nil
}

// backupRemoteStage 同步成功后向配置的备份远端 push 镜像副本（GitHub/GitLab 等）。
// 失败只记 details，不影响主同步结果（冷备是附加能力）。
type backupRemoteStage struct{}

func (backupRemoteStage) Name() string     { return stageBackupRemote }
func (backupRemoteStage) IsOptional() bool { return true }

func (backupRemoteStage) Run(ctx context.Context, rc *RunContext) error {
	cfg := rc.Exec.service.GetConfig()
	if cfg == nil || len(cfg.Sync.BackupRemotes) == 0 {
		return nil
	}
	rc.logf("push backup remotes...\n")
	rc.Exec.pushBackupRemotes(ctx, rc.RepoDir, rc.Task, rc.SourceRepo, cfg.Sync.BackupRemotes, rc.Details)
	return nil
}

// defaultPipeline 标准同步流水线:clone/fetch → remote → push → wiki → bundle → backup remotes。
// 顺序固定、阶段可单测;新增能力只需追加 Stage,不动 Execute。
func defaultPipeline() *Pipeline {
	return NewPipeline(
		cloneFetchStage{},
		ensureRemoteStage{},
		pushStage{},
		backupRemoteStage{},
		wikiStage{},
		bundleStage{},
	)
}
