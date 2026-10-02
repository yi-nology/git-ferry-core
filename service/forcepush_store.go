package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ===== force-push 审批（gitea-mirror block-and-approve 模式） =====
//
// force_push_policy=block 时，目标分歧触发的覆盖请求进入 pending 列表，
// Admin 一次性放行后允许本次 force。存储在 <backup_dir>/force-push-approvals.json
// （backup_dir 未配置时回落 data/），字段与历史壳层文件逐字兼容。

// ForcePushApproval 一条审批记录。
type ForcePushApproval struct {
	ID         string     `json:"id"`
	TaskKey    string     `json:"task_key"`
	Branch     string     `json:"branch"`
	Reason     string     `json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	Approved   bool       `json:"approved"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

// ForcePushStore 文件型审批存储，满足 executor.ForcePushApprover。
type ForcePushStore struct {
	path string
	mu   sync.Mutex
}

// ForcePushApprovals 返回 force-push 审批存储（NewService 按 backup_dir 定位文件）。
func (s *Service) ForcePushApprovals() *ForcePushStore { return s.approvals }

// NewForcePushStore 打开审批存储（文件不存在=空列表）。
func NewForcePushStore(path string) *ForcePushStore {
	return &ForcePushStore{path: path}
}

// Path 审批文件路径。
func (st *ForcePushStore) Path() string { return st.path }

func (st *ForcePushStore) load() []ForcePushApproval {
	data, err := os.ReadFile(st.path) //nolint:gosec // 内部状态文件
	if err != nil {
		return []ForcePushApproval{}
	}
	var list []ForcePushApproval
	if json.Unmarshal(data, &list) != nil {
		return []ForcePushApproval{}
	}
	return list
}

func (st *ForcePushStore) save(list []ForcePushApproval) error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(st.path, data, 0o600)
}

// List 返回全部审批（文件不可读时返回空列表）。
func (st *ForcePushStore) List() []ForcePushApproval {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.load()
}

// PendingCount 未放行条数。
func (st *ForcePushStore) PendingCount() int {
	n := 0
	for _, a := range st.List() {
		if !a.Approved {
			n++
		}
	}
	return n
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
	list := st.load()
	list = append(list, item)
	if err := st.save(list); err != nil {
		return ForcePushApproval{}, err
	}
	return item, nil
}

// RequestApproval 满足 executor.ForcePushApprover：同 task+branch 已有 pending 则跳过，
// 写失败 best-effort（不影响同步主流程）。
func (st *ForcePushStore) RequestApproval(taskKey, branch, reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	list := st.load()
	for _, a := range list {
		if a.TaskKey == taskKey && a.Branch == branch && !a.Approved {
			return
		}
	}
	list = append(list, ForcePushApproval{
		ID:        fmt.Sprintf("fp-%d", time.Now().UnixNano()),
		TaskKey:   taskKey,
		Branch:    branch,
		Reason:    reason,
		CreatedAt: time.Now().UTC(),
	})
	_ = st.save(list)
}

// Approve 放行指定审批；未找到返回 found=false。
func (st *ForcePushStore) Approve(id, actor string) (found bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	list := st.load()
	now := time.Now().UTC()
	for i := range list {
		if list[i].ID != id {
			continue
		}
		list[i].Approved = true
		list[i].ApprovedBy = actor
		list[i].ApprovedAt = &now
		if err := st.save(list); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, nil
}

// IsForcePushApproved 查询任务+分支是否已放行（executor 分歧保护回调）。
func (st *ForcePushStore) IsForcePushApproved(taskKey, branch string) bool {
	for _, a := range st.List() {
		if a.TaskKey == taskKey && a.Branch == branch && a.Approved {
			return true
		}
	}
	return false
}
