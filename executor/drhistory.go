package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// DrillHistoryEntry 演练历史落盘条目。
type DrillHistoryEntry struct {
	Report   *DrillReport `json:"report"`
	PrevHash string       `json:"prev_hash"`
	Hash     string       `json:"hash"`
}

// SaveDrillReport 将演练报告追加到 historyFile(JSONL,哈希链防篡改)。
func SaveDrillReport(historyFile string, rep *DrillReport) (*DrillHistoryEntry, error) {
	prev := lastChainHash(historyFile)
	entry := DrillHistoryEntry{Report: rep, PrevHash: prev}
	entry.Hash = chainHash(prev, rep)
	f, err := os.OpenFile(historyFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	enc, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return nil, err
	}
	return &entry, nil
}

// loadDrillHistoryInOrder 按写入顺序读取(供哈希链校验)。
func loadDrillHistoryInOrder(historyFile string) ([]DrillHistoryEntry, error) {
	data, err := os.ReadFile(historyFile) //nolint:gosec // 内部路径
	if err != nil {
		if os.IsNotExist(err) {
			return []DrillHistoryEntry{}, nil
		}
		return nil, err
	}
	var out []DrillHistoryEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e DrillHistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// LoadDrillHistory 读取演练历史(最多 limit 条,0=全部),按开始时间倒序。
func LoadDrillHistory(historyFile string, limit int) ([]DrillHistoryEntry, error) {
	out, err := loadDrillHistoryInOrder(historyFile)
	if err != nil {
		return nil, err
	}
	// 时间倒序;同刻按写入逆序(新的在前)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := out[i].Report, out[j].Report
		if ri == nil || rj == nil {
			return i > j
		}
		if !ri.StartedAt.Equal(rj.StartedAt) {
			return ri.StartedAt.After(rj.StartedAt)
		}
		return i > j
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// VerifyDrillChain 校验演练历史哈希链完整性(按写入顺序)。
func VerifyDrillChain(historyFile string) (ok bool, checked int, brokenAt string) {
	entries, err := loadDrillHistoryInOrder(historyFile)
	if err != nil {
		return false, 0, err.Error()
	}
	prev := ""
	for idx, e := range entries {
		if e.PrevHash != prev {
			return false, idx, fmt.Sprintf("entry %d prev_hash mismatch", idx)
		}
		expect := chainHash(e.PrevHash, e.Report)
		if e.Hash != expect {
			return false, idx, fmt.Sprintf("entry %d hash mismatch", idx)
		}
		prev = e.Hash
		checked++
	}
	return true, checked, ""
}

func chainHash(prev string, rep *DrillReport) string {
	payload, _ := json.Marshal(rep)
	h := sha256.Sum256(append([]byte(prev), payload...))
	return hex.EncodeToString(h[:])
}

func lastChainHash(historyFile string) string {
	data, err := os.ReadFile(historyFile) //nolint:gosec // 内部路径
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e DrillHistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err == nil {
			return e.Hash
		}
	}
	return ""
}
