package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestToTargetDTO_RedactsEmbeddedCredentials 响应里不得回显 URL 内嵌的凭据。
//
// 回归背景(P2):推送目标的地址可能内嵌凭据
// (https://user:token@host/、?access_token=…) ⇒ CRUD 响应原样回显 ⇒
// 凭据被抄进 CLI 输出与 MCP/模型上下文。预检路径已经用 RedactRepoURL 脱敏,
// CRUD 路径没有。
//
// 这条用例是**反向验证**:把 toTargetDTO 改回直接回显 t.RepoURL,本用例变红。
func TestToTargetDTO_RedactsEmbeddedCredentials(t *testing.T) {
	dto := toTargetDTO(&model.HistoryTarget{
		ID: 1, ChannelID: 2, Name: "publish",
		RepoURL: "https://user:ghp_secrettoken@github.com/org/repo.git",
	})
	assert.NotContains(t, dto.RepoURL, "ghp_secrettoken",
		"响应里不得出现 URL 内嵌的凭据")
	assert.Contains(t, dto.RepoURL, "github.com/org/repo.git",
		"host 与路径要保留(用户还要靠它识别推到哪个仓库)")

	// 查询串里的 token 也要脱敏
	dto2 := toTargetDTO(&model.HistoryTarget{
		ID: 1, ChannelID: 2, Name: "publish",
		RepoURL: "https://github.com/org/repo.git?access_token=abc123",
	})
	assert.NotContains(t, dto2.RepoURL, "abc123", "查询串里的 token 也要脱敏")
}

// TestUpdateHistoryTarget_KeepsCredentialWhenURLUnchanged 无关更新不得抹掉凭据。
//
// 回归背景(P2):响应里的地址是脱敏的 ⇒ 客户端把脱敏后的地址回传 ⇒
// 若无条件写回,一次无关的更新(如改描述)就把地址里的凭据抹掉了。
//
// 这条用例是**反向验证**:把"回传值等于脱敏值就保留原值"这条规则去掉,
// 本用例变红。
func TestUpdateHistoryTarget_KeepsCredentialWhenURLUnchanged(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "redact", TargetBranch: strPtr("main"), SourceBranch: strPtr("main"),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceDir:    strPtr(makeHistorySourceDir(t)),
		TargetDir:    strPtr(makeHistoryTargetDir(t)),
		Enabled:      boolPtr(true),
	})
	require.NoError(t, err)

	target, err := h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: "https://user:ghp_secrettoken@github.com/org/repo.git",
	})
	require.NoError(t, err)
	assert.NotContains(t, target.RepoURL, "ghp_secrettoken", "响应里不得回显凭据")

	// 客户端把脱敏后的地址原样回传(改个名字)⇒ 库里带凭据的地址必须保住
	updated, err := h.UpdateHistoryTarget(ctx, target.ID, HistoryTargetInput{
		Name:    "publish-2",
		RepoURL: target.RepoURL,
	})
	require.NoError(t, err)
	assert.Equal(t, "publish-2", updated.Name)

	stored, err := h.targets.FindByID(target.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Contains(t, stored.RepoURL, "ghp_secrettoken",
		"回传脱敏地址时不得把库里的凭据抹掉")

	// 真正换了地址 ⇒ 应当写入新地址
	updated2, err := h.UpdateHistoryTarget(ctx, target.ID, HistoryTargetInput{
		RepoURL: "https://github.com/other/repo.git",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/other/repo.git", updated2.RepoURL)
}

// TestCreateHistoryTarget_RejectsDuplicateName 同名目标必须被拒。
//
// 回归背景(P2):目标名在通道内没有唯一约束 ⇒ 推送时按名字定位凭据,
// 同名目标会让"推给谁"变得不确定(历史上出现过凭据串号)。
func TestCreateHistoryTarget_RejectsDuplicateName(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "dup", TargetBranch: strPtr("main"), SourceBranch: strPtr("main"),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		SourceDir:    strPtr(makeHistorySourceDir(t)),
		TargetDir:    strPtr(makeHistoryTargetDir(t)),
		Enabled:      boolPtr(true),
	})
	require.NoError(t, err)

	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: "https://github.com/org/a.git",
	})
	require.NoError(t, err)

	_, err = h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: "https://github.com/org/b.git",
	})
	require.Error(t, err, "同一通道内目标名必须唯一")
	assert.Contains(t, err.Error(), "唯一", "错误信息要说明是重名")
}

// makeHistoryTargetDir 造一个可用的目标目录。
func makeHistoryTargetDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tgt")
	makeHistoryRepo(t, dir, map[string]string{"go.mod": "module github.com/yi-nology/app\n"})
	return dir
}

// makeHistorySourceDir 造一个可用的源目录(通道要求 source_repo_key 或 source_dir 至少给一个)。
func makeHistorySourceDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "src")
	makeHistoryRepo(t, dir, map[string]string{"go.mod": "module git.enjoye.top/enjoydream/app\n"})
	return dir
}
