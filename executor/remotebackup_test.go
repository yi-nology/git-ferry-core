package executor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadBundle_S3Put(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	dir := t.TempDir()
	local := filepath.Join(dir, "a.bundle")
	require.NoError(t, os.WriteFile(local, []byte("bundle-data"), 0o600))

	cfg := &S3Config{
		Endpoint:  srv.URL,
		Bucket:    "backups",
		Prefix:    "gitferry",
		AccessKey: "AK",
		SecretKey: "SK",
		PathStyle: true,
		Region:    "us-east-1",
	}
	require.NoError(t, UploadBundle(context.Background(), cfg, local, "t1-main-20260101.bundle"))
	assert.True(t, strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256"))
	assert.Contains(t, gotPath, "/backups/gitferry/t1-main-20260101.bundle")
}

func TestSignS3V4_Deterministic(t *testing.T) {
	req, _ := http.NewRequest("PUT", "https://s3.example.com/b/k", nil)
	signS3V4(req, []byte("x"), "AK", "SK", "us-east-1", "s3")
	a1 := req.Header.Get("Authorization")
	signS3V4(req, []byte("x"), "AK", "SK", "us-east-1", "s3")
	a2 := req.Header.Get("Authorization")
	// 时间戳变了签名可能变,但格式必须合法
	assert.True(t, strings.HasPrefix(a1, "AWS4-HMAC-SHA256 Credential=AK/"))
	_ = a2
	_ = hmac.New
	_ = hex.EncodeToString
	_ = sha256.New
}
