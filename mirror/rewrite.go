package mirror

import (
	"bytes"
	"fmt"
)

// IdentityKind 身份规则的匹配语义,决定边界如何判定。
type IdentityKind int

const (
	// KindModule 按 Go module path 边界匹配:源标识之后必须跟非模块路径字符
	// (字母/数字/下划线/点/连字符之外)或文本末尾。
	// 子包路径会被正确改写,兄弟路径(b-extra / b.cool)不会被误伤。
	KindModule IdentityKind = iota
	// KindHost 按主机名边界匹配:源标识之后必须跟非主机名字符
	// (字母/数字/点/连字符之外)或文本末尾。
	// 点被算作主机名字符,避免 git.enjoye.top.evil.com 被半改写。
	KindHost
	// KindLiteral 按字面量词边界匹配:源标识的前后都必须是"非字母数字下划线"
	// 或文本端点。用于环境标识、部署代号一类的独立词。
	KindLiteral
)

// String 返回规则种类的可读名称,用于计划报告与错误信息。
func (k IdentityKind) String() string {
	switch k {
	case KindModule:
		return "module"
	case KindHost:
		return "host"
	case KindLiteral:
		return "literal"
	default:
		return "unknown"
	}
}

// IdentityRule 一条身份映射规则。
type IdentityRule struct {
	// Source 是待替换的源标识。
	Source string
	// Target 是替换后的目标标识。
	// 为空表示"剥离":删除源标识本身,而不是替换成占位符——
	// 占位符只是把内网痕迹换了个形式留在公开树里,反而更难发现。
	Target string
	// Kind 决定边界匹配语义,默认 KindModule。
	Kind IdentityKind
}

// compiledRule 是编译后的规则,持有预编译的匹配信息。
type compiledRule struct {
	IdentityRule
	source []byte
	target []byte
}

// Rewriter 是编译后的身份改写器,可在多个 blob 上复用。
//
// 规则执行顺序按 Kind 固定为 KindModule → KindHost → KindLiteral,
// 同类规则保持声明顺序。这不是可选的约定:若先执行主机名规则,
// "git.enjoye.top/enjoydream/x" 这类模块路径的前缀会先被改掉,
// 模块规则随后找不到源标识,静默失效——路径改写看起来成功了,
// 实际上 import 全部指向了不存在的模块。
type Rewriter struct {
	rules []compiledRule
}

// NewRewriter 编译身份规则。规则非法时返回错误,不做静默降级。
func NewRewriter(rules []IdentityRule) (*Rewriter, error) {
	compiled := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		if r.Source == "" {
			return nil, fmt.Errorf("第 %d 条身份规则的 Source 不能为空", i+1)
		}
		if r.Target == r.Source {
			return nil, fmt.Errorf("第 %d 条身份规则的 Source 与 Target 相同(%s),无改写意义", i+1, r.Source)
		}
		if r.Target == "" && r.Kind == KindModule {
			// 剥离模块路径会留下 import "" 这类语法残骸,
			// 正确做法是让整条 import 走类型门禁报错,而不是产出坏代码。
			return nil, fmt.Errorf("第 %d 条身份规则(Kind=module)不支持剥离:Target 不能为空", i+1)
		}
		if r.Kind != KindModule && r.Kind != KindHost && r.Kind != KindLiteral {
			return nil, fmt.Errorf("第 %d 条身份规则的 Kind 非法: %d", i+1, int(r.Kind))
		}
		compiled = append(compiled, compiledRule{
			IdentityRule: r,
			source:       []byte(r.Source),
			target:       []byte(r.Target),
		})
	}
	// 按 Kind 稳定排序:module → host → literal。
	sortByKind(compiled)
	return &Rewriter{rules: compiled}, nil
}

// sortByKind 按 Kind 稳定排序。同类规则保持声明顺序。
func sortByKind(rules []compiledRule) {
	for i := 1; i < len(rules); i++ {
		for j := i; j > 0 && rules[j].Kind < rules[j-1].Kind; j-- {
			rules[j], rules[j-1] = rules[j-1], rules[j]
		}
	}
}

// Rewrite 对 data 依次应用全部规则。没有任何规则命中时返回原切片。
func (r *Rewriter) Rewrite(data []byte) []byte {
	if r == nil || len(r.rules) == 0 {
		return data
	}
	out := data
	for _, rule := range r.rules {
		out = rule.apply(out)
	}
	return out
}

// Rules 返回编译后的规则副本(按实际执行顺序),供报告与审计使用。
func (r *Rewriter) Rules() []IdentityRule {
	if r == nil {
		return nil
	}
	out := make([]IdentityRule, 0, len(r.rules))
	for _, rule := range r.rules {
		out = append(out, rule.IdentityRule)
	}
	return out
}

// apply 对单条规则做一次边界安全改写。
//
// 单侧边界(module/host):源标识之后必须是边界字符或文本末尾。
// 双侧边界(literal):源标识之前也必须是非词字符或文本开头。
func (rule compiledRule) apply(data []byte) []byte {
	if len(data) == 0 || !bytes.Contains(data, rule.source) {
		return data
	}
	out := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		idx := bytes.Index(data[i:], rule.source)
		if idx < 0 {
			out = append(out, data[i:]...)
			break
		}
		start := i + idx
		end := start + len(rule.source)
		out = append(out, data[i:start]...)

		// 前边界(literal 专用):源标识之前必须是词边界或文本开头。
		if rule.Kind == KindLiteral && start > 0 && isLiteralWordByte(data[start-1]) {
			out = append(out, rule.source...)
			i = end
			continue
		}
		// 后边界:源标识之后必须是边界字符或文本末尾。
		if end < len(data) && !isBoundaryByte(data[end], rule.Kind) {
			out = append(out, rule.source...)
			i = end
			continue
		}
		out = append(out, rule.target...)
		i = end
	}
	return out
}

// isBoundaryByte 判断字符是否构成该规则种类下的边界。
// 边界字符意味着"源标识在此处结束",后继字符不属于同类标识的延续。
func isBoundaryByte(b byte, kind IdentityKind) bool {
	switch kind {
	case KindModule:
		// 与既有 modulePathTerminator 保持一致:非 [a-zA-Z0-9_.-] 即为边界。
		return (b < 'a' || b > 'z') &&
			(b < 'A' || b > 'Z') &&
			(b < '0' || b > '9') &&
			b != '_' && b != '.' && b != '-'
	case KindHost:
		// 主机名可含点,故点不是边界;避免半改写域名。
		return (b < 'a' || b > 'z') &&
			(b < 'A' || b > 'Z') &&
			(b < '0' || b > '9') &&
			b != '.' && b != '-'
	case KindLiteral:
		return !isLiteralWordByte(b)
	default:
		return true
	}
}

// isLiteralWordByte 判断字符是否属于"词"字符(字母数字下划线)。
func isLiteralWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' ||
		b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' ||
		b == '_'
}

// RewriteIdentities 按 rules 对 data 做身份改写的一次性便捷入口。
// 需要对多个 blob 复用同一组规则时,请改用 NewRewriter。
func RewriteIdentities(data []byte, rules []IdentityRule) ([]byte, error) {
	r, err := NewRewriter(rules)
	if err != nil {
		return nil, err
	}
	return r.Rewrite(data), nil
}
