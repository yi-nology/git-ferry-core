package mirror

import (
	"context"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// TypeCheckGate 对 dir 下的 Go 模块做全量类型检查,作为推送前的编译门禁。
//
// 通过 golang.org/x/tools/go/packages 驱动 go 工具链完成语义加载与
// go/types 检查(与 gopls 同一机制):无法解析的导入、缺失的包、语法与
// 类型错误都会在此暴露——这正是模块身份改写可能引入的全部错误类别
// (纯文本替换不会产生代码生成/链接层的错误,后者本包不涉及)。
// 门禁不通过时调用方必须中止,不得推送。
//
// 这是唯一的编译门禁实现:身份镜像模式与历史同步共用。
func TypeCheckGate(ctx context.Context, dir string) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
			packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo,
		Dir:     dir,
		Env:     append(os.Environ(), "GOWORK=off"),
		Tests:   false,
		Context: ctx,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("加载包失败: %w", err)
	}
	if len(pkgs) == 0 {
		return fmt.Errorf("模块内未发现任何 Go 包")
	}

	var firstErr error
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %s", e.Pos, e.Msg)
			}
		}
	})
	if firstErr != nil {
		// **报错里的本机绝对路径要抹掉**。
		//
		// go list 报错时会带上 module cache 的绝对路径:
		//   ... in /Users/<用户名>/go/pkg/mod/cache/vcs/<hash>: exit status 128
		// 那是本机目录布局 + 操作者用户名 —— 对定位问题毫无用处,
		// 却会一路流进模型上下文与日志(门禁错误是 agent 工具的返回内容)。
		//
		// 只动路径:报错里的 file:line:col 位置保持原样 —— 那是诊断的关键。
		return fmt.Errorf("类型检查失败: %s", sanitizeLocalPaths(firstErr.Error()))
	}
	return nil
}

// sanitizeLocalPaths 把门禁错误里的**本机绝对路径**抹成占位符。
//
// 判据是「本机路径前缀」(用户主目录、Go module 缓存)——
// 不做泛化的绝对路径匹配:报错里也有 URL 与 file:line:col 位置,
// 一刀切会把诊断信息也毁掉。
func sanitizeLocalPaths(msg string) string {
	if msg == "" {
		return msg
	}
	prefixes := localPathPrefixes()
	// 长的先替换:否则先替换主目录,module cache 的前缀就匹配不到了。
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	for _, p := range prefixes {
		if p == "" || p == "/" {
			continue
		}
		msg = strings.ReplaceAll(msg, p, "[本机路径已脱敏]")
	}
	return msg
}

// localPathPrefixes 返回本机可能出现的路径前缀。
func localPathPrefixes() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		out = append(out, home)
		out = append(out, filepath.Join(home, "go", "pkg", "mod"))
	}
	if gp := build.Default.GOPATH; gp != "" {
		out = append(out, filepath.Join(gp, "pkg", "mod"))
	}
	return out
}
