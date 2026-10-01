package executor

import (
	"context"
	"fmt"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// expandBranchSpec 解析 SourceBranch:
//   - 精确分支名(无通配) → 单分支
//   - 含 * ? [ 的 glob → 匹配本地/远端引用得到多分支
//
// 多分支同步时 clone/fetch 不再 SingleBranch,push 逐分支映射到 TargetBranch
// (同名直推;TargetBranch 非空且是单分支时才做改名)。
func expandBranchSpec(sourceSpec string, listRefs func() ([]string, error)) (branches []string, multi bool, err error) {
	spec := strings.TrimSpace(sourceSpec)
	if spec == "" {
		return nil, false, errors.New("empty source branch")
	}
	if !strings.ContainsAny(spec, "*?[") {
		return []string{spec}, false, nil
	}
	refs, err := listRefs()
	if err != nil {
		return nil, false, errors.Wrap(err, "list refs for branch glob failed")
	}
	var out []string
	for _, r := range refs {
		name := strings.TrimPrefix(r, "refs/heads/")
		if name == r {
			continue // 非分支引用
		}
		if matchBranchGlob(spec, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, true, errors.Newf("branch glob %q matched nothing", spec)
	}
	return out, true, nil
}

// matchBranchGlob 简化 glob(* 任意段、? 单字符),避免引入额外依赖。
func matchBranchGlob(pattern, name string) bool {
	return globMatch(pattern, name)
}

// globMatch 递归通配匹配。
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*") {
		rest := pattern[1:]
		for i := 0; i <= len(s); i++ {
			if globMatch(rest, s[i:]) {
				return true
			}
		}
		return false
	}
	if s == "" {
		return false
	}
	switch pattern[0] {
	case '?':
		return globMatch(pattern[1:], s[1:])
	default:
		if pattern[0] != s[0] {
			return false
		}
		return globMatch(pattern[1:], s[1:])
	}
}

// divergentError 分歧保护触发的错误。
type divergentError struct {
	Branch string
	Extra  []string // 目标独有提交
}

func (e *divergentError) Error() string {
	return fmt.Sprintf("target branch %q has %d commit(s) not in source (divergent); enable keep_divergent=false to force overwrite", e.Branch, len(e.Extra))
}

// isDivergent 判断是否分歧保护错误。
func isDivergent(err error) bool {
	var d *divergentError
	return errors.As(err, &d)
}

// checkDivergencePolicy 按显式策略做分歧保护。
// backupDir 非空且 policy=backup_on_demand 时,覆盖前写入回滚快照。
// 策略: allow=放行 | block=拒绝覆盖(可经 Approver 放行) | backup_on_demand=先快照再覆盖。
func (e *Executor) checkDivergencePolicy(ctx context.Context, dir, sourceBranch, targetBranch string, force bool, policy, backupDir, taskKey string) error {
	// 未 force 时 git 自己会拒绝非快进,无需额外检查
	if !force {
		return nil
	}
	switch policy {
	case "allow":
		return nil
	case "block", "backup_on_demand", "":
		// 继续检测分歧
	default:
		// 未知策略按最安全处理
		policy = "block"
	}
	// 本地 source 与 target 远端跟踪比对:target 有而 source 没有的提交
	targetRef := "refs/remotes/" + RemoteTarget + "/" + targetBranch
	// 远端跟踪可能尚未更新,先 fetch 目标分支
	if _, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		Branches: []string{targetBranch},
	}); err != nil {
		// 目标分支可能不存在(首次推送),无分歧风险
		return nil
	}
	// 原实现:git rev-list --right-only --count source...targetRef,
	// 取的是右端(target)独有提交数 —— 即相对 source 的 behind。
	// GetBranchSyncInfo(branch, upstream) 返回 (ahead, behind),
	// upstream 侧前缀 refs/remotes/,故 upstream 传 RemoteTarget/targetBranch。
	_, behind, err := e.backend.GetBranchSyncInfo(ctx, dir, sourceBranch, RemoteTarget+"/"+targetBranch)
	if err != nil {
		// ref 不存在等:保守放行,交给 push 决定
		return nil
	}
	if behind == 0 {
		return nil
	}
	count := fmt.Sprintf("%d", behind)
	// 列出目标独有提交(最多 5 条,便于日志)。
	// 原实现:git log --oneline -5 source..targetRef → "hash subject"。
	extra := targetOnlyCommits(ctx, e.backend, dir, sourceBranch, targetRef)
	if policy == "backup_on_demand" && backupDir != "" {
		// 覆盖前对目标分支打快照,可回滚
		snap := filepath.Join(backupDir,
			fmt.Sprintf("predemote-%s-%s.bundle", strutil.SanitizeFileToken(targetBranch), time.Now().Format("20060102-150405")))
		if _, berr := e.gitOutput(ctx, dir, "bundle", "create", snap, targetRef); berr != nil {
			return errors.Wrap(berr, "backup_on_demand snapshot failed; refusing to overwrite divergent branch")
		}
		return nil
	}
	// block 策略：若已有审批放行则放行；否则登记 pending 并拒绝
	if e.Approver != nil {
		if e.Approver.IsForcePushApproved(taskKey, targetBranch) {
			return nil
		}
		e.Approver.RequestApproval(taskKey, targetBranch,
			fmt.Sprintf("divergent target has %s unique commits", count))
	}
	return &divergentError{Branch: targetBranch, Extra: extra}
}

// maxDivergentLog 与原 `log --oneline -5` 的条数上限一致。
const maxDivergentLog = 5

// targetOnlyCommits 列出 target 独有(不在 source 上)的提交,形如
// `git log --oneline` 的 "<hash> <subject>"(hash 为完整 SHA),最多 5 条。
// 只用于错误详情的条数与列表,解析失败按“无附加信息”处理(原实现同样忽略
// log 的错误)。
func targetOnlyCommits(ctx context.Context, backend gitbackend.GitBackend, dir, sourceBranch, targetRef string) []string {
	commits, err := backend.GetCommitsBetween(ctx, dir, sourceBranch, targetRef)
	if err != nil {
		return nil
	}
	var extra []string
	for i, c := range commits {
		if i >= maxDivergentLog {
			break
		}
		subject := c.Message
		if idx := strings.IndexByte(subject, '\n'); idx >= 0 {
			subject = subject[:idx]
		}
		extra = append(extra, c.Hash+" "+subject)
	}
	return extra
}

// gitOutput 执行只读/幂等 git 命令,统一收口到 gitbackend:
// native 后端的 RunRaw 覆盖到的子命令走 RunRaw(bundle/fsck/update-ref/lfs
// 等平台白名单之外的命令,以及非 native 后端,回落裸 git 保持行为不变)。
func (e *Executor) gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitThroughBackend(ctx, e.backend, dir, args...)
}

// runGitThroughBackend 把 git 命令交给 backend 执行,backend 跑不了的回落裸 git。
//
// 回落只发生在平台的参数预校验阶段(ErrInvalidGitArg,exec 之前)或 backend
// 非 native(gogit 不 shell out)时,不存在“平台跑过一次又跑一次”的重复执行。
func runGitThroughBackend(ctx context.Context, backend gitbackend.GitBackend, dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("empty git args")
	}
	if native, ok := backend.(*gitbackend.NativeGitBackend); ok && native != nil {
		stdout, stderr, err := native.RunRaw(ctx, dir, args)
		switch {
		case err == nil:
			return stdout, nil
		case errors.Is(err, gitbackend.ErrInvalidGitArg):
			// 平台白名单之外(bundle/fsck/update-ref/lfs/...)→ 回落裸 git
		default:
			return "", gitCmdError(args, stderr, err)
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // 参数由内部构造
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", gitCmdError(args, string(out), err)
	}
	return string(out), nil
}

// gitCmdError 保持历史错误文案:`git <argv>: <输出>`。
func gitCmdError(args []string, out string, err error) error {
	return errors.Wrapf(err, "git %s: %s", strings.Join(args, " "), strings.TrimSpace(out))
}

// syncLFS 同步 LFS 对象(git-lfs 命令行;未安装时返回可识别错误)。
// clone/fetch 后 pull LFS,push 前 push LFS。
// `git lfs` 不在平台 RunRaw 白名单内,实际仍走裸 git(gitOutput 内回落)。
func (e *Executor) syncLFS(ctx context.Context, dir, remote string, pull bool) error {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		return errors.New("git-lfs not installed in PATH (install git-lfs to sync LFS objects)")
	}
	var args []string
	if pull {
		args = []string{"lfs", "fetch", remote, "--all"}
	} else {
		args = []string{"lfs", "push", remote}
	}
	if _, err := e.gitOutput(ctx, dir, args...); err != nil {
		return err
	}
	return nil
}

// pushRefSpecs 构造推送 refspec。
// multi=true 时逐分支同名映射;否则 SourceBranch→TargetBranch(支持改名)。
// prune=true 时追加 :refs/heads/<branch> 删除目标已不存在的对应分支。
func pushRefSpecs(branches []string, sourceBranch, targetBranch string, multi bool) []string {
	var specs []string
	if multi {
		for _, b := range branches {
			specs = append(specs, fmt.Sprintf("refs/heads/%s:refs/heads/%s", b, b))
		}
		return specs
	}
	specs = append(specs, fmt.Sprintf("refs/heads/%s:refs/heads/%s", sourceBranch, targetBranch))
	return specs
}

// withTimeout 给 LFS/差异检测等辅助步骤统一超时。
func withTimeout(parent context.Context, sec int) (context.Context, context.CancelFunc) {
	if sec <= 0 {
		sec = 60
	}
	return context.WithTimeout(parent, time.Duration(sec)*time.Second)
}
