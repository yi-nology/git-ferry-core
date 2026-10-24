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
	"errors"
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
	// BackendType git 后端类型(native|gogit);空=自动选择(native 优先)。
	//
	// 必须与全系统其余部分**同一个后端**:生产配置是 git.backend=gogit,
	// 而这里硬编码 Options{} ⇒ 实际走 native —— 同一份配置下不同路径用不同后端,
	// 行为差异(可rewritable判定、超时、错误类型)就不可比了。
	BackendType string

	// ScrubRules 痕迹清理规则:发布前把内容里的内网痕迹清掉。
	//
	// **为什么镜像发布也需要它**:身份改写只管 **module path**,
	// 而内容里还有别���内网标识 —— 内网 registry 地址、部署代号、令牌。
	// 它们原样进**公开 module** 就是泄漏(历史同步侧早有这套规则)。
	//
	// 语义与历史同步侧**同一套原语**(mirror.ScrubRule):
	//   - auto:按 Replacement 替换(为空则删除命中内容);
	//   - mandatory:命中即**中止发布**(密钥类不进公开仓),不做自动替换。
	ScrubRules []ScrubRule
	// AllowOverwrite 允许覆盖镜像远端上 **tree 不一致**的同名 tag。
	// false 时检测到不一致直接报错,保护已发布版本不被误覆盖。
	//
	// ⚠️ **覆盖 = 同一个公开版本号出现两个不同的 h1**:
	// Go 的 checksum DB 对一个版本号只保留**首次**见到的 hash,
	// 之后任何拿到新 zip 的消费者都会得到 checksum mismatch,
	// 而且**无法通过重试自愈**(proxy 也会按版本永久缓存旧 zip)。
	// 这是「对外可见的公开 module」上最不可逆的一类破坏 ——
	// 所以它必须显式开启,且报告里必须打出「该版本号的 h1 将变化」。
	AllowOverwrite bool
	// AllowUnverifiedPush 远端不可达/认证失败时**盲推**(跳过一致性检查)。
	//
	// 与 AllowOverwrite 是两件事,必须分开授权:
	//   - AllowOverwrite:「我知道内容会变,确认覆盖」;
	//   - AllowUnverifiedPush:「我连不上远端,查不了状态,先推了再说」。
	//
	// 原实现里 AllowOverwrite 顺带允许了后者(文档没写),已拆开 ——
	// 一个开关同时管「覆盖」与「盲推」,等于把两个风险捆在一起授权。
	AllowUnverifiedPush bool
	// DryRun 为 true 时执行到远端一致性检查为止(校验、身份改写、门禁、
	// 远端比对),不合成 commit/tag、不推送;用于发布前预检。
	DryRun bool

	// backendOverride 直接指定 git 后端,跳过 BackendType 的自动选择。
	//
	// **仅供同包测试注入**:竞态场景(并发发布)必须在"推 tag 的那一瞬间"插手,
	// 而生产路径拿不到那个插手点 —— 不留这个口子,「推 main 前重读远端」
	// 这条防护就只能靠人肉推理,测不到。
	backendOverride gitbackend.GitBackend
}

// TagReport 是单个 tag 的发布报告。
type TagReport struct {
	Tag          string
	SourceCommit string
	Tree         string
	Commit       string
	// ReplacedFiles 是**内容真的被身份改写**的文件(仓库内相对路径)。
	ReplacedFiles []string
	// ResidualFiles 是**仍带着内网标识**的文件:规则本该改写,却因为
	// 二进制/超限而原样保留。非空即意味着本次发布被中止(未推送)。
	ResidualFiles []string
	// VerifyCmd 是发布后建议执行的验证命令。
	VerifyCmd string

	// RemoteTagExisted 表示推送前镜像远端已存在同名 tag。
	RemoteTagExisted bool
	// TreeMatchedRemote 表示远端已有 tag 的 tree 与本次合成结果一致
	// (重跑幂等的直接证据);仅在 RemoteTagExisted 且成功比对时有意义。
	TreeMatchedRemote bool
	// OverwroteDivergent 表示在 tree 不一致且 AllowOverwrite 下完成了覆盖。
	OverwroteDivergent bool
	// ScrubHits 本次发布清理掉的痕迹命中(逐条带规则名与行号)。
	//
	// 密钥类(mandatory)的命中**不回显原文**,只标记位置。
	ScrubHits []ScrubHit
	// TagAlreadyPublished 表示远端已有**同内容**的同名 tag,本次未重推。
	//
	// 幂等重跑不该改写对外可见的 tag 对象:tag 的 hash 变了,
	// 已经 pin 了 pseudo-version(形如 v0.0.0-<时间>-<commit12>)的消费者
	// 就拉不到那个版本了。内容一致时**什么都不动**才是真幂等。
	TagAlreadyPublished bool
	// H1WillChange 表示本次发布**改写了已发布版本的内容**:
	// 同一个公开版本号将出现两个不同的 h1,已发布过该版本的消费者
	// 会 checksum mismatch 且无法自愈。
	//
	// 报告里必须显式带出这一条 —— 覆盖是唯一会造成这种不可逆后果的动作。
	H1WillChange bool
	// Warning 面向人的提醒(覆盖/盲推这类「用户可能没意识到的后果」)。
	Warning string
	// MainUpdated 表示本次是否**真的更新了远端 main 快照**。
	// false ≠ 失败:可能本次版本不比远端新(不该动 main),也可能远端已有更高版本
	// (并发发布,动 main 会把默认分支回退 ⇒ 主动不动)。
	MainUpdated bool
	// MainSkipped 说明 main 快照为何没动(非空即 MainUpdated=false 的原因)。
	MainSkipped string
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
	if opts.backendOverride != nil {
		p.backend = opts.backendOverride
	} else {
		p.backend, err = gitbackend.NewGitBackend(gitbackend.Options{Type: opts.BackendType})
		if err != nil {
			return nil, fmt.Errorf("初始化 git backend 失败: %w", err)
		}
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
	b, err := newBuilder(p.repo, p.workRepo.Storer, p.opts.Mapping, p.opts.ScrubRules)
	if err != nil {
		return nil, fmt.Errorf("编译身份规则失败: %w", err)
	}
	treeHash, err := b.rewriteTree(srcCommit.TreeHash, "")
	if err != nil {
		return nil, err
	}
	report.ReplacedFiles = b.replaced
	report.ResidualFiles = b.residual
	report.ScrubHits = b.scrubHits
	// **强制人工处理的痕迹命中 ⇒ 中止发布**。
	//
	// 密钥类不做自动替换(自动替换可能留下半吊子状态,比留痕更危险),
	// 但「留痕」不等于「可以发布」—— 密钥进公开 module 就是泄漏。
	// 历史同步侧的处理是「该文件不发布」;镜像发布是整棵树,
	// 所以只能整棵中止。
	if len(b.mandatoryHits) > 0 {
		return report, fmt.Errorf("内容命中强制人工处理的痕迹规则(%d 处),已中止(未推送):%s"+
			" —— 密钥类不做自动替换,请人工清理后再发布",
			len(b.mandatoryHits), strings.Join(b.mandatoryHits, ", "))
	}
	report.Tree = treeHash.String()
	slog.Info("mirror: 身份改写完成", "tag", tag, "tree", treeHash.String(),
		"files", len(b.replaced), "residual", len(b.residual))

	// **有残留就中止,不推送**。
	//
	// 残留 = 规则本该改写、却因为二进制/超限而原样保留的内网标识。
	// 把它原样发到公开仓,等于这个工具的核心职责(公开化)没做到。
	// 与其发出去再让人发现,不如在这里停住并把文件列出来。
	if len(b.residual) > 0 {
		// **把报告一起返回**:残留清单在 report 里,
		// 只报错不给清单的话,调用方无从知道该处理哪些文件。
		return report, fmt.Errorf("身份改写后仍有内网标识残留(%d 个文件),已中止(未推送):%s"+
			" —— 这些文件是二进制或超过 %d 字节的大小上限,无法安全改写;请人工处理后再发布",
			len(b.residual), strings.Join(b.residual, ", "), maxRewriteSize)
	}

	// --- 编译门禁:改写后必须可编译才允许推送 ---
	buildDir, err := os.MkdirTemp("", "gomirror-build-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	if err = materializeTree(p.workRepo.Storer, treeHash, buildDir); err != nil {
		return nil, fmt.Errorf("物化改写后源码失败: %w", err)
	}
	if err = TypeCheckGate(ctx, buildDir); err != nil {
		return nil, fmt.Errorf("编译门禁未通过,已中止(未推送): %w", err)
	}
	slog.Info("mirror: 编译门禁通过", "tag", tag)

	// --- 远端一致性检查:同名 tag 已存在且 tree 不一致时默认中止 ---
	res, existed, overwrote, err := p.checkRemoteTag(ctx, tag, treeHash)
	if err != nil {
		// 远端为空仓库是首次发布的正常状态(无内容可覆盖),安全跳过。
		//
		// **必须精确判空**:旧实现用 `strings.Contains(err.Error(), "empty")` ——
		// 任何含 "empty" 的错误都被当成空仓库,于是
		// "empty git-upload-pack given"、"cred session: empty secret"
		// 这类错误会把「一致性检查失败」降级成「空仓库,跳过」,
		// 于是可能覆盖已发布的同名 tag,而报告说「没覆盖」。
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			slog.Info("mirror: 远端为空仓库(首次发布),跳过一致性检查", "tag", tag)
		} else if !p.opts.AllowUnverifiedPush {
			// 网络故障/认证失败时无法确认远端状态,拒绝盲推防覆盖已发布版本。
			//
			// **盲推由 AllowUnverifiedPush 单独授权** —— 原实现里它被
			// AllowOverwrite 顺带放行(文档没写),等于把「覆盖已发布版本」
			// 与「查不到远端状态」捆成一个开关。
			return nil, fmt.Errorf("远端一致性检查失败(拒绝盲推,防覆盖已发布版本;"+
				"确认要盲推请设置 AllowUnverifiedPush): %w", err)
		} else {
			slog.Warn("mirror: 远端一致性检查不可用,AllowUnverifiedPush 已开启,继续推送", "tag", tag, "error", err)
			report.Warning = "远端一致性检查不可用(远端不可达),本次推送未核对远端已有版本"
		}
	}
	report.RemoteTagExisted = existed
	report.TreeMatchedRemote = res == compareMatched
	report.OverwroteDivergent = overwrote
	if res == compareMismatched && !overwrote {
		return nil, fmt.Errorf(
			"镜像远端已存在同名 tag %s 且内容(tree)与本次合成结果不一致,拒绝覆盖已发布版本;确认要覆盖请设置 AllowOverwrite", tag)
	}
	if overwrote {
		// **覆盖的不可逆后果必须显式带出**:
		// 同一个公开版本号将出现两个不同的 h1,已发布过该版本的消费者
		// 会 checksum mismatch 且无法自愈(checksum DB 对一个版本号
		// 只保留首次见到的 hash,proxy 也按版本永久缓存)。
		report.H1WillChange = true
		report.Warning = "覆盖已发布的同名 tag:该版本号的内容 h1 将变化," +
			"已发布过该版本的消费者会 checksum mismatch 且无法自愈;正确做法是发新版本号"
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

	// --- 推送:先推 tag(对外可见的 module 版本),再 best-effort 推 main 快照 ---
	//
	// **必须拆成两次推**。一次 push 带两个 refspec 时,公开仓保护默认分支
	// (GitHub 默认分支保护 / 组织策略)会拒掉 main 那个 ref ——
	// 而没有 --atomic 时,**tag 其实已经推上去了**,core 却返回失败,
	// 壳层把它记成「发布失败」。报告与事实相反,比失败本身更危险:
	// 运营会以为没发出去,改配置重发。
	//
	// main 快照该指向谁,**在推 tag 之前**就算好 ——
	// 推完再算,远端已经有本次这个 tag,判断会失真。
	updateMain := p.shouldUpdateMain(ctx, tag)

	// **内容一致就不推 tag**。
	//
	// 幂等重跑不该改写对外可见的 tag 对象:tag 的 hash 变了,
	// 已经 pin 了 pseudo-version 的消费者就拉不到那个版本了。
	// (module 的 zip/h1 只由 tree 决定,所以内容确实没变 ——
	//  但 tag 对象变了,对外就是「这个 tag 被替换过」。)
	if res == compareMatched {
		report.TagAlreadyPublished = true
		slog.Info("mirror: 远端已有同内容的同名 tag,本次不重推(保持 tag 对象稳定)", "tag", tag)
	} else if _, err = p.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: p.tmpRepo,
		Remote:   p.remote,
		RefSpecs: []string{"refs/tags/" + tag + ":refs/tags/" + tag},
		Force:    true,
		Auth:     p.opts.Auth,
	}); err != nil {
		return nil, fmt.Errorf("推送镜像远端 %s 的 tag %s 失败: %w", mirrorURL, tag, err)
	}
	if updateMain {
		// **推 main 前必须重读远端状态** —— 上面那次判断是在推 tag **之前**做的,
		// 隔着一个网络往返。这段时间里别人可能刚发布了更高版本并把 main 指过去,
		// 我们再强推 main 就是**把默认分支回退到旧版本**
		// (clone 公开仓 / go get module@main 的人拿到过期代码)。
		//
		// 这里重读远端 tag 列表、**排除本次已推的 tag**(否则它自己就是"最高版本",
		// 判断永远失真),若发现远端已有比本次更新的版本 ⇒ 不动 main。
		//
		// 宁可 main 停在旧版本,也不能回退:main 是公开仓的默认分支,
		// 回退是不可自愈的(已经 clone 走旧代码的人拿不到更新)。
		if newer := p.remoteHighestVersionExcept(ctx, tag); newer != "" && compareVersions(newer, tag) > 0 {
			slog.Info("mirror: 远端已有更高版本,本次不动 main 快照(避免默认分支回退)",
				"tag", tag, "remote_newer", newer)
			report.Warning = appendWarning(report.Warning,
				"远端已有更高版本("+newer+"),本次不更新 main 快照(避免默认分支回退)")
			report.MainSkipped = "远端已有更高版本 " + newer
		} else if _, err = p.backend.Push(ctx, gitbackend.PushOptions{
			RepoPath: p.tmpRepo,
			Remote:   p.remote,
			RefSpecs: []string{"refs/heads/main:refs/heads/main"},
			Force:    true,
			Auth:     p.opts.Auth,
		}); err != nil {
			// main 推失败**不影响发布结论**:tag 已经公开了。
			// 报「发布失败」是假的 —— 只记警告,并在报告里说明。
			slog.Warn("mirror: main 快照推送失败(tag 已发布)", "tag", tag, "error", err)
			report.Warning = appendWarning(report.Warning,
				"main 快照推送失败(不影响本次 tag 的发布):"+err.Error())
		} else {
			report.MainUpdated = true
		}
	} else {
		slog.Info("mirror: 本次 tag 不比远端已有版本新,main 快照保持不动", "tag", tag)
		report.MainSkipped = "本次版本不比远端已有版本新"
	}

	report.VerifyCmd = "go list -m " + p.opts.Mapping.Target + "@" + tag
	slog.Info("mirror: 已发布", "module", p.opts.Mapping.Target, "tag", tag, "commit", report.Commit)
	return report, nil
}

// shouldUpdateMain 判断本次发布是否应当更新远端 main 快照。
//
// main 是公开仓的默认分支(git clone 公开仓、`go get module@main` 拿的是它),
// 所以它必须**始终指向最新发布的版本**。
//
// 原实现是「main ← 本次发布的 tag」—— 于是**补发历史版本会把默认分支回退**:
// 实测先发 v1.0.0 再补发 v0.9.0 ⇒ main 变成 v0.9.0 的快照,
// 任何 clone 公开仓的人拿到的是过期代码。
func (p *publisher) shouldUpdateMain(ctx context.Context, tag string) bool {
	best := p.remoteHighestVersionExcept(ctx, "")
	if best == "" {
		return true // 远端还没有任何版本 ⇒ 首次发布
	}
	return compareVersions(tag, best) > 0
}

// remoteHighestVersionExcept 返回远端已有 tag 中的最高版本
// (空 = 远端还没有版本)。exclude 非空时**排除该 tag**。
//
// 为什么要"排除":推 main 前重读远端时,本次的 tag 已经推上去了,
// 它自己就是最高版本 ⇒ 不排除的话判断永远是"远端不比我新",回退防护失效。
func (p *publisher) remoteHighestVersionExcept(ctx context.Context, exclude string) string {
	rem, err := p.workRepo.Remote(p.remote)
	if err != nil {
		return ""
	}
	refs, err := rem.List(&git.ListOptions{Auth: p.auth, InsecureSkipTLS: p.opts.Auth.InsecureSkipTLS})
	if err != nil {
		return ""
	}
	best := ""
	for _, ref := range refs {
		name := ref.Name().Short()
		if !strings.HasPrefix(name, "v") {
			continue
		}
		if exclude != "" && name == exclude {
			continue
		}
		if best == "" || compareVersions(name, best) > 0 {
			best = name
		}
	}
	return best
}

// appendWarning 往报告的提醒里追加一条(去重,按插入顺序)。
func appendWarning(cur, msg string) string {
	if msg == "" {
		return cur
	}
	if cur == "" {
		return msg
	}
	return cur + ";" + msg
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
	refs, err := rem.List(&git.ListOptions{Auth: p.auth, InsecureSkipTLS: p.opts.Auth.InsecureSkipTLS})
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
		RemoteName:      p.remote,
		RefSpecs:        []config.RefSpec{config.RefSpec("+refs/tags/" + tag + ":" + string(checkRef))},
		Auth:            p.auth,
		Force:           true,
		InsecureSkipTLS: p.opts.Auth.InsecureSkipTLS,
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
