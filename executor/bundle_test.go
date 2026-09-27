package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotateBundles_KeepN(t *testing.T) {
	dir := t.TempDir()
	// 创建 5 份,时间戳递增
	for _, ts := range []string{"20260101-000000", "20260102-000000", "20260103-000000", "20260104-000000", "20260105-000000"} {
		name := filepath.Join(dir, "t1-main-"+ts+".bundle")
		require.NoError(t, os.WriteFile(name, []byte("x"), 0o600))
	}
	// 不相关文件
	require.NoError(t, os.WriteFile(filepath.Join(dir, "t2-main-20260101-000000.bundle"), []byte("y"), 0o600))

	removed := rotateBundles(dir, "t1", 2)
	assert.Equal(t, 3, removed)

	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	assert.ElementsMatch(t, []string{
		"t1-main-20260104-000000.bundle",
		"t1-main-20260105-000000.bundle",
		"t2-main-20260101-000000.bundle",
	}, left)
}

func TestRotateBundles_KeepZero(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "t1-a.bundle"), []byte("x"), 0o600))
	assert.Equal(t, 0, rotateBundles(dir, "t1", 0))
}
