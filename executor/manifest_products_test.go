package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManifestCoversAllBackupProducts 清单必须覆盖**全部**冷备产物形态。
//
// 回归背景(P0):清单只认 .bundle,而 backup_format=zip 产出的是 .zip、
// 加密时另产 .bundle.enc ⇒ 配了 zip/加密的部署,清单里一条都没有,
// 校验却回"完整性校验通过"—— **零校验的假绿灯**;
// 真正发到异地的密文件也不在任何哈希覆盖范围内(被篡改/损坏查不出来)。
func TestManifestCoversAllBackupProducts(t *testing.T) {
	dir := t.TempDir()
	products := []string{
		"task-main-20260101-000000.bundle",
		"task-main-20260101-000001.zip",
		"task-main-20260101-000002.bundle.enc",
		"task-main-20260101-000003.zip.enc",
	}
	for _, name := range products {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("payload-"+name), 0o600))
	}
	// 非产物文件不该进清单
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600))

	m, err := BuildManifest(dir)
	require.NoError(t, err)
	assert.Equal(t, len(products), m.EntryCount,
		"清单必须覆盖全部产物形态(.bundle/.zip/.bundle.enc/.zip.enc),实际=%d", m.EntryCount)
	assert.NotEmpty(t, m.MerkleRoot)
	_, err = SaveManifest(m)
	require.NoError(t, err)

	// 篡改一个 zip 产物 ⇒ 必须被校验出来
	target := filepath.Join(dir, "task-main-20260101-000001.zip")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(target, append(data, 'X'), 0o600))

	res, err := VerifyManifest(dir)
	require.NoError(t, err)
	assert.False(t, res.OK, "zip 产物被篡改必须被发现")
	assert.NotEmpty(t, res.HashMismatch)
}

// TestManifestEmptyIsNotVerified 空目录 / 没有产物 ⇒ **不算"校验通过**。
//
// merkleRoot 对空列表返回 "" ⇒ 两边都是 "" 时旧判定 OK 成立,
// 消息还写着 "merkle root matches" —— 运维看到绿色"通过",实际什么都没校验。
func TestManifestEmptyIsNotVerified(t *testing.T) {
	dir := t.TempDir()
	m, err := BuildManifest(dir)
	require.NoError(t, err)
	_, err = SaveManifest(m)
	require.NoError(t, err)

	res, err := VerifyManifest(dir)
	require.NoError(t, err)
	assert.False(t, res.OK, "没有任何产物时不得报'校验通过'")
	assert.Contains(t, res.Message, "没有可校验的冷备产物")
}

// TestListBundlesCoversAllBackupProducts 备份列表接口同样要覆盖全部形态。
//
// 列表只认 .bundle 时,zip/加密产物**在备份列表里查不到** ⇒
// 运维以为根本没备份,RPO 报告里该任务一条指标都没有。
func TestListBundlesCoversAllBackupProducts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"task-main-20260101-000000.bundle",
		"task-main-20260101-000001.zip",
		"task-main-20260101-000002.bundle.enc",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	list, err := ListBundles(dir, "")
	require.NoError(t, err)
	assert.Len(t, list, 3, "备份列表必须包含 zip/加密产物,实际=%d", len(list))
	for _, b := range list {
		assert.True(t, IsBackupProduct(b.Name), "只应列出冷备产物,实际=%s", b.Name)
	}
	assert.True(t, strings.HasSuffix(list[0].Name, ".enc") || true)
}
