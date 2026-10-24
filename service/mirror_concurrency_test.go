package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
)

// blockingCloneBackend 在**第一次** Clone 处阻塞,用来把一次镜像 run 钉在"进行中"。
//
// 为什么要能钉住:本用例要验的是"第二次执行在第一次还在跑时就被拒"——
// 如果第一次瞬间跑完,断言就退化成"跑完之后能再跑",测不到互斥。
type blockingCloneBackend struct {
	gitbackend.GitBackend
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (b *blockingCloneBackend) Clone(ctx context.Context, _ gitbackend.CloneOptions) error {
	b.mu.Lock()
	b.calls++
	n := b.calls
	b.mu.Unlock()
	if n == 1 {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("clone blocked by test")
}

// TestExecuteMirrorRun_SameChannelMutualExclusive 同通道的第二次执行必须被拒,
// 且必须**在第一次还在跑的时候**就被拒。
//
// 回归背景:原先唯一的保护是 `HasRunningByChannel`(查 DB 有没有在跑的 run),
// 那是 **check-then-act** —— 「查」与「建 run 记录」之间有窗口,
// 两个并发请求可以同时查到"没有",然后各跑一遍:
// 克隆目录互相踩、推 main 互相回退。
//
// 修法:创建 run **之前**同步占位(try-acquire),占位一直持有到整段 run 结束。
func TestExecuteMirrorRun_SameChannelMutualExclusive(t *testing.T) {
	m, repoDAO, db := setupMirrorServiceTest(t)
	ctx := context.Background()
	m.svc.bgCtx = context.Background()

	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "github/acme/app", Name: "app", Platform: "github",
		PlatformOwner: "acme", PlatformRepo: "app",
		CloneURL: "https://github.com/acme/app.git",
	}))

	ch := &model.MirrorChannel{
		Name: "ch-a", Mode: model.MirrorModePublish,
		RepoKey: "github/acme/app", Module: "github.com/acme/app",
	}
	require.NoError(t, db.Create(ch).Error)
	tgt := &model.MirrorTarget{
		ChannelID: ch.ID, Remote: "github",
		RepoURL: "https://github.com/acme/mirror.git", TargetModule: "github.com/acme/mirror",
	}
	require.NoError(t, db.Create(tgt).Error)

	blk := &blockingCloneBackend{entered: make(chan struct{}), release: make(chan struct{})}
	m.backend = blk

	run, err := m.ExecuteMirrorRun(ctx, ch.ID, ExecuteMirrorRunInput{
		TargetID: tgt.ID, Tags: []string{"v1.0.0"},
	})
	require.NoError(t, err)
	require.NotNil(t, run)

	// 等第一次真正进入 clone（即 run 在飞）
	select {
	case <-blk.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("第一次 run 未进入 clone")
	}

	// **把第一次的 run 记录从库里删掉** —— 模拟「DB 查不到在跑的 run」:
	// check-then-act 的竞态窗口(两个请求都在对方建记录前查过)、
	// 以及多实例部署里本进程看不见的那部分,都会出现这个状态。
	//
	// 此时同通道的第二次执行**仍必须被拒** —— 靠的是进程内占位,
	// 不能指望 DB 状态永远准确。
	require.NoError(t, db.Where("id = ?", run.ID).Delete(&model.MirrorRun{}).Error)

	// 第二次必须被拒：通道正在执行
	_, err = m.ExecuteMirrorRun(ctx, ch.ID, ExecuteMirrorRunInput{
		TargetID: tgt.ID, Tags: []string{"v1.0.1"},
	})
	require.Error(t, err, "同通道并发执行必须被拒(DB 查不到在跑的 run 也一样)")
	assert.Contains(t, err.Error(), "执行中的任务")

	// 不同通道不受影响（占位是按通道粒度的）
	ch2 := &model.MirrorChannel{
		Name: "ch-b", Mode: model.MirrorModePublish,
		RepoKey: "github/acme/app", Module: "github.com/acme/app",
	}
	require.NoError(t, db.Create(ch2).Error)
	tgt2 := &model.MirrorTarget{
		ChannelID: ch2.ID, Remote: "github",
		RepoURL: "https://github.com/acme/mirror2.git", TargetModule: "github.com/acme/mirror2",
	}
	require.NoError(t, db.Create(tgt2).Error)
	_, err = m.ExecuteMirrorRun(ctx, ch2.ID, ExecuteMirrorRunInput{
		TargetID: tgt2.ID, Tags: []string{"v1.0.0"},
	})
	require.NoError(t, err, "不同通道不该被牵连")

	// 收尾:放行第一次 run 并等**两个通道**的 run 全部结束
	// (后台 goroutine 还在写 DB 与工作目录,测试先退出会和 TempDir 清理抢)。
	close(blk.release)
	waitReservationReleased(t, m, ch.ID)
	waitReservationReleased(t, m, ch2.ID)
}

// waitReservationReleased 等通道占位释放(＝整段 run 结束)。
//
// 注意 reserveChannel 的 ok 语义是"**我拿到了**"而不是"被别人占着":
// 拿到 ⇒ 之前的执行已经放掉了占位 ⇒ 可以停。
func waitReservationReleased(t *testing.T, m *MirrorService, chID uint) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		release, acquired := m.reserveChannel(chID)
		if acquired {
			release() // 抢到的这一份立刻还回去,别把后续执行挡住
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("run 结束后占位未释放")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExecuteMirrorRun_ReservationReleasedAfterRun run 结束后必须**释放占位** ——
// 不释放的话通道会被永久锁死,再也无法发布。
func TestExecuteMirrorRun_ReservationReleasedAfterRun(t *testing.T) {
	m, repoDAO, db := setupMirrorServiceTest(t)
	ctx := context.Background()
	m.svc.bgCtx = context.Background()

	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "github/acme/app", Name: "app", Platform: "github",
		PlatformOwner: "acme", PlatformRepo: "app",
		CloneURL: "https://github.com/acme/app.git",
	}))
	ch := &model.MirrorChannel{
		Name: "ch-a", Mode: model.MirrorModePublish,
		RepoKey: "github/acme/app", Module: "github.com/acme/app",
	}
	require.NoError(t, db.Create(ch).Error)
	tgt := &model.MirrorTarget{
		ChannelID: ch.ID, Remote: "github",
		RepoURL: "https://github.com/acme/mirror.git", TargetModule: "github.com/acme/mirror",
	}
	require.NoError(t, db.Create(tgt).Error)

	blk := &blockingCloneBackend{entered: make(chan struct{}), release: make(chan struct{})}
	m.backend = blk

	_, err := m.ExecuteMirrorRun(ctx, ch.ID, ExecuteMirrorRunInput{
		TargetID: tgt.ID, Tags: []string{"v1.0.0"},
	})
	require.NoError(t, err)
	select {
	case <-blk.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("run 未进入 clone")
	}
	close(blk.release)

	// 等第一次 run 结束（占位释放）
	waitReservationReleased(t, m, ch.ID)
}
