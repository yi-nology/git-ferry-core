package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"

	_ "github.com/yi-nology/go-git-platform/backends/all"
)

// TestResolveRepoProvider_GitHubAppOnlyPlatform 钉住「仓库级 provider 必须认 GitHub App」。
//
// 回归背景:resolveRepoProvider 以前直接
// `providerMgr.Get(providerConfig(platform, platform.AccessToken, hooks))`
// —— 绕过了 providerFromManager 里的 ResolvePlatformToken
// ⇒ **只配了 GitHub App(没有 PAT)的平台拿到空 token**
// ⇒ 仓库级 API 调用(webhook 注册 / webhook 事件处理)全部 401。
// 其余 provider 路径(platformProvider / ProviderForPlatform)都走
// ResolvePlatformToken ⇒ 只有仓库这一条是例外。
//
// 这条用例是**反向验证**:把 resolveRepoProvider 改回直接读 platform.AccessToken,
// 本用例会变红(请求头里没有 installation token)。
func TestResolveRepoProvider_GitHubAppOnlyPlatform(t *testing.T) {
	ClearGitHubAppTokenCache()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	pemStr, appID := testRSAPrivateKeyPEM(t)

	var mu sync.Mutex
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/app/installations/99/access_tokens"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_installation_token_repo",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case strings.Contains(r.URL.Path, "repos"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "tester"})
		}
	}))
	defer srv.Close()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Platform{}, &model.Repo{}))

	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)

	// **只配 GitHub App,没有 PAT** ⇒ platform.AccessToken 为空。
	// 这正是旧实现拿到空 token 的场景。
	platform := &model.Platform{
		Key: "gh-app-only", Name: "GitHub App Only", Type: "github",
		APIURL:               srv.URL + "/",
		GitHubAppID:          appID,
		GitHubInstallationID: 99,
		GitHubPrivateKey:     pemStr,
	}
	require.NoError(t, platformDAO.Create(platform))
	require.Empty(t, platform.AccessToken, "本用例的前提:平台没有 PAT")

	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "app-only-repo", Name: "demo",
		PlatformID: platform.ID, Platform: platform.Type,
		PlatformOwner: "octo", PlatformRepo: "demo",
		CloneURL: srv.URL + "/octo/demo.git",
		Status:   model.RepoStatusActive,
	}))

	svc := &Service{repos: NewRepoService(repoDAO, platformDAO, sdkprov.NewManager(0))}

	_, prov, err := svc.repos.GetRepoWithProvider("app-only-repo")
	require.NoError(t, err)
	require.NotNil(t, prov)

	// 让 provider 真发一次请求,把请求头抓下来
	_, err = prov.TestConnection(context.Background())
	require.NoError(t, err)

	mu.Lock()
	headers := append([]string(nil), authHeaders...)
	mu.Unlock()

	var sawInstallationToken bool
	for _, h := range headers {
		if strings.Contains(h, "ghs_installation_token_repo") {
			sawInstallationToken = true
		}
	}
	assert.True(t, sawInstallationToken,
		"只配 GitHub App 的平台,仓库级 provider 必须带 installation token;实际请求头=%v", headers)
}
