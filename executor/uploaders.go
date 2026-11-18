package executor

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/httpjson"
	"github.com/oekit/pkg/pathx"
)

// uploadHTTPClient 出站上传专用客户端:统一超时,避免默认客户端无限挂起。
var uploadHTTPClient = &http.Client{Timeout: 5 * time.Minute}

// putViaHTTPJSON 统一出站 PUT:oekit/pkg/httpjson 负责 ctx 感知请求、
// 限容读体与状态码可见性;调用方只准备 URL/body/headers。
func putViaHTTPJSON(ctx context.Context, url string, body []byte, prepare func(http.Header)) error {
	resp, err := httpjson.DoRaw(ctx, uploadHTTPClient, httpjson.Request{
		Method: http.MethodPut,
		URL:    url,
		Body:   body,
		Header: prepare,
	}, 1<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return errors.New(fmt.Sprintf("PUT %s: status %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(resp.Body))))
	}
	return nil
}

// Uploader 单目的地上传策略(注册表驱动,新增目的地只需实现此接口)。
type Uploader interface {
	Type() DestinationType
	// Upload 返回 object key;失败返回 error。
	Upload(ctx context.Context, d Destination, objectKey string, data []byte) (string, error)
}

// uploaderRegistry 目的地类型 → 上传策略。
type uploaderRegistry map[DestinationType]Uploader

var uploaders = uploaderRegistry{
	DestS3:     s3Uploader{},
	DestWebDAV: webdavUploader{},
	DestAzure:  azureUploader{},
	DestLocal:  localUploader{},
}

// FanoutUpload 把本地文件扇出到多个目的地(策略注册表分发)。
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
		name := d.Name
		if name == "" {
			name = string(d.Type)
		}
		res := UploadResult{Destination: name, Type: string(d.Type)}
		up, ok := uploaders[d.Type]
		if d.Type == "" {
			up, ok = uploaders[DestS3]
		}
		if !ok {
			res.Error = "unknown destination type: " + string(d.Type)
			out = append(out, res)
			continue
		}
		key := base
		if d.Prefix != "" && d.Type != DestWebDAV {
			key = strings.Trim(d.Prefix, "/") + "/" + base
		}
		gotKey, err := up.Upload(ctx, d, key, data)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
			res.ObjectKey = gotKey
			res.Bytes = int64(len(data))
		}
		out = append(out, res)
	}
	return out
}

// ===== 策略实现 =====

type s3Uploader struct{}

func (s3Uploader) Type() DestinationType { return DestS3 }
func (s3Uploader) Upload(ctx context.Context, d Destination, objectKey string, data []byte) (string, error) {
	if d.Bucket == "" {
		return "", errors.New("s3: bucket required")
	}
	cfg := &S3Config{
		Endpoint:  d.Endpoint,
		Region:    d.Region,
		Bucket:    d.Bucket,
		Prefix:    d.Prefix,
		AccessKey: d.AccessKey,
		SecretKey: d.SecretKey,
		PathStyle: d.PathStyle,
	}
	// objectKey 已含 prefix
	cfg.Prefix = ""
	if err := s3PutObject(ctx, cfg, objectKey, data); err != nil {
		return "", err
	}
	return objectKey, nil
}

type webdavUploader struct{}

func (webdavUploader) Type() DestinationType { return DestWebDAV }
func (webdavUploader) Upload(ctx context.Context, d Destination, objectKey string, data []byte) (string, error) {
	if d.URL == "" {
		return "", errors.New("webdav: url required")
	}
	url := pathx.JoinURL(d.URL, path.Base(objectKey))
	err := putViaHTTPJSON(ctx, url, data, func(h http.Header) {
		h.Set("Content-Type", "application/octet-stream")
		if d.Username != "" {
			h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.Username+":"+d.Password)))
		}
	})
	if err != nil {
		return "", err
	}
	return url, nil
}

type azureUploader struct{}

func (azureUploader) Type() DestinationType { return DestAzure }
func (azureUploader) Upload(ctx context.Context, d Destination, objectKey string, data []byte) (string, error) {
	if d.AccountName == "" || d.Container == "" {
		return "", errors.New("azure: account_name and container required")
	}
	if err := azureBlobPut(ctx, d.AccountName, d.AccountKey, d.Container, objectKey, data); err != nil {
		return "", err
	}
	return objectKey, nil
}

type localUploader struct{}

func (localUploader) Type() DestinationType { return DestLocal }
func (localUploader) Upload(_ context.Context, d Destination, objectKey string, data []byte) (string, error) {
	if d.URL == "" {
		return "", errors.New("local: dir (url field) required")
	}
	dst := strings.TrimRight(d.URL, "/") + "/" + path.Base(objectKey)
	if err := os.WriteFile(dst, data, 0o640); err != nil {
		return "", err
	}
	return dst, nil
}
