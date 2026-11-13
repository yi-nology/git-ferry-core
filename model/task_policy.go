package model

import (
	"encoding/json"
	"fmt"
	"sort"

	oekiterrors "github.com/oekit/pkg/errors"
)

// ===== 同步任务策略即代码(P3) =====
//
// 两层形态,与 mirror 侧 BuildPolicySnapshot 同纪律(确定性 JSON):
//  1. **执行时快照**(BuildTaskPolicySnapshot → SyncRun.PolicySnapshot):
//     固化「这次执行是在什么策略下跑的」,策略后续被改不影响历史复核;
//  2. **全量导出/回放**(ExportTaskPolicies / ApplyTaskPolicies):
//     全部任务的关键策略按 key 排序导出成可进 git 的声明文件,
//     变更走 PR 审阅,环境间用同一份文件回放(策略即代码主形态)。

// taskPolicy 是一条任务的策略声明(快照与导出共用同一 schema)。
//
// 只收**策略语义字段**(决定同步行为的),不收 Name/LastRunAt 这类
// 运行态/展示态——策略文件应能跨环境回放,运行态搬过去没有意义。
// WebhookToken 是凭据,绝不进策略文件。
type TaskPolicy struct {
	Key                string `json:"key"`
	SourceRepoKey      string `json:"source_repo_key"`
	SourceBranch       string `json:"source_branch"`
	TargetRepoKey      string `json:"target_repo_key"`
	TargetBranch       string `json:"target_branch"`
	SyncMode           string `json:"sync_mode"`
	Cron               string `json:"cron,omitempty"`
	Enabled            bool   `json:"enabled"`
	GitTags            bool   `json:"git_tags"`
	GitForce           bool   `json:"git_force"`
	GitLFS             bool   `json:"git_lfs"`
	SyncWiki           bool   `json:"sync_wiki"`
	GitBundle          bool   `json:"git_bundle"`
	Submodules         bool   `json:"submodules"`
	GitPushPrune       bool   `json:"git_push_prune"`
	ForcePushPolicy    string `json:"force_push_policy,omitempty"`
	IncludeBranches    string `json:"include_branches,omitempty"`
	ExcludeRefPatterns string `json:"exclude_ref_patterns,omitempty"`
}

// taskPolicyDoc 导出文件的信封(schema 版本 + 按 key 排序的策略条目)。
type taskPolicyDoc struct {
	Version int          `json:"version"`
	Tasks   []TaskPolicy `json:"tasks"`
}

// PolicyDocVersion 当前策略文件 schema 版本(回放侧拒绝未知版本)。
const PolicyDocVersion = 1

// BuildTaskPolicySnapshot 单任务的执行时策略快照(确定性 JSON)。
//
// 确定性:结构体声明序字段,无 map 参与——同任务状态 ⇒ 同字节,
// 便于跨 run 比对「策略变了没有」。
func BuildTaskPolicySnapshot(t *SyncTask) string {
	if t == nil {
		return ""
	}
	p := policyOf(t)
	out, err := json.Marshal(p)
	if err != nil {
		return "" // 理论不可达(全标量);快照失败不阻断执行
	}
	return string(out)
}

// ExportTaskPolicies 全部任务的策略导出(key 升序,确定性)。
func ExportTaskPolicies(tasks []*SyncTask) (string, error) {
	ps := make([]TaskPolicy, 0, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		ps = append(ps, policyOf(t))
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Key < ps[j].Key })
	out, err := json.MarshalIndent(taskPolicyDoc{Version: PolicyDocVersion, Tasks: ps}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// ParseTaskPolicies 解析策略文件(校验 schema 版本)。
func ParseTaskPolicies(data []byte) ([]TaskPolicy, error) {
	var doc taskPolicyDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, oekiterrors.Wrap(err, "parse policy doc")
	}
	if doc.Version != PolicyDocVersion {
		return nil, fmt.Errorf("unsupported policy doc version %d (want %d)", doc.Version, PolicyDocVersion)
	}
	return doc.Tasks, nil
}

// ToCreateRequest 把策略条目转成 CreateTaskRequest(回放入口复用既有
// 建任务校验:branch 名校验/字段合法性都在那条路径上)。
// 注意:CreateTaskRequest 无 Enabled 字段——回放侧建任务后按
// policy.Enabled 单独 Update(见 Service.ApplyTaskPolicies)。
func (p TaskPolicy) ToCreateRequest() *CreateTaskRequest {
	return &CreateTaskRequest{
		Key:                p.Key, // 回放保持策略文件身份(同 key 跨环境)
		Name:               p.Key, // Name 不在策略文件里,用 key 占位
		SourceRepoKey:      p.SourceRepoKey,
		SourceBranch:       p.SourceBranch,
		TargetRepoKey:      p.TargetRepoKey,
		TargetBranch:       p.TargetBranch,
		SyncMode:           p.SyncMode,
		Cron:               p.Cron,
		GitTags:            p.GitTags,
		GitForce:           p.GitForce,
		GitLFS:             p.GitLFS,
		SyncWiki:           p.SyncWiki,
		GitBundle:          p.GitBundle,
		Submodules:         p.Submodules,
		GitPushPrune:       p.GitPushPrune,
		ForcePushPolicy:    p.ForcePushPolicy,
		IncludeBranches:    p.IncludeBranches,
		ExcludeRefPatterns: p.ExcludeRefPatterns,
	}
}

// policyOf 任务 → 策略条目(单一事实源,快照与导出共用)。
func policyOf(t *SyncTask) TaskPolicy {
	return TaskPolicy{
		Key:                t.Key,
		SourceRepoKey:      t.SourceRepoKey,
		SourceBranch:       t.SourceBranch,
		TargetRepoKey:      t.TargetRepoKey,
		TargetBranch:       t.TargetBranch,
		SyncMode:           t.SyncMode,
		Cron:               t.Cron,
		Enabled:            t.Enabled,
		GitTags:            t.GitTags,
		GitForce:           t.GitForce,
		GitLFS:             t.GitLFS,
		SyncWiki:           t.SyncWiki,
		GitBundle:          t.GitBundle,
		Submodules:         t.Submodules,
		GitPushPrune:       t.GitPushPrune,
		ForcePushPolicy:    t.ForcePushPolicy,
		IncludeBranches:    t.IncludeBranches,
		ExcludeRefPatterns: t.ExcludeRefPatterns,
	}
}
