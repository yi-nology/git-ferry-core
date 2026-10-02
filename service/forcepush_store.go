package service

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// ===== force-push 审批（gitea-mirror block-and-approve 模式） =====
//
// force_push_policy=block 时，目标分歧触发的覆盖请求进入 pending 列表，
// Admin 一次性放行后允许本次 force。持久化在 force_push_approvals 表；
// 历史 <backup_dir>/force-push-approvals.json 在表为空时一次性导入（幂等）。

// ForcePushApproval 一条审批记录（JSON 契约与历史文件逐字兼容）。
type ForcePushApproval = model.ForcePushApproval

// ForcePushStore 审批存储，满足 executor.ForcePushApprover。
type ForcePushStore struct {
	dao *dao.ForcePushApprovalDAO
	// mu 序列化本实例内的读-改-写（RequestApproval 幂等判定）。
	mu sync.Mutex
	// legacy 旧 JSON 文件路径（仅迁移源）。
	legacy string
}

// NewForcePushStore 打开审批存储；表为空时从 legacy JSON 一次性导入。
func NewForcePushStore(d *dao.ForcePushApprovalDAO, legacyPath string) *ForcePushStore {
	st := &ForcePushStore{dao: d, legacy: legacyPath}
	st.migrateLegacy()
	return st
}

// ForcePushApprovals 返回 force-push 审批存储（NewService 按 backup_dir 定位旧文件迁移源）。
func (s *Service) ForcePushApprovals() *ForcePushStore { return s.approvals }

// migrateLegacy 旧文件在表为空时导入（幂等：只在空表执行）。
func (st *ForcePushStore) migrateLegacy() {
	if st.legacy == "" {
		return
	}
	n, err := st.dao.Count()
	if err != nil || n > 0 {
		return
	}
	data, rerr := os.ReadFile(st.legacy) //nolint:gosec // 内部状态文件
	if rerr != nil {
		return
	}
	var list []ForcePushApproval
	if len(data) == 0 || json.Unmarshal(data, &list) != nil {
		return
	}
	imported := 0
	for i := range list {
		if list[i].ID == "" {
			continue
		}
		if cerr := st.dao.Create(&list[i]); cerr != nil {
			slog.Warn("import legacy force-push approval failed", "id", list[i].ID, "error", cerr)
			continue
		}
		imported++
	}
	if imported > 0 {
		slog.Info("imported legacy force-push approvals into db", "count", imported, "path", st.legacy)
	}
}

// List 返回全部审批（按创建时间升序，与历史文件 append 顺序一致）。
func (st *ForcePushStore) List() []ForcePushApproval {
	list, err := st.dao.All()
	if err != nil {
		slog.Error("list force-push approvals failed", "error", err)
		return []ForcePushApproval{}
	}
	return list
}

// PendingCount 未放行条数。
func (st *ForcePushStore) PendingCount() int {
	n, err := st.dao.PendingCount()
	if err != nil {
		return 0
	}
	return int(n)
}

// Request 追加一条 pending 审批（HTTP 端点语义：不幂等，调用方校验入参）。
func (st *ForcePushStore) Request(taskKey, branch, reason string) (ForcePushApproval, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	item := ForcePushApproval{
		ID:        fmt.Sprintf("fp-%d", time.Now().UnixNano()),
		TaskKey:   taskKey,
		Branch:    branch,
		Reason:    reason,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.dao.Create(&item); err != nil {
		return ForcePushApproval{}, err
	}
	return item, nil
}

// RequestApproval 满足 executor.ForcePushApprover：同 task+branch 已有 pending 则跳过，
// 写失败 best-effort（不影响同步主流程）。
func (st *ForcePushStore) RequestApproval(taskKey, branch, reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	exists, err := st.dao.PendingExists(taskKey, branch)
	if err != nil || exists {
		return
	}
	_ = st.dao.Create(&ForcePushApproval{
		ID:        fmt.Sprintf("fp-%d", time.Now().UnixNano()),
		TaskKey:   taskKey,
		Branch:    branch,
		Reason:    reason,
		CreatedAt: time.Now().UTC(),
	})
}

// Approve 放行指定审批；未找到返回 found=false。
func (st *ForcePushStore) Approve(id, actor string) (found bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, gerr := st.dao.Get(id)
	if gerr != nil {
		return false, gerr
	}
	if a == nil {
		return false, nil
	}
	now := time.Now().UTC()
	a.Approved = true
	a.ApprovedBy = actor
	a.ApprovedAt = &now
	if err := st.dao.Update(a); err != nil {
		return true, err
	}
	return true, nil
}

// IsForcePushApproved 查询任务+分支是否已放行（executor 分歧保护回调）。
func (st *ForcePushStore) IsForcePushApproved(taskKey, branch string) bool {
	ok, err := st.dao.IsApproved(taskKey, branch)
	if err != nil {
		slog.Error("query force-push approval failed", "error", err)
		return false
	}
	return ok
}
