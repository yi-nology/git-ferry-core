package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/loop"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== 持久化执行队列(P2 分布式地基,见 model.ExecutionJob) =====
//
// 触发(RunTaskAsync/RunTaskWithTrigger)在 durable_queue=true 时不再直接
// 执行,而是入队;本实例(execute_jobs!=false 时)的派发器按 lease 认领:
//
//	触发 → 入队(同任务去重) → 派发器 Claim(原子单胜者)
//	     → 执行(续租;失锁即中止) → 完成写终态
//	          ↑________(lease 过期)reaper 放回 pending(attempt+1),接管重试
//	执行权被占(良性繁忙)⇒ 退避放回 pending,attempt 不增(不烧毒丸预算)
//
// 收益(对应评审 P2 验收):实例崩溃/被 K8s 驱逐 ⇒ 作业不丢,lease 过期后
// 其它实例安全接管;并发/重复触发 ⇒ 入队去重合并不重复发布;纯 API 实例
// 配 execute_jobs=false 即可只入队不执行(Worker 化的第一步,同二进制)。

const (
	// jobLeaseTTL 执行权租约:覆盖一次克隆+推送的最长耗时;过期可被接管。
	jobLeaseTTL = 5 * time.Minute
	// jobRenewInterval 续租周期 = TTL/3,留足网络抖动余量。
	jobRenewInterval = 90 * time.Second
	// jobDispatchInterval 派发器轮询周期。
	jobDispatchInterval = 3 * time.Second
	// jobReaperInterval 过期租约接管周期。
	jobReaperInterval = 30 * time.Second
	// jobClaimBatch 单轮认领上限(防一次吞太多)。
	jobClaimBatch = 8
	// jobBusyBackoff 执行权被占时的退避(不丢作业,防派发忙旋)。
	jobBusyBackoff = 30 * time.Second
	// jobFinishedRetention 已完成作业的保留期。
	jobFinishedRetention = 7 * 24 * time.Hour
)

// durableQueueOn 是否启用持久化执行队列。
func (s *Service) durableQueueOn() bool {
	return s.config != nil && s.config.Sync.DurableQueue
}

// executesJobs 本实例是否派发并执行队列任务(nil=true:API/Worker 合一;
// false=只入队的纯 API/调度实例)。
func (s *Service) executesJobs() bool {
	if !s.durableQueueOn() {
		return true
	}
	return s.config.Sync.ExecuteJobs == nil || *s.config.Sync.ExecuteJobs
}

// enqueueTaskRun 队列模式的触发入口:入队(同任务合并语义)。
func (s *Service) enqueueTaskRun(taskKey, trigger string, webhookEventID *uint) error {
	if s.jobs == nil {
		// fail-closed:队列开启但 DAO 没接线(裸构造)——拒绝而不是静默直跑,
		// 否则多实例下去重语义 silently 退化成双执行。
		return oekiterrors.New("durable queue enabled but execution job DAO not wired")
	}
	enqueued, err := s.jobs.Enqueue(taskKey, trigger, webhookEventID)
	if err != nil {
		return err
	}
	if enqueued {
		slog.Info("执行任务已入队", "task", taskKey, "trigger", trigger)
	} else {
		slog.Info("触发已合并:任务已有待执行作业", "task", taskKey, "trigger", trigger)
	}
	return nil
}

// newInstanceID 实例标识(hostname-pid-随机):lease 认领/续租/完成的属主判据。
func newInstanceID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// dispatchJobLoop 派发器:周期认领并执行待处理作业。
func (s *Service) dispatchJobLoop() {
	loop.Run(s.bgCtx, loop.Config{
		Interval:   jobDispatchInterval,
		FirstDelay: 0, // 启动立即派一轮:重启接管遗留作业
		Name:       "dispatch-execution-jobs",
	}, func(ctx context.Context) {
		s.dispatchOnce()
	}).Wait()
}

// dispatchOnce 认领一批 pending 作业并异步执行。
func (s *Service) dispatchOnce() {
	if s.jobs == nil {
		return
	}
	ids, err := s.jobs.ListPendingIDs(jobClaimBatch)
	if err != nil {
		slog.Error("job: 查询待执行作业失败", "error", err)
		return
	}
	for _, id := range ids {
		claimed, err := s.jobs.Claim(id, s.instanceID, jobLeaseTTL)
		if err != nil {
			slog.Error("job: 认领失败", "id", id, "error", err)
			continue
		}
		if !claimed {
			continue // 被其它实例抢先,正常竞争
		}
		job, err := s.jobs.FindByID(id)
		if err != nil {
			slog.Error("job: 认领后读取失败", "id", id, "error", err)
			continue
		}
		s.executeJob(*job)
	}
}

// executeJob 执行一个已认领作业:执行权互斥(双保险,跨模式混部也安全)→
// rebuild 准备 → 执行 → finishRun(先 release 再通知,outbox/pending 闭环)。
//
// panic 安全:GoSafe  recover 后作业停留在 claimed,lease 过期由 reaper
// 接管重试(attempt+1)——宁可慢,不可丢。
func (s *Service) executeJob(job model.ExecutionJob) {
	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()

		release, err := s.guard.Acquire(s.bgCtx, job.TaskKey)
		if err != nil {
			// 执行权被占(本实例或跨实例有执行在跑)——良性繁忙:退避重排,
			// 作业放回 pending 且 attempt 不增(毒丸预算只留给崩溃接管)。
			slog.Warn("job: 执行权被占,退避重排(不占接管预算)", "id", job.ID, "task", job.TaskKey, "error", err)
			if rerr := s.jobs.Reschedule(job.ID, s.instanceID, jobBusyBackoff); rerr != nil {
				if errors.Is(rerr, dao.ErrLeaseLost) {
					slog.Warn("job: 退避重排时已失锁(作业已被其它实例接管),本实例停手", "id", job.ID)
					return
				}
				slog.Error("job: 退避重排失败", "id", job.ID, "error", rerr)
			}
			return
		}

		task, terr := s.tasks.FindTaskByKey(job.TaskKey)
		if terr != nil || task == nil {
			s.failJobAndRelease(job, release, ErrTaskNotFound)
			return
		}
		if !task.Enabled {
			s.failJobAndRelease(job, release, ErrTaskDisabled)
			return
		}
		// rebuild 语义:拿到执行权之后再清工作目录(同直跑路径)。
		if err := s.runWithRebuildPrep(job.TaskKey, job.Trigger); err != nil {
			s.failJobAndRelease(job, release, err)
			return
		}

		// 执行用可取消 ctx:续租发现失锁 ⇒ 取消,旧属主立即停手(防与
		// 接管方并发推同一目标仓库 —— 脑裂分叉推送)。
		execCtx, cancelExec := context.WithCancel(s.bgCtx)
		defer cancelExec()
		stopRenew := make(chan struct{})
		go s.renewLeaseLoop(job.ID, stopRenew, cancelExec)
		run, execErr := s.executor.Execute(execCtx, task, job.Trigger, job.WebhookEventID)
		close(stopRenew)

		s.finishRun(job.TaskKey, job.Trigger, run, execErr, release)
		var runID uint
		if run != nil {
			runID = run.ID
		}
		if cerr := s.jobs.Complete(job.ID, s.instanceID, runID, execErr); cerr != nil {
			if errors.Is(cerr, dao.ErrLeaseLost) {
				slog.Error("job: 完成落库时已失锁(执行结果未入终态,作业已被其它实例接管重试)", "id", job.ID)
				return
			}
			slog.Error("job: 完成落库失败", "id", job.ID, "error", cerr)
		}
	})
}

// failJobAndRelease 执行前失败(任务不存在/被禁用/准备工作失败):
// 不放行执行权后直接失败终态 —— 这类失败重试无意义,重放会被毒丸拦。
func (s *Service) failJobAndRelease(job model.ExecutionJob, release func(), cause error) {
	release()
	slog.Warn("job: 执行前失败,作业判失败", "id", job.ID, "task", job.TaskKey, "error", cause)
	if cerr := s.jobs.Complete(job.ID, s.instanceID, 0, cause); cerr != nil {
		if errors.Is(cerr, dao.ErrLeaseLost) {
			slog.Warn("job: 失败落库时已失锁(作业已被其它实例接管重试)", "id", job.ID)
			return
		}
		slog.Error("job: 失败落库失败", "id", job.ID, "error", cerr)
	}
}

// renewLeaseLoop 执行中周期续租,直到 stop 关闭或实例退出;
// 续租发现失锁(ErrLeaseLost)⇒ Error 告警 + 取消执行 ctx(onLost),
// 旧属主不得无感继续推送。
func (s *Service) renewLeaseLoop(id uint, stop <-chan struct{}, onLost func()) {
	tick := time.NewTicker(jobRenewInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.bgCtx.Done():
			return
		case <-tick.C:
			if !s.renewLeaseTick(id, onLost) {
				return
			}
		}
	}
}

// renewLeaseTick 续租一次:返回是否继续续租循环(false = 已失锁,该停手)。
func (s *Service) renewLeaseTick(id uint, onLost func()) bool {
	err := s.jobs.RenewLease(id, s.instanceID, jobLeaseTTL)
	switch {
	case err == nil:
		return true
	case errors.Is(err, dao.ErrLeaseLost):
		// 失锁:执行权已被其它实例接管,继续执行 = 脑裂分叉推送。
		slog.Error("job: 续租发现已失锁,中止本次执行", "id", id, "error", err)
		if onLost != nil {
			onLost()
		}
		return false
	default:
		slog.Warn("job: 续租失败(lease 过期将被接管重试)", "id", id, "error", err)
		return true
	}
}

// reaperJobLoop 过期租约接管 + 已完成作业清理。
func (s *Service) reaperJobLoop() {
	loop.Run(s.bgCtx, loop.Config{
		Interval:   jobReaperInterval,
		FirstDelay: jobReaperInterval,
		Name:       "reap-execution-jobs",
	}, func(ctx context.Context) {
		s.reapExpiredJobs()
	}).Wait()
}

// reapExpiredJobs claimed 且 lease 过期 ⇒ 放回 pending(attempt+1,其它实例
// 可接管);attempt 超限 ⇒ 判 failed 毒丸。对应评审验收:
// 「Worker 执行时被 K8s 驱逐 ⇒ Lease 到期后可安全接管」。
func (s *Service) reapExpiredJobs() {
	if s.jobs == nil {
		return
	}
	requeued, poisoned, err := s.jobs.RequeueExpired(time.Now())
	if err != nil {
		slog.Error("job: 接管过期作业失败", "error", err)
		return
	}
	if requeued > 0 || poisoned > 0 {
		slog.Info("job: 接管过期作业", "requeued", requeued, "poisoned", poisoned)
	}
	if _, err := s.jobs.CleanupFinished(context.Background(), jobFinishedRetention); err != nil {
		slog.Warn("job: 清理已完成作业失败", "error", err)
	}
}
