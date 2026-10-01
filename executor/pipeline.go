package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/yi-nology/git-ferry-core/model"
)

// Stage 同步流水线的一个阶段。
// 职责单一:只做一件事,通过 execCtx 传递运行态,错误即中止(除 IsOptional)。
type Stage interface {
	Name() string
	// IsOptional 可选阶段失败不中止流水线(如 wiki/冷备)。
	IsOptional() bool
	Run(ctx context.Context, rc *RunContext) error
}

// RunContext 流水线共享运行态(阶段间传参,替代超长函数局部变量)。
type RunContext struct {
	Task        *model.SyncTask // 本地副本(分支已回退,不回写 DB)
	SourceRepo  *model.Repo
	TargetRepo  *model.Repo
	Platforms   map[uint]*model.Platform
	WorkDir     string
	RepoDir     string
	Details     *strings.Builder
	Run         *model.SyncRun
	Exec        *Executor
	BundlesPath string
}

// logf 写入执行详情(统一入口,防各阶段各自 fprintf)。
func (rc *RunContext) logf(format string, args ...any) {
	fmt.Fprintf(rc.Details, format, args...)
}

// Pipeline 有序阶段执行器。
type Pipeline struct {
	stages []Stage
}

// NewPipeline 组装流水线。
func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages}
}

// Run 顺序执行;可选阶段失败记日志继续,必选失败即返回。
func (p *Pipeline) Run(ctx context.Context, rc *RunContext) error {
	for _, st := range p.stages {
		if err := ctx.Err(); err != nil {
			return err
		}
		rc.logf("\n>> %s\n", st.Name())
		step := rc.Exec.beginStep(rc.Run.ID, stepNameFor(st.Name()))
		err := st.Run(ctx, rc)
		if err != nil {
			rc.Exec.failStep(step, err)
			rc.logf("%s error: %v\n", st.Name(), err)
			if st.IsOptional() {
				rc.logf("  (%s failed; continuing)\n", st.Name())
				continue
			}
			return err
		}
		rc.Exec.completeStep(step, "")
		rc.logf("<< %s completed\n", st.Name())
	}
	return nil
}

func stepNameFor(stage string) string {
	switch stage {
	case stageCloneFetch:
		return model.StepClone // 具体 clone/fetch 由阶段内部再细分
	case stageEnsureRemote:
		return model.StepEnsureRemote
	case stagePush:
		return model.StepPush
	case stageWiki:
		return model.StepWiki
	case stageBundle:
		return "bundle"
	case stageBackupRemote:
		return "backup-remote"
	default:
		return stage
	}
}

const (
	stagePrepare      = "prepare"
	stageCloneFetch   = "clone-fetch"
	stageEnsureRemote = "ensure-remote"
	stagePush         = "push"
	stageWiki         = "wiki"
	stageBundle       = "bundle"
	stageBackupRemote = "backup-remote"
)
