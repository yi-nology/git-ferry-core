package service

import (
	"context"
	"sort"
	"time"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/health"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 运维聚合查询（趋势 / 统一待办） =====

// trendsHistoryLimit 扫描的最大历史条数（沿用壳层既定值）。
const trendsHistoryLimit = 2000

// TrendPoint 单日同步成功率/耗时聚合（供前端小图）。
// JSON 标签与壳层历史响应逐字一致。
type TrendPoint struct {
	Date    string  `json:"date"`
	Total   int     `json:"total"`
	Success int     `json:"success"`
	Failed  int     `json:"failed"`
	AvgMs   float64 `json:"avg_duration_ms"`
}

// OpsTrendsResult 趋势序列 + 汇总。
type OpsTrendsResult struct {
	Days        int
	Items       []*TrendPoint
	Total       int
	Success     int
	Failed      int
	SuccessRate float64
}

// OpsTrends 同步成功率/耗时时间序列。
//
// 取全局最近 trendsHistoryLimit 条 run 按日聚合（id DESC → 新日期在前，
// 顺序沿用历史实现）。days ≤0 或 >365 由调用方归一化为 30。
func (s *Service) OpsTrends(ctx context.Context, days int) (*OpsTrendsResult, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	runs, err := s.tasks.RecentRuns(0, trendsHistoryLimit)
	if err != nil {
		return nil, err
	}
	return aggregateTrends(runs, days), nil
}

func aggregateTrends(runs []*model.SyncRun, days int) *OpsTrendsResult {
	res := &OpsTrendsResult{Days: days}
	cutoff := time.Now().AddDate(0, 0, -days)
	byDay := map[string]*TrendPoint{}
	order := []string{}
	for _, r := range runs {
		if r.StartTime.Before(cutoff) {
			continue
		}
		d := r.StartTime.Format("2006-01-02")
		agg, exists := byDay[d]
		if !exists {
			agg = &TrendPoint{Date: d}
			byDay[d] = agg
			order = append(order, d)
		}
		agg.Total++
		switch r.Status {
		case model.StatusSuccess:
			agg.Success++
		case model.StatusFailed:
			agg.Failed++
		}
		agg.AvgMs += float64(r.DurationMs)
	}
	res.Items = make([]*TrendPoint, 0, len(order))
	for _, d := range order {
		agg := byDay[d]
		if agg.Total > 0 {
			agg.AvgMs /= float64(agg.Total)
		}
		res.Items = append(res.Items, agg)
	}
	for _, a := range res.Items {
		res.Total += a.Total
		res.Success += a.Success
		res.Failed += a.Failed
	}
	if res.Total > 0 {
		res.SuccessRate = float64(res.Success) / float64(res.Total) * 100
	}
	return res
}

// OpsTodoItem 统一待办项（Renovate Dependency Dashboard 模式）。
type OpsTodoItem struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`     // health | orphan | rpo | drift
	Priority int             `json:"priority"` // 1=紧急
	TaskKey  string          `json:"task_key,omitempty"`
	RepoKey  string          `json:"repo_key,omitempty"`
	Title    string          `json:"title"`
	Reason   string          `json:"reason,omitempty"`
	Actions  []health.Action `json:"actions,omitempty"`
}

// todoHealthLimit 参与健康扫描的任务数上限（沿用壳层既定值）。
const todoHealthLimit = 100

// todoRepoLimit 孤儿仓库扫描的任务/仓库上限。
const todoRepoLimit = 200

// rpoWindowSeconds RPO 违规判定窗口（沿用壳层既定值：24h）。
const rpoWindowSeconds = 86400

// OpsTodo 聚合健康 attention + 孤儿仓库 + RPO 超标，按优先级输出可执行队列。
// 排序：优先级 → kind → id。
func (s *Service) OpsTodo(ctx context.Context) []OpsTodoItem {
	items := []OpsTodoItem{}

	// 1) 健康 attention（分数 <60）
	if snap, err := s.HealthSnapshot(ctx, todoHealthLimit, nil); err == nil {
		for hi := range snap.Items {
			h := &snap.Items[hi]
			if h.Score >= 60 {
				continue
			}
			pri := 2
			if h.Score < 40 {
				pri = 1
			}
			reason := ""
			if len(h.Issues) > 0 {
				reason = h.Issues[0]
			}
			items = append(items, OpsTodoItem{
				ID:       "health:" + h.Key,
				Kind:     "health",
				Priority: pri,
				TaskKey:  h.Key,
				Title:    "健康分偏低：" + nameOr(h.Name, h.Key),
				Reason:   reason,
				Actions:  h.ActionItems,
			})
		}
	}

	// 2) 孤儿仓库（无任务覆盖）
	repos, _, rerr := s.ListRepos(ctx, 0, todoRepoLimit)
	tasks, _, terr := s.ListTasks(ctx, "", 0, todoRepoLimit)
	if rerr == nil && terr == nil {
		taskByRepo := map[string]bool{}
		for _, t := range tasks {
			if t.SourceRepoKey != "" {
				taskByRepo[t.SourceRepoKey] = true
			}
			if t.TargetRepoKey != "" {
				taskByRepo[t.TargetRepoKey] = true
			}
		}
		for _, r := range repos {
			if taskByRepo[r.Key] {
				continue
			}
			items = append(items, OpsTodoItem{
				ID:       "orphan:" + r.Key,
				Kind:     "orphan",
				Priority: 2,
				RepoKey:  r.Key,
				Title:    "仓库无同步任务覆盖",
				Reason:   r.Name,
				Actions: []health.Action{{
					Kind: "cli", Dimension: "coverage", Priority: 2,
					Title:   "创建同步任务",
					Command: "gitferry task +create --name ... --source-repo " + r.Key + " --target-repo ...",
				}},
			})
		}
	}

	// 3) RPO 超标
	if rep, perr := s.RPOReport(rpoWindowSeconds); perr == nil && rep != nil {
		for mi := range rep.Metrics {
			m := &rep.Metrics[mi]
			if !m.RPOViolated {
				continue
			}
			items = append(items, OpsTodoItem{
				ID:       "rpo:" + m.TaskKey,
				Kind:     "rpo",
				Priority: 1,
				TaskKey:  m.TaskKey,
				Title:    "备份时效超标（RPO）",
				Reason:   m.RPOHuman,
				Actions: []health.Action{{
					Kind: "cli", Dimension: "freshness", Priority: 1,
					Title:   "查看 RPO 明细",
					Command: "gitferry ops +rpo --max-seconds 86400 --format json",
				}},
			})
		}
	}

	// 排序：优先级 → kind → key
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].ID < items[j].ID
	})
	return items
}

func nameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// RecentRuns 取全局最近的 run（不分任务），供趋势类聚合使用。
func (ts *TaskService) RecentRuns(offset, limit int) ([]*model.SyncRun, error) {
	return ts.runDAO.FindRecent(dao.DefaultPagination(offset, limit))
}
