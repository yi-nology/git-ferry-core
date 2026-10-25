package health

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectRunFacts_Empty(t *testing.T) {
	f := CollectRunFacts(nil, time.Now())
	assert.Equal(t, "false", f["has_history"])
	assert.Equal(t, "0", f["run_total"])
}

func TestCollectRunFacts_StreakAndRate(t *testing.T) {
	now := time.Now()
	runs := []RunSnapshot{
		{Status: "failed", ErrorMessage: "boom", EndAt: now.Add(-1 * time.Hour)},
		{Status: "failed", ErrorMessage: "boom", EndAt: now.Add(-2 * time.Hour)},
		{Status: "success", EndAt: now.Add(-3 * time.Hour)},
		{Status: "success", EndAt: now.Add(-4 * time.Hour)},
	}
	f := CollectRunFacts(runs, now)
	assert.Equal(t, "4", f["run_total"])
	assert.Equal(t, "2", f["run_success"])
	assert.Equal(t, "2", f["fail_streak"])
	assert.Equal(t, "false", f["recent_success"])
	assert.Equal(t, "false", f["error_free"])
	assert.Equal(t, "true", f["has_history"])
}

func TestEvaluateDimensions_AllGreen(t *testing.T) {
	f := Facts{
		"has_name": "true", "has_cron": "true", "cron": "0 2 * * *",
		"enabled": "true", "force_push_policy": "block",
		"backup_enabled": "true", "sync_wiki": "true",
		"has_history": "true", "run_total": "10", "run_success": "10",
		"fail_streak": "0", "recent_success": "true", "last_run_age_hours": "1",
	}
	res := EvaluateDimensions(nil, f)
	require.NotEmpty(t, res.Dimensions)
	assert.GreaterOrEqual(t, res.Score, 90)
	assert.Equal(t, "gold", res.Level)
	assert.Empty(t, res.Actions)

	names := map[string]Dimension{}
	for _, d := range res.Dimensions {
		names[d.Name] = d
	}
	assert.Equal(t, 100, names["reliability"].Score)
	assert.Equal(t, 100, names["freshness"].Score)
	assert.Equal(t, 100, names["safety"].Score)
}

func TestEvaluateDimensions_FailureDragsScore(t *testing.T) {
	f := Facts{
		"has_name": "true", "has_cron": "false", "enabled": "true",
		"force_push_policy": "allow", "git_force": "true",
		"has_history": "true", "run_total": "5", "run_success": "1",
		"fail_streak": "4", "recent_success": "false",
		"last_run_age_hours": "200",
	}
	res := EvaluateDimensions(nil, f)
	assert.Less(t, res.Score, 50)
	assert.Equal(t, "basic", res.Level)
	assert.NotEmpty(t, res.Issues)
	assert.NotEmpty(t, res.Actions)

	var reliability, safety *Dimension
	for i := range res.Dimensions {
		switch res.Dimensions[i].Name {
		case "reliability":
			reliability = &res.Dimensions[i]
		case "safety":
			safety = &res.Dimensions[i]
		}
	}
	require.NotNil(t, reliability)
	require.NotNil(t, safety)
	assert.LessOrEqual(t, reliability.Score, 25) // 连续失败 4 次
	assert.Less(t, safety.Score, 70)             // allow + git_force
	assert.NotEmpty(t, safety.Action)
}

func TestEvaluateDimensions_NoHistory(t *testing.T) {
	res := EvaluateDimensions(nil, Facts{"has_name": "true", "has_cron": "true", "enabled": "true"})
	assert.Equal(t, "basic", res.Level)
	found := false
	for _, d := range res.Dimensions {
		if d.Name == "reliability" {
			found = true
			assert.Equal(t, 0, d.Score)
			assert.NotEmpty(t, d.Action)
		}
	}
	assert.True(t, found)
}

// TestEvaluateDimensions_ErrorFreeSignalSurfaced 钉住"最近执行带有错误信息"这条信号。
//
// 回归背景(删除旧规则引擎时):`error_free` 这个 fact 原先**只有**旧规则引擎
// 消费(旧引擎单独报这条,权重 10)。旧引擎删掉后若没人接手,采集到的信号就
// 静默消失了 —— fact 不能没人用。现在由 reliability 维度接管。
//
// 这条用例是**反向验证**:把 reliability 里那段 error_free 分支去掉,此用例变红。
func TestEvaluateDimensions_ErrorFreeSignalSurfaced(t *testing.T) {
	base := Facts{
		"has_name": "true", "has_cron": "true", "enabled": "true",
		"has_history": "true", "recent_success": "true",
		"run_total": "3", "run_success": "3", "fail_streak": "0",
		"step_fail_count": "0", "retry_total": "0",
		"last_run_age_hours": "1",
		"force_push_policy":  "block",
		"backup_enabled":     "true", "sync_wiki": "true",
	}

	detailOf := func(f Facts, dim string) []string {
		res := EvaluateDimensions(nil, f)
		for _, d := range res.Dimensions {
			if d.Name == dim {
				return d.Detail
			}
		}
		return nil
	}

	// error_free=false ⇒ 必须报出来
	bad := Facts{}
	for k, v := range base {
		bad[k] = v
	}
	bad["error_free"] = "false"
	assert.Contains(t, detailOf(bad, "reliability"), "最近执行带有错误信息",
		"error_free=false 必须被 reliability 维度报出来")

	// error_free=true ⇒ 不报
	good := Facts{}
	for k, v := range base {
		good[k] = v
	}
	good["error_free"] = "true"
	assert.NotContains(t, detailOf(good, "reliability"), "最近执行带有错误信息",
		"error_free=true 不应报错误信息")

	// fact 缺失 = 不知道 ⇒ 不能当成"有错误"(不采集该 fact 的调用方不该被误报)
	missing := Facts{}
	for k, v := range base {
		missing[k] = v
	}
	delete(missing, "error_free")
	assert.NotContains(t, detailOf(missing, "reliability"), "最近执行带有错误信息",
		"error_free 缺失时不应报错误信息")
}

// TestEvaluateDimensions_DominantErrorSurfaced 钉住「最近失败主因」这条信号。
//
// 回归背景:`dominant_error`(最近若干次运行里出现最多的错误类型)**一直在采集,
// 却没有任何消费方** ⇒ 运维看不到"最近失败主因"(是分歧?认证?网络?),
// 排障只能逐条翻 run 记录。现在由 reliability 维度接管上报。
//
// 这条用例是**反向验证**:把 reliability 里那段 dominant_error 分支去掉,
// 此用例变红。
func TestEvaluateDimensions_DominantErrorSurfaced(t *testing.T) {
	now := time.Now()
	runs := []RunSnapshot{
		{Status: "failed", ErrorMessage: "boom", ErrorType: "divergent", EndAt: now.Add(-1 * time.Hour)},
		{Status: "failed", ErrorMessage: "boom", ErrorType: "divergent", EndAt: now.Add(-2 * time.Hour)},
		{Status: "success", ErrorType: "", EndAt: now.Add(-3 * time.Hour)},
	}
	f := CollectRunFacts(runs, now)
	require.Equal(t, "divergent", f["dominant_error"], "主导错误类型应是出现次数最多的那个")

	res := EvaluateDimensions(nil, f)
	var detail []string
	for _, d := range res.Dimensions {
		if d.Name == "reliability" {
			detail = d.Detail
		}
	}
	assert.Contains(t, detail, "最近失败主因: divergent",
		"dominant_error 必须被 reliability 维度报出来(此前采集了没人用)")

	// 没有失败 ⇒ 没有主导错误类型 ⇒ 不该出现该文案
	clean := CollectRunFacts([]RunSnapshot{{Status: "success", EndAt: now.Add(-time.Hour)}}, now)
	res2 := EvaluateDimensions(nil, clean)
	for _, d := range res2.Dimensions {
		if d.Name == "reliability" {
			assert.NotContains(t, d.Detail, "最近失败主因",
				"没有失败记录时不应报失败主因")
		}
	}
}

func TestDimensionWeightsSum(t *testing.T) {
	total := 0
	for _, s := range DefaultDimensions() {
		total += s.Weight
	}
	assert.Equal(t, 100, total)
}

func TestEvaluateDimensions_DriftPullsSafety(t *testing.T) {
	f := Facts{
		"has_name": "true", "has_cron": "true", "enabled": "true",
		"force_push_policy": "block",
		"has_history":       "true", "run_total": "5", "run_success": "5",
		"fail_streak": "0", "recent_success": "true", "last_run_age_hours": "1",
		"drift_count": "2",
	}
	res := EvaluateDimensions(nil, f)
	var safety *Dimension
	for i := range res.Dimensions {
		if res.Dimensions[i].Name == "safety" {
			safety = &res.Dimensions[i]
		}
	}
	require.NotNil(t, safety)
	assert.LessOrEqual(t, safety.Score, 35)
	assert.Contains(t, safety.Reason, "drift=2")
	assert.NotEmpty(t, safety.Action)
	// 其它维度仍可高分，但总分被 safety 拖低
	assert.Less(t, res.Score, 90)
}

func TestSortedDimensionNames(t *testing.T) {
	names := SortedDimensionNames()
	assert.ElementsMatch(t,
		[]string{"reliability", "freshness", "schedule", "safety", "completeness"},
		names,
	)
}
