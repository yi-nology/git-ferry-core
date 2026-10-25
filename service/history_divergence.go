package service

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// remoteRelation 远端 HEAD 与「本地基线 / 本次产物 HEAD」的关系。
type remoteRelation int

const (
	// remoteUnknown 状态未知(拉取失败、远端没有该分支)。
	remoteUnknown remoteRelation = iota
	// remoteSameAsBase 远端 == 本地基线 ⇒ 推送是快进。
	remoteSameAsBase
	// remoteSameAsProduct 远端 == 本次产物 HEAD ⇒ 已经是本次移植的结果(重复发布)。
	remoteSameAsProduct
	// remoteAhead 远端领先(基线是远端的祖先)⇒ 强推会丢掉远端上的提交。
	remoteAhead
	// remoteBehind 远端落后(远端是基线的祖先)⇒ 推送会把它往前推。
	remoteBehind
	// remoteUnrelated 历史无关 ⇒ 强推会替换远端的整段历史。
	remoteUnrelated
)

// classifyRemote 判定远端 HEAD 与本地基线/产物 HEAD 的关系。
//
// **预检与推送闸门必须用同一份判定**。
// 以前两处各写一份且口径相反:预检只认"远端领先"与"其它"两种,于是
//
//	① 远端**落后**于基线(推送只是把它往前推,推送闸门明确放行)被预检报成
//	   「已分叉、推送需要强推」;
//	② 远端**已经是本次产物**(重复发布)也被报成「已分叉、需强推」。
//
// ⇒ 报告与事实相反,而且 `NeedsForce` 会把一次本可安全完成的同步**卡住**
// (Force=false 时直接不推),还把操作者引向不需要的强推。
//
// 参数:
//   - repo:用于做祖先判定的工作仓库(非 nil);
//   - remoteHead:远端分支当前 HEAD;
//   - baseHead:本地基线(移植前的目标树 HEAD);
//   - productHead:本次产物 HEAD(推送后远端会变成的那个)。
func classifyRemote(repo *git.Repository, remoteHead, baseHead, productHead string) (remoteRelation, string) {
	if remoteHead == "" {
		return remoteUnknown, "远端 HEAD 未知"
	}
	if baseHead != "" && remoteHead == baseHead {
		return remoteSameAsBase, "远端与本地基线一致,推送是快进"
	}
	if productHead != "" && remoteHead == productHead {
		return remoteSameAsProduct, "远端已经是本次移植的结果(重复发布)"
	}
	if baseHead != "" {
		// 远端领先(基线是远端的祖先)→ 强推会把远端的提交丢掉。
		if isAncestorCommit(repo, baseHead, remoteHead) {
			return remoteAhead, fmt.Sprintf("远端 HEAD %s 领先于本地基线 %s",
				shortHash(remoteHead), shortHash(baseHead))
		}
		// 远端落后(远端是基线的祖先)→ 推送只是把远端往前推,不丢东西。
		if isAncestorCommit(repo, remoteHead, baseHead) {
			return remoteBehind, "远端落后于本地基线,推送会把它往前推"
		}
	}
	// 历史无关 → 推送会替换远端的整段历史,必须显式确认。
	return remoteUnrelated, fmt.Sprintf("远端 HEAD %s 与本地基线 %s 历史无关,推送需要强推",
		shortHash(remoteHead), shortHash(baseHead))
}

// needForceForRelation 该关系是否必须强推。
func needForceForRelation(rel remoteRelation) bool {
	switch rel {
	case remoteAhead, remoteUnrelated:
		return true
	default:
		return false
	}
}

// fastForwardMirrorBranches 把本地分支快进到对应的远端跟踪分支。
//
// **为什么必须做**:gitbackend.Fetch 只更新 refs/remotes/<remote>/*,不动本地
// refs/heads/* ⇒ 本地分支永远停在**首次克隆时**。而历史同步的差异计算
// (mirror/history.BuildPlan)与基线判定读的都是本地分支 ⇒
// ① 已发布的文件每次仍判「需移植」(差异永不收敛);
// ② 第二次起的发布一律被判「远端已分叉」拦下(只能强推,而强推会丢掉远端上
// 的人工提交)—— 报错还让操作者「先更新目标仓」,而系统自己的「拉取最新」
// 恰恰不做这件事。
//
// **只快进**:本地不是远端的祖先(分叉)时不动 —— 那种情况要交给推送闸门报告,
// 不能在这里悄悄改基线。
//
// 只更新引用(不碰工作区/索引):这两个目录是**只读镜像**,计划阶段读的是提交树
// 而不是工作区文件。
func fastForwardMirrorBranches(dir, remote string) error {
	repo, err := openRepo(dir)
	if err != nil {
		return err
	}
	iter, err := repo.References()
	if err != nil {
		return err
	}
	defer iter.Close()

	prefix := "refs/remotes/" + remote + "/"
	var firstErr error
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil // 符号引用(如 origin/HEAD)不是分支
		}
		name := ref.Name().String()
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		branch := strings.TrimPrefix(name, prefix)
		if branch == "" {
			return nil
		}
		localName := plumbing.NewBranchReferenceName(branch)
		localRef, err := repo.Reference(localName, true)
		if err != nil {
			return nil // 本地没有这个分支 ⇒ 不管
		}
		localCommit, err := repo.CommitObject(localRef.Hash())
		if err != nil {
			return nil
		}
		remoteCommit, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return nil
		}
		ancestor, err := localCommit.IsAncestor(remoteCommit)
		if err != nil || !ancestor {
			return nil // 分叉/判定不了 ⇒ 不动,交给推送闸门报告
		}
		newRef := plumbing.NewHashReference(localName, ref.Hash())
		if err := repo.Storer.SetReference(newRef); err != nil && firstErr == nil {
			firstErr = err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return firstErr
}

// applyRemoteRelation 把远端关系判定落到预检的影响面上。
//
//   - 快进(远端==基线 / 远端落后)⇒ FastForward,不标 Diverged;
//   - 远端已是本次产物 ⇒ 不标 Diverged(重复发布,推送是空操作);
//   - 远端领先 / 历史无关 ⇒ Diverged(强推会覆盖远端已有的提交,必须人工确认)。
//
// **与推送闸门同一份判定**:口径相反时预检会把"远端落后""重复发布"报成
// "已分叉需强推",还会让 NeedsForce 把一次本可安全完成的同步卡住。
func applyRemoteRelation(imp *TargetImpact, rel remoteRelation, note string) {
	imp.Note = note
	switch rel {
	case remoteSameAsBase, remoteBehind:
		imp.FastForward = true
	case remoteSameAsProduct:
		// 已经是本次移植的结果:不需要强推(推送是空操作)。
	default:
		imp.Diverged = true
	}
}
