package history

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/yi-nology/go-git-platform/gitbackend"
)

// PushTarget 是一个推送目标。
type PushTarget struct {
	// Name 远端名(工作仓库里的 remote 名)。
	Name string
	// URL 远端 URL。
	URL string
}

// PushOptions 是推送选项。
type PushOptions struct {
	// RepoPath 工作仓库路径(Apply 的产物)。
	RepoPath string
	// Targets 推送目标,**按顺序推送**。
	//
	// 约定:第一个是留底(本地裸镜像),其后才是发布(GitHub 真上游)——
	// 留底必须先落地,否则发布失败就没有回滚依据。
	Targets []PushTarget
	// Branch 目标分支,默认 main。
	Branch string
	// Auth 认证配置。
	Auth gitbackend.AuthConfig
	// Force 强推。
	//
	// 历史同步是幂等的:同一输入两次移植产生同一批提交(hash 相同),
	// 因此重复推送不会产生分叉,强推是安全的。
	Force bool
}

// PushReport 是推送报告。
type PushReport struct {
	// Pushed 成功推送的远端(按推送顺序)。
	Pushed []string
	// Heads 各远端推送后的 HEAD。
	Heads map[string]string
	// Branch 推送的分支。
	Branch string
}

// Push 把移植结果推到目标远端。
//
// 顺序保证:**按 Targets 的顺序依次推送,任一远端失败立即中止,
// 不推送其余远端** —— 避免「一半发布、一半没发布」的半吊子状态。
//
// 推送走 gitbackend,不执行动态参数子进程(与镜像中心的安全约束一致)。
func Push(ctx context.Context, opts PushOptions) (*PushReport, error) {
	if opts.RepoPath == "" {
		return nil, fmt.Errorf("RepoPath 不能为空")
	}
	if len(opts.Targets) == 0 {
		return nil, fmt.Errorf("至少需要一个推送目标")
	}
	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}

	repo, err := git.PlainOpen(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("打开工作仓库失败: %w", err)
	}
	head, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return nil, fmt.Errorf("读取分支 %s 失败: %w", branch, err)
	}

	backend, err := gitbackend.NewGitBackend(gitbackend.Options{})
	if err != nil {
		return nil, fmt.Errorf("初始化 git backend 失败: %w", err)
	}

	report := &PushReport{Heads: map[string]string{}, Branch: branch}
	for _, target := range opts.Targets {
		if target.Name == "" || target.URL == "" {
			return nil, fmt.Errorf("推送目标的 Name 与 URL 都不能为空")
		}
		if err := EnsureRemote(repo, target.Name, target.URL); err != nil {
			return nil, err
		}
		if _, err := backend.Push(ctx, gitbackend.PushOptions{
			RepoPath: opts.RepoPath,
			Remote:   target.Name,
			RefSpecs: []string{
				fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch),
			},
			Force: opts.Force,
			Auth:  opts.Auth,
		}); err != nil {
			// 立即中止:不推送其余远端,避免半发布状态。
			return nil, fmt.Errorf("推送到远端 %s 失败(已推送:%v,其余远端未推送): %w",
				target.Name, report.Pushed, err)
		}
		report.Pushed = append(report.Pushed, target.Name)
		report.Heads[target.Name] = head.Hash().String()
	}
	return report, nil
}

// EnsureRemote 确保工作仓库里有该远端且 URL 一致;URL 变了就改写它
// (远端换地址是正常运维场景)。
//
// **单一实现**:服务层(预检的影响面检查、回滚的强推)也用它 ——
// 两份实现漂移过一次(服务侧那份对「URLs 为空」的处理与这里不同),
// 同一个远端在两条路径上行为不一致。
func EnsureRemote(repo *git.Repository, name, url string) error {
	remotes, err := repo.Remotes()
	if err != nil {
		return fmt.Errorf("读取远端列表失败: %w", err)
	}
	for _, r := range remotes {
		if r.Config().Name != name {
			continue
		}
		if len(r.Config().URLs) > 0 && r.Config().URLs[0] == url {
			return nil
		}
		// URL 变了(或远端没记 URL):更新它。
		if err := repo.DeleteRemote(name); err != nil {
			return fmt.Errorf("删除远端 %s 失败: %w", name, err)
		}
		break
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: name,
		URLs: []string{url},
	}); err != nil {
		return fmt.Errorf("创建远端 %s 失败: %w", name, err)
	}
	return nil
}
