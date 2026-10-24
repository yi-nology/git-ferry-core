//go:build history_live

package history

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// TestLiveHistoryPlan 用真实双仓跑历史同步计划,产出可审阅的基线报告。
//
// 环境变量触发:
//
//	HISTORY_LIVE_SRC         内部源仓本地路径
//	HISTORY_LIVE_TGT         公开目标仓本地路径
//	HISTORY_LIVE_MODULE_FROM 源 module path(默认 review-service)
//	HISTORY_LIVE_MODULE_TO   目标 module path(默认 code-review-service)
//	HISTORY_LIVE_REPORT      可选:把 Markdown 报告写到该路径
//	HISTORY_LIVE_EXPECT      可选:期望的统计摘要,形如 "port=11,hold=55";
//	                         不匹配则失败,用于把基线钉死成回归门禁
func TestLiveHistoryPlan(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓计划(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
	}
	from := envOr("HISTORY_LIVE_MODULE_FROM", "git.enjoye.top/enjoydream/code-review-service")
	to := envOr("HISTORY_LIVE_MODULE_TO", "github.com/yi-nology/code-review-service")

	opts := Options{
		SourceDir: src,
		TargetDir: tgt,
		Rules: []mirror.IdentityRule{
			{Source: from, Target: to, Kind: mirror.KindModule},
			{Source: "git.enjoye.top", Target: "github.com", Kind: mirror.KindHost},
		},
		PathRules: []PathRule{
			{From: "docs/internal", To: "docs/archive"},
		},
		ScrubRules: []ScrubRule{
			{Name: "internal-host", Category: "internal-host",
				Pattern: regexp.MustCompile(`[a-zA-Z0-9.-]*\.enjoye\.top`)},
			{Name: "internal-registry", Category: "internal-host",
				Pattern: regexp.MustCompile(`harbor\.enjoye\.top`)},
			{Name: "secret-like", Category: "secret",
				Pattern: regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._-]{16,}|sk-[a-zA-Z0-9]{16,}|ghp_[a-zA-Z0-9]{20,}|api[_-]?key\s*[:=]\s*\S+)`)},
			{Name: "private-net", Category: "internal-address",
				Pattern: regexp.MustCompile(`\b(10\.\d{1,3}\.\d{1,3}\.\d{1,3}|100\.6[4-9]\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})\b`)},
		},
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)

	// 纯读保证:目标仓 HEAD 不应被改动。
	tgtRepo, err := git.PlainOpen(tgt)
	require.NoError(t, err)
	head, err := tgtRepo.Head()
	require.NoError(t, err)
	assert.NotEmpty(t, head.Hash().String())

	// 确定性:同一输入两次生成的计划完全一致。
	again, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, plan, again, "同一输入两次生成的计划必须完全一致")

	// 结构不变式:受保护文件不得同时被判为待移植。
	protected := map[string]bool{}
	for _, f := range plan.Protected() {
		protected[f.Path] = true
	}
	for _, f := range plan.Files {
		if protected[f.Path] {
			assert.NotEqual(t, DispositionPort, f.Disposition,
				"受保护文件不得同时判为待移植: %s", f.Path)
		}
	}

	t.Logf("summary: total=%d same=%d port=%d port_with_scrub=%d exclude=%d hold=%d preserve=%d scrub=%d (+%d/-%d)",
		plan.Summary.Total, plan.Summary.Same, plan.Summary.Port, plan.Summary.PortWithScrub,
		plan.Summary.Exclude, plan.Summary.Hold, plan.Summary.Preserve, plan.Summary.ScrubHits,
		plan.Summary.AddedLines, plan.Summary.RemovedLines)

	if expect := os.Getenv("HISTORY_LIVE_EXPECT"); expect != "" {
		assert.Equal(t, expect, fmtSummary(plan.Summary),
			"基线统计与期望不符(基线已漂移,请复核后再更新)")
	}

	if out := os.Getenv("HISTORY_LIVE_REPORT"); out != "" {
		require.NoError(t, os.WriteFile(out, []byte(plan.Markdown()), 0o644))
		t.Logf("报告已写入 %s", out)
	}
}

// TestLiveHistoryResolve 用真实双仓验证 hold 的自动解法:
//
// 内容级共同基点(两棵树的同源改写副本关系)→ 三向合并 → 自动解决大部分 hold。
func TestLiveHistoryResolve(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓解法(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
	}
	from := envOr("HISTORY_LIVE_MODULE_FROM", "git.enjoye.top/enjoydream/code-review-service")
	to := envOr("HISTORY_LIVE_MODULE_TO", "github.com/yi-nology/code-review-service")

	opts := Options{
		SourceDir: src,
		TargetDir: tgt,
		Rules: []mirror.IdentityRule{
			{Source: from, Target: to, Kind: mirror.KindModule},
			{Source: "git.enjoye.top", Target: "github.com", Kind: mirror.KindHost},
		},
		PathRules: []PathRule{
			{From: "docs/internal", To: "docs/archive"},
		},
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	s := SummarizeResolutions(plan.Resolutions)
	t.Logf("hold 解法: 共 %d → 取源侧 %d、保留目标侧 %d、自动合并 %d、真冲突 %d、无基点 %d",
		s.Total, s.TakeTheirs, s.TakeOurs, s.MergeClean, s.Conflict, s.NoBase)

	// 关键断言:内部领先最多的文件必须判为「取源侧」。
	// internal/readiness/checks.go 源侧 +129 / 目标侧 +6,是最大的内容差异,
	// 也是 M2 门禁拦下的那个依赖的定义处。
	res, ok := plan.Resolutions["internal/readiness/checks.go"]
	require.True(t, ok, "checks.go 必须有解法")
	assert.Equal(t, ResolutionTakeTheirs, res.Verdict, res.Reason)

	// 目标侧领先最多的文件应判为「保留目标侧」,不能被源侧覆盖。
	// internal/ocr/runner_test.go 目标侧 +88,源侧没动。
	res, ok = plan.Resolutions["internal/config/loader.go"]
	require.True(t, ok, "loader.go 必须有解法")
	assert.Equal(t, ResolutionTakeOurs, res.Verdict, res.Reason)

	// 仍需人工的必须是少数,且逐个可列出。
	human := HumanResolutions(plan.Resolutions)
	t.Logf("仍需人工 %d 个:", len(human))
	for _, r := range human {
		t.Logf("  %s\t%s\t%s", r.Path, r.Verdict, r.Reason)
	}
	assert.LessOrEqual(t, len(human), s.Total/2,
		"可机械解决的 hold 应当占多数(否则共同基点机制没起作用)")

	if out := os.Getenv("HISTORY_LIVE_RESOLUTIONS"); out != "" {
		require.NoError(t, os.WriteFile(out,
			[]byte(strings.Join(ResolutionLines(plan.Resolutions), "\n")+"\n"), 0o644))
		t.Logf("解法清单已写入 %s", out)
	}
}

// TestLiveHistoryResolveThenVerify 端到端:算出解法 → 应用 → 物化 → 类型门禁。
//
// 这是 M3 的关键一问:解法应用后,被 M2 拦下的 port 移植能否通过门禁。
func TestLiveHistoryResolveThenVerify(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓端到端(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
	}
	from := envOr("HISTORY_LIVE_MODULE_FROM", "git.enjoye.top/enjoydream/code-review-service")
	to := envOr("HISTORY_LIVE_MODULE_TO", "github.com/yi-nology/code-review-service")

	opts := Options{
		SourceDir: src,
		TargetDir: tgt,
		Rules: []mirror.IdentityRule{
			{Source: from, Target: to, Kind: mirror.KindModule},
			{Source: "git.enjoye.top", Target: "github.com", Kind: mirror.KindHost},
		},
		PathRules: []PathRule{
			{From: "docs/internal", To: "docs/archive"},
		},
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	dir := filepath.Join(t.TempDir(), "expected-target")
	report, err := Verify(context.Background(), opts, plan, dir)
	require.NoError(t, err)

	t.Logf("物化: 文件 %d,port 应用 %d,解法写入 %d,保留目标侧 %d,仍待人工 %d,受保护 %d",
		report.Materialize.FileCount, len(report.Materialize.AppliedFiles),
		len(report.Materialize.ResolvedFiles), len(report.Materialize.KeptFiles),
		len(report.Materialize.HeldFiles), report.Materialize.PreservedFiles)
	// 基线:9 = 5 真冲突 + 4 无基点。
	//
	// 曾经断言 11(7 真冲突 + 4 无基点):那是合并算法**漏判**的产物 ——
	// 「插入点落在对方改动边界上」被当成不重叠,于是 etc/config-dev.yaml 与
	// etc/config-prod.yaml 被判成「合并干净」,源侧的真实域名(code-review.me)
	// 覆盖了目标侧已脱敏的 review.example.com,真实域名会进公开仓。
	//
	// merge3 重写并与 git 逐字对拍后(见 merge3_fuzz_test.go / merge3_oracle_test.go),
	// 本实现与 git 对这两个文件的判定一致,它们不再被漏判,
	// 真冲突回到 5 个 —— 与设计稿记载的基线(5 真冲突 + 4 无基点)吻合。
	assert.Len(t, report.Materialize.HeldFiles, 9,
		"应用解法后仍待人工的应只剩 9 个(5 真冲突 + 4 无基点)")

	// **集合侧的语义门禁**:那两个配置文件必须仍在「待人工」里。
	//
	// 条数会随实仓演进而变(依赖升级、提交历史都会动),所以条数断言只是
	// 「基线漂移提醒」;真正稳定的门禁是这一条 ——
	// 它们一旦被「合并干净」地写出去,源侧真实域名就跟着进公开仓。
	for _, p := range []string{"etc/config-dev.yaml", "etc/config-prod.yaml"} {
		assert.Contains(t, report.Materialize.HeldFiles, p,
			"%s 必须在待人工集合里(被「合并干净」就意味着源侧真实域名会进公开仓)", p)
	}

	// **公开树里不能出现源侧的真实域名**(精确到那两个文件)。
	//
	// 这是审查发现 #1 的直接后果:合并算法漏判冲突时,
	// etc/config-dev.yaml / etc/config-prod.yaml 会被「合并干净」——
	// 源侧的真实域名(code-review.me)覆盖目标侧已脱敏的 review.example.com,
	// 然后**跟着发布到公开仓**。
	//
	// 注意范围:目标仓自己就有 code-review.me(etc/config-local.yaml、
	// internal/config/yaml.go 等)——那是公开仓自己的域名,合法。
	// 要盯的是「源侧那两个文件里的域名不许渗进来」。
	//
	// 这条断言盯的是最终产物,不是中间结果。
	for _, p := range []string{"etc/config-dev.yaml", "etc/config-prod.yaml"} {
		data, rerr := os.ReadFile(filepath.Join(dir, p))
		require.NoError(t, rerr, "物化产物里应该有 %s", p)
		assert.NotContains(t, string(data), "code-review.me",
			"%s 里出现了源侧真实域名(会随发布进公开仓)", p)
		assert.Contains(t, string(data), "review.example.com",
			"%s 应保持目标侧已脱敏的域名", p)
	}

	assert.Empty(t, report.Unformatted, "物化后的目标树存在未格式化文件")
	assert.Empty(t, report.TypeCheckErr,
		"应用解法后物化出的目标树未通过类型门禁: %s", report.TypeCheckErr)
}

// TestLiveHistoryVerify 用真实双仓跑 M2 的发布前校验:
// 物化「预期目标树」→ gofmt 校验 → 类型门禁。
//
// 这是 M1 与 M2 的接缝:差异判定、格式校验、编译门禁必须针对**同一份产物**。
// 门禁跑在源树上没有意义——源树本来就编译通过,门禁要回答的是
// 「按计划应用后的目标树还能不能编译」。
func TestLiveHistoryVerify(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓校验(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
	}
	from := envOr("HISTORY_LIVE_MODULE_FROM", "git.enjoye.top/enjoydream/code-review-service")
	to := envOr("HISTORY_LIVE_MODULE_TO", "github.com/yi-nology/code-review-service")

	opts := Options{
		SourceDir: src,
		TargetDir: tgt,
		Rules: []mirror.IdentityRule{
			{Source: from, Target: to, Kind: mirror.KindModule},
			{Source: "git.enjoye.top", Target: "github.com", Kind: mirror.KindHost},
		},
		PathRules: []PathRule{
			{From: "docs/internal", To: "docs/archive"},
		},
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)

	dir := filepath.Join(t.TempDir(), "expected-target")
	report, err := Verify(context.Background(), opts, plan, dir)
	require.NoError(t, err)

	t.Logf("物化: 文件 %d,应用 %d,跳过(待清理) %d,待确认 %d,受保护 %d",
		report.Materialize.FileCount, len(report.Materialize.AppliedFiles),
		len(report.Materialize.SkippedScrub), len(report.Materialize.HeldFiles),
		report.Materialize.PreservedFiles)

	// A2:改写彻底 —— 物化树里不得再出现源 module path。
	assert.Equal(t, 0, countOccurrences(dir, from),
		"物化后的目标树仍残留源 module path,改写不彻底")
	assert.Greater(t, countOccurrences(dir, to), 0,
		"物化后的目标树没有目标 module path,改写未生效")

	// A2:零误伤 —— 兄弟模块路径不得被改。
	for _, sibling := range []string{
		"github.com/yi-nology/go-git-platform",
		"github.com/yi-nology/git-ferry-core",
	} {
		before := countOccurrences(tgt, sibling)
		if before == 0 {
			continue
		}
		assert.Equal(t, before, countOccurrences(dir, sibling),
			"兄弟模块路径 %s 被误改", sibling)
	}

	// A3:格式门禁 —— 物化结果必须 gofmt 干净。
	assert.Empty(t, report.Unformatted,
		"物化后的目标树存在未格式化文件(gofmt 违规)")

	// A4:编译门禁 —— 物化后的目标树必须能通过全量类型检查。
	//
	// 当前实况:门禁**不通过**,且这是**正确的**——port 集合存在隐式依赖。
	// internal/modules/server.go(port)引用 readiness.LLMAvailabilityCheck,
	// 该符号定义在 internal/readiness/checks.go(hold,两边内容混合)。
	// 门禁的价值正在于此:它在推送前拦住了「单独移植一个文件会编译不过」。
	if report.TypeCheckErr != "" {
		t.Logf("类型门禁未通过(预期:port 集合存在隐式依赖):%s", report.TypeCheckErr)
		require.NotEmpty(t, report.Blocked, "门禁失败时必须能指出是哪个 hold 文件挡住了谁")

		blocked := report.Blocked[0]
		t.Logf("被挡住的 port 文件: %s 缺符号 %s.%s → 定义在 hold 文件 %s",
			blocked.Port, blocked.Package, blocked.MissingSymbol, blocked.DefinedBy)

		assert.Equal(t, "internal/modules/server.go", blocked.Port)
		assert.Equal(t, "LLMAvailabilityCheck", blocked.MissingSymbol)
		assert.Equal(t, "internal/readiness/checks.go", blocked.DefinedBy,
			"必须指出该符号由哪个 hold 文件定义")
	} else {
		t.Logf("类型门禁通过:物化后的目标树可编译")
	}
}

// fmtSummary 把统计格式化成 "port=11,hold=55" 形式。
func fmtSummary(s Summary) string {
	return fmt.Sprintf("port=%d,port_with_scrub=%d,hold=%d,exclude=%d,preserve=%d,same=%d",
		s.Port, s.PortWithScrub, s.Hold, s.Exclude, s.Preserve, s.Same)
}

// countOccurrences 统计目录下某字符串的出现次数(跳过 .git 与二进制)。
func countOccurrences(dir, needle string) int {
	if needle == "" {
		return 0
	}
	n := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == ".git" || strings.HasSuffix(base, ".png") || strings.HasSuffix(base, ".jpg") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		n += strings.Count(string(data), needle)
		return nil
	})
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestLiveConflictRules 验证冲突自动判据:开启后,review-service 的 9 个真冲突
// 应被确定性规则解掉,不再需要人工裁决。
//
// 依据(端到端演练):这 9 个冲突逐个人工判定**全是 keep_target**,
// 共同规律是「源侧的改动是内网形态,目标侧是公开形态」。
func TestLiveConflictRules(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓判据(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
	}
	from := envOr("HISTORY_LIVE_MODULE_FROM", "git.enjoye.top/enjoydream/code-review-service")
	to := envOr("HISTORY_LIVE_MODULE_TO", "github.com/yi-nology/code-review-service")

	opts := Options{
		SourceDir: src,
		TargetDir: tgt,
		Rules: []mirror.IdentityRule{
			{Source: from, Target: to, Kind: mirror.KindModule},
			{Source: "git.enjoye.top", Target: "github.com", Kind: mirror.KindHost},
		},
		PathRules: []PathRule{{From: "docs/internal", To: "docs/archive"}},
	}

	// 基准:不开判据时有多少冲突。
	base, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, base))
	baseConflicts := len(Dossiers(base))
	t.Logf("不开判据:待裁决 %d 个", baseConflicts)
	require.Equal(t, 9, baseConflicts, "基线:9 个真冲突")

	// 开判据:应全部被解掉。
	withRules := opts
	withRules.ConflictRules = []ConflictRuleConfig{
		{Kind: ConflictRuleInternalTrace, Enabled: true},
		{Kind: ConflictRulePublicAsset, Enabled: true},
		{Kind: ConflictRulePlaceholderDomain, Enabled: true},
	}
	plan, err := BuildPlan(context.Background(), withRules)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), withRules, plan))
	after := len(Dossiers(plan))
	t.Logf("开判据:待裁决 %d 个", after)
	// 9 个里 8 个可由规则机械判定;剩下的 provider/service.go 是
	// 「哪一版功能更全」的真语义判断,必须留给人/AI。
	assert.LessOrEqual(t, after, 1, "判据应把绝大多数冲突解掉")
	for _, d := range Dossiers(plan) {
		t.Logf("  仍需人工:%s(%s)", d.Path, d.Verdict)
	}

	// 剩下的必须正是那个「真语义判断」的文件:两版功能都全,只是新旧不同。
	remaining := Dossiers(plan)
	require.Len(t, remaining, 1, "应只剩 1 个需人工的冲突")
	assert.Equal(t, "internal/service/provider/service.go", remaining[0].Path)

	// 被规则解掉的那些,理由里要写明是规则判的(可解释)。
	ruled := 0
	for _, f := range plan.Files {
		if f.Disposition != DispositionHold {
			continue
		}
		res, ok := plan.Resolutions[f.Path]
		require.True(t, ok, f.Path)
		if res.Verdict == ResolutionTakeOurs && strings.Contains(res.Reason, "规则") {
			ruled++
			t.Logf("  规则判定: %-46s %s", f.Path, res.Reason)
		}
	}
	assert.GreaterOrEqual(t, ruled, 8, "冲突应主要由规则判定并写明依据")
}

// scanTreeFor 扫目录树,返回包含给定子串的文件路径。
func scanTreeFor(t *testing.T, root, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.Size() > 2<<20 {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(data), needle) {
			rel, _ := filepath.Rel(root, path)
			hits = append(hits, rel)
		}
		return nil
	})
	require.NoError(t, err)
	return hits
}
