package mirror

import (
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	oekiterrors "github.com/oekit/pkg/errors"
)

// NormalizeGitURL 把 git 位置串归一化成可比较形态:
//   - 网络 URL(scheme:// 或 scp 形式):归一到「host(小写)/path」,
//     剥离 scheme、凭据、端口、query/fragment、结尾 .git 与 '/';
//   - 本地路径:归一到 "local:<cleaned>"。
//
// 归一化只做「同一性判定」,不承诺还原 —— 返回值不用于展示。
// 端口被剥离:身份是 host+path,端口是传输细节;自建实例换端口
// 不该让守卫漏拦(误拦比误放更难排查,但这里误放 = 自毁,更糟)。
func NormalizeGitURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if isNetworkURL(u) {
		if pu, err := url.Parse(u); err == nil && pu.Host != "" {
			host := strings.ToLower(pu.Hostname())
			path := pu.Path
			path = strings.TrimSuffix(path, ".git")
			path = strings.TrimSuffix(path, "/")
			return host + path
		}
	}
	// scp 形式:git@host:org/repo.git(或 host:org/repo.git)
	if i := strings.IndexByte(u, ':'); i > 0 && !strings.Contains(u[:i], "/") {
		host := strings.ToLower(u[:i])
		// scp 形式的 user@ 属于凭据,不参与身份:git@host 与 host 同仓。
		if at := strings.IndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		path := u[i+1:]
		path = strings.TrimSuffix(path, ".git")
		return host + "/" + strings.TrimSuffix(path, "/")
	}
	// 本地路径
	return "local:" + filepath.Clean(u)
}

// isNetworkURL 判断 u 是否网络地址(scheme:// 或 scp 形式)。
func isNetworkURL(u string) bool {
	if strings.Contains(u, "://") {
		return true
	}
	// scp:git@host:org/repo.git(或 host:org/repo.git)
	if i := strings.IndexByte(u, ':'); i > 0 && !strings.Contains(u[:i], "/") {
		return true
	}
	return false
}

// SameGitLocation 判断两个位置串是否指同一仓库(归一化后比较)。
//
// 供 core service 层复用:t.RepoURL 与源仓库 URL 的比对必须与
// mirror 包内守卫同一套语义(同一实现,两处守卫不会各说各话)。
func SameGitLocation(a, b string) bool {
	na, nb := NormalizeGitURL(a), NormalizeGitURL(b)
	return na != "" && nb != "" && na == nb
}

// guardSelfPublish 是镜像发布的自毁保护:拒绝把改写后的内容推回源仓库自身。
//
// 三个判据(任一命中即拒绝;守卫跑在一切写入之前):
//  1. 目标就是 RepoDir 本身(本地路径守卫);
//  2. 目标与 canonical 仓的其他远端同址 —— 最常见错配是把镜像远端
//     配成源仓库 origin 的 URL(名字不同、URL 相同);
//  3. 目标 module 身份与 URL 不一致(仅当目标是网络身份且 URL 是
//     网络地址)—— 抓「remote 指回内网、TargetModule 却是公开身份」
//     的漏网配置。
//
// 判据 3 只在目标身份形如 host/path 时生效:模块路径首段含 '.' 才算
// 网络身份;非 host 形式(如 "intranet/foo")无法与 URL 对齐,不判定
// (fail-open 但无害:该形态的 module 本就不可被 go 工具链消费)。
func guardSelfPublish(repo *git.Repository, repoDir, mirrorURL, targetModule, remoteName string) error {
	// 先归一化:判据 1 的 local: 前缀只在归一化形态里出现。
	norm := NormalizeGitURL(mirrorURL)
	if strings.HasPrefix(norm, "local:") {
		p := strings.TrimPrefix(norm, "local:")
		if p == filepath.Clean(repoDir) {
			return oekiterrors.Errorf(
				"自毁保护:镜像目标 %s 就是源仓库工作区本身,拒绝发布", repoDir)
		}
	}
	if cfg, err := repo.Config(); err == nil {
		names := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == remoteName {
				// 镜像远端本身跳过(它必然出现在 canonical 的远端表里)。
				continue
			}
			for _, u := range cfg.Remotes[name].URLs {
				if SameGitLocation(u, mirrorURL) {
					return oekiterrors.Errorf(
						"自毁保护:镜像目标与源仓库的远端 %s(%s)同址,拒绝发布 —— "+
							"镜像目标必须是与源仓库不同的仓库", name, u)
				}
			}
		}
	}
	// 判据 3:目标 module 身份与 URL 一致性(网络身份 vs 网络 URL)。
	if isNetworkURL(mirrorURL) && hostLikeModule(targetModule) {
		expected := strings.ToLower(firstSegment(targetModule)) + "/" + restOfModulePath(targetModule)
		if NormalizeGitURL(mirrorURL) != expected {
			return oekiterrors.Errorf(
				"自毁保护:目标 module 身份 %s 与镜像目标 URL 不一致(归一化后为 %s),"+
					"拒绝发布 —— 检查通道目标的 targetModule 与 repoUrl 是否指向同一仓库", targetModule, NormalizeGitURL(mirrorURL))
		}
	}
	return nil
}

// hostLikeModule 判断 module path 是否形如 host/path(首段含 '.')。
func hostLikeModule(m string) bool {
	s := firstSegment(m)
	return s != "" && strings.Contains(s, ".")
}

// firstSegment 取 module path 首段(host 部分)。
func firstSegment(m string) string {
	if i := strings.IndexByte(m, '/'); i >= 0 {
		return m[:i]
	}
	return m
}

// restOfModulePath 取 module path 去掉首段后的剩余部分。
func restOfModulePath(m string) string {
	if i := strings.IndexByte(m, '/'); i >= 0 {
		return m[i+1:]
	}
	return ""
}

// CheckTargetAgainstSource 是服务层的自毁预检:目标 URL 与源仓库 URL
// 同址即拒绝。与 guardSelfPublish 用同一套归一化(SameGitLocation),
// 两层守卫不会各说各话;这里在建通道与发布前都调用 —— 配置错误越早
// 暴露越好,不必等到发布那一刻。
func CheckTargetAgainstSource(sourceURL, targetURL string) error {
	if sourceURL == "" || targetURL == "" {
		return nil
	}
	if SameGitLocation(sourceURL, targetURL) {
		return oekiterrors.Errorf(
			"自毁保护:目标仓库(%s)与源仓库同址,拒绝 —— 镜像目标必须是与源仓库不同的仓库", targetURL)
	}
	return nil
}
