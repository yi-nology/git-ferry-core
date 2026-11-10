package mirror

import (
	"encoding/json"
	"strings"
)

// PolicySnapshotFile 是公开侧复验用的策略文件名(建议放在公开仓根)。
const PolicySnapshotFile = ".mirror-policy.json"

// snapshotRule 是 PolicySnapshot 里的一条痕迹规则。
//
// JSON 字段名与 service 层入参(JSON 落库)保持 snake_case 对齐,
// verifycmd 用同一 schema 反序列化 —— 两侧同一份文件才叫同源。
type snapshotRule struct {
	Name          string   `json:"name"`
	Category      string   `json:"category"`
	Severity      string   `json:"severity"`
	Pattern       string   `json:"pattern"`
	Replacement   string   `json:"replacement"`
	PathAllowlist []string `json:"pathAllowlist,omitempty"`
}

// PolicySnapshot 是随发布报告带出的策略快照。
//
// 为什么逐 tag 带:公开仓 CI 用同一份规则复验(oss-sync 同款纪律
// 「两侧同源才叫钉契约」),快照就是那份「同一份」—— 发布侧应用了
// 什么规则,公开侧就验什么规则,规则漂移在两侧各自可见。
type PolicySnapshot struct {
	// Version 快照 schema 版本(当前恒为 1;verifycmd 拒绝非 1)。
	Version int `json:"version"`
	// Mapping 身份映射(source→target)。
	Mapping snapshotMapping `json:"mapping"`
	// ScrubRules 痕迹规则(声明顺序保留;verifycmd 按同一顺序扫描)。
	ScrubRules []snapshotRule `json:"scrubRules"`
}

// snapshotMapping 是快照里的身份映射。
type snapshotMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// BuildPolicySnapshot 从发布输入构造确定性策略快照 JSON。
//
// 确定性:字段序固定(结构体声明序),规则保持声明顺序,无 map 参与
// —— 同输入 ⇒ 同字节,同 tag 重跑报告一致。
func BuildPolicySnapshot(m Mapping, rules []ScrubRule) string {
	out := PolicySnapshot{
		Version: 1,
		Mapping: snapshotMapping{Source: m.Source, Target: m.Target},
	}
	for _, r := range rules {
		sev := string(ScrubAuto)
		if r.Mandatory() {
			sev = string(ScrubMandatory)
		}
		out.ScrubRules = append(out.ScrubRules, snapshotRule{
			Name:          r.Name,
			Category:      r.Category,
			Severity:      sev,
			Pattern:       r.Pattern.String(),
			Replacement:   r.Replacement,
			PathAllowlist: r.PathAllowlist,
		})
	}
	return string(marshalSnapshot(out))
}

// marshalSnapshot 以缩进 JSON 序列化快照(文件形态人类可读,方便
// 公开仓 review diff)。
func marshalSnapshot(v PolicySnapshot) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b2, _ := json.Marshal(v)
		return b2
	}
	return b
}

// VerifyWorkflowTemplate 返回公开仓禁词门禁 CI 的 workflow 文本。
//
// 语义对应 oss-sync 的「公开仓 CI 用同一 policy 复验」:verifycmd
// 是发布侧同一套扫描原语(Rewriter.Pending 边界残留 + scrub 规则)
// 的独立可执行形态,workflow 把 policy 文件钉在公开仓根
// (.mirror-policy.json),CI 对整个仓树复验。
//
// coreRef 建议传 core 的版本 tag(如 v0.35.0);传空则用 @latest
// (不推荐,供应链未钉版本)。
func VerifyWorkflowTemplate(coreRef string) string {
	ref := coreRef
	if ref == "" {
		ref = "latest"
	}
	return strings.ReplaceAll(workflowTemplate, "@REF@", "@"+ref)
}

const workflowTemplate = `# 公开仓禁词门禁(文件面)。
# 由 git-ferry 镜像发布生成;policy 文件(.mirror-policy.json)与本
# workflow 一起提交进公开仓 —— 发布侧应用什么规则,公开侧就复验什么。
name: mirror-verify

on:
  push:
  pull_request:

jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - name: 禁词复验(文件面)
        run: go run github.com/yi-nology/git-ferry-core/mirror/verifycmd@REF@ -policy .mirror-policy.json .
`
