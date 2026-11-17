package service

import (
	"context"
	"sync"
	"time"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/repository"
)

// OperationLogService 处理审计日志相关操作。
type OperationLogService struct {
	// mu 进程内串行化哈希链写入:sqlite 没有 SELECT ... FOR UPDATE,
	// 事务 + 单写者仍可能让两个 goroutine 同时读到旧链尾。
	mu              sync.Mutex
	opLogRepository *repository.OperationLogRepository
}

// NewOperationLogService 创建新的 OperationLogService 实例。
func NewOperationLogService(opLogRepository *repository.OperationLogRepository) *OperationLogService {
	return &OperationLogService{opLogRepository: opLogRepository}
}

// Record 记录一条审计日志(best-effort,由调用方决定如何处理错误)。
// 自动续接哈希链:EntryHash = SHA256(PrevHash || payload)。
func (s *OperationLogService) Record(ctx context.Context, entry *model.OperationLog) error {
	if entry.Status == "" {
		entry.Status = model.StatusSuccess
	}
	if entry.PrevHash == "" && entry.EntryHash == "" {
		// 时间先归一到毫秒**再**算哈希:哈希输入与入库值必须是同一个值。
		// (MySQL 的 datetime(3) 存不下纳秒,入库时被四舍五入 →
		//  校验时从库里读回来重算的哈希对不上 → 整条链误报被篡改。)
		//
		// 注意哈希算法本身**不能改**:这条链是既有的,
		// 改了算法会让历史记录全部对不上,等于毁掉过去的防篡改证据。
		// 归一放在写入侧,历史数据原样保留。
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = time.Now()
		}
		entry.CreatedAt = model.TruncateDBTime(entry.CreatedAt)
		// 锁定读 + 事务提交:链尾读取与插入是原子的,不再分叉。
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.opLogRepository.CreateChained(ctx, entry)
	}
	return s.opLogRepository.Create(entry)
}

// AuditChainResult 审计哈希链校验结果。
type AuditChainResult struct {
	OK       bool   `json:"ok"`
	Checked  int    `json:"checked"`
	BrokenAt uint   `json:"broken_at,omitempty"`
	Message  string `json:"message"`
}

// VerifyAuditChain 全量校验审计哈希链完整性。
func (s *OperationLogService) VerifyAuditChain() (*AuditChainResult, error) {
	logs, err := s.opLogRepository.ListAscending()
	if err != nil {
		return nil, err
	}
	res := &AuditChainResult{OK: true}
	prev := ""
	for _, e := range logs {
		if e.PrevHash != prev {
			res.OK = false
			res.BrokenAt = e.ID
			res.Message = "prev_hash mismatch"
			return res, nil
		}
		expect := model.ComputeEntryHash(e.PrevHash, e)
		if e.EntryHash != expect {
			res.OK = false
			res.BrokenAt = e.ID
			res.Message = "entry_hash mismatch (tampered?)"
			return res, nil
		}
		prev = e.EntryHash
		res.Checked++
	}
	res.Message = "audit chain intact"
	return res, nil
}

// List 按过滤条件分页返回审计日志。
func (s *OperationLogService) List(ctx context.Context, offset, limit int, filter *repository.OperationLogFilter) ([]*model.OperationLog, int64, error) {
	page := repository.DefaultPagination(offset, limit)
	return s.opLogRepository.List(page, filter)
}

// Stats 返回今日、近 7 天（本周）、总操作数。单次 SQL 查询。
func (s *OperationLogService) Stats(ctx context.Context) (today, week, total int64, err error) {
	now := time.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	r, err := s.opLogRepository.StatsOnce(startOfToday)
	if err != nil {
		return 0, 0, 0, err
	}
	return r.Today, r.Week, r.Total, nil
}
