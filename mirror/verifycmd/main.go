package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/yi-nology/git-ferry-core/mirror"
)

// verifycmd 是公开仓禁词复验的独立可执行形态。
//
// 语义与发布侧同一套原语(单一实现,两个形态):
//
//   - 身份残留:mirror.Rewriter.Pending(边界语义与发布侧改写器一致);
//
//   - 痕迹规则:mirror.ApplyScrub 的规则语义(auto/mandatory/allowlist)。
//
//     用法:go run github.com/yi-nology/git-ferry-core/mirror/verifycmd@<ref> \
//     -policy .mirror-policy.json .
//
// 退出码:0=干净;1=有命中;2=policy 解析/正则编译失败。
// allowlisted 命中打印但不计入失败(「不报警」与「不留痕」是两件事)。
func main() {
	policyPath := flag.String("policy", "", "policy 快照文件(必需)")
	flag.Parse()
	if *policyPath == "" {
		fmt.Fprintln(os.Stderr, "-policy 必需")
		os.Exit(2)
	}
	data, err := os.ReadFile(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 policy 失败:", err)
		os.Exit(2)
	}
	rules, mapping, err := loadPolicy(data)
	if err != nil {
		// policy 解析失败 = 非零退出(策略解析失败即失败,archcheck/strict-conf 同款纪律)
		fmt.Fprintln(os.Stderr, "policy 解析失败:", err)
		os.Exit(2)
	}
	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	hits, err := scanAll(dirs, rules, mapping)
	if err != nil {
		fmt.Fprintln(os.Stderr, "扫描失败:", err)
		os.Exit(2)
	}
	printHits(hits)
	for _, h := range hits {
		if !h.Allowlisted {
			os.Exit(1)
		}
	}
	os.Exit(0)
}

// loadPolicy 严格解析 policy 快照:未知字段=失败(DisallowUnknownFields),
// 正则编译失败=失败。策略解析失败 = 非零退出。
func loadPolicy(data []byte) ([]mirror.ScrubRule, mirror.Mapping, error) {
	var snap mirror.PolicySnapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		// 兼容裸数组形态?不 —— 单一 schema,fail-closed。
		return nil, mirror.Mapping{}, fmt.Errorf("policy JSON 解析失败: %w", err)
	}
	if snap.Version != 1 {
		return nil, mirror.Mapping{}, fmt.Errorf("不支持的 policy 版本: %d", snap.Version)
	}
	rules := make([]mirror.ScrubRule, 0, len(snap.ScrubRules))
	for i, sr := range snap.ScrubRules {
		pat, err := regexp.Compile(sr.Pattern)
		if err != nil {
			return nil, mirror.Mapping{}, fmt.Errorf("第 %d 条规则正则非法(%s): %w", i+1, sr.Name, err)
		}
		sev := mirror.ScrubAuto
		if sr.Severity == string(mirror.ScrubMandatory) {
			sev = mirror.ScrubMandatory
		}
		rules = append(rules, mirror.ScrubRule{
			Name:          sr.Name,
			Category:      sr.Category,
			Pattern:       pat,
			Severity:      sev,
			Replacement:   sr.Replacement,
			PathAllowlist: sr.PathAllowlist,
		})
	}
	return rules, mirror.Mapping{Source: snap.Mapping.Source, Target: snap.Mapping.Target}, nil
}

// scanHit 是一次命中记录。
type scanHit struct {
	Path        string
	Line        int
	Rule        string
	Text        string
	Allowlisted bool
}

// scanAll 扫描 dirs 下全部常规文件。
//
// 文件分两类(与发布侧同一分法):
//   - 文本文件:全量规则 + 身份残留(Pending);
//   - 二进制/超限(>8MB 或含 NUL):身份残留 + mandatory 规则
//     (与发布侧 checkMandatoryOnly 同语义:密钥类不因二进制放行)。
//
// allowlisted 命中照常记录但不算失败。
func scanAll(dirs []string, rules []mirror.ScrubRule, mapping mirror.Mapping) ([]scanHit, error) {
	rw, err := mirror.NewRewriter([]mirror.IdentityRule{{
		Source: mapping.Source, Target: mapping.Target, Kind: mirror.KindModule,
	}})
	if err != nil {
		return nil, err
	}
	mandatory := make([]mirror.ScrubRule, 0, len(rules))
	for _, r := range rules {
		if r.Mandatory() {
			mandatory = append(mandatory, r)
		}
	}
	out := make([]scanHit, 0)
	seen := make(map[string]bool)
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel := filepath.ToSlash(p)
			if seen[rel] {
				return nil
			}
			seen[rel] = true
			data, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("读文件 %s 失败: %w", p, err)
			}

			oversized := len(data) > 8<<20
			binary := bytes.IndexByte(data, 0) >= 0
			switch {
			case oversized || binary:
				// 二进制/超限:身份残留 + mandatory(密钥类不放行)
				if mapping.Source != "" && rw.Pending(data) {
					out = append(out, scanHit{Path: rel, Line: 0, Rule: "identity-residual"})
				}
				out = append(out, scrubScan(rel, data, mandatory)...)
			default:
				if mapping.Source != "" && rw.Pending(data) {
					out = append(out, scanHit{Path: rel, Line: 0, Rule: "identity-residual"})
				}
				out = append(out, scrubScan(rel, data, rules)...)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scrubScan 对内容跑 scrub 规则(只记命中不改内容 —— 复验是只读的)。
// allowlist 命中标记 Allowlisted,打印但不算失败。
func scrubScan(path string, data []byte, rules []mirror.ScrubRule) []scanHit {
	res := mirror.ApplyScrub(path, data, rules)
	out := make([]scanHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, scanHit{
			Path:        h.Path,
			Line:        h.Line,
			Rule:        h.Rule,
			Text:        h.Text,
			Allowlisted: h.Allowlisted,
		})
	}
	return out
}

// printHits 把命中打印到 stderr(每行:文件:行 规则 [allowlisted] 原文)。
func printHits(hits []scanHit) {
	for _, h := range hits {
		line := fmt.Sprintf("%s:%d %s", h.Path, h.Line, h.Rule)
		if h.Allowlisted {
			line += " [allowlisted]"
		}
		if h.Text != "" {
			line += ": " + h.Text
		}
		fmt.Fprintln(os.Stderr, line)
	}
	if len(hits) == 0 {
		fmt.Fprintln(os.Stderr, "verify: clean")
	}
}
