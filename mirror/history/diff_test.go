package history

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyDiff(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		source      string
		want        diffVerdict
		wantAdded   int
		wantRemoved int
	}{
		{
			name:   "完全一致",
			target: "a\nb\n",
			source: "a\nb\n",
			want:   diffIdentical,
		},
		{
			name:      "源侧只增不删",
			target:    "a\nb\n",
			source:    "a\nb\nc\n",
			want:      diffSourceAhead,
			wantAdded: 1,
		},
		{
			name:        "目标侧只增不删",
			target:      "a\nb\nc\n",
			source:      "a\nb\n",
			want:        diffTargetAhead,
			wantRemoved: 1,
		},
		{
			name:        "两侧各有独有内容",
			target:      "a\nb\nc\n",
			source:      "a\nx\n",
			want:        diffMixed,
			wantAdded:   1,
			wantRemoved: 2,
		},
		{
			name:        "仅措辞不同(等行数)",
			target:      "docs: 补「预编译二进制」构建说明\n",
			source:      "docs: 补「预编译二进制」发布说明\n",
			want:        diffMixed,
			wantAdded:   1,
			wantRemoved: 1,
		},
		{
			name:      "空目标侧",
			target:    "",
			source:    "a\n",
			want:      diffSourceAhead,
			wantAdded: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, added, removed := classifyDiff([]byte(tt.target), []byte(tt.source))
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantAdded, added, "added")
			assert.Equal(t, tt.wantRemoved, removed, "removed")
		})
	}
}

func TestCountLines(t *testing.T) {
	assert.Equal(t, 0, countLines(nil))
	assert.Equal(t, 0, countLines([]byte("")))
	assert.Equal(t, 1, countLines([]byte("a")))
	assert.Equal(t, 1, countLines([]byte("a\n")))
	assert.Equal(t, 2, countLines([]byte("a\nb")))
	assert.Equal(t, 2, countLines([]byte("a\nb\n")))
}
