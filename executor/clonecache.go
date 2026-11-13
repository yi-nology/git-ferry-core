package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
	// mu 按共享仓路径互斥:同源的种入/刷新/借用准备同刻只跑一个。
	// KeyedMutex 同键竞争不排队(后来者立即拿不到)——拿不到时调用方
	// 本次克隆**降级为不带 reference 的普通克隆**(正确性不变,只是不省流量),
	// 绝不阻塞在锁上等一次全量克隆做完。
	mu *syncx.KeyedMutex[string]
}

func newCloneCache(dir string) *cloneCache {
	return &cloneCache{dir: dir, mu: syncx.NewKeyedMutex[string]()}
}

// ensure 返回可作 git clone --reference 的共享仓路径。
//
//   - 未种入(miss)或**结构损坏**:no-checkout 全量克隆到 <entry>.tmp 再
//     os.Rename 原子落位(崩溃残留 tmp 下次持锁时先清);种入失败 fail-fast
//     返回错误——缓存目录是显式配置,绝不静默退化成普通克隆;
//   - 已存在且健康(hit):best-effort 增量 fetch 保鲜(把 delta 灌进共享仓,
//     后续新克隆都省;网络类失败仅告警——陈旧的共享仓仍然可用,正确性由
//     workdir 自己的 fetch 兜底);
//   - 半删损坏(HEAD/refs 没了、objects 还在):**键锁内清除并重种(fallthrough)**
//     ——缓存是可再生资产,必须 fail-open 自愈;只查 .git 目录存在会把半删条目
//     当健康仓交给 --reference,此后该源每次执行永久失败(#21);
//   - 锁被同进程其它克隆持有:(usable=false)——调用方本次不带 reference。
func (c *cloneCache) ensure(ctx context.Context, backend gitbackend.GitBackend, cloneURL, filter string, auth gitbackend.AuthConfig) (path string, usable bool, err error) {
	entry := cacheEntryPath(c.dir, cloneURL)
	release, ok := c.mu.Acquire(entry)
	if !ok {
		// 同源克隆正在种/刷:这次降级普通克隆(正确性不变),留痕便于运维识别。
		slog.Info("clone cache contended, cloning without reference", "entry", entry)
		return "", false, nil
	}
	defer release()

	if _, serr := os.Stat(entry); serr == nil {
		if cacheEntryHealthy(entry) {
			if _, ferr := backend.Fetch(ctx, gitbackend.FetchOptions{
				RepoPath: entry,
				Tags:     true,
				Auth:     auth,
			}); ferr != nil {
				slog.Warn("clone cache refresh failed, using stale cache",
					"entry", entry, "error", ferr)
			}
			slog.Info("clone cache reused", "entry", entry)
			return entry, true, nil
		}
		// 结构损坏(磁盘压力下中断的 RemoveAll/半删写入):清掉重种。
		// 不清的话 isGitRepo 只看 .git 目录会误判健康,坏仓交给 --reference
		// 之后每次执行都 fail-closed。
		slog.Warn("damaged clone cache entry detected, re-seeding", "entry", entry)
		if rerr := os.RemoveAll(entry); rerr != nil {
			return "", false, oekiterrors.Wrap(rerr,
				"remove damaged clone cache entry failed", "entry", entry)
		}
	} else if !os.IsNotExist(serr) {
		return "", false, oekiterrors.Wrap(serr,
			"stat clone cache entry failed", "entry", entry)
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
		return "", false, oekiterrors.Wrap(err, "seed clone cache failed", "entry", entry)
	}
	if err := commitCacheEntry(tmp, entry); err != nil {
		return "", false, oekiterrors.Wrap(err,
			"commit clone cache entry failed", "entry", entry)
	}
	slog.Info("clone cache seeded", "entry", entry)
	return entry, true, nil
}

// cacheEntryHealthy 结构校验共享缓存条目:种入产物是 <entry>/.git 下的完整仓
// (HEAD 指针 + objects/ + refs/)。半删损坏的典型形态正是 HEAD/refs 没了、
// objects 还在——只查 .git 目录存在会把它当健康仓交给 git clone --reference,
// 此后该源每次执行永久失败(#21)。
func cacheEntryHealthy(entry string) bool {
	gitDir := filepath.Join(entry, ".git")
	if st, err := os.Stat(gitDir); err != nil || !st.IsDir() {
		return false
	}
	// HEAD 必须存在、是文件、内容是合法指针("ref: <ref>" 或 40/64 位 hex SHA)——
	// 中断写入留下的垃圾 HEAD 同样会让 --reference 失败。
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return false
	}
	head := strings.TrimSpace(string(data))
	switch {
	case strings.HasPrefix(head, "ref:"):
		if strings.TrimSpace(strings.TrimPrefix(head, "ref:")) == "" {
			return false
		}
	case isFullSHA(head):
	default:
		return false
	}
	for _, dir := range []string{"objects", "refs"} {
		if st, err := os.Stat(filepath.Join(gitDir, dir)); err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// isFullSHA 判断 s 是 40 位(sha1)或 64 位(sha256)全 hex 的对象名。
func isFullSHA(s string) bool {
	n := len(s)
	if n != 40 && n != 64 {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// commitCacheEntry 把 tmp 原子落位到 entry(os.Rename)。
//
//   - 目标非空(半删残留/外部写入)导致 Rename 失败:目标健康则直接沿用它
//     (丢弃 tmp);否则清掉目标重试一次——不重试的话残留目录会让该条目
//     **每次**种入都报 "commit clone cache entry failed" 永久 fail-closed;
//   - 两次 Rename 都失败:上抛(调用方带 entry kv 包装)。
func commitCacheEntry(tmp, entry string) error {
	err := os.Rename(tmp, entry)
	if err == nil {
		return nil
	}
	if cacheEntryHealthy(entry) {
		// 防御:锁内不应出现,另一实例/外部进程已落位健康仓——沿用之。
		_ = os.RemoveAll(tmp)
		return nil
	}
	if rmErr := os.RemoveAll(entry); rmErr != nil {
		_ = os.RemoveAll(tmp)
		return errors.Join(err, rmErr)
	}
	if err2 := os.Rename(tmp, entry); err2 != nil {
		_ = os.RemoveAll(tmp)
		return err2
	}
	return nil
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
