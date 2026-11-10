package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMatchGlob 钉 allowlist glob 的保守语义:
// '*' 不跨段,'**' 作为独立段跨层,无分隔符模式只匹配单段。
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// 单段 '*' 不跨层
		{"*_test.go", "app_test.go", true},
		{"*_test.go", "sub/app_test.go", false},
		{"**/*_test.go", "sub/app_test.go", true},
		{"**/*_test.go", "a/b/app_test.go", true},
		// '**' 含零层
		{"fixtures/**", "fixtures", true},
		{"fixtures/**", "fixtures/a.json", true},
		{"fixtures/**", "other/fixtures/a.json", false},
		// '**' 在中间
		{"test/**/x.json", "test/x.json", true},
		{"test/**/x.json", "test/a/b/x.json", true},
		{"test/**/x.json", "other/test/x.json", false},
		// 无分隔符模式只匹配同名单段(不做 basename 语义)
		{"fixtures.json", "fixtures.json", true},
		{"fixtures.json", "sub/fixtures.json", false},
		{"**/fixtures.json", "sub/fixtures.json", true},
		// '?' 单字符
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		// 大小写敏感
		{"README", "readme", false},
		// 空串
		{"", "x", false},
		{"x", "", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, matchGlob(c.pattern, c.path),
			"pattern=%q path=%q", c.pattern, c.path)
	}
}

// TestValidateGlob:非法模式在建通道时就被拒(而不是发布时静默不放行)。
func TestValidateGlob(t *testing.T) {
	assert.NoError(t, ValidateGlob("**/*_test.go"))
	assert.NoError(t, ValidateGlob("fixtures/**"))
	assert.Error(t, ValidateGlob(""))
	assert.Error(t, ValidateGlob("a//b"))
	assert.Error(t, ValidateGlob(`a\b`))
	assert.Error(t, ValidateGlob("a[b-"))
}
