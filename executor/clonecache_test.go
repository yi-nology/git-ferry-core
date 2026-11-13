package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/stretchr/testify/require"
	"github.com/yi-nology/git-ferry-core/model"
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

	// 同源第二次克隆(hit):共享仓须先保鲜再借用。
	wd2 := filepath.Join(t.TempDir(), "wd2")
	require.NoError(t, e.cloneRepo(ctx, wd2, repo, task, nil))

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

// TestCloneCache_DamagedEntrySelfHeals(#21):半删损坏的缓存条目(HEAD/refs 没了、
// objects 还在)必须被检出——键锁内清除并重种,此后每次执行照常成功;
// 只查 .git 目录存在的旧判据会把坏仓交给 --reference,该源永久 fail-closed。
func TestCloneCache_DamagedEntrySelfHeals(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	srcURL := newCloneCacheSource(t)
	ctx := context.Background()
	repo := &model.Repo{CloneURL: srcURL}
	task := &model.SyncTask{SourceBranch: "main"}

	// 首轮:正常种入。
	wd1 := filepath.Join(t.TempDir(), "wd1")
	require.NoError(t, e.cloneRepo(ctx, wd1, repo, task, nil))
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "expected one seeded entry, got %v", entries)
	entry := entries[0]

	// 半删损坏(磁盘压力淘汰中断的 RemoveAll 形态):HEAD/refs 没了、objects 还在。
	require.NoError(t, os.Remove(filepath.Join(entry, ".git", "HEAD")))
	require.NoError(t, os.RemoveAll(filepath.Join(entry, ".git", "refs")))
	_, err = os.Stat(filepath.Join(entry, ".git", "objects"))
	require.NoError(t, err, "precondition: objects dir still present")
	require.False(t, cacheEntryHealthy(entry), "precondition: entry structurally damaged")

	// 损坏后第一次执行:必须自愈重种(旧实现:每轮都以
	// "reference path is not a git repository" 永久失败)。
	wd2 := filepath.Join(t.TempDir(), "wd2")
	require.NoError(t, e.cloneRepo(ctx, wd2, repo, task, nil),
		"damaged entry must self-heal and re-seed, not fail forever")
	require.True(t, cacheEntryHealthy(entry), "entry must be structurally restored")
	alt, err := os.ReadFile(filepath.Join(wd2, ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "healed run must borrow via alternates")
	require.Contains(t, string(alt), cacheDir, "workdir must reference the rebuilt entry")

	// 第三次:健康 hit 照常,且自愈不产生重复条目。
	wd3 := filepath.Join(t.TempDir(), "wd3")
	require.NoError(t, e.cloneRepo(ctx, wd3, repo, task, nil), "healthy hit must keep working")
	entries, err = filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "self-heal must not duplicate entries, got %v", entries)
}

// TestCloneCache_JunkEntryReplaced(#21 的 Rename 半分支):条目路径被非 git 残留
// 目录占住时——旧实现 Clone→Rename 永远撞非空目录,该源每轮都报
// "commit clone cache entry failed" 永久 fail-closed;新实现清掉残留重种。
func TestCloneCache_JunkEntryReplaced(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	srcURL := newCloneCacheSource(t)
	entry := cacheEntryPath(cacheDir, srcURL)
	require.NoError(t, os.MkdirAll(filepath.Join(entry, "leftover"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(entry, "leftover", "junk.txt"), []byte("x"), 0o644))

	wd := filepath.Join(t.TempDir(), "wd")
	require.NoError(t, e.cloneRepo(context.Background(), wd,
		&model.Repo{CloneURL: srcURL}, &model.SyncTask{SourceBranch: "main"}, nil),
		"non-git junk entry must be replaced, not fail forever")
	require.True(t, cacheEntryHealthy(entry), "junk must be replaced by a healthy entry")
	_, err = os.Stat(filepath.Join(entry, "leftover"))
	require.True(t, os.IsNotExist(err), "junk residue must be gone")
}

// TestCommitCacheEntry:tmp→entry 落位的两条防御路径——
// ①目标非空残留:清掉重试一次(不重试则该条目永久 fail-closed);
// ②目标已是健康仓:沿用目标、丢弃 tmp(不许把健康仓清掉)。
func TestCommitCacheEntry(t *testing.T) {
	newTmp := func(t *testing.T, tmp string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(tmp, 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(tmp, "marker.txt"), []byte("seeded"), 0o644))
	}

	t.Run("cleans non-empty junk target and retries", func(t *testing.T) {
		dir := t.TempDir()
		entry := filepath.Join(dir, "repo.git")
		tmp := entry + ".tmp"
		require.NoError(t, os.MkdirAll(filepath.Join(entry, "junk"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(entry, "junk", "x.txt"), []byte("x"), 0o644))
		newTmp(t, tmp)

		require.NoError(t, commitCacheEntry(tmp, entry))
		require.FileExists(t, filepath.Join(entry, "marker.txt"), "tmp must land after cleanup")
		_, err := os.Stat(filepath.Join(entry, "junk"))
		require.True(t, os.IsNotExist(err), "junk target must be gone")
		_, err = os.Stat(tmp)
		require.True(t, os.IsNotExist(err), "tmp must be consumed")
	})

	t.Run("keeps healthy target and discards tmp", func(t *testing.T) {
		dir := t.TempDir()
		entry := filepath.Join(dir, "repo.git")
		tmp := entry + ".tmp"
		// 造一棵结构健康的条目(HEAD 指针 + objects/ + refs/)。
		gitDir := filepath.Join(entry, ".git")
		require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
			[]byte("ref: refs/heads/main\n"), 0o644))
		require.True(t, cacheEntryHealthy(entry))
		newTmp(t, tmp)

		require.NoError(t, commitCacheEntry(tmp, entry))
		require.FileExists(t, filepath.Join(gitDir, "HEAD"), "healthy target must be preserved")
		_, err := os.Stat(filepath.Join(entry, "marker.txt"))
		require.True(t, os.IsNotExist(err), "healthy target must not be overwritten by tmp")
		_, err = os.Stat(tmp)
		require.True(t, os.IsNotExist(err), "tmp must be discarded")
	})
}

// TestCacheEntryHealthy:结构校验的判定面——半删形态(HEAD 缺/refs 缺/垃圾 HEAD/
// .git 缺)必须判不健康(交给 --reference 会永久失败),完整仓判健康。
func TestCacheEntryHealthy(t *testing.T) {
	newEntry := func(t *testing.T) (entry, gitDir string) {
		t.Helper()
		entry = filepath.Join(t.TempDir(), "repo.git")
		gitDir = filepath.Join(entry, ".git")
		require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
			[]byte("ref: refs/heads/main\n"), 0o644))
		return entry, gitDir
	}

	t.Run("complete entry is healthy", func(t *testing.T) {
		entry, _ := newEntry(t)
		require.True(t, cacheEntryHealthy(entry))
	})

	t.Run("detached hex HEAD is healthy", func(t *testing.T) {
		entry, gitDir := newEntry(t)
		sha := strings.Repeat("ab", 20)
		require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(sha+"\n"), 0o644))
		require.True(t, cacheEntryHealthy(entry))
	})

	t.Run("missing HEAD is unhealthy", func(t *testing.T) {
		entry, gitDir := newEntry(t)
		require.NoError(t, os.Remove(filepath.Join(gitDir, "HEAD")))
		require.False(t, cacheEntryHealthy(entry))
	})

	t.Run("garbage HEAD content is unhealthy", func(t *testing.T) {
		entry, gitDir := newEntry(t)
		require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("garbage"), 0o644))
		require.False(t, cacheEntryHealthy(entry))
	})

	t.Run("missing refs dir is unhealthy", func(t *testing.T) {
		entry, gitDir := newEntry(t)
		require.NoError(t, os.RemoveAll(filepath.Join(gitDir, "refs")))
		require.False(t, cacheEntryHealthy(entry))
	})

	t.Run("missing objects dir is unhealthy", func(t *testing.T) {
		entry, gitDir := newEntry(t)
		require.NoError(t, os.RemoveAll(filepath.Join(gitDir, "objects")))
		require.False(t, cacheEntryHealthy(entry))
	})

	t.Run("missing .git is unhealthy", func(t *testing.T) {
		entry, gitDir := newEntry(t)
		require.NoError(t, os.RemoveAll(gitDir))
		require.False(t, cacheEntryHealthy(entry))
	})
}

// TestCloneCache_WikiHealsBrokenAlternates(#25):共享仓被淘汰/删除后,fetch 分支
// 的 wiki workdir alternates 悬空(git fetch 不报错,只把历史重下一遍)——
// syncWiki 决策点必须与 cloneFetchStage 同款自愈:清 wikiDir → clone 分支 →
// ensure 重新种入,配置字段文档对 wiki 的承诺才成立。
func TestCloneCache_WikiHealsBrokenAlternates(t *testing.T) {
	svc := newMockService()
	cacheDir := filepath.Join(t.TempDir(), "clones")
	svc.config.Sync.CloneCacheDir = cacheDir
	e, err := NewExecutor(svc)
	require.NoError(t, err)

	base := t.TempDir()
	srcWiki := filepath.Join(base, "src.wiki.git")
	initFixtureRepo(t, srcWiki)
	dstWiki := filepath.Join(base, "dst.wiki.git")
	out, err := exec.Command("git", "init", "-q", "--bare", dstWiki).CombinedOutput()
	require.NoError(t, err, "init bare: %s", out)

	srcRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "src")}
	dstRepo := &model.Repo{CloneURL: "file://" + filepath.Join(base, "dst")}
	workDir := t.TempDir()

	// 第一轮:种入共享仓,wiki workdir 经 alternates 借用。
	details1 := &strings.Builder{}
	require.NoError(t, e.syncWiki(context.Background(), workDir,
		&model.SyncTask{}, srcRepo, dstRepo, nil, nil, details1),
		"first syncWiki failed: %s", details1.String())
	alt1, err := os.ReadFile(filepath.Join(workDir, "wiki", ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "first run must borrow: %s", details1.String())
	require.Contains(t, string(alt1), cacheDir)
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "one wiki cache entry expected, got %v", entries)

	// 模拟淘汰/运维清缓存:共享仓整目录消失(不碰 wiki workdir)。
	require.NoError(t, os.RemoveAll(cacheDir))

	// 第二轮:决策点检出悬空 → 清 wikiDir → clone 分支 → ensure 重新种入。
	details2 := &strings.Builder{}
	require.NoError(t, e.syncWiki(context.Background(), workDir,
		&model.SyncTask{}, srcRepo, dstRepo, nil, nil, details2),
		"wiki must self-heal after cache loss instead of fetching forever with dangling alternates: %s",
		details2.String())
	require.Contains(t, details2.String(), "rebuilt with full clone",
		"heal must be visible in run details: %s", details2.String())

	entries, err = filepath.Glob(filepath.Join(cacheDir, "*.git"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "wiki cache entry must be re-seeded, got %v", entries)
	alt2, err := os.ReadFile(filepath.Join(workDir, "wiki", ".git", "objects", "info", "alternates"))
	require.NoError(t, err, "healed wiki must borrow again: %s", details2.String())
	target := strings.TrimSpace(string(alt2))
	st, err := os.Stat(target)
	require.NoError(t, err, "alternates target %q must exist", target)
	require.True(t, st.IsDir())

	// 端到端未破坏:推送仍达 bare 目标。
	rev, err := exec.Command("git", "-C", dstWiki,
		"rev-parse", "--verify", "refs/heads/main").CombinedOutput()
	require.NoError(t, err, "bare target must receive main: %s / %s", rev, details2.String())
	require.NotEmpty(t, strings.TrimSpace(string(rev)))
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
