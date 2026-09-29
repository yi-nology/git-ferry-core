package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
)

// ManifestEntry 单个冷备文件的完整性条目。
type ManifestEntry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	ModTime  time.Time `json:"mod_time"`
	TaskKey  string    `json:"task_key,omitempty"`
	LeafHash string    `json:"leaf_hash"`
}

// BackupManifest 冷备目录的完整性清单(Merkle 根可证明「目录未被静默篡改」)。
type BackupManifest struct {
	Version    int             `json:"version"`
	Generated  time.Time       `json:"generated"`
	BackupDir  string          `json:"backup_dir"`
	EntryCount int             `json:"entry_count"`
	TotalSize  int64           `json:"total_size"`
	MerkleRoot string          `json:"merkle_root"`
	Entries    []ManifestEntry `json:"entries"`
}

const manifestFileName = "backup-manifest.json"

// BuildManifest 扫描 backupDir 下所有 .bundle,计算 SHA256 与 Merkle 根。
func BuildManifest(backupDir string) (*BackupManifest, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &BackupManifest{
				Version:   1,
				Generated: time.Now().UTC(),
				BackupDir: backupDir,
				Entries:   []ManifestEntry{},
			}, nil
		}
		return nil, err
	}
	m := &BackupManifest{
		Version:   1,
		Generated: time.Now().UTC(),
		BackupDir: backupDir,
		Entries:   []ManifestEntry{},
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bundle") {
			continue
		}
		path := filepath.Join(backupDir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return nil, errors.Wrapf(err, "hash %s", e.Name())
		}
		leaf := leafHash(e.Name(), fi.Size(), sum)
		m.Entries = append(m.Entries, ManifestEntry{
			Name:     e.Name(),
			Size:     fi.Size(),
			SHA256:   sum,
			ModTime:  fi.ModTime().UTC(),
			LeafHash: leaf,
		})
		m.TotalSize += fi.Size()
	}
	// 名称排序保证 Merkle 根确定性
	sort.Slice(m.Entries, func(i, j int) bool {
		return m.Entries[i].Name < m.Entries[j].Name
	})
	for i := range m.Entries {
		m.Entries[i].TaskKey = inferTaskKey(m.Entries[i].Name)
	}
	m.EntryCount = len(m.Entries)
	m.MerkleRoot = merkleRoot(m.Entries)
	return m, nil
}

// SaveManifest 写入 backupDir/backup-manifest.json。
func SaveManifest(m *BackupManifest) (string, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(m.BackupDir, manifestFileName)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return "", err
	}
	return path, nil
}

// LoadManifest 读取已保存的清单。
func LoadManifest(backupDir string) (*BackupManifest, error) {
	path := filepath.Join(backupDir, manifestFileName)
	data, err := os.ReadFile(path) //nolint:gosec // 内部路径
	if err != nil {
		return nil, err
	}
	var m BackupManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ManifestVerifyResult 清单校验结果。
type ManifestVerifyResult struct {
	OK           bool     `json:"ok"`
	MerkleRoot   string   `json:"merkle_root"`
	StoredRoot   string   `json:"stored_root,omitempty"`
	CheckedCount int      `json:"checked_count"`
	MissingFiles []string `json:"missing_files,omitempty"`
	HashMismatch []string `json:"hash_mismatch,omitempty"`
	ExtraFiles   []string `json:"extra_files,omitempty"`
	Message      string   `json:"message"`
}

// VerifyManifest 现场重算并与存储清单比对。
// 既能发现文件被删/被改,也能发现目录被塞入多余 bundle。
func VerifyManifest(backupDir string) (*ManifestVerifyResult, error) {
	stored, err := LoadManifest(backupDir)
	if err != nil {
		return nil, errors.Wrap(err, "load manifest")
	}
	fresh, err := BuildManifest(backupDir)
	if err != nil {
		return nil, errors.Wrap(err, "rebuild manifest")
	}
	res := &ManifestVerifyResult{
		MerkleRoot: fresh.MerkleRoot,
		StoredRoot: stored.MerkleRoot,
	}
	storedMap := map[string]ManifestEntry{}
	for _, e := range stored.Entries {
		storedMap[e.Name] = e
	}
	freshMap := map[string]ManifestEntry{}
	for _, e := range fresh.Entries {
		freshMap[e.Name] = e
	}
	for name, se := range storedMap {
		fe, ok := freshMap[name]
		if !ok {
			res.MissingFiles = append(res.MissingFiles, name)
			continue
		}
		res.CheckedCount++
		if fe.SHA256 != se.SHA256 {
			res.HashMismatch = append(res.HashMismatch, name)
		}
	}
	for name := range freshMap {
		if _, ok := storedMap[name]; !ok {
			res.ExtraFiles = append(res.ExtraFiles, name)
		}
	}
	res.OK = len(res.MissingFiles) == 0 && len(res.HashMismatch) == 0 &&
		len(res.ExtraFiles) == 0 && fresh.MerkleRoot == stored.MerkleRoot
	if res.OK {
		res.Message = "backup integrity verified: merkle root matches"
	} else {
		parts := []string{}
		if len(res.MissingFiles) > 0 {
			parts = append(parts, "missing")
		}
		if len(res.HashMismatch) > 0 {
			parts = append(parts, "hash mismatch")
		}
		if len(res.ExtraFiles) > 0 {
			parts = append(parts, "extra files")
		}
		if fresh.MerkleRoot != stored.MerkleRoot {
			parts = append(parts, "merkle root drift")
		}
		res.Message = "integrity check failed: " + strings.Join(parts, ", ")
	}
	return res, nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // 内部路径
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func leafHash(name string, size int64, contentSHA string) string {
	h := sha256.Sum256([]byte(name + "|" + strutil.Itoa64(size) + "|" + contentSHA))
	return hex.EncodeToString(h[:])
}

// merkleRoot 自底向上两两哈希;奇数个时最后一个提升。
func merkleRoot(entries []ManifestEntry) string {
	if len(entries) == 0 {
		return ""
	}
	level := make([]string, len(entries))
	for i, e := range entries {
		level[i] = e.LeafHash
	}
	for len(level) > 1 {
		var next []string
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				h := sha256.Sum256([]byte(level[i] + level[i+1]))
				next = append(next, hex.EncodeToString(h[:]))
			} else {
				next = append(next, level[i])
			}
		}
		level = next
	}
	return level[0]
}

func inferTaskKey(bundleName string) string {
	// 命名: <taskKey>-<branch>-<ts>.bundle
	base := strings.TrimSuffix(bundleName, ".bundle")
	parts := strings.Split(base, "-")
	if len(parts) >= 3 {
		return parts[0]
	}
	return ""
}
