package mirror

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRewriteModulePathGitCloneURL 回归:克隆 URL(带 .git 后缀)必须整体改写。
//
// 背景(第四轮实测):`https://git.enjoye.top/enjoydream/app.git` 是最常见的
// 克隆 URL 形态,而 "." 被当成 module path 的延续字符 → 边界不成立 →
// **整条 URL 原样留在公开仓**。而且是「半改半留」:同一文件里
// 不带 .git 的那行被改写了,带 .git 的那行没有。
//
// ".git" 是 git 协议后缀,不是 module path 的一部分,必须特判。
func TestRewriteModulePathGitCloneURL(t *testing.T) {
	const source = "git.enjoye.top/enjoydream/app"
	const target = "github.com/yi-nology/app"

	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "https 克隆 URL",
			data: "clone: https://git.enjoye.top/enjoydream/app.git\n",
			want: "clone: https://github.com/yi-nology/app.git\n",
		},
		{
			name: "ssh 克隆 URL",
			data: "clone: ssh://git@git.enjoye.top/enjoydream/app.git\n",
			want: "clone: ssh://git@github.com/yi-nology/app.git\n",
		},
		{
			name: "scp 风格克隆地址",
			data: "clone: git@git.enjoye.top:enjoydream/app.git\n",
			want: "clone: git@github.com:yi-nology/app.git\n",
		}, {
			name: "裸 .git 后缀(文本末尾)",
			data: "repo=git.enjoye.top/enjoydream/app.git",
			want: "repo=github.com/yi-nology/app.git",
		},
		{
			name: ".git 后面跟分隔符",
			data: "git.enjoye.top/enjoydream/app.git,next\n",
			want: "github.com/yi-nology/app.git,next\n",
		},
		{
			name: ".git 后面跟斜杠",
			data: "git.enjoye.top/enjoydream/app.git/sub\n",
			want: "github.com/yi-nology/app.git/sub\n",
		},
		{
			name: "不是 .git 后缀(.github.com)不改写",
			data: "git.enjoye.top/enjoydream/app.github.com\n",
			want: "git.enjoye.top/enjoydream/app.github.com\n",
		},
		{
			name: "兄弟模块(b-extra)仍不改写",
			data: "module git.enjoye.top/enjoydream/app-extra\n",
			want: "module git.enjoye.top/enjoydream/app-extra\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RewriteIdentities([]byte(tt.data), []IdentityRule{{
				Source: source, Target: target, Kind: KindModule,
			}})
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

// TestRewriterPending 回归:残留检测的判据。
//
// 「本该被改写却没改写」才算残留 ——
//   - 改写器能改的,改写后不该再有 pending;
//   - 共享前缀但不是同一个标识的(app-extra)不算残留;
//   - 克隆 URL 形态在改写后不该再有 pending(否则会误报残留)。
func TestRewriterPending(t *testing.T) {
	rw, err := NewRewriter([]IdentityRule{{
		Source: "git.enjoye.top/enjoydream/app", Target: "github.com/yi-nology/app", Kind: KindModule,
	}})
	require.NoError(t, err)

	assert.False(t, rw.Pending([]byte("module github.com/yi-nology/app\n")),
		"改写后的内容不该再有残留")
	assert.False(t, rw.Pending([]byte("https://github.com/yi-nology/app.git\n")),
		"克隆 URL 改写后不该再有残留")
	assert.False(t, rw.Pending([]byte("module git.enjoye.top/enjoydream/app-extra\n")),
		"共享前缀但不是同一个标识,不算残留")
	assert.True(t, rw.Pending([]byte("clone: https://git.enjoye.top/enjoydream/app.git\n")),
		"未改写的克隆 URL 是残留")
	assert.True(t, rw.Pending([]byte("module git.enjoye.top/enjoydream/app\n")),
		"未改写的 module 声明是残留")
}

// TestRewriteIdentitiesLiteralHyphen 锁定 KindLiteral 的连字符口径。
//
// 口径:**"-" 是词分隔符,不是词字符**(与 "_" 不同 —— "_" 是标识符字符,
// 属于同一个词)。于是:
//
//   - `PROD-1` = 词 "PROD" + 分隔符 + 词 "1" → 改写 "PROD";
//   - `PROD_1` = 一个词 → 整体不改写。
//
// 选这个口径的理由:公开化场景下**漏改比误改更糟** ——
// `PROD_1` 这种整体是一个标识符的情形,原样留着反而是另一个问题;
// 而 `PROD-1` 里嵌着内网环境代号,不改写就等于把内网标识发出去。
//
// 这里把口径钉死:将来若要改口径,先想清楚「漏改」与「误伤」哪个更要命。
func TestRewriteIdentitiesLiteralHyphen(t *testing.T) {
	rules := []IdentityRule{{Source: "PROD", Target: "PUBLIC", Kind: KindLiteral}}

	got, err := RewriteIdentities([]byte("env PROD-1 and PROD and PROD_1\n"), rules)
	require.NoError(t, err)
	assert.Equal(t, "env PUBLIC-1 and PUBLIC and PROD_1\n", string(got))
}
