package service

import (
	"context"
	"time"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// OperationLogService 处理审计日志相关操作。
type OperationLogService struct {
	opLogDAO *dao.OperationLogDAO
}

// NewOperationLogService 创建新的 OperationLogService 实例。
func NewOperationLogService(opLogDAO *dao.OperationLogDAO) *OperationLogService {
	return &OperationLogService{opLogDAO: opLogDAO}
}

// Record 记录一条审计日志(best-effort,由调用方决定如何处理错误)。
// 自动续接哈希链:EntryHash = SHA256(PrevHash || payload)。
func (s *OperationLogService) Record(ctx context.Context, entry *model.OperationLog) error {
	if entry.Status == "" {
		entry.Status = model.StatusSuccess
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.PrevHash == "" && entry.EntryHash == "" {
		prev, err := s.opLogDAO.Latest()
		if err == nil && prev != nil {
			entry.PrevHash = prev.EntryHash
		}
		entry.EntryHash = model.ComputeEntryHash(entry.PrevHash, entry)
	}
	return s.opLogDAO.Create(entry)
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
	logs, err := s.opLogDAO.ListAscending()
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
func (s *OperationLogService) List(ctx context.Context, offset, limit int, filter *dao.OperationLogFilter) ([]*model.OperationLog, int64, error) {
	page := dao.DefaultPagination(offset, limit)
	return s.opLogDAO.List(page, filter)
}

// Stats 返回今日、近 7 天（本周）、总操作数。单次 SQL 查询。
func (s *OperationLogService) Stats(ctx context.Context) (today, week, total int64, err error) {
	now := time.Now()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	r, err := s.opLogDAO.StatsOnce(startOfToday)
	if err != nil {
		return 0, 0, 0, err
	}
	return r.Today, r.Week, r.Total, nil
}
