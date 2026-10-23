package history

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
)

// BlockedPort 描述一个「移植被 hold 文件挡住」的 port 文件。
//
// 这是文件级主轴引入的真问题:提交是自洽的(引入 server.go 的提交同时
// 引入 readiness/checks.go),文件不是。移植一个文件可能依赖另一个
// 尚未落地的文件,而后者因为两边内容混合而停在 hold。
type BlockedPort struct {
	// Port 待移植的文件(目标侧路径)。
	Port string
	// MissingSymbol 目标侧缺失的符号(不含包限定)。
	MissingSymbol string
	// Package 符号的包限定名(门禁报错里的 pkg 部分)。
	Package string
	// DefinedBy 定义该符号的 hold 文件(源侧路径);找不到时为空。
	DefinedBy string
	// Pos 门禁报错位置(相对物化目录的 路径:行:列)。
	Pos string
}

// blockedPattern 匹配类型门禁的 "undefined: pkg.Symbol" 报错。
//
// 门禁错误形如:
//
//	类型检查失败: /tmp/xxx/expected-target/internal/modules/server.go:103:67: undefined: readiness.LLMAvailabilityCheck
var blockedPattern = regexp.MustCompile(
	`(?P<file>[^\s:]+):(?P<line>\d+):(?P<col>\d+):\s*undefined:\s*(?P<qual>[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)?)`)

// findBlockedPorts 从门禁报错里还原「谁挡住了谁」。
//
// 门禁是唯一的语义裁判:它知道目标侧缺哪个符号,而只有源侧知道这个符号
// 由谁定义。把两者接起来,审阅者拿到的就不是一句「编译失败」,
// 而是「要移植 A,得先处理 B」。
func findBlockedPorts(gateErr string, plan *Plan, srcRepo *git.Repository,
	srcFiles map[string]treeFile, materialDir string) []BlockedPort {

	if gateErr == "" || plan == nil {
		return nil
	}
	symbols := holdSymbols(plan, srcRepo, srcFiles)

	var out []BlockedPort
	seen := make(map[string]bool)
	for _, m := range blockedPattern.FindAllStringSubmatch(gateErr, -1) {
		qual := m[blockedPattern.SubexpIndex("qual")]
		pkg, sym := "", qual
		if i := strings.LastIndexByte(qual, '.'); i >= 0 {
			pkg, sym = qual[:i], qual[i+1:]
		}
		file := m[blockedPattern.SubexpIndex("file")]
		port := relativeTo(file, materialDir)
		pos := file + ":" + m[blockedPattern.SubexpIndex("line")] + ":" + m[blockedPattern.SubexpIndex("col")]

		bp := BlockedPort{
			Port:          port,
			MissingSymbol: sym,
			Package:       pkg,
			Pos:           pos,
		}
		if def, ok := symbols[sym]; ok {
			if pkg == "" || def.pkg == pkg {
				bp.DefinedBy = def.path
			}
		}
		key := bp.Port + "|" + bp.MissingSymbol
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, bp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].MissingSymbol < out[j].MissingSymbol
	})
	return out
}

// symbolOwner 记录一个符号由哪个 hold 文件定义。
type symbolOwner struct {
	path string
	pkg  string
}

// holdSymbols 收集所有 hold 文件在**源侧**声明的包级符号。
//
// 只收包级符号:方法带接收者,不参与跨文件引用。
func holdSymbols(plan *Plan, srcRepo *git.Repository, srcFiles map[string]treeFile) map[string]symbolOwner {
	out := make(map[string]symbolOwner)
	if plan == nil {
		return out
	}
	for _, f := range plan.Files {
		if f.Disposition != DispositionHold || f.SourcePath == "" {
			continue
		}
		if !strings.HasSuffix(f.SourcePath, ".go") {
			continue
		}
		entry, ok := srcFiles[f.SourcePath]
		if !ok {
			continue
		}
		content, err := readBlob(srcRepo, entry.hash)
		if err != nil {
			continue
		}
		names, pkg := topLevelDecls(f.SourcePath, content)
		for _, n := range names {
			if _, exists := out[n]; !exists {
				out[n] = symbolOwner{path: f.SourcePath, pkg: pkg}
			}
		}
	}
	return out
}

// topLevelDecls 解析 Go 文件,返回它声明的包级符号名与包名。
func topLevelDecls(path string, src []byte) ([]string, string) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, ""
	}
	var names []string
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil {
				names = append(names, decl.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names = append(names, n.Name)
					}
				}
			}
		}
	}
	return names, f.Name.Name
}

// relativeTo 把绝对路径转成相对 base 的相对路径(斜杠分隔)。
func relativeTo(path, base string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
