package service

import (
	"context"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 策略即代码(P3):全量导出与回放 =====

// ExportTaskPolicies 全部任务(含禁用)的策略导出——确定性 JSON,
// 按 key 升序,可进 git 做版本化变更(策略走 PR 审阅)。
func (s *Service) ExportTaskPolicies(_ context.Context) (string, error) {
	tasks, _, err := s.tasks.taskDAO.FindAll(dao.DefaultPagination(0, 10000))
	if err != nil {
		return "", err
	}
	return model.ExportTaskPolicies(tasks)
}

// ApplyTaskPoliciesResult 一次回放的结果(逐任务成败,不整体回滚——
// 任务间独立,部分成功也要可见)。
type ApplyTaskPoliciesResult struct {
	Created int      `json:"created"`
	Updated int      `json:"updated"`
	Errors  []string `json:"errors,omitempty"`
}

// ApplyTaskPolicies 回放策略文件:已有任务更新策略字段,没有的创建。
// 校验复用 CreateTask 分支名校验路径;单任务失败记入 Errors 不中断
// 其余任务(策略文件是声明,回放是尽力应用+完整报告)。
func (s *Service) ApplyTaskPolicies(ctx context.Context, data []byte) (*ApplyTaskPoliciesResult, error) {
	policies, err := model.ParseTaskPolicies(data)
	if err != nil {
		return nil, err
	}
	res := &ApplyTaskPoliciesResult{}
	for _, p := range policies {
		if p.Key == "" {
			res.Errors = append(res.Errors, "policy entry missing key")
			continue
		}
		created, err := s.applyOnePolicy(ctx, p)
		if err != nil {
			res.Errors = append(res.Errors, p.Key+": "+err.Error())
			continue
		}
		if created {
			res.Created++
		} else {
			res.Updated++
		}
	}
	return res, nil
}

// applyOnePolicy 单任务回放:存在 ⇒ 更新策略字段;不存在 ⇒ 创建。
//
// UpdateTaskRequest 的语义差异要对齐:
//   - SourceBranch/TargetBranch/SyncMode:string,空串=不修改
//     (策略文件里必有值,直接带);
//   - Cron/ForcePushPolicy/Include/Exclude:指针,nil=不改,
//     空串=清空 ⇒ 带指针(空串也是「策略声明为清空」);
//   - bool 全指针:false=显式关;
//   - **SourceRepoKey/TargetRepoKey 更新侧不支持改指向**
//     (UpdateTaskRequest 无此字段):回放只在创建新任务时落 repo 指向,
//     已有任务改仓库=手工操作,不在策略回放面内。
//
// 返回是否为新建。
func (s *Service) applyOnePolicy(ctx context.Context, p model.TaskPolicy) (bool, error) {
	existing, err := s.tasks.FindTaskByKey(p.Key)
	if err != nil {
		return false, err
	}
	if existing == nil {
		req := p.ToCreateRequest()
		if _, err := s.CreateTask(ctx, req); err != nil {
			return false, err
		}
		if !p.Enabled {
			// 新建默认 enabled=true;策略声明禁用 ⇒ 建后立即关。
			if _, uerr := s.UpdateTask(ctx, &model.UpdateTaskRequest{Key: p.Key, Enabled: &p.Enabled}); uerr != nil {
				return true, uerr
			}
		}
		return true, nil
	}
	enabled := p.Enabled
	upd := &model.UpdateTaskRequest{
		Key:                p.Key,
		SourceBranch:       p.SourceBranch,
		TargetBranch:       p.TargetBranch,
		SyncMode:           p.SyncMode,
		Cron:               &p.Cron,
		Enabled:            &enabled,
		GitTags:            &p.GitTags,
		GitForce:           &p.GitForce,
		GitLFS:             &p.GitLFS,
		SyncWiki:           &p.SyncWiki,
		GitBundle:          &p.GitBundle,
		Submodules:         &p.Submodules,
		GitPushPrune:       &p.GitPushPrune,
		ForcePushPolicy:    &p.ForcePushPolicy,
		IncludeBranches:    &p.IncludeBranches,
		ExcludeRefPatterns: &p.ExcludeRefPatterns,
	}
	_, err = s.UpdateTask(ctx, upd)
	return false, err
}
