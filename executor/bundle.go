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
	details.WriteString(fmt.Sprintf("  bundle: %s\n", outPath))
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
