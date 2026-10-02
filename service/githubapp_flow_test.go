package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

// TestGitHubApp_FullFlow_JWT_to_Token 全链路:私钥 → JWT → installation token → 缓存。
func TestGitHubApp_FullFlow_JWT_to_Token(t *testing.T) {
	ClearGitHubAppTokenCache()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))

	var calls int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotAuth = r.Header.Get("Authorization")
		require.Equal(t, http.MethodPost, r.Method)
		require.True(t, strings.HasSuffix(r.URL.Path, "/app/installations/99/access_tokens"),
			"path=%s", r.URL.Path)
		require.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installation_token_abc",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	// 1) JWT 可解析且 iss 正确
	jwt, err := MintGitHubAppJWT(42, pemStr)
	require.NoError(t, err)
	require.Len(t, strings.Split(jwt, "."), 3)

	// 2) 换 installation token,请求头是 Bearer <JWT>
	tok, err := FetchGitHubInstallationToken(5*time.Second, srv.URL, 42, 99, pemStr)
	require.NoError(t, err)
	assert.Equal(t, "ghs_installation_token_abc", tok)
	assert.True(t, strings.HasPrefix(gotAuth, "Bearer "))
	assert.NotContains(t, gotAuth, "ghs_") // 用的是 JWT 不是上一次的 token

	// 3) ResolvePlatformToken 走缓存:第二次不再打 API
	p := &model.Platform{
		Key:                  "gh",
		Type:                 "github",
		APIURL:               srv.URL,
		GitHubAppID:          42,
		GitHubInstallationID: 99,
		GitHubPrivateKey:     pemStr,
	}
	tok2, err := ResolvePlatformToken(p)
	require.NoError(t, err)
	assert.Equal(t, "ghs_installation_token_abc", tok2)
	// Fetch(直调) + ResolvePlatformToken(缓存未命中)= 2 次
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))

	tok3, err := ResolvePlatformToken(p)
	require.NoError(t, err)
	assert.Equal(t, tok2, tok3)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "缓存命中不应再次请求")
}

// TestGitHubApp_ErrorStatus 联调失败路径:非 2xx 不落缓存。
func TestGitHubApp_ErrorStatus(t *testing.T) {
	ClearGitHubAppTokenCache()
	pemStr, _ := testRSAPrivateKeyPEM(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := FetchGitHubInstallationToken(2*time.Second, srv.URL, 1, 2, pemStr)
	assert.Error(t, err)

	// 失败不应缓存;重配可用服务后能成功
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ok2"})
	}))
	defer srv2.Close()
	p := &model.Platform{
		Key: "gh2", APIURL: srv2.URL,
		GitHubAppID: 1, GitHubInstallationID: 2, GitHubPrivateKey: pemStr,
	}
	tok, err := ResolvePlatformToken(p)
	require.NoError(t, err)
	assert.Equal(t, "ok2", tok)
}

// TestGitHubApp_UsedByPlatformProvider 认证配置拼装:token 进 provider.Config。
func TestGitHubApp_UsedByPlatformProvider(t *testing.T) {
	ClearGitHubAppTokenCache()
	pemStr, _ := testRSAPrivateKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "provider_token"})
	}))
	defer srv.Close()

	p := &model.Platform{
		Key: "ghp", Type: "github", APIURL: srv.URL,
		GitHubAppID: 7, GitHubInstallationID: 8, GitHubPrivateKey: pemStr,
		AccessToken: "fallback_pat",
	}
	tok, err := ResolvePlatformToken(p)
	require.NoError(t, err)
	assert.Equal(t, "provider_token", tok, "GitHub App 优先于 PAT")

	cfg := providerConfig(p, tok, nil)
	assert.Equal(t, "provider_token", cfg.Token)
	assert.Equal(t, "github", string(cfg.Platform))
}

// TestMintJWT_HeaderPayload 解析完整 JWT 三段内容。
func TestMintJWT_HeaderPayload(t *testing.T) {
	pemStr, appID := testRSAPrivateKeyPEM(t)
	token, err := MintGitHubAppJWT(appID, pemStr)
	require.NoError(t, err)
	parts := strings.Split(token, ".")
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	assert.Contains(t, string(header), "RS256")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(payload, &claims))
	assert.Equal(t, "12345", claims["iss"])
}
