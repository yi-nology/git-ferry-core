package service

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/yi-nology/git-ferry-core/model"
)

// prepareRebuildWorkDir 在**拿到执行权之后**清空任务工作目录,让本次执行
// 从源重新克隆(等价"冷启动全量拉取")。
//
// 为什么必须放在 core 而不是调用方:
//   - workdir 按 taskKey 共享(service.GetTempDir)。调用方在受理阶段就删,
//     会和**正在跑的同步**抢同一个目录 —— 把跑着的工作树删掉,那次同步必炸。
//   - core 在拿到执行权(guard)之后删 ⇒ 同一个任务此刻没有别的执行在跑,
//     删了不会打断别人。
//   - 之前壳层的 POST /ops/rebuild 就是"先删目录,再受理",顺序正好反了:
//     任务正在跑时,目录已经被删掉了,而受理却因 ErrTaskRunning 失败。
//
// 只在 rebuild 触发器下生效(见 model.IsRebuildTrigger);其它触发器是增量同步,
// 保留工作区才有增量收益。
func (s *Service) prepareRebuildWorkDir(taskKey string) error {
	dir := s.GetTempDir(taskKey)
	if dir == "" {
		return fmt.Errorf("rebuild: 空的工作目录路径")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("rebuild: 清空工作目录失败: %w", err)
	}
	slog.Info("rebuild: 工作目录已清空,本次将从源重新克隆", "task", taskKey, "dir", dir)
	return nil
}

// runWithRebuildPrep 在执行前按触发器语义决定是否清空工作目录。
func (s *Service) runWithRebuildPrep(taskKey, trigger string) error {
	if !model.IsRebuildTrigger(trigger) {
		return nil
	}
	return s.prepareRebuildWorkDir(taskKey)
}
