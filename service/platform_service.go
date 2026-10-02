package service

import (
	"context"
	"log/slog"
	"net/url"
	"strings"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// PlatformService 平台服务
type PlatformService struct {
	platformDAO *dao.PlatformDAO
	repoDAO     *dao.RepoDAO
	providerMgr *sdkprov.Manager
	hooks       *sdkprov.Hooks
}

// NewPlatformService 创建 PlatformService
func NewPlatformService(platformDAO *dao.PlatformDAO, repoDAO *dao.RepoDAO, providerMgr *sdkprov.Manager) *PlatformService {
	return &PlatformService{
		platformDAO: platformDAO,
		repoDAO:     repoDAO,
		providerMgr: providerMgr,
	}
}

// CreatePlatform 创建平台
func (s *PlatformService) CreatePlatform(ctx context.Context, platform *model.Platform) error {
	return s.platformDAO.Create(platform)
}

// GetPlatform 获取平台
func (s *PlatformService) GetPlatform(ctx context.Context, key string) (*model.Platform, error) {
	return s.platformDAO.FindByKey(key)
}

// GetPlatformByID 根据 ID 获取平台
func (s *PlatformService) GetPlatformByID(ctx context.Context, id uint) (*model.Platform, error) {
	return s.platformDAO.FindByID(id)
}

// ListPlatforms 列出所有平台
func (s *PlatformService) ListPlatforms(ctx context.Context) ([]*model.Platform, error) {
	return s.platformDAO.FindAll()
}

// UpdatePlatform 更新平台
func (s *PlatformService) UpdatePlatform(ctx context.Context, platform *model.Platform) error {
	return s.platformDAO.Update(platform)
}

// DeletePlatform 删除平台
func (s *PlatformService) DeletePlatform(ctx context.Context, key string) error {
	return s.platformDAO.Delete(key)
}

// SetDefaultPlatform 设置默认平台
func (s *PlatformService) SetDefaultPlatform(ctx context.Context, key string) error {
	return s.platformDAO.SetDefault(key)
}

// UpdatePlatformStatus 更新平台状态
func (s *PlatformService) UpdatePlatformStatus(ctx context.Context, key, status, testResult string) error {
	return s.platformDAO.UpdateStatus(key, status, testResult)
}

// TestPlatformConnection 测试平台连接
func (s *PlatformService) TestPlatformConnection(ctx context.Context, key string) (*sdkprov.TestConnectionResult, error) {
	platform, err := s.platformDAO.FindByKey(key)
	if err != nil {
		return nil, errors.Wrap(err, "query platform failed")
	}
	if platform == nil {
		return nil, errors.Newf("platform not found: %s", key)
	}

	provider, err := platformProvider(s.providerMgr, platform, s.hooks)
	if err != nil {
		return nil, err
	}

	// 测试连接
	result, err := provider.TestConnection(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "test connection failed")
	}

	return result, nil
}

// ListPlatformRepos 列出平台上的仓库
func (s *PlatformService) ListPlatformRepos(ctx context.Context, key, page, perPage string) ([]*sdkprov.PlatformRepo, error) {
	platform, err := s.platformDAO.FindByKey(key)
	if err != nil {
		return nil, errors.Wrap(err, "query platform failed")
	}
	if platform == nil {
		return nil, errors.Newf("platform not found: %s", key)
	}

	provider, err := platformProvider(s.providerMgr, platform, s.hooks)
	if err != nil {
		return nil, err
	}

	// 分页参数归一化后真正下传给 SDK
	p, pp := parsePageOpts(page, perPage)
	repos, err := provider.ListRepos(ctx, sdkprov.ListRepoOptions{Page: p, PerPage: pp})
	if err != nil {
		return nil, errors.Wrap(err, "list repos failed")
	}

	return repos, nil
}

// SyncPlatformRepos 同步平台仓库到本地
// SyncPlatformRepos 同步平台仓库(不过滤)。
func (s *PlatformService) SyncPlatformRepos(ctx context.Context, key string) (int, error) {
	return s.SyncPlatformReposFiltered(ctx, key, nil)
}

// SyncPlatformReposFiltered 按过滤器同步平台仓库(排除 archived/fork 等)。
func (s *PlatformService) SyncPlatformReposFiltered(ctx context.Context, key string, filter *RepoImportFilter) (int, error) {
	platform, err := s.platformDAO.FindByKey(key)
	if err != nil {
		return 0, errors.Wrap(err, "query platform failed")
	}
	if platform == nil {
		return 0, errors.Newf("platform not found: %s", key)
	}
	provider, err := platformProvider(s.providerMgr, platform, s.hooks)
	if err != nil {
		return 0, err
	}
	// 翻页拉全量,否则超过一页的仓库永远不会被同步
	repos, err := fetchAllPlatformRepos(ctx, provider)
	if err != nil {
		return 0, errors.Wrap(err, "list repos failed")
	}

	existingRepos, existingErr := s.repoDAO.FindByPlatformID(platform.ID)
	if existingErr != nil {
		slog.Warn("sync repo: failed to load existing repos, treating all as new",
			"platform_id", platform.ID, "error", existingErr)
		existingRepos = nil
	}

	plan := planRepoUpsert(platform, repos, indexExistingByKey(existingRepos), filter)
	count := s.persistRepoPlan(plan)

	if err := s.platformDAO.UpdateRepoCount(platform.ID); err != nil {
		slog.Warn("sync repo: failed to update platform repo count", "platform_id", platform.ID, "error", err)
	}
	return count, nil
}

// splitRepoPath 从仓库路径拆出 (owner, pathName),供 ListBranches/Webhook 等
// SDK 调用拼 pidOf(owner, repo) 使用。
// 优先用 FullName:嵌套群组 "obs/sdk/server" → ("obs", "sdk/server"),
// 与 SDK SplitFullName 一致,pidOf 还原后仍是完整路径。
// FullName 缺失时回退 SDK Owner+Name。
func splitRepoPath(fullName, sdkOwner, sdkName string) (owner, pathName string) {
	if fullName != "" {
		return sdkprov.SplitFullName(fullName)
	}
	return sdkOwner, sdkName
}

// rewriteCloneHost 将仓库 clone/ssh 地址的 scheme+host 替换为平台实例地址。
// 仅当平台配置了私有实例地址(instance_url)且与地址 host 不同时重写,
// 避免 GitHub 等公网平台(api.github.com 与 github.com host 天然不同)被误改。
func rewriteCloneHost(rawURL string, platform *model.Platform) string {
	if rawURL == "" || platform == nil || platform.InstanceURL == "" {
		return rawURL
	}
	instance := platform.InstanceURL
	if !strings.Contains(instance, "://") {
		instance = "https://" + instance
	}
	iu, err := url.Parse(instance)
	if err != nil || iu.Host == "" {
		return rawURL
	}
	cu, err := url.Parse(rawURL)
	if err != nil || cu.Host == "" || cu.Host == iu.Host {
		return rawURL
	}
	cu.Scheme = iu.Scheme
	cu.Host = iu.Host
	return cu.String()
}

// ListReposByPlatform 列出平台下的仓库
func (s *PlatformService) ListReposByPlatform(ctx context.Context, platformKey string) ([]*model.Repo, error) {
	platform, err := s.platformDAO.FindByKey(platformKey)
	if err != nil {
		return nil, err
	}
	if platform == nil {
		return nil, errors.Newf("platform not found: %s", platformKey)
	}
	return s.repoDAO.FindByPlatformID(platform.ID)
}

// CountReposByPlatform 统计平台下的仓库数量(不加载数据)
func (s *PlatformService) CountReposByPlatform(ctx context.Context, platformKey string) (int64, error) {
	platform, err := s.platformDAO.FindByKey(platformKey)
	if err != nil {
		return 0, err
	}
	if platform == nil {
		return 0, errors.Newf("platform not found: %s", platformKey)
	}
	return s.repoDAO.CountByPlatformID(platform.ID)
}
