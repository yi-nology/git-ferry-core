package mirror

import (
	"strconv"
	"strings"
)

// compareVersions 比较两个语义化版本号(vMAJOR.MINOR.PATCH[-prerelease])。
//
// **只用于一件事**:决定「公开仓的 main 快照该指向哪个已发布版本」。
// 语义化的完整规则很复杂(预发布标识的优先级、构建元数据……),
// 而这里只需要一个**稳定且可预期**的全序:
//   - 主版本号逐段比数字;
//   - 预发布标识:无预发布 > 有预发布(1.0.0 > 1.0.0-rc1),这是语义化版本的规矩;
//   - 预发布标识之间按点分段逐段比数字,非数字段按字符串比;
//   - 解析不了的部分退回字符串比较 —— 宁可行为保守,不要随机。
//
// 返回 -1/0/+1(a 小于/等于/大于 b)。
func compareVersions(a, b string) int {
	av, apre := splitVersion(a)
	bv, bpre := splitVersion(b)
	if c := compareVersionNumbers(av, bv); c != 0 {
		return c
	}
	switch {
	case apre == "" && bpre == "":
		return 0
	case apre == "":
		return 1 // 正式版 > 预发布版
	case bpre == "":
		return -1
	}
	return comparePrerelease(apre, bpre)
}

// splitVersion 拆出版本号的主版本部分与预发布标识。
func splitVersion(v string) (nums []string, pre string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i] // 构建元数据不参与比较
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return strings.Split(v[:i], "."), v[i+1:]
	}
	return strings.Split(v, "."), ""
}

// compareVersionNumbers 逐段比较版本号的主版本部分。
func compareVersionNumbers(a, b []string) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		xi, xerr := strconv.Atoi(x)
		yi, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xi != yi {
				if xi < yi {
					return -1
				}
				return 1
			}
		case xerr == nil: // 数字段 < 非数字段("1.0" < "1.0.x")
			return -1
		case yerr == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

// comparePrerelease 比较两个预发布标识(点分段)。
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xi, xerr := strconv.Atoi(x)
		yi, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xi != yi {
				if xi < yi {
					return -1
				}
				return 1
			}
		case xerr == nil:
			return -1 // 数字标识 < 字母标识
		case yerr == nil:
			return 1
		default:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}
