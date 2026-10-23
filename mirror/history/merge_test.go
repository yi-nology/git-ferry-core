package history

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
			want:          "a\n<<<<<<< 目标侧(保留)\nOURS\n=======\nTHEIRS\n>>>>>>> 源侧\n",
			wantConflicts: 1,
		},
		{
			name: "目标侧插入,源侧修改另一处",
			base: "a\nb\n", ours: "a\nX\nb\n", theirs: "a\nb\nc\n",
			want: "a\nX\nb\nc\n",
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
