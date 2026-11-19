package service

import (
	"context"
	"time"

	"github.com/oekit/logx"

	"github.com/oekit/concurrency/loop"
)

// RecoverStuckMirrorRuns 回收卡在进行中(pending/running)的镜像执行记录,标成 failed。
//
// 回归背景(P0,"通道永久占用"):ExecuteMirrorRun 受理时会查
// `HasRunningByChannel`(DB 里有没有在跑的 run)—— 那是**跨进程/重启后的兜底**,
// 但 DB 里的僵尸 run(进程被 SIGKILL / 崩溃 / OOM 杀掉)没人回收:
// 状态永远停在 pending|running ⇒ 该通道**永久**报
// "该通道有执行中的任务,请稍后再试",再也无法触发任何发布/校验。
//
// 回收成 failed ⇒ 通道可再次触发,且历史里能看出"这次没跑完"(可人工重试)。
//
// 只回收**超过 olderThan** 的:正在跑的长任务不该被误伤。
func (m *MirrorService) RecoverStuckMirrorRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	n, err := m.runs.MarkStuckFailed(olderThan,
		"执行中断(进程重启或超时),已回收,可重新触发")
	if err != nil {
		return 0, err
	}
	if n > 0 {
		logx.Warn("已批量回收卡住的镜像执行", "count", n)
	}
	return n, nil
}

// recoverStuckMirrorRuns 周期回收卡住的镜像执行记录。
//
// 阈值取 1 小时:一次大仓库的镜像发布可能跑很久,不能误伤正在跑的任务。
func (s *Service) recoverStuckMirrorRuns() {
	const interval = 10 * time.Minute
	const stuckTimeout = 1 * time.Hour
	loop.Run(s.bgCtx, loop.Config{
		Interval:   interval,
		FirstDelay: interval,
		Name:       "recover-stuck-mirror-runs",
	}, func(ctx context.Context) {
		if s.mirror == nil {
			return
		}
		n, err := s.mirror.RecoverStuckMirrorRuns(ctx, stuckTimeout)
		if err != nil {
			logx.Error("回收卡住的镜像执行失败", "error", err)
			return
		}
		if n > 0 {
			logx.Info("已回收卡住的镜像执行", "count", n)
		}
	}).Wait()
}
