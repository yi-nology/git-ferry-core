package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/executor"
)

// TestRPOReport 冷备新鲜度聚合：超标标记、排序、整体指标与边界防护。
func TestRPOReport(t *testing.T) {
	dir := t.TempDir()
	s := newBackupService(dir, 0, false)

	// 命名需 ≥3 段才能从文件名推断 taskKey（inferTaskFromBundle）
	oldB := filepath.Join(dir, "taskB-20260101-010101.bundle")
	newB := filepath.Join(dir, "taskA-20260101-020202.bundle")
	prevA := filepath.Join(dir, "taskA-20260101-010101.bundle")
	require.NoError(t, os.WriteFile(oldB, make([]byte, 10), 0o600))
	require.NoError(t, os.WriteFile(newB, make([]byte, 20), 0o600))
	require.NoError(t, os.WriteFile(prevA, make([]byte, 30), 0o600))
	old := time.Now().Add(-10 * time.Hour)
	require.NoError(t, os.Chtimes(oldB, old, old))
	require.NoError(t, os.Chtimes(prevA, old, old))

	rep, err := s.RPOReport(3600) // 1 小时 RPO 目标
	require.NoError(t, err)
	require.Len(t, rep.Metrics, 2)
	assert.Equal(t, "taskB", rep.WorstTask, "最旧备份的任务为 worst")
	assert.Equal(t, 1, rep.Violations)
	assert.Greater(t, rep.OverallRPOSec, int64(35000))
	assert.Equal(t, "10h", rep.OverallRPOHuman)
	// 按 RPOSeconds 降序：超标任务在前
	assert.Equal(t, "taskB", rep.Metrics[0].TaskKey)
	assert.True(t, rep.Metrics[0].RPOViolated)
	assert.Equal(t, "10h", rep.Metrics[0].RPOHuman)
	assert.False(t, rep.Metrics[1].RPOViolated)
	assert.Equal(t, 2, rep.Metrics[1].BundleCount)
	assert.Equal(t, "3s", rep.Metrics[0].EstRTO, "小文件估算 = 落盘1s + fsck 2s")

	// 不设上限 → 不标记违规
	rep2, err := s.RPOReport(0)
	require.NoError(t, err)
	assert.Equal(t, 0, rep2.Violations)
	assert.False(t, rep2.Metrics[0].RPOViolated)

	// 未配置 backup_dir
	_, err = newBackupService("", 0, false).RPOReport(1)
	require.ErrorIs(t, err, errBackupDisabled)

	// 空目录
	empty, err := newBackupService(filepath.Join(dir, "none"), 0, false).RPOReport(1)
	require.NoError(t, err)
	assert.Empty(t, empty.Metrics)
	assert.EqualValues(t, -1, empty.OverallRPOSec)
}

// TestDrillHistoryChain 演练历史 JSONL 读取、limit、哈希链校验与篡改检测。
func TestDrillHistoryChain(t *testing.T) {
	dir := t.TempDir()
	s := newBackupService(dir, 0, false)
	history := filepath.Join(dir, "drill-history.jsonl")

	rep1 := &executor.DrillReport{BundleName: "a.bundle", Success: true, FsckOK: true}
	rep2 := &executor.DrillReport{BundleName: "b.bundle", Success: false}
	_, err := executor.SaveDrillReport(history, rep1)
	require.NoError(t, err)
	_, err = executor.SaveDrillReport(history, rep2)
	require.NoError(t, err)

	entries, err := s.DrillHistory(10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	// LoadDrillHistory 返回新在前
	assert.Equal(t, "b.bundle", entries[0].Report.BundleName)
	assert.Equal(t, "a.bundle", entries[1].Report.BundleName)
	assert.Equal(t, entries[1].Hash, entries[0].PrevHash, "哈希链相连")

	limited, err := s.DrillHistory(1)
	require.NoError(t, err)
	require.Len(t, limited, 1)

	ok, n, detail := s.VerifyDrillChain()
	assert.True(t, ok, "链路应完整: %s", detail)
	assert.Equal(t, 2, n)

	// 篡改：改掉第一行内容
	raw, err := os.ReadFile(history)
	require.NoError(t, err)
	lines := strings.SplitN(string(raw), "\n", 2)
	tampered := strings.Replace(lines[0], "a.bundle", "evil.bundle", 1)
	require.NoError(t, os.WriteFile(history, []byte(tampered+"\n"+lines[1]), 0o600))
	ok, _, _ = s.VerifyDrillChain()
	assert.False(t, ok, "篡改后应被检出")

	// 导出 CSV / JSON
	ct, data, err := s.ExportDrillHistory("csv", 10)
	require.NoError(t, err)
	assert.Contains(t, ct, "text/csv")
	assert.Contains(t, string(data), "bundle_name")
	ct, data, err = s.ExportDrillHistory("json", 10)
	require.NoError(t, err)
	assert.Contains(t, ct, "json")
	assert.NotEmpty(t, data)
}

// TestBackupManifest 生成 + 校验 + 篡改检出。
func TestBackupManifest(t *testing.T) {
	dir := t.TempDir()
	s := newBackupService(dir, 0, false)
	bundle := filepath.Join(dir, "taskA-20260101-010101.bundle")
	require.NoError(t, os.WriteFile(bundle, []byte("payload"), 0o600))

	m, err := s.BuildBackupManifest()
	require.NoError(t, err)
	require.Len(t, m.Entries, 1)
	assert.Equal(t, "taskA-20260101-010101.bundle", m.Entries[0].Name)

	res, err := s.VerifyBackupManifest()
	require.NoError(t, err)
	assert.True(t, res.OK, "未篡改应通过: %+v", res)

	// 篡改 bundle 内容
	require.NoError(t, os.WriteFile(bundle, []byte("tampered!"), 0o600))
	res, err = s.VerifyBackupManifest()
	require.NoError(t, err)
	assert.False(t, res.OK, "篡改后应失败")
	assert.NotEmpty(t, res.HashMismatch, "应报告哈希差异项")
	assert.NotEmpty(t, res.Message)

	// 未配置 backup_dir
	_, err = newBackupService("", 0, false).BuildBackupManifest()
	require.ErrorIs(t, err, errBackupDisabled)
	_, err = newBackupService("", 0, false).VerifyBackupManifest()
	require.ErrorIs(t, err, errBackupDisabled)
}
