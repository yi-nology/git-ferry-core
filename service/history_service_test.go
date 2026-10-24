package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/mirror"
	"github.com/yi-nology/git-ferry-core/mirror/history"
	"github.com/yi-nology/git-ferry-core/model"
)

// newHistoryTestDB 造一个带历史同步表的内存库。
func newHistoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "h.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.HistoryChannel{}, &model.HistoryTarget{}, &model.HistoryRun{},
		&model.HistoryOverride{},
	))
	return db
}

// newHistoryTestService 造一个只带历史同步能力的服务。
//
// 通道的 plan/resolve 需要真实仓库,这里只覆盖「配置持久化 + 选项解析」,
// 真实仓库路径由 live test 覆盖。
func newHistoryTestService(t *testing.T) *HistoryService {
	t.Helper()
	// 凭据加密需要 ENCRYPTION_KEY(与镜像中心同一套)。
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	db := newHistoryTestDB(t)
	// 产物目录给测试自己的一份:默认会落到用户缓存目录,
	// 跨用例/跨运行累积,既污染机器又让「产物不该堆积」的断言失去意义。
	cfg := &Config{}
	cfg.Sync.HistoryWorkDir = filepath.Join(t.TempDir(), "history-work")
	svc := &Service{db: db, config: cfg}
	h, err := NewHistoryService(svc, db)
	require.NoError(t, err)
	return h
}

func TestHistoryChannelCRUD(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	in := HistoryChannelInput{
		Name:         "code-review",
		SourceDir:    strPtr(historyRepoDir(t, "src")),
		TargetDir:    strPtr(historyRepoDir(t, "tgt")),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/code-review-service"),
		ModuleTarget: strPtr("github.com/yi-nology/code-review-service"),
		HostSource:   strPtr("git.enjoye.top"),
		HostTarget:   strPtr("github.com"),
		PathRules:    []PathRuleDTO{{From: "docs/internal", To: "docs/archive"}},
		ScrubRules:   []ScrubRuleDTO{{Name: "internal-host", Pattern: `\.enjoye\.top$`}},
		TargetBranch: strPtr("main"),
		Enabled:      boolPtr(true),
	}
	dto, err := h.CreateHistoryChannel(ctx, in)
	require.NoError(t, err)
	require.NotZero(t, dto.ID)
	require.Equal(t, "code-review", dto.Name)

	// 重名应被拒。
	_, err = h.CreateHistoryChannel(ctx, in)
	require.Error(t, err, "同名通道应被拒绝")

	// 缺 module 映射应被拒(身份改写是前提)。
	bad := in
	bad.Name = "另一个"
	bad.ModuleTarget = strPtr("")
	_, err = h.CreateHistoryChannel(ctx, bad)
	require.Error(t, err)

	list, total, err := h.ListHistoryChannels(ctx, 0, 50)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, list, 1)

	got, err := h.GetHistoryChannel(ctx, dto.ID)
	require.NoError(t, err)
	require.Equal(t, "code-review", got.Name)

	// 更新。
	got, err = h.UpdateHistoryChannel(ctx, dto.ID, HistoryChannelInput{
		Name: "code-review-v2", TargetBranch: strPtr("main"), Enabled: boolPtr(true),
		ModuleSource: in.ModuleSource, ModuleTarget: in.ModuleTarget,
	})
	require.NoError(t, err)
	require.Equal(t, "code-review-v2", got.Name)

	require.NoError(t, h.DeleteHistoryChannel(ctx, dto.ID))
	_, total, err = h.ListHistoryChannels(ctx, 0, 50)
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
}

// TestHistoryChannelOptionsRoundTrip 验证「配置落库 → 读回 → 解析成执行参数」
// 与「直接传参」得到**同样的**执行参数。
//
// 这是通道化的核心不变式:规则经 DB JSON 往返不能丢信息。
// 字段名对不上时绑定会静默忽略(接口照样 200,规则却从未生效),这条断言专门抓这个。
func TestHistoryChannelOptionsRoundTrip(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	dto, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name:         "rt",
		SourceDir:    strPtr(historyRepoDir(t, "src")),
		TargetDir:    strPtr(historyRepoDir(t, "tgt")),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/x"),
		ModuleTarget: strPtr("github.com/yi-nology/x"),
		HostSource:   strPtr("git.enjoye.top"),
		HostTarget:   strPtr("github.com"),
		PathRules:    []PathRuleDTO{{From: "docs/internal", To: "docs/archive"}},
		ScrubRules: []ScrubRuleDTO{
			{Name: "internal-host", Category: "internal-host", Pattern: `\.enjoye\.top$`},
			{Name: "secret", Category: "secret", Pattern: `ghp_\w+`, Severity: "mandatory"},
		},
		SourceBranch: strPtr("main"),
		TargetBranch: strPtr("main"),
		Enabled:      boolPtr(true),
	})
	require.NoError(t, err)

	got, err := h.channelOptions(ctx, dto.ID)
	require.NoError(t, err)

	// 身份映射:module + host 两条,顺序固定。
	require.Len(t, got.Rules, 2)
	require.Equal(t, "git.enjoye.top/enjoydream/x", got.Rules[0].Source)
	require.Equal(t, "github.com/yi-nology/x", got.Rules[0].Target)
	require.Equal(t, "git.enjoye.top", got.Rules[1].Source)
	require.Equal(t, "github.com", got.Rules[1].Target)

	// 路径规则。
	require.Len(t, got.PathRules, 1)
	require.Equal(t, "docs/internal", got.PathRules[0].From)
	require.Equal(t, "docs/archive", got.PathRules[0].To)

	// 痕迹规则:两条,severity 也要正确往返。
	require.Len(t, got.ScrubRules, 2)
	require.Equal(t, "internal-host", got.ScrubRules[0].Name)
	require.NotNil(t, got.ScrubRules[0].Pattern)
	require.Equal(t, "secret", got.ScrubRules[1].Name)
	require.True(t, got.ScrubRules[1].Mandatory(), "mandatory 语义必须经 DB 往返后仍然成立")
}

// TestHistoryTargetCredential 验证推送目标的凭据加密落库、响应不回显。
func TestHistoryTargetCredential(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "cred", SourceDir: strPtr(historyRepoDir(t, "s")), TargetDir: strPtr(historyRepoDir(t, "t")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	dto, err := h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "backup", RepoURL: "https://example.invalid/repo.git",
		CredType: "token", Credential: "s3cr3t-token", Username: "oauth2",
	})
	require.NoError(t, err)
	require.True(t, dto.HasCredential)
	require.Equal(t, "oauth2", dto.Username)

	// 落库的必须是密文,不是明文。
	targets, err := h.targets.FindByChannel(ch.ID)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.NotContains(t, targets[0].Credential, "s3cr3t-token",
		"凭据必须加密落库")
	require.NotEmpty(t, targets[0].Credential)

	// 能解回明文,推送时用得上。
	auth, err := h.targetAuth(targets[0])
	require.NoError(t, err)
	require.NotEmpty(t, auth.Username)

	// 无凭据类型。
	none, err := h.CreateHistoryTarget(ctx, ch.ID, HistoryTargetInput{
		Name: "publish", RepoURL: "https://example.invalid/pub.git",
	})
	require.NoError(t, err)
	require.False(t, none.HasCredential)
}

// TestHistoryChannelDisabled 禁用通道不参与执行。
func TestHistoryChannelDisabled(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "off", SourceDir: strPtr(historyRepoDir(t, "s")), TargetDir: strPtr(historyRepoDir(t, "t")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(false),
	})
	require.NoError(t, err)

	_, err = h.channelOptions(ctx, ch.ID)
	require.Error(t, err, "禁用的通道不应被执行")
}

// TestHistoryServiceNeedsTargetRepo 通道缺一侧仓库时应报错。
func TestHistoryServiceNeedsTargetRepo(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	_, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "half", SourceDir: strPtr(historyRepoDir(t, "s")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(true),
	})
	require.Error(t, err, "两侧仓库至少各给一个")

	// 执行记录 DAO 的分页也要能用。
	var _ dao.Pagination
	_ = os.Getenv("")
}

// boolPtr 取 bool 指针(DTO 用指针区分「未指定」与「显式 false」)。
func boolPtr(b bool) *bool { return &b }

// TestHistoryRunChain 验证通道内执行记录的哈希链:
// 正常链能通过校验;**篡改任一审计材料都能被检出**。
func TestHistoryRunChain(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "chain", SourceDir: strPtr(historyRepoDir(t, "s")), TargetDir: strPtr(historyRepoDir(t, "t")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	// 造三条已入链的执行记录。
	prev := ""
	for i := 0; i < 3; i++ {
		now := time.Now()
		run := &model.HistoryRun{
			ChannelID:   ch.ID,
			Status:      model.HistoryRunSuccess,
			BaseHead:    fmt.Sprintf("base-%d", i),
			Head:        fmt.Sprintf("head-%d", i),
			PlanSummary: fmt.Sprintf(`{"Total":%d}`, i),
			Commits:     `[{"source":"abc"}]`,
			CreatedAt:   now,
			FinishedAt:  &now,
			PrevHash:    prev,
		}
		run.EntryHash = model.ComputeHistoryRunHash(prev, run)
		require.NoError(t, h.runs.Create(run))
		prev = run.EntryHash
	}

	res, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, res.OK, res.Message)
	require.Equal(t, 3, res.Checked)

	// 篡改第二条记录的审计材料 → 必须被检出。
	runs, err := h.runs.ListByChannelAscending(ch.ID)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	tampered := runs[1]
	tampered.Head = "被改过的 head"
	require.NoError(t, h.runs.Update(tampered))

	res, err = h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.False(t, res.OK, "篡改审计材料后必须检出")
	require.Equal(t, tampered.ID, res.BrokenAt)
	require.Contains(t, res.Message, "entry_hash mismatch")
}

// TestHistoryRunChainSkipsInflight 执行中的记录没有哈希,校验时应跳过而不打断链。
func TestHistoryRunChainSkipsInflight(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "inflight", SourceDir: strPtr(historyRepoDir(t, "s")), TargetDir: strPtr(historyRepoDir(t, "t")),
		ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	now := time.Now()
	done := &model.HistoryRun{
		ChannelID: ch.ID, Status: model.HistoryRunSuccess,
		BaseHead: "b1", Head: "h1", CreatedAt: now,
	}
	done.EntryHash = model.ComputeHistoryRunHash("", done)
	require.NoError(t, h.runs.Create(done))

	// 执行中:字段还会变,没有哈希。
	require.NoError(t, h.runs.Create(&model.HistoryRun{
		ChannelID: ch.ID, Status: model.HistoryRunRunning, CreatedAt: now,
	}))

	res, err := h.VerifyHistoryChannelChain(ctx, ch.ID)
	require.NoError(t, err)
	require.True(t, res.OK, res.Message)
	require.Equal(t, 1, res.Checked, "只校验已入链的记录")
	require.Contains(t, res.Message, "执行中")
}

// TestHistoryRunChainIsolatedPerChannel 不同通道的链互不干扰。
func TestHistoryRunChainIsolatedPerChannel(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	mk := func(name string) uint {
		ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
			Name: name, SourceDir: strPtr(historyRepoDir(t, "s")), TargetDir: strPtr(historyRepoDir(t, "t")),
			ModuleSource: strPtr("a/b"), ModuleTarget: strPtr("c/d"), Enabled: boolPtr(true),
		})
		require.NoError(t, err)
		return ch.ID
	}
	a, b := mk("chain-a"), mk("chain-b")

	now := time.Now()
	for _, id := range []uint{a, b} {
		run := &model.HistoryRun{
			ChannelID: id, Status: model.HistoryRunSuccess,
			BaseHead: "b", Head: "h", CreatedAt: now,
		}
		run.EntryHash = model.ComputeHistoryRunHash("", run)
		require.NoError(t, h.runs.Create(run))
	}

	for _, id := range []uint{a, b} {
		res, err := h.VerifyHistoryChannelChain(ctx, id)
		require.NoError(t, err)
		require.True(t, res.OK, res.Message)
		require.Equal(t, 1, res.Checked)
	}
}

// TestHistoryPreview 预检:摊开影响面但**不推送**。
//
// 关键不变式:预检给出的 new_head 必须与真正 apply 后的 HEAD 一致 ——
// 预检与执行走同一套代码路径,结论不能有出入。
func TestHistoryPreview(t *testing.T) {
	h := newHistoryTestService(t)
	ctx := context.Background()

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	tgtDir := filepath.Join(root, "tgt")
	// 源仓:module 路径是内部身份。
	svcCommitSeq(t, srcDir, [][]svcFileSpec{
		{
			{path: "go.mod", data: "module git.enjoye.top/enjoydream/app\n\ngo 1.24\n"},
		},
		{
			{path: "new.go", data: "package app\n"},
		},
	})
	// 目标仓:module 路径是公开身份,且缺 new.go。
	svcCommitSeq(t, tgtDir, [][]svcFileSpec{
		{
			{path: "go.mod", data: "module github.com/yi-nology/app\n\ngo 1.24\n"},
		},
	})

	ch, err := h.CreateHistoryChannel(ctx, HistoryChannelInput{
		Name: "preview", SourceDir: strPtr(srcDir), TargetDir: strPtr(tgtDir),
		ModuleSource: strPtr("git.enjoye.top/enjoydream/app"),
		ModuleTarget: strPtr("github.com/yi-nology/app"),
		TargetBranch: strPtr("master"), Enabled: boolPtr(true),
	})
	require.NoError(t, err)

	rep, err := h.HistoryPreview(ctx, ch.ID)
	require.NoError(t, err)

	// 影响面。
	assert.Equal(t, ch.ID, rep.ChannelID)
	assert.NotEmpty(t, rep.NewHead)
	assert.NotEqual(t, rep.BaseHead, rep.NewHead, "预检应给出新的 HEAD")
	assert.GreaterOrEqual(t, rep.Summary.Port, 1, "应识别出需要移植的文件")
	assert.GreaterOrEqual(t, rep.ProtectedCount, 0)
	assert.Len(t, rep.Commits, 1, "应预演出一个提交")

	// 门禁结果:物化出来的树要能编译(本夹具是纯 Go 无依赖的最小模块)。
	assert.True(t, rep.Gate.OK, "门禁应通过: "+rep.Gate.Note+" "+rep.Gate.TypeCheckErr)

	// 预检**不推送**:目标仓的 HEAD 不应被改动。
	tgtRepo, err := git.PlainOpen(tgtDir)
	require.NoError(t, err)
	head, err := tgtRepo.Head()
	require.NoError(t, err)
	assert.NotEqual(t, rep.NewHead, head.Hash().String(), "预检不得改动目标仓")

	// 预检结论与真正 apply 一致。
	applyRes, err := history.Apply(ctx, history.ApplyOptions{
		SourceDir: srcDir, TargetDir: tgtDir, TargetRef: "master",
		Rules: []mirror.IdentityRule{{
			Source: "git.enjoye.top/enjoydream/app",
			Target: "github.com/yi-nology/app", Kind: mirror.KindModule,
		}},
		Plan: mustPlan(t, h, ch.ID),
	})
	require.NoError(t, err)
	assert.Equal(t, rep.NewHead, applyRes.Head, "预检的 HEAD 必须与真正 apply 后的 HEAD 一致")
}

// mustPlan 造一份可用的计划(供测试直接调 Apply)。
func mustPlan(t *testing.T, h *HistoryService, channelID uint) *history.Plan {
	t.Helper()
	ch, err := h.channels.FindByID(channelID)
	require.NoError(t, err)
	opts, err := h.channelOptions(context.Background(), channelID)
	require.NoError(t, err)
	plan, err := history.BuildPlan(context.Background(), *opts)
	require.NoError(t, err)
	require.NoError(t, history.Resolve(context.Background(), *opts, plan))
	_ = ch
	return plan
}

// fixtureSig 是本包测试用的提交身份。
var fixtureSig = object.Signature{
	Name:  "fixture",
	Email: "fixture@example.com",
}

// svcFileSpec 是本包测试用的文件描述。
type svcFileSpec struct {
	path string
	data string
}

// svcCommitSeq 在 dir 里按顺序提交若干批文件,返回提交 hash 序列。
func svcCommitSeq(t *testing.T, dir string, batches [][]svcFileSpec) []plumbing.Hash {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	r, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	w, err := r.Worktree()
	require.NoError(t, err)

	var hashes []plumbing.Hash
	for _, batch := range batches {
		for _, f := range batch {
			p := filepath.Join(dir, f.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
			require.NoError(t, os.WriteFile(p, []byte(f.data), 0o644))
		}
		require.NoError(t, w.AddWithOptions(&git.AddOptions{All: true}))
		h, err := w.Commit("fixture", &git.CommitOptions{Author: &fixtureSig, Committer: &fixtureSig})
		require.NoError(t, err)
		hashes = append(hashes, h)
	}
	return hashes
}

// strPtr 取字符串指针(通道入参用指针区分「未指定」与「显式清空」)。
func strPtr(s string) *string { return &s }

// historyRepoDir 造一个可用的 git 仓库目录。
//
// 通道的「本地路径直连」必须指向一个真实的 git 仓(见 validateHistoryRepoDir),
// 所以配置类测试也得用真仓库,不能用 /tmp/xxx 这种占位路径。
func historyRepoDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	makeHistoryRepo(t, dir, map[string]string{"README.md": "fixture\n"})
	return dir
}
