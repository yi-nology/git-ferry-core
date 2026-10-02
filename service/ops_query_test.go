package service

import (
	"testing"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
)

func TestAggregateTrends(t *testing.T) {
	now := time.Now()
	day := func(offset int) time.Time { return now.AddDate(0, 0, -offset) }

	runs := []*model.SyncRun{
		{Status: model.StatusSuccess, StartTime: day(0), DurationMs: 100},
		{Status: model.StatusSuccess, StartTime: day(0), DurationMs: 300},
		{Status: model.StatusFailed, StartTime: day(0), DurationMs: 50},
		{Status: model.StatusSuccess, StartTime: day(1), DurationMs: 200},
		// 超出窗口：不计入
		{Status: model.StatusSuccess, StartTime: day(30), DurationMs: 10},
	}
	res := aggregateTrends(runs, 7)

	if res.Total != 4 {
		t.Fatalf("total = %d, want 4", res.Total)
	}
	if res.Success != 3 || res.Failed != 1 {
		t.Fatalf("success/failed = %d/%d, want 3/1", res.Success, res.Failed)
	}
	if res.SuccessRate != 75.0 {
		t.Fatalf("rate = %v, want 75", res.SuccessRate)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d, want 2 (今天+昨天)", len(res.Items))
	}
	// 首次出现顺序 = runs 顺序（新日期在前）
	if res.Items[0].Total != 3 || res.Items[1].Total != 1 {
		t.Fatalf("day order wrong: %+v", res.Items)
	}
	// 平均耗时
	if res.Items[0].AvgMs != float64(100+300+50)/3 {
		t.Fatalf("avg = %v", res.Items[0].AvgMs)
	}
}

func TestAggregateTrends_Empty(t *testing.T) {
	res := aggregateTrends(nil, 30)
	if res.Total != 0 || res.SuccessRate != 0 || len(res.Items) != 0 {
		t.Fatalf("empty case: %+v", res)
	}
}

func TestSummarizeHealth(t *testing.T) {
	items := []HealthScoreItem{
		{Key: "a", Score: 90, Level: "gold", Actions: []string{"fix a"}},
		{Key: "b", Score: 50, Level: "bronze", Actions: []string{"fix a"}},
		{Key: "c", Score: 30, Level: "basic", Actions: []string{"fix b"}},
	}
	sum := summarizeHealth(items)

	if sum.BelowSilver != 2 {
		t.Fatalf("below_silver = %d, want 2", sum.BelowSilver)
	}
	if len(sum.Attention) != 2 || sum.Attention[0].Key != "b" || sum.Attention[1].Key != "c" {
		t.Fatalf("attention order wrong: %+v", sum.Attention)
	}
	if sum.Levels["gold"] != 1 || sum.Levels["bronze"] != 1 || sum.Levels["basic"] != 1 {
		t.Fatalf("levels = %+v", sum.Levels)
	}
	// 动作去重汇总并排序
	if len(sum.TopActions) != 2 || sum.TopActions[0] != "fix a（×2）" || sum.TopActions[1] != "fix b（×1）" {
		t.Fatalf("top_actions = %+v", sum.TopActions)
	}
}

func TestSummarizeHealth_Empty(t *testing.T) {
	sum := summarizeHealth(nil)
	if sum.Attention == nil {
		t.Fatal("attention 应为空切片而非 nil（响应 JSON 需为 []）")
	}
	if len(sum.TopActions) != 0 || sum.BelowSilver != 0 {
		t.Fatalf("empty summary: %+v", sum)
	}
}
