package health

import (
	"time"

	"github.com/yi-nology/git-ferry-core/model"
)

// SnapshotsFromRuns 把同步运行记录转成评分快照。
//
// **唯一实现**:core 的健康评分(`service.HealthSnapshot`)与壳层的 AI agent 工具
// 都用它 ⇒ 两边的口径不会漂移。
//
// 以前两边各写一份映射,而壳层那份**漏了三个字段**(StepFails / RetryTotal /
// ErrorType)⇒ 同一个任务,AI 看到的健康分比 API 给运维看的乐观
// (步骤级失败、重试堆积、失败主因三条信号在 AI 侧恒为空)。
//
// 依赖 model:只为拿到运行记录与步骤明细的类型定义;model 是叶子包,
// 不会与本包成环。
func SnapshotsFromRuns(runs []*model.SyncRun) []RunSnapshot {
	if len(runs) == 0 {
		return nil
	}
	snaps := make([]RunSnapshot, 0, len(runs))
	for _, r := range runs {
		if r == nil {
			continue
		}
		end := time.Time{}
		if r.EndTime != nil {
			end = *r.EndTime
		}
		stepFails := 0
		for i := range r.Steps {
			if r.Steps[i].Status == "failed" {
				stepFails++
			}
		}
		snaps = append(snaps, RunSnapshot{
			Status:       r.Status,
			ErrorMessage: r.ErrorMessage,
			EndAt:        end,
			StepFails:    stepFails,
			RetryTotal:   r.RetryTotal,
			ErrorType:    r.ErrorType,
		})
	}
	return snaps
}
