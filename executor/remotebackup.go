package executor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/oekit/pkg/encoding"
	"github.com/oekit/pkg/errors"
)

// S3Config S3 兼容对象存储(阿里云 OSS/MinIO/AWS)。
type S3Config struct {
	Endpoint  string `yaml:"endpoint"` // https://s3.amazonaws.com 或 https://oss-cn-hangzhou.aliyuncs.com
	Region    string `yaml:"region"`   // us-east-1 等
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"` // 对象 key 前缀
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	// PathStyle true=MinIO/部分兼容端点
	PathStyle bool `yaml:"path_style"`
}

// UploadBundle 把 bundle 文件推到 S3(冷备异地)。
// 只支持 path-style + SigV4,覆盖 MinIO/阿里 OSS/AWS。
func UploadBundle(ctx context.Context, cfg *S3Config, localPath, objectKey string) error {
	if cfg == nil || cfg.Bucket == "" {
		return errors.New("s3 config incomplete")
	}
	data, err := os.ReadFile(localPath) //nolint:gosec // 本地冷备路径由部署方配置
	if err != nil {
		return errors.Wrap(err, "read bundle")
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
	var objectURL string
	if cfg.PathStyle {
		objectURL = fmt.Sprintf("%s/%s/%s", endpoint, cfg.Bucket, strings.TrimPrefix(key, "/"))
	} else {
		// virtual-host style
		u := strings.TrimPrefix(endpoint, "https://")
		u = strings.TrimPrefix(u, "http://")
		scheme := "https"
		if strings.HasPrefix(endpoint, "http://") {
			scheme = "http"
		}
		objectURL = fmt.Sprintf("%s://%s.%s/%s", scheme, cfg.Bucket, u, strings.TrimPrefix(key, "/"))
	}

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	u, err := url.Parse(objectURL)
	if err != nil {
		return err
	}
	return putViaHTTPJSON(ctx, objectURL, body, func(h http.Header) {
		h.Set("Content-Type", "application/octet-stream")
		signS3V4Header(h, http.MethodPut, u, body, cfg.AccessKey, cfg.SecretKey, region, "s3")
	})
}

// signS3V4Header AWS Signature Version 4(简化:只覆盖 PUT object)。
func signS3V4Header(h http.Header, method string, u *url.URL, payload []byte, accessKey, secretKey, region, service string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	h.Set("X-Amz-Date", amzDate)
	h.Set("X-Amz-Content-Sha256", encoding.Sha256Hex(payload))

	// canonical headers
	host := u.Host
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		host, encoding.Sha256Hex(payload), amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalURI := u.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := u.RawQuery

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		encoding.Sha256Hex(payload),
	}, "\n")

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		encoding.Sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := s3SigningKey(secretKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	h.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature))
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
