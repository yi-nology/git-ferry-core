package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/yi-nology/git-ferry-core/model"
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
		// 更新/添加 remote
		_, _ = e.gitOutput(ctx, dir, "remote", "remove", remoteName)
		args := []string{"remote", "add", remoteName, url}
		if _, err := e.gitOutput(ctx, dir, args...); err != nil {
			fmt.Fprintf(details, "  backup remote %s: add failed: %v\n", name, err)
			continue
		}
		// 推送当前分支 + tags
		refSpecs := []string{
			fmt.Sprintf("refs/heads/%s:refs/heads/%s", task.SourceBranch, task.TargetBranch),
		}
		pushArgs := []string{"push", remoteName}
		if r.Force && task.GitForce {
			pushArgs = append(pushArgs, "--force")
		}
		pushArgs = append(pushArgs, refSpecs...)
		if task.GitTags {
			pushArgs = append(pushArgs, "--tags")
		}
		if _, err := e.gitOutput(ctx, dir, pushArgs...); err != nil {
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
