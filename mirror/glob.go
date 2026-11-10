package mirror

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// matchGlob 判断 p 是否命中 pattern(glob,允许 ** 跨层)。
//
// allowlist 是安全配置,采用保守语义:
//   - 以 '/' 分段匹配;'*' 不跨段;'**' 作为独立段跨任意层(含零层);
//   - 不含 '/' 的模式只匹配单段(跨层必须显式写 "**/name");
//   - 大小写敏感;不支持否定。
//
// 模式非法时返回 false:allowlist 是放行面,这里 false 只影响放行,
// 禁词门禁面不受影响(fail-closed,见 scrub.go)。
func matchGlob(pattern, p string) bool {
	ps := splitGlobSegments(pattern)
	ss := splitGlobSegments(p)
	if len(ps) == 0 || len(ss) == 0 {
		return false
	}
	return globSegs(ps, 0, ss, 0)
}

func splitGlobSegments(s string) []string {
	s = strings.Trim(s, "/")
	if s == "" {
		return nil
	}
	return strings.Split(s, "/")
}

// globSegs 递归逐段匹配。"**" 匹配零或多段。
func globSegs(pat []string, pi int, seg []string, si int) bool {
	if pi == len(pat) {
		return si == len(seg)
	}
	if pat[pi] == "**" {
		// "**" 跨任意层(含零层):吃 0..rest 段。
		for k := si; k <= len(seg); k++ {
			if globSegs(pat, pi+1, seg, k) {
				return true
			}
		}
		return false
	}
	if si == len(seg) {
		return false
	}
	if ok, err := path.Match(pat[pi], seg[si]); err == nil && ok {
		return globSegs(pat, pi+1, seg, si+1)
	}
	return false
}

// ValidateGlob 校验 allowlist 的 glob 模式(建通道时拒绝非法模式,
// 而不是发布时静默不放行)。
//
// 规则:段不能为空;不能含反斜杠;每段必须能被 path.Match 正常编译
// (坏区间如 [a- 会返回 ErrBadPattern)。
func ValidateGlob(pattern string) error {
	pattern = strings.Trim(pattern, "/")
	if pattern == "" {
		return errors.New("glob 模式为空")
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "" {
			return fmt.Errorf("glob 模式含空段: %q", pattern)
		}
		if strings.ContainsRune(seg, 92) {
			return fmt.Errorf("glob 模式含反斜杠: %q", pattern)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("glob 模式非法: %q: %w", pattern, err)
		}
	}
	return nil
}
