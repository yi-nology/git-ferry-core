package service

import (
	"context"
	"fmt"
	"log/slog"

	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// ===== Issue/PR 元数据高保真迁移(P3 收尾) =====
//
// 评审原话:「进一步区分 Git 对象恢复和 Issue/PR 元数据的高保真迁移」——
// git 对象(分支/commit)由同步任务搬运,Issue/PR 是平台侧数据,git 协议
// 搬不了,必须走平台 API 抽取+写入。
//
// **报告式 + dry-run**:迁移是一次性运维操作,先盘点(dry-run)再执行;
// 不做静默去重(确定性优于智能)——重复执行的去重面由调用方按报告决策。
//
// **保真边界(诚实声明,进报告 Notes)**:
//   - 作者/创建与评论时间 = 平台按调用身份重置,非原值;
//   - assignee/milestone/label 归属人不跨平台搬运(ID 空间不同);
//   - 源 PR 的 merged 状态不跨平台重放(只创建目标侧 CR,状态如实报告)。

// metadataMigrator 迁移所需的最小 provider 能力(源侧用读方法,
// 目标侧用写方法;同一接口,两侧各传一个 provider 实现实例)。
type metadataMigrator interface {
	ListIssues(ctx context.Context, opts sdkprov.ListIssuesOptions) ([]*sdkprov.Issue, int, error)
	CreateIssue(ctx context.Context, opts sdkprov.CreateIssueOptions) (*sdkprov.Issue, error)
	CloseIssue(ctx context.Context, owner, repo, number string) (*sdkprov.Issue, error)
	ListIssueComments(ctx context.Context, owner, repo, number string) ([]*sdkprov.IssueComment, error)
	CreateIssueComment(ctx context.Context, owner, repo, number, body string) (*sdkprov.IssueComment, error)
	ListCRs(ctx context.Context, opts sdkprov.ListCROptions) ([]*sdkprov.ChangeRequest, *int, error)
	CreateCR(ctx context.Context, opts sdkprov.CreateCROptions) (*sdkprov.ChangeRequest, error)
	CloseCR(ctx context.Context, owner, repo, number string) (*sdkprov.ChangeRequest, error)
}

// MetadataMigrateOptions 一次元数据迁移的选项。
type MetadataMigrateOptions struct {
	// TaskKey 复用同步任务的源/目标仓库与凭据解析。
	TaskKey string
	// Issues/Comments/PRs 选择要迁的类别(至少一项)。
	Issues   bool
	Comments bool
	PRs      bool
	// DryRun 只盘点不写(报告将创建数)。
	DryRun bool
	// Limit 单类别上限,0=默认 500(防单次操作预算爆炸)。
	Limit int
}

// defaultMigrateLimit 单类别默认上限(与 SDK 快照预算先例同量级)。
const defaultMigrateLimit = 500

// MetadataMigrateReport 迁移报告(诚实呈现做了什么、什么没做到)。
type MetadataMigrateReport struct {
	SourceIssues int `json:"sourceIssues"`
	SourcePRs    int `json:"sourcePrs"`
	// Created* :实际写入数(dry-run 时为 0,DryRunPlanned 为将创建数)。
	CreatedIssues   int      `json:"createdIssues"`
	CreatedComments int      `json:"createdComments"`
	CreatedPRs      int      `json:"createdPrs"`
	ClosedIssues    int      `json:"closedIssues"`
	ClosedPRs       int      `json:"closedPrs"`
	DryRun          bool     `json:"dryRun"`
	DryRunPlanned   []string `json:"dryRunPlanned,omitempty"`
	Errors          []string `json:"errors,omitempty"`
	Notes           []string `json:"notes,omitempty"`
}

// metadataFidelityNotes 保真边界(每次报告固定携带,不静默)。
var metadataFidelityNotes = []string{
	"作者与创建/评论时间由平台按调用身份重置,非原值",
	"assignee/milestone 不跨平台搬运(ID 空间不同)",
	"源 PR 的 merged 状态不跨平台重放:目标侧只创建 CR(源 closed 的会同步关闭)",
	"GitHub 的 issue 列表含 PR(IsPullRequest),已过滤后由 PR 通道单独搬运",
}

// MigrateRepoMetadata 执行 Issue/PR 元数据迁移(见文件头语义)。
func (s *Service) MigrateRepoMetadata(ctx context.Context, opts MetadataMigrateOptions) (*MetadataMigrateReport, error) {
	if opts.TaskKey == "" {
		return nil, ErrTaskNotFound
	}
	if !opts.Issues && !opts.PRs {
		return nil, fmt.Errorf("nothing selected: enable issues and/or prs")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultMigrateLimit
	}

	task, err := s.tasks.FindTaskByKey(opts.TaskKey)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrTaskNotFound
	}
	srcRec, srcProv, err := s.repos.GetRepoWithProvider(task.SourceRepoKey)
	if err != nil {
		return nil, fmt.Errorf("resolve source provider: %w", err)
	}
	tgtRec, tgtProv, err := s.repos.GetRepoWithProvider(task.TargetRepoKey)
	if err != nil {
		return nil, fmt.Errorf("resolve target provider: %w", err)
	}
	src, ok := srcProv.(metadataMigrator)
	if !ok {
		return nil, fmt.Errorf("source provider does not support metadata migration")
	}
	tgt, ok := tgtProv.(metadataMigrator)
	if !ok {
		return nil, fmt.Errorf("target provider does not support metadata migration")
	}

	// owner/repo 用 repo 记录的平台侧坐标(不从 key 猜)。
	srcOwner, srcRepo := srcRec.PlatformOwner, srcRec.PlatformRepo
	tgtOwner, tgtRepo := tgtRec.PlatformOwner, tgtRec.PlatformRepo

	rep := &MetadataMigrateReport{DryRun: opts.DryRun, Notes: metadataFidelityNotes}

	if opts.Issues {
		if err := migrateIssues(ctx, src, tgt, srcOwner, srcRepo, tgtOwner, tgtRepo, limit, opts, rep); err != nil {
			return rep, err
		}
	}
	if opts.PRs {
		if err := migrateCRs(ctx, src, tgt, srcOwner, srcRepo, tgtOwner, tgtRepo, limit, opts, rep); err != nil {
			return rep, err
		}
	}
	if len(rep.Errors) > 0 {
		slog.Warn("metadata migration finished with errors",
			"task", opts.TaskKey, "errors", len(rep.Errors))
	}
	return rep, nil
}

// migrateIssues 搬运 Issue(含可选评论)。
func migrateIssues(ctx context.Context,
	src, tgt metadataMigrator,
	srcOwner, srcRepo, tgtOwner, tgtRepo string,
	limit int, opts MetadataMigrateOptions, rep *MetadataMigrateReport,
) error {
	issues, err := listAllIssues(ctx, src, srcOwner, srcRepo, limit)
	if err != nil {
		return fmt.Errorf("list source issues: %w", err)
	}
	rep.SourceIssues = len(issues)
	for _, is := range issues {
		if is == nil || is.IsPullRequest {
			continue // PR 由 PR 通道搬运(GitHub 列表混入)
		}
		if opts.DryRun {
			rep.DryRunPlanned = append(rep.DryRunPlanned, "issue: "+is.Title)
			continue
		}
		created, cerr := tgt.CreateIssue(ctx, sdkprov.CreateIssueOptions{
			Owner:  tgtOwner,
			Repo:   tgtRepo,
			Title:  is.Title,
			Body:   is.Body,
			Labels: is.Labels,
		})
		if cerr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("create issue %q: %v", is.Title, cerr))
			continue
		}
		rep.CreatedIssues++
		// 源已关闭 ⇒ 目标同步关闭(状态保真;时间戳仍会重置)。
		if is.State == sdkprov.IssueStateClosed && created != nil && created.Number != "" {
			if _, cerr := tgt.CloseIssue(ctx, tgtOwner, tgtRepo, created.Number); cerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("close issue %q: %v", is.Title, cerr))
			} else {
				rep.ClosedIssues++
			}
		}
		if !opts.Comments || created == nil {
			continue
		}
		comments, lerr := src.ListIssueComments(ctx, srcOwner, srcRepo, is.Number)
		if lerr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("list comments of %q: %v", is.Title, lerr))
			continue
		}
		for _, c := range comments {
			if c == nil || c.Body == "" {
				continue
			}
			if _, cerr := tgt.CreateIssueComment(ctx, tgtOwner, tgtRepo, created.Number, c.Body); cerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("create comment on %q: %v", is.Title, cerr))
				continue
			}
			rep.CreatedComments++
		}
	}
	return nil
}

// migrateCRs 搬运 PR/MR。
func migrateCRs(ctx context.Context,
	src, tgt metadataMigrator,
	srcOwner, srcRepo, tgtOwner, tgtRepo string,
	limit int, opts MetadataMigrateOptions, rep *MetadataMigrateReport,
) error {
	crs, err := listAllCRs(ctx, src, srcOwner, srcRepo, limit)
	if err != nil {
		return fmt.Errorf("list source CRs: %w", err)
	}
	rep.SourcePRs = len(crs)
	for _, cr := range crs {
		if cr == nil {
			continue
		}
		if opts.DryRun {
			rep.DryRunPlanned = append(rep.DryRunPlanned, "cr: "+cr.Title)
			continue
		}
		created, cerr := tgt.CreateCR(ctx, sdkprov.CreateCROptions{
			Owner:        tgtOwner,
			Repo:         tgtRepo,
			Title:        cr.Title,
			Description:  cr.Description,
			SourceBranch: cr.SourceBranch,
			TargetBranch: cr.TargetBranch,
		})
		if cerr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("create CR %q: %v", cr.Title, cerr))
			continue
		}
		rep.CreatedPRs++
		// 源已关闭 ⇒ 目标同步关闭;merged 不重放(见 Notes)。
		if cr.State == sdkprov.CRStateClosed && created != nil && created.Number != "" {
			if _, cerr := tgt.CloseCR(ctx, tgtOwner, tgtRepo, created.Number); cerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("close CR %q: %v", cr.Title, cerr))
			} else {
				rep.ClosedPRs++
			}
		}
	}
	return nil
}

// listAllIssues 分页取尽(Page=0 首页语义,短页=结束;limit 截断)。
func listAllIssues(ctx context.Context, p metadataMigrator, owner, repo string, limit int) ([]*sdkprov.Issue, error) {
	var all []*sdkprov.Issue
	for page := 0; ; page++ {
		batch, _, err := p.ListIssues(ctx, sdkprov.ListIssuesOptions{
			Owner: owner, Repo: repo, Page: page, PerPage: 100,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 || len(all) >= limit {
			break
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// listAllCRs 分页取尽(同上)。
func listAllCRs(ctx context.Context, p metadataMigrator, owner, repo string, limit int) ([]*sdkprov.ChangeRequest, error) {
	var all []*sdkprov.ChangeRequest
	for page := 0; ; page++ {
		batch, _, err := p.ListCRs(ctx, sdkprov.ListCROptions{
			Owner: owner, Repo: repo, Page: page, PerPage: 100,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 || len(all) >= limit {
			break
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
