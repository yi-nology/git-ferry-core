package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// providerConfig 由平台记录构建 SDK provider 配置(token 可覆盖平台默认)。
// RetryConfig 给默认重试(429/5xx 退避),避免下游自己再包一层;nil=不重试。
// hooks 来自 WithProviderHooks 装配,随 Service 实例走,不再用包级全局变量。
func providerConfig(p *model.Platform, token string, hooks *sdkprov.Hooks) sdkprov.Config {
	rc := sdkprov.DefaultRetryConfig()
	return sdkprov.Config{
		Platform:    sdkprov.Platform(p.Type),
		BaseURL:     p.APIURL,
		Token:       token,
		SkipTLS:     p.SkipTLSVerify,
		RetryConfig: &rc,
		Hooks:       hooks,
	}
}

// providerFromManager 按 token 取 provider(统一走 Manager 缓存)。
// tokenOverride 非空时优先;否则按平台解析(GitHub App installation token 感知)。
func providerFromManager(mgr *sdkprov.Manager, p *model.Platform, tokenOverride string, hooks *sdkprov.Hooks) (sdkprov.Provider, error) {
	if mgr == nil {
		return nil, errors.New("provider manager is nil")
	}
	token := tokenOverride
	if token == "" {
		resolved, err := ResolvePlatformToken(p)
		if err != nil {
			return nil, fmt.Errorf("resolve platform token: %w", err)
		}
		token = resolved
	}
	prov, err := mgr.Get(providerConfig(p, token, hooks))
	if err != nil {
		return nil, fmt.Errorf("create provider failed: %w", err)
	}
	return prov, nil
}

// platformProvider 返回平台对应的 provider,统一经 Manager 缓存,
// 替代散落各处的 Config+NewProvider 样板。
// Token 优先取 GitHub App installation token(若配置),否则用 AccessToken。
func platformProvider(mgr *sdkprov.Manager, p *model.Platform, hooks *sdkprov.Hooks) (sdkprov.Provider, error) {
	return providerFromManager(mgr, p, "", hooks)
}

// ProviderForPlatform 按平台取 provider,供壳层复用:
// tokenOverride 非空优先,否则 ResolvePlatformToken(GitHub App 感知)。
func (s *Service) ProviderForPlatform(p *model.Platform, tokenOverride string) (sdkprov.Provider, error) {
	if p == nil {
		return nil, errors.New("platform is nil")
	}
	if s.platforms == nil {
		return nil, errors.New("provider manager is nil")
	}
	return providerFromManager(s.platforms.providerMgr, p, tokenOverride, s.providerHooks)
}

// parsePageOpts 解析分页参数字符串并归一化(空/非法回落 SDK 默认值)。
func parsePageOpts(page, perPage string) (int, int) {
	p, _ := strconv.Atoi(page)
	pp, _ := strconv.Atoi(perPage)
	return sdkprov.NormalizePageOpts(p, pp)
}

// RepoImportFilter 平台仓库导入过滤(借鉴 gickup filter.*)。
type RepoImportFilter struct {
	ExcludeArchived bool
	ExcludeForks    bool
	MinStars        int
	// IncludeLanguage 只导入该语言(空=不过滤)
	IncludeLanguage string
	// IncludeGlobs / ExcludeGlobs 按 FullName 匹配
	IncludeGlobs []string
	ExcludeGlobs []string
}

// Allow 判断仓库是否应导入。
func (f *RepoImportFilter) Allow(r *sdkprov.PlatformRepo) bool {
	if f == nil {
		return true
	}
	if f.ExcludeArchived && r.Archived {
		return false
	}
	if f.ExcludeForks && r.Fork {
		return false
	}
	if f.MinStars > 0 && r.Stars < f.MinStars {
		return false
	}
	if f.IncludeLanguage != "" && !strings.EqualFold(r.Language, f.IncludeLanguage) {
		return false
	}
	name := r.FullName
	if name == "" {
		name = r.Name
	}
	for _, g := range f.ExcludeGlobs {
		if globMatchName(g, name) {
			return false
		}
	}
	if len(f.IncludeGlobs) > 0 {
		ok := false
		for _, g := range f.IncludeGlobs {
			if globMatchName(g, name) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func globMatchName(pattern, s string) bool {
	ok, _ := filepath.Match(pattern, s)
	return ok
}

// fetchAllPlatformRepos 按最大页大小循环翻页,拉取平台全部仓库。
// 空页终止,复用平台 provider.CollectBounded(v0.73 起唯一分页面;
// 短页≠末页——服务端可能把页大小压到请求值以下);maxPages 作为安全
// 上限防止异常平台(忽略 page 参数)无限翻页,撞上限报 ErrPageBudgetExceeded
// 而非静默截断。这里只保留按 CloneURL/FullName 的去重。
func fetchAllPlatformRepos(ctx context.Context, provider sdkprov.Provider) ([]*sdkprov.PlatformRepo, error) {
	const maxPages = 100
	perPage := sdkprov.MaxPerPage

	repos, err := sdkprov.CollectBounded(ctx,
		func(ctx context.Context, page int) ([]*sdkprov.PlatformRepo, error) {
			return provider.ListRepos(ctx, sdkprov.ListRepoOptions{Page: page, PerPage: perPage})
		}, maxPages)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(repos))
	all := make([]*sdkprov.PlatformRepo, 0, len(repos))
	for _, r := range repos {
		id := r.CloneURL
		if id == "" {
			id = r.FullName
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		all = append(all, r)
	}
	return all, nil
}
