package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/oekit/logx"
	"sort"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	sdkmodel "github.com/yi-nology/go-git-platform/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
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
//   - 源 PR 的 merged 状态不跨平台重放(只创建目标侧 CR,状态如实报告);
//   - draft 状态按源 PR 透传(SDK #160 写侧);目标平台创建面表达不了 draft
//     时降级为非 draft 并计入 DraftDegraded(不静默)。

// metadataMigrator 迁移所需的最小 provider 能力(源侧用读方法,
// 目标侧用写方法;同一接口,两侧各传一个 provider 实现实例)。
type metadataMigrator interface {
	ListIssues(ctx context.Context, opts sdkprov.ListIssuesOptions) ([]*sdkmodel.Issue, int, error)
	CreateIssue(ctx context.Context, opts sdkprov.CreateIssueOptions) (*sdkmodel.Issue, error)
	CloseIssue(ctx context.Context, owner, repo, number string) (*sdkmodel.Issue, error)
	ListIssueComments(ctx context.Context, owner, repo, number string) ([]*sdkmodel.IssueComment, error)
	CreateIssueComment(ctx context.Context, owner, repo, number, body string) (*sdkmodel.IssueComment, error)
	ListCRs(ctx context.Context, opts sdkprov.ListCROptions) ([]*sdkmodel.ChangeRequest, *int, error)
	CreateCR(ctx context.Context, opts sdkprov.CreateCROptions) (*sdkmodel.ChangeRequest, error)
	CloseCR(ctx context.Context, owner, repo, number string) (*sdkmodel.ChangeRequest, error)
}

// issueEnsurer 收敛能力最小面(SDK 2B Ensure* 的可注入缝):
// 生产用 sdkEnsurer 包完整 Provider;测试用 fake 返回可控 outcome。
type issueEnsurer interface {
	EnsureIssueState(ctx context.Context, owner, repo, number string, want sdkmodel.IssueState) (sdkrt.MutationOutcome, error)
	EnsureIssueLabels(ctx context.Context, owner, repo, number string, want []string) (sdkrt.MutationOutcome, error)
}

// sdkEnsurer 生产适配:包完整 Provider 调 SDK 包级 Ensure* 函数。
type sdkEnsurer struct{ p sdkprov.Provider }

func (e sdkEnsurer) EnsureIssueState(ctx context.Context, owner, repo, number string, want sdkmodel.IssueState) (sdkrt.MutationOutcome, error) {
	out, _, err := sdkrt.EnsureIssueState(ctx, e.p, owner, repo, number, want)
	return out, err
}

func (e sdkEnsurer) EnsureIssueLabels(ctx context.Context, owner, repo, number string, want []string) (sdkrt.MutationOutcome, error) {
	out, _, err := sdkrt.EnsureIssueLabels(ctx, e.p, owner, repo, number, want)
	return out, err
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
	CreatedIssues   int `json:"createdIssues"`
	CreatedComments int `json:"createdComments"`
	CreatedPRs      int `json:"createdPrs"`
	// 按 MutationOutcome 分类的幂等收敛统计(SDK 2B Ensure* 语义):
	// Updated=已映射 issue 且状态/标签发生写入;Unchanged=已达标零写入。
	UpdatedIssues   int `json:"updatedIssues,omitempty"`
	UnchangedIssues int `json:"unchangedIssues,omitempty"`
	// UnclosedMappings 创建过程中断的 pending 映射数(目标对象已写但
	// 编号未回填)。显式报告,要求人工核对——绝不静默再建一份。
	UnclosedMappings int `json:"unclosedMappings,omitempty"`
	// DraftDegraded 目标平台创建面无 draft 表达(SDK #160 ErrDraftNotSupported)
	// 而降级为非 draft 创建的 CR 数——如实计数,绝不静默。
	DraftDegraded int      `json:"draftDegraded,omitempty"`
	ClosedIssues  int      `json:"closedIssues"`
	ClosedPRs     int      `json:"closedPrs"`
	DryRun        bool     `json:"dryRun"`
	DryRunPlanned []string `json:"dryRunPlanned,omitempty"`
	Errors        []string `json:"errors,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

// metadataFidelityNotes 保真边界(每次报告固定携带,不静默)。
var metadataFidelityNotes = []string{
	"作者与创建/评论时间由平台按调用身份重置,非原值",
	"assignee/milestone 不跨平台搬运(ID 空间不同)",
	"源 PR 的 merged 状态不跨平台重放:目标侧只创建 CR(源 closed 的会同步关闭)",
	"draft 状态按源 PR 透传;目标平台创建面无法表达 draft 时降级为非 draft 并计入 draftDegraded",
	"GitHub 的 issue 列表含 PR(IsPullRequest),已过滤后由 PR 通道单独搬运",
}

// MigrateRepoMetadata 执行 Issue/PR 元数据迁移(见文件头语义)。
func (s *Service) MigrateRepoMetadata(ctx context.Context, opts MetadataMigrateOptions) (*MetadataMigrateReport, error) {
	if opts.TaskKey == "" {
		return nil, ErrTaskNotFound
	}
	if !opts.Issues && !opts.PRs {
		return nil, errors.New("nothing selected: enable issues and/or prs")
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
		return nil, errors.Wrap(err, "resolve source provider")
	}
	tgtRec, tgtProv, err := s.repos.GetRepoWithProvider(task.TargetRepoKey)
	if err != nil {
		return nil, errors.Wrap(err, "resolve target provider")
	}
	src, ok := srcProv.(metadataMigrator)
	if !ok {
		return nil, errors.New("source provider does not support metadata migration")
	}
	tgt, ok := tgtProv.(metadataMigrator)
	if !ok {
		return nil, errors.New("target provider does not support metadata migration")
	}

	// owner/repo 用 repo 记录的平台侧坐标(不从 key 猜)。
	srcOwner, srcRepo := srcRec.PlatformOwner, srcRec.PlatformRepo
	tgtOwner, tgtRepo := tgtRec.PlatformOwner, tgtRec.PlatformRepo

	rep := &MetadataMigrateReport{DryRun: opts.DryRun, Notes: metadataFidelityNotes}

	if opts.Issues {
		if err := migrateIssues(ctx, src, tgt, sdkEnsurer{tgtProv}, s.migrationMappings,
			srcRec.Platform, srcOwner, srcRepo, tgtRec.Platform, tgtOwner, tgtRepo,
			limit, opts, rep); err != nil {
			return rep, err
		}
		// 断点续迁可见性:把该源仓库仍未闭环的 pending 映射显式带进报告。
		if s.migrationMappings != nil {
			unclosed, uerr := s.migrationMappings.ListUnclosed(
				srcRec.Platform, srcOwner+"/"+srcRepo, model.MigrationObjectIssue)
			if uerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("list unclosed migration mappings: %v", uerr))
			} else {
				// 当前页内的 pending 已在 migrateIssues 逐条报告;这里只补
				// 「不在本次源列表里」的存量未闭环映射(避免重复噪音)。
				if len(unclosed) > rep.UnclosedMappings {
					nums := make([]string, 0, len(unclosed))
					for _, m := range unclosed {
						nums = append(nums, "#"+m.SourceNumber)
					}
					rep.Errors = append(rep.Errors, fmt.Sprintf(
						"unclosed migration mappings outside current page: %s (target objects may have been created; manual reconciliation required)",
						strings.Join(nums, ", ")))
					rep.UnclosedMappings = len(unclosed)
				}
			}
		}
	}
	if opts.PRs {
		if err := migrateCRs(ctx, src, tgt, srcOwner, srcRepo, tgtOwner, tgtRepo, limit, opts, rep); err != nil {
			return rep, err
		}
	}
	if len(rep.Errors) > 0 {
		logx.Warn("metadata migration finished with errors",
			"task", opts.TaskKey, "errors", len(rep.Errors))
	}
	return rep, nil
}

// migrationMappingStore 迁移映射的最小持久化面(生产用 Repository,测试可 fake/nil)。
// nil store = 无断点续迁能力(仅创建),用于不接 DB 的裸调用。
type migrationMappingStore interface {
	Find(sourcePlatform, sourceRepo, objectType, sourceNumber string) (*model.MigrationMapping, error)
	CreatePending(m *model.MigrationMapping) (bool, error)
	Save(m *model.MigrationMapping) error
	MarkFailed(sourcePlatform, sourceRepo, objectType, sourceNumber string, cause error) error
}

// issueFingerprint 源 Issue 内容指纹(title/body/state/labels 的规范化摘要)。
// 用于增量收敛判断与审计;排序 labels 保证同一内容不同顺序同指纹。
func issueFingerprint(is *sdkmodel.Issue) string {
	labels := append([]string(nil), is.Labels...)
	sort.Strings(labels)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s",
		is.Title, is.Body, string(is.State), strings.Join(labels, "\x00"))
	return hex.EncodeToString(h.Sum(nil))
}

// migrateIssues 搬运 Issue(含可选评论)——**映射身份键 + 幂等收敛**:
//   - 身份键 = (平台, 仓库, object_type, 源 issue number),绝不使用标题;
//     同标题的 N 个源 Issue 各自创建/映射到不同目标对象,不再互相归并;
//   - 映射命中且目标编号已回填 ⇒ EnsureIssueState/EnsureIssueLabels 收敛
//     (Unchanged 零写 / Updated 恰写);重复执行可安全重跑;
//   - 映射未命中 ⇒ 先写 pending 映射再创建目标对象,创建成功后回填编号;
//     进程在创建与回填之间崩溃时,pending 映射成为断点线索,报告显式
//     列出并要求人工核对(绝不静默再建一份);
//   - 评论仅对**新建**的 issue 重放(已有目标对象跳过,避免重复评论)。
func migrateIssues(ctx context.Context,
	src, tgt metadataMigrator, ens issueEnsurer, maps migrationMappingStore,
	srcPlatform, srcOwner, srcRepo, tgtPlatform, tgtOwner, tgtRepo string,
	limit int, opts MetadataMigrateOptions, rep *MetadataMigrateReport,
) error {
	issues, err := listAllIssues(ctx, src, srcOwner, srcRepo, limit)
	if err != nil {
		return errors.Wrap(err, "list source issues")
	}
	rep.SourceIssues = len(issues)

	srcRepoID := srcOwner + "/" + srcRepo
	lookupMapping := func(is *sdkmodel.Issue) (*model.MigrationMapping, error) {
		if maps == nil || is.Number == "" {
			return nil, nil
		}
		return maps.Find(srcPlatform, srcRepoID, model.MigrationObjectIssue, is.Number)
	}
	// saveMapping 回填/更新映射;失败不中断主流程,但必须在报告里可见。
	saveMapping := func(is *sdkmodel.Issue, targetNumber, status string, cause error) {
		if maps == nil || is.Number == "" {
			return
		}
		msg := ""
		if cause != nil {
			msg = cause.Error()
		}
		m := &model.MigrationMapping{
			SourcePlatform:     srcPlatform,
			SourceRepo:         srcRepoID,
			ObjectType:         model.MigrationObjectIssue,
			SourceNumber:       is.Number,
			TargetPlatform:     tgtPlatform,
			TargetRepo:         tgtOwner + "/" + tgtRepo,
			TargetNumber:       targetNumber,
			SourceUpdatedAt:    is.UpdatedAt,
			ContentFingerprint: issueFingerprint(is),
			Status:             status,
			LastError:          msg,
		}
		if status == model.MigrationMappingMigrated || status == model.MigrationMappingConverged {
			now := time.Now()
			m.MigratedAt = &now
		}
		if err := maps.Save(m); err != nil {
			rep.Errors = append(rep.Errors,
				fmt.Sprintf("save migration mapping for source issue #%s: %v", is.Number, err))
		}
	}

	for _, is := range issues {
		if is == nil || is.IsPullRequest {
			continue // PR 由 PR 通道搬运(GitHub 列表混入)
		}
		want := sdkmodel.IssueStateOpen
		if is.State == sdkmodel.IssueStateClosed {
			want = sdkmodel.IssueStateClosed
		}

		cur, merr := lookupMapping(is)
		if merr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("find migration mapping for source issue #%s: %v", is.Number, merr))
			continue
		}
		// ---- 断点识别:已登记 pending 但目标编号未回填 ----
		if cur != nil && cur.TargetNumber == "" && cur.Status == model.MigrationMappingPending {
			rep.UnclosedMappings++
			rep.Errors = append(rep.Errors, fmt.Sprintf(
				"unclosed migration mapping: source issue #%s %q has no target number (target object may have been created; manual reconciliation required)",
				is.Number, is.Title))
			continue
		}
		// ---- 收敛路径:映射命中且目标编号已知 ----
		if cur != nil && cur.TargetNumber != "" {
			if opts.DryRun {
				rep.DryRunPlanned = append(rep.DryRunPlanned, "converge: "+is.Title)
				continue
			}
			outcome, cerr := ens.EnsureIssueState(ctx, tgtOwner, tgtRepo, cur.TargetNumber, want)
			if cerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("ensure state %q: %v", is.Title, cerr))
				continue
			}
			if _, cerr := ens.EnsureIssueLabels(ctx, tgtOwner, tgtRepo, cur.TargetNumber, is.Labels); cerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("ensure labels %q: %v", is.Title, cerr))
				continue
			}
			if outcome == sdkrt.OutcomeUpdated {
				rep.UpdatedIssues++
			} else {
				rep.UnchangedIssues++
			}
			saveMapping(is, cur.TargetNumber, model.MigrationMappingConverged, nil)
			continue
		}
		// ---- 创建路径(映射未命中,或上次创建失败待重试) ----
		if opts.DryRun {
			rep.DryRunPlanned = append(rep.DryRunPlanned, "issue: "+is.Title)
			continue
		}
		if maps != nil && is.Number != "" {
			// 先写 pending 占位:并发/崩溃时同一源对象不会静默创建两份。
			pending := &model.MigrationMapping{
				SourcePlatform:     srcPlatform,
				SourceRepo:         srcRepoID,
				ObjectType:         model.MigrationObjectIssue,
				SourceNumber:       is.Number,
				TargetPlatform:     tgtPlatform,
				TargetRepo:         tgtOwner + "/" + tgtRepo,
				SourceUpdatedAt:    is.UpdatedAt,
				ContentFingerprint: issueFingerprint(is),
				Status:             model.MigrationMappingPending,
			}
			// 上次创建失败(failed)⇒ 显式重置为 pending 后重试创建;
			// 其余情况走「先占位再创建」的单赢家路径。
			if cur == nil || cur.Status == model.MigrationMappingFailed {
				placed := false
				if cur != nil {
					if serr := maps.Save(pending); serr != nil {
						rep.Errors = append(rep.Errors, fmt.Sprintf("reset failed mapping for source issue #%s: %v", is.Number, serr))
						continue
					}
					placed = true
				} else {
					var perr error
					placed, perr = maps.CreatePending(pending)
					if perr != nil {
						rep.Errors = append(rep.Errors, fmt.Sprintf("create pending mapping for source issue #%s: %v", is.Number, perr))
						continue
					}
				}
				if !placed {
					// 并发迁移先占位(pending 已在上面拦截)。
					again, ferr := lookupMapping(is)
					if ferr != nil {
						rep.Errors = append(rep.Errors, fmt.Sprintf("re-read migration mapping for source issue #%s: %v", is.Number, ferr))
						continue
					}
					if again != nil && again.TargetNumber != "" {
						// 别的执行者刚创建完:转收敛,不重复创建。
						outcome, cerr := ens.EnsureIssueState(ctx, tgtOwner, tgtRepo, again.TargetNumber, want)
						if cerr != nil {
							rep.Errors = append(rep.Errors, fmt.Sprintf("ensure state %q: %v", is.Title, cerr))
							continue
						}
						if _, cerr := ens.EnsureIssueLabels(ctx, tgtOwner, tgtRepo, again.TargetNumber, is.Labels); cerr != nil {
							rep.Errors = append(rep.Errors, fmt.Sprintf("ensure labels %q: %v", is.Title, cerr))
							continue
						}
						if outcome == sdkrt.OutcomeUpdated {
							rep.UpdatedIssues++
						} else {
							rep.UnchangedIssues++
						}
						continue
					}
					rep.UnclosedMappings++
					rep.Errors = append(rep.Errors, fmt.Sprintf(
						"concurrent/unclosed migration mapping for source issue #%s %q; skipped create to avoid duplicate",
						is.Number, is.Title))
					continue
				}
			}
		}

		created, cerr := tgt.CreateIssue(ctx, sdkprov.CreateIssueOptions{
			Owner:  tgtOwner,
			Repo:   tgtRepo,
			Title:  is.Title,
			Body:   is.Body,
			Labels: is.Labels,
		})
		if cerr != nil {
			if maps != nil {
				_ = maps.MarkFailed(srcPlatform, srcRepoID, model.MigrationObjectIssue, is.Number, cerr)
			}
			rep.Errors = append(rep.Errors, fmt.Sprintf("create issue %q: %v", is.Title, cerr))
			continue
		}
		rep.CreatedIssues++
		if created != nil && created.Number != "" {
			saveMapping(is, created.Number, model.MigrationMappingMigrated, nil)
		}
		// 源已关闭 ⇒ EnsureIssueState 幂等收敛(新建成 open → closed)。
		if want == sdkmodel.IssueStateClosed && created != nil && created.Number != "" {
			if _, err := ens.EnsureIssueState(ctx, tgtOwner, tgtRepo, created.Number, want); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("ensure state %q: %v", is.Title, err))
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
		return errors.Wrap(err, "list source CRs")
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
		created, cerr := createTargetCR(ctx, tgt, tgtOwner, tgtRepo, cr, rep)
		if cerr != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("create CR %q: %v", cr.Title, cerr))
			continue
		}
		rep.CreatedPRs++
		// 源已关闭 ⇒ 目标同步关闭;merged 不重放(见 Notes)。
		if cr.State == sdkmodel.CRStateClosed && created != nil && created.Number != "" {
			if _, cerr := tgt.CloseCR(ctx, tgtOwner, tgtRepo, created.Number); cerr != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("close CR %q: %v", cr.Title, cerr))
			} else {
				rep.ClosedPRs++
			}
		}
	}
	return nil
}

// createTargetCR 创建目标侧 CR:源 draft 状态按平台能力透传(SDK #160
// CreateCROptions.Draft 写侧);目标平台创建面无 draft 表达
// (ErrDraftNotSupported)⇒ 降级为非 draft 并计入 DraftDegraded——
// 如实计数,绝不静默产出"看起来成功"的非 draft CR。
func createTargetCR(ctx context.Context, tgt metadataMigrator,
	owner, repo string, cr *sdkmodel.ChangeRequest, rep *MetadataMigrateReport,
) (*sdkmodel.ChangeRequest, error) {
	opts := sdkprov.CreateCROptions{
		Owner:        owner,
		Repo:         repo,
		Title:        cr.Title,
		Description:  cr.Description,
		SourceBranch: cr.SourceBranch,
		TargetBranch: cr.TargetBranch,
		Draft:        cr.Draft,
	}
	created, err := tgt.CreateCR(ctx, opts)
	if err == nil || !cr.Draft || !errors.Is(err, sdkprov.ErrDraftNotSupported) {
		return created, err
	}
	// 降级路径:平台表达不了 draft ⇒ 非 draft 创建,计数留痕
	opts.Draft = false
	created, err = tgt.CreateCR(ctx, opts)
	if err == nil {
		rep.DraftDegraded++
	}
	return created, err
}

// pageSize 单页上限(SDK 统一 PerPage 约定)。
const pageSize = 100

// listAllPages 分页取尽(Page=0 首页语义,短页=结束;limit 截断)。
// Issue/CR 共用——骨架只差一个"取一页"闭包。
func listAllPages[T any](limit int, fetch func(page int) ([]T, error)) ([]T, error) {
	var all []T
	for page := 0; ; page++ {
		batch, err := fetch(page)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < pageSize || len(all) >= limit {
			break
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// listAllIssues 分页取尽 issue 列表。
func listAllIssues(ctx context.Context, p metadataMigrator, owner, repo string, limit int) ([]*sdkmodel.Issue, error) {
	return listAllPages(limit, func(page int) ([]*sdkmodel.Issue, error) {
		batch, _, err := p.ListIssues(ctx, sdkprov.ListIssuesOptions{
			Owner: owner, Repo: repo, Page: page, PerPage: pageSize,
		})
		return batch, err
	})
}

// listAllCRs 分页取尽 CR 列表。
func listAllCRs(ctx context.Context, p metadataMigrator, owner, repo string, limit int) ([]*sdkmodel.ChangeRequest, error) {
	return listAllPages(limit, func(page int) ([]*sdkmodel.ChangeRequest, error) {
		batch, _, err := p.ListCRs(ctx, sdkprov.ListCROptions{
			Owner: owner, Repo: repo, Page: page, PerPage: pageSize,
		})
		return batch, err
	})
}
