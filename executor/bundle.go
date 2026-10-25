package executor

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
)

// writeBundle 冷备:把同步后的分支打成 git bundle(单文件、可校验、可异地存储)。
// 借鉴 restic/borg 的「可搬运归档」思路,但按 git 原生 bundle 保留全部引用,
// 不引入新的存储格式,恢复时 `git clone bundle` 即可。
//
// 命名: <taskKey>-<branch>-<yyyyMMdd-HHmmss>.bundle
// BackupFormat=zip 时改为打包 workdir 为 .zip(gickup 风格,便于人工取件)。
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
	format := ""
	if cfg := e.service.GetConfig(); cfg != nil {
		format = cfg.Sync.BackupFormat
	}
	if strings.EqualFold(format, "zip") {
		zipName := strings.TrimSuffix(name, ".bundle") + ".zip"
		zipPath := filepath.Join(backupDir, zipName)
		if err := zipDir(repoDir, zipPath); err != nil {
			return "", errors.Wrap(err, "zip archive")
		}
		fmt.Fprintf(details, "  archive: %s\n", zipPath)
		return zipPath, nil
	}

	// 打当前同步分支 + HEAD(缺 HEAD 则 clone 后无法 checkout) + 标签
	//
	// **多分支(glob)不能把 glob 直接塞进 refspec**:"refs/heads/release/*"
	// 不是合法 rev ⇒ git bundle create 必然失败(退出码 128)⇒
	// 多分支任务**永远产不出冷备**,而 bundle 阶段是 optional(失败只进 details)
	// ⇒ 同步仍报成功、冷备静默停止,RPO 指标却以为冷备在正常跑。
	// 这里按 glob 展开成实际分支列表(逐个作为 rev)。
	refs := []string{"refs/heads/" + task.SourceBranch}
	if strings.ContainsAny(task.SourceBranch, "*?[") {
		expanded, err := e.expandBundleRefs(ctx, repoDir, task)
		if err != nil {
			return "", err
		}
		refs = expanded
	}
	args := []string{"bundle", "create", outPath}
	args = append(args, refs...)
	args = append(args, "HEAD")
	if task.GitTags {
		args = append(args, "--tags")
	}
	if _, err := e.gitOutput(ctx, repoDir, args...); err != nil {
		return "", errors.Wrap(err, "git bundle create")
	}
	fmt.Fprintf(details, "  bundle: %s\n", outPath)
	return outPath, nil
}

// zipDir 将目录打包为 zip(相对路径,便于解压还原)。
func zipDir(srcDir, dest string) error {
	f, err := os.Create(dest) //nolint:gosec // dest 由 backupDir+任务 key 构造
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	defer func() { _ = zw.Close() }()
	return filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, p)
		if rerr != nil {
			return rerr
		}
		w, cerr := zw.Create(rel)
		if cerr != nil {
			return cerr
		}
		data, rerr := os.ReadFile(p) //nolint:gosec // workdir 内文件
		if rerr != nil {
			return rerr
		}
		_, werr := w.Write(data)
		return werr
	})
}

// expandBundleRefs 把 glob 分支模式展开成实际的 refs/heads/<b> 列表。
func (e *Executor) expandBundleRefs(ctx context.Context, repoDir string, task *model.SyncTask) ([]string, error) {
	branches, err := e.listLocalBranches(repoDir)
	if err != nil {
		return nil, errors.Wrap(err, "list local branches for bundle")
	}
	var refs []string
	for _, b := range branches {
		if !matchBranchGlob(task.SourceBranch, b) {
			continue
		}
		refs = append(refs, "refs/heads/"+b)
	}
	if len(refs) == 0 {
		return nil, errors.Errorf("no local branches match %q", task.SourceBranch)
	}
	return refs, nil
}

// rotateBundles 每任务保留最近 keep 份,按文件名时间戳排序删除旧的。
// 借鉴 gickup zip keep N:防冷备目录无限膨胀。
func rotateBundles(backupDir, taskKey string, keep int) (removed int, err error) {
	if keep <= 0 {
		return 0, nil
	}
	prefix := strutil.SanitizeFileToken(taskKey) + "-"
	entries, rerr := os.ReadDir(backupDir)
	if rerr != nil {
		return 0, rerr
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".bundle") && !strings.HasSuffix(e.Name(), ".zip") &&
			!strings.HasSuffix(e.Name(), ".bundle.enc") && !strings.HasSuffix(e.Name(), ".zip.enc") {
			continue
		}
		names = append(names, e.Name())
	}
	// 文件名含时间戳 yyyyMMdd-HHmmss,字典序即时间序
	if len(names) <= keep {
		return 0, nil
	}
	// 升序:最早在前(文件名含时间戳,字典序即时间序)
	sort.Strings(names)
	var failures []string
	for _, name := range names[:len(names)-keep] {
		if rerr := os.Remove(filepath.Join(backupDir, name)); rerr != nil {
			// 删除失败**不计数也不沉默**:否则"轮转在生效"是假的,
			// 冷备目录照样无限涨(而运维以为有上限)。
			failures = append(failures, name+": "+rerr.Error())
			continue
		}
		removed++
	}
	if len(failures) > 0 {
		return removed, errors.Errorf("轮转删除失败 %d 项:%s", len(failures), strings.Join(failures, "; "))
	}
	return removed, nil
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

	// 2) 组装目的地列表(BackupDestinations)
	var dests []Destination
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
