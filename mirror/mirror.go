// Package mirror 提供"双仓双身份"的 Go module 镜像发布。
//
// 一份源码、两个可被 go get 的模块身份:canonical 仓库保持只读(连对象库
// 都不写入),每个 tag 在镜像侧合成为一个身份改写后的独立孤儿 commit
// (无父提交),各版本互不依赖,可对任意历史 tag 重跑。go get 按 tag 取
// 快照、proxy 按 tag 永久缓存,因此同 tag 的 tree 必须确定性一致——重跑
// 只允许改变 commit 包装,不允许改变内容;本包的合成 commit 继承源 commit
// 的作者/提交者身份与时间戳,同 tag 重跑 hash 完全一致。
//
// 流程:前置校验 → 读源树并全量身份改写 → 物化到临时目录跑 go build 门禁
// → 镜像远端同名 tag 的 tree 一致性检查(不一致默认中止)→ 合成
// tree/commit/附注 tag 写入临时仓库 → 强推 tag 与 main 快照 → 清理。
//
// git 侧全部通过 go-git 对象 API 与 SDK gitbackend 完成,不执行任何
// 动态参数的子进程;go 工具链调用(build 门禁、模块验证)只用静态参数,
// 动态数据经由文件传递。
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// Mapping 描述一次模块身份改写:canonical module path → 镜像 module path。
type Mapping struct {
	// Source 是 canonical module path,须与 RepoDir/go.mod 的 module 行一致。
	Source string
	// Target 是镜像 module path(如 github.com/org/repo)。
	Target string
}

// Options 是一次镜像发布的输入。
type Options struct {
	// RepoDir 是 canonical 仓库的工作区路径(读取 go.mod、tag 与远端配置)。
	RepoDir string
	// Mapping 指定身份改写方向。
	Mapping Mapping
	// Tags 是要发布的 tag 列表,至少一个;每个 tag 独立生成孤儿快照,
	// 支持批量补历史版本。单个失败即中止,之前成功的保留。
	Tags []string
	// Remote 是 canonical 仓库中指向镜像仓库的远端名,默认 "github"。
	Remote string
	// Auth 是访问镜像远端用的凭据;零值(AuthNone)时 native 后端以
	// 本机 git 默认凭据(ssh-agent、credential helper)推送。
	Auth gitbackend.AuthConfig
	// AllowOverwrite 允许覆盖镜像远端上 tree 不一致的同名 tag。
	// false 时检测到不一致直接报错,保护已发布版本不被误覆盖。
	AllowOverwrite bool
	// DryRun 为 true 时执行到远端一致性检查为止(校验、身份改写、门禁、
	// 远端比对),不合成 commit/tag、不推送;用于发布前预检。
	DryRun bool
}

// TagReport 是单个 tag 的发布报告。
type TagReport struct {
	Tag          string
	SourceCommit string
	Tree         string
	Commit       string
	// ReplacedFiles 是发生了身份改写的文件(仓库内相对路径)。
	ReplacedFiles []string
	// VerifyCmd 是发布后建议执行的验证命令。
	VerifyCmd string

	// RemoteTagExisted 表示推送前镜像远端已存在同名 tag。
	RemoteTagExisted bool
	// TreeMatchedRemote 表示远端已有 tag 的 tree 与本次合成结果一致
	// (重跑幂等的直接证据);仅在 RemoteTagExisted 且成功比对时有意义。
	TreeMatchedRemote bool
	// OverwroteDivergent 表示在 tree 不一致且 AllowOverwrite 下完成了覆盖。
	OverwroteDivergent bool
}

// Report 是整次发布的报告。
type Report struct {
	Tags []TagReport
}

const defaultRemote = "github"

// Publish 按 Options 逐 tag 发布镜像快照。中止于首个失败的 tag,
// 此时返回的 Report 仍包含已成功部分,便于核对。
func Publish(ctx context.Context, opts Options) (*Report, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	report := &Report{}
	for _, tag := range opts.Tags {
		r, err := publishTag(ctx, opts, tag)
		if r != nil {
			report.Tags = append(report.Tags, *r)
		}
		if err != nil {
			return report, fmt.Errorf("发布 tag %s 失败: %w", tag, err)
		}
	}
	return report, nil
}

func validateOptions(opts Options) error {
	if opts.RepoDir == "" {
		return fmt.Errorf("RepoDir 不能为空")
	}
	if len(opts.Tags) == 0 {
		return fmt.Errorf("tags 至少需要一个")
	}
	if opts.Mapping.Source == "" || opts.Mapping.Target == "" {
		return fmt.Errorf("Mapping.Source / Mapping.Target 不能为空")
	}
	if opts.Mapping.Source == opts.Mapping.Target {
		return fmt.Errorf("Mapping.Source 与 Target 相同(%s),无改写意义", opts.Mapping.Source)
	}
	if err := validateName("tag", opts.Tags[0]); err != nil {
		return err
	}
	for _, tag := range opts.Tags {
		if err := validateName("tag", tag); err != nil {
			return err
		}
	}
	remote := opts.Remote
	if remote == "" {
		remote = defaultRemote
	}
	return validateName("remote", remote)
}

type publisher struct {
	opts     Options
	remote   string
	repo     *git.Repository
	repoDir  string
	workRepo *git.Repository // 临时仓库:承载合成对象并执行推送
	tmpRepo  string
	auth     transport.AuthMethod
	backend  gitbackend.GitBackend
}

func publishTag(ctx context.Context, opts Options, tag string) (*TagReport, error) {
	remote := opts.Remote
	if remote == "" {
		remote = defaultRemote
	}

	repo, err := git.PlainOpen(opts.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("打开 canonical 仓库失败: %w", err)
	}

	mirrorURL, err := remoteURL(repo, remote)
	if err != nil {
		return nil, err
	}

	p := &publisher{
		opts:    opts,
		remote:  remote,
		repo:    repo,
		repoDir: opts.RepoDir,
	}
	// go-git 传输凭据统一由平台转换(含 SSH host key 指纹钉扎)。
	p.auth, err = gitbackend.TransportAuth(opts.Auth)
	if err != nil {
		return nil, err
	}
	p.backend, err = gitbackend.NewGitBackend(gitbackend.Options{})
	if err != nil {
		return nil, fmt.Errorf("初始化 git backend 失败: %w", err)
	}

	p.tmpRepo, err = os.MkdirTemp("", "gomirror-repo-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(p.tmpRepo) }()
	p.workRepo, err = git.PlainInit(p.tmpRepo, false)
	if err != nil {
		return nil, fmt.Errorf("初始化临时仓库失败: %w", err)
	}
	if _, err = p.workRepo.CreateRemote(&config.RemoteConfig{Name: remote, URLs: []string{mirrorURL}}); err != nil {
		return nil, err
	}

	return p.publish(ctx, tag, mirrorURL)
}

func (p *publisher) publish(ctx context.Context, tag, mirrorURL string) (*TagReport, error) {
	report := &TagReport{Tag: tag}

	// --- 前置校验:身份防呆 + tag 存在 ---
	modulePath, err := readModulePath(p.repoDir)
	if err != nil {
		return nil, err
	}
	if modulePath != p.opts.Mapping.Source {
		return nil, fmt.Errorf("go.mod module 是 %s,与 Mapping.Source %s 不一致", modulePath, p.opts.Mapping.Source)
	}

	srcCommit, err := resolveTagCommit(p.repo, tag)
	if err != nil {
		return nil, err
	}
	report.SourceCommit = srcCommit.Hash.String()

	// --- 读源树并全量身份改写(只读 canonical,写临时仓库) ---
	b := &builder{repo: p.repo, storer: p.workRepo.Storer, mapping: p.opts.Mapping}
	treeHash, err := b.rewriteTree(srcCommit.TreeHash, "")
	if err != nil {
		return nil, err
	}
	report.ReplacedFiles = b.replaced
	report.Tree = treeHash.String()
	slog.Info("mirror: 身份改写完成", "tag", tag, "tree", treeHash.String(), "files", len(b.replaced))

	// --- 编译门禁:改写后必须可编译才允许推送 ---
	buildDir, err := os.MkdirTemp("", "gomirror-build-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	if err = materializeTree(p.workRepo.Storer, treeHash, buildDir); err != nil {
		return nil, fmt.Errorf("物化改写后源码失败: %w", err)
	}
	if err = typeCheckGate(ctx, buildDir); err != nil {
		return nil, fmt.Errorf("编译门禁未通过,已中止(未推送): %w", err)
	}
	slog.Info("mirror: 编译门禁通过", "tag", tag)

	// --- 远端一致性检查:同名 tag 已存在且 tree 不一致时默认中止 ---
	res, existed, overwrote, err := p.checkRemoteTag(ctx, tag, treeHash)
	if err != nil {
		// 远端为空仓库是首次发布的正常状态(无内容可覆盖),安全跳过
		if strings.Contains(err.Error(), "empty") {
			slog.Info("mirror: 远端为空仓库(首次发布),跳过一致性检查", "tag", tag)
		} else if !p.opts.AllowOverwrite {
			// 网络故障/认证失败时无法确认远端状态,拒绝盲推防覆盖已发布版本
			return nil, fmt.Errorf("远端一致性检查失败(拒绝盲推,防覆盖已发布版本;确认覆盖请设置 AllowOverwrite): %w", err)
		} else {
			slog.Warn("mirror: 远端一致性检查不可用,AllowOverwrite 已开启,继续推送", "tag", tag, "error", err)
		}
	}
	report.RemoteTagExisted = existed
	report.TreeMatchedRemote = res == compareMatched
	report.OverwroteDivergent = overwrote
	if res == compareMismatched && !overwrote {
		return nil, fmt.Errorf(
			"镜像远端已存在同名 tag %s 且内容(tree)与本次合成结果不一致,拒绝覆盖已发布版本;确认要覆盖请设置 AllowOverwrite", tag)
	}

	// --- 合成孤儿 commit 与附注 tag(身份/时间戳继承源 commit,保证幂等) ---
	if p.opts.DryRun {
		report.VerifyCmd = "go list -m " + p.opts.Mapping.Target + "@" + tag
		slog.Info("mirror: 预检完成(未推送)", "tag", tag, "tree", report.Tree)
		return report, nil
	}
	commitHash, tagHash, err := synthesizeRefs(p.workRepo.Storer, srcCommit, treeHash, tag, p.opts.Mapping)
	if err != nil {
		return nil, err
	}
	if err = p.workRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), commitHash)); err != nil {
		return nil, err
	}
	if err = p.workRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName(tag), tagHash)); err != nil {
		return nil, err
	}
	report.Commit = commitHash.String()

	// --- 推送:同名 tag + main 快照,--force 仅为幂等重跑(同 tag tree 一致) ---
	if _, err = p.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: p.tmpRepo,
		Remote:   p.remote,
		RefSpecs: []string{
			"refs/tags/" + tag + ":refs/tags/" + tag,
			"refs/heads/main:refs/heads/main",
		},
		Force: true,
		Auth:  p.opts.Auth,
	}); err != nil {
		return nil, fmt.Errorf("推送镜像远端 %s 失败: %w", mirrorURL, err)
	}

	report.VerifyCmd = "go list -m " + p.opts.Mapping.Target + "@" + tag
	slog.Info("mirror: 已发布", "module", p.opts.Mapping.Target, "tag", tag, "commit", report.Commit)
	return report, nil
}

// compareResult 描述远端同名 tag 与本次合成 tree 的比对结论。
type compareResult int

const (
	compareUnknown    compareResult = iota // 远端不可达/对象取回失败,无法比对
	compareMatched                         // tree 一致(重跑幂等)
	compareMismatched                      // tree 不一致(远端版本有分歧)
)

// checkRemoteTag 比对镜像远端同名 tag 的 tree 与本次合成结果。
// existed 表示远端已有同名 tag;overwrote 表示在 AllowOverwrite 下
// 确认覆盖分歧版本;err 仅用于基础设施失败(网络/凭据),由调用方
// 决定是否降级——tree 不一致的中止逻辑在调用方,不可被降级吞掉。
func (p *publisher) checkRemoteTag(ctx context.Context, tag string, treeHash plumbing.Hash) (res compareResult, existed, overwrote bool, err error) {
	rem, err := p.workRepo.Remote(p.remote)
	if err != nil {
		return compareUnknown, false, false, err
	}
	refs, err := rem.List(&git.ListOptions{Auth: p.auth})
	if err != nil {
		return compareUnknown, false, false, err
	}

	found, foundPeeled := false, false
	for _, ref := range refs {
		switch ref.Name().String() {
		case "refs/tags/" + tag:
			found = true
		case "refs/tags/" + tag + "^{}":
			foundPeeled = true
		}
	}
	if !found && !foundPeeled {
		return compareUnknown, false, false, nil
	}
	existed = true

	// 该 tag 的对象尚不在临时仓库,必须先取回再剥离比较;
	// 取到 refs/mirror-check/<tag>,避免覆盖随后要写入的本地 tag 引用
	checkRef := plumbing.ReferenceName("refs/mirror-check/" + tag)
	err = p.workRepo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: p.remote,
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/tags/" + tag + ":" + string(checkRef))},
		Auth:       p.auth,
		Force:      true,
	})
	if err != nil {
		return compareUnknown, existed, false, err
	}
	commit, err := resolveRefCommit(p.workRepo, checkRef)
	if err != nil {
		return compareUnknown, existed, false, err
	}

	if commit.TreeHash == treeHash {
		return compareMatched, existed, false, nil
	}
	if !p.opts.AllowOverwrite {
		return compareMismatched, existed, false, nil
	}
	return compareMismatched, existed, true, nil
}

// resolveTagCommit 解析 canonical 仓库上的 tag(附注或轻量)为 commit。
func resolveTagCommit(repo *git.Repository, tag string) (*object.Commit, error) {
	return resolveRefCommit(repo, plumbing.NewTagReferenceName(tag))
}

func resolveRefCommit(repo *git.Repository, ref plumbing.ReferenceName) (*object.Commit, error) {
	r, err := repo.Reference(ref, false)
	if err != nil {
		return nil, fmt.Errorf("引用 %s 不存在: %w", ref.Short(), err)
	}
	h := r.Hash()
	if obj, err := repo.TagObject(h); err == nil {
		h = obj.Target
	} else if !isNotFound(err) {
		return nil, err
	}
	commit, err := repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("%s 未指向 commit: %w", ref.Short(), err)
	}
	return commit, nil
}

func isNotFound(err error) bool {
	return err == git.ErrTagNotFound || err == plumbing.ErrObjectNotFound ||
		err == plumbing.ErrReferenceNotFound
}

func remoteURL(repo *git.Repository, name string) (string, error) {
	cfg, err := repo.Config()
	if err != nil {
		return "", err
	}
	rem, ok := cfg.Remotes[name]
	if !ok || len(rem.URLs) == 0 {
		return "", fmt.Errorf("canonical 仓库未配置远端 %s(先 git remote add %s <镜像仓库 URL>)", name, name)
	}
	return rem.URLs[0], nil
}

// ReadModulePath 从 dir/go.mod 解析 module 行(兼容带引号写法),供壳层
// 创建通道时带出源模块身份。
func ReadModulePath(dir string) (string, error) {
	return readModulePath(dir)
}

// readModulePath 从 RepoDir/go.mod 解析 module 行(兼容带引号写法)。
func readModulePath(dir string) (string, error) {
	data, err := os.ReadFile(dir + "/go.mod")
	if err != nil {
		return "", fmt.Errorf("读取 go.mod 失败: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if unquoted, err := strconv.Unquote(p); err == nil {
			p = unquoted
		}
		if p == "" {
			break
		}
		return p, nil
	}
	return "", fmt.Errorf("go.mod 中未找到 module 行")
}
