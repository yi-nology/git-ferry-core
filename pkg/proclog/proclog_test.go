package proclog

import (
	"context"
	"log/slog"
	"testing"
)

// TestSetup_LevelFiltering 各级别映射与过滤（Setup 决定默认 handler 的最低级别）。
func TestSetup_LevelFiltering(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	ctx := context.Background()

	cases := []struct {
		level   string
		enabled map[slog.Level]bool
	}{
		{"debug", map[slog.Level]bool{slog.LevelDebug: true, slog.LevelInfo: true, slog.LevelWarn: true}},
		{"info", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: true, slog.LevelWarn: true}},
		{"warn", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: false, slog.LevelWarn: true}},
		{"error", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: false, slog.LevelWarn: false}},
		{"nope", map[slog.Level]bool{slog.LevelDebug: false, slog.LevelInfo: true, slog.LevelWarn: true}},
	}
	for _, c := range cases {
		Setup(c.level, "json")
		h := slog.Default().Handler()
		for lv, want := range c.enabled {
			if got := h.Enabled(ctx, lv); got != want {
				t.Errorf("level=%q: Enabled(%v)=%v want %v", c.level, lv, got, want)
			}
		}
	}

	// format=text 与 format=json 都应给出可用 handler
	Setup("info", "text")
	if slog.Default().Handler() == nil {
		t.Fatal("text handler 未安装")
	}
	Setup("info", "json")
	if slog.Default().Handler() == nil {
		t.Fatal("json handler 未安装")
	}
}
