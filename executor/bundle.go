package executor

import (
	"context"
	"fmt"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
)

// writeBundle 冷备:把同步后的分支打成 git bundle(单文件、可校验、可异地存储)。
// 借鉴 restic/borg 的「可搬运归档」思路,但按 git 原生 bundle 保留全部引用,
// 不引入新的存储格式,恢复时 `git clone bundle` 即可。
//
// 命名: <taskKey>-<branch>-<yyyyMMdd-HHmmss>.bundle
func (e *Executor) writeBundle(ctx context.Context, workDir, backupDir string, task *model.SyncTask, details *strings.Builder) (string, error) {
	if backupDir == "" {
		return "", errors.New("backup dir not configured")
	}
	if err := os.MkdirAll(backupDir, 0o750); err != nil {
		return "", errors.Wrap(err, "create backup dir")
	}
	name := fmt.Sprintf("%s-%s-%s.bundle",
		strutil.SanitizeFileToken(task.Key),
		strutil.SanitizeFileToken(task.TargetBranch),
		time.Now().Format("20060102-150405"))
	outPath := filepath.Join(backupDir, name)

	repoDir := filepath.Join(workDir, RepoDir)
	// 打当前同步分支 + HEAD(缺 HEAD 则 clone 后无法 checkout) + 标签
	ref := "refs/heads/" + task.SourceBranch
	args := []string{"bundle", "create", outPath, ref, "HEAD"}
	if task.GitTags {
		args = append(args, "--tags")
	}
	if _, err := e.gitOutput(ctx, repoDir, args...); err != nil {
		return "", errors.Wrap(err, "git bundle create")
	}
	fmt.Fprintf(details, "  bundle: %s\n", outPath)
	return outPath, nil
}

// rotateBundles 每任务保留最近 keep 份,按文件名时间戳排序删除旧的。
// 借鉴 gickup zip keep N:防冷备目录无限膨胀。
func rotateBundles(backupDir, taskKey string, keep int) (removed int) {
	if keep <= 0 {
		return 0
	}
	prefix := strutil.SanitizeFileToken(taskKey) + "-"
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return 0
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".bundle") {
			continue
		}
		names = append(names, e.Name())
	}
	// 文件名含时间戳 yyyyMMdd-HHmmss,字典序即时间序
	if len(names) <= keep {
		return 0
	}
	// 升序:最早在前
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	for _, name := range names[:len(names)-keep] {
		if os.Remove(filepath.Join(backupDir, name)) == nil {
			removed++
		}
	}
	return removed
}

// fanoutBundle 冷备完成后:可选 AES-GCM 加密 + 多目的地扇出上传。
// 失败只记 details,不影响同步主流程成功状态(冷备是附加能力)。
func (e *Executor) fanoutBundle(ctx context.Context, bundlePath string, cfg *model.Config, details *strings.Builder) {
	// 1) 可选加密
	uploadPath := bundlePath
	if cfg.Sync.BackupEncryptKey != "" {
		encPath, err := EncryptFile(bundlePath, cfg.Sync.BackupEncryptKey)
		if err != nil {
			fmt.Fprintf(details, "  bundle encrypt error: %v\n", err)
		} else {
			uploadPath = encPath
			fmt.Fprintf(details, "  bundle encrypted: %s\n", filepath.Base(encPath))
		}
	}

	// 2) 组装目的地列表(legacy BackupS3 + BackupDestinations)
	var dests []Destination
	if cfg.Sync.BackupS3.Bucket != "" {
		dests = append(dests, Destination{
			Type:      DestS3,
			Name:      "s3",
			Endpoint:  cfg.Sync.BackupS3.Endpoint,
			Region:    cfg.Sync.BackupS3.Region,
			Bucket:    cfg.Sync.BackupS3.Bucket,
			Prefix:    cfg.Sync.BackupS3.Prefix,
			AccessKey: cfg.Sync.BackupS3.AccessKey,
			SecretKey: cfg.Sync.BackupS3.SecretKey,
			PathStyle: cfg.Sync.BackupS3.PathStyle,
		})
	}
	for _, d := range cfg.Sync.BackupDestinations {
		dests = append(dests, Destination{
			Type:        DestinationType(d.Type),
			Name:        d.Name,
			Endpoint:    d.Endpoint,
			Region:      d.Region,
			Bucket:      d.Bucket,
			Prefix:      d.Prefix,
			AccessKey:   d.AccessKey,
			SecretKey:   d.SecretKey,
			PathStyle:   d.PathStyle,
			URL:         d.URL,
			Username:    d.Username,
			Password:    d.Password,
			AccountName: d.AccountName,
			AccountKey:  d.AccountKey,
			Container:   d.Container,
			Enabled:     d.Enabled,
		})
	}
	if len(dests) == 0 {
		return
	}

	results := FanoutUpload(ctx, uploadPath, dests)
	okN, failN := 0, 0
	for _, r := range results {
		if r.OK {
			okN++
			fmt.Fprintf(details, "  upload %s (%s): ok (%d bytes)\n", r.Destination, r.Type, r.Bytes)
		} else {
			failN++
			fmt.Fprintf(details, "  upload %s (%s): FAILED: %s\n", r.Destination, r.Type, r.Error)
		}
	}
	fmt.Fprintf(details, "  fanout: %d ok, %d failed\n", okN, failN)
}
