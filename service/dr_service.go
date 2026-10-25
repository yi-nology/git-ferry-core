package service

import (
	"context"
	"log/slog"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/pkg/pathx"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
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
		return nil, ErrBackupDisabled
	}
	if !pathx.IsSafeFileName(name) {
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
		return nil, nil, ErrBackupDisabled
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
			if !pathx.IsSafeFileName(n) {
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
		return nil, ErrBackupDisabled
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
		return nil, ErrBackupDisabled
	}
	// 还没有清单(没做过冷备)不是"错误" —— 返回"没东西可校验"的可读结果。
	// 否则调用方拿到的是 500 "load manifest: no such file or directory",
	// 看不出是"还没做冷备"还是"真出事了"。
	if _, err := os.Stat(filepath.Join(dir, "backup-manifest.json")); err != nil {
		return &executor.ManifestVerifyResult{
			OK:      false,
			Message: "尚无冷备完整性清单(还没做过冷备):先跑一次冷备生成清单,再回来校验",
		}, nil
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
		return nil, ErrBackupDisabled
	}
	all, err := executor.ListBundles(dir, "")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	// 任务 key 的**权威来源是任务表**,不是文件名。
	//
	// 旧实现从文件名反推(按 "-" 切第一段)⇒ 任务 key 里带 "-" 时
	// ("prod-eu" / "prod-us"、以及 UUID)反推出来的值要么对不上任何任务,
	// 要么**两个任务被合成一条指标** —— 停更 3 天的任务被另一个
	// 5 分钟前刚备份的任务"洗白",超标不报警。
	known := s.knownTaskPrefixes()
	byTask := map[string][]executor.BundleInfo{}
	for _, b := range all {
		key := b.TaskKey
		if key == "" {
			key = resolveBundleTaskKey(b.Name, known)
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


// knownTaskPrefixes 返回"任务 key 文件名前缀 → 真实任务 key"的映射。
//
// 冷备文件名以 SanitizeFileToken(taskKey) + "-" 开头 ⇒ 用**最长前缀匹配**
// 就能把文件映射回真实任务,不需要从文件名里"猜"。
func (s *Service) knownTaskPrefixes() map[string]string {
	out := map[string]string{}
	if s == nil || s.tasks == nil {
		// 没有任务服务(测试/精简装配)⇒ 退回"按文件名归组"。
		return out
	}
	ctx := context.Background()
	tasks, _, err := s.ListTasks(ctx, "", 0, dao.MaxPageSize)
	if err != nil {
		slog.Warn("RPO 报告:取任务列表失败,冷备将按文件名归组(可能不精确)", "error", err)
		return out
	}
	for _, t := range tasks {
		if t == nil || t.Key == "" {
			continue
		}
		out[strutil.SanitizeFileToken(t.Key)+"-"] = t.Key
	}
	return out
}

// resolveBundleTaskKey 把冷备文件名映射回任务 key。
//
// 已知任务 ⇒ 最长前缀匹配(返回真实 key);
// 未知任务(已删除/超出任务列表上限)⇒ 去掉扩展名与末尾时间戳,
// **至少不把两个任务合成一条指标**(旧实现按 "-" 切第一段会)。
func resolveBundleTaskKey(name string, known map[string]string) string {
	best, bestKey := "", ""
	for prefix, key := range known {
		if strings.HasPrefix(name, prefix) && len(prefix) > len(best) {
			best, bestKey = prefix, key
		}
	}
	if bestKey != "" {
		return bestKey
	}
	base := name
	for _, suf := range []string{".bundle.enc", ".zip.enc", ".bundle", ".zip"} {
		if strings.HasSuffix(base, suf) {
			base = strings.TrimSuffix(base, suf)
			break
		}
	}
	// 末尾是 "-20060102-150405"(时间戳 15 字符 + 分隔符 = 16)⇒ 去掉
	if len(base) > 16 && base[len(base)-16] == '-' {
		base = base[:len(base)-16]
	}
	return base
}

// inferTaskFromBundle 已废弃:从文件名反推任务 key 不可靠(见 resolveBundleTaskKey)。
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

// ExportDrillHistory 导出演练历史为 JSON 或 CSV(供审计/上报)。
func (s *Service) ExportDrillHistory(format string, limit int) (contentType string, data []byte, err error) {
	entries, err := s.DrillHistory(limit)
	if err != nil {
		return "", nil, err
	}
	if format == "csv" {
		var b strings.Builder
		b.WriteString("bundle_name,started_at,duration_ms,success,fsck_ok,commit_count,est_rto,bundle_size,missing_refs,errors,hash\n")
		for _, e := range entries {
			r := e.Report
			if r == nil {
				continue
			}
			miss := strings.Join(r.MissingRefs, "|")
			errs := strings.Join(r.Errors, "|")
			b.WriteString(strutil.CSVEscape(r.BundleName) + "," +
				strutil.CSVEscape(r.StartedAt.Format(time.RFC3339)) + "," +
				strconv.FormatInt(r.DurationMS, 10) + "," +
				strconv.FormatBool(r.Success) + "," +
				strconv.FormatBool(r.FsckOK) + "," +
				strconv.Itoa(r.CommitCount) + "," +
				strutil.CSVEscape(r.EstRTO) + "," +
				strconv.FormatInt(r.BundleSize, 10) + "," +
				strutil.CSVEscape(miss) + "," +
				strutil.CSVEscape(errs) + "," +
				strutil.CSVEscape(e.Hash) + "\n")
		}
		return "text/csv; charset=utf-8", []byte(b.String()), nil
	}
	out, err := json.MarshalIndent(map[string]any{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"total":       len(entries),
		"items":       entries,
	}, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return "application/json; charset=utf-8", out, nil
}
