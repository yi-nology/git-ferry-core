package service

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yi-nology/git-ferry-core/executor"
)

// DrillReport = executor.DrillReport(壳层直接使用)
type DrillReport = executor.DrillReport

// DrillHistoryEntry = executor.DrillHistoryEntry
type DrillHistoryEntry = executor.DrillHistoryEntry

// BackupManifest = executor.BackupManifest
type BackupManifest = executor.BackupManifest

// ManifestVerifyResult = executor.ManifestVerifyResult
type ManifestVerifyResult = executor.ManifestVerifyResult

// RunDRDrill 对指定 bundle 做灾备演练,并写入哈希链历史。
func (s *Service) RunDRDrill(ctx context.Context, name string) (*DrillReport, error) {
	dir := s.config.Sync.BackupDir
	if dir == "" {
		return nil, errBackupDisabled
	}
	if filepath.Base(name) != name {
		return nil, errInvalidName
	}
	rep, err := executor.RunDRDrill(ctx, filepath.Join(dir, name))
	if rep != nil {
		_, _ = executor.SaveDrillReport(s.drillHistoryPath(), rep)
	}
	return rep, err
}

// BatchDRDrill 演练多个 bundle(空=目录下全部,最多 max 个)。
func (s *Service) BatchDRDrill(ctx context.Context, names []string, max int) ([]*DrillReport, map[string]any, error) {
	dir := s.config.Sync.BackupDir
	if dir == "" {
		return nil, nil, errBackupDisabled
	}
	var paths []string
	if len(names) == 0 {
		all, err := executor.ListBundles(dir, "")
		if err != nil {
			return nil, nil, err
		}
		// 新→旧
		sort.Slice(all, func(i, j int) bool { return all[i].ModTime.After(all[j].ModTime) })
		if max > 0 && len(all) > max {
			all = all[:max]
		}
		for _, b := range all {
			paths = append(paths, b.Path)
		}
	} else {
		for _, n := range names {
			if filepath.Base(n) != n {
				return nil, nil, errInvalidName
			}
			paths = append(paths, filepath.Join(dir, n))
		}
	}
	reports, summary := executor.BatchDRDrill(ctx, paths)
	for _, r := range reports {
		_, _ = executor.SaveDrillReport(s.drillHistoryPath(), r)
	}
	return reports, summary, nil
}

// DrillHistory 读取演练历史。
func (s *Service) DrillHistory(limit int) ([]executor.DrillHistoryEntry, error) {
	return executor.LoadDrillHistory(s.drillHistoryPath(), limit)
}

// VerifyDrillChain 校验演练历史哈希链。
func (s *Service) VerifyDrillChain() (bool, int, string) {
	return executor.VerifyDrillChain(s.drillHistoryPath())
}

// BuildBackupManifest 生成并保存冷备完整性清单。
func (s *Service) BuildBackupManifest() (*executor.BackupManifest, error) {
	dir := s.config.Sync.BackupDir
	if dir == "" {
		return nil, errBackupDisabled
	}
	m, err := executor.BuildManifest(dir)
	if err != nil {
		return nil, err
	}
	if _, err := executor.SaveManifest(m); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyBackupManifest 现场重算并比对清单。
func (s *Service) VerifyBackupManifest() (*executor.ManifestVerifyResult, error) {
	dir := s.config.Sync.BackupDir
	if dir == "" {
		return nil, errBackupDisabled
	}
	return executor.VerifyManifest(dir)
}

// RPOMetric 单任务/仓库的恢复点观测。
type RPOMetric struct {
	TaskKey      string    `json:"task_key"`
	RepoKey      string    `json:"repo_key,omitempty"`
	LatestBundle string    `json:"latest_bundle"`
	LastBackupAt time.Time `json:"last_backup_at"`
	BundleCount  int       `json:"bundle_count"`
	TotalSize    int64     `json:"total_size"`
	RPOSeconds   int64     `json:"rpo_seconds"`
	RPOHuman     string    `json:"rpo_human"`
	RPOViolated  bool      `json:"rpo_violated"`
	EstRTO       string    `json:"est_rto"`
}

// RPOReport 全量 RPO/RTO 观测。
type RPOReport struct {
	Generated       time.Time   `json:"generated"`
	BackupDir       string      `json:"backup_dir"`
	RPOMaxSeconds   int64       `json:"rpo_max_seconds"`
	OverallRPOSec   int64       `json:"overall_rpo_seconds"`
	OverallRPOHuman string      `json:"overall_rpo_human"`
	WorstTask       string      `json:"worst_task"`
	Violations      int         `json:"violations"`
	Metrics         []RPOMetric `json:"metrics"`
}

// RPOReport 按冷备目录聚合每个任务的备份新鲜度(RPO)与估算恢复时间(RTO)。
// rpoMaxSeconds>0 时标记超标任务。
func (s *Service) RPOReport(rpoMaxSeconds int64) (*RPOReport, error) {
	dir := s.config.Sync.BackupDir
	if dir == "" {
		return nil, errBackupDisabled
	}
	all, err := executor.ListBundles(dir, "")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	byTask := map[string][]executor.BundleInfo{}
	for _, b := range all {
		key := b.TaskKey
		if key == "" {
			key = inferTaskFromBundle(b.Name)
		}
		byTask[key] = append(byTask[key], b)
	}
	rep := &RPOReport{
		Generated:     now.UTC(),
		BackupDir:     dir,
		RPOMaxSeconds: rpoMaxSeconds,
		Metrics:       []RPOMetric{},
	}
	var worstSec int64 = -1
	for task, bundles := range byTask {
		sort.Slice(bundles, func(i, j int) bool { return bundles[i].ModTime.After(bundles[j].ModTime) })
		latest := bundles[0]
		rpo := int64(now.Sub(latest.ModTime).Seconds())
		var total int64
		for _, b := range bundles {
			total += b.Size
		}
		m := RPOMetric{
			TaskKey:      task,
			LatestBundle: latest.Name,
			LastBackupAt: latest.ModTime.UTC(),
			BundleCount:  len(bundles),
			TotalSize:    total,
			RPOSeconds:   rpo,
			RPOHuman:     humanSeconds(rpo),
			EstRTO:       estimateRTO(latest.Size),
		}
		if rpoMaxSeconds > 0 {
			m.RPOViolated = rpo > rpoMaxSeconds
			if m.RPOViolated {
				rep.Violations++
			}
		}
		if rpo > worstSec {
			worstSec = rpo
			rep.WorstTask = task
		}
		rep.Metrics = append(rep.Metrics, m)
	}
	sort.Slice(rep.Metrics, func(i, j int) bool {
		return rep.Metrics[i].RPOSeconds > rep.Metrics[j].RPOSeconds
	})
	rep.OverallRPOSec = worstSec
	if worstSec >= 0 {
		rep.OverallRPOHuman = humanSeconds(worstSec)
	}
	return rep, nil
}

func (s *Service) drillHistoryPath() string {
	dir := s.config.Sync.BackupDir
	if dir == "" {
		dir = "data"
	}
	return filepath.Join(dir, "drill-history.jsonl")
}

func inferTaskFromBundle(name string) string {
	base := strings.TrimSuffix(name, ".bundle")
	parts := strings.Split(base, "-")
	if len(parts) >= 3 {
		return parts[0]
	}
	return "unknown"
}

func humanSeconds(sec int64) string {
	if sec < 0 {
		return "n/a"
	}
	if sec < 60 {
		return formatSec(sec) + "s"
	}
	if sec < 3600 {
		return formatSec(sec/60) + "m"
	}
	if sec < 86400 {
		return formatSec(sec/3600) + "h"
	}
	return formatSec(sec/86400) + "d"
}

func formatSec(n int64) string {
	return strconv.FormatInt(n, 10)
}

// estimateRTO 按 bundle 大小粗估恢复耗时(经验值:约 80MB/s 落盘+fsck)。
func estimateRTO(size int64) string {
	const bytesPerSec = 80 * 1024 * 1024
	if size <= 0 {
		return "<1s"
	}
	sec := size / bytesPerSec
	if sec < 1 {
		sec = 1
	}
	// fsck 固定开销约 2s
	sec += 2
	return humanSeconds(sec)
}
