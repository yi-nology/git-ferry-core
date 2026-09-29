package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
)

// DrillReport 一次灾备演练(DR Drill)的结果。
// 语义:从冷备 bundle 恢复到临时目录 → fsck → 比对 refs → 估算 RTO。
type DrillReport struct {
	BundleName   string            `json:"bundle_name"`
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   time.Time         `json:"finished_at"`
	DurationMS   int64             `json:"duration_ms"`
	Success      bool              `json:"success"`
	RefSpecs     []string          `json:"ref_specs"`
	RestoredRefs []string          `json:"restored_refs"`
	MissingRefs  []string          `json:"missing_refs"`
	ExtraRefs    []string          `json:"extra_refs"`
	FsckOK       bool              `json:"fsck_ok"`
	FsckOutput   string            `json:"fsck_output,omitempty"`
	CommitCount  int               `json:"commit_count"`
	EstRTO       string            `json:"est_rto"` // 人类可读,如 "12s"
	BundleSize   int64             `json:"bundle_size"`
	Errors       []string          `json:"errors,omitempty"`
	CheckedAt    time.Time         `json:"checked_at"`
	Details      map[string]string `json:"details,omitempty"`
}

// RunDRDrill 对单个 bundle 做灾备恢复演练。
//
// 流程:
//  1. bundle list-heads 取期望 refs
//  2. RestoreBundle 到临时目录
//  3. git fsck --strict 校验对象完整性
//  4. for-each-ref 比对恢复出的 refs
//  5. 统计 commit 数与耗时(作为 RTO 观测值)
//
// 临时目录在返回前清理,不留残留。
func RunDRDrill(ctx context.Context, bundlePath string) (*DrillReport, error) {
	start := time.Now()
	rep := &DrillReport{
		BundleName: filepath.Base(bundlePath),
		StartedAt:  start,
		Details:    map[string]string{},
	}
	defer func() {
		rep.FinishedAt = time.Now()
		rep.DurationMS = rep.FinishedAt.Sub(start).Milliseconds()
		rep.EstRTO = formatDuration(rep.FinishedAt.Sub(start))
		rep.CheckedAt = rep.FinishedAt
	}()

	fi, err := os.Stat(bundlePath)
	if err != nil {
		rep.Errors = append(rep.Errors, "bundle not found: "+err.Error())
		return rep, errors.Wrap(err, "bundle not found")
	}
	rep.BundleSize = fi.Size()

	// 1) 期望 refs
	heads, err := runGitRead(ctx, "", "bundle", "list-heads", bundlePath)
	if err != nil {
		rep.Errors = append(rep.Errors, "list-heads: "+err.Error())
		return rep, errors.Wrap(err, "bundle list-heads")
	}
	expected := parseHeadRefs(heads)
	rep.RefSpecs = expected
	if len(expected) == 0 {
		rep.Errors = append(rep.Errors, "bundle has no refs")
		return rep, errors.New("bundle has no refs")
	}

	// 2) 恢复到临时目录
	tmpDir, err := os.MkdirTemp("", "git-ferry-drill-*")
	if err != nil {
		rep.Errors = append(rep.Errors, "temp dir: "+err.Error())
		return rep, errors.Wrap(err, "create temp dir")
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	dest := filepath.Join(tmpDir, "restored")

	if err := RestoreBundle(ctx, bundlePath, dest); err != nil {
		rep.Errors = append(rep.Errors, "restore: "+err.Error())
		return rep, errors.Wrap(err, "restore")
	}

	// 3) fsck
	fsckOut, fsckErr := runGitRead(ctx, dest, "fsck", "--strict", "--no-progress")
	rep.FsckOutput = truncate(fsckOut, 4000)
	rep.FsckOK = fsckErr == nil
	if fsckErr != nil {
		rep.Errors = append(rep.Errors, "fsck: "+fsckErr.Error())
	}

	// 4) 比对 refs
	restoredOut, err := runGitRead(ctx, dest, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		rep.Errors = append(rep.Errors, "for-each-ref: "+err.Error())
		return rep, errors.Wrap(err, "for-each-ref")
	}
	restored := parseForEachRef(restoredOut)
	rep.RestoredRefs = sortedKeys(restored)
	rep.MissingRefs, rep.ExtraRefs = diffRefs(expected, restored)

	// 5) commit 计数(浅层统计,失败不致命)
	if out, err := runGitRead(ctx, dest, "rev-list", "--all", "--count"); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &rep.CommitCount)
	}

	rep.Success = rep.FsckOK && len(rep.MissingRefs) == 0 && len(rep.Errors) == 0
	return rep, nil
}

// BatchDRDrill 对多个 bundle 演练,返回逐个报告与汇总。
func BatchDRDrill(ctx context.Context, bundlePaths []string) (reports []*DrillReport, summary map[string]any) {
	ok, fail := 0, 0
	var totalMS int64
	for _, p := range bundlePaths {
		r, err := RunDRDrill(ctx, p)
		if r == nil {
			r = &DrillReport{
				BundleName: filepath.Base(p),
				StartedAt:  time.Now(),
				Errors:     []string{"drill produced no report: " + errString(err)},
			}
		}
		reports = append(reports, r)
		if r.Success {
			ok++
		} else {
			fail++
		}
		totalMS += r.DurationMS
	}
	avg := int64(0)
	if len(reports) > 0 {
		avg = totalMS / int64(len(reports))
	}
	summary = map[string]any{
		"total":     len(reports),
		"success":   ok,
		"failed":    fail,
		"total_ms":  totalMS,
		"avg_ms":    avg,
		"est_rto":   formatDuration(time.Duration(avg) * time.Millisecond),
		"generated": time.Now().UTC().Format(time.RFC3339),
	}
	return reports, summary
}

func parseHeadRefs(out string) []string {
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 {
			refs = append(refs, fields[1])
		}
	}
	return refs
}

func parseForEachRef(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 {
			m[fields[0]] = fields[1]
		}
	}
	return m
}

// diffRefs 比对期望 ref 名集合与恢复集合。
// 期望来自 bundle list-heads(可能是 refs/heads/x 或 heads/x 简写);
// 恢复侧 for-each-ref 输出完整名,统一归一化后再比。
func diffRefs(expected []string, restored map[string]string) (missing, extra []string) {
	restoredSet := map[string]bool{}
	for name := range restored {
		restoredSet[normalizeRef(name)] = true
	}
	expectedSet := map[string]bool{}
	for _, e := range expected {
		n := normalizeRef(e)
		expectedSet[n] = true
		if !restoredSet[n] {
			missing = append(missing, e)
		}
	}
	for name := range restoredSet {
		n := name
		found := false
		for e := range expectedSet {
			if e == n {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

func normalizeRef(ref string) string {
	r := strings.TrimSpace(ref)
	r = strings.TrimPrefix(r, "refs/heads/")
	r = strings.TrimPrefix(r, "refs/remotes/origin/")
	r = strings.TrimPrefix(r, "heads/")
	r = strings.TrimPrefix(r, "origin/")
	return r
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
