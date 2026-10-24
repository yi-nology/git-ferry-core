package history

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMerge3(t *testing.T) {
	tests := []struct {
		name          string
		base          string
		ours          string
		theirs        string
		want          string
		wantConflicts int
	}{
		{
			name: "两侧相同",
			base: "a\nb\n", ours: "a\nb\n", theirs: "a\nb\n",
			want: "a\nb\n",
		},
		{
			name: "只有源侧改动",
			base: "a\nb\n", ours: "a\nb\n", theirs: "a\nb\nc\n",
			want: "a\nb\nc\n",
		},
		{
			name: "只有目标侧改动",
			base: "a\nb\n", ours: "a\nb\nc\n", theirs: "a\nb\n",
			want: "a\nb\nc\n",
		},
		{
			name: "两侧改不同位置,自动合并",
			base: "a\nb\nc\n", ours: "X\na\nb\nc\n", theirs: "a\nb\nc\nY\n",
			want: "X\na\nb\nc\nY\n",
		},
		{
			name: "两侧改同一位置,冲突",
			base: "a\nb\n", ours: "a\nOURS\n", theirs: "a\nTHEIRS\n",
			want:          "a\n<<<<<<< ours\nOURS\n||||||| base\nb\n=======\nTHEIRS\n>>>>>>> theirs\n",
			wantConflicts: 1,
		},
		{
			name: "目标侧插入,源侧修改另一处",
			base: "a\nb\n", ours: "a\nX\nb\n", theirs: "a\nb\nc\n",
			want: "a\nX\nb\nc\n",
		},
		{
			// 回归:插入点正好落在源侧改动的边界上 —— 改的是同一处,
			// 必须判冲突。漏判会让源侧内容直接覆盖目标侧已脱敏的内容。
			name: "目标侧插入落在源侧改动的边界上",
			base: "l1\nl2\nl3\n", ours: "l1\nl2\nNEW\nl3\n", theirs: "l1\nl2\nL3\n",
			want: "l1\nl2\n" +
				"<<<<<<< ours\nNEW\nl3\n" +
				"||||||| base\nl3\n" +
				"=======\nL3\n" +
				">>>>>>> theirs\n",
			wantConflicts: 1,
		},
		{
			// 回归:两侧在相邻位置改动 —— git 会合成一块,行为要一致。
			name: "两侧改动紧邻",
			base: "a\nb\nc\n", ours: "A\nb\nc\n", theirs: "a\nB\nc\n",
			want:          "<<<<<<< ours\nA\nb\n||||||| base\na\nb\n=======\na\nB\n>>>>>>> theirs\nc\n",
			wantConflicts: 1,
		},
		{
			// 回归:冲突标记里必须带基点段,否则裁决者看不到「原本是什么」。
			name: "冲突块含基点段",
			base: "x\n", ours: "OURS\n", theirs: "THEIRS\n",
			want:          "<<<<<<< ours\nOURS\n||||||| base\nx\n=======\nTHEIRS\n>>>>>>> theirs\n",
			wantConflicts: 1,
		},
		{
			// 回归:真实场景 —— 目标侧已脱敏的域名 vs 源侧真实域名。
			// 漏判冲突会把真实域名带进公开树。
			name:   "脱敏域名与真实域名冲突",
			base:   "# base\n",
			ours:   "# base\n# external_base_url: http://review.example.com\n",
			theirs: "# base\n# external_base_url: http://code-review.me\n",
			want: "# base\n" +
				"<<<<<<< ours\n" +
				"# external_base_url: http://review.example.com\n" +
				"||||||| base\n" +
				"=======\n" +
				"# external_base_url: http://code-review.me\n" +
				">>>>>>> theirs\n",
			wantConflicts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, conflicts := merge3(tt.base, tt.ours, tt.theirs)
			assert.Equal(t, tt.wantConflicts, conflicts, "冲突数")
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestMerge3KeepsNonConflictingLines 确认合并不会丢行:
// 两侧在**不同**位置改动时,两侧的改动都要保留。
func TestMerge3KeepsNonConflictingLines(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\n"
	ours := "l1\nOURS\nl3\nl4\nl5\n"
	theirs := "l1\nl2\nl3\nl4\nTHEIRS\n"

	got, conflicts := merge3(base, ours, theirs)
	assert.Equal(t, 0, conflicts)
	assert.Contains(t, got, "OURS")
	assert.Contains(t, got, "THEIRS")
	assert.NotContains(t, got, "<<<<<<<")
}

// TestMerge3ConflictMarksBothSides 确认冲突标记里两侧内容都在。
func TestMerge3ConflictMarksBothSides(t *testing.T) {
	base := "x\n"
	ours := "OURS\n"
	theirs := "THEIRS\n"

	got, conflicts := merge3(base, ours, theirs)
	assert.Equal(t, 1, conflicts)
	assert.True(t, strings.Contains(got, "OURS"))
	assert.True(t, strings.Contains(got, "THEIRS"))
	assert.True(t, strings.Contains(got, "<<<<<<<"))
	assert.True(t, strings.Contains(got, ">>>>>>>"))
}

func TestSplitAndJoinLines(t *testing.T) {
	assert.Nil(t, splitLines(""))
	assert.Equal(t, []string{"a", "b"}, splitLines("a\nb"))
	assert.Equal(t, []string{"a", "b"}, splitLines("a\nb\n"))
	assert.Equal(t, []string{"a", ""}, splitLines("a\n\n"))
	assert.Equal(t, "a\nb\n", joinLines([]string{"a", "b"}))
	assert.Equal(t, "", joinLines(nil))
}

func TestGitBlobHash(t *testing.T) {
	// git blob hash = sha1("blob <len>\0" + content)
	assert.Equal(t,
		"e69de29bb2d1d6434b8b29ae775ad8c2e48c5391",
		gitBlobHash(nil))
	assert.Len(t, gitBlobHash([]byte("hello")), 40)
}

// TestMerge3SameInsertionAppliedOnce 回归:同一处插入被两侧各记一次时,
// 只能应用一次。
//
// diff 在内容重复时,插入的那一行与附近同样的行无法区分 —— 同一处插入
// 可能被两侧锚到不同的 base 位置。锚点不同 → 两块不相交 → 各自应用
// → **同一份内容被插入两遍**,而且两边都判「干净」→ 直接发布到公开仓。
//
// 这最小复现(来自对抗式审查):
//
//	base   = a b a a b
//	ours   = b a a a b      # 改首行 + 插入一个 a
//	theirs = a b a a a b    # 只插入一个 a
//
// git merge-file --diff3 的结果是 b a a a b(5 行,只应用一次)。
func TestMerge3SameInsertionAppliedOnce(t *testing.T) {
	base := "a\nb\na\na\nb\n"
	ours := "b\na\na\na\nb\n"
	theirs := "a\nb\na\na\na\nb\n"

	got, conflicts := merge3(base, ours, theirs)
	assert.Equal(t, 0, conflicts)
	assert.Equal(t, "b\na\na\na\nb\n", got, "同一处插入只能应用一次")
}

// TestMerge3KeepsTrailingNewline 回归:文件末尾的换行/空行不能丢。
//
// 拼接时若靠「拼出来是否以 \\n 结尾」判断,末行是空行时就会少一个换行
// (Join(["", "c", ""]) = "\\nc\\n",正确内容是 "\\nc\\n\\n")。
// 实测:随机对拍里 2.3% 的「与 git 内容不同」全是这一类。
func TestMerge3KeepsTrailingNewline(t *testing.T) {
	base := "c\n# head\n\n"
	ours := "\nc\n\n"
	theirs := "c\n\n"

	got, _ := merge3(base, ours, theirs)
	assert.Equal(t, "\nc\n\n", got, "末尾的空行/换行不能丢")
}

// TestMerge3KeepsTwoNearbyInsertions 回归:两处**相邻**的不同位置插入
// 不能被合成成一处(会造出假冲突,也会丢内容)。
//
// 合成只在「锚点真有歧义」时做:插入的那一行与 base 里同样的行无法区分,
// 才可能��同一处插入被两侧锚到不同位置。
//
// base=a b c,一侧在 b 前插 X、另一侧在 b 后插 X:
// 插入内容 X 在 base 附近根本不存在 → 锚点是唯一的 → 两处就是两处。
func TestMerge3KeepsTwoNearbyInsertions(t *testing.T) {
	base := "a\nb\nc\n"
	ours := "a\nX\nb\nc\n"
	theirs := "a\nb\nX\nc\n"

	got, conflicts := merge3(base, ours, theirs)
	assert.Equal(t, 0, conflicts, "两处不同的插入不该被合成成冲突")
	assert.Equal(t, "a\nX\nb\nX\nc\n", got, "两处插入都要保留")
}

// TestMerge3SameInsertionAppliedOnceReversed 同一个复现的**反向**:
// ours 与 theirs 对调,结果必须一样。
//
// 合成逻辑曾经只认「ours 的锚点在前」,把两侧对调就漏判 ——
// 同一处插入被应用两遍,而且判干净、无冲突标记,直接发布到公开仓。
func TestMerge3SameInsertionAppliedOnceReversed(t *testing.T) {
	base := "a\nb\na\na\nb\n"
	ours := "a\nb\na\na\na\nb\n" // 插入一个 a
	theirs := "b\na\na\na\nb\n"  // 改首行 + 插入同一个 a

	got, conflicts := merge3(base, ours, theirs)
	assert.Equal(t, 0, conflicts)
	assert.Equal(t, "b\na\na\na\nb\n", got, "同一处插入只能应用一次(反向也要)")
}

// TestSafeJoinRejectsTraversal 物化的目标路径不能越出工作目录。
//
// 目标路径来自路径规则的 `To`,而规则是调用方给的 ——
// `To` 里带 `..` 就能写到目录之外(实测能写到 out/../escaped/x.go)。
func TestSafeJoinRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"../escaped/x.go",
		"a/../../escaped/x.go",
		"/abs/path/x.go",
		"",
	} {
		_, err := safeJoin(dir, bad)
		require.Error(t, err, "路径 %q 必须被拒绝", bad)
	}
	// 正常路径要能过。
	got, err := safeJoin(dir, "docs/a/b.md")
	require.NoError(t, err)
	require.Contains(t, got, "docs")
}
