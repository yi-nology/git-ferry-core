// Package health 实现仓库/任务健康评分的可配置规则 DSL(借鉴 Port Scorecards)。
//
// 规则 = 属性比较 + 权重 + 等级门槛。属性目前只暴露壳层可计算的字段,
// 后续接 core 指标时只需扩展 attr 函数表,DSL 不变。
package health

import (
	"strings"

	"github.com/oekit/pkg/convert"
)

// Rule 一条加权规则。
// Attr 支持: has_name, has_cron, recent_success, recent_failed, has_history, error_free
type Rule struct {
	ID   string `yaml:"id" json:"id"`
	Attr string `yaml:"attr" json:"attr"`
	// Op: eq / ne / gt / lt / truthy / falsy
	Op    string `yaml:"op" json:"op"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
	// Weight 命中得分,满分归一到 100
	Weight int `yaml:"weight" json:"weight"`
	// Message 未命中时的说明
	Message string `yaml:"message,omitempty" json:"message,omitempty"`
}

// Level 等级门槛。
type Level struct {
	Name     string `yaml:"name" json:"name"`
	MinScore int    `yaml:"min_score" json:"min_score"`
}

// Config 规则集。
type Config struct {
	Rules  []Rule  `yaml:"rules" json:"rules"`
	Levels []Level `yaml:"levels" json:"levels"`
}

// DefaultConfig 默认规则(无自定义配置时使用)。
func DefaultConfig() *Config {
	return &Config{
		Rules: []Rule{
			{ID: "has_name", Attr: "has_name", Op: "truthy", Weight: 10, Message: "缺少任务名称"},
			{ID: "has_cron", Attr: "has_cron", Op: "truthy", Weight: 20, Message: "未配置 cron 调度"},
			{ID: "has_history", Attr: "has_history", Op: "truthy", Weight: 30, Message: "尚无执行历史"},
			{ID: "recent_success", Attr: "recent_success", Op: "truthy", Weight: 30, Message: "最近一次执行未成功"},
			{ID: "error_free", Attr: "error_free", Op: "truthy", Weight: 10, Message: "最近执行带有错误信息"},
		},
		Levels: []Level{
			{Name: "gold", MinScore: 80},
			{Name: "silver", MinScore: 60},
			{Name: "bronze", MinScore: 40},
			{Name: "basic", MinScore: 0},
		},
	}
}

// Facts 评分对象的属性快照。
type Facts map[string]string

// Result 单对象评分结果。
type Result struct {
	Score  int      `json:"score"`
	Level  string   `json:"level"`
	Issues []string `json:"issues,omitempty"`
}

// Evaluate 按规则集评分。
func (c *Config) Evaluate(f Facts) Result {
	if c == nil || len(c.Rules) == 0 {
		return Result{Score: 0, Level: "basic"}
	}
	totalWeight, score := 0, 0
	var issues []string
	for _, r := range c.Rules {
		totalWeight += max(r.Weight, 0)
		if match(f, &r) {
			score += max(r.Weight, 0)
		} else if r.Message != "" {
			issues = append(issues, r.Message)
		}
	}
	if totalWeight > 0 {
		score = score * 100 / totalWeight
	}
	if score > 100 {
		score = 100
	}
	return Result{Score: score, Level: c.levelOf(score), Issues: issues}
}

func (c *Config) levelOf(score int) string {
	best := "basic"
	bestMin := -1
	for _, lv := range c.Levels {
		if score >= lv.MinScore && lv.MinScore > bestMin {
			best, bestMin = lv.Name, lv.MinScore
		}
	}
	return best
}

func match(f Facts, r *Rule) bool {
	v := strings.ToLower(strings.TrimSpace(f[r.Attr]))
	want := strings.ToLower(strings.TrimSpace(r.Value))
	switch strings.ToLower(r.Op) {
	case "", "truthy":
		return v != "" && v != "false" && v != "0"
	case "falsy":
		return v == "" || v == "false" || v == "0"
	case "eq":
		return v == want
	case "ne":
		return v != want
	case "gt":
		return toFloat(v) > toFloat(want)
	case "lt":
		return toFloat(v) < toFloat(want)
	default:
		return false
	}
}

// toFloat 字符串转 float64，解析失败回落 0。
//
// 语义变更说明：原实现用 fmt.Sscanf(s, "%f")，"12abc" 会被部分解析成 12；
// 现委托 oekit/pkg/convert.Str2Float64（内部为 strconv.ParseFloat），
// 遇到任何非纯数字串（如 "12abc"、""、带空白）一律解析失败并回落 0。
// facts 全部由内部代码生成（数值均为合法十进制），故实际行为一致。
func toFloat(s string) float64 {
	return convert.Str2Float64(s, 0)
}
