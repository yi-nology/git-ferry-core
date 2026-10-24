package mirror

import (
	"bytes"
	"fmt"
	"go/format"
	"strings"
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
	// variant 标记这是「同一规则的另一种写法」(如 scp 形式的克隆地址),
	// 不是用户单独配的一条规则 —— 报告里不单列,执行时照常生效。
	variant bool
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
		// **module 规则额外编译一个 scp 形式的变体**。
		//
		// git 的 scp 形式克隆地址用 ":" 分隔主机与路径:
		//   git@git.enjoye.top:enjoydream/app.git
		// 而 module path 用 "/" 分隔 —— 两者只差**第一个**分隔符。
		// 不处理的话,module 规则压根碰不到这种写法:内网组织名与仓库名
		// 原样留在公开仓里,公开仓的 clone 指令直接失效。
		if r.Kind == KindModule {
			if s2, t2, ok := scpVariant(r.Source, r.Target); ok {
				compiled = append(compiled, compiledRule{
					IdentityRule: IdentityRule{Source: s2, Target: t2, Kind: KindModule},
					source:       []byte(s2),
					target:       []byte(t2),
					variant:      true,
				})
			}
		}
	}
	// 按 Kind 稳定排序:module → host → literal。
	sortByKind(compiled)
	return &Rewriter{rules: compiled}, nil
}

// scpVariant 生成 module 规则的 scp 形式变体(源/目标各替换**第一个**分隔符)。
//
// module path 的第一段是主机名,scp 形式把主机与路径之间的 "/" 换成 ":"。
// 源或目标里找不到可替换的分隔符时返回 ok=false(不生成变体)。
func scpVariant(source, target string) (string, string, bool) {
	si := strings.Index(source, "/")
	ti := strings.Index(target, "/")
	if si < 0 || ti < 0 {
		return "", "", false
	}
	vs := source[:si] + ":" + source[si+1:]
	vt := target[:ti] + ":" + target[ti+1:]
	if vs == vt {
		// 变体成了恒等替换:留着会让「残留检测」永远报残留。
		return "", "", false
	}
	return vs, vt, true
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

// PublishForm 计算内容在**发布形态**下的样子:身份改写 + gofmt 归一化。
//
// **两条链路共用这一个实现** ——
// 镜像发布(mirror.Publish)此前只改写、**不归一化**;
// 历史同步(mirror/history)改写后还要 gofmt。
// 于是同一份源码经两条链路发布出**不同的字节**:
// import 顺序会因 module path 改写而翻转,镜像侧没归一化 →
// 公开仓的 gofmt CI 挂、`go get` 用户拿到未格式化代码,
// 而且**类型门禁看不出来**(编译不看 import 顺序)。
func (r *Rewriter) PublishForm(path string, data []byte) []byte {
	return GofmtNormalize(path, r.Rewrite(data))
}

// GofmtNormalize 对 .go 文件做 gofmt 归一化(语义与 gofmt 一致,含 import 排序)。
//
// **为什么必须归一化**:身份改写会改变 import 的字典序位置 ——
// 改写后的源内容会变成「未格式化」(实测 review-service:改写后 50 个文件违反
// gofmt,两棵树原始都是 0)。不归一化就比对,每个引用了模块路径的文件都会留下
// 一处 1 行的假差异,40 个文件凭空变成「待人工确认」。
//
// 只处理 .go。语法错误时原样返回:这类问题由类型门禁(GOWORK=off 下的
// 全量类型检查)报错,不该在这里被吞掉。
func GofmtNormalize(path string, data []byte) []byte {
	if !strings.HasSuffix(path, ".go") {
		return data
	}
	out, err := format.Source(data)
	if err != nil {
		return data
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
		if rule.variant {
			// 变体是同一规则的另一种写法,不是用户单独配的规则 ——
			// 报告里不单列(否则审计材料会多出用户没配过的条目)。
			continue
		}
		out = append(out, rule.IdentityRule)
	}
	return out
}

// Pending 判断 data 里是否还有**本该被改写、却仍然原样留着**的源标识。
//
// 用来做残留检测:改写器能改的都改了,剩下的才是真正的残留
// (比如二进制/超限 blob 走了「原样保留」路径,或者内容根本没过改写器)。
//
// 判据用与 apply 同一套边界语义:只有**满足边界条件**的源标识才算残留。
// 「app-extra」这种「共享前缀但不是同一个标识」不算 ——
// 它是另一个模块路径,不是我们这个身份漏改了。
func (r *Rewriter) Pending(data []byte) bool {
	if r == nil || len(r.rules) == 0 {
		return false
	}
	for _, rule := range r.rules {
		if rule.matches(data) {
			return true
		}
	}
	return false
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
		if !rule.boundaryAt(data, end) {
			out = append(out, rule.source...)
			i = end
			continue
		}
		out = append(out, rule.target...)
		i = end
	}
	return out
}

// matches 判断 data 里是否有一处**满足边界条件**的源标识。
//
// 这是残留检测的判据:「规则本该改写这里,却没改写」= 内网标识还在。
// 与 apply 用同一套边界语义(共用 boundaryAt),两者不会各说各话。
func (rule compiledRule) matches(data []byte) bool {
	if len(data) == 0 || !bytes.Contains(data, rule.source) {
		return false
	}
	i := 0
	for i < len(data) {
		idx := bytes.Index(data[i:], rule.source)
		if idx < 0 {
			return false
		}
		start := i + idx
		end := start + len(rule.source)
		if rule.Kind == KindLiteral && start > 0 && isLiteralWordByte(data[start-1]) {
			i = end
			continue
		}
		if rule.boundaryAt(data, end) {
			return true
		}
		i = end
	}
	return false
}

// boundaryAt 判断「源标识结束于 end」这个位置是否构成边界。
func (rule compiledRule) boundaryAt(data []byte, end int) bool {
	if end >= len(data) {
		return true // 文本末尾
	}
	// **".git" 特判**:克隆 URL(`https://host/org/repo.git`)里的 ".git"
	// 是 git 协议后缀,不是 module path 的延续。
	// 不特判的话,"." 被当成 module path 字符 → 边界不成立 →
	// 整条克隆 URL 原样进公开仓(而且是「半改半留」:同一文件里
	// 不带 .git 的那行改了,带 .git 的那行没改)。
	if rule.Kind == KindModule && isModuleGitSuffix(data, end) {
		return true
	}
	return isBoundaryByte(data[end], rule.Kind)
}

// isModuleGitSuffix 判断 data[end:] 是不是 ".git" 且 ".git" 之后是边界。
//
// 只认精确的 ".git" 后缀(其后不能再跟 module path 字符):
// "app.github.com" 不是 git 后缀,不改。
func isModuleGitSuffix(data []byte, end int) bool {
	if end+4 > len(data) || !bytes.Equal(data[end:end+4], []byte(".git")) {
		return false
	}
	after := end + 4
	if after >= len(data) {
		return true
	}
	return isBoundaryByte(data[after], KindModule)
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
