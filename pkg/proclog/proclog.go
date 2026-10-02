// Package proclog 进程级日志初始化与失败退出。
//
// 公网壳（git-ferry）与内网壳（git-ferry-intranet）此前各自复制了一份逐字相同的
// 实现；两者都依赖 core，故收敛到这里。core 自身不调用本包（库不应决定宿主进程
// 的日志与退出行为）。
package proclog

import (
	"log/slog"
	"os"
)

// Setup 按 level/format 配置默认 slog（level: debug|info|warn|error；format: text|json）。
func Setup(level, format string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	} else {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	}
	slog.SetDefault(slog.New(h))
}

// ExitOnFail 打日志并退出进程（启动期不可恢复错误用）。
func ExitOnFail(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
