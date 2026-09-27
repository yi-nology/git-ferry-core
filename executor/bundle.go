package executor

import (
	"context"
	"fmt"
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
		sanitizeFileToken(task.Key),
		sanitizeFileToken(task.TargetBranch),
		time.Now().Format("20060102-150405"))
	outPath := filepath.Join(backupDir, name)

	repoDir := filepath.Join(workDir, RepoDir)
	// 打当前同步分支 + 标签
	ref := "refs/heads/" + task.SourceBranch
	args := []string{"bundle", "create", outPath, ref}
	if task.GitTags {
		args = append(args, "--tags")
	}
	if _, err := e.gitOutput(ctx, repoDir, args...); err != nil {
		return "", errors.Wrap(err, "git bundle create")
	}
	fmt.Fprintf(details, "  bundle: %s\n", outPath)
	return outPath, nil
}

// sanitizeFileToken 文件名安全化(防路径穿越)。
func sanitizeFileToken(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		return "unnamed"
	}
	return s
}

// rotateBundles 每任务保留最近 keep 份,按文件名时间戳排序删除旧的。
// 借鉴 gickup zip keep N:防冷备目录无限膨胀。
func rotateBundles(backupDir, taskKey string, keep int) (removed int) {
	if keep <= 0 {
		return 0
	}
	prefix := sanitizeFileToken(taskKey) + "-"
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
