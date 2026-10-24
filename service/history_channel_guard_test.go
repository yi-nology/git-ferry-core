package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHistoryChannelRejectsUnsafeLocalDir 回归:本地路径直连必须指向一个
// 真实的 git 仓,且不能落在系统敏感位置。
//
// 背景:source_dir/target_dir 原本是任意本地路径 —— 指到 /etc、~/.ssh
// 这类地方,配合可配置的推送目标,一次执行就能把本机内容发到远端。
func TestHistoryChannelRejectsUnsafeLocalDir(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	base := HistoryChannelInput{
		Name:         "unsafe",
		TargetDir:    strPtr(historyRepoDir(t, "tgt")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"),
		Enabled: boolPtr(true),
	}

	// 1) 不是 git 仓的目录 → 拒绝。
	plain := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	in := base
	in.Name = "plain"
	in.SourceDir = strPtr(plain)
	_, err := h.CreateHistoryChannel(ctx, in)
	require.Error(t, err, "非 git 仓目录必须被拒绝")
	require.Contains(t, err.Error(), "git")

	// 2) 系统敏感目录 → 拒绝。
	if _, statErr := os.Stat("/etc"); statErr == nil {
		in = base
		in.Name = "etc"
		in.SourceDir = strPtr("/etc")
		_, err = h.CreateHistoryChannel(ctx, in)
		require.Error(t, err, "/etc 必须被拒绝")
	}

	// 3) 家目录下的凭据目录 → 拒绝。
	home, homeErr := os.UserHomeDir()
	if homeErr == nil && home != "" {
		sshDir := filepath.Join(home, ".ssh")
		if _, statErr := os.Stat(sshDir); statErr == nil {
			in = base
			in.Name = "ssh"
			in.SourceDir = strPtr(sshDir)
			_, err = h.CreateHistoryChannel(ctx, in)
			require.Error(t, err, "~/.ssh 必须被拒绝")
		}
	}

	// 4) 相对路径 → 拒绝(基准不确定)。
	in = base
	in.Name = "rel"
	in.SourceDir = strPtr("relative/path")
	_, err = h.CreateHistoryChannel(ctx, in)
	require.Error(t, err, "相对路径必须被拒绝")
}

// TestHistoryChannelPartialUpdateKeepsIdentityMapping 回归:部分更新
// 不能抹掉身份映射。
//
// 身份映射被抹掉 → 执行时没有任何改写规则 → 内网 module path
// **原样**进公开仓。这不是理论风险:只传目录的更新请求就能造成。
func TestHistoryChannelPartialUpdateKeepsIdentityMapping(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	srcDir := historyRepoDir(t, "src")
	tgtDir := historyRepoDir(t, "tgt")
	newTgt := historyRepoDir(t, "tgt2")

	dto, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "partial", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		HostSource:   strPtr("git.enjoye.top"), HostTarget: strPtr("github.com"),
		TargetBranch: strPtr("main"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	// 只改目标目录,其它什么都不传。
	_, err = h.UpdateHistoryChannel(ctx, dto.ID, HistoryChannelInput{
		TargetDir: strPtr(newTgt),
	})
	require.NoError(t, err)

	ch, err := h.channels.FindByID(dto.ID)
	require.NoError(t, err)
	require.Equal(t, "git.enjoye.top/enjoydream/app", ch.ModuleSource,
		"部分更新不能抹掉 module_source")
	require.Equal(t, "github.com/yi-nology/app", ch.ModuleTarget,
		"部分更新不能抹掉 module_target")
	require.Equal(t, "git.enjoye.top", ch.HostSource, "部分更新不能抹掉 host_source")
	require.Equal(t, "main", ch.TargetBranch, "部分更新不能抹掉 target_branch")
	require.Equal(t, srcDir, ch.SourceDir, "未给的字段保持原值")
	require.Equal(t, newTgt, ch.TargetDir, "给了的字段要更新")

	// 显式清空是允许的(指针语义:给了空串就是清空)。
	_, err = h.UpdateHistoryChannel(ctx, dto.ID, HistoryChannelInput{
		HostSource: strPtr(""),
	})
	require.NoError(t, err)
	ch, err = h.channels.FindByID(dto.ID)
	require.NoError(t, err)
	require.Empty(t, ch.HostSource, "显式给空串应当清空")

	// 但身份映射不许被清空 —— 清空了就没有任何改写规则了。
	_, err = h.UpdateHistoryChannel(ctx, dto.ID, HistoryChannelInput{
		ModuleSource: strPtr(""),
	})
	require.Error(t, err, "身份映射不允许被清空")
}

// TestHistoryChannelExecWithoutIdentityMappingRefused 回归:执行期必须有
// 身份映射。没有映射就没有改写规则,内网 module path 会原样进公开仓 ——
// 这种执行必须被拒绝,而不是发一棵树出去。
func TestHistoryChannelExecWithoutIdentityMappingRefused(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	srcDir := historyRepoDir(t, "src")
	tgtDir := historyRepoDir(t, "tgt")

	dto, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "nomap", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"),
		TargetBranch: strPtr("main"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	// 绕过服务层直接把映射抹掉(模拟历史数据/直改库)。
	ch, err := h.channels.FindByID(dto.ID)
	require.NoError(t, err)
	ch.ModuleSource = ""
	ch.ModuleTarget = ""
	require.NoError(t, h.channels.Update(ch))

	_, err = h.HistoryApply(ctx, dto.ID)
	require.Error(t, err, "没有身份映射的执行必须被拒绝")
	require.Contains(t, err.Error(), "身份映射")
}

// TestCheckSensitivePath 单元测敏感位置判定。
func TestCheckSensitivePath(t *testing.T) {
	// 系统敏感目录。
	for _, p := range []string{"/etc", "/etc/ssh", "/root", "/proc/self", "/var/lib/x"} {
		require.Error(t, checkSensitivePath(p, "source_dir"), "%s 应判为敏感", p)
	}
	// 家目录下的凭据目录。
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		for _, name := range []string{".ssh", ".aws", ".gnupg", ".kube"} {
			require.Error(t, checkSensitivePath(filepath.Join(home, name), "source_dir"),
				"%s 应判为敏感", name)
		}
		// 家目录本身也不行。
		require.Error(t, checkSensitivePath(home, "source_dir"), "家目录本身应判为敏感")
		// 普通子目录可以。
		require.NoError(t, checkSensitivePath(filepath.Join(home, "projects", "app"), "source_dir"))
	}
	// 普通位置可以。
	require.NoError(t, checkSensitivePath("/tmp/git-sync/history/channel-1/source", "source_dir"))
}
