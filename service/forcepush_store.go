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
func (st *ForcePushStore) Request(taskKey, branch, reason, targetRef string) (ForcePushApproval, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	item := ForcePushApproval{
		ID:        fmt.Sprintf("fp-%d", time.Now().UnixNano()),
		TaskKey:   taskKey,
		Branch:    branch,
		Reason:    reason,
		TargetRef: targetRef,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.dao.Create(&item); err != nil {
		return ForcePushApproval{}, err
	}
	return item, nil
}

// RequestApproval 满足 executor.ForcePushApprover：同 task+branch 已有 pending 则跳过，
// 写失败 best-effort（不影响同步主流程）。
func (st *ForcePushStore) RequestApproval(taskKey, branch, reason, targetRef string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// 已有 pending 时**刷新**它(reason / target_ref / created_at),而不是跳过。
	//
	// 跳过会有个隐蔽的后果:pending 是"针对目标某个状态"的申请,
	// 目标侧此后又变了(独有提交从 1 个变成 50 个)时,旧的 pending 仍挂着、
	// 绑的还是**旧状态** ⇒ 运维批的是一份过期的描述。
	// 刷新后,待批的永远是"当前这一次分歧"。
	existing, err := st.dao.FindPending(taskKey, branch)
	if err == nil && existing != nil {
		existing.Reason = reason
		existing.TargetRef = targetRef
		existing.CreatedAt = time.Now().UTC()
		_ = st.dao.Update(existing)
		return
	}
	_ = st.dao.Create(&ForcePushApproval{
		ID:        fmt.Sprintf("fp-%d", time.Now().UnixNano()),
		TaskKey:   taskKey,
		Branch:    branch,
		Reason:    reason,
		TargetRef: targetRef,
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

// IsForcePushApproved 查询任务+分支是否已放行**且尚未被用掉**（executor 分歧保护回调）。
func (st *ForcePushStore) IsForcePushApproved(taskKey, branch, targetRef string) bool {
	ok, err := st.dao.IsApproved(taskKey, branch, targetRef)
	if err != nil {
		slog.Error("query force-push approval failed", "error", err)
		return false
	}
	return ok
}

// ConsumeForcePushApproval 消费掉一次放行（放行是一次性的）。
//
// 契约:"Admin 一次性放行后允许**本次** force" —— 用掉之后,同一任务同一分支
// 再出现分歧必须重新审批。记录保留（标 consumed_at）,留住审计线索。
func (st *ForcePushStore) ConsumeForcePushApproval(taskKey, branch string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	found, err := st.dao.ConsumeApproved(taskKey, branch, time.Now().UTC())
	if err != nil {
		slog.Error("consume force-push approval failed", "error", err, "taskKey", taskKey, "branch", branch)
		return
	}
	if found {
		slog.Info("force-push approval consumed", "taskKey", taskKey, "branch", branch)
	}
}

// Revoke 撤销一条放行(放行不再生效)。
//
// 为什么需要:放行是"人工授权覆盖目标侧独有提交",批错了要在**兑现之前**收回;
// 而且已经放行、但因为别的原因还没推成功时,授权也不该继续挂着。
// 没有撤销口子的话,唯一的"取消"手段是等它被用掉 —— 那正是最不该覆盖的时刻。
func (st *ForcePushStore) Revoke(id, actor string) (found bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	found, err = st.dao.Revoke(id, actor, time.Now().UTC())
	if err != nil {
		return false, err
	}
	if found {
		slog.Info("force-push approval revoked", "id", id, "actor", actor)
	}
	return found, nil
}
