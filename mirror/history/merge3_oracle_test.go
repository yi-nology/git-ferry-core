package history

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 差分对拍:拿 git merge-file --diff3 当 oracle,逐字比对 merge3 的输出。
//
// 这是 merge3 的行为门禁 —— 历史同步要把内容发布到公开仓,
// 合并结果与 git 的语义必须一致,否则会出现「本地看着合干净、
// 推到公开仓却串味」这种只能靠人眼发现的错误。
//
// git 不可用时自动跳过(不阻断无 git 环境的 CI)。

var oursLabelRe = regexp.MustCompile(`(?m)^<<<<<<< .*$`)
var baseLabelRe = regexp.MustCompile(`(?m)^\|\|\|\|\|\|\| .*$`)
var theirsLabelRe = regexp.MustCompile(`(?m)^>>>>>>> .*$`)

// normalizeGit 把 git 的冲突标记标签换成我们的标签,只比内容不比标签措辞。
//
// 我们用 git 原生标签(ours/base/theirs),但 git 自己会带上文件名
// (<<<<<<< /tmp/xxx/ours)—— 归一后才好逐字比。
func normalizeGit(s string) string {
	s = oursLabelRe.ReplaceAllString(s, "<<<<<<< ours")
	s = baseLabelRe.ReplaceAllString(s, "||||||| base")
	s = theirsLabelRe.ReplaceAllString(s, ">>>>>>> theirs")
	return s
}

func gitMergeFile(t *testing.T, base, ours, theirs string) (string, bool) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	dir := t.TempDir()
	bf := filepath.Join(dir, "base")
	of := filepath.Join(dir, "ours")
	tf := filepath.Join(dir, "theirs")
	for path, content := range map[string]string{bf: base, of: ours, tf: theirs} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "merge-file", "-p", "--diff3", of, bf, tf)
	out, err := cmd.Output()
	// git merge-file 冲突时退出码非 0,但 stdout 仍带完整合并结果。
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			_ = ee
		} else {
			t.Fatalf("git merge-file 执行失败: %v", err)
		}
	}
	return string(out), true
}

type diff3Case struct {
	name   string
	base   string
	ours   string
	theirs string
	// wantConflict 为期望的冲突块数;-1 表示不校验(只比内容)。
	wantConflict int
}

func TestMerge3AgainstGitOracle(t *testing.T) {
	cases := []diff3Case{
		{
			name:   "两侧改不同行-无冲突",
			base:   "a\nb\nc\nd\n",
			ours:   "A\nb\nc\nd\n",
			theirs: "a\nb\nc\nD\n",
		},
		{
			name:   "两侧改同一行-冲突",
			base:   "a\nb\nc\n",
			ours:   "a\nX\nc\n",
			theirs: "a\nY\nc\n",
		},
		{
			name:   "两侧改成同样内容-无冲突",
			base:   "a\nb\nc\n",
			ours:   "a\nX\nc\n",
			theirs: "a\nX\nc\n",
		},
		{
			name:   "一侧插入-另一侧改别处",
			base:   "a\nb\nc\n",
			ours:   "a\nb\nNEW\nc\n",
			theirs: "a\nb\nC\n",
		},
		{
			name:   "一侧插入-另一侧改相邻行",
			base:   "a\nb\nc\n",
			ours:   "a\nNEW\nb\nc\n",
			theirs: "a\nB\nc\n",
		},
		{
			name:   "两侧都在同一位置插入-不同内容",
			base:   "a\nb\n",
			ours:   "a\nX\nb\n",
			theirs: "a\nY\nb\n",
		},
		{
			name:   "两侧都在同一位置插入-相同内容",
			base:   "a\nb\n",
			ours:   "a\nX\nb\n",
			theirs: "a\nX\nb\n",
		},
		{
			name:   "一侧删除-另一侧插入别处",
			base:   "a\nb\nc\n",
			ours:   "a\nc\n",
			theirs: "a\nb\nNEW\nc\n",
		},
		{
			name:   "一侧删除-另一侧改被删行",
			base:   "a\nb\nc\n",
			ours:   "a\nc\n",
			theirs: "a\nB\nc\n",
		},
		{
			name:   "一侧删除-另一侧插入被删位置",
			base:   "a\nb\nc\n",
			ours:   "a\nc\n",
			theirs: "a\nNEW\nc\n",
		},
		{
			name:   "两侧都删除-相同",
			base:   "a\nb\nc\n",
			ours:   "a\nc\n",
			theirs: "a\nc\n",
		},
		{
			name:   "两侧都删除-不同",
			base:   "a\nb\nc\nd\n",
			ours:   "a\nd\n",
			theirs: "a\nc\nd\n",
		},
		{
			name:   "一侧全删",
			base:   "a\nb\nc\n",
			ours:   "",
			theirs: "a\nb\nc\n",
		},
		{
			name:   "两侧全改-范围不同",
			base:   "a\nb\nc\nd\ne\n",
			ours:   "A\nb\nc\nd\nE\n",
			theirs: "A\nB\nc\nd\ne\n",
		},
		{
			name:   "末尾插入-末尾被改",
			base:   "a\nb\nc\n",
			ours:   "a\nb\nc\nX\n",
			theirs: "a\nb\nC\n",
		},
		{
			name:   "开头插入-开头被改",
			base:   "a\nb\nc\n",
			ours:   "X\na\nb\nc\n",
			theirs: "A\nb\nc\n",
		},
		{
			name:   "两侧各自插入不同位置",
			base:   "a\nb\nc\nd\n",
			ours:   "a\nX\nb\nc\nd\n",
			theirs: "a\nb\nc\nY\nd\n",
		},
		{
			name:   "改动相邻但不重叠",
			base:   "a\nb\nc\nd\n",
			ours:   "A\nb\nc\nd\n",
			theirs: "a\nb\nC\nd\n",
		},
		{
			name:   "无结尾换行-两侧改不同行",
			base:   "a\nb\nc",
			ours:   "A\nb\nc",
			theirs: "a\nb\nC",
		},
		{
			name:   "空 base-两侧不同",
			base:   "",
			ours:   "a\n",
			theirs: "b\n",
		},
		{
			name:   "两侧都删空行",
			base:   "a\n\nb\n",
			ours:   "a\nb\n",
			theirs: "a\nb\n",
		},
		{
			name:   "一侧删空行-另一侧在空行处插入",
			base:   "a\n\nb\n",
			ours:   "a\nb\n",
			theirs: "a\nX\nb\n",
		},
		{
			name:   "真实场景-域名脱敏冲突",
			base:   "# 对外基地址：镜像源地址，不携带 target_url\n",
			ours:   "# 对外基地址：镜像源地址，不携带 target_url\n# external_base_url: http://review.example.com\n",
			theirs: "# 对外基地址：镜像源地址，不携带 target_url\n# external_base_url: http://code-review.me\n",
		},
		{
			name:   "真实场景-一侧改注释行",
			base:   "# 对外基地址：镜像源地址，不携带 target_url\n",
			ours:   "# 对外基地址：镜像源地址，不携带 target_url\n",
			theirs: "# 对外基地址：镜像源地址，不携带 target_url\n# external_base_url: http://code-review.me\n",
		},
		{
			name:   "两侧插入位置相邻但不同",
			base:   "a\nb\n",
			ours:   "a\nX\nb\n",
			theirs: "a\nb\nY\n",
		},
		{
			name:   "插入点-改动区间左侧相邻",
			base:   "a\nb\nc\nd\n",
			ours:   "a\nb\nNEW\nc\nd\n",
			theirs: "a\nB\nc\nd\n",
		},
		{
			name:   "插入点-改动区间右侧相邻",
			base:   "a\nb\nc\nd\n",
			ours:   "a\nb\nc\nNEW\nd\n",
			theirs: "a\nb\nc\nD\n",
		},
		{
			name:   "插入点-改动区间隔一行",
			base:   "a\nb\nc\nd\ne\n",
			ours:   "a\nb\nNEW\nc\nd\ne\n",
			theirs: "a\nb\nC\nd\ne\n",
		},
		{
			name:   "两侧改动紧邻(无间隔)",
			base:   "a\nb\nc\n",
			ours:   "A\nb\nc\n",
			theirs: "a\nB\nc\n",
		},
		{
			name:   "两侧改动隔一行",
			base:   "a\nb\nc\n",
			ours:   "A\nb\nc\n",
			theirs: "a\nb\nC\n",
		},
		{
			name:   "一侧插入-另一侧删除同一行",
			base:   "a\nb\nc\n",
			ours:   "a\nNEW\nc\n",
			theirs: "a\nc\n",
		},
		{
			name:   "一侧插入-另一侧删除相邻行",
			base:   "a\nb\nc\nd\n",
			ours:   "a\nb\nNEW\nc\nd\n",
			theirs: "a\nc\nd\n",
		},
		{
			name:   "两侧都在末尾插入-不同",
			base:   "a\nb\n",
			ours:   "a\nb\nX\n",
			theirs: "a\nb\nY\n",
		},
		{
			name:   "一侧末尾插入-另一侧改最后一行",
			base:   "a\nb\n",
			ours:   "a\nb\nX\n",
			theirs: "a\nB\n",
		},
		{
			name:   "一侧开头插入-另一侧改第一行",
			base:   "a\nb\n",
			ours:   "X\na\nb\n",
			theirs: "A\nb\n",
		},
		{
			name:   "两侧各自改不同位置-中间有改动",
			base:   "a\nb\nc\nd\ne\nf\n",
			ours:   "A\nb\nc\nd\ne\nF\n",
			theirs: "a\nb\nC\nd\ne\nf\n",
		},
		{
			name:   "一侧全改-另一侧改一行",
			base:   "a\nb\nc\n",
			ours:   "x\ny\nz\n",
			theirs: "a\nB\nc\n",
		},
		{
			name:   "一侧全删-另一侧插入",
			base:   "a\nb\nc\n",
			ours:   "",
			theirs: "a\nNEW\nb\nc\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gitOut, ok := gitMergeFile(t, tc.base, tc.ours, tc.theirs)
			if !ok {
				t.Skip("git 不可用")
			}
			want := normalizeGit(gitOut)

			got, conflicts := merge3(tc.base, tc.ours, tc.theirs)

			if got != want {
				t.Errorf("内容不一致\n--- git ---\n%s\n--- ours ---\n%s", show(gitOut), show(got))
			}
			gitConflicts := strings.Count(gitOut, "<<<<<<< ")
			if conflicts != gitConflicts {
				t.Errorf("冲突数不一致: ours=%d git=%d\n--- git ---\n%s\n--- ours ---\n%s",
					conflicts, gitConflicts, show(gitOut), show(got))
			}
		})
	}
}

func show(s string) string {
	return strings.ReplaceAll(s, "\n", "\\n\n")
}

// 把单测里的关键用例也纳入 git oracle 对拍。
func TestMerge3OracleExtra(t *testing.T) {
	cases := []diff3Case{
		{
			name: "插入落在改动边界",
			base: "l1\nl2\nl3\n", ours: "l1\nl2\nNEW\nl3\n", theirs: "l1\nl2\nL3\n",
		},
		{
			name:   "脱敏域名与真实域名",
			base:   "# base\n",
			ours:   "# base\n# external_base_url: http://review.example.com\n",
			theirs: "# base\n# external_base_url: http://code-review.me\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gitOut, ok := gitMergeFile(t, tc.base, tc.ours, tc.theirs)
			if !ok {
				t.Skip("git 不可用")
			}
			want := normalizeGit(gitOut)
			got, conflicts := merge3(tc.base, tc.ours, tc.theirs)
			if got != want {
				t.Errorf("内容不一致\n--- git ---\n%s\n--- ours ---\n%s", show(gitOut), show(got))
			}
			if conflicts != strings.Count(gitOut, "<<<<<<< ") {
				t.Errorf("冲突数不一致: ours=%d git=%d", conflicts, strings.Count(gitOut, "<<<<<<< "))
			}
		})
	}
}
