// Package proclog 进程级日志初始化与失败退出。
//
// 公网壳（git-ferry）与内网壳（git-ferry-intranet）此前各自复制了一份逐字相同的
// 实现；两者都依赖 core，故收敛到这里。core 自身不调用本包（库不应决定宿主进程
// 的日志与退出行为）。
package proclog

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/logx"
)

// Setup 按 level/format 配置默认 slog（level: debug|info|warn|error；format: text|json），
// 并把进程内 oekit 组件接到同一套输出：
//   - async.GoSafe 恢复的 panic 经 slog.Error 记录（库默认走标准库 log，格式不一致）；
//   - oekit 组件内部日志（如 distlock 续期告警）经 logx 门面输出，service 字段取可执行
//     文件名，级别与 level 对齐。logx 的 slog 后端自带 JSON 输出、不经过 slog handler，
//     因此 format=text 时这部分仍是 JSON 行。
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

	async.SetPanicHandler(func(msg string, stack []byte) {
		slog.Error("goroutine panic recovered", "panic", msg, "stack", string(stack))
	})

	l := logx.NewSlogLogger(filepath.Base(os.Args[0]))
	if lx := logx.ParseLogLevel(level); lx != nil {
		l.SetLevel(*lx)
	}
	logx.SetDefault(l)
}

// ExitOnFail 打日志并退出进程（启动期不可恢复错误用）。
func ExitOnFail(msg string, err error) {
	slog.Error(msg, "error", err)
	os.Exit(1)
}
