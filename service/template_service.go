package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yi-nology/git-ferry-core/health"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/tpl"
)

// ===== 同步策略模板（借鉴 Renovate packageRules/presets） =====
//
// 模板库文件存储在 core 统一持有（Service.Templates）；命中预览、批量套用、
// 仓库资产盘点等业务规则在本文件，壳层只做 HTTP 绑定/审计/响应包装。

// tplTaskLimit 参与模板匹配 / 盘点扫描的任务与仓库上限。
const tplTaskLimit = 200

// Templates 返回同步策略模板库（NewService 时按 cfg.Templates.Path 打开）。
func (s *Service) Templates() *tpl.Store { return s.templates }

// SetTemplates 覆盖模板库（测试隔离 / 运行时切换文件）。
func (s *Service) SetTemplates(st *tpl.Store) {
	if st != nil {
		s.templates = st
	}
}

// tplMatchFilter 把模板 Match 条件转成 health.Filter（include/exclude/globs）。
func tplMatchFilter(m map[string][]string) *health.Filter {
	f := &health.Filter{}
	if m == nil {
		return f
	}
	f.Include = m["include"]
	f.Exclude = m["exclude"]
	f.IncludeGlobs = m["include_globs"]
	f.ExcludeGlobs = m["exclude_globs"]
	return f
}

// TemplateMatch 命中的任务摘要。
type TemplateMatch struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// PreviewTemplate 套用前预览命中结果（Renovate dry-run 语义）。
func (s *Service) PreviewTemplate(ctx context.Context, templateID string) (*tpl.Template, []TemplateMatch, error) {
	t, err := s.templates.Get(templateID)
	if err != nil {
		return nil, nil, ErrTemplateNotFound
	}
	tasks, _, err := s.ListTasks(ctx, "", 0, tplTaskLimit)
	if err != nil {
		return nil, nil, err
	}
	filter := tplMatchFilter(t.Match)
	matched := []TemplateMatch{}
	for _, task := range tasks {
		if filter.Allow(task.Key, task.Name) {
			matched = append(matched, TemplateMatch{Key: task.Key, Name: task.Name})
		}
	}
	return t, matched, nil
}

// TemplateChange 批量套用产生的单条变更（Before/After 为变更前后字段快照）。
type TemplateChange struct {
	Key    string         `json:"key"`
	Name   string         `json:"name"`
	Before map[string]any `json:"before"`
	After  map[string]any `json:"after"`
}

// TemplateApplyResult 批量套用结果。
type TemplateApplyResult struct {
	Template     *tpl.Template
	Effective    tpl.Spec
	ExtendsChain []string
	Changed      []TemplateChange
	DryRun       bool
}

// ApplyTemplate 批量套用策略模板：沿 Extends 链合并 Spec（子覆盖父），
// 只更新已存在任务的 cron/启用位；不隐式创建任务，避免误建。
func (s *Service) ApplyTemplate(ctx context.Context, templateID string, dryRun bool) (*TemplateApplyResult, error) {
	eff, chain, err := s.templates.Resolve(templateID)
	if err != nil {
		if errors.Is(err, tpl.ErrCycle) {
			return nil, fmt.Errorf("%w: %s", ErrTemplateCycle, strings.Join(chain, " → "))
		}
		return nil, ErrTemplateNotFound
	}
	baseTpl, err := s.templates.Get(templateID)
	if err != nil {
		return nil, ErrTemplateNotFound
	}
	tasks, _, err := s.ListTasks(ctx, "", 0, tplTaskLimit)
	if err != nil {
		return nil, err
	}

	filter := tplMatchFilter(baseTpl.Match)
	res := &TemplateApplyResult{Template: baseTpl, Effective: eff, ExtendsChain: chain, Changed: []TemplateChange{}, DryRun: dryRun}
	for _, task := range tasks {
		if !filter.Allow(task.Key, task.Name) {
			continue
		}
		before := map[string]any{"cron": task.Cron, "enabled": task.Enabled}
		after := map[string]any{"cron": task.Cron, "enabled": task.Enabled}
		need := false
		if eff.Cron != "" && eff.Cron != task.Cron {
			after["cron"] = eff.Cron
			need = true
		}
		if eff.Enabled != nil && *eff.Enabled != task.Enabled {
			after["enabled"] = *eff.Enabled
			need = true
		}
		if !need {
			continue
		}
		if !dryRun {
			// UpdateTaskRequest：空字符串=不改；Enabled 指针 nil=不改
			upd := &model.UpdateTaskRequest{Key: task.Key, Name: task.Name}
			if eff.Cron != "" {
				upd.Cron = eff.Cron
			}
			if eff.Enabled != nil {
				upd.Enabled = eff.Enabled
			}
			if _, uerr := s.UpdateTask(ctx, upd); uerr != nil {
				continue
			}
		}
		res.Changed = append(res.Changed, TemplateChange{Key: task.Key, Name: task.Name, Before: before, After: after})
	}
	return res, nil
}

// ===== 仓库资产盘点 =====

// InventoryItem 仓库资产盘点项：有没有任务覆盖、最近执行。
type InventoryItem struct {
	RepoKey       string   `json:"repo_key"`
	RepoName      string   `json:"repo_name"`
	Platform      string   `json:"platform,omitempty"`
	Status        string   `json:"status,omitempty"`
	HasTask       bool     `json:"has_task"`
	TaskKeys      []string `json:"task_keys,omitempty"`
	LastRunStatus string   `json:"last_run_status,omitempty"`
	LastRunAt     string   `json:"last_run_at,omitempty"`
	Coverage      string   `json:"coverage"` // covered | no_task | stale | failing
}

// InventoryResult 资产盘点汇总。
type InventoryResult struct {
	Items   []InventoryItem
	Covered int
	Orphan  int
	Failing int
}

// inventoryTimeFormat 最近执行时间格式（与历史响应一致）。
const inventoryTimeFormat = "2006-01-02 15:04:05"

// RepoInventory 消灭孤儿仓库：哪些仓库没同步任务、哪些一直失败、哪些很久没跑。
func (s *Service) RepoInventory(ctx context.Context) (*InventoryResult, error) {
	repos, _, err := s.ListRepos(ctx, 0, tplTaskLimit)
	if err != nil {
		return nil, err
	}
	tasks, _, err := s.ListTasks(ctx, "", 0, tplTaskLimit)
	if err != nil {
		return nil, err
	}
	taskByRepo := map[string][]string{}
	for _, t := range tasks {
		if t.SourceRepoKey != "" {
			taskByRepo[t.SourceRepoKey] = append(taskByRepo[t.SourceRepoKey], t.Key)
		}
		if t.TargetRepoKey != "" && t.TargetRepoKey != t.SourceRepoKey {
			taskByRepo[t.TargetRepoKey] = append(taskByRepo[t.TargetRepoKey], t.Key)
		}
	}

	res := &InventoryResult{Items: make([]InventoryItem, 0, len(repos))}
	for _, r := range repos {
		item := InventoryItem{
			RepoKey:  r.Key,
			RepoName: r.Name,
			Platform: r.Platform,
			Status:   r.Status,
			TaskKeys: taskByRepo[r.Key],
			HasTask:  len(taskByRepo[r.Key]) > 0,
			Coverage: "",
		}
		if !item.HasTask {
			item.Coverage = "no_task"
			res.Orphan++
		} else {
			res.Covered++
			// 取任一任务最近历史
			for _, tk := range item.TaskKeys {
				runs, _, herr := s.ListHistory(ctx, tk, 0, 1)
				if herr != nil || len(runs) == 0 {
					continue
				}
				item.LastRunStatus = runs[0].Status
				if runs[0].EndTime != nil {
					item.LastRunAt = runs[0].EndTime.Format(inventoryTimeFormat)
				}
				if runs[0].Status == model.StatusFailed {
					item.Coverage = "failing"
					res.Failing++
					res.Covered--
				}
				break
			}
			if item.Coverage == "" {
				item.Coverage = "covered"
			}
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}
