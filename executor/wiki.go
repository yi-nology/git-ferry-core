package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	oekiterrors "github.com/oekit/pkg/errors"
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

	// Clone Cache 自愈与主仓同轨(#25):共享仓被逐出/手删后,存量 wiki
	// workdir 的 alternates 悬空——git fetch 对此不报错(exit 0 + 整段历史
	// 重下),必须在决策点主动检出:清掉 wikiDir,下面的 Stat 自然走 clone
	// 分支 → ensure 重种条目。无缓存/无 alternates 时 no-op。
	healed, herr := healBrokenAlternates(wikiDir)
	if herr != nil {
		return oekiterrors.Wrap(herr, "heal wiki alternates failed")
	}
	if healed {
		details.WriteString("  wiki: shared cache missing, rebuilding wiki clone\n")
	}

	// clone or fetch
	if _, err := os.Stat(filepath.Join(wikiDir, ".git")); os.IsNotExist(err) {
		details.WriteString("  wiki: clone " + srcURL + "\n")
		cloneOpts := gitbackend.CloneOptions{
			URL:  srcURL,
			Path: wikiDir,
			Auth: srcAuth,
		}
		// Clone Cache:wiki 仓与主仓同为同源共享面(wiki URL 不同 ⇒ 独立缓存条目)。
		// 种入失败必须同样过 wiki 不存在分类——关键词在 cause 文本里,Wrap 保留,
		// 不能因为多了一层缓存包装就把「wiki 不存在 → 跳过」变成任务失败。
		if e.cache != nil {
			ref, usable, release, cerr := e.cache.ensure(ctx, e.backend, srcURL, "", srcAuth)
			if cerr != nil {
				if isWikiNotFound(cerr) {
					details.WriteString("  wiki: not found on source, skip\n")
					return nil
				}
				return oekiterrors.Wrap(cerr, "clone wiki failed")
			}
			if usable {
				// 锁持有到 wiki clone 结束(#23 在途保护);release 幂等。
				defer release()
				cloneOpts.Reference = ref
			}
		}
		if err := e.backend.Clone(ctx, cloneOpts); err != nil {
			// #27:共享仓 ensure 后消失的竞态——ErrNotAGitRepo 不是 wiki 不存在,
			// 降级无 reference 重试一次(与同源争用降级同形),绝不让
			// "does not exist" 关键词把竞态吞成静默 skip。
			if isCacheRaceNotRepo(err, cloneOpts.Reference) {
				slog.Info("clone cache vanished mid-clone, retrying wiki clone without reference",
					"error", err)
				cloneOpts.Reference = ""
				err = e.backend.Clone(ctx, cloneOpts)
			}
			if err != nil {
				if isWikiNotFound(err) {
					details.WriteString("  wiki: not found on source, skip\n")
					return nil
				}
				return oekiterrors.Wrap(err, "clone wiki failed")
			}
		}
	} else {
		details.WriteString("  wiki: fetch\n")
		if err := e.syncRemoteURL(ctx, wikiDir, RemoteOrigin, srcURL); err != nil {
			fmt.Fprintf(details, "  wiki: update origin URL failed: %v\n", err)
		}
		if _, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
			RepoPath: wikiDir,
			Remote:   RemoteOrigin,
			// 恒剪枝:wiki 与主仓同语义(源端删了 wiki 分支,本地也不该留着它)。
			// gogit 后端只在 Prune=true 时才剪 ⇒ 不显式传 true 两后端行为不同。
			Prune: true,
			Auth:  srcAuth,
		}); err != nil {
			return oekiterrors.Wrap(err, "fetch wiki failed")
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
		return oekiterrors.Wrap(err, "align wiki branches failed")
	}

	// ensure target remote
	if err := e.ensureWikiRemote(ctx, wikiDir, dstURL); err != nil {
		return oekiterrors.Wrap(err, "ensure wiki remote failed")
	}

	details.WriteString("  wiki: push\n")
	// **逐分支推**,不整批推。
	//
	// 整批推("refs/heads/*:refs/heads/*")时,目标上**任一**分支非快进
	// ⇒ 平台拒收整批 ⇒ 其它无分歧的分支也同步不了(一个分支的冲突拖垮全部)。
	// 逐分支推:有分歧的那个分支失败,其它照常送达。
	//
	// 而且 refspec 不带 "+":原来 "+refs/heads/*:refs/heads/*" 是无条件强推 ——
	// 目标 wiki 上的人��提交被直接覆盖,而 task.GitForce 形同虚设。
	// 现在:快进照推;目标有独有提交 ⇒ git 拒绝(非快进),
	// 要覆盖必须显式开 git_force。
	branches, berr := e.remoteTrackingBranches(ctx, wikiDir, RemoteOrigin)
	if berr != nil {
		return oekiterrors.Wrap(berr, "list wiki branches")
	}
	if len(branches) == 0 {
		details.WriteString("  wiki: no branches to push\n")
		return nil
	}
	var failed []string
	for _, b := range branches {
		_, perr := e.backend.Push(ctx, gitbackend.PushOptions{
			RepoPath: wikiDir,
			Remote:   RemoteTarget,
			RefSpecs: []string{"refs/heads/" + b + ":refs/heads/" + b},
			Force:    task.GitForce,
			Auth:     dstAuth,
		})
		if perr != nil {
			if isWikiNotFound(perr) {
				details.WriteString("  wiki: target not found, skip\n")
				return nil
			}
			if !task.GitForce && isNonFastForward(perr) {
				failed = append(failed, b+": target wiki has commits not in source (non-fast-forward)")
				continue
			}
			failed = append(failed, b+": "+perr.Error())
		}
	}
	if len(failed) > 0 {
		// 逐分支推:失败的分支明确点名,其它分支已经送达。
		return fmt.Errorf("push wiki failed for %d/%d branch(es):%s",
			len(failed), len(branches), strings.Join(failed, "; "))
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
