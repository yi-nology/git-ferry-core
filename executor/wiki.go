package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-platform-sdk/gitbackend"
)

// wikiURL 从仓库 clone URL 推导 wiki 仓库地址。
// 规则:URL 以 .git 结尾 → 换成 .wiki.git;否则追加 .wiki.git。
// GitHub/GitLab/Gitea 的 wiki 都是独立 git 仓库,遵循该约定。
// 返回空串表示无法推导(如本地路径)。
func wikiURL(cloneURL string) string {
	u := strings.TrimSpace(cloneURL)
	if u == "" {
		return ""
	}
	// 本地路径/文件传输没有 wiki 仓库
	if !strings.Contains(u, "://") && !strings.HasPrefix(u, "git@") {
		return ""
	}
	if strings.HasSuffix(u, ".wiki.git") {
		return u
	}
	if strings.HasSuffix(u, ".git") {
		return strings.TrimSuffix(u, ".git") + ".wiki.git"
	}
	return u + ".wiki.git"
}

// syncWiki 同步 wiki 仓库(clone/fetch → push)。
// wiki 可能不存在(未启用):clone 失败且是 not-found 时跳过,不算任务失败。
func (e *Executor) syncWiki(ctx context.Context, workDir string, task *model.SyncTask,
	srcRepo, dstRepo *model.Repo, srcPlat, dstPlat *model.Platform, details *strings.Builder,
) error {
	srcURL := wikiURL(srcRepo.CloneURL)
	dstURL := wikiURL(dstRepo.CloneURL)
	if srcURL == "" || dstURL == "" {
		details.WriteString("  wiki: skip (cannot derive wiki URL)\n")
		return nil
	}

	wikiDir := filepath.Join(workDir, "wiki")
	srcAuth := e.authConfig(ctx, srcRepo, srcPlat)
	dstAuth := e.authConfig(ctx, dstRepo, dstPlat)

	// clone or fetch
	if _, err := os.Stat(filepath.Join(wikiDir, ".git")); os.IsNotExist(err) {
		details.WriteString("  wiki: clone " + srcURL + "\n")
		if err := e.backend.Clone(ctx, gitbackend.CloneOptions{
			URL:  srcURL,
			Path: wikiDir,
			Auth: srcAuth,
		}); err != nil {
			if isWikiNotFound(err) {
				details.WriteString("  wiki: not found on source, skip\n")
				return nil
			}
			return errors.Wrap(err, "clone wiki failed")
		}
	} else {
		details.WriteString("  wiki: fetch\n")
		if err := e.syncRemoteURL(ctx, wikiDir, RemoteOrigin, srcURL); err != nil {
			fmt.Fprintf(details, "  wiki: update origin URL failed: %v\n", err)
		}
		if _, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
			RepoPath: wikiDir,
			Remote:   RemoteOrigin,
			Prune:    task.GitPrune,
			Auth:     srcAuth,
		}); err != nil {
			return errors.Wrap(err, "fetch wiki failed")
		}
	}

	// ensure target remote
	if err := e.ensureWikiRemote(ctx, wikiDir, dstURL); err != nil {
		return errors.Wrap(err, "ensure wiki remote failed")
	}

	details.WriteString("  wiki: push\n")
	if _, err := e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: wikiDir,
		Remote:   RemoteTarget,
		RefSpecs: []string{"+refs/heads/*:refs/heads/*"},
		Force:    task.GitForce,
		Auth:     dstAuth,
	}); err != nil {
		if isWikiNotFound(err) {
			details.WriteString("  wiki: target not found, skip\n")
			return nil
		}
		return errors.Wrap(err, "push wiki failed")
	}
	details.WriteString("  wiki: done\n")
	return nil
}

func (e *Executor) ensureWikiRemote(ctx context.Context, dir, url string) error {
	remotes, err := e.backend.GetRemotes(ctx, dir)
	if err != nil {
		return err
	}
	for _, name := range remotes {
		if name == RemoteTarget {
			return e.syncRemoteURL(ctx, dir, RemoteTarget, url)
		}
	}
	return e.backend.AddRemote(ctx, dir, RemoteTarget, url)
}

// isWikiNotFound wiki 仓库未启用时的 clone/fetch/push 错误(各平台文案不一)。
func isWikiNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, kw := range []string{"not found", "404", "repository not found", "does not exist", "could not read Username"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
