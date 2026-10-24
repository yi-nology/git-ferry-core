package history

import "github.com/yi-nology/git-ferry-core/mirror"

// 痕迹清理的原语在 **mirror 包**(`mirror/scrub.go`)。
//
// **为什么上提**:身份镜像发布(mirror.Publish)也需要痕迹清理 ——
// 内网 registry 地址、部署代号、令牌原样进**公开 module** 是真实的泄漏面;
// 而历史同步侧早就有了这套原语。留在 mirror/history 里,镜像侧就只能:
//   - 反向依赖 history(⇒ 循环依赖,history 已经依赖 mirror),或
//   - 复制一份(⇒ 两份实现迟早漂移)。
//
// 这里用**类型别名**保持 `history.ScrubRule` 等名字照旧可用 ——
// 上层(model / service / 报告)零改动。
type (
	// ScrubSeverity 痕迹的处理强度。
	ScrubSeverity = mirror.ScrubSeverity
	// ScrubRule 一条痕迹清理规则。
	ScrubRule = mirror.ScrubRule
	// ScrubHit 一处痕迹命中。
	ScrubHit = mirror.ScrubHit
	// ScrubResult 痕迹清理的结果。
	ScrubResult = mirror.ScrubResult
	// ScrubSummary 一组规则的命中汇总。
	ScrubSummary = mirror.ScrubSummary
)

const (
	// ScrubAuto 可自动清理:命中后按规则的 Replacement 替换(或删除)。
	ScrubAuto = mirror.ScrubAuto
	// ScrubMandatory 强制 hold:命中即必须人工处理,不做自动替换。
	ScrubMandatory = mirror.ScrubMandatory
)

// ApplyScrub 对内容做痕迹清理(见 mirror.ApplyScrub)。
func ApplyScrub(content []byte, rules []ScrubRule) ScrubResult {
	return mirror.ApplyScrub(content, rules)
}

// HasMandatory 判断内容是否命中强制人工处理的规则。
func HasMandatory(content []byte, rules []ScrubRule) bool {
	return mirror.HasMandatory(content, rules)
}

// SummarizeScrub 汇总一组规则的命中情况。
func SummarizeScrub(hits []ScrubHit, rules []ScrubRule) ScrubSummary {
	return mirror.SummarizeScrub(hits, rules)
}

// StripInternalHost 是一条常用的自动清理规则:删除内网主机名。
func StripInternalHost(host string) ScrubRule {
	return mirror.StripInternalHost(host)
}
