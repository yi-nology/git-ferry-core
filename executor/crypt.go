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

	errors "github.com/cockroachdb/errors"
)

const encSuffix = ".enc"

// EncryptFile 用 AES-256-GCM 加密文件,写出 <name>.enc。
// keyParam: base64(32 字节)或任意口令(SHA-256 派生)。
// 返回加密文件路径。
func EncryptFile(srcPath, keyParam string) (string, error) {
	key, err := deriveKey(keyParam)
	if err != nil {
		return "", err
	}
	plain, err := os.ReadFile(srcPath) //nolint:gosec // 内部冷备路径
	if err != nil {
		return "", errors.Wrap(err, "read plain")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	dst := srcPath + encSuffix
	if err := os.WriteFile(dst, sealed, 0o640); err != nil {
		return "", errors.Wrap(err, "write encrypted")
	}
	return dst, nil
}

// DecryptFile 解密 <name>.enc → <name 去掉 .enc>。
func DecryptFile(encPath, keyParam, destPath string) error {
	key, err := deriveKey(keyParam)
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(encPath) //nolint:gosec // 内部冷备路径
	if err != nil {
		return errors.Wrap(err, "read encrypted")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	ns := gcm.NonceSize()
	if len(sealed) < ns {
		return errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return errors.Wrap(err, "decrypt failed (wrong key or corrupted)")
	}
	if destPath == "" {
		destPath = strings.TrimSuffix(encPath, encSuffix)
	}
	if err := os.WriteFile(destPath, plain, 0o640); err != nil {
		return errors.Wrap(err, "write plain")
	}
	return nil
}

// IsEncryptedFile 判断是否 AES 加密冷备文件。
func IsEncryptedFile(name string) bool {
	return strings.HasSuffix(name, encSuffix)
}

// DecryptToTemp 解密到临时目录,返回明文路径(调用方负责清理)。
func DecryptToTemp(encPath, keyParam string) (string, error) {
	base := strings.TrimSuffix(filepath.Base(encPath), encSuffix)
	tmp, err := os.MkdirTemp("", "git-ferry-decrypt-*")
	if err != nil {
		return "", err
	}
	dest := filepath.Join(tmp, base)
	if err := DecryptFile(encPath, keyParam, dest); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return dest, nil
}

func deriveKey(keyParam string) ([]byte, error) {
	if keyParam == "" {
		return nil, errors.New("encrypt key is empty")
	}
	// 优先按 base64 的 32 字节密钥
	if raw, err := base64.StdEncoding.DecodeString(keyParam); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(keyParam); err == nil && len(raw) == 32 {
		return raw, nil
	}
	// 口令模式:SHA-256 派生
	sum := sha256.Sum256([]byte(keyParam))
	return sum[:], nil
}

// EncryptFileWithSuffix 便于测试:指定输出后缀。
func EncryptFileWithSuffix(srcPath, keyParam, suffix string) (string, error) {
	if suffix == "" {
		suffix = encSuffix
	}
	key, err := deriveKey(keyParam)
	if err != nil {
		return "", err
	}
	plain, err := os.ReadFile(srcPath) //nolint:gosec // 内部
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	dst := srcPath + suffix
	if err := os.WriteFile(dst, gcm.Seal(nonce, nonce, plain, nil), 0o640); err != nil {
		return "", err
	}
	return dst, nil
}
