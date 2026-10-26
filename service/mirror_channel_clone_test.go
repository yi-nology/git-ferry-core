package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
)

// TestMirrorChannelCreate_PublishUsesOwnClone 每个 publish 通道必须克隆**自己的**源仓库。
//
// 回归背景(P0,第 2 个及以后的通道永远发不出去):
// 以前是"先 clone 再落库" ⇒ 建通道时 ch.ID 必为 0 ⇒ 工作目录键
// `mirror/channel-<ID>` 对**所有**通道都是 `mirror/channel-0/repo`
// ⇒ 第 2 个通道拿到的是**上一个通道的克隆** ⇒ 只对 origin(A) 做增量 fetch、
// 从不克隆 B ⇒ 源 module 身份记成 A 的 ⇒ 之后发布时身份校验必然不通过
// ⇒ **该通道永远发不出去**。
//
// 这条用例是**反向验证**:把创建顺序改回"先 clone 再落库",本用例会变红。
func TestMirrorChannelCreate_PublishUsesOwnClone(t *testing.T) {
	m, repoDAO, _ := setupMirrorServiceTest(t)
	ctx := context.Background()

	srcA := filepath.Join(t.TempDir(), "alpha")
	srcB := filepath.Join(t.TempDir(), "beta")
	makeHistoryRepo(t, srcA, map[string]string{"go.mod": "module example.com/alpha\n"})
	makeHistoryRepo(t, srcB, map[string]string{"go.mod": "module example.com/beta\n"})

	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "github/acme/alpha", Name: "alpha", Platform: "github",
		CloneURL: srcA, Status: model.RepoStatusActive,
	}))
	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "github/acme/beta", Name: "beta", Platform: "github",
		CloneURL: srcB, Status: model.RepoStatusActive,
	}))

	chA, err := m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "A", Mode: model.MirrorModePublish, RepoKey: "github/acme/alpha",
		Targets: []MirrorTargetInput{{
			Remote: "origin", RepoURL: "https://example.com/target-a",
			TargetModule: "github.com/acme/app-a",
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, chA)

	chB, err := m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "B", Mode: model.MirrorModePublish, RepoKey: "github/acme/beta",
		Targets: []MirrorTargetInput{{
			Remote: "origin", RepoURL: "https://example.com/target-b",
			TargetModule: "github.com/acme/app-b",
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, chB)

	assert.Equal(t, "example.com/alpha", chA.Module, "第一个通道的源 module")
	assert.Equal(t, "example.com/beta", chB.Module,
		"第二个通道必须读**自己的**源仓库(以前共用 channel-0 目录 ⇒ 记成上一个通道的 module)")
	assert.NotEqual(t, chA.ID, chB.ID, "两个通道必须是不同的记录")
}
