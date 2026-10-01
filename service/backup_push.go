package service

import (
	"context"
	"strings"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// PushTaskBackup 把任务 mirror 工作区(workDir)推到任意备份远端(github/gitlab 任意
// git remote)。与 executor/backup_remote.go 同套路:临时 remote + backend.Push,
// 推送带仓库凭证——裸 exec git push 无凭证,https 私有仓必败。
//
//   - refspec 原样传入(壳层默认 "+refs/heads/*:refs/heads/*");空值补默认。
//   - force 决定 PushOptions.Force(--force)。
//   - 凭证按 task.SourceRepoKey 查仓库走 executor.BuildRepoAuth(repo token 优先、
//     平台 token 回退);取不到仓库时退 AuthNone(匿名)。
//
// 返回成功的 git 输出摘要(推上的 ref 列表,可为空串);失败时错误自带 stderr
// (gitbackend GitError)。临时 remote 推送后清理。
func (s *Service) PushTaskBackup(ctx context.Context, task *model.SyncTask, workDir, remoteURL, refspec string, force bool) (string, error) {
	if task == nil {
		return "", errors.New("task is nil")
	}
	if strings.TrimSpace(workDir) == "" {
		return "", errors.New("workDir is required")
	}
	if strings.TrimSpace(remoteURL) == "" {
		return "", errors.New("remote URL is required")
	}
	if refspec == "" {
		refspec = "+refs/heads/*:refs/heads/*"
	}

	backendType := ""
	if s.config != nil {
		backendType = s.config.Git.Backend
	}
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{Type: backendType})
	if err != nil {
		return "", errors.Wrap(err, "init git backend failed")
	}

	// 远端以临时名注册(与 executor/backup_remote.go 一致:remove 失败忽略),
	// Push 只认已配置的 remote 名,URL 直传不稳。
	const remoteName = "backup-manual"
	_ = backend.RemoveRemote(ctx, workDir, remoteName)
	if err := backend.AddRemote(ctx, workDir, remoteName, remoteURL); err != nil {
		return "", errors.Wrap(err, "add backup remote failed")
	}
	defer func() { _ = backend.RemoveRemote(ctx, workDir, remoteName) }()

	res, err := backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: workDir,
		Remote:   remoteName,
		RefSpecs: []string{refspec},
		Force:    force,
		Auth:     s.backupPushAuth(ctx, task),
	})
	if err != nil {
		return "", err
	}
	if res == nil {
		return "", nil
	}
	// native 后端回填逐 ref 结果;gogit 回填请求的 refspec——都足以做摘要。
	return strings.Join(res.PushedRefs, "\n"), nil
}

// backupPushAuth 按任务源仓库构建推送凭证;查不到仓库退 AuthNone。
func (s *Service) backupPushAuth(ctx context.Context, task *model.SyncTask) gitbackend.AuthConfig {
	repo, err := s.GetRepo(ctx, task.SourceRepoKey)
	if err != nil || repo == nil {
		return executor.BuildRepoAuth(nil, nil)
	}
	var p *model.Platform
	if repo.PlatformID > 0 {
		if got, gerr := s.GetPlatformByID(ctx, repo.PlatformID); gerr == nil {
			p = got
		}
	}
	return executor.BuildRepoAuth(repo, p)
}
