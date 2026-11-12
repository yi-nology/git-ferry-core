package service

import (
	"context"
	"log/slog"

	"github.com/oekit/concurrency/async"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 待补跑触发(webhook 合并) =====
//
// **场景**：webhook 推送到达时任务正在同步 ⇒ guard.Acquire 快速失败
// (ErrTaskRunning)⇒ **连运行记录都没有** ⇒ 自动重跑机制(RetryTracker,
// 只认 status=failed 的运行)看不到它 ⇒ **这次推送的提交被静默丢掉**
// (要等下一次推送或 cron 才补上)。
//
// **修法**：把这类触发**合并**记入 pending_triggers 表,当前运行结束、
// 释放执行权之后补跑一次。合并语义:同一任务积压多次只补跑一次 ——
// 补跑是"再同步一次",会把期间到达的所有提交一起带上,不需要按次数补。
//
// **为什么落库**(P0 修复,替代旧进程内 map):
//  1. 双执行路径都能消费:旧实现只有 RunTaskWithTrigger 收尾消费,
//     RunTaskAsync(手动/重试/auto_retry)跑完不消费 ⇒ 手动执行期间积压的
//     webhook 永远补不上;
//  2. 跨实例可认领:Claim=原子 DELETE,任意实例执行完都能补;
//  3. 重启不丢:启动时 ClaimAll 扫描补跑。

// markPendingWebhook 记一笔待补跑(同任务重复记合并,generation+1)。
func (s *Service) markPendingWebhook(taskKey string) {
	if s.pending == nil {
		return
	}
	if err := s.pending.Upsert(taskKey); err != nil {
		slog.Error("待补跑触发落库失败", "taskKey", taskKey, "error", err)
		return
	}
	slog.Info("任务正在执行,webhook 触发已合并记入待补跑", "taskKey", taskKey)
}

// runPendingWebhook 认领并补跑一笔待补跑触发。
// 双执行路径(RunTaskWithTrigger / RunTaskAsync)收尾都会调用;
// 认领失败不补跑(下次运行结束再试),认领成功才发起补跑。
func (s *Service) runPendingWebhook(taskKey string) {
	if s.pending == nil {
		return
	}
	claimed, err := s.pending.Claim(taskKey)
	if err != nil {
		slog.Error("待补跑认领失败", "taskKey", taskKey, "error", err)
		return
	}
	if !claimed {
		return
	}
	s.triggerPendingCatchup(taskKey)
}

// sweepPendingTriggersOnStartup 启动时认领并补跑全部遗留待补跑触发。
// 多实例同时启动双认领的窗口由补跑侧执行权互斥兜底(撞忙重新记账,最终收敛)。
func (s *Service) sweepPendingTriggersOnStartup() {
	if s.pending == nil {
		return
	}
	keys, err := s.pending.ClaimAll()
	if err != nil {
		slog.Warn("启动扫描待补跑触发失败", "error", err)
		return
	}
	for _, key := range keys {
		slog.Info("启动补跑遗留的 webhook 触发", "taskKey", key)
		s.triggerPendingCatchup(key)
	}
}

// triggerPendingCatchup 发起一次补跑(已认领待补跑之后的实际触发)。
// 补跑走 RunTaskWithTrigger(trigger=webhook):若执行权被占(另一实例正在跑),
// 它内部的 ErrTaskRunning 分支会重新记账 ⇒ 最终收敛,不丢触发。
func (s *Service) triggerPendingCatchup(taskKey string) {
	ctx := s.bgCtx
	if ctx == nil {
		ctx = context.Background()
	}
	async.GoSafe(func() {
		if err := s.RunTaskWithTrigger(ctx, taskKey, model.TriggerWebhook, nil); err != nil {
			slog.Error("补跑被合并的 webhook 触发失败", "taskKey", taskKey, "error", err)
		}
	})
}
