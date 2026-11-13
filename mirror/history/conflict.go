package history

import (
	"fmt"
	"regexp"
	"strings"
)

// ConflictRuleKind 是冲突自动判据的种类。
type ConflictRuleKind string

const (
	// ConflictRuleInternalTrace 源侧独有行命中**内网形态特征**
	// (内网 registry / 内网域名 / 构建期跳过证书校验 / 内网措辞)
	// → 保留目标侧。
	//
	// 依据(实测):review-service 的 9 个真冲突里,有 5 个落在这类 ——
	// 源侧的改动是**内网形态**,公开树不该带。
	ConflictRuleInternalTrace ConflictRuleKind = "internal_trace"

	// ConflictRulePlaceholderDomain 源侧独有行用**真实对外域名**,
	// 而目标侧对应位置已**脱敏成示例域名** → 保留目标侧。
	//
	// 依据(实测):poster_test.go 的两边域名不同,公开树应保持脱敏值。
	ConflictRulePlaceholderDomain ConflictRuleKind = "placeholder_domain"

	// ConflictRulePublicAsset 目标侧独有行含**公开资产**(license 副本 / badge /
	// CI 徽章 / 公共镜像源 / 多平台集成),而源侧独有行**不含**这些资产
	// → 保留目标侧(源侧是旧版)。
	//
	// 依据(实测):README 的目标侧是**发布版**(badge + Release 产物表),
	// 源侧是**本地构建版**;Dockerfile 的目标侧补了 LICENSE 副本并换成公共镜像源。
	//
	// 方向是安全的:源侧**新增**公开资产时,源侧会命中、目标侧不会命中,规则不触发。
	ConflictRulePublicAsset ConflictRuleKind = "public_asset"
)

// ConflictRuleConfig 是一条判据的配置(通道级,JSON 形态)。
type ConflictRuleConfig struct {
	// Kind 判据种类。
	Kind ConflictRuleKind `json:"kind"`
	// Enabled 是否启用。
	Enabled bool `json:"enabled"`
	// Patterns 自定义正则,覆盖该种类的默认特征集。
	Patterns []string `json:"patterns,omitempty"`
}

// defaultInternalTracePatterns 内网形态的默认特征集。
var defaultInternalTracePatterns = []string{
	`harbor\.[a-zA-Z0-9.-]+`,       // 内网 registry
	`[a-zA-Z0-9-]*\.enjoye\.top\b`, // 内网域名
	`GIT_SSL_NO_VERIFY`,            // 构建期跳过证书校验
	`GOINSECURE`,                   // 同上
	`internal[- ]only`,             // internal-only artifacts
	`internal mirror`,              // 内网镜像说明
	`内网`,                           // 中文内网措辞
}

// defaultPublicAssetPatterns 公开资产的默认特征集。
var defaultPublicAssetPatterns = []string{
	`Apache-2\.0`,                    // license 声明与副本
	`shields\.io`,                    // badge
	`COPY --from=build /src/LICENSE`, // license 副本拷贝
	`docker\.io/library`,             // 公共镜像源
	`actions/workflows/`,             // CI 徽章
	`Forgejo`, `Gitee`,               // 多平台集成
	`Gerrit`,
}

// exampleDomainRe 示例域名(占位域名)。
var exampleDomainRe = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:example\.(?:com|org|net)|test|invalid|local)\b`)

// domainRe 一般的域名。
var domainRe = regexp.MustCompile(`(?i)\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+\b`)

// compilePatterns 编译特征集(自定义覆盖默认)。
func compilePatterns(custom, defaults []string) ([]*regexp.Regexp, error) {
	src := defaults
	if len(custom) > 0 {
		src = custom
	}
	out := make([]*regexp.Regexp, 0, len(src))
	for _, p := range src {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("判据正则非法 %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// matchAny 判断任一行是否命中任一特征。
func matchAny(lines []string, pats []*regexp.Regexp) (string, bool) {
	for _, line := range lines {
		for _, re := range pats {
			if re.MatchString(line) {
				return strings.TrimSpace(line), true
			}
		}
	}
	return "", false
}

// applyConflictRules 用判据集给一个「两边都改过」的文件定夺。
//
// 只在**确定性判定解不了**时兜底(否则自动解法优先,判据不越权)。
// 命中即判为「保留目标侧」——这些判据的共同前提是:
// **内网形态的东西不该进公开树**,而目标侧是公开形态。
//
// 返回 (命中?, 规则名, 理由)。
func applyConflictRules(rules []ConflictRuleConfig, ours, theirs string) (bool, string, string) {
	if len(rules) == 0 {
		return false, "", ""
	}
	sourceOnly := exclusiveLines(theirs, ours)
	targetOnly := exclusiveLines(ours, theirs)

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		switch rule.Kind {
		case ConflictRuleInternalTrace:
			pats, err := compilePatterns(rule.Patterns, defaultInternalTracePatterns)
			if err != nil {
				continue
			}
			if line, ok := matchAny(sourceOnly, pats); ok {
				return true, string(rule.Kind),
					fmt.Sprintf("源侧独有内容含内网形态特征,不应进公开树(命中:%s)", truncate(line, 120))
			}
		case ConflictRulePublicAsset:
			assetPats, err := compilePatterns(rule.Patterns, defaultPublicAssetPatterns)
			if err != nil {
				continue
			}
			asset, has := matchAny(targetOnly, assetPats)
			if !has {
				continue
			}
			if _, srcHas := matchAny(sourceOnly, assetPats); srcHas {
				continue // 源侧也有这些资产,不是「源侧是旧版」
			}
			return true, string(rule.Kind),
				fmt.Sprintf("目标侧含公开资产而源侧没有(命中:%s),源侧是旧版,保留目标侧", truncate(asset, 100))
		case ConflictRulePlaceholderDomain:
			real, ok := matchRealDomain(sourceOnly)
			if !ok {
				continue
			}
			if other, has := matchRealDomain(targetOnly); has {
				_ = other
				continue // 目标侧也有真实域名,不是脱敏差异
			}
			return true, string(rule.Kind),
				fmt.Sprintf("源侧用真实对外域名(%s),目标侧已脱敏为示例域名,公开树应保持脱敏", real)
		}
	}
	return false, "", ""
}

// matchRealDomain 判断行里是否含**非示例**域名,返回该域名。
func matchRealDomain(lines []string) (string, bool) {
	for _, line := range lines {
		for _, d := range domainRe.FindAllString(line, -1) {
			if exampleDomainRe.MatchString(d) {
				continue
			}
			// 排除常见的非域名片段(版本号、文件扩展名等)。
			if strings.Contains(d, ".") && !strings.HasSuffix(d, ".go") &&
				!strings.HasSuffix(d, ".md") && !strings.HasSuffix(d, ".yml") &&
				!strings.HasSuffix(d, ".yaml") && !strings.HasSuffix(d, ".json") {
				return d, true
			}
		}
	}
	return "", false
}
