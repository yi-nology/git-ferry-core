// Package history 实现「历史同步」:把内部源仓与公开目标仓之间的**内容差异**
// 整理成一份可审阅的同步计划,同时保留目标仓自己的提交历史。
//
// 与 mirror 包的 publish 模式并存,语义不同:
//
//   - publish(身份镜像):同一仓库的多个身份,每个 tag 一个孤儿快照,
//     目标侧没有自己的历史;
//   - history(历史同步):两个各自有完整历史的仓库,按内容差异叠加,
//     目标侧的既有提交原样保留。
//
// 计划的主轴是**文件级内容差异**,不是提交级差异。
// 理由(实测):两个同源仓库在演进中会出现「同一工作、不同提交切分/措辞」,
// 提交级匹配(subject/author/date)会把大量已发布的同源工作误判为「内部领先」,
// 进而产出重复移植与大面积冲突。内容差异不受切分方式影响。
package history

import (
	"bytes"
	"context"
	"fmt"
	"go/format"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// Disposition 单个文件的处置方式。
type Disposition string

const (
	// DispositionSame 两侧内容一致(经身份改写后),无需动作。
	DispositionSame Disposition = "same"
	// DispositionPort 源侧为目标的严格超集(只增不删),以源侧内容覆盖。
	DispositionPort Disposition = "port"
	// DispositionPortWithScrub 源侧需要移植,但内容命中痕迹规则,须清理后移植。
	DispositionPortWithScrub Disposition = "port_with_scrub"
	// DispositionExclude 命中排除规则,不参与同步。
	DispositionExclude Disposition = "exclude"
	// DispositionHold 两侧各有独有内容,方向无法机械判定,需人工/AI 确认。
	DispositionHold Disposition = "hold"
	// DispositionPreserve 仅目标侧存在,原样保留;移植时不得覆盖。
	DispositionPreserve Disposition = "preserve"
)

// ScrubRule 一条痕迹检测与清理规则。
type ScrubRule struct {
	// Name 规则名,出现在计划的命中原因里。
	Name string
	// Category 类别分组(如 "internal-host" / "secret"),仅用于展示。
	Category string
	// Pattern 匹配模式。对**改写后**的内容生效。
	Pattern *regexp.Regexp
	// Severity 处理强度,默认 ScrubAuto。
	Severity ScrubSeverity
	// Replacement 命中后的替换文本。ScrubAuto 且为空表示**删除**命中内容。
	Replacement string
}

// PathRule 一条路径规则。作用于源侧路径,决定它去哪个目标路径、还是被排除。
type PathRule struct {
	// From 源仓路径前缀(目录或文件)。
	From string
	// To 目标仓路径前缀。为空表示「排除」——该路径不参与同步。
	To string
}

// Options 一次历史同步比对的输入。
type Options struct {
	// SourceDir 内部源仓的本地路径(已克隆)。
	SourceDir string
	// TargetDir 公开目标仓的本地路径(已克隆)。
	TargetDir string
	// SourceRef 源仓比对 ref,默认 HEAD。
	SourceRef string
	// TargetRef 目标仓比对 ref,默认 HEAD。
	TargetRef string
	// Rules 身份映射规则。执行顺序由 mirror.Rewriter 固定:
	// module → host → literal。
	Rules []mirror.IdentityRule
	// PathRules 路径重命名/排除规则,按声明顺序匹配,首个命中生效。
	PathRules []PathRule
	// ScrubRules 痕迹检测规则。
	ScrubRules []ScrubRule
	// ConflictRules 冲突自动判据。只在确定性判定解不了时兜底。
	ConflictRules []ConflictRuleConfig
}

// ScrubHit 一处痕迹命中。
type ScrubHit struct {
	// Rule 命中的规则名。
	Rule string
	// Category 命中的类别。
	Category string
	// Line 行号(1 起)。
	Line int
	// Text 命中所在行(截断到 200 字节)。
	Text string
}

// FilePlan 单个文件的处置计划。
type FilePlan struct {
	// Path 目标仓路径(已应用路径规则)。
	Path string
	// SourcePath 源仓原始路径(应用路径规则前)。
	SourcePath string
	// Disposition 处置方式。
	Disposition Disposition
	// Reason 触发原因(规则名/判定依据)。可解释性要求:每个非 same 的处置
	// 都必须能回答「凭什么」。
	Reason string
	// ScrubHits 痕迹命中项。
	ScrubHits []ScrubHit
	// OriginCommit 源侧最后一次修改该文件的提交(辅助标注,非主轴)。
	OriginCommit string
	// OriginSubject 该提交的 subject。
	OriginSubject string
	// AddedLines 相对目标侧的新增行数。
	AddedLines int
	// RemovedLines 相对目标侧删除的行数。
	RemovedLines int
}

// CommitPlan 一个源侧提交在计划中的汇总(回滚视图)。
type CommitPlan struct {
	// Hash 源侧提交 hash。
	Hash string
	// Subject 提交标题。
	Subject string
	// Files 该提交涉及的、已进入计划的文件。
	Files []string
	// Disposition 组内聚合处置。取最严格的一档:
	// hold > exclude > port_with_scrub > port > preserve > same。
	Disposition Disposition
}

// Summary 计划统计。
type Summary struct {
	// Total 参与比对的路径总数。
	Total int
	// Same 两侧一致。
	Same int
	// Port 源侧超集,需覆盖目标。
	Port int
	// PortWithScrub 需清理后移植。
	PortWithScrub int
	// Exclude 命中排除规则。
	Exclude int
	// Hold 方向不明,待确认。
	Hold int
	// Resolved 其中已有裁决(人工/AI)的数量。
	Resolved int
	// Preserve 仅目标侧存在,受保护。
	Preserve int
	// ScrubHits 痕迹命中总数。
	ScrubHits int
	// AddedLines / RemovedLines 全量增删行数。
	AddedLines   int
	RemovedLines int
}

// Plan 一份历史同步计划。
type Plan struct {
	// Files 逐文件处置,按 Path 升序(确定性)。
	Files []FilePlan
	// Summary 统计。
	Summary Summary
	// Commits 按源侧提交分组的回滚视图,按首次出现顺序。
	Commits []CommitPlan
	// Resolutions hold 文件的自动解法(键为路径),由 Resolve 填充。
	// 未填充时为空 —— 计划仍可用,但 hold 需人工逐个裁决。
	Resolutions map[string]Resolution
}

// Actionable 返回所有需要动作的文件(排除 same 与 preserve)。
func (p *Plan) Actionable() []FilePlan {
	out := make([]FilePlan, 0, len(p.Files))
	for _, f := range p.Files {
		if f.Disposition == DispositionSame || f.Disposition == DispositionPreserve {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Protected 返回受保护的目标侧文件(不得覆盖)。
func (p *Plan) Protected() []FilePlan {
	out := make([]FilePlan, 0, len(p.Files))
	for _, f := range p.Files {
		if f.Disposition == DispositionPreserve {
			out = append(out, f)
		}
	}
	return out
}

// BuildPlan 生成历史同步计划。
//
// 纯读操作:不写目标仓、不改工作区、不产生任何推送。
// 同一输入两次生成的计划完全一致(确定性)。
func BuildPlan(ctx context.Context, opts Options) (*Plan, error) {
	if opts.SourceDir == "" || opts.TargetDir == "" {
		return nil, fmt.Errorf("SourceDir 与 TargetDir 都不能为空")
	}
	replayer, err := mirror.NewRewriter(opts.Rules)
	if err != nil {
		return nil, fmt.Errorf("编译身份规则失败: %w", err)
	}

	srcRepo, err := git.PlainOpen(opts.SourceDir)
	if err != nil {
		return nil, fmt.Errorf("打开源仓失败: %w", err)
	}
	tgtRepo, err := git.PlainOpen(opts.TargetDir)
	if err != nil {
		return nil, fmt.Errorf("打开目标仓失败: %w", err)
	}

	srcCommit, err := resolveCommit(srcRepo, opts.SourceRef)
	if err != nil {
		return nil, fmt.Errorf("解析源仓 ref %q 失败: %w", opts.SourceRef, err)
	}
	tgtCommit, err := resolveCommit(tgtRepo, opts.TargetRef)
	if err != nil {
		return nil, fmt.Errorf("解析目标仓 ref %q 失败: %w", opts.TargetRef, err)
	}

	srcTree, err := srcCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("读取源仓 tree 失败: %w", err)
	}
	tgtTree, err := tgtCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("读取目标仓 tree 失败: %w", err)
	}

	srcFiles, err := flattenTree(srcTree)
	if err != nil {
		return nil, fmt.Errorf("展开源仓文件表失败: %w", err)
	}
	tgtFiles, err := flattenTree(tgtTree)
	if err != nil {
		return nil, fmt.Errorf("展开目标仓文件表失败: %w", err)
	}

	// 源侧路径先过路径规则:重命名落到目标路径,排除项单列。
	effectiveToSource := make(map[string]string, len(srcFiles))
	excluded := make([]string, 0)
	for path := range srcFiles {
		rule, ok := matchPathRule(opts.PathRules, path)
		if !ok {
			effectiveToSource[path] = path
			continue
		}
		if rule.To == "" {
			excluded = append(excluded, path)
			continue
		}
		effectiveToSource[rule.To+strings.TrimPrefix(path, rule.From)] = path
	}
	sort.Strings(excluded)

	// 参与比对的路径集合:源侧(排除项单列)与目标侧的并集。
	allPaths := make(map[string]struct{}, len(effectiveToSource)+len(tgtFiles))
	for path := range effectiveToSource {
		allPaths[path] = struct{}{}
	}
	for path := range tgtFiles {
		allPaths[path] = struct{}{}
	}
	ordered := make([]string, 0, len(allPaths))
	for path := range allPaths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)

	plan := &Plan{}

	// 来源提交标注:单遍历史扫描,避免逐文件路径过滤的平方级代价。
	origins, err := originCommitMap(ctx, srcRepo, srcCommit.Hash)
	if err != nil {
		return nil, fmt.Errorf("扫描源仓历史失败: %w", err)
	}

	for _, path := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fp, err := planFile(opts, replayer, srcRepo, tgtRepo, origins,
			effectiveToSource, srcFiles, tgtFiles, path)
		if err != nil {
			return nil, err
		}
		plan.addFile(fp)
	}

	// 排除项单独列出,便于审计「为什么不带过去」。
	for _, path := range excluded {
		fp := FilePlan{
			Path:        path,
			SourcePath:  path,
			Disposition: DispositionExclude,
			Reason:      "命中路径排除规则",
		}
		annotateOrigin(origins, &fp)
		plan.addFile(fp)
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].Path < plan.Files[j].Path })

	plan.Commits = groupByCommit(plan.Files)
	return plan, nil
}

// addFile 把一个文件计划并入统计。
func (p *Plan) addFile(fp FilePlan) {
	p.Files = append(p.Files, fp)
	p.Summary.Total++
	switch fp.Disposition {
	case DispositionSame:
		p.Summary.Same++
	case DispositionPort:
		p.Summary.Port++
	case DispositionPortWithScrub:
		p.Summary.PortWithScrub++
	case DispositionExclude:
		p.Summary.Exclude++
	case DispositionHold:
		p.Summary.Hold++
	case DispositionPreserve:
		p.Summary.Preserve++
	}
	p.Summary.ScrubHits += len(fp.ScrubHits)
	p.Summary.AddedLines += fp.AddedLines
	p.Summary.RemovedLines += fp.RemovedLines
}

// planFile 比对单个路径,产出处置计划。
func planFile(opts Options, replayer *mirror.Rewriter,
	srcRepo, tgtRepo *git.Repository, origins map[string]originInfo,
	effectiveToSource map[string]string, srcFiles map[string]treeFile, tgtFiles map[string]treeFile,
	path string) (FilePlan, error) {

	srcPath, srcOK := effectiveToSource[path]
	tgtEntry, tgtOK := tgtFiles[path]

	fp := FilePlan{Path: path, SourcePath: srcPath}

	switch {
	case srcOK && !tgtOK:
		// 源侧独有:需要移植。
		content, err := readBlob(srcRepo, srcFiles[srcPath].hash)
		if err != nil {
			return fp, err
		}
		rewritten := normalizeSource(srcPath, replayer.Rewrite(content))
		fp.ScrubHits = detectScrub(rewritten, opts.ScrubRules)
		if len(fp.ScrubHits) > 0 {
			fp.Disposition = DispositionPortWithScrub
			fp.Reason = fmt.Sprintf("源侧新增,命中 %d 处痕迹规则,需清理后移植", len(fp.ScrubHits))
		} else {
			fp.Disposition = DispositionPort
			fp.Reason = "源侧新增,目标侧无此路径"
		}
		annotateOrigin(origins, &fp)
		return fp, nil

	case !srcOK && tgtOK:
		// 目标侧独有:受保护。
		fp.Disposition = DispositionPreserve
		fp.Reason = "仅目标侧存在,原样保留,不得覆盖"
		return fp, nil
	}

	// 两侧都存在:先比 blob hash,再比改写后的内容。
	srcEntry := srcFiles[srcPath]
	if srcEntry.hash == tgtEntry.hash {
		fp.Disposition = DispositionSame
		fp.Reason = "两侧内容一致"
		return fp, nil
	}
	srcContent, err := readBlob(srcRepo, srcEntry.hash)
	if err != nil {
		return fp, err
	}
	tgtContent, err := readBlob(tgtRepo, tgtEntry.hash)
	if err != nil {
		return fp, err
	}
	rewritten := normalizeSource(srcPath, replayer.Rewrite(srcContent))
	if bytes.Equal(rewritten, tgtContent) {
		// 改写后一致 —— 这正是身份改写要达成的结果。
		fp.Disposition = DispositionSame
		fp.Reason = "经身份改写后两侧一致"
		return fp, nil
	}

	verdict, added, removed := classifyDiff(tgtContent, rewritten)
	fp.AddedLines = added
	fp.RemovedLines = removed
	fp.ScrubHits = detectScrub(rewritten, opts.ScrubRules)

	switch verdict {
	case diffSourceAhead:
		fp.Disposition = DispositionPort
		fp.Reason = fmt.Sprintf("源侧为超集(仅新增 %d 行,无删改),以源侧内容覆盖", added)
	case diffTargetAhead:
		fp.Disposition = DispositionPreserve
		fp.Reason = fmt.Sprintf("目标侧为超集(仅新增 %d 行),保留目标侧内容", removed)
	case diffIdentical:
		// 字节不同但行级 diff 找不到差异:差异不在行内容上
		// (行尾符、结尾换行等)。方向无从判定,交人工。
		fp.Disposition = DispositionHold
		fp.Reason = "差异不在行级(疑似行尾符或结尾换行),需人工确认"
	default:
		fp.Disposition = DispositionHold
		fp.Reason = fmt.Sprintf("两侧各有独有内容(源侧 +%d / 目标侧 +%d),需人工确认", added, removed)
	}
	annotateOrigin(origins, &fp)
	return fp, nil
}

// normalizeSource 对改写后的源内容做发布侧归一化。
//
// 目标侧有 gofmt 门禁(公开树 CI 的 quality.yml 有格式检查步骤),
// 而身份改写会改变 import 的字典序位置——改写后的源内容会变成「未格式化」
// (实测 review-service:改写后 50 个文件违反 gofmt,两棵树原始都是 0)。
//
// 不归一化就比对,每个引用了模块路径的文件都会留下一处 1 行的假差异,
// 40 个文件凭空变成「待人工确认」。归一化后再比对,差异才是真实的内容差异。
//
// 只处理 .go。语法错误时原样返回:这类问题由类型门禁(GOWORK=off 下的
// 全量类型检查)报错,不该在这里被吞掉。
func normalizeSource(path string, data []byte) []byte {
	if !strings.HasSuffix(path, ".go") {
		return data
	}
	out, err := format.Source(data)
	if err != nil {
		return data
	}
	return out
}

// annotateOrigin 标注源侧最后一次修改该文件的提交(辅助信息,非主轴)。
func annotateOrigin(origins map[string]originInfo, fp *FilePlan) {
	if fp.SourcePath == "" {
		return
	}
	info, ok := origins[fp.SourcePath]
	if !ok {
		return
	}
	fp.OriginCommit = info.Hash
	fp.OriginSubject = info.Subject
}

// originInfo 是一个源侧提交的标注信息。
type originInfo struct {
	Hash    string
	Subject string
}

// originCommitMap 一次遍历源仓历史,得到「每个路径最后一次被修改的提交」。
//
// 逐文件调 repo.Log(FileName) 的代价是 O(文件数 × 历史长度) 次树遍历,
// 在真实仓库(数百文件、数百提交)上慢到不可用。这里改为单遍回溯:
// 从 HEAD 往回走,首个触及某路径的提交即该路径最后一次修改它的提交。
//
// 重命名会被记成「删除 + 新增」两笔(不启用重命名检测),
// 两个路径都归到同一个提交上——对「这批改动来自哪个提交」的用途足够。
func originCommitMap(ctx context.Context, repo *git.Repository, from plumbing.Hash) (map[string]originInfo, error) {
	out := make(map[string]originInfo)
	commit, err := repo.CommitObject(from)
	if err != nil {
		return nil, err
	}
	for {
		var parent *object.Commit
		if commit.NumParents() > 0 {
			parent, err = commit.Parent(0)
			if err != nil {
				return nil, err
			}
		}
		if parent == nil {
			// 根提交:它树里的全部路径都由它引入。
			// 注意只填补尚未归属的路径——后到的提交更近,不能被根提交覆盖。
			tree, err := commit.Tree()
			if err != nil {
				return nil, err
			}
			files, err := flattenTree(tree)
			if err != nil {
				return nil, err
			}
			subject := firstLine(commit.Message)
			for path := range files {
				if _, ok := out[path]; !ok {
					out[path] = originInfo{Hash: commit.Hash.String(), Subject: subject}
				}
			}
			return out, nil
		}

		parentTree, err := parent.Tree()
		if err != nil {
			return nil, err
		}
		commitTree, err := commit.Tree()
		if err != nil {
			return nil, err
		}
		changes, err := object.DiffTreeWithOptions(ctx, parentTree, commitTree,
			&object.DiffTreeOptions{DetectRenames: false})
		if err != nil {
			return nil, err
		}
		subject := firstLine(commit.Message)
		hash := commit.Hash.String()
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				if name == "" {
					continue
				}
				if _, ok := out[name]; !ok {
					out[name] = originInfo{Hash: hash, Subject: subject}
				}
			}
		}
		commit = parent
	}
}

// groupByCommit 把文件计划按源侧提交分组,产出回滚视图。
func groupByCommit(files []FilePlan) []CommitPlan {
	index := make(map[string]int)
	var out []CommitPlan
	for _, f := range files {
		if f.OriginCommit == "" || f.Disposition == DispositionSame {
			continue
		}
		idx, ok := index[f.OriginCommit]
		if !ok {
			index[f.OriginCommit] = len(out)
			out = append(out, CommitPlan{
				Hash:        f.OriginCommit,
				Subject:     f.OriginSubject,
				Disposition: f.Disposition,
			})
			idx = len(out) - 1
		}
		out[idx].Files = append(out[idx].Files, f.Path)
		if stricter(f.Disposition, out[idx].Disposition) {
			out[idx].Disposition = f.Disposition
		}
	}
	return out
}

// stricter 判断 a 是否比 b 更严格。
func stricter(a, b Disposition) bool {
	return severity(a) > severity(b)
}

func severity(d Disposition) int {
	switch d {
	case DispositionHold:
		return 5
	case DispositionExclude:
		return 4
	case DispositionPortWithScrub:
		return 3
	case DispositionPort:
		return 2
	case DispositionPreserve:
		return 1
	default:
		return 0
	}
}

// matchPathRule 找到首个命中的路径规则。
func matchPathRule(rules []PathRule, path string) (PathRule, bool) {
	for _, r := range rules {
		if r.From == "" {
			continue
		}
		if path == r.From || strings.HasPrefix(path, strings.TrimSuffix(r.From, "/")+"/") {
			return r, true
		}
	}
	return PathRule{}, false
}

// detectScrub 在内容中检测痕迹命中。
func detectScrub(content []byte, rules []ScrubRule) []ScrubHit {
	if len(content) == 0 || len(rules) == 0 {
		return nil
	}
	lines := strings.Split(string(content), "\n")
	var hits []ScrubHit
	for _, rule := range rules {
		if rule.Pattern == nil {
			continue
		}
		for i, line := range lines {
			if rule.Pattern.MatchString(line) {
				hits = append(hits, ScrubHit{
					Rule:     rule.Name,
					Category: rule.Category,
					Line:     i + 1,
					Text:     truncate(line, 200),
				})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Line != hits[j].Line {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Rule < hits[j].Rule
	})
	return hits
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// treeFile 是展开后的文件条目。
type treeFile struct {
	mode filemode.FileMode
	hash plumbing.Hash
}

// flattenTree 把 tree 展开成 path → 条目 的映射。
func flattenTree(t *object.Tree) (map[string]treeFile, error) {
	out := make(map[string]treeFile)
	if err := walkTree(t, "", out); err != nil {
		return nil, err
	}
	return out, nil
}

func walkTree(t *object.Tree, prefix string, out map[string]treeFile) error {
	for _, entry := range t.Entries {
		path := prefix + entry.Name
		switch entry.Mode {
		case filemode.Dir:
			sub, err := t.Tree(entry.Name)
			if err != nil {
				return fmt.Errorf("读取子目录 %s 失败: %w", path, err)
			}
			if err := walkTree(sub, path+"/", out); err != nil {
				return err
			}
		case filemode.Regular, filemode.Executable, filemode.Symlink:
			out[path] = treeFile{mode: entry.Mode, hash: entry.Hash}
		default:
			// gitlink(子模块):git 推送本就不携带子模块对象
		}
	}
	return nil
}

// resolveCommit 解析 ref 为提交,ref 为空时取 HEAD。
func resolveCommit(repo *git.Repository, ref string) (*object.Commit, error) {
	if ref == "" {
		ref = "HEAD"
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(*hash)
}

// readBlob 读取 blob 内容。
func readBlob(repo *git.Repository, h plumbing.Hash) ([]byte, error) {
	blob, err := repo.BlobObject(h)
	if err != nil {
		return nil, fmt.Errorf("读取 blob %s 失败: %w", h, err)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
