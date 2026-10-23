package history

import (
	"regexp"
	"sort"
	"strings"
)

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
			for _, m := range matches {
				hit := ScrubHit{
					Rule:     rule.Name,
					Category: rule.Category,
					Line:     i + 1,
					Text:     truncate(line, 200),
				}
				res.Hits = append(res.Hits, hit)
				if rule.Mandatory() {
					res.MandatoryHits = append(res.MandatoryHits, hit)
					continue
				}
				replacement := rule.Replacement
				line = line[:m[0]] + replacement + line[m[1]:]
				res.Cleaned = true
			}
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
