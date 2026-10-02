package service

import (
	"log/slog"
	"sync"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 运行完成事件 =====
//
// 此前壳层只能用 ListHistory 轮询当事件源（runwatch 自述「core 不暴露完成回调」）；
// 本文件提供进程内订阅：RunTaskAsync / RunTaskWithTrigger 执行收尾时广播。

// RunEvent 一次运行结束的事件。
type RunEvent struct {
	TaskKey string
	// Run 完成态快照（CreateRun 失败时为 nil）。
	Run     *model.SyncRun
	Trigger string
	// Err Execute 返回的错误（结果本身以 Run.Status 为准）。
	Err error
	At  time.Time
}

// runSubscriber 订阅者：每订阅者独立 goroutine 分发，慢回调不阻塞同步主流程。
type runSubscriber struct {
	id uint64
	fn func(RunEvent)
}

// SubscribeRuns 注册运行完成回调，返回取消函数（幂等）。
// 回调在独立 goroutine 中执行并 recover panic；调用方需自行保证非阻塞语义
// （要串行处理就自己起 worker）。Stop/进程退出不等待订阅者。
func (s *Service) SubscribeRuns(fn func(RunEvent)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	s.runSubMu.Lock()
	s.runSubID++
	id := s.runSubID
	s.runSubs = append(s.runSubs, runSubscriber{id: id, fn: fn})
	s.runSubMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.runSubMu.Lock()
			defer s.runSubMu.Unlock()
			for i := range s.runSubs {
				if s.runSubs[i].id == id {
					s.runSubs = append(s.runSubs[:i], s.runSubs[i+1:]...)
					break
				}
			}
		})
	}
}

// emitRunCompleted 广播一次运行结束（快照订阅者后异步分发）。
func (s *Service) emitRunCompleted(taskKey, trigger string, run *model.SyncRun, err error) {
	s.runSubMu.Lock()
	if len(s.runSubs) == 0 {
		s.runSubMu.Unlock()
		return
	}
	subs := make([]runSubscriber, len(s.runSubs))
	copy(subs, s.runSubs)
	s.runSubMu.Unlock()

	ev := RunEvent{TaskKey: taskKey, Run: run, Trigger: trigger, Err: err, At: time.Now()}
	for _, sub := range subs {
		go func(fn func(RunEvent)) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("run subscriber panic recovered", "panic", r)
				}
			}()
			fn(ev)
		}(sub.fn)
	}
}
