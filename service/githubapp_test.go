package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
)

func testRSAPrivateKeyPEM(t *testing.T) (string, int64) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der := x509.MarshalPKCS1PrivateKey(key)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
	return pemStr, 12345
}

func TestMintGitHubAppJWT(t *testing.T) {
	pemStr, appID := testRSAPrivateKeyPEM(t)
	token, err := MintGitHubAppJWT(appID, pemStr)
	require.NoError(t, err)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	assert.Contains(t, string(header), "RS256")

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(payload, &claims))
	assert.Equal(t, "12345", claims["iss"])
	// exp 在约 9 分钟后
	exp, ok := claims["exp"].(float64)
	require.True(t, ok)
	assert.InDelta(t, time.Now().Add(9*time.Minute).Unix(), int64(exp), 30)
}

func TestMintGitHubAppJWT_BadKey(t *testing.T) {
	_, err := MintGitHubAppJWT(1, "not-a-pem")
	assert.Error(t, err)
	_, err = MintGitHubAppJWT(0, "x")
	assert.Error(t, err)
}

func TestParseRSAPrivateKey_PKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	parsed, err := parseRSAPrivateKey(pemStr)
	require.NoError(t, err)
	assert.Equal(t, key.N, parsed.N)
}

func TestResolvePlatformToken_PrefersAccessTokenWithoutApp(t *testing.T) {
	ClearGitHubAppTokenCache()
	p := &model.Platform{Key: "gh", AccessToken: "ghp_xxx"}
	tok, err := ResolvePlatformToken(p)
	require.NoError(t, err)
	assert.Equal(t, "ghp_xxx", tok)
}

func TestResolvePlatformToken_Nil(t *testing.T) {
	_, err := ResolvePlatformToken(nil)
	assert.Error(t, err)
}
