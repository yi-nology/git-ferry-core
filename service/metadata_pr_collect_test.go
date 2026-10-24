package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

// stateFilterCRProv 模拟**真实平台**的列表端点：
// ① 按状态过滤(不传状态 = 只返回 open,与 GitHub/Gitea/Forgejo 的默认一致);
// ② 真的分页(每页 PerPage 条)。
//
// 这两条正是回归的根因所在,所以桩必须复现它们 ——
// 否则测试会用一个"什么状态都给、一次给全"的假平台,把缺陷盖住。
type stateFilterCRProv struct {
	sdkprov.Provider
	prs []*sdkprov.ChangeRequest
	mu  sync.Mutex
	// calls 记录每次 ListCRs 的入参,用于断言"状态两侧都查了""真的翻页了"。
	calls []sdkprov.ListCROptions
}

func (p *stateFilterCRProv) ListCRs(_ context.Context, opts sdkprov.ListCROptions) ([]*sdkprov.ChangeRequest, *int, error) {
	p.mu.Lock()
	p.calls = append(p.calls, opts)
	p.mu.Unlock()

	var out []*sdkprov.ChangeRequest
	for _, pr := range p.prs {
		switch opts.State {
		case sdkprov.CRStateOpened:
			if pr.State == sdkprov.CRStateOpened {
				out = append(out, pr)
			}
		case sdkprov.CRStateClosed:
			// GitHub/Gitea 的 closed 侧同时覆盖"已关闭"与"已合并"
			if pr.State == sdkprov.CRStateClosed || pr.State == sdkprov.CRStateMerged {
				out = append(out, pr)
			}
		default:
			// **空状态 = 只返回 open** —— 这正是 GitHub/Gitea/Forgejo 的默认。
			// 桩必须复刻它,否则"漏掉 closed PR"这个缺陷会被假平台盖住。
			if pr.State == sdkprov.CRStateOpened {
				out = append(out, pr)
			}
		}
	}
	total := len(out)
	page := opts.Page
	if page <= 0 {
		page = 1
	}
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	start := (page - 1) * perPage
	if start >= len(out) {
		return nil, &total, nil
	}
	end := min(start+perPage, len(out))
	return out[start:end], &total, nil
}

func (p *stateFilterCRProv) seenStates() []sdkprov.CRState {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []sdkprov.CRState
	for _, c := range p.calls {
		out = append(out, c.State)
	}
	return out
}

func crAt(n int, title string, state sdkprov.CRState, updated time.Time) *sdkprov.ChangeRequest {
	return &sdkprov.ChangeRequest{
		Number:    strconv.Itoa(n),
		Title:     title,
		State:     state,
		UpdatedAt: updated,
	}
}

// newPRCollectSnap 造一个可落盘的快照上下文。
func newPRCollectSnap(t *testing.T) *MetadataSnapshot {
	t.Helper()
	return &MetadataSnapshot{
		Dir:    t.TempDir(),
		Counts: map[string]int32{},
	}
}

func collectPRs(t *testing.T, prov sdkprov.Provider, maxItems int) *MetadataSnapshot {
	t.Helper()
	svc := &Service{}
	snap := newPRCollectSnap(t)
	svc.collectPullRequests(context.Background(), prov, &model.Repo{
		PlatformOwner: "o", PlatformRepo: "r",
	}, maxItems, snap)
	return snap
}

// TestCollectPullRequests_IncludesClosedAndMerged 快照必须包含**已关闭/已合并**的 PR。
//
// 回归背景:列表端点按状态过滤,不传状态 = 只返回 open ⇒ 已合并/已关闭的 PR
// 被**静默丢弃** —— 真实仓里那是绝大多数。后果:快照不完整(灾备回灌缺历史),
// 而且回灌侧的「PR 状态跟着回灌」根本无从触发(快照里压根没有 closed/merged 的 PR)。
func TestCollectPullRequests_IncludesClosedAndMerged(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prov := &stateFilterCRProv{prs: []*sdkprov.ChangeRequest{
		crAt(1, "open one", sdkprov.CRStateOpened, base),
		crAt(2, "closed one", sdkprov.CRStateClosed, base.Add(time.Hour)),
		crAt(3, "merged one", sdkprov.CRStateMerged, base.Add(2*time.Hour)),
	}}

	snap := collectPRs(t, prov, 500)

	var got []*sdkprov.ChangeRequest
	data, err := os.ReadFile(filepath.Join(snap.Dir, "pull_requests.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &got))

	numbers := map[string]bool{}
	for _, pr := range got {
		numbers[pr.Number] = true
	}
	assert.True(t, numbers["1"], "open PR 必须收")
	assert.True(t, numbers["2"], "closed PR 必须收(原来被静默丢弃)")
	assert.True(t, numbers["3"], "merged PR 必须收(原来被静默丢弃)")
	assert.EqualValues(t, 3, snap.Counts["pull_requests"])

	// 两侧状态都必须查过
	states := prov.seenStates()
	assert.Contains(t, states, sdkprov.CRStateOpened)
	assert.Contains(t, states, sdkprov.CRStateClosed)
}

// TestCollectPullRequests_Paginates 上限 500 就该真翻页 —— 列表端点每页上限是 100。
//
// 回归背景:原来一次性 `PerPage: maxItems`,被 SDK 截成 100 且只取第 1 页 ⇒
// "上限 500"实际只拿到 100 条,而且**没有任何告警**。
func TestCollectPullRequests_Paginates(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var prs []*sdkprov.ChangeRequest
	for i := 1; i <= 120; i++ {
		prs = append(prs, crAt(i, "pr", sdkprov.CRStateOpened, base.Add(time.Duration(i)*time.Minute)))
	}
	prov := &stateFilterCRProv{prs: prs}

	snap := collectPRs(t, prov, 500)

	assert.EqualValues(t, 120, snap.Counts["pull_requests"], "120 条 open PR 必须全部收到(原来只拿 100 条)")

	// 真的翻页了:至少 3 页(每页 50)
	pages := map[int]bool{}
	prov.mu.Lock()
	for _, c := range prov.calls {
		pages[c.Page] = true
	}
	prov.mu.Unlock()
	assert.True(t, pages[1] && pages[2] && pages[3], "必须翻到第 3 页,实际页码=%v", pages)
}

// TestCollectPullRequests_CapsByUpdatedAt 截断必须按**更新时间**取最近的,
// 且 open/closed 两侧一起参与 —— 否则 open 侧顶满上限就把 closed 侧整批挤掉。
func TestCollectPullRequests_CapsByUpdatedAt(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prov := &stateFilterCRProv{prs: []*sdkprov.ChangeRequest{
		crAt(1, "open old", sdkprov.CRStateOpened, base),
		crAt(2, "open new", sdkprov.CRStateOpened, base.Add(3*time.Hour)),
		crAt(3, "closed mid", sdkprov.CRStateClosed, base.Add(2*time.Hour)),
		crAt(4, "closed new", sdkprov.CRStateClosed, base.Add(4*time.Hour)),
	}}

	snap := collectPRs(t, prov, 2)

	assert.EqualValues(t, 2, snap.Counts["pull_requests"])
	data, err := os.ReadFile(filepath.Join(snap.Dir, "pull_requests.json"))
	require.NoError(t, err)
	var got []*sdkprov.ChangeRequest
	require.NoError(t, json.Unmarshal(data, &got))
	require.Len(t, got, 2)
	// 最近的两个:closed new(4h) 与 open new(3h)
	assert.Equal(t, "4", got[0].Number)
	assert.Equal(t, "2", got[1].Number)
}
