package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/githubapp"
)

// githubAppTokenCache 缓存 installation token(有效期约 1h,提前 5min 刷新)。
type githubAppTokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken // key: platformKey|installationID
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

var ghAppCache = &githubAppTokenCache{tokens: map[string]cachedToken{}}

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
	ghAppCache.mu.Lock()
	if c, ok := ghAppCache.tokens[cacheKey]; ok && time.Now().Before(c.expiresAt.Add(-5*time.Minute)) {
		ghAppCache.mu.Unlock()
		return c.token, nil
	}
	ghAppCache.mu.Unlock()

	token, err := FetchGitHubInstallationToken(15*time.Second, p.APIURL,
		p.GitHubAppID, p.GitHubInstallationID, p.GitHubPrivateKey)
	if err != nil {
		return "", err
	}
	ghAppCache.mu.Lock()
	ghAppCache.tokens[cacheKey] = cachedToken{
		token:     token,
		expiresAt: time.Now().Add(55 * time.Minute), // GitHub token 有效 1h
	}
	ghAppCache.mu.Unlock()
	return token, nil
}

// ClearGitHubAppTokenCache 清空缓存(测试用)。
func ClearGitHubAppTokenCache() {
	ghAppCache.mu.Lock()
	ghAppCache.tokens = map[string]cachedToken{}
	ghAppCache.mu.Unlock()
}
