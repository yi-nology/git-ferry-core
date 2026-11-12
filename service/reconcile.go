package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/oekit/concurrency/loop"
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

// ===== 周期对账 + 漂移告警(P3 闭环) =====

// reconcileEnabled 周期对账是否启用(间隔 >0)。
func (s *Service) reconcileEnabled() bool {
	return s.config != nil && s.config.Sync.ReconcileIntervalHours > 0
}

// reconcileLoop 周期对账:按 sync.reconcile_interval_hours 对全部启用任务
// 跑 ReconcileTask,结果落 reconcile_results,drift/缺失写 outbox 事件。
func (s *Service) reconcileLoop() {
	interval := time.Duration(s.config.Sync.ReconcileIntervalHours) * time.Hour
	loop.Run(s.bgCtx, loop.Config{
		Interval:   interval,
		FirstDelay: interval, // 不在启动瞬间打平台 API
		Name:       "periodic-reconcile",
	}, func(ctx context.Context) {
		s.runReconcilePass(ctx)
	}).Wait()
}

// runReconcilePass 对全部启用任务跑一轮对账;单任务失败不阻断其它任务。
func (s *Service) runReconcilePass(ctx context.Context) {
	if s.reconciles == nil {
		return
	}
	tasks, err := s.tasks.taskDAO.FindAllEnabled()
	if err != nil {
		slog.Error("reconcile: 列出启用任务失败", "error", err)
		return
	}
	var drift int
	for _, task := range tasks {
		res, rerr := s.ReconcileTask(ctx, task.Key)
		if rerr != nil {
			slog.Error("reconcile: 任务对账失败", "task", task.Key, "error", rerr)
			continue
		}
		if werr := s.recordReconcile(res); werr != nil {
			slog.Error("reconcile: 结果落库失败", "task", task.Key, "error", werr)
		}
		if res.Status != model.ReconcileInSync {
			drift++
		}
		// 平台 API 限速:任务间小睡,宁可慢不可被限流封。
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if drift > 0 {
		slog.Warn("reconcile: 本轮发现漂移/缺失", "drift", drift, "total", len(tasks))
	}
}

// recordReconcile 结果落库 + 漂移时写 outbox 事件(通知链的源头:
// 壳层订阅 reconcile.drift 后可接 octopus/邮件等渠道)。
func (s *Service) recordReconcile(res *model.ReconcileResult) error {
	if s.reconciles == nil {
		return nil
	}
	rec := &model.ReconcileRecord{
		TaskKey:     res.TaskKey,
		Status:      res.Status,
		SourceSHA:   res.SourceSHA,
		TargetSHA:   res.TargetSHA,
		SourceAhead: res.SourceAhead,
		TargetAhead: res.TargetAhead,
		Message:     res.Message,
		CheckedAt:   res.CheckedAt,
	}
	if err := s.reconciles.Upsert(rec); err != nil {
		return err
	}
	if res.Status == model.ReconcileInSync {
		return nil
	}
	slog.Warn("reconcile: 漂移/缺失", "task", res.TaskKey, "status", res.Status,
		"source_ahead", res.SourceAhead, "target_ahead", res.TargetAhead, "msg", res.Message)
	if s.outbox == nil {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{
		"status":       res.Status,
		"source_sha":   res.SourceSHA,
		"target_sha":   res.TargetSHA,
		"source_ahead": res.SourceAhead,
		"target_ahead": res.TargetAhead,
		"message":      res.Message,
		"checked_at":   res.CheckedAt,
	})
	// 漂移事件直接落 outbox(独立事实,无需事务);投递沿用既有派发器。
	return s.outbox.CreateInTx(s.tasks.runDAO.DB(), &model.OutboxEvent{
		Topic:   model.OutboxTopicReconcileDrift,
		TaskKey: res.TaskKey,
		Status:  res.Status,
		Payload: string(payload),
	})
}

// ListReconcileDrift 全部非 in_sync 的对账结果(告警面)。
func (s *Service) ListReconcileDrift(_ context.Context) ([]model.ReconcileRecord, error) {
	if s.reconciles == nil {
		return nil, nil
	}
	return s.reconciles.ListDrift()
}

// ListReconcileAll 全部对账结果(观测面)。
func (s *Service) ListReconcileAll(_ context.Context) ([]model.ReconcileRecord, error) {
	if s.reconciles == nil {
		return nil, nil
	}
	return s.reconciles.ListAll()
}
