package executor

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	oekiterrors "github.com/oekit/pkg/errors"
)

// ===== 工作目录磁盘配额(P2 大仓库资源限制) =====
//
// 大仓库(全历史 + LFS)× 多任务共享 temp 目录,磁盘会被默默吃满、
// 拖垮宿主机上所有执行。quota 在 clone/fetch/LFS 之后检查
// (这是工作区体积的增长点),超限即中止并**清掉工作区 reclaim 磁盘**
// —— 留着超限工作区只会让下次执行在同一颗雷上再踩一遍。

// dirSizeBytes 递归求目录字节数(WalkDir 不跟随符号链接,防环)。
func dirSizeBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个子项不可读不阻断整体统计(如被并发清理的临时文件)。
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// enforceWorkdirQuota 配额检查:未配置(<=0)直接放行;
// 配置后**测不出大小也中止**(fail-closed——配额既然定了,测不准等于没保护)。
func (e *Executor) enforceWorkdirQuota(rc *RunContext) error {
	cfg := e.service.GetConfig()
	if cfg == nil || cfg.Sync.MaxWorkdirMB <= 0 {
		return nil
	}
	limit := int64(cfg.Sync.MaxWorkdirMB) << 20
	size, err := dirSizeBytes(rc.RepoDir)
	if err != nil {
		return oekiterrors.Wrap(err, "measure workdir size failed (quota configured: fail-closed)")
	}
	if size <= limit {
		rc.logf("workdir size: %d MB (quota %d MB)\n", size>>20, cfg.Sync.MaxWorkdirMB)
		return nil
	}
	// 超限:留痕 + 清工作区 reclaim,再中止本次执行。
	fmt.Fprintf(rc.Details, "workdir quota exceeded: %d MB > %d MB; removing workdir to reclaim disk\n",
		size>>20, cfg.Sync.MaxWorkdirMB)
	rc.logf("workdir quota exceeded: %d MB > %d MB, removing workdir\n", size>>20, cfg.Sync.MaxWorkdirMB)
	if rerr := os.RemoveAll(rc.WorkDir); rerr != nil {
		rc.logf("workdir cleanup after quota breach failed: %v\n", rerr)
	}
	return oekiterrors.Errorf("工作目录超出磁盘配额: %d MB > 上限 %d MB(工作区已清理,下次执行将全量克隆)",
		size>>20, cfg.Sync.MaxWorkdirMB)
}
