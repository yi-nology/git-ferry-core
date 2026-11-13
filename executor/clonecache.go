package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oekit/concurrency/syncx"
	oekiterrors "github.com/oekit/pkg/errors"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// ===== Clone Cache:同源共享克隆缓存(P2 大仓库省流) =====
//
// 多个同步任务指向同一源仓库时,首次克隆种一棵共享仓(no-checkout 全量),
// 之后 workdir 经 git clone --reference 借用其对象库(objects/info/alternates):
// 同源 N 任务网络从 N×全量降到 1×全量+N×增量,磁盘从 N×仓库体降到
// 1×共享+N×元数据。详见 model.SyncConfig.CloneCacheDir 的字段文档。

// cacheEntryPath 共享缓存仓的落盘路径:<cacheDir>/<repo>-<urlhash16>.git。
// hash 是整个 CloneURL 的 sha256 前 16 位 hex——同源确定性、跨源不相撞;
// 仓名只作可读前缀,清洗 + 固定 "-hash.git" 后缀保证路径不逃出 cacheDir。
func cacheEntryPath(cacheDir, cloneURL string) string {
	sum := sha256.Sum256([]byte(cloneURL))
	return filepath.Join(cacheDir, repoNameForCache(cloneURL)+"-"+hex.EncodeToString(sum[:])[:16]+".git")
}

// repoNameForCache 从 URL 提取可读仓名:去 query/fragment 与 .git 后缀,
// 取末段,非法字符折成 _,限 40 字符,空/./.. 回退 "repo"。
func repoNameForCache(cloneURL string) string {
	u := cloneURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	u = strings.TrimSuffix(u, ".git")
	var b strings.Builder
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	name := strings.ReplaceAll(b.String(), "..", "_")
	if name == "" || name == "." || name == ".." {
		return "repo"
	}
	return name
}

// cloneCache 同源共享克隆缓存(per-instance 本地目录,不支持多实例共享同一路径)。
type cloneCache struct {
	dir string
	// maxMB 缓存总量上限(MiB),<=0=不限制。超限按 mtime 淘汰最旧条目。
	maxMB int
	// mu 按共享仓路径互斥:同源的种入/刷新/借用准备同刻只跑一个。
	// KeyedMutex 同键竞争不排队(后来者立即拿不到)——拿不到时调用方
	// 本次克隆**降级为不带 reference 的普通克隆**(正确性不变,只是不省流量),
	// 绝不阻塞在锁上等一次全量克隆做完。
	mu *syncx.KeyedMutex[string]
}

func newCloneCache(dir string, maxMB int) *cloneCache {
	return &cloneCache{dir: dir, maxMB: maxMB, mu: syncx.NewKeyedMutex[string]()}
}

// ensure 返回可作 git clone --reference 的共享仓路径,以及**必须由调用方
// 在 clone 完成后调用的 release**(#23:键锁的生命周期 = 整个借用生命周期,
// 覆盖「path 返回之后、clone 完成之前」的在途窗口;release 幂等,漏调的
// 代价只是该源此后降级为普通克隆)。
//
//   - 未种入(miss):no-checkout 全量克隆到 <entry>.tmp 再 os.Rename 原子落位
//     (崩溃残留 tmp 下次持锁时先清);种入失败 fail-fast 返回错误——缓存目录是
//     显式配置,绝不静默退化成普通克隆;
//   - 已存在(hit):best-effort 增量 fetch 保鲜 + 触摸 mtime(淘汰排序是真
//     LRU 而非种入序 FIFO,#22);失败仅告警——陈旧的共享仓仍然可用;
//   - 锁被同进程其它克隆持有:(usable=false, release=nil)——调用方本次不带
//     reference。
func (c *cloneCache) ensure(ctx context.Context, backend gitbackend.GitBackend, cloneURL, filter string, auth gitbackend.AuthConfig) (path string, usable bool, release func(), err error) {
	entry := cacheEntryPath(c.dir, cloneURL)
	release, ok := c.mu.Acquire(entry)
	if !ok {
		// 同源克隆正在种/刷:这次降级普通克隆(正确性不变),留痕便于运维识别。
		slog.Info("clone cache contended, cloning without reference", "entry", entry)
		return "", false, nil, nil
	}
	path, usable, err = c.ensureLocked(ctx, backend, cloneURL, filter, auth, entry)
	if err != nil {
		release()
		return "", false, nil, err
	}
	// 成功:锁交给调用方持有到 clone 结束(#23)。
	return path, usable, release, nil
}

// ensureLocked 在已持 entry 键锁的前提下执行种入/刷新(锁管理见 ensure)。
func (c *cloneCache) ensureLocked(ctx context.Context, backend gitbackend.GitBackend, cloneURL, filter string, auth gitbackend.AuthConfig, entry string) (path string, usable bool, err error) {
	if isUsableEntry(entry) {
		if _, ferr := backend.Fetch(ctx, gitbackend.FetchOptions{
			RepoPath: entry,
			Tags:     true,
			Auth:     auth,
		}); ferr != nil {
			slog.Warn("clone cache refresh failed, using stale cache",
				"entry", entry, "error", ferr)
		}
		slog.Info("clone cache reused", "entry", entry)
		// #22:命中即「最近使用」——刷新写的是 entry/.git 内部,不改 entry
		// 目录 mtime,不显式触摸的话淘汰排序永远等于种入序(FIFO)。
		touchEntry(entry)
		// 刷新会往共享仓灌对象,复用后同样受总量上限约束。
		c.evictOverCap(entry)
		return entry, true, nil
	}

	// 缺失,或「半删损坏」(中断 RemoveAll 留下 .git 残骸):缓存是可再生
	// 资产,fail-open——损坏先清再重种,绝不让该源永久 fail-closed(#21)。
	if _, serr := os.Stat(entry); serr == nil {
		if rerr := os.RemoveAll(entry); rerr != nil {
			return "", false, oekiterrors.Wrap(rerr, "remove damaged clone cache entry failed", "entry", entry)
		}
		slog.Info("clone cache damaged entry removed for re-seed", "entry", entry)
	}

	if err := os.MkdirAll(c.dir, 0o750); err != nil {
		return "", false, oekiterrors.Wrap(err, "create clone cache dir failed")
	}
	tmp := entry + ".tmp"
	_ = os.RemoveAll(tmp) // 上次进程崩溃的残留
	if err := backend.Clone(ctx, gitbackend.CloneOptions{
		URL:        cloneURL,
		Path:       tmp,
		NoCheckout: true, // 全量对象+全量 refs,不落工作树文件(共享仓只要对象库)
		Filter:     filter,
		Auth:       auth,
	}); err != nil {
		_ = os.RemoveAll(tmp)
		return "", false, oekiterrors.Wrap(err, "seed clone cache failed")
	}
	if err := os.Rename(tmp, entry); err != nil {
		if isUsableEntry(entry) {
			// 跨进程抢先种入了完整条目:采纳它,清掉自己的 tmp。
			_ = os.RemoveAll(tmp)
			slog.Info("clone cache seeded", "entry", entry, "adopted", true)
			c.evictOverCap(entry)
			return entry, true, nil
		}
		// entry 存在但损坏/非空残留 → 清掉重试一次(#21 的 rename 撞残骸面)。
		_ = os.RemoveAll(entry)
		if err2 := os.Rename(tmp, entry); err2 != nil {
			_ = os.RemoveAll(tmp)
			return "", false, oekiterrors.Wrap(err2, "commit clone cache entry failed")
		}
	}
	slog.Info("clone cache seeded", "entry", entry)
	c.evictOverCap(entry)
	return entry, true, nil
}

// orphanTmpGrace 孤儿 <entry>.tmp 的清扫宽限:短于此的视为在途种子
// (种子是分钟级全量克隆),不碰。
const orphanTmpGrace = time.Hour

// sweepOrphanTmp 清扫崩溃残留的孤儿 <entry>.tmp(#24):它计入总量却永远
// 不可淘汰(*.git 枚举天然排除 .tmp),不扫会把「配置上限」逼成「只剩
// keep + 垃圾」的塌缩,不配上限则是纯磁盘泄漏。宽限期内(在途种子)或
// 对应 entry 键锁被持有(同进程种子进行中)的一律不动。
func (c *cloneCache) sweepOrphanTmp() {
	dents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, de := range dents {
		name := de.Name()
		if !strings.HasSuffix(name, ".git.tmp") {
			continue
		}
		tmpPath := filepath.Join(c.dir, name)
		st, serr := os.Stat(tmpPath)
		if serr != nil {
			continue
		}
		if time.Since(st.ModTime()) < orphanTmpGrace {
			continue // 在途种子 / 新鲜残留
		}
		entry := strings.TrimSuffix(name, ".tmp")
		rel, ok := c.mu.Acquire(entry)
		if !ok {
			continue // 对应种子在途
		}
		rerr := os.RemoveAll(tmpPath)
		rel()
		if rerr != nil {
			slog.Warn("clone cache orphan tmp sweep failed", "tmp", tmpPath, "error", rerr)
			continue
		}
		slog.Info("clone cache orphan tmp swept", "tmp", tmpPath)
	}
}

// evictOverCap 缓存总量超限时按 mtime 淘汰最旧条目(LRU 近似)。
// 只删 *.git 条目(不碰在途 *.git.tmp);keep(刚种入/刚复用的)永不删——
// 单条超限时保留并告警,避免「删了又种」的抖动。被淘汰条目若被存量
// workdir 引用,下轮执行由 healBrokenAlternates 自愈重建(与手动删
// 缓存同一轨)。size 复算走 dirSizeBytes(与配额同源)。
func (c *cloneCache) evictOverCap(keep string) {
	// 孤儿清扫与上限无关:不配上限也要清(纯泄漏面,#24)。
	c.sweepOrphanTmp()
	if c.maxMB <= 0 {
		return
	}
	limit := int64(c.maxMB) << 20
	size, err := dirSizeBytes(c.dir)
	if err != nil {
		slog.Warn("clone cache size check failed", "error", err)
		return
	}
	for size > limit {
		// 枚举用 os.ReadDir 而非 filepath.Glob(#26):配置路径里的 *?[ 会被
		// Glob 当模式语义解析,匹配到目录外的兄弟路径(实证:cc[x] 吃到 ccx/)。
		dents, rerr := os.ReadDir(c.dir)
		if rerr != nil {
			slog.Warn("clone cache list failed", "error", rerr)
			return
		}
		// 逐个 try-Acquire 锁定候选:拿不到=该条目正被在途克隆借用(#23),
		// 跳过;拿到则持锁选出 mtime 最旧者,其余候选立即释放。
		oldest, oldestM := "", time.Time{}
		var oldestRelease func()
		for _, de := range dents {
			if !de.IsDir() || !strings.HasSuffix(de.Name(), ".git") {
				continue
			}
			p := filepath.Join(c.dir, de.Name())
			if p == keep {
				continue
			}
			rel, aok := c.mu.Acquire(p)
			if !aok {
				continue // 在途使用中,不可删
			}
			st, serr := os.Stat(p)
			if serr != nil {
				rel() // 已被并发移除
				continue
			}
			if oldest == "" || st.ModTime().Before(oldestM) {
				if oldestRelease != nil {
					oldestRelease() // 释放上一个候选,只保留最旧者
				}
				oldest, oldestM, oldestRelease = p, st.ModTime(), rel
			} else {
				rel()
			}
		}
		if oldest == "" {
			break // 只剩 keep 或全部在途(单条超限)
		}
		// 持最旧者的键锁删除:期间并发 ensure 该键会降级普通克隆(不借用),
		// 不会拿到一个正在消失的 path。
		rerr = os.RemoveAll(oldest)
		oldestRelease()
		if rerr != nil {
			slog.Warn("clone cache evict failed", "entry", oldest, "error", rerr)
			break
		}
		slog.Info("clone cache evicted", "entry", oldest)
		size, err = dirSizeBytes(c.dir)
		if err != nil {
			slog.Warn("clone cache size recheck failed", "error", err)
			return
		}
	}
	if size > limit {
		slog.Warn("clone cache single entry exceeds cap, keeping to avoid thrash",
			"entry", keep, "size_bytes", size, "cap_mb", c.maxMB)
	}
}

// isUsableEntry 判断缓存条目是否为完整可用的 no-checkout 克隆产物:
// .git/HEAD + .git/objects + .git/refs 三件齐(只查 .git 目录存在会放过
// 半删残骸,残骸过不了 SDK 校验却会被当成健康条目复用 → 永久 fail-closed,#21)。
func isUsableEntry(path string) bool {
	base := filepath.Join(path, ".git")
	if st, err := os.Stat(filepath.Join(base, "HEAD")); err != nil || st.IsDir() {
		return false
	}
	for _, d := range []string{"objects", "refs"} {
		if st, err := os.Stat(filepath.Join(base, d)); err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// isCacheRaceNotRepo 判定「共享仓在 ensure 返回后消失」的竞态错误:
// Reference 已设 + ErrNotAGitRepo 哨兵。这类错误必须降级无 reference
// 重试一次(#27)——其文案含 "does not exist",直接进 wiki 分类器会被
// isWikiNotFound 误吞成「wiki 不存在 → 静默 skip」(与缓存竞态矛盾)。
func isCacheRaceNotRepo(err error, reference string) bool {
	return err != nil && reference != "" && oekiterrors.Is(err, gitbackend.ErrNotAGitRepo)
}

// touchEntry 把条目目录 mtime 拨到现在(「最近使用」标记,#22)。
func touchEntry(entry string) {
	now := time.Now()
	_ = os.Chtimes(entry, now, now)
}

// healBrokenAlternates 自愈:workdir 的 objects/info/alternates 指向的共享仓
// 已被删除(运维手动清缓存/磁盘回收)时,清掉整个 workdir——探针实测
// git fetch 对悬空 alternates **不报错**(exit 0,只打 unable-to-normalize
// 告警并把整段历史重下一遍),失败驱动的自愈等不到触发点,必须主动检出。
// 清除后由调用方自然走全量重克隆(缓存目录若也删了会顺带重新种入)。
// 返回 (healed, err):healed=true 表示已清除,调用方应改走克隆路径;
// 无 alternates/目标健在 ⇒ (false, nil) 不动;清除失败 ⇒ err(fail-loud)。
func healBrokenAlternates(workdir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(workdir, ".git", "objects", "info", "alternates"))
	if err != nil {
		return false, nil // 没有 alternates(普通克隆或 workdir 不存在)——健康
	}
	for _, line := range strings.Split(string(data), "\n") {
		target := strings.TrimSpace(line)
		if target == "" {
			continue
		}
		if _, serr := os.Stat(target); serr == nil {
			continue // 目标健在
		} else if !os.IsNotExist(serr) {
			return false, oekiterrors.Wrap(serr, "stat alternates target failed")
		}
		// 悬空:清 workdir,让调用方全量重克隆。
		if rerr := os.RemoveAll(workdir); rerr != nil {
			return false, oekiterrors.Wrap(rerr, "remove workdir with broken alternates failed")
		}
		return true, nil
	}
	return false, nil
}
