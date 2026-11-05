package executor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealLegacy 用存量裸二进制格式(nonce‖ct,无版本前缀)封一段明文,
// 供 legacy 兼容测试构造旧密文。
func sealLegacy(t *testing.T, plain, key []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, gcm.NonceSize())
	_, err = io.ReadFull(rand.Reader, nonce)
	require.NoError(t, err)
	return gcm.Seal(nonce, nonce, plain, nil)
}

// key32 测试用 base64(32B) 密钥。
func key32() string {
	raw := sha256.Sum256([]byte("test-key"))
	return base64.StdEncoding.EncodeToString(raw[:])
}

// b64 便捷 base64 编码。
func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// 存量密文兼容契约:旧格式(裸 nonce‖ct 二进制,无版本前缀)必须能解——
// 升级前落库的冷备密文不迁移、不解密失败。
func TestDecryptLegacyFormat(t *testing.T) {
	dir := t.TempDir()
	key, err := deriveKey(key32())
	require.NoError(t, err)
	plain := []byte("legacy ciphertext written before v1 envelope")
	enc := filepath.Join(dir, "old.bundle.enc")
	require.NoError(t, os.WriteFile(enc, sealLegacy(t, plain, key), 0o600))

	dest := filepath.Join(dir, "restored.bin")
	require.NoError(t, DecryptFile(enc, key32(), dest))
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
}

// 新密文一律 secretx v1 信封:带版本前缀(未来主密钥轮换按前缀选钥的缝线)。
func TestEncryptProducesV1Envelope(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "bundle.tar")
	require.NoError(t, os.WriteFile(src, []byte("payload"), 0o600))

	enc, err := EncryptFile(src, key32())
	require.NoError(t, err)
	raw, err := os.ReadFile(enc)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(raw), "v1:"),
		"新密文应带 v1: 版本前缀(旧格式无前缀)")
}

func TestEncryptFileWithSuffix(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o600))
	enc, err := EncryptFileWithSuffix(src, key32(), ".bak.enc")
	require.NoError(t, err)
	assert.Equal(t, src+".bak.enc", enc)
}

func TestDecryptToTemp(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dump.tar.gz")
	require.NoError(t, os.WriteFile(src, []byte("archive"), 0o600))
	enc, err := EncryptFile(src, key32())
	require.NoError(t, err)

	plain, err := DecryptToTemp(enc, key32())
	require.NoError(t, err)
	defer os.RemoveAll(filepath.Dir(plain))
	assert.Equal(t, "dump.tar.gz", filepath.Base(plain))
	got, err := os.ReadFile(plain)
	require.NoError(t, err)
	assert.Equal(t, "archive", string(got))
}

// 口令派生与 16B 密钥档位(base64 32B 档已有 TestDeriveKeyBase64)。
func TestDeriveKeyPassphraseAndSizes(t *testing.T) {
	k, err := deriveKey("my-passphrase")
	require.NoError(t, err)
	assert.Len(t, k, 32)

	k, err = deriveKey(b64(make([]byte, 16)))
	require.NoError(t, err)
	assert.Len(t, k, 16)

	k, err = deriveKey(b64(make([]byte, 24)))
	require.NoError(t, err)
	assert.Len(t, k, 24)

	_, err = deriveKey("")
	assert.Error(t, err)
}
