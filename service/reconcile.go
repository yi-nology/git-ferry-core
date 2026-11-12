package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// ===== 跨平台一致性对账(P3 Repository Governance) =====
//
// 回答一个问题:「这个任务的两端现在到底一不一样?」——不克隆、不推送,
// 纯平台 API 只读查询(分支 HEAD SHA + CompareCommits),代价一次 API 调用,
// 可随时对全量任务批量跑(发现静默漂移:目标被旁路推送/源删除分支等)。

// branchHeadQuerier 对账所需的最小 provider 能力(测试可 fake,
// 不用实现整个 sdkprov.Provider 九合一接口)。
type branchHeadQuerier interface {
	ListCommits(ctx context.Context, owner, repo string, opts sdkprov.ListCommitsOptions) ([]*sdkprov.CommitInfo, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*sdkprov.CompareResult, error)
}

// ReconcileTask 对账单个同步任务:查源/目标分支 HEAD SHA 并比较。
func (s *Service) ReconcileTask(ctx context.Context, taskKey string) (*model.ReconcileResult, error) {
	task, err := s.tasks.FindTaskByKey(taskKey)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrTaskNotFound
	}
	srcRepo, srcProv, err := s.repos.GetRepoWithProvider(task.SourceRepoKey)
	if err != nil {
		return nil, err
	}
	tgtRepo, tgtProv, err := s.repos.GetRepoWithProvider(task.TargetRepoKey)
	if err != nil {
		return nil, err
	}
	res := reconcileBranches(ctx, srcProv, tgtProv,
		srcRepo.PlatformOwner, srcRepo.PlatformRepo, task.SourceBranch,
		tgtRepo.PlatformOwner, tgtRepo.PlatformRepo, task.TargetBranch)
	res.TaskKey = taskKey
	return res, nil
}

// reconcileBranches 对账纯逻辑(平台无关):取两端 HEAD SHA → 比较 →
// 漂移时 CompareCommits 量化 ahead/behind。
func reconcileBranches(ctx context.Context,
	srcProv, tgtProv branchHeadQuerier,
	srcOwner, srcRepo, srcBranch, tgtOwner, tgtRepo, tgtBranch string,
) *model.ReconcileResult {
	res := &model.ReconcileResult{
		SourceBranch: srcBranch,
		TargetBranch: tgtBranch,
		CheckedAt:    time.Now(),
	}

	srcSHA, err := branchHeadSHA(ctx, srcProv, srcOwner, srcRepo, srcBranch)
	if err != nil {
		res.Status = model.ReconcileSourceMissing
		res.Message = "查询源分支失败: " + err.Error()
		return res
	}
	if srcSHA == "" {
		res.Status = model.ReconcileSourceMissing
		res.Message = "源分支不存在: " + srcBranch
		return res
	}
	tgtSHA, err := branchHeadSHA(ctx, tgtProv, tgtOwner, tgtRepo, tgtBranch)
	if err != nil {
		res.Status = model.ReconcileTargetMissing
		res.Message = "查询目标分支失败: " + err.Error()
		return res
	}
	if tgtSHA == "" {
		res.Status = model.ReconcileTargetMissing
		res.Message = "目标分支不存在: " + tgtBranch
		return res
	}
	res.SourceSHA, res.TargetSHA = srcSHA, tgtSHA

	if srcSHA == tgtSHA {
		res.Status = model.ReconcileInSync
		res.Message = "两端一致"
		return res
	}

	// 漂移:在**目标端**比较 base(目标)..head(源) ⇒ ahead = 目标缺的提交。
	cmp, err := tgtProv.CompareCommits(ctx, tgtOwner, tgtRepo, tgtSHA, srcSHA)
	if err != nil {
		res.Status = model.ReconcileDrift
		res.Message = "SHA 不一致,且 CompareCommits 失败: " + err.Error()
		slog.Warn("reconcile: compare failed", "src_sha", srcSHA, "tgt_sha", tgtSHA, "error", err)
		return res
	}
	res.Status = model.ReconcileDrift
	res.SourceAhead = cmp.AheadBy
	res.TargetAhead = cmp.BehindBy
	res.Message = "两端漂移:目标缺提交 / 目标多出提交(旁路写入?)"
	return res
}

// branchHeadSHA 取分支 HEAD SHA:ListCommits(branch, PerPage=1) 的第一条。
// 返回 "" = 分支不存在(空列表);错误向上抛。
func branchHeadSHA(ctx context.Context, q branchHeadQuerier, owner, repo, branch string) (string, error) {
	commits, err := q.ListCommits(ctx, owner, repo, sdkprov.ListCommitsOptions{
		Branch: branch, PerPage: 1,
	})
	if err != nil {
		return "", err
	}
	if len(commits) == 0 || commits[0] == nil {
		return "", nil
	}
	return commits[0].SHA, nil
}
