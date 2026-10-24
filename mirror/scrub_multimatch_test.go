package mirror

import (
	"regexp"
	"strings"
	"testing"
)

// TestApplyScrub_MultipleMatchesOnSameLine 同一行多处命中必须全部清干净。
//
// 回归背景:替换循环把**原始下标**用在**已被改动的行**上 ⇒
// ①第二处及以后的痕迹没清干净(残留),②中间的文本被吃掉,③三处以上越界 panic。
// 内置的 StripInternalHost 规则没有 Replacement(默认=删除),
// 用户只填 pattern 是最常见的配置,正好落在删除语义上。
func TestApplyScrub_MultipleMatchesOnSameLine(t *testing.T) {
	rule := ScrubRule{
		Name:        "strip",
		Category:    "internal-host",
		Pattern:     regexp.MustCompile(`SECRET`),
		Replacement: "***",
	}

	res := ApplyScrub([]byte("aaa SECRET bbb SECRET ccc"), []ScrubRule{rule})
	got := string(res.Content)
	if strings.Contains(got, "SECRET") {
		t.Errorf("痕迹没清干净: %q", got)
	}
	if got != "aaa *** bbb *** ccc" {
		t.Errorf("替换错位: got %q, want %q", got, "aaa *** bbb *** ccc")
	}
	if len(res.Hits) != 2 {
		t.Errorf("命中数不对: got %d, want 2", len(res.Hits))
	}
}

// TestApplyScrub_DeleteSemantics_MultipleMatches 删除语义(Replacement 为空)同样要全清。
func TestApplyScrub_DeleteSemantics_MultipleMatches(t *testing.T) {
	rule := ScrubRule{
		Name:     "strip",
		Category: "internal-host",
		Pattern:  regexp.MustCompile(`AA`),
		// Replacement 为空 = 删除命中内容
	}
	res := ApplyScrub([]byte("x AA y AA z"), []ScrubRule{rule})
	got := string(res.Content)
	if strings.Contains(got, "AA") {
		t.Errorf("删除语义没清干净: %q", got)
	}
	if got != "x  y  z" {
		t.Errorf("删除错位: got %q, want %q", got, "x  y  z")
	}
}

// TestApplyScrub_ManyMatchesDoesNotPanic 三处以上命中曾直接越界 panic。
func TestApplyScrub_ManyMatchesDoesNotPanic(t *testing.T) {
	rule := ScrubRule{
		Name:     "strip",
		Category: "internal-host",
		Pattern:  regexp.MustCompile(`AA`),
	}
	res := ApplyScrub([]byte("x AA y AA z AA w"), []ScrubRule{rule})
	if got := string(res.Content); strings.Contains(got, "AA") {
		t.Errorf("痕迹没清干净: %q", got)
	}
	if len(res.Hits) != 3 {
		t.Errorf("命中数不对: got %d, want 3", len(res.Hits))
	}
}

// TestApplyScrub_MandatoryReportsEveryMatch 密钥类要报**每一处**命中(内容不改)。
func TestApplyScrub_MandatoryReportsEveryMatch(t *testing.T) {
	rule := ScrubRule{
		Name:     "secret",
		Category: "secret",
		Pattern:  regexp.MustCompile(`SECRET`),
		Severity: ScrubMandatory,
	}
	res := ApplyScrub([]byte("a SECRET b SECRET c"), []ScrubRule{rule})
	if len(res.MandatoryHits) != 2 {
		t.Errorf("密钥类命中数不对: got %d, want 2", len(res.MandatoryHits))
	}
	if got := string(res.Content); got != "a SECRET b SECRET c" {
		t.Errorf("密钥类不该改内容: got %q", got)
	}
}
