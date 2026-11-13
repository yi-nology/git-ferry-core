package executor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// TestCacheEntryPath:共享缓存仓的路径推导——同 URL 确定性、异 URL 不同仓、
// 恶意/怪异 URL 不逃出 cacheDir、仓名可读前缀。
func TestCacheEntryPath(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "clones")

	t.Run("deterministic for same URL", func(t *testing.T) {
		u := "https://github.com/foo/bar.git"
		if p1, p2 := cacheEntryPath(cacheDir, u), cacheEntryPath(cacheDir, u); p1 != p2 {
			t.Fatalf("same URL must map to same path, got %q vs %q", p1, p2)
		}
	})

	t.Run("distinct URLs map to distinct paths", func(t *testing.T) {
		p1 := cacheEntryPath(cacheDir, "https://github.com/foo/bar.git")
		p2 := cacheEntryPath(cacheDir, "https://github.com/foo/baz.git")
		if p1 == p2 {
			t.Fatalf("distinct URLs must not collide, both %q", p1)
		}
	})

	t.Run("hostile URL cannot escape cacheDir", func(t *testing.T) {
		hostile := "https://evil.example/../../etc/passwd?x=../.."
		p := cacheEntryPath(cacheDir, hostile)
		if !strings.HasPrefix(p, cacheDir+string(filepath.Separator)) {
			t.Fatalf("path %q escaped cacheDir %q", p, cacheDir)
		}
		if strings.Contains(filepath.Base(p), "..") {
			t.Fatalf("base name %q must not contain ..", filepath.Base(p))
		}
	})

	t.Run("readable repo-name prefix and .git suffix", func(t *testing.T) {
		p := cacheEntryPath(cacheDir, "https://github.com/foo/bar.git")
		base := filepath.Base(p)
		if !strings.HasPrefix(base, "bar-") {
			t.Fatalf("expected readable prefix bar-, got %q", base)
		}
		if !strings.HasSuffix(base, ".git") {
			t.Fatalf("expected .git suffix, got %q", base)
		}
	})
}

// newCloneCacheSource 造一棵本地源仓(main 分支两提交),返回 file:// URL。
// 身份写进仓库本地配置,不依赖宿主机 git 全局身份(AGENTS 测试夹具纪律)。
func newCloneCacheSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git("-C", dir, "init", "-q")
	git("-C", dir, "symbolic-ref", "HEAD", "refs/heads/main")
	git("-C", dir, "config", "user.email", "fixture@example.com")
	git("-C", dir, "config", "user.name", "fixture")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644))
	git("-C", dir, "add", ".")
	git("-C", dir, "commit", "-qm", "c1")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644))
	git("-C", dir, "add", ".")
	git("-C", dir, "commit", "-qm", "c2")
	return "file://" + dir
}

// TestCloneCache_SameSourceSecondCloneBorrows:配置 clone_cache_dir 后——
// 同源第一次克隆种共享仓且 workdir 走 alternates;第二次克隆复用同一共享仓
// (只有一棵)且同样走 alternates 借对象。
func TestCloneCache_SameSourceSecondCloneBorrows(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	srcURL := newCloneCacheSource(t)
	ctx := context.Background()
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}

	wd1 := filepath.Join(t.TempDir(), "wd1")
	require.NoError(t, e.cloneRepo(ctx, wd1, repo, task, nil), "first clone failed")

	// 首克隆后:共享仓已种入(恰一棵 *.git),workdir 借用其对象库。
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "expected exactly one seeded cache repo, got %v", entries)
	alt1, err := os.ReadFile(filepath.Join(wd1, ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "first workdir must borrow via alternates")
	require.Contains(t, string(alt1), cacheDir, "alternates must point into cache dir")

	// 同源第二次克隆:复用同一共享仓(仍只有一棵),同样走 alternates。
	wd2 := filepath.Join(t.TempDir(), "wd2")
	require.NoError(t, e.cloneRepo(ctx, wd2, repo, task, nil), "second clone failed")
	entries2, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries2, 1, "second clone must reuse the same cache repo, got %v", entries2)
	alt2, err := os.ReadFile(filepath.Join(wd2, ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "second workdir must borrow via alternates")
	require.Contains(t, string(alt2), cacheDir, "alternates must point into cache dir")

	// 借用不是摆设:两个 workdir 的对象查询都正常。
	for _, wd := range []string{wd1, wd2} {
		out, err := exec.Command("git", "-C", wd, "rev-parse", "--verify", "HEAD").CombinedOutput()
		require.NoError(t, err, "rev-parse in %s: %s", wd, out)
		require.NotEmpty(t, strings.TrimSpace(string(out)))
	}
}

// commitToCloneCacheSource 往夹具源仓追加一个提交,返回新 HEAD SHA。
func commitToCloneCacheSource(t *testing.T, srcURL, name, content string) string {
	t.Helper()
	dir := strings.TrimPrefix(srcURL, "file://")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return string(out)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	git("-C", dir, "add", ".")
	git("-C", dir, "commit", "-qm", "add "+name)
	return strings.TrimSpace(git("-C", dir, "rev-parse", "HEAD"))
}

// TestCloneCache_HitRefreshesCache:同源第二次克隆(hit)前共享仓做增量保鲜——
// 源仓新增的提交进共享仓对象库,新 workdir 直接拿到。
func TestCloneCache_HitRefreshesCache(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	srcURL := newCloneCacheSource(t)
	ctx := context.Background()
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}

	// 首克隆:种入共享仓(此时源仓停在 c2)。
	wd1 := filepath.Join(t.TempDir(), "wd1")
	require.NoError(t, e.cloneRepo(ctx, wd1, repo, task, nil))

	// 源仓新增 c3。
	wantSHA := commitToCloneCacheSource(t, srcURL, "c.txt", "c\n")

	// #33:在共享仓里放标记文件——hit 若退化成「删了重种」,标记必消失。
	entries0, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries0, 1)
	marker := filepath.Join(entries0[0], "HIT-MARKER")
	require.NoError(t, os.WriteFile(marker, []byte("1"), 0o644))

	// 同源第二次克隆(hit):共享仓须先保鲜再借用。
	wd2 := filepath.Join(t.TempDir(), "wd2")
	require.NoError(t, e.cloneRepo(ctx, wd2, repo, task, nil))
	require.FileExists(t, marker,
		"hit must be incremental refresh, not delete-and-reseed (marker vanished)")

	// 共享仓的 remote-tracking main 已刷到源仓新 HEAD(没刷新则停在 c2)。
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	out, err := exec.Command("git", "-C", entries[0],
		"rev-parse", "refs/remotes/origin/main").CombinedOutput()
	require.NoError(t, err, "cache origin/main: %s", out)
	require.Equal(t, wantSHA, strings.TrimSpace(string(out)),
		"cache must be refreshed to source HEAD on hit")

	// 新 workdir 拿到 c3。
	require.FileExists(t, filepath.Join(wd2, "c.txt"), "second workdir must see the new commit")
}

// TestCloneCache_DisabledZeroChange:未配置 clone_cache_dir(默认态)——
// 克隆走老路:workdir 无 alternates、对象自持,不产生任何缓存痕迹。
func TestCloneCache_DisabledZeroChange(t *testing.T) {
	svc := newMockService() // CloneCacheDir 为空 = 关闭
	require.Empty(t, svc.config.Sync.CloneCacheDir, "precondition: cache disabled")
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	srcURL := newCloneCacheSource(t)
	wd := filepath.Join(t.TempDir(), "wd")
	require.NoError(t, e.cloneRepo(context.Background(), wd,
		&model.Repo{CloneURL: srcURL}, &model.SyncTask{SourceBranch: "main"}, nil))

	// 无 alternates:对象全部自持(与启用缓存互为镜像的两面)。
	_, err = os.Stat(filepath.Join(wd, ".git", "objects", "info", "alternates"))
	require.True(t, os.IsNotExist(err),
		"cache disabled must not create alternates, stat err=%v", err)

	// 对象自持:两个 blob 都能在本仓读到。
	for _, spec := range []string{"HEAD:a.txt", "HEAD:b.txt"} {
		out, err := exec.Command("git", "-C", wd, "cat-file", "-e", spec).CombinedOutput()
		require.NoError(t, err, "cat-file %s: %s", spec, out)
	}
}

// TestNewExecutor_CloneCacheRequiresNative:配了 clone_cache_dir 但解析出的
// 后端不是 native(gogit 无 reference clone 能力)⇒ 启动 fail-fast——
// 静默不生效等于配置撒谎。
func TestNewExecutor_CloneCacheRequiresNative(t *testing.T) {
	svc := &mockServiceWithConfig{
		mockService: newMockService(),
		backendType: "gogit",
		cacheDir:    t.TempDir(),
	}
	_, err := NewExecutor(svc)
	require.Error(t, err, "gogit + clone_cache_dir must fail fast at startup")
	require.Contains(t, err.Error(), "clone_cache_dir",
		"error must name the offending config key, got: %v", err)
}

// TestCloneCache_ConcurrentSameSourceSingleSeed:同源并发克隆——
// 全部成功(拿不到缓存锁的降级普通克隆),共享仓只种入一棵。
func TestCloneCache_ConcurrentSameSourceSingleSeed(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	srcURL := newCloneCacheSource(t)
	ctx := context.Background()
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}

	const n = 4
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wd := filepath.Join(t.TempDir(), "wd")
			errs <- e.cloneRepo(ctx, wd, repo, task, nil)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "concurrent same-source clone must all succeed")
	}

	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one seeded cache repo expected, got %v", entries)
}

// TestHealBrokenAlternates:workdir 的 alternates 指向已删除的共享仓 ⇒
// 清掉 workdir(下轮/本阶段走全量重克隆);健康与无 alternates 均不动。
func TestHealBrokenAlternates(t *testing.T) {
	t.Run("dangling target removes workdir", func(t *testing.T) {
		wd := t.TempDir()
		info := filepath.Join(wd, ".git", "objects", "info")
		require.NoError(t, os.MkdirAll(info, 0o750))
		bogus := filepath.Join(t.TempDir(), "gone-objects")
		require.NoError(t, os.WriteFile(filepath.Join(info, "alternates"),
			[]byte(bogus+"\n"), 0o644))
		healed, err := healBrokenAlternates(wd)
		require.NoError(t, err)
		require.True(t, healed, "dangling alternates must trigger heal")
		_, err = os.Stat(wd)
		require.True(t, os.IsNotExist(err), "workdir must be removed")
	})

	t.Run("healthy target keeps workdir", func(t *testing.T) {
		wd := t.TempDir()
		info := filepath.Join(wd, ".git", "objects", "info")
		require.NoError(t, os.MkdirAll(info, 0o750))
		okTarget := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(info, "alternates"),
			[]byte(okTarget+"\n"), 0o644))
		healed, err := healBrokenAlternates(wd)
		require.NoError(t, err)
		require.False(t, healed, "healthy workdir must not be touched")
		_, err = os.Stat(wd)
		require.NoError(t, err, "workdir must remain")
	})

	t.Run("no alternates is a no-op", func(t *testing.T) {
		wd := t.TempDir()
		healed, err := healBrokenAlternates(wd)
		require.NoError(t, err)
		require.False(t, healed)
	})
}

// TestCloneCache_StageHealsBrokenAlternates:cloneFetchStage 决策点集成——
// 共享仓被手动删除后,阶段入口检出 alternates 悬空 ⇒ 清 workdir 重克隆,
// 缓存重建、新 workdir 借用有效。
func TestCloneCache_StageHealsBrokenAlternates(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	srcURL := newCloneCacheSource(t)
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}

	wd := filepath.Join(t.TempDir(), "wd")
	require.NoError(t, e.cloneRepo(context.Background(), wd, repo, task, nil))

	// 模拟运维手动删共享仓(不碰 workdir)。
	require.NoError(t, os.RemoveAll(cacheDir))

	rc := &RunContext{
		Task:       task,
		SourceRepo: repo,
		Platforms:  map[uint]*model.Platform{},
		WorkDir:    filepath.Dir(wd),
		RepoDir:    wd,
		Details:    &strings.Builder{},
		Run:        &model.SyncRun{},
		Exec:       e,
	}
	require.NoError(t, (cloneFetchStage{}).Run(context.Background(), rc),
		"stage must self-heal instead of failing on dangling alternates")

	// 缓存重建(恰一棵)且新 workdir 的 alternates 指向真实存在的对象库。
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "cache must be re-seeded after heal, got %v", entries)
	alt, err := os.ReadFile(filepath.Join(wd, ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "healed workdir must borrow again")
	target := strings.TrimSpace(string(alt))
	st, err := os.Stat(target)
	require.NoError(t, err, "alternates target %q must exist", target)
	require.True(t, st.IsDir())
}

// TestCloneCache_SeedFailureFailsFast:源不可达时种入失败——错误必须上抛
// (显式配置的缓存坏了就炸,不静默退化成普通克隆),且不留残缺缓存条目。
func TestCloneCache_SeedFailureFailsFast(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	wd := filepath.Join(t.TempDir(), "wd")
	badRepo := &model.Repo{CloneURL: "file:///nonexistent/clone-cache-src.git"}
	task := &model.SyncTask{SourceBranch: "main"}

	err = e.cloneRepo(context.Background(), wd, badRepo, task, nil)
	require.Error(t, err, "seed failure must fail fast")
	require.Contains(t, err.Error(), "seed clone cache",
		"error must identify the cache seeding step, got: %v", err)

	// 残留清理:无半成品缓存条目、无 dest。
	entries, gerr := filepath.Glob(filepath.Join(cacheDir, "*"))
	require.NoError(t, gerr)
	require.Empty(t, entries, "no partial cache entry may remain, got %v", entries)
	_, serr := os.Stat(wd)
	require.True(t, os.IsNotExist(serr), "dest must not be created on seed failure")
}

// initFixtureRepo 在 dir 造一棵 main 单提交的普通仓(夹具复用)。
func initFixtureRepo(t *testing.T, dir string) {
	t.Helper()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.MkdirAll(dir, 0o750))
	git("-C", dir, "init", "-q")
	git("-C", dir, "symbolic-ref", "HEAD", "refs/heads/main")
	git("-C", dir, "config", "user.email", "fixture@example.com")
	git("-C", dir, "config", "user.name", "fixture")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.md"), []byte("wiki\n"), 0o644))
	git("-C", dir, "add", ".")
	git("-C", dir, "commit", "-qm", "c1")
}

// TestCloneCache_WikiCloneBorrows:syncWiki 的 wiki 克隆同样走共享缓存——
// wiki URL 与主仓不同 ⇒ 独立缓存条目;wiki workdir 走 alternates 借对象,
// 且端到端(对齐+逐分支推送 bare 目标)不被接线破坏。
func TestCloneCache_WikiCloneBorrows(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	base := t.TempDir()
	srcWiki := filepath.Join(base, "src.wiki.git") // 源 wiki:普通仓
	initFixtureRepo(t, srcWiki)
	dstWiki := filepath.Join(base, "dst.wiki.git") // 目标 wiki:bare(推送落点)
	out, err := exec.Command("git", "init", "-q", "--bare", dstWiki).CombinedOutput()
	require.NoError(t, err, "init bare: %s", out)

	srcRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "src")}
	dstRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "dst")}
	workDir := t.TempDir()
	details := &strings.Builder{}

	require.NoError(t, e.syncWiki(context.Background(), workDir,
		&model.SyncTask{}, srcRepo, dstRepo, nil, nil, details),
		"syncWiki failed: %s", details.String())

	// wiki workdir 借用共享缓存(独立条目)。
	alt, err := os.ReadFile(filepath.Join(workDir, "wiki", ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "wiki workdir must borrow via alternates: %s", details.String())
	require.Contains(t, string(alt), cacheDir)
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one wiki cache entry, got %v", entries)

	// 端到端未破坏:推送到达 bare 目标。
	rev, err := exec.Command("git", "-C", dstWiki,
		"rev-parse", "--verify", "refs/heads/main").CombinedOutput()
	require.NoError(t, err, "bare target must receive main: %s / %s", rev, details.String())
	require.NotEmpty(t, strings.TrimSpace(string(rev)))
}

// TestCloneCache_SecondRunFetchesWithAlternates:热路径护栏——workdir 建成后
// 每轮执行走的是 fetch 分支(不是 clone):fetch 必须在 alternates 借用形态下
// 正常增量,且源端新提交能到达 workdir。
func TestCloneCache_SecondRunFetchesWithAlternates(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	srcURL := newCloneCacheSource(t)
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}
	wd := filepath.Join(t.TempDir(), "wd")

	mkRC := func() *RunContext {
		return &RunContext{
			Task:       task,
			SourceRepo: repo,
			Platforms:  map[uint]*model.Platform{},
			WorkDir:    filepath.Dir(wd),
			RepoDir:    wd,
			Details:    &strings.Builder{},
			Run:        &model.SyncRun{},
			Exec:       e,
		}
	}

	// 第一轮:clone(种缓存+借用)。
	rc1 := mkRC()
	require.NoError(t, (cloneFetchStage{}).Run(context.Background(), rc1))

	// 源端前进 → 第二轮:fetch 分支(带 alternates)。
	wantSHA := commitToCloneCacheSource(t, srcURL, "fetch.txt", "f\n")
	rc2 := mkRC()
	require.NoError(t, (cloneFetchStage{}).Run(context.Background(), rc2),
		"second run fetch must work with borrowed objects: %s", rc2.Details.String())
	require.FileExists(t, filepath.Join(wd, "fetch.txt"),
		"fetch path must deliver the new commit into borrowed workdir")
	out, err := exec.Command("git", "-C", wd, "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)
	require.Equal(t, wantSHA, strings.TrimSpace(string(out)))
	// alternates 仍在(没有被 fetch 路径破坏)。
	_, err = os.Stat(filepath.Join(wd, ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "alternates must survive the fetch path")
}

// TestCloneCache_WikiNotFoundClassification:wiki 不存在的分类语义不被缓存
// 接线改变——①file:// 缺失 wiki 在有/无缓存两侧报同一形态(平价);
// ②HTTPS 形态关键词穿过 seed 包装仍可分类(Wrap 保留 cause 文本)。
func TestCloneCache_WikiNotFoundClassification(t *testing.T) {
	base := t.TempDir()
	srcRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "missing")}
	dstRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "dst")}

	run := func(cacheDir string) error {
		svc := newMockService()
		svc.config.Sync.CloneCacheDir = cacheDir
		e, err := NewExecutor(svc)
		require.NoError(t, err)
		return e.syncWiki(context.Background(), t.TempDir(),
			&model.SyncTask{}, srcRepo, dstRepo, nil, nil, &strings.Builder{})
	}

	errNoCache := run("")
	require.Error(t, errNoCache, "precondition: missing wiki errors without cache")
	errCache := run(filepath.Join(t.TempDir(), "clones"))
	require.Error(t, errCache, "missing wiki must still error with cache (file:// shape is not a not-found keyword)")
	require.Contains(t, errCache.Error(), "clone wiki failed",
		"cache path must keep the same failure shape as no-cache, got: %v", errCache)

	// HTTPS 形态关键词:分类器必须穿透 seed 包装。
	wrapped := oekiterrors.Wrap(
		fmt.Errorf("fatal: remote: Repository not found.\\nfatal: Could not read from remote repository."),
		"seed clone cache failed")
	require.True(t, isWikiNotFound(wrapped),
		"not-found keyword must survive the seed wrap, got: %v", wrapped)
}

// incompressibleBytes 确定性不可压缩负载(pack 压不掉,体积可控)。
func incompressibleBytes(n int) []byte {
	b := make([]byte, 0, n+32)
	for counter := 0; len(b) < n; counter++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("payload-%d", counter)))
		b = append(b, sum[:]...)
	}
	return b[:n]
}

// newCloneCacheSourceWithPayload 造带不可压缩负载的源仓(体积≈payload)。
func newCloneCacheSourceWithPayload(t *testing.T, payloadBytes int) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git("-C", dir, "init", "-q")
	git("-C", dir, "symbolic-ref", "HEAD", "refs/heads/main")
	git("-C", dir, "config", "user.email", "fixture@example.com")
	git("-C", dir, "config", "user.name", "fixture")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"),
		incompressibleBytes(payloadBytes), 0o644))
	git("-C", dir, "add", ".")
	git("-C", dir, "commit", "-qm", "payload")
	return "file://" + dir
}

// TestCloneCache_EvictsOldestOverCap:超 clone_cache_max_mb 后按 mtime 淘汰
// 最旧条目,最新种入的保留,总量回到上限内。
func TestCloneCache_EvictsOldestOverCap(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	svc.config.Sync.CloneCacheMaxMB = 1 // 1 MiB 上限
	e, err := NewExecutor(svc)
	require.NoError(t, err, "NewExecutor failed")

	ctx := context.Background()
	task := &model.SyncTask{SourceBranch: "main"}
	const payload = 160_000 // 单仓约 160KB+元数据,10 个 ≫ 1MiB

	// 逐轮记录「本轮新种入」的条目(glob 集合差),结束时硬断言
	// 最旧被淘汰、最新幸存——重言式 DirExists 与 Logf 对冲在 #30 拆除。
	var firstEntry, lastEntry string
	prev := map[string]bool{}
	for i := 0; i < 10; i++ {
		srcURL := newCloneCacheSourceWithPayload(t, payload)
		wd := filepath.Join(t.TempDir(), "wd")
		require.NoError(t, e.cloneRepo(ctx, wd, &model.Repo{CloneURL: srcURL}, task, nil),
			"clone %d failed", i)
		entries, gerr := filepath.Glob(filepath.Join(cacheDir, "*.git"))
		require.NoError(t, gerr)
		require.NotEmpty(t, entries)
		for _, p := range entries {
			if !prev[p] {
				if i == 0 {
					firstEntry = p
				}
				lastEntry = p
				prev[p] = true
			}
		}
	}
	require.NotEmpty(t, firstEntry)
	require.NotEmpty(t, lastEntry)

	size, err := dirSizeBytes(cacheDir)
	require.NoError(t, err)
	require.LessOrEqual(t, size, int64(1)<<20,
		"cache total %d bytes must be within 1MiB cap after eviction", size)

	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Less(t, len(entries), 10, "some oldest entries must be evicted, got %v", entries)
	// 名字里的断言:最旧(首个种入)必须真被淘汰,最新必须幸存。
	if _, err := os.Stat(firstEntry); err == nil {
		t.Fatalf("oldest entry %s must be evicted first, survivors=%v", firstEntry, entries)
	}
	require.DirExists(t, lastEntry, "newest entry must survive eviction")
}

// TestCloneCache_EvictionDisabledByDefault:clone_cache_max_mb 未配置(<=0)
// = 不限制——再多条目也不淘汰(默认态零行为变化)。
func TestCloneCache_EvictionDisabledByDefault(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	require.Zero(t, svc.config.Sync.CloneCacheMaxMB, "precondition: cap unset")
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	ctx := context.Background()
	task := &model.SyncTask{SourceBranch: "main"}
	for i := 0; i < 3; i++ {
		srcURL := newCloneCacheSourceWithPayload(t, 600_000) // 3×600KB ≫ 1MiB
		wd := filepath.Join(t.TempDir(), "wd")
		require.NoError(t, e.cloneRepo(ctx, wd, &model.Repo{CloneURL: srcURL}, task, nil))
	}
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 3, "cap unset must never evict, got %v", entries)
}

// TestCloneCache_SingleOversizedEntryKept:单条超过上限时保留并告警——
// 刚种入的不删(防「删了又种」抖动)。
func TestCloneCache_SingleOversizedEntryKept(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	svc.config.Sync.CloneCacheMaxMB = 1
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	srcURL := newCloneCacheSourceWithPayload(t, 2<<20) // 单仓 2MiB > 1MiB 上限
	wd := filepath.Join(t.TempDir(), "wd")
	require.NoError(t, e.cloneRepo(context.Background(), wd,
		&model.Repo{CloneURL: srcURL}, &model.SyncTask{SourceBranch: "main"}, nil))

	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "single oversized entry must be kept, not deleted then re-seeded")
	size, err := dirSizeBytes(cacheDir)
	require.NoError(t, err)
	require.Greater(t, size, int64(1)<<20, "precondition: entry really exceeds cap")
}

// TestCloneCache_DamagedEntryReSeeds(#21):半删损坏的缓存条目(HEAD/refs
// 没了、objects 还在)必须被检出并重建——缓存是可再生资产,损坏要 fail-open
// 重种,不能让该源永久 fail-closed。
func TestCloneCache_DamagedEntryReSeeds(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	srcURL := newCloneCacheSource(t)
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}
	ctx := context.Background()

	wd1 := filepath.Join(t.TempDir(), "wd1")
	require.NoError(t, e.cloneRepo(ctx, wd1, repo, task, nil))
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// 制造半删态:HEAD+refs 没了,objects 还在(中断 RemoveAll 的形态)。
	require.NoError(t, os.Remove(filepath.Join(entries[0], ".git", "HEAD")))
	require.NoError(t, os.RemoveAll(filepath.Join(entries[0], ".git", "refs")))

	// 第二次克隆:检出损坏 → 清掉重种 → 成功借用。
	wd2 := filepath.Join(t.TempDir(), "wd2")
	require.NoError(t, e.cloneRepo(ctx, wd2, repo, task, nil),
		"damaged entry must be rebuilt, not fail-closed")

	// 条目已重建为完整形状。
	require.FileExists(t, filepath.Join(entries[0], ".git", "HEAD"))
	require.DirExists(t, filepath.Join(entries[0], ".git", "refs"))
	alt, err := os.ReadFile(filepath.Join(wd2, ".git", "objects", "info", "alternates"))
	require.NoError(t, err)
	require.Contains(t, string(alt), cacheDir)
}

// TestCloneCache_RefreshedEntrySurvivesEviction(#22):命中刷新必须把条目
// 标记为最近使用——否则「mtime LRU」实为种入序 FIFO,热条目被系统性淘汰。
func TestCloneCache_RefreshedEntrySurvivesEviction(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	svc.config.Sync.CloneCacheMaxMB = 1
	e, err := NewExecutor(svc)
	require.NoError(t, err)
	ctx := context.Background()
	task := &model.SyncTask{SourceBranch: "main"}

	// 条目 A:种入。
	srcA := newCloneCacheSource(t)
	require.NoError(t, e.cloneRepo(ctx, filepath.Join(t.TempDir(), "w1"), &model.Repo{CloneURL: srcA}, task, nil))
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entryA := entries[0]

	// 条目 B:种入后拨老 48h——最旧且此后无人使用。
	srcB := newCloneCacheSource(t)
	require.NoError(t, e.cloneRepo(ctx, filepath.Join(t.TempDir(), "w3"), &model.Repo{CloneURL: srcB}, task, nil))
	entries, err = filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	var entryB string
	for _, p := range entries {
		if p != entryA {
			entryB = p
		}
	}
	require.NotEmpty(t, entryB)
	stale := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(entryB, stale, stale))

	// 压力铺底:4 个 160KB 载荷源(实测单条 ~190KB,4 发约 820KB < 1MiB,
	// 不触发淘汰)。
	for i := 0; i < 4; i++ {
		src := newCloneCacheSourceWithPayload(t, 160_000)
		require.NoError(t, e.cloneRepo(ctx, filepath.Join(t.TempDir(), "wp"), &model.Repo{CloneURL: src}, task, nil))
	}

	// hit A:修复后必须把 A 的 mtime 拨到现在(否则 A 比 P1..P4 更老,下一步
	// 超限时会先于未使用的 P1 被淘汰——那正是 FIFO 病)。
	require.NoError(t, e.cloneRepo(ctx, filepath.Join(t.TempDir(), "w2"), &model.Repo{CloneURL: srcA}, task, nil))

	// 两发超限(820+190×2 ≈ 1.20MB > 1MiB):淘汰循环先删最旧的 B(-48h),
	// 再删次旧的 P1(种于 hit 之前);A 因刚被 hit 标记为最近使用而幸存。
	for i := 0; i < 2; i++ {
		src := newCloneCacheSourceWithPayload(t, 160_000)
		require.NoError(t, e.cloneRepo(ctx, filepath.Join(t.TempDir(), "wp"), &model.Repo{CloneURL: src}, task, nil))
	}

	// 断言:刷新过的 A 活着;48h 前种入且没再用过的 B 被淘汰。
	require.DirExists(t, entryA, "recently-refreshed hot entry must survive eviction (LRU, not FIFO)")
	if _, err := os.Stat(entryB); err == nil {
		t.Fatalf("stale unused entry %s must be evicted first", entryB)
	}
}

// TestCloneCache_EvictSkipsInFlightEntry(#23):淘汰不得删除「键锁被在途
// 克隆持有」的条目——path 已返回、clone 还在锁外跑,删了就是无谓失败。
func TestCloneCache_EvictSkipsInFlightEntry(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	svc.config.Sync.CloneCacheMaxMB = 1
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	// 造一个「在途」条目:持有其键锁(与 ensure 借用生命周期同形)。
	entryInflight := cacheEntryPath(cacheDir, "https://x/inflight.git")
	require.NoError(t, os.MkdirAll(filepath.Join(entryInflight, ".git"), 0o750))
	release, ok := e.cache.mu.Acquire(entryInflight)
	require.True(t, ok)
	defer release()

	// 造超限压力:一个 2MiB 条目(不持锁)+ cap=1MiB,必然触发淘汰。
	entryBig := cacheEntryPath(cacheDir, "https://x/big.git")
	require.NoError(t, os.MkdirAll(filepath.Join(entryBig, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(entryBig, ".git", "junk"),
		incompressibleBytes(2<<20), 0o644))

	e.cache.evictOverCap(entryBig)

	_, err = os.Stat(entryInflight)
	require.NoError(t, err,
		"in-flight (key-locked) entry must be skipped by eviction")
}

// TestCloneCache_EvictWithMetaCharPathStaysInside(#26):配置路径含 glob
// 元字符时,淘汰枚举不得逃到目录外(Glob 会把 cc[x] 当字符类匹配到兄弟目录)。
func TestCloneCache_EvictWithMetaCharPathStaysInside(t *testing.T) {
	base := t.TempDir()
	metaDir := filepath.Join(base, "cc[x]", "clones") // 字面量含 [
	victim := filepath.Join(base, "ccx", "clones", "victim.git")
	require.NoError(t, os.MkdirAll(metaDir, 0o750))
	require.NoError(t, os.MkdirAll(victim, 0o750))

	svc := newMockService()
	svc.config.Sync.CloneCacheDir = metaDir
	svc.config.Sync.CloneCacheMaxMB = 1
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	// cacheDir 内放 2MiB 垃圾条目触发淘汰,keep 用它自己。
	garbage := filepath.Join(metaDir, "garbage.git")
	require.NoError(t, os.MkdirAll(garbage, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(garbage, "junk"),
		incompressibleBytes(2<<20), 0o644))

	e.cache.evictOverCap(garbage)

	_, err = os.Stat(victim)
	require.NoError(t, err,
		"eviction must never touch paths outside cacheDir (glob metachar escape)")
}

// TestCloneCache_SweepsOrphanTmp(#24):崩溃残留的 <entry>.git.tmp 必须按
// 年龄清扫——它计入总量却永不可淘汰,不清的话配置上限后缓存会塌缩到只剩
// keep(探针实证 survivors=1 + 垃圾永驻)。
func TestCloneCache_SweepsOrphanTmp(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	svc.config.Sync.CloneCacheMaxMB = 1
	e, err := NewExecutor(svc)
	require.NoError(t, err)
	ctx := context.Background()
	task := &model.SyncTask{SourceBranch: "main"}

	// 孤儿 tmp:2MiB 垃圾,2 小时前的(超过清扫宽限),对应 entry 不存在。
	orphan := filepath.Join(cacheDir, "orphan.git.tmp")
	require.NoError(t, os.MkdirAll(filepath.Join(orphan, "x"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "x", "big"),
		incompressibleBytes(2<<20), 0o644))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(orphan, old, old))

	// 连种 3 个小源触发容量检查。
	for i := 0; i < 3; i++ {
		src := newCloneCacheSourceWithPayload(t, 160_000)
		require.NoError(t, e.cloneRepo(ctx, filepath.Join(t.TempDir(), "wd"),
			&model.Repo{CloneURL: src}, task, nil))
	}

	// 断言:孤儿被清;健康条目没有被「计了孤儿的总量」逼到塌缩。
	_, err = os.Stat(orphan)
	require.True(t, os.IsNotExist(err),
		"aged orphan tmp must be swept, still present: %v", err)
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Greater(t, len(entries), 1,
		"cache must not collapse to a single entry under cap (got %v)", entries)
}

// TestCloneCache_WikiHealsAfterEntryDeleted(#25):共享仓被删后,存量 wiki
// workdir 下一轮必须走自愈(清 workdir → clone 分支 → 重种条目)——
// 与主仓同轨,配置字段文档的自愈承诺对 wiki 同样成立。
func TestCloneCache_WikiHealsAfterEntryDeleted(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	base := t.TempDir()
	initFixtureRepo(t, filepath.Join(base, "src.wiki.git"))
	cmd := exec.Command("git", "init", "-q", "--bare", filepath.Join(base, "dst.wiki.git"))
	outb, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", outb)
	srcRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "src")}
	dstRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "dst")}
	workDir := t.TempDir()

	// 第一轮:种入 + wiki 借用。
	require.NoError(t, e.syncWiki(context.Background(), workDir, &model.SyncTask{},
		srcRepo, dstRepo, nil, nil, &strings.Builder{}))
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// 删共享仓(等价被逐出/手删)。
	require.NoError(t, os.RemoveAll(cacheDir))

	// 第二轮:必须自愈重建,而不是带着悬空 alternates 空转。
	require.NoError(t, e.syncWiki(context.Background(), workDir, &model.SyncTask{},
		srcRepo, dstRepo, nil, nil, &strings.Builder{}))
	entries2, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries2, 1,
		"wiki cache entry must be rebuilt after deletion (self-heal), got %v", entries2)
	alt, err := os.ReadFile(filepath.Join(workDir, "wiki", ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "healed wiki workdir must borrow again")
	require.Contains(t, string(alt), cacheDir)
}

// TestCloneCache_WikiAndMainSeparateEntries(#32):同一 cacheDir 下主仓与
// wiki 各自成条目(键是完整 URL 的哈希)——防止「裁掉 .wiki.git 再哈希」
// 一类回归让两者共用一个条目。
func TestCloneCache_WikiAndMainSeparateEntries(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	base := t.TempDir()
	initFixtureRepo(t, filepath.Join(base, "src"))          // 主仓
	initFixtureRepo(t, filepath.Join(base, "src.wiki.git")) // wiki
	outb, err := exec.Command("git", "init", "-q", "--bare",
		filepath.Join(base, "dst.wiki.git")).CombinedOutput()
	require.NoError(t, err, "%s", outb)

	// 主仓先种一个条目。
	require.NoError(t, e.cloneRepo(context.Background(), filepath.Join(t.TempDir(), "wd"),
		&model.Repo{CloneURL: "file://" + filepath.Join(base, "src")},
		&model.SyncTask{SourceBranch: "main"}, nil))

	// wiki 再种一个:必须是第二棵,且键名带 src.wiki 前缀。
	require.NoError(t, e.syncWiki(context.Background(), t.TempDir(), &model.SyncTask{},
		&model.Repo{CloneURL: "file://" + filepath.Join(base, "src")},
		&model.Repo{CloneURL: "file://" + filepath.Join(base, "dst")},
		nil, nil, &strings.Builder{}))

	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 2, "main and wiki must be separate entries, got %v", entries)
	var wikiEntry string
	for _, p := range entries {
		if strings.Contains(filepath.Base(p), "src.wiki-") {
			wikiEntry = p
		}
	}
	require.NotEmpty(t, wikiEntry,
		"wiki entry must be keyed by the wiki URL (basename src.wiki-*), got %v", entries)
}

// TestWikiCacheRaceClassification(#27):竞态错误的分类冲突前提 + 守卫判定。
// 前提:共享仓消失导致的 ErrNotAGitRepo 文案含 "does not exist",同时命中
// wiki 的 not-found 分类器——若无守卫,竞态会被吞成静默 skip。
func TestWikiCacheRaceClassification(t *testing.T) {
	svc := newMockService()
	svc.config.Sync.CloneCacheDir = filepath.Join(t.TempDir(), "clones")
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	// 真实造一个「Reference 指向不存在路径」的克隆错误。
	dest := filepath.Join(t.TempDir(), "wd")
	err = e.backend.Clone(context.Background(), gitbackend.CloneOptions{
		URL:       newCloneCacheSource(t),
		Path:      dest,
		Reference: filepath.Join(t.TempDir(), "vanished"),
	})
	require.Error(t, err)
	require.True(t, oekiterrors.Is(err, gitbackend.ErrNotAGitRepo),
		"premise: dangling reference must carry ErrNotAGitRepo, got: %v", err)
	require.True(t, isWikiNotFound(err),
		"premise: the same error also matches wiki not-found (conflict), got: %v", err)

	// 守卫:带哨兵 + 已设 Reference → 竞态,必须走降级重试而非 wiki 分类。
	require.True(t, isCacheRaceNotRepo(err, "/some/reference"))
	// 无 Reference(非缓存路径)或无哨兵(真 wiki 404)→ 不是竞态。
	require.False(t, isCacheRaceNotRepo(err, ""))
	require.False(t, isCacheRaceNotRepo(
		fmt.Errorf("fatal: repository 'http://x/y.wiki.git/' not found"), "/some/reference"))
}

// TestCloneCache_WikiNotFoundSkipsEndToEnd(#28):wiki 不存在 → skip 的核心
// 契约端到端覆盖——httptest 404 的 git 报文含 "not found"(探针实测),
// cache 开/关两侧都必须 nil + skip 文案,skip 分支第一次被真实执行。
func TestCloneCache_WikiNotFoundSkipsEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	for _, cacheOn := range []bool{false, true} {
		name := "no-cache"
		if cacheOn {
			name = "with-cache"
		}
		t.Run(name, func(t *testing.T) {
			svc := newMockService()
			if cacheOn {
				svc.config.Sync.CloneCacheDir = filepath.Join(t.TempDir(), "clones")
			}
			e, err := NewExecutor(svc)
			require.NoError(t, err)

			details := &strings.Builder{}
			err = e.syncWiki(context.Background(), t.TempDir(), &model.SyncTask{},
				&model.Repo{CloneURL: srv.URL + "/src"},
				&model.Repo{CloneURL: srv.URL + "/dst"},
				nil, nil, details)
			require.NoError(t, err,
				"missing wiki must skip, not fail (cache=%v): %s\n%s", cacheOn, err, details.String())
			require.Contains(t, details.String(), "wiki: not found on source, skip",
				"skip branch must execute, details: %s", details.String())
		})
	}
}
