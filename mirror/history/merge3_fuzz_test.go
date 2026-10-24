package history

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMerge3RandomParity 随机对拍:merge3 与 `git merge-file --diff3` 的关系。
//
// ## 这条测试守什么
//
// 守**真正危险的那一类**:合并结果不能丢任何一侧新增的行。
// 丢内容 = 一侧的新内容被静默覆盖 = 内网真实域名可能进公开仓。
// 这条性质**与 diff 的对齐方式无关**。
//
// ## 为什么不断言「与 git 逐字一致」
//
// 三向合并的**冲突粒度**取决于 diff 的对齐方式。内容重复时,最短编辑脚本
// 不唯一,选哪个是实现细节 —— git 的 xdiff 与 go-diff 选的不一样,
// 于是同一处改动被归到不同的 base 位置,区域划分不同,冲突块数不同。
//
// 实测(3000 组随机用例):git 判冲突而本实现判干净 38 组(1.3%),
// 反向 14 组,**内容被静默丢弃 0 组**。
// 逐个核对过:git 多报的那几个是**保守**的 —— 两边其实收敛了,
// 或差异只在其中一行(git 整块报冲突,本实现精确到那一行)。
//
// 所以本测试断言「不丢内容」,并把粒度差异统计出来供人看。
func TestMerge3RandomParity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	n := 500
	if v := os.Getenv("HISTORY_MERGE_FUZZ_N"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			n = k
		}
	}
	rng := rand.New(rand.NewSource(42))
	dir := t.TempDir()
	vocab := []string{
		"a", "b", "c", "", "---", "# head", "x = 1", "url",
		// 带真实域名的行:模拟「脱敏 vs 真实」这类高危内容。
		"code-review.me", "review.example.com",
	}
	gen := func(k int) []string {
		out := make([]string, k)
		for i := range out {
			out[i] = vocab[rng.Intn(len(vocab))]
		}
		return out
	}
	mutate := func(src []string) []string {
		out := append([]string{}, src...)
		for i := 0; i < 1+rng.Intn(3); i++ {
			switch rng.Intn(3) {
			case 0:
				if len(out) > 0 {
					out[rng.Intn(len(out))] = vocab[rng.Intn(len(vocab))]
				}
			case 1:
				p := rng.Intn(len(out) + 1)
				out = append(out[:p], append([]string{vocab[rng.Intn(len(vocab))]}, out[p:]...)...)
			case 2:
				if len(out) > 1 {
					p := rng.Intn(len(out))
					out = append(out[:p], out[p+1:]...)
				}
			}
		}
		return out
	}

	var gitConflictMineClean, mineConflictGitClean, bothConflict, bothCleanDiff int
	var realDiff, blankOnly int
	var cleanDiffSamples []string
	var dropped []string
	for i := 0; i < n; i++ {
		base := gen(1 + rng.Intn(8))
		ours := mutate(base)
		theirs := mutate(base)
		bs, os_, ts := joinLines(base), joinLines(ours), joinLines(theirs)

		if err := os.WriteFile(dir+"/b", []byte(bs), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/o", []byte(os_), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/t", []byte(ts), 0o644); err != nil {
			t.Fatal(err)
		}
		out, _ := exec.Command("git", "merge-file", "-p", "--diff3", dir+"/o", dir+"/b", dir+"/t").Output()
		gc := strings.Count(string(out), "<<<<<<< ")

		// 用 merge3Result 拿「升级前」的合并结果:
		// 不变式触发时 merge3 会换成整文件冲突(内嵌完整 ours+theirs),
		// 对它重算「有没有丢行」必然为空 → 断言就成了永远绿的假绿。
		merged, mc, _ := merge3Result(bs, os_, ts)
		switch {
		case gc > 0 && mc == 0:
			gitConflictMineClean++
		case gc == 0 && mc > 0:
			mineConflictGitClean++
		case gc > 0 && mc > 0:
			bothConflict++
		}
		// 危险类别:两边都判干净,但合并出的内容不同。
		if gc == 0 && mc == 0 {
			gitOut := string(out)
			if gitOut != merged {
				bothCleanDiff++
				// 区分「只是尾部空行」与「真的内容不同」。
				if diffNonEmpty(splitLines(gitOut), splitLines(merged)) {
					realDiff++
					if len(cleanDiffSamples) < 3 {
						cleanDiffSamples = append(cleanDiffSamples,
							fmt.Sprintf("base=%q ours=%q theirs=%q\n git=%q\n mine=%q",
								base, ours, theirs, gitOut, merged))
					}
				} else {
					blankOnly++
				}
			}
		}
		// **独立实现**丢行检测(不复用被测代码的 droppedAddedLines):
		// 复用被测代码 → 变异测试时断言可能恒真。
		if d := droppedLinesIndependent(bs, os_, ts, merged); len(d) > 0 {
			dropped = append(dropped, fmt.Sprintf("case %d: base=%q ours=%q theirs=%q 丢=%q",
				i, base, ours, theirs, d))
		}
	}

	t.Logf("%d 组随机对拍: git冲突/我干净=%d  我冲突/git干净=%d  两边都冲突=%d  都干净但内容不同=%d(其中真内容差异=%d 仅空行=%d)",
		n, gitConflictMineClean, mineConflictGitClean, bothConflict, bothCleanDiff, realDiff, blankOnly)
	for _, s := range cleanDiffSamples {
		t.Log(s)
	}
	assert.Empty(t, dropped, "合并结果丢了某一侧新增的行:\n%s", strings.Join(dropped, "\n"))
	// **两边都判干净、但合并内容与 git 不同(非空行级别)** → 必须为 0。
	//
	// 这是「同一处插入被应用两遍」的兜底:重复的内容**不会丢**,
	// 丢行检测抓不到它,但它会让合并结果与 git 不同。
	// (行计数不变式抓不到重复:「同一处」与「两处」在行计数上不可分。)
	assert.Equal(t, 0, realDiff,
		"两边都判干净但合并内容与 git 不同(非空行级别)——多半是同一处改动被应用了两遍:\n%s",
		strings.Join(cleanDiffSamples, "\n"))
}

// droppedLinesIndependent 独立实现的丢行检测。
//
// **不复用**被测代码里的 droppedAddedLines —— 复用会让变异测试失去意义:
// 不变式触发时 merge3 返回整文件冲突(内嵌完整 ours+theirs),
// 对它重算必然得到空,断言永远绿。
func droppedLinesIndependent(base, ours, theirs, merged string) []string {
	mergedCount := map[string]int{}
	for _, l := range strings.Split(merged, "\n") {
		mergedCount[l]++
	}
	otherCount := map[string]int{}
	for _, l := range strings.Split(ours, "\n") {
		otherCount[l]++
	}
	var out []string
	for _, l := range addedLinesMultiset(base, theirs) {
		if mergedCount[l] > 0 {
			mergedCount[l]--
			continue
		}
		if otherCount[l] > 0 {
			// 另一侧本来就有这一行 → 不算「源侧新增的内容消失」。
			continue
		}
		out = append(out, l)
	}
	return out
}

// addedLinesMultiset 返回 side 相对 base 多出来的行(多重集差集)。
func addedLinesMultiset(base, side string) []string {
	count := map[string]int{}
	for _, l := range strings.Split(base, "\n") {
		count[l]++
	}
	var out []string
	for _, l := range strings.Split(side, "\n") {
		if count[l] > 0 {
			count[l]--
			continue
		}
		out = append(out, l)
	}
	return out
}

// diffNonEmpty 判断两段行是否存在「非空行」级别的差异。
func diffNonEmpty(a, b []string) bool {
	ca := map[string]int{}
	for _, l := range a {
		if l != "" {
			ca[l]++
		}
	}
	cb := map[string]int{}
	for _, l := range b {
		if l != "" {
			cb[l]++
		}
	}
	for k, v := range ca {
		if cb[k] != v {
			return true
		}
	}
	for k, v := range cb {
		if ca[k] != v {
			return true
		}
	}
	return false
}
