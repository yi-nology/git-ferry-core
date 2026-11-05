package executor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/oekit/pkg/secretx"
)

const encSuffix = ".enc"

// EncryptFile 加密文件,写出 <name>.enc。
// keyParam: base64(16/24/32 字节)或任意口令(口令经 SHA-256 派生 32B)。
// 返回加密文件路径。
//
// 新密文一律 oekit secretx 信封格式("v1:" + base64(nonce‖ct),AES-GCM 带认证,
// 自带版本前缀——未来主密钥轮换时按前缀选钥重封的缝线已在);存量旧密文
// (裸 nonce‖ct 二进制,无前缀)在 Decrypt 侧继续兼容读取,零迁移。
func EncryptFile(srcPath, keyParam string) (string, error) {
	return EncryptFileWithSuffix(srcPath, keyParam, encSuffix)
}

// DecryptFile 解密 <name>.enc → <name 去掉 .enc>。
// v1: 前缀走 secretx 信封;无前缀的存量裸二进制(nonce‖ct,AES-256-GCM)走旧路径。
func DecryptFile(encPath, keyParam, destPath string) error {
	key, err := deriveKey(keyParam)
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(encPath) //nolint:gosec // 内部冷备路径
	if err != nil {
		return oekiterrors.Wrap(err, "read encrypted")
	}
	var plain []byte
	if strings.HasPrefix(string(sealed), secretxPrefixV1) {
		plain, err = openV1(key, sealed)
	} else {
		plain, err = openLegacy(sealed, key)
	}
	if err != nil {
		return err
	}
	if destPath == "" {
		destPath = strings.TrimSuffix(encPath, encSuffix)
	}
	if err := os.WriteFile(destPath, plain, 0o640); err != nil {
		return oekiterrors.Wrap(err, "write plain")
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

// EncryptFileWithSuffix 加密文件,指定输出后缀(默认 .enc)。
// 内容为 secretx v1 信封("v1:"+base64(nonce‖ct)),文本形态,一行可贴日志比对。
func EncryptFileWithSuffix(srcPath, keyParam, suffix string) (string, error) {
	if suffix == "" {
		suffix = encSuffix
	}
	key, err := deriveKey(keyParam)
	if err != nil {
		return "", err
	}
	plain, err := os.ReadFile(srcPath) //nolint:gosec // 内部冷备路径
	if err != nil {
		return "", oekiterrors.Wrap(err, "read plain")
	}
	box, err := secretx.NewBox(key)
	if err != nil {
		return "", oekiterrors.Wrap(err, "init cipher")
	}
	sealed, err := box.Seal(plain)
	if err != nil {
		return "", oekiterrors.Wrap(err, "seal")
	}
	dst := srcPath + suffix
	if err := os.WriteFile(dst, []byte(sealed), 0o640); err != nil {
		return "", oekiterrors.Wrap(err, "write encrypted")
	}
	return dst, nil
}

// secretxPrefixV1 secretx 信封的版本前缀。
const secretxPrefixV1 = "v1:"

// boxFor 用派生密钥构造 secretx Box。
func boxFor(key []byte) (*secretx.Box, error) {
	return secretx.NewBox(key)
}

// openV1 解 secretx v1 信封。
func openV1(key, sealed []byte) ([]byte, error) {
	box, err := boxFor(key)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init cipher")
	}
	plain, err := box.Open(string(sealed))
	if err != nil {
		return nil, oekiterrors.Wrap(err, "decrypt failed (wrong key or corrupted)")
	}
	return plain, nil
}

// openLegacy 兼容存量裸二进制密文(nonce‖ct,AES-256-GCM,无版本前缀)。
// 只读不写:新密文一律走 v1 信封。
func openLegacy(sealed, key []byte) ([]byte, error) {
	aead, err := legacyAEAD(key)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(sealed) < ns {
		return nil, oekiterrors.New("ciphertext too short")
	}
	plain, err := aead.Open(nil, sealed[:ns], sealed[ns:], nil)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "decrypt failed (wrong key or corrupted)")
	}
	return plain, nil
}

// legacyAEAD 构造存量格式的 AES-256-GCM AEAD。
func legacyAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, oekiterrors.Wrap(err, "init cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm, nil
}

// deriveKey base64(16/24/32B) 直接用;其余按口令 SHA-256 派生 32B。
// 口令形态属于存量兼容面(部署方配置里可能一直是口令),不收紧。
func deriveKey(keyParam string) ([]byte, error) {
	if keyParam == "" {
		return nil, oekiterrors.New("encrypt key is empty")
	}
	if raw, err := base64.StdEncoding.DecodeString(keyParam); err == nil && (len(raw) == 16 || len(raw) == 24 || len(raw) == 32) {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(keyParam); err == nil && (len(raw) == 16 || len(raw) == 24 || len(raw) == 32) {
		return raw, nil
	}
	sum := sha256.Sum256([]byte(keyParam))
	return sum[:], nil
}
