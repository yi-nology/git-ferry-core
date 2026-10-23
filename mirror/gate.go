package mirror

import (
	"context"
	"fmt"
	"os"

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
		return fmt.Errorf("类型检查失败: %w", firstErr)
	}
	return nil
}
