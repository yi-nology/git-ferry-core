package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
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

	// **把本地分支推进到刚取到的源端 ref**(与主仓同款语义)。
	//
	// 原来缺这一步:fetch 只更新 refs/remotes/origin/*,本地 refs/heads/* 停在
	// 克隆那一刻的提交 ⇒ 推出去的 wiki **永远是旧内容**(而且每次都覆盖目标,
	// 目标侧人工改的内容被抹掉),而 details 写着 "wiki: done"。
	//
	// 用 "*" 对齐**全部分支**:clone 只把默认分支建成本地分支,
	// 其余 wiki 分支只存在于 refs/remotes/origin/* ⇒ 不对齐就永远同步不过去。
	if err := e.alignSourceBranches(ctx, wikiDir, "*"); err != nil {
		return errors.Wrap(err, "align wiki branches failed")
	}

	// ensure target remote
	if err := e.ensureWikiRemote(ctx, wikiDir, dstURL); err != nil {
		return errors.Wrap(err, "ensure wiki remote failed")
	}

	details.WriteString("  wiki: push\n")
	// **refspec 不带 "+"**:原来 "+refs/heads/*:refs/heads/*" 是无条件强推 ——
	// 目标 wiki 上有人工提交也会被直接覆盖,而 task.GitForce 形同虚设。
	// 现在:快进照推;目标有独有提交 ⇒ git 拒绝(非快进),
	// 要覆盖必须显式开 git_force。
	if _, err := e.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: wikiDir,
		Remote:   RemoteTarget,
		RefSpecs: []string{"refs/heads/*:refs/heads/*"},
		Force:    task.GitForce,
		Auth:     dstAuth,
	}); err != nil {
		if isWikiNotFound(err) {
			details.WriteString("  wiki: target not found, skip\n")
			return nil
		}
		if !task.GitForce && isNonFastForward(err) {
			return errors.Wrap(err,
				"push wiki failed: target wiki has commits not in source (non-fast-forward); "+
					"refused to overwrite — enable git_force to allow overwriting")
		}
		return errors.Wrap(err, "push wiki failed")
	}
	details.WriteString("  wiki: done\n")
	return nil
}

// isNonFastForward 判断是不是"非快进拒绝"(目标侧有源端没有的提交)。
func isNonFastForward(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// 关键词要**精确**:裸的 "rejected" 会命中 GitLab/Gitea 的受保护分支拒收
	// ("! [remote rejected] main -> main (protected branch hook declined)")
	// ⇒ 把"平台策略拒收"说成"目标有独有提交",并引导运维去开 git_force
	// (而 git_force 对受保护分支毫无作用)—— 误导 + 权限升级压力。
	for _, kw := range []string{
		"non-fast-forward", "fetch first", "would be overwritten",
		"updates were rejected",
	} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
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
