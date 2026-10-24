package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeLocalPaths 回归:门禁错误里的本机绝对路径要抹掉。
//
// 背景(第四轮实测):go list 的报错原文带 module cache 的绝对路径
// (`/Users/<用户名>/go/pkg/mod/cache/vcs/<hash>: exit status 128`)
// —— 那是本机目录布局 + 操作者用户名,对定位问题毫无用处,
// 却会一路流进模型上下文与日志。
//
// 同时:定位信息(file:line:col)必须原样保留,否则门禁错误就没用了。
func TestSanitizeLocalPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("取不到用户主目录")
	}

	msg := "main.go:3:8: module git.enjoye.top/enjoydream/app/pkg: " +
		"git ls-remote -q --end-of-options https://git.enjoye.top/enjoydream/app.git in " +
		filepath.Join(home, "go/pkg/mod/cache/vcs/2445b881") +
		": exit status 128:\n\tForgejo: Cannot find repository: enjoydream/app"

	out := sanitizeLocalPaths(msg)

	assert.NotContains(t, out, home, "本机主目录必须被脱敏")
	assert.NotContains(t, out, "go/pkg/mod/cache/vcs",
		"module cache 路径必须被脱敏")
	assert.Contains(t, out, "main.go:3:8",
		"定位信息必须原样保留")
	assert.Contains(t, out, "https://git.enjoye.top/enjoydream/app.git",
		"URL 不该被本机路径脱敏波及(那是内网标识,由上层按通道配置处理)")
}

// TestSanitizeLocalPathsKeepsRelativePos 相对位置不受影响。
func TestSanitizeLocalPathsKeepsRelativePos(t *testing.T) {
	msg := "internal/modules/server.go:103:67: undefined: readiness.Check"
	out := sanitizeLocalPaths(msg)
	assert.Equal(t, msg, out)
	assert.False(t, strings.Contains(out, "[本机路径已脱敏]"))
}
