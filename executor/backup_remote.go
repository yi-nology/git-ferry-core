package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// pushBackupRemotes 把当前同步结果推到配置的备份远端（GitHub/GitLab 等）。
// 与镜像中心区分：不改写 module 身份，纯备份副本。失败只记 details。
func (e *Executor) pushBackupRemotes(ctx context.Context, dir string, task *model.SyncTask,
	repo *model.Repo, remotes []model.BackupRemoteConfig, details *strings.Builder) {
	for _, r := range remotes {
		if r.Enabled != nil && !*r.Enabled {
			continue
		}
		name := r.Name
		if name == "" {
			name = "backup"
		}
		if r.URL == "" {
			fmt.Fprintf(details, "  backup remote %s: skipped (no url)\n", name)
			continue
		}
		url := expandBackupURL(r.URL, task, repo)
		remoteName := "backup-" + name
		// 更新/添加 remote(与原 `git remote remove/add` 一致:remove 失败忽略)
		_ = e.backend.RemoveRemote(ctx, dir, remoteName)
		if err := e.backend.AddRemote(ctx, dir, remoteName, url); err != nil {
			fmt.Fprintf(details, "  backup remote %s: add failed: %v\n", name, err)
			continue
		}
		// 推送当前分支 + tags
		refSpecs := []string{
			fmt.Sprintf("refs/heads/%s:refs/heads/%s", task.SourceBranch, task.TargetBranch),
		}
		if task.GitTags {
			// 原实现是 `git push --tags`(全量推 tag、不强制覆盖);
			// Push 无 --tags 选项,用等价 refspec,行为一致。
			refSpecs = append(refSpecs, "refs/tags/*:refs/tags/*")
		}
		// 走 backend 推送才能带上仓库凭证(原裸 git 只能靠本机 credential helper)。
		if _, err := e.backend.Push(ctx, gitbackend.PushOptions{
			RepoPath: dir,
			Remote:   remoteName,
			RefSpecs: refSpecs,
			Force:    r.Force && task.GitForce,
			Auth:     e.authConfig(ctx, repo, nil),
		}); err != nil {
			fmt.Fprintf(details, "  backup remote %s: push failed: %v\n", name, err)
			continue
		}
		fmt.Fprintf(details, "  backup remote %s: ok (%s)\n", name, url)
	}
}

// expandBackupURL 替换 {owner}/{repo}/{key} 占位。
func expandBackupURL(raw string, task *model.SyncTask, repo *model.Repo) string {
	out := raw
	if repo != nil {
		out = strings.ReplaceAll(out, "{owner}", repo.PlatformOwner)
		out = strings.ReplaceAll(out, "{repo}", repo.PlatformRepo)
		out = strings.ReplaceAll(out, "{key}", repo.Key)
	}
	if task != nil {
		out = strings.ReplaceAll(out, "{task}", task.Key)
	}
	return out
}
