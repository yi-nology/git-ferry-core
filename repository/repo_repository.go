package repository

import (
	"context"
	"strings"

	"github.com/yi-nology/go-git-platform/auth/credential"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Pagination struct {
	Offset int
	Limit  int
}

// 分页缺省与上限——全仓唯一口径。
// 此前 core repo、壳层 converter.PageToOffset、壳层 mirror 的 parsePage
// 各写一份钳制,且缺省值还分 50 与 20 两种——同一次请求在任务列表返回 50 条、
// 在镜像通道列表返回 20 条。现在统一从这里取。
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

func DefaultPagination(offset, limit int) Pagination {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return Pagination{Offset: offset, Limit: limit}
}

type RepoRepository struct {
	base[model.Repo, uint]
	cm *credential.CryptoManager
}

func NewRepoRepository(db *gorm.DB) (*RepoRepository, error) {
	cm, err := credential.NewCryptoManager()
	if err != nil {
		return nil, oekiterrors.Wrap(err, "failed to create CryptoManager")
	}
	return &RepoRepository{base: newBase[model.Repo, uint](db), cm: cm}, nil
}

func (d *RepoRepository) FindAll(ctx context.Context, page Pagination) ([]*model.Repo, int64, error) {
	var repos []*model.Repo
	total, err := Paginate(d.db.WithContext(ctx), page, &repos)
	return repos, total, err
}

func (d *RepoRepository) FindByKey(ctx context.Context, key string) (*model.Repo, error) {
	repo, err := d.getByCondition(ctx, "`key` = ?", key)
	if err != nil || repo == nil {
		return repo, err
	}
	// Decrypt webhook secret
	if repo.WebhookSecret != "" {
		if decrypted, err := d.cm.Decrypt(repo.WebhookSecret); err == nil {
			repo.WebhookSecret = decrypted
		}
	}
	return repo, nil
}

func (d *RepoRepository) Create(repo *model.Repo) error {
	if repo.WebhookSecret != "" {
		encrypted, err := d.cm.Encrypt(repo.WebhookSecret)
		if err != nil {
			return err
		}
		repo.WebhookSecret = encrypted
	}
	return d.create(context.Background(), repo)
}

func (d *RepoRepository) Update(repo *model.Repo) error {
	if repo.WebhookSecret != "" {
		encrypted, err := d.cm.Encrypt(repo.WebhookSecret)
		if err != nil {
			return err
		}
		repo.WebhookSecret = encrypted
	}
	return d.update(context.Background(), repo)
}

// BatchCreate 批量创建仓库(用于同步场景,逐条加密后批量插入)。
func (d *RepoRepository) BatchCreate(repos []*model.Repo, batchSize int) error {
	for _, r := range repos {
		if r.WebhookSecret != "" {
			encrypted, err := d.cm.Encrypt(r.WebhookSecret)
			if err != nil {
				return err
			}
			r.WebhookSecret = encrypted
		}
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	return d.batchCreate(context.Background(), repos, batchSize)
}

// BatchUpdateRepoMeta 批量更新仓库元数据(路径/展示名/默认分支/clone URL)。
// 每个分块一条 CASE WHEN UPDATE(此前逐行 UPDATE,盘点几百个仓库会发几百条语句);
// 仍用列白名单,避免全字段回写覆盖加密 token/secret。
func (d *RepoRepository) BatchUpdateRepoMeta(repos []*model.Repo) error {
	if len(repos) == 0 {
		return nil
	}
	const chunkSize = 100
	return d.db.Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(repos); start += chunkSize {
			end := start + chunkSize
			if end > len(repos) {
				end = len(repos)
			}
			if err := updateRepoMetaChunk(tx, repos[start:end]); err != nil {
				return err
			}
		}
		return nil
	})
}

// updateRepoMetaChunk 单条语句更新一批仓库的元数据列。
func updateRepoMetaChunk(tx *gorm.DB, repos []*model.Repo) error {
	ids := make([]uint, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	caseExpr := func(value func(*model.Repo) any) clause.Expr {
		var sb strings.Builder
		sb.WriteString("CASE id")
		vars := make([]any, 0, len(repos)*2)
		for _, r := range repos {
			sb.WriteString(" WHEN ? THEN ?")
			vars = append(vars, r.ID, value(r))
		}
		sb.WriteString(" END")
		return clause.Expr{SQL: sb.String(), Vars: vars}
	}
	return tx.Model(&model.Repo{}).Where("id IN ?", ids).Updates(map[string]any{
		"clone_url":      caseExpr(func(r *model.Repo) any { return r.CloneURL }),
		"ssh_url":        caseExpr(func(r *model.Repo) any { return r.SSHURL }),
		"platform_owner": caseExpr(func(r *model.Repo) any { return r.PlatformOwner }),
		"platform_repo":  caseExpr(func(r *model.Repo) any { return r.PlatformRepo }),
		"name":           caseExpr(func(r *model.Repo) any { return r.Name }),
		"default_branch": caseExpr(func(r *model.Repo) any { return r.DefaultBranch }),
	}).Error
}

func (d *RepoRepository) Delete(key string) error {
	return d.db.Where("`key` = ?", key).Delete(&model.Repo{}).Error
}

// FindByCloneURL 按 clone URL 精确查仓库(批量镜像幂等用)。
// 查无此条返回 ErrNotFound。
func (d *RepoRepository) FindByCloneURL(cloneURL string) (*model.Repo, error) {
	return d.getByCondition(context.Background(), "clone_url = ?", cloneURL)
}

// FindByKeys 批量按 key 查仓库(批量删除用,避免逐 key 查询)。
func (d *RepoRepository) FindByKeys(keys []string) ([]*model.Repo, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	var repos []*model.Repo
	err := d.db.Where("`key` IN ?", keys).Find(&repos).Error
	return repos, err
}

// DeleteByKeys 批量按 key 删除仓库(单条 DELETE)。
func (d *RepoRepository) DeleteByKeys(keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	res := d.db.Where("`key` IN ?", keys).Delete(&model.Repo{})
	return res.RowsAffected, res.Error
}

// FindByPlatformID 根据平台 ID 查找仓库
func (d *RepoRepository) FindByPlatformID(platformID uint) ([]*model.Repo, error) {
	var repos []*model.Repo
	err := d.db.Where("platform_id = ?", platformID).Find(&repos).Error
	if err != nil {
		return nil, err
	}
	return repos, nil
}

// CountByPlatformID 统计平台下的仓库数量(不加载数据,比 FindByPlatformID + len 高效)
func (d *RepoRepository) CountByPlatformID(platformID uint) (int64, error) {
	var count int64
	err := d.db.Model(&model.Repo{}).Where("platform_id = ?", platformID).Count(&count).Error
	return count, err
}

// RepoFilter contains optional filters for listing repositories.
type RepoFilter struct {
	Search   string // search by name or clone_url (LIKE)
	Platform string // exact match on platform
	Status   string // exact match on status
	SortBy   string // column to sort by (default: created_at)
	OrderBy  string // sort direction: asc or desc (default: desc)
}

// allowedRepoSortColumns 是允许用于排序的列白名单。
// SortBy 来自 HTTP query,直拼 ORDER BY 会造成 SQL 注入,必须白名单校验。
var allowedRepoSortColumns = map[string]bool{
	"created_at": true,
	"updated_at": true,
	"name":       true,
}

// isAllowedRepoSort 判断给定的排序列是否在白名单内。
func isAllowedRepoSort(col string) bool {
	return allowedRepoSortColumns[col]
}

// ListWithFilter returns a filtered, sorted, paginated list of repos and the total count.
func (d *RepoRepository) ListWithFilter(ctx context.Context, page Pagination, filter *RepoFilter) ([]*model.Repo, int64, error) {
	var repos []*model.Repo
	var total int64

	query := d.db.WithContext(ctx).Model(&model.Repo{})

	// Apply search filter (LIKE on name or clone_url)
	if filter.Search != "" {
		like := "%" + EscapeLikePattern(filter.Search) + "%"
		query = query.Where("(name LIKE ? OR clone_url LIKE ?)", like, like)
	}

	// Apply exact-match filters
	if filter.Platform != "" {
		query = query.Where("platform = ?", filter.Platform)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}

	// Count total matching records
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Determine sort column and order
	// 白名单校验:只允许已知列名,防止 SortBy 直拼 ORDER BY 造成 SQL 注入
	sortBy := "created_at"
	if isAllowedRepoSort(filter.SortBy) {
		sortBy = filter.SortBy
	}
	order := "DESC"
	if filter.OrderBy == "asc" {
		order = "ASC"
	}

	// Apply pagination and sorting.
	// 主排序列(name/status/updated_at 等)非唯一,跨页可能重复/丢行——
	// 追加主键 tiebreaker 凑满全序(对齐 gormx#269 对 gormrepo.List 的修法)。
	if err := query.Offset(page.Offset).Limit(page.Limit).
		Order(sortBy + " " + order).Order("id " + order).Find(&repos).Error; err != nil {
		return nil, 0, err
	}

	return repos, total, nil
}

// Count 返回仓库总数(走 COUNT 聚合,不加载行)。
func (d *RepoRepository) Count() (int64, error) {
	var count int64
	if err := d.db.Model(&model.Repo{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
