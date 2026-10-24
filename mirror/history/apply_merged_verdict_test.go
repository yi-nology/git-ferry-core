package history

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/mirror"
)

// TestApply_MergedVerdictGoesThroughIdentityRewrite merged 裁决的内容必须走
// **同一条发布形态**管线(身份改写 → gofmt → 痕迹清理)。
//
// 回归背景:merged 裁决(res.Content = 人工/AI 给的原文)此前**整体替换**了
// 已改写的源内容 ⇒ 身份改写与 gofmt 都被跳过。裁决内容里写一句
// 「详见 git.enjoye.top/enjoydream/app/internal」,内网 module path 就原样
// 进公开仓 —— 而门禁全绿(注释既不是 gofmt 违规也不是类型错误)。
func TestApply_MergedVerdictGoesThroughIdentityRewrite(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")

	// 目标侧:shared.go 与源侧**各有独有内容** ⇒ 判定为 hold
	commitSeq(t, tgtDir, [][]fileSpec{
		{{path: "go.mod", data: "module github.com/yi-nology/app\n"},
			{path: "shared.go", data: "package app\n\n// target only\n"}},
	})
	// 源侧:同名文件内容不同 + 内网 module path
	commitSeq(t, srcDir, [][]fileSpec{
		{{path: "go.mod", data: "module git.enjoye.top/enjoydream/app\n"},
			{path: "shared.go", data: "package app\n\n// source only\n"}},
	})

	opts := Options{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		Rules: []mirror.IdentityRule{
			{Source: "git.enjoye.top/enjoydream/app", Target: "github.com/yi-nology/app",
				Kind: mirror.KindModule},
		},
	}
	apply := ApplyOptions{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		TargetRef: "master",
		Rules:     opts.Rules,
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	// 找一个 hold 文件
	var holdPath string
	for _, f := range plan.Files {
		if f.Disposition == DispositionHold {
			holdPath = f.Path
			break
		}
	}
	require.NotEmpty(t, holdPath, "两侧各有独有内容 ⇒ 应当产生 hold 文件")

	// 给它一条 merged 裁决:内容里带内网 module path
	res := plan.Resolutions[holdPath]
	res.Verdict = ResolutionMergeClean
	res.Content = "package app\n\n// 裁决:请看 git.enjoye.top/enjoydream/app/internal\n"
	plan.Resolutions[holdPath] = res
	apply.Plan = plan

	out, err := Apply(context.Background(), apply)
	require.NoError(t, err)

	// 产物里不应再有内网 module path
	data, rerr := os.ReadFile(filepath.Join(out.RepoPath, holdPath))
	require.NoError(t, rerr, "产物里应当有 %s", holdPath)
	got := string(data)
	assert.NotContains(t, got, "git.enjoye.top/enjoydream/app",
		"merged 裁决内容里的内网 module path 必须被改写掉")
	assert.True(t, strings.Contains(got, "github.com/yi-nology/app"),
		"应当被改写成目标 module path,实际: %s", got)
}
