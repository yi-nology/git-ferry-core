package mirror

import (
	"regexp"
	"sort"
	"strings"
)

// ScrubRule 一条痕迹清理规则。
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
	// Mandatory 该命中是否强制人工处理(密钥类)。
	//
	// 必须区分:自动清理类的命中**已随内容一起发布**,
	// 强制人工类的命中意味着**这个文件没有被发布**。
	// 不区分的话,审计报告会把两者混为一谈。
	Mandatory bool
}

// ScrubSeverity 是痕迹的处理强度。
type ScrubSeverity string

const (
	// ScrubAuto 可自动清理:命中后按规则的 Replacement 替换(或删除)。
	ScrubAuto ScrubSeverity = "auto"
	// ScrubMandatory 强制 hold:命中即必须人工处理,**不做自动替换**。
	//
	// 密钥类走这条:自动替换密钥可能留下半吊子状态,比留痕更危险。
	ScrubMandatory ScrubSeverity = "mandatory"
)

// Mandatory 判断该规则是否强制人工处理。
func (r ScrubRule) Mandatory() bool {
	return r.Severity == ScrubMandatory
}

// ScrubResult 是痕迹清理的结果。
type ScrubResult struct {
	// Content 清理后的内容。
	Content []byte
	// Hits 全部命中项(含已清理与强制 hold 的)。
	Hits []ScrubHit
	// MandatoryHits 强制人工处理的命中项。
	MandatoryHits []ScrubHit
	// Cleaned 是否发生过自动清理。
	Cleaned bool
}

// ApplyScrub 对内容做痕迹清理。
//
// 规则语义:
//   - ScrubAuto:按 Replacement 替换;Replacement 为空则删除命中内容;
//   - ScrubMandatory:命中即报告,但**不改动内容** —— 密钥类不自动处理。
//
// 清理必须可审计:每个命中都带行号与原文,出现在计划里。
func ApplyScrub(content []byte, rules []ScrubRule) ScrubResult {
	res := ScrubResult{Content: content}
	if len(content) == 0 || len(rules) == 0 {
		return res
	}

	lines := strings.Split(string(content), "\n")
	// 按行处理,保留行结构,便于逐行定位命中。
	for i, line := range lines {
		for _, rule := range rules {
			if rule.Pattern == nil {
				continue
			}
			matches := rule.Pattern.FindAllStringIndex(line, -1)
			if len(matches) == 0 {
				continue
			}
			// 命中报告与替换都基于**同一份快照**(替换前的 line)。
			//
			// 之前是"边报边改":第一处替换后行长度变了,后续命中仍用**原始下标**
			// 去切 ⇒ ①第二处及以后的痕迹没清干净(残留),②中间文本被吃掉,
			// ③三处以上直接越界 panic。
			snapshot := line
			for _, m := range matches {
				// **密钥类命中不能回显原文**:命中行会进执行记录(落库)
				// 与 API 响应 —— 原样回显等于把密钥抄一份到日志和数据库里。
				// 命中的那一段替换成 ***,上下文仍保留(足够定位)。
				text := snapshot
				if rule.Mandatory() {
					text = redactMatch(snapshot, m[0], m[1])
				}
				hit := ScrubHit{
					Rule:      rule.Name,
					Category:  rule.Category,
					Line:      i + 1,
					Text:      truncate(text, 200),
					Mandatory: rule.Mandatory(),
				}
				res.Hits = append(res.Hits, hit)
				if rule.Mandatory() {
					res.MandatoryHits = append(res.MandatoryHits, hit)
				}
			}
			if rule.Mandatory() {
				continue // 密钥类不自动改内容
			}
			// 一次性替换**所有**命中(字面量替换,replacement 里的 $ 不做展开)。
			line = rule.Pattern.ReplaceAllLiteralString(line, rule.Replacement)
			res.Cleaned = true
		}
		lines[i] = line
	}

	sort.SliceStable(res.Hits, func(i, j int) bool {
		if res.Hits[i].Line != res.Hits[j].Line {
			return res.Hits[i].Line < res.Hits[j].Line
		}
		return res.Hits[i].Rule < res.Hits[j].Rule
	})
	res.Content = []byte(strings.Join(lines, "\n"))
	return res
}

// HasMandatory 判断内容是否命中强制人工处理的规则。
func HasMandatory(content []byte, rules []ScrubRule) bool {
	return len(ApplyScrub(content, rules).MandatoryHits) > 0
}

// ScrubSummary 汇总一组规则的命中情况。
type ScrubSummary struct {
	// ByRule 各规则命中数。
	ByRule map[string]int
	// Total 总命中数。
	Total int
	// Mandatory 强制人工处理的命中数。
	Mandatory int
}

// SummarizeScrub 汇总一组规则的命中情况。
func SummarizeScrub(hits []ScrubHit, rules []ScrubRule) ScrubSummary {
	s := ScrubSummary{ByRule: make(map[string]int)}
	mandatory := make(map[string]bool)
	for _, r := range rules {
		if r.Mandatory() {
			mandatory[r.Name] = true
		}
	}
	for _, h := range hits {
		s.ByRule[h.Rule]++
		s.Total++
		if mandatory[h.Rule] {
			s.Mandatory++
		}
	}
	return s
}

// stripInternalHost 是一条常用的自动清理规则:删除内网主机名。
//
// 用途:公开树不该出现内部 registry 地址。删除而不是替换成占位符——
// 占位符只是把内网痕迹换个形式留在公开树里,反而更难发现。
func StripInternalHost(host string) ScrubRule {
	return ScrubRule{
		Name:     "strip-" + host,
		Category: "internal-host",
		Pattern:  regexp.MustCompile(regexp.QuoteMeta(host)),
		Severity: ScrubAuto,
	}
}

// truncate 截断字符串到 n 字节(不切断 UTF-8 字符)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// redactMatch 把 [start,end) 区间替换成 ***。
func redactMatch(line string, start, end int) string {
	if start < 0 || end > len(line) || start >= end {
		return line
	}
	return line[:start] + "***" + line[end:]
}
