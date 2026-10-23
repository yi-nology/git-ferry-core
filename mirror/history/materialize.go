package history

import (
	"context"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// MaterializeReport 物化报告。
type MaterializeReport struct {
	// Dir 物化目录。
	Dir string
	// AppliedFiles 实际写入的 port 文件(相对路径)。
	AppliedFiles []string
	// ResolvedFiles 实际写入的 hold 解法文件(相对路径)。
	ResolvedFiles []string
	// KeptFiles 判定为「保留目标侧」、目标树已是该内容因而无需写入的文件。
	KeptFiles []string
	// SkippedScrub 命中痕迹规则、需先清理因而未应用的文件。
	SkippedScrub []string
	// HeldFiles 待人工确认、未应用的文件。
	HeldFiles []string
	// PreservedFiles 受保护、保持目标侧原样的文件数。
	PreservedFiles int
	// FileCount 物化出的文件总数。
	FileCount int
}

// Materialize 把计划落到「预期目标树」:目标树现状 + port 文件的改写内容,
// 写入 dir。
//
// 这是发布前的最后一道预演:类型门禁与格式校验都跑在物化出来的那棵树上,
// 而不是跑在源树上。差异判定、门禁、格式校验因此针对的是同一份产物——
// 三者对不上时,说明管道里有环节没串起来。
//
// 未应用的部分会如实报告:
//   - `port_with_scrub`:命中痕迹规则,须先清理;
//   - `hold`:待人工确认;
//   - `exclude`:命中排除规则。
func Materialize(ctx context.Context, opts Options, plan *Plan, dir string) (*MaterializeReport, error) {
	if plan == nil {
		return nil, fmt.Errorf("plan 不能为空")
	}
	tgtRepo, err := git.PlainOpen(opts.TargetDir)
	if err != nil {
		return nil, fmt.Errorf("打开目标仓失败: %w", err)
	}
	tgtCommit, err := resolveCommit(tgtRepo, opts.TargetRef)
	if err != nil {
		return nil, fmt.Errorf("解析目标仓 ref %q 失败: %w", opts.TargetRef, err)
	}

	// 先把目标树原样铺开:受保护文件、待确认文件都保持目标侧现状。
	if err := mirror.MaterializeTree(tgtRepo.Storer, tgtCommit.TreeHash, dir); err != nil {
		return nil, fmt.Errorf("物化目标树失败: %w", err)
	}

	srcRepo, err := git.PlainOpen(opts.SourceDir)
	if err != nil {
		return nil, fmt.Errorf("打开源仓失败: %w", err)
	}
	replayer, err := mirror.NewRewriter(opts.Rules)
	if err != nil {
		return nil, fmt.Errorf("编译身份规则失败: %w", err)
	}

	srcFiles, err := sourceFileIndex(srcRepo, opts.SourceRef)
	if err != nil {
		return nil, err
	}

	rep := &MaterializeReport{Dir: dir}
	for _, f := range plan.Files {
		switch f.Disposition {
		case DispositionPort:
			content, err := portContent(srcRepo, srcFiles, replayer, f)
			if err != nil {
				return nil, err
			}
			target := filepath.Join(dir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(target, content, 0o644); err != nil {
				return nil, fmt.Errorf("写入 %s 失败: %w", f.Path, err)
			}
			rep.AppliedFiles = append(rep.AppliedFiles, f.Path)
		case DispositionPortWithScrub:
			rep.SkippedScrub = append(rep.SkippedScrub, f.Path)
		case DispositionHold:
			// 有自动解法就按解法处理;解法仍需人工(或没有解法)才保持目标侧原样。
			res, ok := plan.Resolutions[f.Path]
			if !ok {
				rep.HeldFiles = append(rep.HeldFiles, f.Path)
				continue
			}
			if res.NeedsHuman() {
				rep.HeldFiles = append(rep.HeldFiles, f.Path)
				continue
			}
			if res.Verdict == ResolutionTakeOurs {
				// 保留目标侧:目标树已是该内容,无需写入。
				rep.KeptFiles = append(rep.KeptFiles, f.Path)
				continue
			}
			if res.Content == "" {
				rep.HeldFiles = append(rep.HeldFiles, f.Path)
				continue
			}
			target := filepath.Join(dir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(target, []byte(res.Content), 0o644); err != nil {
				return nil, fmt.Errorf("写入 %s 失败: %w", f.Path, err)
			}
			rep.ResolvedFiles = append(rep.ResolvedFiles, f.Path)
		case DispositionPreserve:
			rep.PreservedFiles++
		}
	}
	sort.Strings(rep.AppliedFiles)
	sort.Strings(rep.ResolvedFiles)
	sort.Strings(rep.KeptFiles)
	sort.Strings(rep.SkippedScrub)
	sort.Strings(rep.HeldFiles)

	rep.FileCount = countFiles(dir)
	return rep, nil
}

// portContent 计算某个 port 文件在目标侧的发布形态:源内容经身份改写与
// gofmt 归一化后的结果。
func portContent(srcRepo *git.Repository, srcFiles map[string]treeFile,
	replayer *mirror.Rewriter, f FilePlan) ([]byte, error) {

	entry, ok := srcFiles[f.SourcePath]
	if !ok {
		return nil, fmt.Errorf("源仓找不到 %s(计划与仓库不一致)", f.SourcePath)
	}
	content, err := readBlob(srcRepo, entry.hash)
	if err != nil {
		return nil, err
	}
	return normalizeSource(f.SourcePath, replayer.Rewrite(content)), nil
}

// sourceFileIndex 展开源仓文件表。
func sourceFileIndex(repo *git.Repository, ref string) (map[string]treeFile, error) {
	commit, err := resolveCommit(repo, ref)
	if err != nil {
		return nil, fmt.Errorf("解析源仓 ref %q 失败: %w", ref, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("读取源仓 tree 失败: %w", err)
	}
	return flattenTree(tree)
}

// VerifyReport 发布前校验报告。
type VerifyReport struct {
	// Materialize 物化报告。
	Materialize *MaterializeReport
	// Unformatted 物化结果中违反 gofmt 的文件。
	Unformatted []string
	// TypeCheckErr 类型门禁的错误信息,空表示通过。
	TypeCheckErr string
	// Blocked 被 hold 文件挡住的 port 文件(门禁失败时才有内容)。
	Blocked []BlockedPort
	// OK 全部校验通过。
	OK bool
}

// Verify 对计划做发布前校验:物化预期目标树 → gofmt 校验 → 类型门禁。
//
// 门禁失败不是错误返回,而是报告里的 TypeCheckErr —— 调用方要能拿到
// 「哪一行错了」才能修,不能只看到一个 error。
func Verify(ctx context.Context, opts Options, plan *Plan, dir string) (*VerifyReport, error) {
	rep, err := Materialize(ctx, opts, plan, dir)
	if err != nil {
		return nil, err
	}
	out := &VerifyReport{Materialize: rep}

	out.Unformatted = unformattedFiles(dir)

	if err := mirror.TypeCheckGate(ctx, dir); err != nil {
		out.TypeCheckErr = err.Error()
		// 把「编译失败」翻译成「谁挡住了谁」:缺哪个符号、由哪个 hold 文件定义。
		srcRepo, openErr := git.PlainOpen(opts.SourceDir)
		if openErr == nil {
			if srcFiles, idxErr := sourceFileIndex(srcRepo, opts.SourceRef); idxErr == nil {
				out.Blocked = findBlockedPorts(out.TypeCheckErr, plan, srcRepo, srcFiles, dir)
			}
		}
	}
	out.OK = len(out.Unformatted) == 0 && out.TypeCheckErr == ""
	return out, nil
}

// CheckGofmt 找出目录中违反 gofmt 的 Go 文件(相对路径)。
//
// 导出以便发布前校验复用:预检与执行跑的是同一份判定。
func CheckGofmt(dir string) []string {
	return unformattedFiles(dir)
}

// unformattedFiles 找出目录中违反 gofmt 的 Go 文件。
//
// 用 go/format.Source 判定,语义与 `gofmt -l` 一致(同样包含 import 排序),
// 但不需要起子进程。
func unformattedFiles(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		formatted, err := format.Source(data)
		if err != nil {
			// 语法错误:交给类型门禁报,这里不重复计。
			return nil
		}
		if string(formatted) != string(data) {
			rel, rerr := filepath.Rel(dir, path)
			if rerr != nil {
				rel = path
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// countFiles 统计目录下的文件数。
func countFiles(dir string) int {
	n := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}
