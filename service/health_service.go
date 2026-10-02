package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/yi-nology/git-ferry-core/health"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
)

// ===== 健康评分（Scorecards 风格） =====
//
// 规则与维度实现在 git-ferry-core/health（纯逻辑包）；本文件负责从 DAO 取事实、
// 拼 Facts、跑规则并聚合成壳层可直接序列化的快照。壳层只做入参与响应包装。

// healthRules / healthDims 全局唯一来源；壳层不再各自持有。
var (
	healthRules = health.DefaultConfig()
	healthDims  = health.DefaultDimensions()
)

// HealthScoreItem 单仓库/任务健康分（含 Scorecards 风格维度）。
// JSON 标签与壳层历史响应逐字一致，前端/CLI 依赖。
type HealthScoreItem struct {
	Key    string   `json:"key"`
	Name   string   `json:"name"`
	Score  int      `json:"score"` // 0-100
	Level  string   `json:"level"` // gold/silver/bronze/basic
	Issues []string `json:"issues,omitempty"`
	// Dimensions 分项：reliability / freshness / schedule / safety / completeness
	Dimensions []health.Dimension `json:"dimensions,omitempty"`
	// Actions 建议动作（CLI 命令或人工步骤）
	Actions []string `json:"actions,omitempty"`
	// ActionItems 结构化动作（带可复制命令）
	ActionItems []health.Action `json:"action_items,omitempty"`
}

// HealthSummary 评分列表的聚合视图。
type HealthSummary struct {
	Levels      map[string]int    `json:"levels"`
	BelowSilver int               `json:"below_silver"`
	TopActions  []string          `json:"top_actions"`
	Attention   []HealthScoreItem `json:"attention"`
}

// HealthSnapshot 一次评分的完整结果。
type HealthSnapshot struct {
	Items   []HealthScoreItem
	Summary HealthSummary
}

// historyLimit 每任务取多少条历史参与事实计算（沿用壳层既定值）。
const healthHistoryLimit = 10

// HealthSnapshot 按维度规则给任务打分(借鉴 OpenSSF Scorecards)。
// 维度：reliability(35) / freshness(20) / schedule(15) / safety(15) / completeness(15)
//
// limit ≤0 → 50，>200 → 200；driftByTask 非空时折入 safety 维度
// （with_drift 的网络开销由调用方决定是否触发 DetectDrift）。
func (s *Service) HealthSnapshot(ctx context.Context, limit int, driftByTask map[string]int) (*HealthSnapshot, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	items, err := computeHealthScores(ctx, s, limit, driftByTask)
	if err != nil {
		return nil, err
	}
	return &HealthSnapshot{Items: items, Summary: summarizeHealth(items)}, nil
}

// summarizeHealth 聚合：按分数升序（最差在前，便于「需要关注」）+ 待办动作去重汇总。
func summarizeHealth(items []HealthScoreItem) HealthSummary {
	sum := HealthSummary{Levels: map[string]int{}}
	for i := range items {
		sum.Levels[items[i].Level]++
	}
	// 待办动作去重汇总（Renovate Dependency Dashboard 模式）
	actionSet := map[string]int{}
	for i := range items {
		for _, a := range items[i].Actions {
			actionSet[a]++
		}
	}
	for a, n := range actionSet {
		sum.TopActions = append(sum.TopActions, fmt.Sprintf("%s（×%d）", a, n))
	}
	sort.Strings(sum.TopActions)

	sum.Attention = []HealthScoreItem{}
	for i := range items {
		if items[i].Score < 60 {
			sum.Attention = append(sum.Attention, items[i])
		}
	}
	sum.BelowSilver = len(sum.Attention)
	return sum
}

// computeHealthScores 取任务与历史、跑规则打分。
// 历史按任务并行拉取（并发上限 8）——每任务各自取最近 N 条，保持既有语义。
func computeHealthScores(ctx context.Context, s *Service, limit int, driftByTask map[string]int) ([]HealthScoreItem, error) {
	tasks, _, err := s.ListTasks(ctx, "", 0, limit)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	type histResult struct {
		runs []*model.SyncRun
		err  error
	}
	histCh := make([]chan histResult, len(tasks))
	sem := make(chan struct{}, 8)
	for i := range tasks {
		ch := make(chan histResult, 1)
		histCh[i] = ch
		go func(i int, taskKey string) {
			sem <- struct{}{}
			defer func() { <-sem }()
			runs, _, herr := s.ListHistory(ctx, taskKey, 0, healthHistoryLimit)
			ch <- histResult{runs: runs, err: herr}
		}(i, tasks[i].Key)
	}

	items := make([]HealthScoreItem, 0, len(tasks))
	for i, t := range tasks {
		facts := health.Facts{
			"has_name":          strutil.BoolFact(t.Name != ""),
			"has_cron":          strutil.BoolFact(t.Cron != ""),
			"cron":              t.Cron,
			"enabled":           strutil.BoolFact(t.Enabled),
			"keep_divergent":    strutil.BoolFact(t.KeepDivergent),
			"git_force":         strutil.BoolFact(t.GitForce),
			"force_push_policy": t.ForcePushPolicy,
			"backup_enabled":    strutil.BoolFact(t.GitBundle),
			"sync_wiki":         strutil.BoolFact(t.SyncWiki),
		}
		if n, ok := driftByTask[t.Key]; ok && n > 0 {
			facts["drift_count"] = strutil.Itoa(n)
		}

		hr := <-histCh[i]
		runs := hr.runs
		if hr.err == nil && len(runs) > 0 {
			snaps := make([]health.RunSnapshot, 0, len(runs))
			for _, r := range runs {
				end := time.Time{}
				if r.EndTime != nil {
					end = *r.EndTime
				}
				stepFails := 0
				for si := range r.Steps {
					if r.Steps[si].Status == "failed" {
						stepFails++
					}
				}
				snaps = append(snaps, health.RunSnapshot{
					Status:       r.Status,
					ErrorMessage: r.ErrorMessage,
					EndAt:        end,
					StepFails:    stepFails,
					RetryTotal:   r.RetryTotal,
					ErrorType:    r.ErrorType,
				})
			}
			for k, v := range health.CollectRunFacts(snaps, now) {
				facts[k] = v
			}
		} else {
			facts["has_history"] = "false"
		}

		dimRes := health.EvaluateDimensions(healthDims, facts)
		legacy := healthRules.Evaluate(facts)
		issues := uniqueStrings(append(legacy.Issues, dimRes.Issues...))

		// 最近失败 run_id（动作路由）
		var failRunID int64
		if hr.err == nil {
			for _, r := range hr.runs {
				if r.Status == "failed" {
					failRunID = int64(r.ID)
					break
				}
			}
		}
		actionItems := health.RouteActions(health.TaskBrief{
			TaskKey:  t.Key,
			TaskName: t.Name,
			RunID:    failRunID,
			DriftN:   driftByTask[t.Key],
		}, dimRes.Dimensions)

		items = append(items, HealthScoreItem{
			Key:         t.Key,
			Name:        t.Name,
			Score:       dimRes.Score,
			Level:       dimRes.Level,
			Issues:      issues,
			Dimensions:  dimRes.Dimensions,
			Actions:     dimRes.Actions,
			ActionItems: actionItems,
		})
	}
	return items, nil
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
