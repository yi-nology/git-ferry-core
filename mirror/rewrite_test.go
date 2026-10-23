package mirror

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteIdentitiesModule(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		rules []IdentityRule
		want  string
	}{
		{
			name:  "go.mod module 声明",
			data:  "module github.com/a/b\n\ngo 1.26\n",
			rules: []IdentityRule{{Source: "github.com/a/b", Target: "github.com/x/y"}},
			want:  "module github.com/x/y\n\ngo 1.26\n",
		},
		{
			name:  "子包路径也被改写",
			data:  "import \"github.com/a/b/pkg/client\"\n",
			rules: []IdentityRule{{Source: "github.com/a/b", Target: "github.com/x/y"}},
			want:  "import \"github.com/x/y/pkg/client\"\n",
		},
		{
			name:  "b-extra 不被误改",
			data:  "module github.com/a/b-extra\n",
			rules: []IdentityRule{{Source: "github.com/a/b", Target: "github.com/x/y"}},
			want:  "module github.com/a/b-extra\n",
		},
		{
			name:  "b.cool 不被误改",
			data:  "require github.com/a/b.cool v1.0.0\n",
			rules: []IdentityRule{{Source: "github.com/a/b", Target: "github.com/x/y"}},
			want:  "require github.com/a/b.cool v1.0.0\n",
		},
		{
			name:  "文本末尾无换行",
			data:  "module github.com/a/b",
			rules: []IdentityRule{{Source: "github.com/a/b", Target: "github.com/x/y"}},
			want:  "module github.com/x/y",
		},
		{
			name:  "source 不存在时不动",
			data:  "module github.com/other/repo\n",
			rules: []IdentityRule{{Source: "github.com/a/b", Target: "github.com/x/y"}},
			want:  "module github.com/other/repo\n",
		},
		{
			name: "多条 module 规则全部生效",
			data: "import \"github.com/a/b\"\nimport \"github.com/c/d\"\n",
			rules: []IdentityRule{
				{Source: "github.com/a/b", Target: "github.com/x/y"},
				{Source: "github.com/c/d", Target: "github.com/x/z"},
			},
			want: "import \"github.com/x/y\"\nimport \"github.com/x/z\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RewriteIdentities([]byte(tt.data), tt.rules)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestRewriteIdentitiesHost(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		rules []IdentityRule
		want  string
	}{
		{
			name:  "主机名整体改写",
			data:  "clone ssh://git@git.enjoye.top/enjoydream/x.git\n",
			rules: []IdentityRule{{Source: "git.enjoye.top", Target: "github.com", Kind: KindHost}},
			want:  "clone ssh://git@github.com/enjoydream/x.git\n",
		},
		{
			name:  "点属于主机名,半改写被拒绝",
			data:  "host git.enjoye.top.evil.com\n",
			rules: []IdentityRule{{Source: "git.enjoye.top", Target: "github.com", Kind: KindHost}},
			want:  "host git.enjoye.top.evil.com\n",
		},
		{
			name:  "主机名后紧跟字母不构成边界",
			data:  "host git.enjoye.topfoo\n",
			rules: []IdentityRule{{Source: "git.enjoye.top", Target: "github.com", Kind: KindHost}},
			want:  "host git.enjoye.topfoo\n",
		},
		{
			name:  "端口号构成边界",
			data:  "push git.enjoye.top:8080/img\n",
			rules: []IdentityRule{{Source: "git.enjoye.top", Target: "github.com", Kind: KindHost}},
			want:  "push github.com:8080/img\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RewriteIdentities([]byte(tt.data), tt.rules)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestRewriteIdentitiesLiteral(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		rules []IdentityRule
		want  string
	}{
		{
			name:  "独立词被改写",
			data:  "deploy to 215 environment\n",
			rules: []IdentityRule{{Source: "215", Target: "prod", Kind: KindLiteral}},
			want:  "deploy to prod environment\n",
		},
		{
			name:  "数字前缀不构成词边界",
			data:  "version 1215\n",
			rules: []IdentityRule{{Source: "215", Target: "prod", Kind: KindLiteral}},
			want:  "version 1215\n",
		},
		{
			name:  "数字后缀不构成词边界",
			data:  "build 2151\n",
			rules: []IdentityRule{{Source: "215", Target: "prod", Kind: KindLiteral}},
			want:  "build 2151\n",
		},
		{
			name:  "相邻两次出现都被改写",
			data:  "215 and 215\n",
			rules: []IdentityRule{{Source: "215", Target: "prod", Kind: KindLiteral}},
			want:  "prod and prod\n",
		},
		{
			name:  "紧邻分隔符的两次出现都被改写",
			data:  "215,215\n",
			rules: []IdentityRule{{Source: "215", Target: "prod", Kind: KindLiteral}},
			want:  "prod,prod\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RewriteIdentities([]byte(tt.data), tt.rules)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestRewriteIdentitiesStrip(t *testing.T) {
	// 剥离:删除源标识本身,而不是替换成占位符。
	got, err := RewriteIdentities(
		[]byte("image: harbor.enjoye.top/proj/img:1\n"),
		[]IdentityRule{{Source: "harbor.enjoye.top", Target: "", Kind: KindHost}},
	)
	require.NoError(t, err)
	assert.Equal(t, "image: /proj/img:1\n", string(got))
}

func TestRewriteIdentitiesRuleOrder(t *testing.T) {
	// 模块规则必须先于主机名规则执行:否则模块路径前缀被主机名规则先改掉,
	// 模块规则找不到源标识,静默失效。
	rules := []IdentityRule{
		{Source: "git.enjoye.top", Target: "github.com", Kind: KindHost},
		{Source: "git.enjoye.top/enjoydream/code-review-service", Target: "github.com/yi-nology/code-review-service", Kind: KindModule},
	}
	r, err := NewRewriter(rules)
	require.NoError(t, err)

	order := r.Rules()
	require.Len(t, order, 2)
	assert.Equal(t, KindModule, order[0].Kind, "module 规则必须排到最前")
	assert.Equal(t, KindHost, order[1].Kind)

	got := r.Rewrite([]byte("import \"git.enjoye.top/enjoydream/code-review-service/internal/x\"\n"))
	assert.Equal(t,
		"import \"github.com/yi-nology/code-review-service/internal/x\"\n",
		string(got),
		"模块路径必须被整体改写,而不是只改掉主机名前缀")
}

func TestNewRewriterRejectsInvalidRules(t *testing.T) {
	tests := []struct {
		name  string
		rules []IdentityRule
	}{
		{name: "Source 为空", rules: []IdentityRule{{Target: "x"}}},
		{name: "Source 与 Target 相同", rules: []IdentityRule{{Source: "a", Target: "a"}}},
		{name: "module 规则不支持剥离", rules: []IdentityRule{{Source: "a/b", Target: "", Kind: KindModule}}},
		{name: "Kind 非法", rules: []IdentityRule{{Source: "a", Target: "b", Kind: IdentityKind(99)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRewriter(tt.rules)
			assert.Error(t, err)
		})
	}
}

// TestRewriteIdentitiesMatchesLegacyRegex 确认新实现与旧正则实现的
// 边界语义一致(旧实现已被新原语取代,此用例以原始正则锁住行为不漂移)。
func TestRewriteIdentitiesMatchesLegacyRegex(t *testing.T) {
	legacy := func(data, source, target string) string {
		terminator := regexp.MustCompile(`[^a-zA-Z0-9_.\-]|$`)
		re := regexp.MustCompile(`(` + regexp.QuoteMeta(source) + `)(` + terminator.String() + `)`)
		return string(re.ReplaceAll([]byte(data), []byte(target+"$2")))
	}

	cases := []struct {
		data   string
		source string
		target string
	}{
		{"module github.com/a/b\n", "github.com/a/b", "github.com/x/y"},
		{"import \"github.com/a/b/pkg\"\n", "github.com/a/b", "github.com/x/y"},
		{"module github.com/a/b-extra\n", "github.com/a/b", "github.com/x/y"},
		{"require github.com/a/b.cool v1.0.0\n", "github.com/a/b", "github.com/x/y"},
		{"module github.com/a/b", "github.com/a/b", "github.com/x/y"},
		{"x github.com/a/b y github.com/a/b z", "github.com/a/b", "github.com/x/y"},
		{"github.com/a/b github.com/a/b-extra github.com/a/b", "github.com/a/b", "github.com/x/y"},
		{"a github.com/a/b-extra b github.com/a/b c", "github.com/a/b", "github.com/x/y"},
	}
	for _, c := range cases {
		got, err := RewriteIdentities([]byte(c.data), []IdentityRule{{
			Source: c.source, Target: c.target, Kind: KindModule,
		}})
		require.NoError(t, err)
		assert.Equal(t, legacy(c.data, c.source, c.target), string(got), "data=%q", c.data)
	}
}
