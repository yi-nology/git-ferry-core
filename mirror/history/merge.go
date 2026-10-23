package history

import (
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffHunk 是相对共同基点(base)的一处改动。
type diffHunk struct {
	// BaseStart 在 base 中的起始行号(含)。
	BaseStart int
	// BaseEnd 在 base 中的结束行号(不含)。等于 BaseStart 表示纯插入。
	BaseEnd int
	// Lines 改动后的行(相对 ours/theirs 一侧)。
	Lines []string
}

// merge3 是 diff3 风格的三向合并。
//
// 两棵树是**同源改写副本**:公开树的每个提交都是内部树对应提交改写后的重做。
// 因此两边存在**内容级共同基点** —— 某个历史版本的内容在两边同时出现。
// 有了基点,就能对「两边都改过的文件」做真正的三方合并,而不是让人肉裁决。
//
// 返回合并结果与冲突数。冲突时:重叠区域取 ours(目标侧),
// 并在结果中插入冲突标记,便于人工收拾。
func merge3(base, ours, theirs string) (string, int) {
	if ours == theirs {
		return ours, 0
	}
	if base == ours {
		return theirs, 0
	}
	if base == theirs {
		return ours, 0
	}

	baseLines := splitLines(base)
	hunksOurs := diffHunks(baseLines, splitLines(ours))
	hunksTheirs := diffHunks(baseLines, splitLines(theirs))

	out := make([]string, 0, len(baseLines))
	conflicts := 0
	oi, ti := 0, 0
	pos := 0

	flush := func(from, to int) {
		if to > from {
			out = append(out, baseLines[from:to]...)
		}
	}
	apply := func(h diffHunk) {
		out = append(out, h.Lines...)
	}

	for oi < len(hunksOurs) || ti < len(hunksTheirs) {
		// 取下一个待处理 hunk 中基点位置更靠前的一个。
		var h *diffHunk
		var isOurs bool
		switch {
		case oi >= len(hunksOurs):
			h, isOurs = &hunksTheirs[ti], false
			ti++
		case ti >= len(hunksTheirs):
			h, isOurs = &hunksOurs[oi], true
			oi++
		case hunksOurs[oi].BaseStart <= hunksTheirs[ti].BaseStart:
			h, isOurs = &hunksOurs[oi], true
			oi++
		default:
			h, isOurs = &hunksTheirs[ti], false
			ti++
		}

		// 检查是否与另一侧尚未处理的 hunk 重叠。
		var other *diffHunk
		if isOurs {
			if ti < len(hunksTheirs) {
				other = &hunksTheirs[ti]
			}
		} else {
			if oi < len(hunksOurs) {
				other = &hunksOurs[oi]
			}
		}
		if other != nil && hunksOverlap(*h, *other) {
			// 重叠 → 冲突:整段取 ours,并标记。
			conflicts++
			flush(pos, h.BaseStart)
			out = append(out, "<<<<<<< 目标侧(保留)")
			out = append(out, oursSideLines(baseLines, hunksOurs, h.BaseStart, h.BaseEnd)...)
			out = append(out, "=======")
			out = append(out, theirsSideLines(baseLines, hunksTheirs, h.BaseStart, h.BaseEnd)...)
			out = append(out, ">>>>>>> 源侧")
			pos = h.BaseEnd
			// 消耗掉重叠的另一侧 hunk。
			if isOurs {
				ti++
			} else {
				oi++
			}
			continue
		}

		flush(pos, h.BaseStart)
		apply(*h)
		pos = h.BaseEnd
	}
	flush(pos, len(baseLines))
	merged := strings.Join(out, "\n")
	// 保留结尾换行:git 的文件内容以 "\n" 结尾,合并结果不该把它吃掉。
	if !strings.HasSuffix(merged, "\n") &&
		(strings.HasSuffix(base, "\n") || strings.HasSuffix(ours, "\n") || strings.HasSuffix(theirs, "\n")) {
		merged += "\n"
	}
	return merged, conflicts
}

// hunksOverlap 判断两个 hunk 在 base 上的范围是否重叠或相接。
func hunksOverlap(a, b diffHunk) bool {
	return a.BaseStart < b.BaseEnd && b.BaseStart < a.BaseEnd
}

// oursSideLines 取出冲突区域里 ours 一侧的内容。
func oursSideLines(base []string, hunks []diffHunk, start, end int) []string {
	for _, h := range hunks {
		if h.BaseStart == start && h.BaseEnd == end {
			return h.Lines
		}
	}
	return nil
}

// theirsSideLines 取出冲突区域里 theirs 一侧的内容。
func theirsSideLines(base []string, hunks []diffHunk, start, end int) []string {
	for _, h := range hunks {
		if h.BaseStart == start && h.BaseEnd == end {
			return h.Lines
		}
	}
	return nil
}

// diffHunks 计算 base → other 的改动块。
func diffHunks(base, other []string) []diffHunk {
	if len(base) == 0 && len(other) == 0 {
		return nil
	}
	dmp := diffmatchpatch.New()
	r1, r2, lineArray := dmp.DiffLinesToRunes(joinLines(base), joinLines(other))
	diffs := dmp.DiffMain(string(r1), string(r2), false)
	// 必须转回行文本:DiffLinesToRunes 把每行编码成一个 rune,
	// 直接用编码串会拿到"行号字符"而不是内容。
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

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
