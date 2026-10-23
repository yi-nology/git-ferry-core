package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// ApplyOptions 是内容移植的输入。
type ApplyOptions struct {
	// SourceDir 内部源仓本地路径(只读)。
	SourceDir string
	// TargetDir 公开目标仓本地路径(只读;移植在临时工作副本上进行)。
	TargetDir string
	// TargetRef 目标仓分支,默认 main。
	TargetRef string
	// Rules 身份映射规则。
	Rules []mirror.IdentityRule
	// PathRules 路径规则。
	PathRules []PathRule
	// ScrubRules 痕迹规则(含自动清理与强制 hold 的区分)。
	ScrubRules []ScrubRule
	// Plan 待应用的计划;必须已调用 Resolve 填好解法。
	Plan *Plan
	// MessageSuffix 追加到 commit message 的说明。为空时用默认说明。
	MessageSuffix string
	// WorkDir 临时工作副本的父目录;为空则用系统临时目录。
	WorkDir string
}

// AppliedCommit 是一次移植产生的新提交。
type AppliedCommit struct {
	// SourceCommit 对应的源侧提交。
	SourceCommit string
	// SourceSubject 源侧提交标题。
	SourceSubject string
	// TargetCommit 目标侧新提交的 hash。
	TargetCommit string
	// Files 本次提交应用的文件。
	Files []string
}

// ApplyResult 是内容移植的结果。
type ApplyResult struct {
	// RepoPath 临时工作仓库路径(推送用它)。
	RepoPath string
	// Branch 目标分支。
	Branch string
	// BaseHead 移植前的目标分支 HEAD(既有提交原样保留,hash 不变)。
	BaseHead string
	// Head 移植后的 HEAD。
	Head string
	// Commits 新建的提交,按应用顺序。
	Commits []AppliedCommit
	// Files 应用的文件总数。
	Files int
	// Skipped 因待人工/强制痕迹而未应用的文件。
	Skipped []string
	// MandatoryHits 强制人工处理的痕迹命中。
	MandatoryHits []ScrubHit
	// Cleanup 本次发布的净变更行数(相对移植前的目标树)。
	AddedLines   int
	RemovedLines int
}

// AppliedFileList 返回本次实际应用的文件清单(按路径升序)。
func (r *ApplyResult) AppliedFileList() []string {
	out := make([]string, 0, r.Files)
	for _, c := range r.Commits {
		out = append(out, c.Files...)
	}
	sort.Strings(out)
	return out
}

// Apply 在目标树上叠加移植内容。
//
// 关键约束:**目标树既有提交原样保留**(hash 全部不变)——
// 新内容以新提交追加在既有 HEAD 之后,目标树上的 tag / 分支 / PR 引用因此全部有效。
//
// 纯本地操作:不推送、不改原始目标仓。推送由 Push 负责。
//
// 提交按**源侧提交分组**:同一源提交带来的文件落在同一个目标提交里,
// 保证「这批改动来自哪个源提交」在目标侧一目了然,回滚也能按提交回滚。
func Apply(ctx context.Context, opts ApplyOptions) (*ApplyResult, error) {
	if opts.Plan == nil {
		return nil, fmt.Errorf("plan 不能为空")
	}
	if opts.TargetDir == "" || opts.SourceDir == "" {
		return nil, fmt.Errorf("SourceDir 与 TargetDir 都不能为空")
	}
	branch := opts.TargetRef
	if branch == "" {
		branch = "main"
	}
	replayer, err := mirror.NewRewriter(opts.Rules)
	if err != nil {
		return nil, fmt.Errorf("编译身份规则失败: %w", err)
	}

	srcRepo, err := git.PlainOpen(opts.SourceDir)
	if err != nil {
		return nil, fmt.Errorf("打开源仓失败: %w", err)
	}

	// 把目标仓克隆到临时工作副本:新提交要挂在目标 HEAD 之后,
	// 因此工作副本必须带着目标仓的完整对象库。
	parent := opts.WorkDir
	if parent == "" {
		parent, err = os.MkdirTemp("", "history-apply-*")
	} else {
		err = os.MkdirAll(parent, 0o755)
	}
	if err != nil {
		return nil, err
	}
	workDir, err := os.MkdirTemp(parent, "repo-*")
	if err != nil {
		return nil, err
	}
	workRepo, err := git.PlainCloneContext(ctx, workDir, false, &git.CloneOptions{
		URL:           opts.TargetDir,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		Tags:          git.AllTags,
	})
	if err != nil {
		return nil, fmt.Errorf("克隆目标仓失败: %w", err)
	}

	headRef, err := workRepo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return nil, fmt.Errorf("解析目标分支 %s 失败: %w", branch, err)
	}
	baseCommit, err := workRepo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("读取目标 HEAD 失败: %w", err)
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, fmt.Errorf("读取目标 tree 失败: %w", err)
	}

	// 当前文件表(路径 → 条目),随提交推进更新。
	files, err := flattenTree(baseTree)
	if err != nil {
		return nil, err
	}
	srcFiles, err := sourceFileIndex(srcRepo, "")
	if err != nil {
		return nil, err
	}

	// 按源提交分组:每个源提交产出一个目标提交。
	groups, skipped, mandatoryHits, err := planApplyGroups(ctx, opts, replayer, srcRepo, srcFiles)
	if err != nil {
		return nil, err
	}

	res := &ApplyResult{
		RepoPath:      workDir,
		Branch:        branch,
		BaseHead:      baseCommit.Hash.String(),
		Skipped:       skipped,
		MandatoryHits: mandatoryHits,
	}

	parentHash := baseCommit.Hash
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 逐文件写入新 blob,并更新文件表。
		changed := make(map[string]treeFile)
		for _, path := range g.Files {
			content, err := applyFileContent(opts, replayer, srcRepo, srcFiles, path)
			if err != nil {
				return nil, err
			}
			blobHash, err := writeBlob(workRepo.Storer, content)
			if err != nil {
				return nil, err
			}
			mode := filemode.Regular
			if old, ok := files[path]; ok {
				mode = old.mode
			}
			changed[path] = treeFile{mode: mode, hash: blobHash}
		}
		for path, entry := range changed {
			files[path] = entry
		}
		newTree, err := buildTreeFromFiles(workRepo.Storer, files)
		if err != nil {
			return nil, err
		}
		treeHash := newTree

		commit, err := buildCommit(workRepo.Storer, treeHash, parentHash, g, opts, srcRepo)
		if err != nil {
			return nil, err
		}
		parentHash = commit.Hash

		res.Commits = append(res.Commits, AppliedCommit{
			SourceCommit:  g.SourceCommit,
			SourceSubject: g.Subject,
			TargetCommit:  commit.Hash.String(),
			Files:         g.Files,
		})
		res.Files += len(g.Files)
	}

	res.Head = parentHash.String()
	if len(res.Commits) > 0 {
		if err := workRepo.Storer.SetReference(plumbing.NewHashReference(
			plumbing.NewBranchReferenceName(branch), parentHash)); err != nil {
			return nil, fmt.Errorf("更新分支引用失败: %w", err)
		}
		// 工作区的 HEAD 也要指过去,否则 checkout 状态与 ref 不一致。
		if err := workRepo.Storer.SetReference(plumbing.NewSymbolicReference(
			plumbing.HEAD, plumbing.NewBranchReferenceName(branch))); err != nil {
			return nil, fmt.Errorf("更新 HEAD 失败: %w", err)
		}
		// **把新 tree 检出到工作区**:工作区目录就是「按计划应用后的目标树」。
		// 门禁(gofmt / 类型检查)必须验这份产物,而不是目标树的旧内容。
		// 不检出的话,工作区里还是旧 HEAD 的文件,门禁会验错对象。
		wt, err := workRepo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("获取工作区失败: %w", err)
		}
		if err := wt.Checkout(&git.CheckoutOptions{Hash: parentHash, Force: true}); err != nil {
			return nil, fmt.Errorf("检出新 tree 失败: %w", err)
		}
	}
	return res, nil
}

// applyGroup 是一个源提交对应的待应用文件集合。
type applyGroup struct {
	SourceCommit string
	Subject      string
	When         int64
	Files        []string
}

// planApplyGroups 依据计划算出「哪些文件应用、按源提交怎么分组」。
func planApplyGroups(ctx context.Context, opts ApplyOptions, replayer *mirror.Rewriter,
	srcRepo *git.Repository, srcFiles map[string]treeFile) ([]applyGroup, []string, []ScrubHit, error) {

	groups := make(map[string]*applyGroup)
	var order []string
	var skipped []string
	var mandatoryHits []ScrubHit

	for _, f := range opts.Plan.Files {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		switch f.Disposition {
		case DispositionPort, DispositionHold:
		default:
			continue
		}
		if f.Disposition == DispositionHold {
			res, ok := opts.Plan.Resolutions[f.Path]
			if !ok || res.NeedsHuman() || res.Verdict == ResolutionTakeOurs {
				// 无解法、需人工、或「保留目标侧」→ 都不应用。
				if f.Disposition == DispositionHold {
					skipped = append(skipped, f.Path)
				}
				continue
			}
		}
		if f.OriginCommit == "" {
			skipped = append(skipped, f.Path)
			continue
		}
		// 强制人工的痕迹 → 不应用。
		content, err := applyFileContent(opts, replayer, srcRepo, srcFiles, f.Path)
		if err != nil {
			return nil, nil, nil, err
		}
		if hits := ApplyScrub(content, opts.ScrubRules).MandatoryHits; len(hits) > 0 {
			mandatoryHits = append(mandatoryHits, hits...)
			skipped = append(skipped, f.Path)
			continue
		}

		g, ok := groups[f.OriginCommit]
		if !ok {
			g = &applyGroup{SourceCommit: f.OriginCommit, Subject: f.OriginSubject}
			groups[f.OriginCommit] = g
			order = append(order, f.OriginCommit)
		}
		g.Files = append(g.Files, f.Path)
	}

	// 按源提交时间排序:先改的在前,目标侧的历史顺序与源侧保持一致。
	for _, hash := range order {
		g := groups[hash]
		if commit, err := srcRepo.CommitObject(plumbing.NewHash(hash)); err == nil {
			g.When = commit.Committer.When.Unix()
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return groups[order[i]].When < groups[order[j]].When
	})

	out := make([]applyGroup, 0, len(order))
	for _, hash := range order {
		g := groups[hash]
		sort.Strings(g.Files)
		out = append(out, *g)
	}
	sort.Strings(skipped)
	return out, skipped, mandatoryHits, nil
}

// applyFileContent 计算文件在目标侧的发布形态:
// 源内容 → 身份改写 → gofmt 归一化 → 痕迹清理。
func applyFileContent(opts ApplyOptions, replayer *mirror.Rewriter,
	srcRepo *git.Repository, srcFiles map[string]treeFile, path string) ([]byte, error) {

	fp, ok := findPlanFile(opts.Plan, path)
	if !ok {
		return nil, fmt.Errorf("计划里找不到 %s", path)
	}
	entry, ok := srcFiles[fp.SourcePath]
	if !ok {
		return nil, fmt.Errorf("源仓找不到 %s", fp.SourcePath)
	}
	content, err := readBlob(srcRepo, entry.hash)
	if err != nil {
		return nil, err
	}
	rewritten := normalizeSource(fp.SourcePath, replayer.Rewrite(content))

	// 已解法的 hold 用解法算出的内容(take_theirs / merge_clean)。
	if fp.Disposition == DispositionHold {
		if res, ok := opts.Plan.Resolutions[path]; ok && res.Content != "" {
			rewritten = []byte(res.Content)
		}
	}

	scrubbed := ApplyScrub(rewritten, opts.ScrubRules)
	return scrubbed.Content, nil
}

// findPlanFile 在计划里按路径找文件条目。
func findPlanFile(plan *Plan, path string) (FilePlan, bool) {
	for _, f := range plan.Files {
		if f.Path == path {
			return f, true
		}
	}
	return FilePlan{}, false
}

// buildCommit 造一个目标侧提交:作者/提交者继承源提交,消息保留原 subject 并追加改写说明。
func buildCommit(st storer.EncodedObjectStorer, treeHash, parentHash plumbing.Hash,
	g applyGroup, opts ApplyOptions, srcRepo *git.Repository) (*object.Commit, error) {

	var author, committer object.Signature
	if c, err := srcRepo.CommitObject(plumbing.NewHash(g.SourceCommit)); err == nil {
		// 作者/提交者身份与时间戳继承源提交:同一输入两次移植得到同一 hash(幂等)。
		author = c.Author
		committer = c.Committer
	} else {
		author = object.Signature{Name: "git-ferry", Email: "git-ferry@local",
			When: time.Unix(g.When, 0).UTC()}
		committer = author
	}

	c := &object.Commit{
		Author:       author,
		Committer:    committer,
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{parentHash},
		Message:      commitMessage(g, opts),
	}
	enc := st.NewEncodedObject()
	if err := c.Encode(enc); err != nil {
		return nil, err
	}
	h, err := st.SetEncodedObject(enc)
	if err != nil {
		return nil, err
	}
	return object.GetCommit(st, h)
}

// commitMessage 渲染提交消息:保留原 subject,追加改写说明与文件清单。
func commitMessage(g applyGroup, opts ApplyOptions) string {
	var b strings.Builder
	b.WriteString(g.Subject)
	b.WriteString("\n\n")

	suffix := opts.MessageSuffix
	if suffix == "" {
		suffix = defaultMessageSuffix(opts)
	}
	b.WriteString(suffix)
	b.WriteString("\n\n文件(")
	fmt.Fprintf(&b, "%d", len(g.Files))
	b.WriteString("):\n")
	for _, f := range g.Files {
		b.WriteString("- ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	return b.String()
}

// defaultMessageSuffix 生成默认的改写说明。
func defaultMessageSuffix(opts ApplyOptions) string {
	from, to := "", ""
	for _, r := range opts.Rules {
		if r.Kind == mirror.KindModule {
			from, to = r.Source, r.Target
			break
		}
	}
	if from == "" {
		return "由 git-ferry 历史同步模式移植"
	}
	return fmt.Sprintf("由 git-ferry 历史同步模式移植:身份改写 %s → %s", from, to)
}

// writeBlob 把内容写成 blob 对象。
func writeBlob(st storer.EncodedObjectStorer, data []byte) (plumbing.Hash, error) {
	enc := st.NewEncodedObject()
	enc.SetType(plumbing.BlobObject)
	enc.SetSize(int64(len(data)))
	w, err := enc.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(enc)
}

// buildTreeFromFiles 按扁平文件表重建 tree。
//
// 重建而非增量修改:目标树有 691 个文件,重建一次的成本可忽略,
// 但省掉了「在既有 tree 里插入/替换条目」的全部边界情况。
// 条目顺序按 git 规范:目录名按「name/」参与比较。
func buildTreeFromFiles(st storer.EncodedObjectStorer, files map[string]treeFile) (plumbing.Hash, error) {
	root := &treeNode{files: map[string]treeFile{}, dirs: map[string]*treeNode{}}
	for path, f := range files {
		dir, name := filepath.ToSlash(filepath.Dir(path)), filepath.Base(path)
		if dir == "." {
			dir = ""
		}
		node := root
		if dir != "" {
			for _, seg := range strings.Split(dir, "/") {
				child, ok := node.dirs[seg]
				if !ok {
					child = &treeNode{files: map[string]treeFile{}, dirs: map[string]*treeNode{}}
					node.dirs[seg] = child
				}
				node = child
			}
		}
		node.files[name] = f
	}
	return buildTreeNode(st, root)
}

// treeNode 是重建 tree 用的中间结构。
type treeNode struct {
	files map[string]treeFile
	dirs  map[string]*treeNode
}

// buildTreeNode 递归生成 tree 对象。
func buildTreeNode(st storer.EncodedObjectStorer, node *treeNode) (plumbing.Hash, error) {
	entries := make([]object.TreeEntry, 0, len(node.files)+len(node.dirs))
	for name, f := range node.files {
		entries = append(entries, object.TreeEntry{Name: name, Mode: f.mode, Hash: f.hash})
	}
	for name, child := range node.dirs {
		h, err := buildTreeNode(st, child)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
	}
	sort.Slice(entries, func(i, j int) bool { return treeEntryLess(entries[i], entries[j]) })

	tree := &object.Tree{Entries: entries}
	enc := st.NewEncodedObject()
	if err := tree.Encode(enc); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(enc)
}

// treeEntryLess 是 git 的 tree 条目排序规则:目录名按「name/」比较。
func treeEntryLess(a, b object.TreeEntry) bool {
	nameA, nameB := a.Name, b.Name
	if a.Mode == filemode.Dir {
		nameA += "/"
	}
	if b.Mode == filemode.Dir {
		nameB += "/"
	}
	return nameA < nameB
}
