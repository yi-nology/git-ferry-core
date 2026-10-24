package history

import (
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffHunk 是相对共同基点(base)的一处改动。
type diffHunk struct {
	// BaseStart 在 base 中的起始行(含)。
	BaseStart int
	// BaseEnd 在 base 中的结束行(不含)。等于 BaseStart 表示纯插入。
	BaseEnd int
	// Lines 改动后的行(该侧的新内容)。
	Lines []string
}

// isInsert 判断是否为纯插入(不消耗 base 的行)。
func (h diffHunk) isInsert() bool { return h.BaseStart == h.BaseEnd }

// hunksConflict 判断两个 hunk 是否落在同一处改动上(必须一起裁决)。
//
// 统一规则:把每个 hunk 看成 base 上的**闭区间** [BaseStart, BaseEnd]
// (纯插入 → 退化成一个点),两个闭区间相交即冲突。
//
// 这条规则覆盖了全部形态,已用 `git merge-file --diff3` 差分对拍校准:
//   - 两侧都替换 base 的行 → 区间相交即冲突;
//   - 紧邻(中间没有未改动的行)也算相交 → git 会把两块合成一块;
//   - 一侧是纯插入 → 插入点落在另一侧区间的**闭区间** [s,e] 内即冲突
//     (含两端)。端点算在内是关键:插入点正好落在被改区间的边界上,
//     改的是同一处语义。漏判会把两边的内容都应用,产出既非 ours
//     也非 theirs 的错误结果(实测:目标侧已脱敏的域名会被源侧真实域名覆盖);
//   - 两侧都是纯插入 → 插入点相同即冲突。
func hunksConflict(a, b diffHunk) bool {
	return a.BaseStart <= b.BaseEnd && b.BaseStart <= a.BaseEnd
}

// diffRegion 是一组相互冲突的改动构成的区域。
//
// 区域是 diff3 的基本单位:区域内若两侧都改过且改法不同 → 冲突;
// 只有一侧改过 → 直接采用那侧;两侧改法相同 → 采用一次。
type diffRegion struct {
	// BaseStart / BaseEnd 区域在 base 中的行范围(含/不含)。
	BaseStart int
	BaseEnd   int
	// Base 区域在 base 中的原始内容。
	Base []string
	// Ours 区域在 ours 侧的内容。
	Ours []string
	// Theirs 区域在 theirs 侧的内容。
	Theirs []string
	// OursChanged / TheirsChanged 两侧是否改动过该区域。
	OursChanged   bool
	TheirsChanged bool
}

// 冲突标记用 **git 原生标签**(ours / base / theirs):
// git mergetool、`git checkout --conflict=diff3` 这类以 git 冲突标记为输入的
// 人工裁决流程才认得;自定义中文标签会让它们解析失败。
//
// ours = 目标侧当前内容(已发布的那份),theirs = 源侧内容(要合进来的那份),
// base = 共同基点 —— 与 git 的语义一致。
//
// merge3 是 diff3 风格的三向合并。
//
// 语义与 `git merge-file --diff3` 对齐:
//   - 两侧只改了一侧 → 采用那侧;
//   - 两侧改成同样内容 → 采用一次;
//   - 两侧改成不同内容 → 产出冲突标记,含 base / ours / theirs 三段;
//   - 插入点落在另一侧改动的边界上 → 同样判冲突(见 hunksConflict)。
//
// 返回合并结果与冲突块数。冲突时结果含 diff3 标记,供人/AI 裁决。
func merge3(base, ours, theirs string) (string, int) {
	merged, conflicts, escalated := merge3Result(base, ours, theirs)
	if escalated {
		// 不变式被打破:整份文件作为一个冲突交给人看。
		// 返回原文(而非整文件冲突)是为了让测试能对「升级前」的结果断言 ——
		// 否则重算丢行检测必然为空,那条断言就成了永远绿的假绿。
		return wholeFileConflict(base, ours, theirs), conflicts
	}
	return merged, conflicts
}

// merge3Result 是 merge3 的内部形态:额外返回「是否升级为整文件冲突」。
//
// 拆出来是为了让测试能拿到**升级前**的合并结果 ——
// 不变式触发时 merge3 会换成整文件冲突,那里面内嵌完整的 ours+theirs,
// 对它重算「有没有丢行」必然得到空,断言就成了假绿。
func merge3Result(base, ours, theirs string) (merged string, conflicts int, escalated bool) {
	if ours == theirs {
		return ours, 0, false
	}
	if base == ours {
		return theirs, 0, false
	}
	if base == theirs {
		return ours, 0, false
	}

	baseLines := splitLines(base)
	oursLines := splitLines(ours)
	theirsLines := splitLines(theirs)

	regions := buildRegions(baseLines, oursLines, theirsLines)

	var out []string
	conflicts = 0
	pos := 0
	for _, r := range regions {
		if r.BaseStart > pos {
			out = append(out, baseLines[pos:r.BaseStart]...)
		}
		switch {
		case r.OursChanged && r.TheirsChanged:
			if sameLines(r.Ours, r.Theirs) {
				// 两侧改成同样内容 → 采用一次。
				out = append(out, r.Ours...)
			} else {
				conflicts++
				out = append(out, "<<<<<<< ours")
				out = append(out, r.Ours...)
				out = append(out, "||||||| base")
				out = append(out, r.Base...)
				out = append(out, "=======")
				out = append(out, r.Theirs...)
				out = append(out, ">>>>>>> theirs")
			}
		case r.OursChanged:
			out = append(out, r.Ours...)
		case r.TheirsChanged:
			out = append(out, r.Theirs...)
		default:
			out = append(out, r.Base...)
		}
		pos = r.BaseEnd
	}
	if pos < len(baseLines) {
		out = append(out, baseLines[pos:]...)
	}

	merged = strings.Join(out, "\n")
	// 文件内容以 "\n" 结尾时,**每一行**都要带换行符。
	//
	// 不能靠「拼出来字符串是否以 \n 结尾」来判断:末行是空行时,
	// Join(["", "c", ""]) = "\nc\n" 已经以 \n 结尾,但正确内容是
	// "\nc\n\n" —— 少一个换行,文件末尾的空行就没了。
	// (实测:随机对拍里 2.3% 的「内容与 git 不同」全是这一类。)
	if len(out) > 0 &&
		(strings.HasSuffix(base, "\n") || strings.HasSuffix(ours, "\n") || strings.HasSuffix(theirs, "\n")) {
		merged += "\n"
	}

	// **不变式**:合并结果不能丢任何一侧「新增的行」。
	//
	// 这条校验**不依赖 diff 的对齐方式**,是对齐歧义的兜底:
	// diff 在内容重复时可能把同一处插入归到不同的 base 位置
	// (实测:3000 组随机用例里 git 判冲突而本实现判干净的有 38 组,
	//  但「内容被静默丢弃」的是 0 组 —— 差异只是冲突粒度)。
	// 万一将来有人改坏了区域内容计算,这条校验会把「静默丢失」
	// 变成「明确冲突」,而不是让内容悄悄消失。
	//
	// 命中即保守升级:整份文件作为一个冲突交给人看。
	if dropped := droppedAddedLines(base, ours, theirs, merged); len(dropped) > 0 {
		return merged, conflicts, true
	}
	return merged, conflicts, false
}

// droppedAddedLines 返回「某侧新增、但合并结果里不见了」的行。
//
// 只看**该侧新增且另一侧没有**的行:两侧都新增的同一行不算,
// 一侧新增、另一侧也有的也不算(那行本来就在)。
func droppedAddedLines(base, ours, theirs, merged string) []string {
	var dropped []string
	mergedLines := splitLines(merged)
	check := func(side, other string) {
		for _, l := range exclusiveLines(side, base) {
			// 空行也要参与判定:空行是完全合法的一行,
			// 漏掉它就看不见「空行被吞了」。
			if containsLine(mergedLines, l) || containsLine(splitLines(other), l) {
				continue
			}
			dropped = append(dropped, l)
		}
	}
	check(theirs, ours)
	check(ours, theirs)
	return dropped
}

// containsLine 判断行集合里有没有这一行。
func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// wholeFileConflict 把整份文件做成一个冲突块(保守兜底)。
//
// 只在不变式被打破时才会走到 —— 那意味着合并结果不可信,
// 与其发布一个可能丢内容的树,不如整份交给人看。
func wholeFileConflict(base, ours, theirs string) string {
	var b strings.Builder
	b.WriteString("<<<<<<< ours\n")
	b.WriteString(ensureTrailingNewline(ours))
	b.WriteString("||||||| base\n")
	b.WriteString(ensureTrailingNewline(base))
	b.WriteString("=======\n")
	b.WriteString(ensureTrailingNewline(theirs))
	b.WriteString(">>>>>>> theirs\n")
	return b.String()
}

// ensureTrailingNewline 补上结尾换行。
//
// 不补的话,原文件末尾没有换行时,分节标记会粘到内容行上
// (`X||||||| base` / `b=======`),人读不懂、git 工具也解析不了 ——
// 而冲突标记用 git 原生格式的目的正是让 git 工具能解析。
func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// buildRegions 把两侧的改动块分组为冲突区域。
//
// 分组用并查集:任意两个 hunk 冲突 → 归入同一区域(传递闭包)。
// 之所以要传递闭包:一侧的「插入」与另一侧的「替换」冲突,而该侧的
// 「替换」又与本侧的相邻改动相邻 —— 不做闭包会切出多余的碎片区域,
// 让本该一整块的冲突被拆散。
func buildRegions(base, ours, theirs []string) []diffRegion {
	hOurs := diffHunks(base, ours)
	hTheirs := diffHunks(base, theirs)

	type node struct {
		h    diffHunk
		ours bool
	}
	nodes := make([]node, 0, len(hOurs)+len(hTheirs))
	for _, h := range hOurs {
		nodes = append(nodes, node{h: h, ours: true})
	}
	for _, h := range hTheirs {
		nodes = append(nodes, node{h: h, ours: false})
	}
	if len(nodes) == 0 {
		return nil
	}

	parent := make([]int, len(nodes))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	for i := 0; i < len(nodes); i++ {
		for j := i + 1; j < len(nodes); j++ {
			if hunksConflict(nodes[i].h, nodes[j].h) {
				union(i, j)
			}
		}
	}

	// 按根分组。
	groups := map[int][]int{}
	for i := range nodes {
		r := find(i)
		groups[r] = append(groups[r], i)
	}

	regions := make([]diffRegion, 0, len(groups))
	for _, idxs := range groups {
		r := diffRegion{
			BaseStart: nodes[idxs[0]].h.BaseStart,
			BaseEnd:   nodes[idxs[0]].h.BaseEnd,
		}
		var oursHunks, theirsHunks []diffHunk
		for _, i := range idxs {
			h := nodes[i].h
			if h.BaseStart < r.BaseStart {
				r.BaseStart = h.BaseStart
			}
			if h.BaseEnd > r.BaseEnd {
				r.BaseEnd = h.BaseEnd
			}
			if nodes[i].ours {
				r.OursChanged = true
				oursHunks = append(oursHunks, h)
			} else {
				r.TheirsChanged = true
				theirsHunks = append(theirsHunks, h)
			}
		}
		if r.BaseStart < 0 {
			r.BaseStart = 0
		}
		if r.BaseEnd > len(base) {
			r.BaseEnd = len(base)
		}
		if r.BaseStart > r.BaseEnd {
			r.BaseStart = r.BaseEnd
		}
		r.Base = base[r.BaseStart:r.BaseEnd]
		sort.SliceStable(oursHunks, func(i, j int) bool { return oursHunks[i].BaseStart < oursHunks[j].BaseStart })
		sort.SliceStable(theirsHunks, func(i, j int) bool { return theirsHunks[i].BaseStart < theirsHunks[j].BaseStart })
		r.Ours = sideContent(base, r.BaseStart, r.BaseEnd, oursHunks)
		r.Theirs = sideContent(base, r.BaseStart, r.BaseEnd, theirsHunks)
		regions = append(regions, r)
	}

	// 区域按 base 位置排序,输出时才能顺序拼接。
	sort.SliceStable(regions, func(i, j int) bool { return regions[i].BaseStart < regions[j].BaseStart })
	// 把「同一处插入被两侧各记一次」的区域合成一块(见 coalesceSameInsertion)。
	return coalesceSameInsertion(regions, base, hOurs, hTheirs)
}

// sameInsertionGap 是「两处插入算同一处」的 base 位置距离上限。
//
// 取 2 的依据:插入点相邻(距离 1)或隔一行(距离 2)时,
// 内容重复会让 diff 的锚点产生歧义(插入的行与附近同样的行无法区分)。
// 距离更大就不太可能是同一处改动了。
const sameInsertionGap = 2

// coalesceSameInsertion 把「同一处插入被两侧各记一次」的区域合成一块。
//
// ## 为什么需要
//
// diff 在内容重复时,最短编辑脚本不唯一 —— 插入的那一行与附近同样的行
// 无法区分,于是同一处插入可能被两侧锚到**不同的 base 位置**。
// 锚点不同 → 两个区域不相交 → 各自应用 → **同一份内容被插入两遍**。
//
// 实测(最小复现):
//
//	base   = a b a a b
//	ours   = b a a a b      # 改首行 + 插入一个 a
//	theirs = a b a a a b    # 只插入一个 a
//
// 一侧把插入锚到 base[2]、另一侧锚到 base[4] → 两个插入都应用
// → 合并结果多出一个 a(而且两边都判「干净」,无冲突标记,直接发布)。
// git 的 xdiff 把两侧锚到同一处,于是只应用一次。
//
// ## 怎么合成
//
// 两个区域相邻(中间没有别的改动)、一侧只改 ours、另一侧只改 theirs、
// 且两侧插入的**内容完全相同**、base 位置相距不超过 sameInsertionGap
// → 合成一块。合成后:
//   - 两侧内容相同 → 只应用一次 ✓(与 git 一致);
//   - 两侧内容不同 → 判冲突 → 交给人看(保守但安全)。
//
// 只在「两侧内容相同」时合成,避免把两处真正不同位置的同内容插入
// 误并成一处(那种情况由合并后的不变式兜底:真丢了内容会升级成冲突)。
func coalesceSameInsertion(regions []diffRegion, base []string,
	hOurs, hTheirs []diffHunk) []diffRegion {

	out := make([]diffRegion, 0, len(regions))
	for i := 0; i < len(regions); i++ {
		r := regions[i]
		if i+1 < len(regions) {
			if next, ok := pairAsSameInsertion(base, r, regions[i+1]); ok {
				out = append(out, mergeRegionPair(r, next, base, hOurs, hTheirs))
				i++
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// pairAsSameInsertion 判断两个相邻区域是否是「同一处插入被两侧各记一次」。
func pairAsSameInsertion(base []string, a, b diffRegion) (diffRegion, bool) {
	// 必须一个只改 ours、一个只改 theirs。
	//
	// **方向不限**:ours 的锚点可能在后、theirs 的锚点可能在前 ——
	// 只认「ours 在前」的话,把两侧对调就漏判,同一处插入会被应用两遍
	// 且判干净 → 直接发布到公开仓。
	aOursOnly := a.OursChanged && !a.TheirsChanged
	aTheirsOnly := a.TheirsChanged && !a.OursChanged
	bOursOnly := b.OursChanged && !b.TheirsChanged
	bTheirsOnly := b.TheirsChanged && !b.OursChanged
	if !((aOursOnly && bTheirsOnly) || (aTheirsOnly && bOursOnly)) {
		return diffRegion{}, false
	}
	// base 位置要近。
	gap := b.BaseStart - a.BaseEnd
	if gap < 0 {
		gap = -gap
	}
	if gap > sameInsertionGap {
		return diffRegion{}, false
	}
	// 插入的内容必须完全相同(取 ours 那侧与 theirs 那侧)。
	oursContent, theirsContent := a.Ours, b.Theirs
	if aTheirsOnly {
		oursContent, theirsContent = b.Ours, a.Theirs
	}
	if !sameLines(oursContent, theirsContent) {
		return diffRegion{}, false
	}
	// **只有锚点真有歧义时才合成**。
	//
	// 歧义来自「插入的那一行,与 base 里同样的行无法区分」——
	// 于是同一处插入可能被两侧锚到不同的 base 位置。
	// 插入的内容在 base 附近**根本不存在**时,锚点是唯一的:
	// 两处插入就是两处,合成会造出假冲突(实测:base=a b c,
	// 两侧分别在 b 前/后插入 X → git 干净合并,合成后却报冲突)。
	if !insertionAmbiguous(base, a, b, oursContent) {
		return diffRegion{}, false
	}
	return b, true
}

// insertionAmbiguous 判断插入内容在 base 附近是否**本来就存在**。
//
// 只看两个锚点之间(含端点)的那段 base:歧义正是「这段里有一行
// 和插入的内容一样」造成的。
func insertionAmbiguous(base []string, a, b diffRegion, content []string) bool {
	from, to := a.BaseStart, b.BaseEnd
	if from > to {
		from, to = to, from
	}
	if from < 0 {
		from = 0
	}
	if to < len(base) {
		to++ // 含终点
	}
	if to > len(base) {
		to = len(base)
	}
	for _, line := range content {
		if line == "" {
			continue
		}
		for _, bline := range base[from:to] {
			if bline == line {
				return true
			}
		}
	}
	return false
}

// mergeRegionPair 把两个区域合成一块,重新计算两侧的内容。
func mergeRegionPair(a, b diffRegion, base []string,
	hOurs, hTheirs []diffHunk) diffRegion {

	m := diffRegion{
		BaseStart:     a.BaseStart,
		BaseEnd:       b.BaseEnd,
		OursChanged:   true,
		TheirsChanged: true,
	}
	m.Base = base[m.BaseStart:m.BaseEnd]
	// 区间取**闭区间**:插在区域终点的插入也属于这块
	// (否则那一侧的内容会少掉插入的那几行,合成后就不是「同一处改动」了)。
	m.Ours = sideContent(base, m.BaseStart, m.BaseEnd, hunksWithin(hOurs, m.BaseStart, m.BaseEnd))
	m.Theirs = sideContent(base, m.BaseStart, m.BaseEnd, hunksWithin(hTheirs, m.BaseStart, m.BaseEnd))
	return m
}

// hunksWithin 取属于区域 [from,to] 的改动块。
//
// 纯插入:落点在区间内(含两端)即属于这块 ——
// 插在区域终点的插入也是「这一处改动」的一部分。
//
// 替换块:必须**完整**落在区间内 ——
// 跨边界的替换属于别的区域,算进来会让区域内容多出一截。
func hunksWithin(hunks []diffHunk, from, to int) []diffHunk {
	var out []diffHunk
	for _, h := range hunks {
		if h.isInsert() {
			if h.BaseStart >= from && h.BaseStart <= to {
				out = append(out, h)
			}
			continue
		}
		if h.BaseStart >= from && h.BaseEnd <= to {
			out = append(out, h)
		}
	}
	return out
}

// sideContent 计算某一侧在区域 [from,to) 上的内容。
//
// 规则:
//   - 区域内的替换块 → 用该侧的新内容;
//   - 区域内的插入块 → 插在对应位置;
//   - 插入点正好落在区域起点 → 排在区域内容**最前**;
//   - 插入点正好落在区域终点 → 排在区域内容**最后**。
func sideContent(base []string, from, to int, hunks []diffHunk) []string {
	out := make([]string, 0, to-from)
	pos := from
	for _, h := range hunks {
		if h.BaseStart > pos {
			out = append(out, base[pos:h.BaseStart]...)
		}
		out = append(out, h.Lines...)
		if h.BaseEnd > pos {
			pos = h.BaseEnd
		}
	}
	if pos < to {
		out = append(out, base[pos:to]...)
	}
	return out
}

// sameLines 判断两段行是否完全相同。
func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffHunks 计算 base → other 的改动块。
func diffHunks(base, other []string) []diffHunk {
	if len(base) == 0 && len(other) == 0 {
		return nil
	}
	dmp := diffmatchpatch.New()
	r1, r2, lineArray := dmp.DiffLinesToRunes(joinLines(base), joinLines(other))
	diffs := dmp.DiffMain(string(r1), string(r2), false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)
	return hunksFromDiffs(diffs)
}

// hunksFromDiffs 把 diff 结果转成改动块。
func hunksFromDiffs(diffs []diffmatchpatch.Diff) []diffHunk {
	var hunks []diffHunk
	basePos := 0
	i := 0
	for i < len(diffs) {
		d := diffs[i]
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			basePos += len(splitLines(d.Text))
			i++
		case diffmatchpatch.DiffDelete, diffmatchpatch.DiffInsert:
			start := basePos
			var lines []string
			for i < len(diffs) && diffs[i].Type != diffmatchpatch.DiffEqual {
				switch diffs[i].Type {
				case diffmatchpatch.DiffDelete:
					basePos += len(splitLines(diffs[i].Text))
				case diffmatchpatch.DiffInsert:
					lines = append(lines, splitLines(diffs[i].Text)...)
				}
				i++
			}
			hunks = append(hunks, diffHunk{BaseStart: start, BaseEnd: basePos, Lines: lines})
		}
	}
	return hunks
}

// splitLines 把文本按行拆开(保留空行,不保留结尾换行产生的空元素)。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// joinLines 把行拼回文本(行间用 \n,结尾补 \n)。
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
