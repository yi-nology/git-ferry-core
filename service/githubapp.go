package service

import (
	"context"
	"fmt"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/oekit/pkg/cache"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/githubapp"
)

// ghAppTokenInner 缓存 installation token（有效期约 1h）。
//
// TTL 取 55min：token 本身有效 1h，到期前 5min 刷新。
// 实现委托 oekit/pkg/cache.LocalCache（TTL 判定 / 并发安全 / 惰性过期回收
// 只有上游一份实现）；NoBackgroundSweep 置 true——库级全局缓存不为回收额外
// 起常驻 goroutine，过期条目在读路径惰性删除。
var ghAppTokenInner = cache.NewLocalCache[string](cache.LocalConfig[string]{
	TTL:               55 * time.Minute,
	NoBackgroundSweep: true,
})

// ghAppTokenCache 在内层缓存上叠一层单飞：同一 key 的并发未命中只回源一次。
//
// installation token 每次都要拿 JWT 向 GitHub 换发（一次网络往返 + GitHub 侧
// 配额），并发重复换发既浪费又会被 GitHub 限流。
var ghAppTokenCache = cache.NewSingleFlightCache(ghAppTokenInner)

// MintGitHubAppJWT 用 App 私钥签发 JWT(RS256),iss=app_id,有效期最长 10 分钟。
// 这是 GitHub App 调 API 的第一层凭证,再换 installation access token。
// 签发逻辑在平台 githubapp.MintJWT,这里只保留导出名与错误风格。
func MintGitHubAppJWT(appID int64, privateKeyPEM string) (string, error) {
	token, err := githubapp.MintJWT(appID, privateKeyPEM)
	if err != nil {
		return "", errors.Wrap(err, "mint github app jwt")
	}
	return token, nil
}

// FetchGitHubInstallationToken 用 JWT 换 installation access token。
// 平台实现带 ctx;这里沿用历史签名(ctxTimeout),包一层超时。
func FetchGitHubInstallationToken(ctxTimeout time.Duration, apiBase string, appID, installationID int64, privateKeyPEM string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	token, err := githubapp.FetchInstallationToken(ctx, apiBase, appID, installationID, privateKeyPEM)
	if err != nil {
		return "", errors.Wrap(err, "fetch github installation token")
	}
	return token, nil
}

// ResolvePlatformToken 解析平台实际使用的访问令牌。
// 优先级:GitHub App installation token(若配置) > AccessToken。
// App token 带缓存,过期前自动刷新。
func ResolvePlatformToken(p *model.Platform) (string, error) {
	if p == nil {
		return "", errors.New("platform is nil")
	}
	// GitHub App
	if p.GitHubAppID > 0 && p.GitHubInstallationID > 0 && p.GitHubPrivateKey != "" {
		return githubAppToken(p)
	}
	if p.AccessToken != "" {
		return p.AccessToken, nil
	}
	return "", nil
}

// ResolvePlatformToken Service 包装,便于壳层/业务调用。
func (s *Service) ResolvePlatformToken(p *model.Platform) (string, error) {
	return ResolvePlatformToken(p)
}

func githubAppToken(p *model.Platform) (string, error) {
	cacheKey := fmt.Sprintf("%s|%d", p.Key, p.GitHubInstallationID)
	ctx := context.Background()

	// 回源函数在调用点注入：换发 installation token 需要本次调用的 platform
	// 字段（app id / 私钥 / api url），构造期固定的 Source 拿不到它们。
	// 上游的按 key 单飞保证同一 platform 的并发调用只换发一次。
	token, err := ghAppTokenCache.GetOrLoadWithSource(ctx, cacheKey, func(context.Context, string) (*string, error) {
		fetched, err := FetchGitHubInstallationToken(15*time.Second, p.APIURL,
			p.GitHubAppID, p.GitHubInstallationID, p.GitHubPrivateKey)
		if err != nil {
			return nil, err
		}
		return &fetched, nil
	})
	if err != nil {
		return "", err
	}
	if token == nil {
		return "", errors.New("empty installation token")
	}
	return *token, nil
}

// ClearGitHubAppTokenCache 清空缓存(测试用)。
func ClearGitHubAppTokenCache() {
	ghAppTokenInner.Clear()
}
