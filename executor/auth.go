package executor

import (
	"context"
	"log/slog"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// BuildRepoAuth 从仓库/平台构建 git 凭证,是 executor 与 service(镜像源仓库
// 读取)共用的唯一规则实现,避免两份平行逻辑漂移。
//
// 安全约定（认证路径重构）：
//   - 令牌只在内存中传给 gitbackend.AuthConfig
//   - native backend 落地时写入 0600 临时文件 + credential.helper / GIT_ASKPASS，
//     **不进 git 进程 argv，也不进 environ 明文**（见 gitbackend/credhelper.go）
//   - 临时凭证目录随 git 命令结束删除（RAII）
//   - 仓库 token 优先于平台 token；均空则 AuthNone
//
// SSH 密钥内容同样经临时 0600 文件注入 GIT_SSH_COMMAND，命令结束后删除。
//
// p 可为 nil:此时不带平台侧的 skipTLS / SSH 指纹 / knownHosts 信息。
// platform 预取回退(repo.PlatformID → 查询)属于调用方语义,留在
// Executor.authConfig 里。
func BuildRepoAuth(repo *model.Repo, p *model.Platform) gitbackend.AuthConfig {
	var skipTLS bool
	var fingerprint, knownHosts string
	var platformToken string
	if p != nil {
		skipTLS = p.SkipTLSVerify
		platformToken = p.AccessToken
		fingerprint = p.SSHHostKeyFingerprint
		knownHosts = p.SSHKnownHostsPath
	}
	var token string
	if repo != nil {
		token = repo.AccessToken
	}
	if token == "" {
		token = platformToken
	}

	applySSHHostKey := func(auth gitbackend.AuthConfig) gitbackend.AuthConfig {
		auth.InsecureSkipTLS = skipTLS
		auth.HostKeyFingerprint = fingerprint
		auth.KnownHostsPath = knownHosts
		return auth
	}

	if token != "" {
		// git over HTTPS 走 HTTP Basic(占位用户名 + token 作密码)。
		// 不能用 Bearer: git 端点只认 Basic（GitLab/GitCode 等）。
		// 令牌由 backend 的 credential helper 会话注入，不进 argv。
		return applySSHHostKey(gitbackend.NewTokenAuth(token))
	}
	return applySSHHostKey(gitbackend.AuthConfig{Type: gitbackend.AuthNone})
}

// authConfig 从仓库/平台构建 git 凭证。
// 在 BuildRepoAuth 之上补一层 platform 预取回退:platform 未预取时按
// repo.PlatformID 查一次(兼容直接调用)。
func (e *Executor) authConfig(ctx context.Context, repo *model.Repo, platform *model.Platform) gitbackend.AuthConfig {
	p := platform
	if p == nil && repo != nil && repo.PlatformID > 0 {
		// 回退:platform 未预取时仍查一次(兼容直接调用)
		if got, err := e.service.GetPlatformByID(ctx, repo.PlatformID); err == nil {
			p = got
		}
	}
	if repo == nil {
		return BuildRepoAuth(nil, p)
	}
	auth := BuildRepoAuth(repo, p)
	if auth.Type == gitbackend.AuthNone {
		slog.Debug("no auth configured", "repo", repo.Key)
	} else {
		slog.Debug("using access token", "repo", repo.Key, "fromPlatform", repo.AccessToken == "")
	}
	return auth
}
