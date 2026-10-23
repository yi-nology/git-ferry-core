package history

import (
	"fmt"
	"sort"
	"strings"
)

// Markdown 把计划渲染成可直接审阅的 Markdown 报告。
//
// 报告按处置分组:需要动作的在前,受保护的在后,一致的文件不列——
// 审阅者关心的是「要做什么」和「不能动什么」。
func (p *Plan) Markdown() string {
	var b strings.Builder

	b.WriteString("# 历史同步计划\n\n")
	b.WriteString("## 统计\n\n")
	b.WriteString("| 处置 | 含义 | 数量 |\n|---|---|---|\n")
	for _, row := range []struct {
		d    Disposition
		desc string
	}{
		{DispositionPort, "源侧超集,需覆盖目标"},
		{DispositionPortWithScrub, "需清理痕迹后移植"},
		{DispositionHold, "两侧各有独有内容,待人工确认"},
		{DispositionExclude, "命中排除规则,不参与同步"},
		{DispositionPreserve, "仅目标侧存在,受保护"},
		{DispositionSame, "两侧一致,无需动作"},
	} {
		fmt.Fprintf(&b, "| `%s` | %s | %d |\n", row.d, row.desc, p.countOf(row.d))
	}
	fmt.Fprintf(&b, "\n合计 %d 个路径;痕迹命中 %d 处;相对目标侧 +%d / -%d 行。\n",
		p.Summary.Total, p.Summary.ScrubHits, p.Summary.AddedLines, p.Summary.RemovedLines)

	writeSection := func(title string, d Disposition) {
		items := p.filesOf(d)
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n## %s（%d）\n\n", title, len(items))
		b.WriteString("| 路径 | 原因 | 增/删 | 来源提交 | 痕迹 |\n|---|---|---|---|---|\n")
		for _, f := range items {
			fmt.Fprintf(&b, "| `%s` | %s | +%d / -%d | %s | %s |\n",
				f.Path, f.Reason, f.AddedLines, f.RemovedLines,
				shortCommit(f.OriginCommit, f.OriginSubject), scrubSummary(f))
		}
	}

	writeSection("需要移植", DispositionPort)
	writeSection("需要清理后移植", DispositionPortWithScrub)
	writeSection("待人工确认", DispositionHold)
	writeSection("已排除", DispositionExclude)
	writeSection("受保护(不得覆盖)", DispositionPreserve)

	if len(p.Commits) > 0 {
		b.WriteString("\n## 来源提交分组\n\n")
		b.WriteString("| 提交 | 标题 | 文件数 | 聚合处置 |\n|---|---|---|---|\n")
		for _, c := range p.Commits {
			fmt.Fprintf(&b, "| `%s` | %s | %d | `%s` |\n",
				shortHash(c.Hash), c.Subject, len(c.Files), c.Disposition)
		}
	}

	if len(p.Resolutions) > 0 {
		s := SummarizeResolutions(p.Resolutions)
		b.WriteString("\n## hold 解法\n\n")
		fmt.Fprintf(&b,
			"共同基点 %d/%d 可机械解决(取源侧 %d、保留目标侧 %d、自动合并 %d);"+
				"仍需人工 %d(真冲突 %d、无基点 %d)。\n\n",
			s.TakeTheirs+s.TakeOurs+s.MergeClean, s.Total,
			s.TakeTheirs, s.TakeOurs, s.MergeClean,
			s.Conflict+s.NoBase, s.Conflict, s.NoBase)
		b.WriteString("| 路径 | 解法 | 依据 |\n|---|---|---|\n")
		for _, line := range ResolutionLines(p.Resolutions) {
			parts := splitTabs(line)
			if len(parts) != 3 {
				continue
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", parts[0], parts[1], parts[2])
		}
	}

	return b.String()
}

// splitTabs 按制表符拆分。
func splitTabs(s string) []string {
	return strings.Split(s, "\t")
}

// countOf 统计某处置的文件数。
func (p *Plan) countOf(d Disposition) int {
	n := 0
	for _, f := range p.Files {
		if f.Disposition == d {
			n++
		}
	}
	return n
}

// filesOf 返回某处置的文件,按路径升序。
func (p *Plan) filesOf(d Disposition) []FilePlan {
	var out []FilePlan
	for _, f := range p.Files {
		if f.Disposition == d {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// scrubSummary 汇总某个文件的痕迹命中。
func scrubSummary(f FilePlan) string {
	if len(f.ScrubHits) == 0 {
		return "—"
	}
	byRule := make(map[string]int)
	for _, h := range f.ScrubHits {
		byRule[h.Rule]++
	}
	keys := make([]string, 0, len(byRule))
	for k := range byRule {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, byRule[k]))
	}
	return strings.Join(parts, " ")
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func shortCommit(hash, subject string) string {
	if hash == "" {
		return "—"
	}
	if subject == "" {
		return shortHash(hash)
	}
	return shortHash(hash) + " " + subject
}
