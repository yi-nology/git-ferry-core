package executor

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/oekit/pkg/pathx"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
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
// 实现复用 pathx.MatchGlob(全仓唯一口径:`*` 跨 `/`)。
func matchBranchGlob(pattern, name string) bool {
	return pathx.MatchGlob(pattern, name)
}

// divergentError 分歧保护触发的错误。
type divergentError struct {
	Branch string
	// Count 是目标侧独有提交的**真实**数量(来自 rev-list 的 behind 计数)。
	// 不能用 len(Extra):Extra 是"目标独有提交"的**展示列表**,有上限截断,
	// 且不同 git 后端对"独有"的判定还不一致 ⇒ 用它当数量会报错数字。
	Count int
	Extra []string // 目标独有提交(展示用,可能截断)
}

func (e *divergentError) Error() string {
	return fmt.Sprintf("target branch %q has %d commit(s) not in source (divergent); enable keep_divergent=false to force overwrite", e.Branch, e.Count)
}

// isDivergent 判断是否分歧保护错误。
func isDivergent(err error) bool {
	var d *divergentError
	return errors.As(err, &d)
}

// checkDivergencePolicy 按显式策略做分歧保护。
// backupDir 非空且 policy=backup_on_demand 时,覆盖前写入回滚快照。
// 策略: allow=放行 | block=拒绝覆盖(可经 Approver 放行) | backup_on_demand=先快照再覆盖。
// checkDivergencePolicy 按显式策略做分歧保护。
// 第二个返回值 usedApproval = 本次是否**用掉了**一次人工放行(调用方据此兑现"一次性")。
// 第三个返回值 approvalTargetRef = 放行所绑定的目标状态(目标分支 HEAD SHA):
// 调用方**必须**拿它去消费那条放行 —— 消费若不绑定,可能被消费掉的是另一条放行,
// 真正被授权的那条仍可反复使用(一次性形同常驻)。
func (e *Executor) checkDivergencePolicy(ctx context.Context, dir, sourceBranch, targetBranch string, force bool, policy, backupDir, taskKey string, repo *model.Repo, platform *model.Platform) (bool, string, error) {
	// 未 force 时 git 自己会拒绝非快进,无需额外检查
	if !force {
		return false, "", nil
	}
	policy, ok := normalizeDivergencePolicy(policy)
	if !ok {
		return false, "", nil
	}
	// backup_on_demand 要求覆盖前先打回滚快照;没配 backup_dir 就打不了。
	//
	// 这里**直接报配置错**、不落 pending 审批:
	// 若按 block 落审批,运维一次"放行"就会在**没有备份**的情况下覆盖目标侧独有提交 ——
	// 那正是这个策略要防的数据丢失。配置没配好就该是配置错,不该被审批绕过。
	if policy == "backup_on_demand" && backupDir == "" {
		return false, "", errors.New("force_push_policy=backup_on_demand 需要先打回滚快照," +
			"但未配置 sync.backup_dir(备份目录);请配置备份目录后重试。" +
			"本次未覆盖任何内容,也未产生审批申请 —— 避免在没有备份的情况下被放行覆盖")
	}
	// 取目标端(一次 fetch);失败按 fail-closed 处理(见 fetchTargetForDivergence)。
	targetKnown, err := e.fetchTargetForDivergence(ctx, dir, []string{targetBranch}, []string{targetBranch}, repo, platform)
	if err != nil {
		return false, "", err
	}
	return e.checkDivergenceForBranch(ctx, dir, sourceBranch, targetBranch, force, policy, backupDir, taskKey, targetKnown)
}

// normalizeDivergencePolicy 归一策略;第二个返回值 false = 不需要做分歧检查。
func normalizeDivergencePolicy(policy string) (string, bool) {
	switch policy {
	case "allow":
		return policy, false
	case "block", "backup_on_demand", "":
		return policy, true
	default:
		// 未知策略按最安全处理
		return "block", true
	}
}

// fetchTargetForDivergence 取目标端(带凭证),用于算"目标侧独有提交"。
//
// **失败要分两种情况**:
//   - 目标分支**压根不存在**(首次推送)⇒ 放行:没有独有提交可覆盖;
//   - 目标分支**已知存在**却连不上 ⇒ fail-closed 报错:
//     放行 = 允许覆盖目标侧独有提交(那正是分歧保护要防的事)。
//
// 远端跟踪可能尚未更新,先 fetch 目标分支。
//
// 必须带凭证:自建实例(私有仓 / 自签证书 / 过期证书)不带凭证就连不上,
// 而连不上时**不能**当作"无分歧"放行 —— 那等于给"覆盖目标侧独有提交"
// 开了一条静默通道(实测:目标侧分叉提交被 force push 覆盖,任务仍报成功)。
// fetchBranches 是"要 fetch 哪些分支";knownBranches 是"本次要推哪些分支"。
//
// 两者**必须分开**:多分支时 fetch 的是全量(nil),而"目标端是否已知存在"
// 要看**要推的那些分支**。之前用 fetch 参数去判断,多分支时传的是 nil ⇒
// 循环体一次都不执行 ⇒ 恒返回 false ⇒ fetch 失败被当成"目标端没有这些分支"
// 放行 —— fail-closed 成了死代码,目标侧独有提交被静默覆盖。
// 第一个返回值 targetKnown = "目标端状态是否已知"。
//
// **这是 fail-closed 的关键**:取回失败时,本地查不到 refs/remotes/target/<b>
// 既可能是"目标端没这个分支",也可能是"目标端连不上/状态未知"——
// 两者在本地看起来一模一样。不区分就会把"不知道"当成"没有"⇒ 放行覆盖。
func (e *Executor) fetchTargetForDivergence(ctx context.Context, dir string,
	fetchBranches, knownBranches []string, repo *model.Repo, platform *model.Platform) (bool, error) {
	if _, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		Branches: fetchBranches,
		Auth:     e.authConfig(ctx, repo, platform),
	}); err != nil {
		// 目标分支**已知存在**(本地留有远端跟踪 ref)却连不上 ⇒ fail-closed。
		if e.targetBranchKnown(ctx, dir, knownBranches) {
			return false, errors.Wrapf(err,
				"cannot verify divergence: target fetch failed while the target branch is known to exist")
		}
		// 已经是**全量** fetch 还是失败 ⇒ 目标端不可达/损坏 ⇒
		// **无法判断目标端有没有这些分支** ⇒ 必须 fail-closed。
		//
		// 放行的话:本地没有 refs/remotes/target/* (新 workdir 从没 fetch 过)⇒
		// 逐分支判定也判不出来("目标分支不存在"与"连不上"都表现为查不到 ref)
		// ⇒ 目标侧独有提交被静默覆盖,而任务报 success。
		if fetchBranches == nil {
			return false, errors.Wrapf(err,
				"cannot verify divergence: target fetch failed (target unreachable or broken); "+
					"refusing to overwrite without knowing the target state")
		}
		// 分支专属的 fetch 失败,可能只是"目标端确实没这个分支"(首次推送)⇒
		// 全量再取一次来区分:全量也失败 ⇒ 目标端不可达 ⇒ fail-closed。
		if ferr := e.fetchTargetAll(ctx, dir, repo, platform); ferr != nil {
			return false, errors.Wrapf(err, "cannot verify divergence: target fetch failed")
		}
		slog.Info("目标端没有这些分支(首次推送),按无分歧处理",
			"branches", strings.Join(knownBranches, ","), "error", err)
		// 目标端可达、取回成功 ⇒ 状态已知(目标端确实没这些分支)
		return true, nil
	}
	return true, nil
}

// fetchTargetAll 全量取回目标端(用来区分"没这个分支"与"连不上")。
func (e *Executor) fetchTargetAll(ctx context.Context, dir string, repo *model.Repo, platform *model.Platform) error {
	_, err := e.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir,
		Remote:   RemoteTarget,
		Branches: nil,
		Auth:     e.authConfig(ctx, repo, platform),
	})
	return err
}

// targetBranchKnown 目标端是否**已知**存在这些分支(本地有对应的远端跟踪 ref)。
func (e *Executor) targetBranchKnown(ctx context.Context, dir string, branches []string) bool {
	for _, b := range branches {
		if b == "" {
			continue
		}
		if _, err := e.backend.RevParse(ctx, dir, "refs/remotes/"+RemoteTarget+"/"+b); err == nil {
			return true
		}
	}
	return false
}

// checkDivergenceForBranch 单个分支的分歧判定(假定目标端已取过)。
//
// 与旧的整段实现相比,这里**不再吞掉"算不出来"的错误**:
// 算不出来时只有一种情况可以放行 —— 目标分支压根不存在;
// 目标分支存在却算不出来 ⇒ fail-closed(报错),否则 force 推送会覆盖独有提交。
func (e *Executor) checkDivergenceForBranch(ctx context.Context, dir, sourceBranch, targetBranch string, force bool, policy, backupDir, taskKey string, targetKnown bool) (bool, string, error) {
	// **未 force 时 git 自己会拒绝非快进**,无需额外检查。
	//
	// 这条早退必须在这里(而不是只在单分支入口):多分支路径直接调本函数,
	// 没有它就会在 force=false 时也去 fetch 目标端 + 判定分歧 ——
	// 白白多一次全量 fetch,还会往审批队列塞待批记录(任务根本没在强推)。
	if !force {
		return false, "", nil
	}
	// 策略归一也必须在这里:"allow" 的语义是"放行",不归一的话
	// 多分支路径会把 allow 当成 block ⇒ 显式允许覆盖的任务反而被拒。
	policy, ok := normalizeDivergencePolicy(policy)
	if !ok {
		return false, "", nil
	}
	// 本地 source 与 target 远端跟踪比对:target 有而 source 没有的提交
	targetRef := "refs/remotes/" + RemoteTarget + "/" + targetBranch
	// 原实现:git rev-list --right-only --count source...targetRef,
	// 取的是右端(target)独有提交数 —— 即相对 source 的 behind。
	// GetBranchSyncInfo(branch, upstream) 返回 (ahead, behind),
	// upstream 侧前缀 refs/remotes/,故 upstream 传 RemoteTarget/targetBranch。
	_, behind, err := e.backend.GetBranchSyncInfo(ctx, dir, sourceBranch, RemoteTarget+"/"+targetBranch)
	if err != nil {
		// **算不出来 ≠ 没分歧**。
		//
		// 旧实现在这里直接放行("保守放行,交给 push 决定")—— 可 force 推送
		// **正是**会覆盖目标侧独有提交的那一步,交给 push 决定 = 没有保护。
		// 多分支任务更糟:sourceBranch 传进来的是 glob("release/*"),
		// git rev-list 对含 * 的 rev 必然报错 ⇒ 分歧保护**整体失效**:
		// block 不拦、backup_on_demand 不备份,随后强推照常执行。
		//
		// 只有一种情况可以放行:目标分支**压根不存在**(首次推送,没有独有提交可覆盖)。
		// 本地有该远端跟踪 ref 却算不出来 ⇒ fail-closed,报错让人工介入。
		if _, rerr := e.backend.RevParse(ctx, dir, targetRef); rerr == nil {
			return false, "", errors.Wrapf(err,
				"cannot verify divergence for %q: target branch exists but the comparison failed", targetBranch)
		}
		if !targetKnown {
			// 目标端状态**未知**(取回失败)且本地也没有它的跟踪 ref ⇒
			// "查不到"既可能是"目标端没这个分支",也可能是"目标端连不上"——
			// 不区分就会把"不知道"当成"没有"⇒ 放行覆盖目标侧独有提交。
			return false, "", errors.Wrapf(err,
				"cannot verify divergence for %q: target state unknown (fetch failed and no local tracking ref)", targetBranch)
		}
		return false, "", nil
	}
	if behind == 0 {
		return false, "", nil
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
			return false, "", errors.Wrap(berr, "backup_on_demand snapshot failed; refusing to overwrite divergent branch")
		}
		return false, "", nil
	}
	// block 策略：若已有审批放行则放行；否则登记 pending 并拒绝
	//
	// 放行绑定到**当前目标分支的 HEAD**:审批是"针对这一次分歧"的授权,
	// 目标侧此后若又前进(独有提交从 1 个变成 50 个),旧放行不该继续适用。
	// 目标分支的 HEAD:算不出来 ⇒ **不能当成"未绑定"**。
	//
	// 空 target_ref 在 DAO 的语义里是"对任何目标状态都适用"(早期版本的审批),
	// 所以这里若把解析失败当成空串,一条**未绑定**的旧审批就会被用来覆盖
	// 此刻的目标状态 —— 正好绕开了"审批绑定目标状态"这层保护。
	// 算不出来就不该放行:报错让人工介入。
	targetSHA, shaErr := e.backend.RevParse(ctx, dir, targetRef)
	if shaErr != nil {
		return false, "", errors.Wrapf(shaErr,
			"cannot resolve target HEAD %q for divergence check", targetRef)
	}
	if e.Approver != nil {
		if e.Approver.IsForcePushApproved(taskKey, targetBranch, targetSHA) {
			// 放行通过 —— 标记"用掉了":本次强推成功后要把这条放行消费掉,
			// 否则一次人工放行会变成该任务+分支上的常驻后门。
			//
			// 连同 targetSHA 一起带回去:消费必须落在**这条**放行上。
			return true, targetSHA, nil
		}
		e.Approver.RequestApproval(taskKey, targetBranch,
			fmt.Sprintf("divergent target has %s unique commits", count), targetSHA)
	}
	return false, "", &divergentError{Branch: targetBranch, Count: behind, Extra: extra}
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
