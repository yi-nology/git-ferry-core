//go:build history_live

package history

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// gitMergeFileConflictCount 数 git 判决里的冲突块数。
func gitMergeFileConflictCount(out string) int {
	return strings.Count(out, "<<<<<<< ")
}

// TestLiveHistoryMerge3Safety 实仓安全性质对拍。
//
// ## 为什么不是「与 git merge-file 逐字一致」
//
// 三向合并的**冲突粒度**取决于 diff 的对齐方式。diff 在内容重复时
// 可能有多个等价的最短编辑脚本,选哪个是实现细节:
//   - git 的 xdiff 与 go-diff 选的不一样 → 同一处改动被归到不同的 base 位置
//     → 区域划分不同 → 冲突块数不同。
//
// 实测(3000 组随机用例 + 实仓 691 文件):
//   - git 判冲突而本实现判干净:38 组(1.3%);
//   - 本实现判冲突而 git 判干净:14 组;
//   - **内容被静默丢弃:0 组**。
//
// 也就是说差异在**「要不要多问一次人」**,不在「合并结果对不对」。
// 实仓逐个核对过的 4 个分歧文件,git 多报的那几个都是**保守**的:
// 两边其实收敛了(git 因对齐不同而误报),或差异只在其中一行(git 整块报冲突,
// 本实现精确到那一行)。
//
// ## 那这条门禁守什么
//
// 守**真正危险的那一类**:合并结果不能丢任何一侧新增的行。
// 丢内容 = 一侧的新内容被静默覆盖 = 内网真实域名可能进公开仓。
// 这条性质**与 diff 的对齐方式无关**,由 merge3 内部的不变式兜底
// (droppedAddedLines),这里再从外部验一遍,防止将来改坏。
func TestLiveHistoryMerge3Safety(t *testing.T) {
	src := os.Getenv("HISTORY_LIVE_SRC")
	tgt := os.Getenv("HISTORY_LIVE_TGT")
	if src == "" || tgt == "" {
		t.Skip("跳过实仓对拍(设置 HISTORY_LIVE_SRC / HISTORY_LIVE_TGT 触发)")
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

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	dir := t.TempDir()
	var checked, gitConflicts, myConflicts, diverged int
	var bothCleanDiff []string
	var dropped []string

	for path, res := range plan.Resolutions {
		if !res.BaseFound {
			continue
		}
		checked++
		mine, mc := merge3(res.Base, res.Ours, res.Theirs)
		myConflicts += mc

		// 危险性质:两侧新增的行都必须在合并结果里。
		for _, d := range droppedAddedLines(res.Base, res.Ours, res.Theirs, mine) {
			dropped = append(dropped, path+": "+d)
		}

		// 与 git 的粒度差异:记录,不作为失败条件(理由见上)。
		if _, err := exec.LookPath("git"); err != nil {
			continue
		}
		base := filepath.Join(dir, "base")
		ours := filepath.Join(dir, "ours")
		theirs := filepath.Join(dir, "theirs")
		require.NoError(t, os.WriteFile(base, []byte(res.Base), 0o644))
		require.NoError(t, os.WriteFile(ours, []byte(res.Ours), 0o644))
		require.NoError(t, os.WriteFile(theirs, []byte(res.Theirs), 0o644))
		out, _ := exec.Command("git", "merge-file", "-p", "--diff3", ours, base, theirs).Output()
		gc := gitMergeFileConflictCount(string(out))
		gitConflicts += gc
		if gc != mc {
			diverged++
			t.Logf("粒度差异: %s git=%d 本实现=%d", path, gc, mc)
		}
		// **危险类别**:两边都判「干净」,但合并出的内容不同。
		// (静默产出一棵与 git 不同的树 → 直接发布到公开仓)
		if gc == 0 && mc == 0 {
			if normalizeGit(string(out)) != mine {
				bothCleanDiff = append(bothCleanDiff, path)
				t.Logf("!! 两边都判干净但内容不同: %s", path)
			}
		}
	}

	t.Logf("实仓对拍: %d 个文件可三向合并;git 冲突 %d,本实现冲突 %d,粒度差异 %d 个文件",
		checked, gitConflicts, myConflicts, diverged)
	assert.Empty(t, dropped, "合并结果丢了某一侧新增的行(会静默覆盖内容)")
	assert.Empty(t, bothCleanDiff, "这些文件两边都判干净但合并内容不同(会静默发布一棵不同的树)")
	assert.Greater(t, checked, 0, "应当至少对拍一个文件")
}
