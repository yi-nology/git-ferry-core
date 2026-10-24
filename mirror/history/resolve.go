package history

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// ResolutionVerdict 是 hold 文件的解法类型。
type ResolutionVerdict string

const (
	// ResolutionTakeTheirs 目标侧自共同基点起未改动 —— 取源侧内容。
	ResolutionTakeTheirs ResolutionVerdict = "take_theirs"
	// ResolutionTakeOurs 源侧自共同基点起未改动 —— 保留目标侧内容。
	ResolutionTakeOurs ResolutionVerdict = "take_ours"
	// ResolutionMergeClean 两侧都改动但不重叠 —— 可自动合并。
	ResolutionMergeClean ResolutionVerdict = "merge_clean"
	// ResolutionConflict 两侧改动重叠 —— 真冲突,需人工。
	ResolutionConflict ResolutionVerdict = "conflict"
	// ResolutionNoBase 找不到内容级共同基点 —— 无法机械判定,需人工。
	ResolutionNoBase ResolutionVerdict = "no_base"
)

// Resolution 是一个 hold 文件的解法。
type Resolution struct {
	// Path 文件路径(目标侧)。
	Path string
	// Verdict 解法类型。
	Verdict ResolutionVerdict
	// Content 解出的内容(take_theirs / merge_clean 时有值)。
	Content string
	// BaseFound 是否找到内容级共同基点。
	BaseFound bool
	// Reason 判定依据(可解释性要求)。
	Reason string

	// 以下三项是裁决所需的原始材料,供冲突卷宗使用。
	// Base 共同基点内容(无基点时为空)。
	Base string
	// Ours 目标侧当前内容。
	Ours string
	// Theirs 源侧当前内容(已身份改写)。
	Theirs string
	// Merged 三向合并结果(含冲突标记)。
	Merged string
	// Conflicts 冲突块数。
	Conflicts int
}

// NeedsHuman 判断该解法是否仍需人工介入。
func (r Resolution) NeedsHuman() bool {
	return r.Verdict == ResolutionConflict || r.Verdict == ResolutionNoBase
}

// Resolve 为计划里的每个 hold 文件计算解法。
//
// 机制:两棵树是**同源改写副本** —— 公开树的每个提交都是内部树对应提交
// 改写后的重做(实测:公开树的根提交与内部树根提交文件数同为 153,
// 某一对应提交的文件内容改写后逐字节相同)。因此两边没有 git 共同祖先,
// 却有**内容级共同基点**:某个历史版本的内容在两边同时出现。
//
// 有了基点就能做真正的三向合并,而不是让人肉裁决:
//   - 目标侧自基点起未动 → 取源侧;
//   - 源侧自基点起未动 → 保留目标侧;
//   - 两侧都动但不重叠 → 自动合并;
//   - 两侧改动重叠 → 真冲突,交人工。
//
// 实测(review-service 的 40 个 hold):30 个可机械解决,6 个真冲突,4 个无基点。
func Resolve(ctx context.Context, opts Options, plan *Plan) error {
	if plan == nil {
		return fmt.Errorf("plan 不能为空")
	}
	replayer, err := mirror.NewRewriter(opts.Rules)
	if err != nil {
		return fmt.Errorf("编译身份规则失败: %w", err)
	}
	srcRepo, err := git.PlainOpen(opts.SourceDir)
	if err != nil {
		return fmt.Errorf("打开源仓失败: %w", err)
	}
	tgtRepo, err := git.PlainOpen(opts.TargetDir)
	if err != nil {
		return fmt.Errorf("打开目标仓失败: %w", err)
	}
	srcCommit, err := resolveCommit(srcRepo, opts.SourceRef)
	if err != nil {
		return fmt.Errorf("解析源仓 ref %q 失败: %w", opts.SourceRef, err)
	}
	tgtCommit, err := resolveCommit(tgtRepo, opts.TargetRef)
	if err != nil {
		return fmt.Errorf("解析目标仓 ref %q 失败: %w", opts.TargetRef, err)
	}

	srcHist, err := fileHistories(ctx, srcRepo, srcCommit.Hash)
	if err != nil {
		return fmt.Errorf("扫描源仓历史失败: %w", err)
	}
	tgtHist, err := fileHistories(ctx, tgtRepo, tgtCommit.Hash)
	if err != nil {
		return fmt.Errorf("扫描目标仓历史失败: %w", err)
	}

	resolutions := make(map[string]Resolution)
	for _, f := range plan.Files {
		if f.Disposition != DispositionHold {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := resolveOne(replayer, srcRepo, tgtRepo, srcCommit, tgtCommit,
			srcHist, tgtHist, f, opts.ConflictRules)
		if err != nil {
			return err
		}
		resolutions[f.Path] = res
	}
	plan.Resolutions = resolutions
	return nil
}

// resolveOne 计算单个 hold 文件的解法。
//
// 判定分三层,从简单到复杂 —— 大多数文件在第一、二层就能定论,
// 只有「两边都改过」的文件才需要找共同基点做真正的三向合并:
//
//	第一层:源侧当前内容出现在目标侧的历史版本里 → 目标侧已前进,保留目标侧;
//	第二层:目标侧当前内容出现在源侧的历史版本里 → 源侧已前进,取源侧;
//	第三层:两边都前进过 → 找共同基点,做三向合并(无冲突则自动合并,否则冲突)。
func resolveOne(replayer *mirror.Rewriter, srcRepo, tgtRepo *git.Repository,
	srcCommit, tgtCommit *object.Commit,
	srcHist, tgtHist map[string][]fileHistory, f FilePlan,
	rules []ConflictRuleConfig) (Resolution, error) {

	res := Resolution{Path: f.Path}

	// 取**解析出的那个 commit** 的内容,不是 HEAD ——
	// 通道配了 source_branch 时,HEAD 与计划用的那棵树可能不是同一棵,
	// 冲突卷宗里的对照材料必须是计划那棵树的内容。
	oursBytes, err := blobAtPath(tgtRepo, tgtCommit.Hash.String(), f.Path)
	if err != nil {
		return res, err
	}
	theirsBytes, err := blobAtPath(srcRepo, srcCommit.Hash.String(), f.SourcePath)
	if err != nil {
		return res, err
	}
	ours := string(oursBytes)
	// 源侧内容**改写 + gofmt 归一化**,与计划阶段的处理路径一致。
	//
	// 只做改写会让解法阶段拿到「未格式化」的源侧内容:
	// 身份改写会改变 import 的字典序位置,归一化后的内容与目标树里
	// 已归一化的内容才是同一个坐标系。
	// 后果是实打实的:取源侧的内容会被原样写进目标树 →
	// 目标树出现 import 排序错乱 → gofmt 门禁红。
	// (实测反向回合:3 个文件因 import 组内排序错乱违规。)
	theirs := string(replayer.PublishForm(f.SourcePath, theirsBytes))
	res.Ours = ours
	res.Theirs = theirs

	// 目标侧该路径的全部历史版本(blob hash)—— diff 顺带取出的,不必读内容。
	targetBlobs := make(map[string]struct{})
	for _, h := range tgtHist[f.Path] {
		targetBlobs[h.Blob] = struct{}{}
	}

	// 源侧该路径的全部历史版本(改写后),按历史顺序(新→旧)。
	srcVersions := make([]string, 0, len(srcHist[f.SourcePath]))
	srcBlobs := make(map[string]struct{}, len(srcHist[f.SourcePath]))
	for _, h := range srcHist[f.SourcePath] {
		content, err := readBlobByHash(srcRepo, h.Blob)
		if err != nil {
			continue
		}
		rewritten := string(replayer.PublishForm(f.SourcePath, content))
		srcVersions = append(srcVersions, rewritten)
		srcBlobs[gitBlobHash([]byte(rewritten))] = struct{}{}
	}

	theirsBlob := gitBlobHash([]byte(theirs))
	oursBlob := gitBlobHash(oursBytes)

	_, targetHasTheirs := targetBlobs[theirsBlob]
	_, sourceHasOurs := srcBlobs[oursBlob]

	switch {
	case targetHasTheirs && !sourceHasOurs:
		res.Verdict = ResolutionTakeOurs
		res.BaseFound = true
		res.Reason = "源侧当前内容就是目标侧历史上的某一版,目标侧已在此基础上前进,保留目标侧"
		return res, nil
	case sourceHasOurs && !targetHasTheirs:
		res.Verdict = ResolutionTakeTheirs
		res.Content = theirs
		res.BaseFound = true
		res.Reason = "目标侧当前内容就是源侧历史上的某一版,源侧已在此基础上前进,取源侧"
		return res, nil
	}

	// 两边都前进过(或互相超越):先找共同基点做三向合并。
	var base string
	for _, v := range srcVersions {
		if _, ok := targetBlobs[gitBlobHash([]byte(v))]; ok {
			base = v
			break
		}
	}
	if base == "" {
		// 找不到基点 → 试冲突判据,还不行才交人工。
		if hit, ruleName, reason := applyConflictRules(rules, ours, theirs); hit {
			res.Verdict = ResolutionTakeOurs
			res.Reason = fmt.Sprintf("规则「%s」判定:%s", ruleName, reason)
			return res, nil
		}
		res.Verdict = ResolutionNoBase
		res.Reason = "两边都改过且找不到共同的历史版本,无法机械判定,需人工"
		return res, nil
	}
	res.BaseFound = true
	res.Base = base

	merged, conflicts := merge3(base, ours, theirs)
	res.Merged = merged
	res.Conflicts = conflicts
	switch conflicts {
	case 0:
		// 合并干净:**优先用合并结果**。
		// 判据只在合并解不了时兜底 —— 否则会把「能干净合并」的文件
		// 降级成「保留目标侧」,丢掉源侧的非冲突改动。
		//
		// **但合并干净也要过一遍判据**:合并算法按 diff 的对齐划分区域,
		// 内容重复时对齐可能与 git 不同(实测:3000 组随机用例里 git 判冲突
		// 而本实现判干净的有 38 组 —— 差异在粒度,不在内容丢失)。
		// 万一某处真被漏判,判据是最后一道防线:
		// 源侧把脱敏域名换回真实域名,不能就这么进公开仓。
		if hit, ruleName, reason := applyConflictRules(rules, ours, theirs); hit {
			res.Verdict = ResolutionConflict
			res.Reason = fmt.Sprintf("合并无冲突,但规则「%s」判定仍需人工:%s", ruleName, reason)
			return res, nil
		}
		res.Verdict = ResolutionMergeClean
		// 合并结果也归一化:diff3 按 hunk 拼装,拼出来的 import 块
		// 未必还是 gofmt 的字典序(两侧都干净 ≠ 拼完干净)。
		res.Content = string(mirror.GofmtNormalize(f.SourcePath, []byte(merged)))
		res.Reason = "两边均改动但不重叠,三向合并无冲突"
	default:
		// 合并有冲突 → 试冲突判据(有些「冲突」其实是机械可判的:
		// 源侧独有内容含内网形态、目标侧是脱敏后的公开形态)。
		if hit, ruleName, reason := applyConflictRules(rules, ours, theirs); hit {
			res.Verdict = ResolutionTakeOurs
			res.Reason = fmt.Sprintf("规则「%s」判定:%s", ruleName, reason)
			return res, nil
		}
		res.Verdict = ResolutionConflict
		res.Reason = fmt.Sprintf("两边改动重叠,三向合并产生 %d 处冲突,需人工", conflicts)
	}
	return res, nil
}

// blobAtPath 读取某提交下某路径的 blob 内容。
func blobAtPath(repo *git.Repository, rev, path string) ([]byte, error) {
	commit, err := resolveCommit(repo, rev)
	if err != nil {
		return nil, err
	}
	file, err := commit.File(path)
	if err != nil {
		return nil, fmt.Errorf("读取 %s@%s 失败: %w", path, rev, err)
	}
	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("读取 %s@%s 内容失败: %w", path, rev, err)
	}
	return []byte(content), nil
}

// readBlobByHash 按 blob hash 直接读内容(不走树查找)。
func readBlobByHash(repo *git.Repository, hash string) ([]byte, error) {
	h := plumbing.NewHash(hash)
	if h.IsZero() {
		return nil, fmt.Errorf("blob hash 为空")
	}
	return readBlob(repo, h)
}

// fileHistory 是某个路径被修改的一次提交,连同该版本内容的 blob hash。
type fileHistory struct {
	Hash    string
	Subject string
	// Blob 该版本内容的 git blob hash。
	Blob string
	// When 提交者时间,用于跨两棵平行历史定序。
	When time.Time
}

// fileHistories 遍历**全部**父提交,得到每个路径被修改的全部提交序列。
//
// 必须走全部父提交:公开树与内部树都有大量 merge commit,
// 只走第一父会漏掉「合并分支上引入的中间版本」——
// 那些版本正是共同基点的候选,漏掉会让本该「取源侧/保留目标侧」的
// 文件被误判成冲突(实测:漏 18 个)。
//
// 版本顺序按提交时间倒序:两棵树是平行历史,对应提交的**时间戳相同**
// (它们是同一批工作的重做),按时间定序能让两边的版本序列对齐。
func fileHistories(ctx context.Context, repo *git.Repository, from plumbing.Hash) (map[string][]fileHistory, error) {
	out := make(map[string][]fileHistory)
	seen := make(map[plumbing.Hash]bool)
	queue := []plumbing.Hash{from}

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h := queue[0]
		queue = queue[1:]
		if seen[h] {
			continue
		}
		seen[h] = true

		commit, err := repo.CommitObject(h)
		if err != nil {
			continue
		}
		var parent *object.Commit
		for _, ph := range commit.ParentHashes {
			queue = append(queue, ph)
			if parent == nil {
				if pc, err := repo.CommitObject(ph); err == nil {
					parent = pc
				}
			}
		}
		subject := firstLine(commit.Message)
		when := commit.Committer.When
		hash := h.String()

		if parent == nil {
			tree, err := commit.Tree()
			if err != nil {
				continue
			}
			files, err := flattenTree(tree)
			if err != nil {
				continue
			}
			for path, f := range files {
				out[path] = append(out[path], fileHistory{
					Hash: hash, Subject: subject, Blob: f.hash.String(), When: when,
				})
			}
			continue
		}

		parentTree, err := parent.Tree()
		if err != nil {
			continue
		}
		commitTree, err := commit.Tree()
		if err != nil {
			continue
		}
		changes, err := object.DiffTreeWithOptions(ctx, parentTree, commitTree,
			&object.DiffTreeOptions{DetectRenames: false})
		if err != nil {
			continue
		}
		for _, ch := range changes {
			if ch.From.Name != "" {
				out[ch.From.Name] = append(out[ch.From.Name], fileHistory{
					Hash: hash, Subject: subject, Blob: ch.From.TreeEntry.Hash.String(), When: when,
				})
			}
			if ch.To.Name != "" {
				out[ch.To.Name] = append(out[ch.To.Name], fileHistory{
					Hash: hash, Subject: subject, Blob: ch.To.TreeEntry.Hash.String(), When: when,
				})
			}
		}
	}

	// 每个路径的版本按时间倒序(新→旧)。
	for path, list := range out {
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].When.Equal(list[j].When) {
				return list[i].When.After(list[j].When)
			}
			return list[i].Hash > list[j].Hash
		})
		out[path] = list
	}
	return out, nil
}

// gitBlobHash 计算内容的 git blob hash(sha1("blob <len>\0" + content))。
func gitBlobHash(data []byte) string {
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "blob %d", len(data))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ResolutionSummary 汇总解法结果。
type ResolutionSummary struct {
	// TakeTheirs 取源侧。
	TakeTheirs int
	// TakeOurs 保留目标侧。
	TakeOurs int
	// MergeClean 自动合并。
	MergeClean int
	// Conflict 真冲突。
	Conflict int
	// NoBase 无共同基点。
	NoBase int
	// Total hold 文件总数。
	Total int
}

// SummarizeResolutions 汇总解法结果。
func SummarizeResolutions(resolutions map[string]Resolution) ResolutionSummary {
	var s ResolutionSummary
	s.Total = len(resolutions)
	for _, r := range resolutions {
		switch r.Verdict {
		case ResolutionTakeTheirs:
			s.TakeTheirs++
		case ResolutionTakeOurs:
			s.TakeOurs++
		case ResolutionMergeClean:
			s.MergeClean++
		case ResolutionConflict:
			s.Conflict++
		case ResolutionNoBase:
			s.NoBase++
		}
	}
	return s
}

// HumanResolutions 列出仍需人工的解法(按路径排序)。
func HumanResolutions(resolutions map[string]Resolution) []Resolution {
	var out []Resolution
	for _, r := range resolutions {
		if r.NeedsHuman() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ResolutionLines 把解法渲染成可读的一行行摘要。
func ResolutionLines(resolutions map[string]Resolution) []string {
	paths := make([]string, 0, len(resolutions))
	for p := range resolutions {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		r := resolutions[p]
		out = append(out, fmt.Sprintf("%s\t%s\t%s", p, r.Verdict, strings.TrimSpace(r.Reason)))
	}
	return out
}
