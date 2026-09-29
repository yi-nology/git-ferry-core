package executor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
)

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
		return "", fmt.Errorf("s3: bucket required")
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
		return "", fmt.Errorf("webdav: url required")
	}
	url := strings.TrimRight(d.URL, "/") + "/" + path.Base(objectKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(string(data)))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	if d.Username != "" {
		req.SetBasicAuth(d.Username, d.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("webdav put %s: status %d", url, resp.StatusCode)
	}
	return url, nil
}

type azureUploader struct{}

func (azureUploader) Type() DestinationType { return DestAzure }
func (azureUploader) Upload(ctx context.Context, d Destination, objectKey string, data []byte) (string, error) {
	if d.AccountName == "" || d.Container == "" {
		return "", fmt.Errorf("azure: account_name and container required")
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
		return "", fmt.Errorf("local: dir (url field) required")
	}
	dst := strings.TrimRight(d.URL, "/") + "/" + path.Base(objectKey)
	if err := os.WriteFile(dst, data, 0o640); err != nil {
		return "", err
	}
	return dst, nil
}
