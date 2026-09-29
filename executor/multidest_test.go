package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bundle")
	require.NoError(t, os.WriteFile(src, []byte("hello cold backup"), 0o640))

	key := "test-passphrase"
	encPath, err := EncryptFile(src, key)
	require.NoError(t, err)
	assert.True(t, IsEncryptedFile(encPath))

	encData, _ := os.ReadFile(encPath)
	assert.NotContains(t, string(encData), "hello cold backup")

	dest := filepath.Join(dir, "a.out")
	require.NoError(t, DecryptFile(encPath, key, dest))
	plain, _ := os.ReadFile(dest)
	assert.Equal(t, "hello cold backup", string(plain))
}

func TestDecryptWrongKey(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bundle")
	require.NoError(t, os.WriteFile(src, []byte("secret"), 0o640))
	encPath, err := EncryptFile(src, "key-a")
	require.NoError(t, err)
	err = DecryptFile(encPath, "key-b", filepath.Join(dir, "out"))
	assert.Error(t, err)
}

func TestDeriveKeyBase64(t *testing.T) {
	// 32 字节 base64
	k, err := deriveKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	require.NoError(t, err)
	assert.Len(t, k, 32)
}

func TestFanoutUpload_LocalDest(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.bundle")
	require.NoError(t, os.WriteFile(src, []byte("payload"), 0o640))

	destDir := filepath.Join(dir, "remote")
	require.NoError(t, os.MkdirAll(destDir, 0o750))

	results := FanoutUpload(context.Background(), src, []Destination{{
		Type: DestLocal,
		Name: "nas",
		URL:  destDir,
	}})
	require.Len(t, results, 1)
	assert.True(t, results[0].OK, "err=%s", results[0].Error)
	data, err := os.ReadFile(filepath.Join(destDir, "x.bundle"))
	require.NoError(t, err)
	assert.Equal(t, "payload", string(data))
}

func TestFanoutUpload_DisabledSkipped(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.bundle")
	require.NoError(t, os.WriteFile(src, []byte("p"), 0o640))
	f := false
	results := FanoutUpload(context.Background(), src, []Destination{{
		Type:    DestLocal,
		Name:    "off",
		URL:     dir,
		Enabled: &f,
	}})
	assert.Len(t, results, 0)
}

func TestFanoutUpload_InvalidDest(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.bundle")
	require.NoError(t, os.WriteFile(src, []byte("p"), 0o640))
	results := FanoutUpload(context.Background(), src, []Destination{{
		Type: DestS3,
		Name: "bad",
	}})
	require.Len(t, results, 1)
	assert.False(t, results[0].OK)
}

func TestRotateRespectsKeep(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		name := "task-main-2026010" + string(rune('1'+i)) + "-000000.bundle"
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o640))
	}
	removed := rotateBundles(dir, "task", 2)
	assert.Equal(t, 3, removed)
	left, _ := ListBundles(dir, "task")
	assert.Len(t, left, 2)
}
