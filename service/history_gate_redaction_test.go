package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestGateErrorRedactsInternalIdentifiers 回归:门禁错误不许把内网标识带出去。
//
// 背景(第四轮实测):门禁错误原文里带着内网的 module path 与内网 git 地址
// (`https://git.enjoye.top/enjoydream/app.git`),而门禁错误是
// **agent 工具的返回内容** → 直接进模型上下文。
// 通道 DTO 又**不暴露** module_source / host_source ——
// 也就是说错误文本是内网标识外流的口子。
//
// 这里用「源仓里 import 了一个改写规则碰不到的兄弟模块路径」构造场景:
// `app-missing` 的连字符让 module 边界不成立,内容原样留着,
// 于是内网路径会真的出现在门禁错误里。
func TestGateErrorRedactsInternalIdentifiers(t *testing.T) {
	// 关掉代理:让 go 工具**立刻**报「模块查找已禁用」而不是去联网,
	// 测试不依赖网络、也不会卡住。
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	h := newHistoryTestService(t)
	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")

	makeHistoryRepo(t, srcDir, map[string]string{
		"go.mod":  "module git.enjoye.top/enjoydream/app\n",
		"main.go": "package app\n\nimport _ \"git.enjoye.top/enjoydream/app-missing/pkg\"\n",
	})
	makeHistoryRepo(t, tgtDir, map[string]string{
		"go.mod":  "module github.com/yi-nology/app\n",
		"keep.go": "package app\n",
	})

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "redact", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceBranch: strPtr("main"), TargetBranch: strPtr("main"),
		Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	pv, err := h.HistoryPreview(ctx, ch.ID)
	require.NoError(t, err)
	require.False(t, pv.Gate.OK, "这个夹具应当过不了类型门禁")
	require.NotEmpty(t, pv.Gate.TypeCheckErr, "失败原因必须留给人看")

	errText := pv.Gate.TypeCheckErr
	assert.NotContains(t, errText, "git.enjoye.top",
		"门禁错误里不许出现内网主机名:%s", errText)
	assert.NotContains(t, errText, "enjoydream/app",
		"门禁错误里不许出现内网组织名/仓库名:%s", errText)

	// **诊断价值必须保留**:定位信息(file:line)不能被一起抹掉。
	assert.Contains(t, errText, "main.go",
		"脱敏不能把定位信息一起抹掉:%s", errText)
}

// TestInternalIdentifiersCoversOrgPath 单元:脱敏标识的集合要覆盖
// 「module path 去掉主机段」的部分。
//
// 远端的报错原文里常见 "Cannot find repository: <org>/<repo>" ——
// 那半截不是完整的 module path,只脱敏完整路径会漏掉它。
func TestInternalIdentifiersCoversOrgPath(t *testing.T) {
	ch := &model.HistoryChannel{
		ModuleSource: "git.enjoye.top/enjoydream/app",
		HostSource:   "git.enjoye.top",
	}
	ids := internalIdentifiers(ch)
	require.NotEmpty(t, ids)

	text := "module git.enjoye.top/enjoydream/app: " +
		"Cannot find repository: enjoydream/app " +
		"host git.enjoye.top unreachable"
	out := redactIdentifiers(text, ids)
	assert.NotContains(t, out, "git.enjoye.top")
	assert.NotContains(t, out, "enjoydream/app")
	assert.Contains(t, out, "[内网标识已脱敏]")
}

// TestRedactIdentifiersNoop 空输入与空标识不得panic。
func TestRedactIdentifiersNoop(t *testing.T) {
	assert.Equal(t, "", redactIdentifiers("", nil))
	assert.Equal(t, "abc", redactIdentifiers("abc", nil))
	assert.Equal(t, "abc", redactIdentifiers("abc", []string{""}))
	assert.Equal(t, "a b", redactIdentifiers("a b", []string{"x"}))
}
