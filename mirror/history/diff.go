package history

import (
	"bytes"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffVerdict 是两侧内容的差异方向判定。
type diffVerdict int

const (
	// diffIdentical 两侧一致。
	diffIdentical diffVerdict = iota
	// diffSourceAhead 源侧是目标侧的超集:只有新增,没有删改。
	diffSourceAhead
	// diffTargetAhead 目标侧是源侧的超集:只有删除,没有新增。
	diffTargetAhead
	// diffMixed 两侧各有独有内容,方向无法机械判定。
	diffMixed
)

// maxDiffLines 是行模式 diff 的行数上限。
//
// 行模式把每一行编码成一个 rune 来做 diff,行数超过 rune 上限(65535)
// 会产生非法 rune,编码随之失真。超限时退化为保守判定:
// 返回 diffMixed(→ hold),宁可交给人,不可给错方向。
const maxDiffLines = 60000

// classifyDiff 比较 target(目标侧现状)与 source(源侧,已身份改写)的内容,
// 判定差异方向,并给出相对目标侧的增删行数。
//
// 方向判定的意义:同步是单向的,目标侧领先的内容不能被覆盖。
// 只有当源侧严格包含目标侧全部内容(只增不删)时,覆盖才是安全的;
// 目标侧领先或两侧各有独有内容时,一律不改。
func classifyDiff(target, source []byte) (diffVerdict, int, int) {
	if bytes.Equal(target, source) {
		return diffIdentical, 0, 0
	}
	if countLines(target)+countLines(source) > maxDiffLines {
		return diffMixed, 0, 0
	}

	dmp := diffmatchpatch.New()
	runes1, runes2, _ := dmp.DiffLinesToRunes(string(target), string(source))
	diffs := dmp.DiffMain(string(runes1), string(runes2), false)

	added, removed := 0, 0
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			// 只在源侧出现的行。
			added += len([]rune(d.Text))
		case diffmatchpatch.DiffDelete:
			// 只在目标侧出现的行。
			removed += len([]rune(d.Text))
		}
	}

	switch {
	case added == 0 && removed == 0:
		return diffIdentical, 0, 0
	case removed == 0:
		return diffSourceAhead, added, removed
	case added == 0:
		return diffTargetAhead, added, removed
	default:
		return diffMixed, added, removed
	}
}

// countLines 统计文本行数,用作行模式 diff 的规模上界。
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
