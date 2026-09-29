package service

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
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
func MintGitHubAppJWT(appID int64, privateKeyPEM string) (string, error) {
	if appID == 0 {
		return "", errors.New("github app id is empty")
	}
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", errors.Wrap(err, "parse github app private key")
	}
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadMap := map[string]any{
		"iss": fmt.Sprintf("%d", appID),
		"iat": now.Unix() - 30, // 时钟偏移容忍
		"exp": now.Add(9 * time.Minute).Unix(),
	}
	payloadJSON, err := json.Marshal(payloadMap)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.Wrap(err, "sign jwt")
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// FetchGitHubInstallationToken 用 JWT 换 installation access token。
func FetchGitHubInstallationToken(ctxTimeout time.Duration, apiBase string, appID, installationID int64, privateKeyPEM string) (string, error) {
	jwt, err := MintGitHubAppJWT(appID, privateKeyPEM)
	if err != nil {
		return "", err
	}
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens",
		strings.TrimRight(apiBase, "/"), installationID)

	client := &http.Client{Timeout: ctxTimeout}
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "request installation token")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return "", errors.Newf("installation token: status %d", resp.StatusCode)
	}
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", errors.Wrap(err, "decode installation token")
	}
	if body.Token == "" {
		return "", errors.New("installation token empty")
	}
	return body.Token, nil
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

// parseRSAPrivateKey 解析 PKCS#1/PKCS#8 PEM RSA 私钥。
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}

// ClearGitHubAppTokenCache 清空缓存(测试用)。
func ClearGitHubAppTokenCache() {
	ghAppCache.mu.Lock()
	ghAppCache.tokens = map[string]cachedToken{}
	ghAppCache.mu.Unlock()
}
