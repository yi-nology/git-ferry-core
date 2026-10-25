package service

import "sync"

// pendingTriggerSet 记录「因为任务正在跑、没能立即触发」的待补跑触发。
//
// **场景**：webhook 推送到达时任务正在同步 ⇒ guard.Acquire 快速失败
// (ErrTaskRunning)⇒ **连运行记录都没有** ⇒ 自动重跑机制(RetryTracker,
// 只认 status=failed 的运行)看不到它 ⇒ **这次推送的提交被静默丢掉**
// (要等下一次推送或 cron 才补上)。
//
// **修法**：把这类触发**合并**记下来,当前运行结束、释放执行权之后补跑一次。
// 合并语义:同一任务积压多次只补跑一次 —— 补跑是"再同步一次",
// 会把期间到达的所有提交一起带上,不需要按次数补。
//
// 进程内状态(与 guard 的进程内模式一致):重启后未补跑的触发会丢,
// 但对应的 webhook 事件已被标成 failed ⇒ 可见、可人工重试。
type pendingTriggerSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func newPendingTriggerSet() *pendingTriggerSet {
	return &pendingTriggerSet{m: make(map[string]struct{})}
}

// Mark 记一笔待补跑(同一任务重复记只算一笔)。
func (p *pendingTriggerSet) Mark(taskKey string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[string]struct{})
	}
	p.m[taskKey] = struct{}{}
}

// Take 取走一笔待补跑;返回是否命中。
func (p *pendingTriggerSet) Take(taskKey string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.m[taskKey]; !ok {
		return false
	}
	delete(p.m, taskKey)
	return true
}

// Len 当前待补跑的任务数。
func (p *pendingTriggerSet) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}
