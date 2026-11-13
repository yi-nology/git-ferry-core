package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DestinationType 冷备目的地类型。
type DestinationType string

const (
	DestS3     DestinationType = "s3"
	DestWebDAV DestinationType = "webdav"
	DestAzure  DestinationType = "azure"
	DestLocal  DestinationType = "local"
)

// Destination 一条冷备异地目的地配置(多目的地扇出,gickup 风格)。
type Destination struct {
	Type DestinationType `yaml:"type" json:"type"`
	Name string          `yaml:"name" json:"name"`
	// S3(含 MinIO/阿里 OSS)
	Endpoint  string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Region    string `yaml:"region,omitempty" json:"region,omitempty"`
	Bucket    string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Prefix    string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	AccessKey string `yaml:"access_key,omitempty" json:"access_key,omitempty"`
	SecretKey string `yaml:"secret_key,omitempty" json:"secret_key,omitempty"`
	PathStyle bool   `yaml:"path_style,omitempty" json:"path_style,omitempty"`
	// WebDAV
	URL      string `yaml:"url,omitempty" json:"url,omitempty"`
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
	// Azure Blob
	AccountName string `yaml:"account_name,omitempty" json:"account_name,omitempty"`
	AccountKey  string `yaml:"account_key,omitempty" json:"account_key,omitempty"`
	Container   string `yaml:"container,omitempty" json:"container,omitempty"`
	// Enabled 默认 true;false 时跳过
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

func (d Destination) enabled() bool {
	return d.Enabled == nil || *d.Enabled
}

// UploadResult 单目的地上传结果。
type UploadResult struct {
	Destination string `json:"destination"`
	Type        string `json:"type"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	ObjectKey   string `json:"object_key,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
}

// azureBlobPut Azure Blob PUT(Shared Key 授权)。
// 覆盖最常见的 Account Key 场景;SAS token 可直接用 WebDAV/HTTP 网关或后续扩展。
func azureBlobPut(ctx context.Context, account, accountKey, container, blob string, body []byte) error {
	if accountKey == "" {
		return fmt.Errorf("azure: account_key required")
	}
	url := fmt.Sprintf("https://%s.blob.core.windows.net/%s/%s", account, container, strings.TrimPrefix(blob, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("x-ms-version", "2020-10-02")
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("Content-Type", "application/octet-stream")

	sig := azureSharedKey(account, accountKey, req, container, blob)
	req.Header.Set("Authorization", "SharedKey "+account+":"+sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("azure put %s: status %d", blob, resp.StatusCode)
	}
	return nil
}

// azureSharedKey 构造 Azure Blob Shared Key 签名(PUT blob)。
func azureSharedKey(account, key string, req *http.Request, container, blob string) string {
	// StringToSign (2015-02-21+):
	// VERB\n Content-Encoding\n Content-Language\n Content-Length\n Content-MD5\n Content-Type\n
	// Date\n If-Modified-Since\n If-Match\n If-None-Match\n If-Unmodified-Since\n Range\n
	// CanonicalizedHeaders + CanonicalizedResource
	blob = strings.TrimPrefix(blob, "/")
	canonicalHeaders := "x-ms-blob-type:" + req.Header.Get("x-ms-blob-type") + "\n" +
		"x-ms-date:" + req.Header.Get("x-ms-date") + "\n" +
		"x-ms-version:" + req.Header.Get("x-ms-version") + "\n"
	canonicalResource := "/" + account + "/" + container + "/" + blob

	length := ""
	if req.ContentLength > 0 {
		length = fmt.Sprintf("%d", req.ContentLength)
	}
	stringToSign := strings.Join([]string{
		req.Method,
		"",     // Content-Encoding
		"",     // Content-Language
		length, // Content-Length
		"",     // Content-MD5
		req.Header.Get("Content-Type"),
		"", // Date (using x-ms-date)
		"", // If-Modified-Since
		"", // If-Match
		"", // If-None-Match
		"", // If-Unmodified-Since
		"", // Range
		canonicalHeaders,
		canonicalResource,
	}, "\n")

	rawKey, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		// 非 base64 的 key 原样使用
		rawKey = []byte(key)
	}
	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
