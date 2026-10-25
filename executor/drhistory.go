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
	// 写链尾锚点:没有它,删掉最后一条(截断)后剩下的前缀仍是"合法链",
	// 校验会通过 ⇒ 演练记录被截断完全不可见。
	if err := writeDrillChainAnchor(historyFile, entry.Hash); err != nil {
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

// loadDrillHistoryStrict 读取演练历史并**报告坏行**。
//
// 第二个返回值是第一个坏行的行号(-1 = 没有坏行)。
// 校验路径必须用它:解析失败的行若被当噪声跳过,等于把损坏/被篡改的记录
// 从链里抹掉 —— 完全不可见。
func loadDrillHistoryStrict(historyFile string) ([]DrillHistoryEntry, int, error) {
	data, err := os.ReadFile(historyFile) //nolint:gosec // 内部路径
	if err != nil {
		if os.IsNotExist(err) {
			return []DrillHistoryEntry{}, -1, nil
		}
		return nil, -1, err
	}
	var out []DrillHistoryEntry
	badLine := -1
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e DrillHistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			if badLine < 0 {
				badLine = i
			}
			continue
		}
		out = append(out, e)
	}
	return out, badLine, nil
}

// drillChainAnchorFile 链尾锚点文件路径(与演练历史同目录)。
func drillChainAnchorFile(historyFile string) string { return historyFile + ".anchor" }

// writeDrillChainAnchor 写链尾锚点(原子替换:先写临时文件再 rename)。
func writeDrillChainAnchor(historyFile, hash string) error {
	path := drillChainAnchorFile(historyFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(hash+"\n"), 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readDrillChainAnchor 读链尾锚点;没有锚点文件时返回 ("", false)。
func readDrillChainAnchor(historyFile string) (string, bool) {
	data, err := os.ReadFile(drillChainAnchorFile(historyFile)) //nolint:gosec // 内部路径
	if err != nil {
		return "", false
	}
	h := strings.TrimSpace(string(data))
	if h == "" {
		return "", false
	}
	return h, true
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
	// **三条都要查**(否则"校验通过"是假的):
	//  ① 坏行:解析失败的行**必须计入 broken**,不能当噪声跳过 ——
	//     跳过等于把损坏/被篡改的记录从链里抹掉,完全不可见;
	//  ② 链内衔接:每条的 prev_hash 必须等于上一条的 hash;
	//  ③ 链尾锚点:锚点里的哈希必须等于最后一条的 hash —— 否则删掉最后一条
	//     (截断)后剩下的前缀仍是"合法链",校验会通过。
	entries, badLine, err := loadDrillHistoryStrict(historyFile)
	if err != nil {
		return false, 0, err.Error()
	}
	if badLine >= 0 {
		return false, badLine, fmt.Sprintf("第 %d 行无法解析(记录损坏或被篡改)", badLine+1)
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
	if anchor, ok := readDrillChainAnchor(historyFile); ok {
		if len(entries) == 0 || entries[len(entries)-1].Hash != anchor {
			return false, len(entries), "链尾锚点不匹配(演练历史可能被截断)"
		}
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
