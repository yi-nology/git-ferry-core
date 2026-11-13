package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
