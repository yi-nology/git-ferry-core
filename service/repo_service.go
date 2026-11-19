package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/oekit/logx"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/stringx"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	sdkrt "github.com/yi-nology/go-git-platform/runtime"
)

// RepoService handles repository-related operations.
type RepoService struct {
	repoRepository     *repository.RepoRepository
	platformRepository *repository.PlatformRepository
	providerMgr        *sdkrt.Manager
	hooks              *sdkprov.Hooks
}

// NewRepoService creates a new RepoService instance.
func NewRepoService(repoRepository *repository.RepoRepository, platformRepository *repository.PlatformRepository, providerMgr *sdkrt.Manager) *RepoService {
	return &RepoService{
		repoRepository:     repoRepository,
		platformRepository: platformRepository,
		providerMgr:        providerMgr,
	}
}

// ListRepos returns a paginated list of repositories.
func (rs *RepoService) ListRepos(ctx context.Context, offset, limit int) ([]*model.Repo, int64, error) {
	page := repository.DefaultPagination(offset, limit)
	return rs.repoRepository.FindAll(ctx, page)
}

// CountRepos returns the total repo count via COUNT aggregate.
func (rs *RepoService) CountRepos() (int64, error) {
	return rs.repoRepository.Count()
}

// ListReposWithFilter returns a filtered, sorted, paginated list of repositories.
func (rs *RepoService) ListReposWithFilter(ctx context.Context, offset, limit int, filter *repository.RepoFilter) ([]*model.Repo, int64, error) {
	page := repository.DefaultPagination(offset, limit)
	return rs.repoRepository.ListWithFilter(ctx, page, filter)
}

// GetRepo returns a repository by key.
func (rs *RepoService) GetRepo(ctx context.Context, key string) (*model.Repo, error) {
	return rs.findRepoCtx(ctx, key)
}

// findRepo 查仓库并归一化查无此条。
func (rs *RepoService) findRepo(key string) (*model.Repo, error) {
	return rs.findRepoCtx(context.Background(), key)
}

func (rs *RepoService) findRepoCtx(ctx context.Context, key string) (*model.Repo, error) {
	r, err := rs.repoRepository.FindByKey(ctx, key)
	if err != nil {
		return nil, mapNotFound(err, ErrRepoNotFound)
	}
	return r, nil
}

// CreateRepo creates a new repository.
func (rs *RepoService) CreateRepo(ctx context.Context, req *model.CreateRepoRequest) (*model.Repo, error) {
	// Try to detect platform from URL
	result, err := sdkrt.DetectPlatform(req.RemoteURL)
	if err != nil && req.PlatformID == 0 {
		// If detection fails and no platform_id provided, return error
		if oekiterrors.Is(err, sdkprov.ErrPlatformNotSupported) {
			return nil, oekiterrors.Wrap(err, fmt.Sprintf("unsupported platform for URL %s", req.RemoteURL))
		}
		return nil, oekiterrors.Wrap(err, "invalid remote URL")
	}

	// If platform_id is provided, use the platform's type
	var platformType string
	var platformOwner, platformRepository string
	if result != nil {
		platformType = string(result.Platform)
		platformOwner = result.Owner
		platformRepository = result.Repo
	}

	if req.PlatformID > 0 && rs.platformRepository != nil {
		platform, err := rs.platformRepository.FindByID(ctx, req.PlatformID)
		if err == nil && platform != nil {
			platformType = platform.Type
		}
	}

	// Parse owner/repo from URL if not detected
	if platformOwner == "" || platformRepository == "" {
		// https://host/group/sub/repo.git → path "group/sub/repo"
		// 嵌套群组必须整段路径保留,否则 ListBranches/Webhook 拼 pidOf 会 404。
		raw := strings.TrimSuffix(strings.TrimSuffix(req.RemoteURL, ".git"), "/")
		if u, uerr := url.Parse(raw); uerr == nil && u.Path != "" {
			path := strings.Trim(u.Path, "/")
			if owner, name, ok := strings.Cut(path, "/"); ok && owner != "" && name != "" {
				platformOwner, platformRepository = owner, name
			}
		} else {
			parts := strings.Split(raw, "/")
			if len(parts) >= 2 {
				platformRepository = parts[len(parts)-1]
				platformOwner = parts[len(parts)-2]
			}
		}
	}

	repo := &model.Repo{
		Key:           stringx.UUIDv4(),
		Name:          req.Name,
		PlatformID:    req.PlatformID,
		Platform:      platformType,
		PlatformOwner: platformOwner,
		PlatformRepo:  platformRepository,
		CloneURL:      req.RemoteURL,
		Status:        model.RepoStatusActive,
	}

	if err := rs.repoRepository.Create(repo); err != nil {
		return nil, err
	}

	// Update platform repo count
	if repo.PlatformID > 0 && rs.platformRepository != nil {
		if err := rs.platformRepository.UpdateRepoCount(repo.PlatformID); err != nil {
			logx.Warn("create repo: failed to update platform repo count", "platform_id", repo.PlatformID, "error", err)
		}
	}

	return repo, nil
}

// UpdateRepo updates an existing repository.
func (rs *RepoService) UpdateRepo(ctx context.Context, req *model.UpdateRepoRequest) (*model.Repo, error) {
	repo, err := rs.findRepo(req.Key)
	if err != nil {
		return nil, err
	}

	if req.Name != "" {
		repo.Name = req.Name
	}

	if err := rs.repoRepository.Update(repo); err != nil {
		return nil, err
	}

	return repo, nil
}

// DeleteRepo deletes a repository by key.
func (rs *RepoService) DeleteRepo(ctx context.Context, key string) error {
	// Get repo first to know the platform_id
	repo, err := rs.findRepo(key)
	if err != nil {
		return err
	}

	platformID := repo.PlatformID

	if err := rs.repoRepository.Delete(key); err != nil {
		return err
	}

	// Update platform repo count
	if platformID > 0 && rs.platformRepository != nil {
		if err := rs.platformRepository.UpdateRepoCount(platformID); err != nil {
			logx.Warn("delete repo: failed to update platform repo count", "platform_id", platformID, "error", err)
		}
	}

	return nil
}

// DeleteRepos 批量删除仓库:一次查询 + 一次 DELETE + 每个受影响平台只刷一次计数。
//
// 逐 key 调用 DeleteRepo 会退化为 3×N 次 DB 往返(查、删、刷计数);
// 返回实际删除数与被忽略的不存在 key(供壳层逐 key 报错)。
func (rs *RepoService) DeleteRepos(ctx context.Context, keys []string) (deleted int, notFound []string, err error) {
	if len(keys) == 0 {
		return 0, nil, nil
	}
	repos, err := rs.repoRepository.FindByKeys(keys)
	if err != nil {
		return 0, nil, err
	}
	existing := make(map[string]struct{}, len(repos))
	platformIDs := make(map[uint]struct{})
	for _, r := range repos {
		existing[r.Key] = struct{}{}
		if r.PlatformID > 0 {
			platformIDs[r.PlatformID] = struct{}{}
		}
	}
	for _, k := range keys {
		if _, ok := existing[k]; !ok {
			notFound = append(notFound, k)
		}
	}
	if len(existing) == 0 {
		return 0, notFound, nil
	}
	n, err := rs.repoRepository.DeleteByKeys(keys)
	if err != nil {
		return 0, notFound, err
	}
	if rs.platformRepository != nil {
		for id := range platformIDs {
			if uerr := rs.platformRepository.UpdateRepoCount(id); uerr != nil {
				logx.Warn("delete repos: failed to update platform repo count", "platform_id", id, "error", uerr)
			}
		}
	}
	return int(n), notFound, nil
}

// GetRepoWithProvider 一次调用完成 repo 查找 + provider 解析,
// 消除调用方重复的 "GetRepoByKey → nil check → resolveRepoProvider" 样板。
func (rs *RepoService) GetRepoWithProvider(repoKey string) (*model.Repo, sdkprov.Provider, error) {
	repo, err := rs.findRepo(repoKey)
	if err != nil {
		return nil, nil, err
	}
	prov, err := rs.resolveRepoProvider(repo)
	if err != nil {
		return nil, nil, err
	}
	return repo, prov, nil
}

// resolveRepoProvider 解析仓库对应的 provider:凭据一律取自关联平台;
// 有关联平台记录时按平台配置构造(经 Manager 缓存),否则按 CloneURL 探测平台。
func (rs *RepoService) resolveRepoProvider(repo *model.Repo) (sdkprov.Provider, error) {
	var platform *model.Platform
	if repo.PlatformID > 0 && rs.platformRepository != nil {
		var findErr error
		platform, findErr = rs.platformRepository.FindByID(context.Background(), repo.PlatformID)
		if findErr != nil && !isRepositoryNotFound(findErr) {
			return nil, oekiterrors.Wrap(findErr, fmt.Sprintf("find platform %d for repo %s", repo.PlatformID, repo.Key))
		}
		if isRepositoryNotFound(findErr) {
			// 关联平台已删除:退化为按 CloneURL 探测(与原 nil 平台同路径)。
			platform = nil
		}
	}
	if platform != nil {
		// **必须走 providerFromManager**:token 由 ResolvePlatformToken 解析
		// (GitHub App installation token 优先,否则 PAT)。
		//
		// 以前这里直接 `providerMgr.Get(providerConfig(platform, platform.AccessToken, hooks))`
		// —— 绕过了这一步 ⇒ **只配了 GitHub App(没有 PAT)的平台拿到空 token**
		// ⇒ 仓库级 API 调用(webhook 注册 / webhook 事件处理)全部 401。
		// 其余 provider 路径(platformProvider / ProviderForPlatform)都走
		// ResolvePlatformToken,只有仓库这一条是例外。
		return providerFromManager(rs.providerMgr, platform, "", rs.hooks)
	}
	return rs.providerMgr.GetByURL(repo.CloneURL, "")
}

// GetRepoByKey returns a repository by key.
func (rs *RepoService) GetRepoByKey(key string) (*model.Repo, error) {
	return rs.findRepo(key)
}

// SetWebhookSecret 设置仓库的 Webhook 密钥(经 repo_dao 加密落库),供入站验签使用。
func (rs *RepoService) SetWebhookSecret(repoKey, secret string) error {
	repo, err := rs.findRepo(repoKey)
	if err != nil {
		return err
	}
	repo.WebhookSecret = secret
	return rs.repoRepository.Update(repo)
}

// TestConnection tests the connection to a repository.
func (rs *RepoService) TestConnection(ctx context.Context, repoKey string) (*model.TestConnectionResult, error) {
	_, prov, err := rs.GetRepoWithProvider(repoKey)
	if err != nil {
		return &model.TestConnectionResult{Success: false, Message: err.Error()}, nil
	}

	result, err := prov.TestConnection(ctx)
	if err != nil {
		return &model.TestConnectionResult{Success: false, Message: err.Error()}, nil
	}

	return &model.TestConnectionResult{Success: result.Connected, Message: result.Message}, nil
}

// ListBranches returns a list of branches for a repository.
func (rs *RepoService) ListBranches(ctx context.Context, repoKey string) ([]string, error) {
	repo, prov, err := rs.GetRepoWithProvider(repoKey)
	if err != nil {
		return nil, err
	}

	branches, err := prov.ListBranches(ctx, repo.PlatformOwner, repo.PlatformRepo)
	if err != nil {
		return nil, err
	}

	var result []string
	for _, b := range branches {
		result = append(result, b.Name)
	}

	return result, nil
}
