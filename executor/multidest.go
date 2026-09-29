package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path"
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

// FanoutUpload 把本地文件扇出到多个目的地(本地目的地=复制到 dir)。
// 逐个独立尝试,失败不阻断其它目的地;返回逐目的地结果。
func FanoutUpload(ctx context.Context, localPath string, dests []Destination) []UploadResult {
	data, err := os.ReadFile(localPath) //nolint:gosec // 本地冷备路径由部署方配置
	if err != nil {
		return []UploadResult{{
			Destination: "source",
			Type:        string(DestLocal),
			OK:          false,
			Error:       "read local: " + err.Error(),
		}}
	}
	base := path.Base(localPath)
	out := make([]UploadResult, 0, len(dests))
	for _, d := range dests {
		if !d.enabled() {
			continue
		}
		res := UploadResult{Destination: d.Name, Type: string(d.Type)}
		if res.Destination == "" {
			res.Destination = string(d.Type)
		}
		switch d.Type {
		case DestS3, "":
			if d.Bucket == "" {
				res.Error = "s3: bucket required"
			} else {
				cfg := &S3Config{
					Endpoint:  d.Endpoint,
					Region:    d.Region,
					Bucket:    d.Bucket,
					Prefix:    d.Prefix,
					AccessKey: d.AccessKey,
					SecretKey: d.SecretKey,
					PathStyle: d.PathStyle,
				}
				key := base
				if err := s3PutObject(ctx, cfg, key, data); err != nil {
					res.Error = err.Error()
				} else {
					res.OK = true
					res.ObjectKey = key
					res.Bytes = int64(len(data))
				}
			}
		case DestWebDAV:
			if d.URL == "" {
				res.Error = "webdav: url required"
			} else {
				key := strings.TrimRight(d.URL, "/") + "/" + base
				if err := webdavPut(ctx, key, d.Username, d.Password, data); err != nil {
					res.Error = err.Error()
				} else {
					res.OK = true
					res.ObjectKey = key
					res.Bytes = int64(len(data))
				}
			}
		case DestAzure:
			if d.AccountName == "" || d.Container == "" {
				res.Error = "azure: account_name and container required"
			} else {
				key := base
				if d.Prefix != "" {
					key = strings.Trim(d.Prefix, "/") + "/" + base
				}
				if err := azureBlobPut(ctx, d.AccountName, d.AccountKey, d.Container, key, data); err != nil {
					res.Error = err.Error()
				} else {
					res.OK = true
					res.ObjectKey = key
					res.Bytes = int64(len(data))
				}
			}
		case DestLocal:
			if d.URL == "" {
				res.Error = "local: dir (url field) required"
			} else {
				dst := strings.TrimRight(d.URL, "/") + "/" + base
				if err := os.WriteFile(dst, data, 0o640); err != nil {
					res.Error = err.Error()
				} else {
					res.OK = true
					res.ObjectKey = dst
					res.Bytes = int64(len(data))
				}
			}
		default:
			res.Error = "unknown destination type: " + string(d.Type)
		}
		out = append(out, res)
	}
	return out
}

// webdavPut WebDAV PUT(Basic Auth)。
func webdavPut(ctx context.Context, url, user, pass string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webdav put %s: status %d", url, resp.StatusCode)
	}
	return nil
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
