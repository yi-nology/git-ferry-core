package history

import (
	"context"
	"go/format"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// commitFileToRepo 在已有仓库里写文件并提交(多提交夹具用)。
func commitFileToRepo(t *testing.T, repo *git.Repository, dir, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644))
	w, err := repo.Worktree()
	require.NoError(t, err)
	_, err = w.Add(path)
	require.NoError(t, err)
	_, err = w.Commit("fixture", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
	require.NoError(t, err)
}

// TestResolveTakeTheirsIsNormalized 锁住:hold 判「取源侧」时,
// 写入目标树的内容必须**已经是 gofmt 归一化后的形态**。
//
// 缺陷背景:身份改写会改变 import 的字典序位置,改写后的源内容是「未格式化」的。
// 计划阶段有归一化(normalizeSource),解法阶段原先没有 ——
// 于是「取源侧」的内容被原样写进目标树,目标树 import 排序错乱,gofmt 门禁红。
//
// 归一化还必须用在**共同基点检测**上:目标树里存的是归一化后的内容,
// 源侧历史版本也必须按同一口径算 hash,否则匹配不上,
// 本该判「取源侧」的文件会退化成「找不到基点」。
func TestResolveTakeTheirsIsNormalized(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	tgtDir := filepath.Join(t.TempDir(), "tgt")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.MkdirAll(tgtDir, 0o755))

	// v1/v2 是内网形态:改写后 import 顺序会错(git.enjoye.top* 排在 github.com* 前)。
	const v1 = "package app\n\nimport (\n\t\"git.enjoye.top/enjoydream/app/internal/x\"\n\t\"github.com/oekit/logx\"\n)\n\nfunc main() {}\n"
	const v2 = "package app\n\nimport (\n\t\"git.enjoye.top/enjoydream/app/internal/x\"\n\t\"github.com/oekit/logx\"\n)\n\nfunc main() { _ = 1 }\n"

	srcRepo, err := git.PlainInit(srcDir, false)
	require.NoError(t, err)
	commitFileToRepo(t, srcRepo, srcDir, "main.go", v1)
	commitFileToRepo(t, srcRepo, srcDir, "main.go", v2)

	// 目标侧当前内容 = 源侧 v1 改写后的形态(且已是 gofmt 干净形态)。
	const w1 = "package app\n\nimport (\n\t\"github.com/oekit/logx\"\n\t\"github.com/yi-nology/app/internal/x\"\n)\n\nfunc main() {}\n"
	tgtRepo, err := git.PlainInit(tgtDir, false)
	require.NoError(t, err)
	commitFileToRepo(t, tgtRepo, tgtDir, "main.go", w1)

	opts := Options{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		Rules: []mirror.IdentityRule{{
			Source: "git.enjoye.top/enjoydream/app",
			Target: "github.com/yi-nology/app",
			Kind:   mirror.KindModule,
		}},
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	res, ok := plan.Resolutions["main.go"]
	require.True(t, ok, "main.go 必须有解法")
	assert.Equal(t, ResolutionTakeTheirs, res.Verdict, res.Reason)

	// 内容必须已是 gofmt 形态:再跑一次 format 不应产生任何改动。
	formatted, err := format.Source([]byte(res.Content))
	require.NoError(t, err)
	assert.Equal(t, res.Content, string(formatted),
		"取源侧的内容必须是 gofmt 归一化后的形态(否则写进目标树会触发 gofmt 门禁)")
}

// TestResolveMergeCleanIsNormalized 锁住:三向合并「无冲突」的结果同样要归一化。
//
// diff3 按 hunk 拼装内容,两侧都干净 ≠ 拼完干净 —— import 块被拆到两个 hunk
// 里拼回去时,字典序可能不再成立。
func TestResolveMergeCleanIsNormalized(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "src")
	tgtDir := filepath.Join(t.TempDir(), "tgt")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.MkdirAll(tgtDir, 0o755))

	// 基点:两边共有的历史版本。**目标侧用目标形态**(目标树里存的是目标形态),
	// 且是 gofmt 干净形态 —— 归一化后的源侧版本必须与它逐字节相同才算命中基点。
	const base = "package app\n\nimport (\n\t\"git.enjoye.top/enjoydream/app/internal/x\"\n\t\"github.com/oekit/logx\"\n)\n\n// marker one\nfunc a() {}\n\n// marker two\nfunc b() {}\n"
	// 源侧前进:改中间的一行。
	const vSrc = "package app\n\nimport (\n\t\"git.enjoye.top/enjoydream/app/internal/x\"\n\t\"github.com/oekit/logx\"\n)\n\n// marker one changed\nfunc a() {}\n\n// marker two\nfunc b() {}\n"
	// 目标侧前进:在末尾追加 —— 与源侧改动**不在同一处**,应能自动合并。
	const tBase = "package app\n\nimport (\n\t\"github.com/oekit/logx\"\n\t\"github.com/yi-nology/app/internal/x\"\n)\n\n// marker one\nfunc a() {}\n\n// marker two\nfunc b() {}\n"
	const tNow = "package app\n\nimport (\n\t\"github.com/oekit/logx\"\n\t\"github.com/yi-nology/app/internal/x\"\n)\n\n// marker one\nfunc a() {}\n\n// marker two\nfunc b() {}\n\n// marker three\nfunc c() {}\n"

	srcRepo, err := git.PlainInit(srcDir, false)
	require.NoError(t, err)
	commitFileToRepo(t, srcRepo, srcDir, "main.go", base)
	commitFileToRepo(t, srcRepo, srcDir, "main.go", vSrc)

	tgtRepo, err := git.PlainInit(tgtDir, false)
	require.NoError(t, err)
	commitFileToRepo(t, tgtRepo, tgtDir, "main.go", tBase)
	commitFileToRepo(t, tgtRepo, tgtDir, "main.go", tNow)

	opts := Options{
		SourceDir: srcDir,
		TargetDir: tgtDir,
		Rules: []mirror.IdentityRule{{
			Source: "git.enjoye.top/enjoydream/app",
			Target: "github.com/yi-nology/app",
			Kind:   mirror.KindModule,
		}},
	}

	plan, err := BuildPlan(context.Background(), opts)
	require.NoError(t, err)
	require.NoError(t, Resolve(context.Background(), opts, plan))

	res, ok := plan.Resolutions["main.go"]
	require.True(t, ok, "main.go 必须有解法")
	assert.Equal(t, ResolutionMergeClean, res.Verdict, res.Reason)

	formatted, err := format.Source([]byte(res.Content))
	require.NoError(t, err)
	assert.Equal(t, res.Content, string(formatted),
		"合并结果也必须是 gofmt 归一化后的形态")
}
