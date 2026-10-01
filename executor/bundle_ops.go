package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"github.com/yi-nology/go-git-platform/gitbackend"

	errors "github.com/cockroachdb/errors"
)

// BundleInfo bundle 文件信息。
type BundleInfo struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	TaskKey  string    `json:"task_key,omitempty"`
	Valid    bool      `json:"valid,omitempty"`
	RefSpecs []string  `json:"ref_specs,omitempty"`
}

// ListBundles 列出 backupDir 下的 bundle(按任务过滤,空=全部)。
func ListBundles(backupDir, taskKey string) ([]BundleInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BundleInfo{}, nil
		}
		return nil, err
	}
	prefix := ""
	if taskKey != "" {
		prefix = strutil.SanitizeFileToken(taskKey) + "-"
	}
	out := []BundleInfo{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bundle") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BundleInfo{
			Name:    e.Name(),
			Path:    filepath.Join(backupDir, e.Name()),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
			TaskKey: taskKey,
		})
	}
	// 时间倒序(新在前)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].ModTime.After(out[i].ModTime) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// VerifyBundle 校验 bundle 完整性(git bundle verify)。
// 返回列出的引用与是否有效。
func VerifyBundle(ctx context.Context, bundlePath string) (*BundleInfo, error) {
	if _, err := os.Stat(bundlePath); err != nil {
		return nil, errors.Wrap(err, "bundle not found")
	}
	// git bundle verify 需要在 git 仓库语境;用 git bundle list-heads 直接读
	out, err := runGitRead(ctx, "", "bundle", "list-heads", bundlePath)
	if err != nil {
		return nil, errors.Wrap(err, "bundle verify failed")
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "<sha> <refname>"
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			refs = append(refs, parts[1])
		}
	}
	info, err := os.Stat(bundlePath)
	if err != nil {
		return nil, err
	}
	return &BundleInfo{
		Name:     filepath.Base(bundlePath),
		Path:     bundlePath,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Valid:    len(refs) > 0,
		RefSpecs: refs,
	}, nil
}

// RestoreBundle 从 bundle 恢复到目标目录。
// destDir 不存在时创建;已存在且非空则拒绝(防误覆盖)。
func RestoreBundle(ctx context.Context, bundlePath, destDir string) error {
	if _, err := os.Stat(bundlePath); err != nil {
		return errors.Wrap(err, "bundle not found")
	}
	if fi, err := os.Stat(destDir); err == nil {
		if !fi.IsDir() {
			return errors.Newf("dest %s is not a directory", destDir)
		}
		entries, err := os.ReadDir(destDir)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return errors.Newf("dest %s is not empty; refusing to overwrite", destDir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return errors.Wrap(err, "create dest dir")
	}
	// git clone <bundle> <dest>
	if _, err := runGitRead(ctx, "", "clone", bundlePath, destDir); err != nil {
		return errors.Wrap(err, "clone from bundle failed")
	}
	// bundle 可能缺 HEAD,clone 后工作树为空;主动 checkout 第一个分支
	if needsCheckout(destDir) {
		// clone bundle 后常常只有 remotes/origin/<branch>,本地分支缺失
		refs, rerr := runGitRead(ctx, destDir, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin/")
		if rerr != nil {
			return errors.Wrap(rerr, "list remote branches after restore")
		}
		first := ""
		for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == "origin/HEAD" {
				continue
			}
			first = strings.TrimPrefix(line, "origin/")
			break
		}
		if first == "" {
			// 再试本地 heads(带 HEAD 的 bundle)
			if heads, err := runGitRead(ctx, destDir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(heads), "\n") {
					if line = strings.TrimSpace(line); line != "" {
						first = line
						break
					}
				}
			}
		}
		if first == "" {
			return errors.New("bundle contains no branches")
		}
		if _, cerr := runGitRead(ctx, destDir, "checkout", "-B", first, "origin/"+first); cerr != nil {
			return errors.Wrap(cerr, "checkout after restore")
		}
	}
	return nil
}

// needsCheckout 判断 restore 后是否未落工作树(.git 在但源码文件不在)。
func needsCheckout(destDir string) bool {
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			return false // 有工作树文件,无需 checkout
		}
	}
	return true
}

// runGitRead 只读 git 命令(独立于 Executor,便于 Service 层调用)。
// 统一经 gitbackend 执行:平台 RunRaw 白名单覆盖到的子命令
// (clone/for-each-ref/checkout/rev-list/...)走 RunRaw,bundle/fsck 这类
// 白名单之外的回落裸 git(dir="" 即无仓库上下文,RunRaw 原样支持)。
func runGitRead(ctx context.Context, dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("empty git args")
	}
	return runGitThroughBackend(ctx, readGitBackend(), dir, args...)
}

var (
	readBackendOnce sync.Once
	readBackend     gitbackend.GitBackend
)

// readGitBackend 包级只读 git 后端(与 MirrorService 同款 Options{} 自动选择)。
// runGitRead 不挂在 Executor 上,进程内复用一个实例即可。
func readGitBackend() gitbackend.GitBackend {
	readBackendOnce.Do(func() {
		b, err := gitbackend.NewGitBackend(gitbackend.Options{})
		if err != nil {
			slog.Warn("init git backend for read-only git failed", "error", err)
			return
		}
		readBackend = b
	})
	return readBackend
}
