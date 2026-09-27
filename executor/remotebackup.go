package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// S3Config S3 兼容对象存储(阿里云 OSS/MinIO/AWS)。
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`   // https://s3.amazonaws.com 或 https://oss-cn-hangzhou.aliyuncs.com
	Region    string `yaml:"region"`     // us-east-1 等
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"`     // 对象 key 前缀
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// PathStyle true=MinIO/部分兼容端点
	PathStyle bool `yaml:"path_style"`
}

// UploadBundle 把 bundle 文件推到 S3(冷备异地)。
// 只支持 path-style + SigV4,覆盖 MinIO/阿里 OSS/AWS。
func UploadBundle(ctx context.Context, cfg *S3Config, localPath, objectKey string) error {
	if cfg == nil || cfg.Bucket == "" {
		return fmt.Errorf("s3 config incomplete")
	}
	data, err := os.ReadFile(localPath) //nolint:gosec // 本地冷备路径由部署方配置
	if err != nil {
		return fmt.Errorf("read bundle: %w", err)
	}
	if cfg.Prefix != "" {
		objectKey = strings.Trim(cfg.Prefix, "/") + "/" + objectKey
	}
	return s3PutObject(ctx, cfg, objectKey, data)
}

func s3PutObject(ctx context.Context, cfg *S3Config, key string, body []byte) error {
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	var url string
	if cfg.PathStyle {
		url = fmt.Sprintf("%s/%s/%s", endpoint, cfg.Bucket, strings.TrimPrefix(key, "/"))
	} else {
		// virtual-host style
		u := strings.TrimPrefix(endpoint, "https://")
		u = strings.TrimPrefix(u, "http://")
		scheme := "https"
		if strings.HasPrefix(endpoint, "http://") {
			scheme = "http"
		}
		url = fmt.Sprintf("%s://%s.%s/%s", scheme, cfg.Bucket, u, strings.TrimPrefix(key, "/"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/octet-stream")

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	signS3V4(req, body, cfg.AccessKey, cfg.SecretKey, region, "s3")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("s3 put %s: status %d", key, resp.StatusCode)
	}
	return nil
}

// signS3V4 AWS Signature Version 4(简化:只覆盖 PUT object)。
func signS3V4(req *http.Request, payload []byte, accessKey, secretKey, region, service string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", sha256Hex(payload))

	// canonical headers
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		host, sha256Hex(payload), amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := req.URL.RawQuery

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		sha256Hex(payload),
	}, "\n")

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := s3SigningKey(secretKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func s3SigningKey(secret, dateStamp, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}
