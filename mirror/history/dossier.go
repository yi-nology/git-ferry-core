package history

import (
	"fmt"
	"sort"
	"strings"
)

// ConflictDossier 是单个未解决冲突的完整卷宗。
//
// **core 不做语义判断** —— 它的职责是把「要裁决的东西」整理成
// 结构化、可解释、可交给 AI 或人看的形态。
// 裁决本身要么由 AI(语义判断)给出,要么由人确认。
type ConflictDossier struct {
	// Path 文件路径(目标侧)。
	Path string
	// SourcePath 源侧路径。
	SourcePath string
	// Verdict 解法类型(conflict / no_base)。
	Verdict ResolutionVerdict
	// Reason 判定依据。
	Reason string
	// Base 共同基点内容(no_base 时为空)。
	Base string
	// Ours 目标侧当前内容。
	Ours string
	// Theirs 源侧当前内容(已身份改写)。
	Theirs string
	// Merged 三向合并结果(含冲突标记)。
	Merged string
	// Conflicts 冲突块数。
	Conflicts int
	// OursOnly 目标侧独有的行(源侧没有的)。
	OursOnly []string
	// TheirsOnly 源侧独有的行(目标侧没有的)。
	TheirsOnly []string
}

// Dossiers 返回仍需人工裁决的文件卷宗,按路径升序。
//
// 判据:解法为 conflict(真冲突)或 no_base(找不到共同版本)。
func Dossiers(plan *Plan) []ConflictDossier {
	if plan == nil {
		return nil
	}
	var out []ConflictDossier
	for _, f := range plan.Files {
		res, ok := plan.Resolutions[f.Path]
		if !ok || !res.NeedsHuman() {
			continue
		}
		d := ConflictDossier{
			Path:       f.Path,
			SourcePath: f.SourcePath,
			Verdict:    res.Verdict,
			Reason:     res.Reason,
			Base:       res.Base,
			Ours:       res.Ours,
			Theirs:     res.Theirs,
			Merged:     res.Merged,
			Conflicts:  res.Conflicts,
		}
		d.OursOnly = exclusiveLines(res.Ours, res.Theirs)
		d.TheirsOnly = exclusiveLines(res.Theirs, res.Ours)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// exclusiveLines 返回 a 中有、b 中没有的行(按出现顺序,去重)。
//
// 这是给裁决者的核心信息:「这一边多出来的到底是什么」。
func exclusiveLines(a, b string) []string {
	countB := make(map[string]int)
	for _, l := range splitLines(b) {
		countB[l]++
	}
	var out []string
	seen := make(map[string]bool)
	for _, l := range splitLines(a) {
		if countB[l] > 0 {
			countB[l]--
			continue
		}
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// Markdown 把卷宗渲染成可交给 AI 或人审阅的 Markdown。
func (d ConflictDossier) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### `%s`\n\n", d.Path)
	fmt.Fprintf(&b, "- 解法状态:**%s**(%s)\n", d.Verdict, d.Reason)
	fmt.Fprintf(&b, "- 源侧路径:`%s`\n", d.SourcePath)
	fmt.Fprintf(&b, "- 冲突块数:%d\n", d.Conflicts)

	if len(d.OursOnly) > 0 {
		fmt.Fprintf(&b, "\n**目标侧独有**(%d 行):\n\n", len(d.OursOnly))
		for _, l := range d.OursOnly {
			fmt.Fprintf(&b, "- `%s`\n", strings.TrimSpace(l))
		}
	}
	if len(d.TheirsOnly) > 0 {
		fmt.Fprintf(&b, "\n**源侧独有**(%d 行):\n\n", len(d.TheirsOnly))
		for _, l := range d.TheirsOnly {
			fmt.Fprintf(&b, "- `%s`\n", strings.TrimSpace(l))
		}
	}
	if d.Merged != "" {
		b.WriteString("\n**三向合并结果**(含冲突标记):\n\n```diff\n")
		b.WriteString(d.Merged)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// DossiersMarkdown 把一组卷宗渲染成一份 Markdown 文档。
func DossiersMarkdown(dossiers []ConflictDossier) string {
	if len(dossiers) == 0 {
		return "# 无待裁决冲突\n\n所有 hold 文件均已自动解决。\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 待裁决冲突(%d 个)\n\n", len(dossiers))
	b.WriteString("这些文件两边都改过,机械合并无法判定,需要语义判断(AI 建议 + 人工确认)。\n\n")
	for _, d := range dossiers {
		b.WriteString(d.Markdown())
		b.WriteString("\n")
	}
	return b.String()
}
