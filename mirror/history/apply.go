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
	// SourceRef 源仓分支,默认 HEAD。
	//
	// **必须与计划/解法用同一个 ref**:计划按 source_branch 算、
	// 写盘却按 HEAD 取 blob 的话,发布出去的是另一棵树 ——
	// 门禁验的还是计划那棵(没验到真正发出去的内容)。
	SourceRef string
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
	// AuthorName / AuthorEmail 目标侧新提交的作者身份(公开形态)。
	//
	// 留空则用 DefaultPublicAuthor。**不继承源提交的作者**:
	// 源侧是内网仓,作者是内网开发者 —— 继承等于把内网人员信息发到公开仓。
	AuthorName  string
	AuthorEmail string
	// WorkDir 临时工作副本的父目录;为空则用系统临时目录。
	WorkDir string
	// KeepProductDirs 是要保留的产物目录(还没推送成功的那些)。
	//
	// 清理产物时必须保留它们:apply 与 push 是两次独立调用,
	// 中间隔太久(人工流程、值班交接)的话,清理会把待推送的产物删掉 ——
	// 推送必然失败,而且产物不可恢复(重做 apply 也会因基线变了而得到不同的 head)。
	KeepProductDirs []string
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
	// ScrubHits 本次发布涉及的全部痕迹命中(含已自动清理的)。
	//
	// 必须与 MandatoryHits 分开报:自动清理类的命中**已随内容发布**,
	// 强制人工类的命中意味着**文件没被发布**。
	ScrubHits []ScrubHit
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
func Apply(ctx context.Context, opts ApplyOptions) (retRes *ApplyResult, retErr error) {
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

	// 清掉过期的移植产物:产物只在「apply 之后紧跟着的那次 push」里有用,
	// 留着就是纯占地方(实测:一次泄漏攒到 5.48 GB / 103 个目录)。
	// 清理范围与产物目录同级 —— 不去扫共享的系统临时目录。
	//
	// **「还没推送成功」的产物要保留**(KeepProductDirs):
	// apply 与 push 是两次独立调用,中间隔久了就会把待推送的产物删掉。
	if opts.WorkDir != "" {
		CleanupStaleProducts(opts.WorkDir, opts.KeepProductDirs...)
	}

	// 把目标仓克隆到临时工作副本:新提交要挂在目标 HEAD 之后,
	// 因此工作副本必须带着目标仓的完整对象库。
	//
	// 自己建的父目录要在**失败时**回收:失败意味着没有产物,
	// 留着只是垃圾。成功时不回收 —— 推送要用它(见 PushOptions.RepoPath)。
	parent := opts.WorkDir
	ownParent := false
	if parent == "" {
		parent, err = os.MkdirTemp("", "history-apply-*")
		ownParent = true
	} else {
		err = os.MkdirAll(parent, 0o755)
	}
	if err != nil {
		return nil, err
	}
	if ownParent {
		defer func() {
			if retErr != nil {
				_ = os.RemoveAll(parent)
			}
		}()
	}
	workDir, err := os.MkdirTemp(parent, productDirPrefix)
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
	srcFiles, err := sourceFileIndex(srcRepo, opts.SourceRef)
	if err != nil {
		return nil, err
	}

	// 按源提交分组:每个源提交产出一个目标提交。
	ap, err := planApplyGroups(ctx, opts, replayer, srcRepo, srcFiles)
	if err != nil {
		return nil, err
	}

	res := &ApplyResult{
		RepoPath:      workDir,
		Branch:        branch,
		BaseHead:      baseCommit.Hash.String(),
		Skipped:       ap.Skipped,
		ScrubHits:     ap.ScrubHits,
		MandatoryHits: ap.MandatoryHits,
	}

	parentHash := baseCommit.Hash
	for _, g := range ap.Groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 逐文件写入新 blob,并更新文件表。
		changed := make(map[string]treeFile)
		for _, path := range g.Files {
			scrub, err := applyFileContent(opts, replayer, srcRepo, srcFiles, path)
			if err != nil {
				return nil, err
			}
			blobHash, err := writeBlob(workRepo.Storer, scrub.Content)
			if err != nil {
				return nil, err
			}
			// 文件模式:目标树已有的按目标树的来;新文件按**源侧**的来
			// (可执行位不能丢 —— 脚本文件丢了 +x 就跑不起来)。
			mode := filemode.Regular
			if old, ok := files[path]; ok {
				mode = old.mode
			} else if fp, ok := findPlanFile(opts.Plan, path); ok {
				if src, ok := srcFiles[fp.SourcePath]; ok && src.mode == filemode.Executable {
					mode = filemode.Executable
				}
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
		// **HEAD 必须是指向分支的符号引用**。
		//
		// go-git v5.19 的 Checkout(Hash: ...) 内部走 setHEADToCommit,
		// 而它**无条件把 HEAD 写成游离的 hash 引用**(detached HEAD)。
		// 于是产物仓变成:refs/heads/main 指着新提交,HEAD 却是个游离 hash。
		// 推的内容没错(推的是 refs/heads/main),但产物仓的状态是错的:
		//   - 人在产物目录里提交,提交成了游离提交,**分支不会动**;
		//   - `git status` 显示 "HEAD detached",产物没法正常检视;
		//   - 门禁/核对脚本按 HEAD 取树时,取到的是游离态而不是分支。
		if err := workRepo.Storer.SetReference(plumbing.NewSymbolicReference(
			plumbing.HEAD, plumbing.NewBranchReferenceName(branch))); err != nil {
			return nil, fmt.Errorf("恢复 HEAD 符号引用失败: %w", err)
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

// applyPlan 是「哪些文件应用、按源提交怎么分组」的结论。
type applyPlan struct {
	// Groups 待应用的分组,按源提交时间排序。
	Groups []applyGroup
	// Skipped 未应用的文件路径。
	Skipped []string
	// ScrubHits 全部痕迹命中(含已自动清理的)。
	ScrubHits []ScrubHit
	// MandatoryHits 强制人工处理的痕迹命中。
	MandatoryHits []ScrubHit
}

// planApplyGroups 依据计划算出「哪些文件应用、按源提交怎么分组」。
//
// 参与移植的处置只有三种:port(直接移植)、port_with_scrub(清理痕迹后移植)、
// hold(有解法才移植)。其余处置(preserve/exclude/same)一律不碰目标树。
func planApplyGroups(ctx context.Context, opts ApplyOptions, replayer *mirror.Rewriter,
	srcRepo *git.Repository, srcFiles map[string]treeFile) (*applyPlan, error) {

	groups := make(map[string]*applyGroup)
	var order []string
	ap := &applyPlan{}

	for _, f := range opts.Plan.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch f.Disposition {
		case DispositionPort, DispositionPortWithScrub, DispositionHold:
		default:
			continue
		}
		if f.Disposition == DispositionHold {
			res, ok := opts.Plan.Resolutions[f.Path]
			if !ok || res.NeedsHuman() || res.Verdict == ResolutionTakeOurs {
				// 无解法、需人工、或「保留目标侧」→ 都不应用。
				ap.Skipped = append(ap.Skipped, f.Path)
				continue
			}
		}
		if f.OriginCommit == "" {
			ap.Skipped = append(ap.Skipped, f.Path)
			continue
		}
		// 痕迹检测:命中全部记录(自动清理类的已随内容发布),
		// 强制人工类的必须跳过该文件 —— 密钥不该被带进公开仓。
		scrub, err := applyFileContent(opts, replayer, srcRepo, srcFiles, f.Path)
		if err != nil {
			return nil, err
		}
		if len(scrub.Hits) > 0 {
			ap.ScrubHits = append(ap.ScrubHits, scrub.Hits...)
		}
		if len(scrub.MandatoryHits) > 0 {
			ap.MandatoryHits = append(ap.MandatoryHits, scrub.MandatoryHits...)
			ap.Skipped = append(ap.Skipped, f.Path)
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

	for _, hash := range order {
		g := groups[hash]
		sort.Strings(g.Files)
		ap.Groups = append(ap.Groups, *g)
	}
	sort.Strings(ap.Skipped)
	sort.SliceStable(ap.ScrubHits, func(i, j int) bool {
		if ap.ScrubHits[i].Line != ap.ScrubHits[j].Line {
			return ap.ScrubHits[i].Line < ap.ScrubHits[j].Line
		}
		return ap.ScrubHits[i].Rule < ap.ScrubHits[j].Rule
	})
	return ap, nil
}

// applyFileContent 计算文件在目标侧的发布形态:
// 源内容 → 身份改写 → gofmt 归一化 → 痕迹清理。
//
// 返回完整痕迹结果(不只是内容):自动清理类的命中在内容里已经看不出来了,
// 只看内容会漏掉「清理过什么」的审计证据。
func applyFileContent(opts ApplyOptions, replayer *mirror.Rewriter,
	srcRepo *git.Repository, srcFiles map[string]treeFile, path string) (ScrubResult, error) {

	fp, ok := findPlanFile(opts.Plan, path)
	if !ok {
		return ScrubResult{}, fmt.Errorf("计划里找不到 %s", path)
	}
	entry, ok := srcFiles[fp.SourcePath]
	if !ok {
		return ScrubResult{}, fmt.Errorf("源仓找不到 %s", fp.SourcePath)
	}
	content, err := readBlob(srcRepo, entry.hash)
	if err != nil {
		return ScrubResult{}, err
	}
	rewritten := replayer.PublishForm(fp.SourcePath, content)

	// 已解法的 hold 用解法算出的内容(take_theirs / merge_clean)。
	//
	// 用 **verdict** 判定,不用「内容是否为空」——
	// 合并的合法结果可能就是空(两侧都清空了这个文件),
	// 用空串判断会把被删掉的内容复活。
	if fp.Disposition == DispositionHold {
		if res, ok := opts.Plan.Resolutions[path]; ok &&
			(res.Verdict == ResolutionTakeTheirs || res.Verdict == ResolutionMergeClean) {
			rewritten = []byte(res.Content)
		}
	}

	return ApplyScrub(rewritten, opts.ScrubRules), nil
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

	// 作者身份用**公开形态**(配置值或默认),**不继承源提交的作者**:
	// 源侧是内网仓,作者是内网开发者(姓名 + 企业邮箱)——
	// 继承等于把内网人员信息发布到公开仓。
	// 时间戳仍继承源提交:同一输入两次移植得到同一 hash(幂等),
	// 且公开树保留源侧的时间线。
	name, email := opts.AuthorName, opts.AuthorEmail
	if strings.TrimSpace(name) == "" || strings.TrimSpace(email) == "" {
		name, email = DefaultPublicAuthor.Name, DefaultPublicAuthor.Email
	}
	author := object.Signature{Name: name, Email: email, When: time.Unix(g.When, 0).UTC()}
	committer := author
	if c, err := srcRepo.CommitObject(plumbing.NewHash(g.SourceCommit)); err == nil {
		author.When = c.Author.When.UTC()
		committer.When = c.Committer.When.UTC()
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

// productTTL 是**已推送成功**的移植产物(临时工作副本)的保留时长。
//
// 产物只在「apply 之后的那次 push」里有用,留久了纯占地方 ——
// 实测一次泄漏能攒到 5.48 GB(103 个目录)。
const productTTL = 24 * time.Hour

// pendingProductTTL 是**还没推送成功**的产物的保护期上限。
//
// apply 与 push 是两次独立调用,中间可能隔很久(人工流程、值班交接),
// 只按「多旧」清会把待推送的产物删掉 —— 推送必然失败,而且产物不可恢复
// (重做 apply 也会因为基线变了而得到不同的 head)。
//
// 但也不能永久保留:一直不推送的产物会重新变成泄漏。
// 所以给一个上限,到期后一律清掉(执行记录里还留着 head,能看出发生过什么)。
const pendingProductTTL = 7 * 24 * time.Hour

// productDirPrefix 是产物目录名前缀。
//
// 产物目录放在**调用方指定的目录**下(服务层用自己的数据目录),
// 不放系统共享临时目录 —— 否则清理逻辑会去动同机其它用户/进程的东西。
const productDirPrefix = "history-repo-"

// CleanupStaleProducts 清理过期的移植产物目录(best-effort,失败不阻断主流程)。
//
// root 是产物目录所在的根(服务层用自己的数据目录,不扫系统共享临时目录)。
// 只清 productDirPrefix 前缀的目录。
//
// keep 是「要保留的产物目录」——由调用方给出「还没推送成功」的那些。
// 不看这个就删,会把待推送的产物删掉,推送必然失败且不可恢复。
//
// 导出是因为推送路径也要触发一次:产物只有「apply 之后的那次 push」
// 与「之后的回滚」才用得上,但没人再执行时也该有人收拾。
func CleanupStaleProducts(root string, keep ...string) {
	if root == "" {
		return
	}
	protected := make(map[string]bool, len(keep))
	for _, k := range keep {
		if k != "" {
			protected[k] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), productDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := time.Since(info.ModTime())
		dir := filepath.Join(root, e.Name())
		if protected[dir] {
			// 还有没推送成功的记录等着用它 —— 但保护期也有上限,
			// 否则一直不推送的产物会重新变成泄漏。
			if age > pendingProductTTL {
				_ = os.RemoveAll(dir)
			}
			continue
		}
		if age > productTTL {
			_ = os.RemoveAll(dir)
		}
	}
}

// DefaultPublicAuthor 是目标侧新提交的默认作者身份(公开形态)。
//
// 刻意**不指向任何真实的人**:源侧提交的作者是内网开发者,
// 继承过来等于把内网人员信息(姓名 + 企业邮箱)发布到公开仓。
var DefaultPublicAuthor = object.Signature{
	Name:  "git-ferry",
	Email: "git-ferry@users.noreply.github.com",
}
