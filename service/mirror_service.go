package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/oekit/concurrency/async"
	"github.com/oekit/concurrency/syncx"
	"github.com/oekit/redisx/distlock"
	"github.com/redis/go-redis/v9"
	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/lock"
	"github.com/yi-nology/git-ferry-core/mirror"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
	"github.com/yi-nology/go-git-platform/pkg/credential"
	"gorm.io/gorm"
)

// MirrorService 镜像通道编排:通道 CRUD、源仓库本地克隆管理、
// 预检/执行/验证,底层调用 git-ferry-core/mirror。
type MirrorService struct {
	svc      *Service
	channels *dao.MirrorChannelDAO
	runs     *dao.MirrorRunDAO
	backend  gitbackend.GitBackend
	cm       *credential.CryptoManager
	// locks 保证同一通道的进程内互斥（克隆/增量 fetch 不可重入）。
	// 用 oekit concurrency/syncx.KeyedGate(cap=1 → 每个 key 一把锁)。
	locks     *syncx.KeyedGate
	locksOnce sync.Once
	// reservations 是"通道执行权"的进程内占位表（try-acquire 语义）。
	//
	// 与 locks 分工：locks 是**排队**闸门（保护工作目录不被交错读写），
	// reservations 是**即拒**占位（同一通道同时只允许一次执行）。
	// key = channelID，值 = struct{}。
	reservations sync.Map
	// locker 配 redis 时提供跨实例互斥：抢不到 = 另一实例正在执行同一通道，
	// 直接返回忙碌错误而不是两边同时克隆（TTL 见 mirrorLockTTL，覆盖整次操作）。
	locker  *distlock.RedisLocker
	lockTTL time.Duration
	// lockClient 是 locker 底层的 redis 连接,由 Service.Stop 统一关闭。
	lockClient *redis.Client
}

// mirrorLockTTL 通道锁 TTL：覆盖一次克隆+推送的最长耗时。
// 远大于任务锁的 30s，避免操作中途锁过期导致并发进入。
const mirrorLockTTL = 30 * time.Minute

func NewMirrorService(svc *Service, db *gorm.DB) (*MirrorService, error) {
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{})
	if err != nil {
		return nil, errors.Wrap(err, "init git backend failed")
	}
	cm, err := credential.NewCryptoManager()
	if err != nil {
		return nil, errors.Wrap(err, "init crypto manager failed")
	}
	// 配 redis 时追加跨实例互斥；未配或 redis 抖动只降级为进程内互斥（有告警）。
	var locker *distlock.RedisLocker
	var lockClient *redis.Client
	if cfg := svc.GetConfig(); cfg != nil && cfg.Redis.Addr != "" {
		lockClient = lock.NewRedisClient(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
		locker = lock.NewLocker(lockClient)
	}
	return &MirrorService{
		svc:        svc,
		channels:   dao.NewMirrorChannelDAO(db),
		runs:       dao.NewMirrorRunDAO(db),
		backend:    backend,
		cm:         cm,
		locks:      syncx.NewKeyedGate(1),
		locker:     locker,
		lockTTL:    mirrorLockTTL,
		lockClient: lockClient,
	}, nil
}

// channelGate 返回本实例的进程内互斥闸门（懒初始化）。
// MirrorService 允许直接字面量构造（测试路径），不能假设 locks 已注入；
// KeyedGate 的零值 cap=0 不构成互斥，必须显式 NewKeyedGate(1)。
func (m *MirrorService) channelGate() *syncx.KeyedGate {
	m.locksOnce.Do(func() {
		if m.locks == nil {
			m.locks = syncx.NewKeyedGate(1)
		}
	})
	return m.locks
}

// lockChannelLocal 取进程内通道闸门(阻塞)。
//
// 闸门(cap=1)覆盖"源仓库工作目录"的独占:ensureRepo 的增量 fetch 会
// `git reset --hard` 改工作区 ⇒ 克隆/fetch 与发布**不能交错**。
//
// 用 oekit concurrency/syncx.KeyedGate(cap=1 → 每个 key 一把锁):
// 不必自维护 sync.Map + 类型断言,且 Acquire 感知 ctx 取消。
func (m *MirrorService) lockChannelLocal(ctx context.Context, chID uint) (func(), error) {
	return m.channelGate().Acquire(ctx, fmt.Sprintf("repo-%d", chID))
}

// tryLockRemote 抢跨实例通道锁(配 redis 时)。
//
// 返回 (release, acquired, err):
//   - acquired=true  :拿到锁,调用方**必须**在结束时 release;
//   - acquired=false :被别的实例占用 ⇒ 忙碌(不排队);
//   - redis 抖动     :降级为"仅进程内互斥",不阻断镜像任务(同 lockChannel 原语义)。
func (m *MirrorService) tryLockRemote(ctx context.Context, chID uint) (release func(), acquired bool, err error) {
	if m.locker == nil {
		return func() {}, true, nil
	}
	key := fmt.Sprintf("mirror-channel-%d", chID)
	id, lerr := m.locker.TryLock(ctx, key, m.lockTTL)
	switch {
	case lerr == nil:
		return func() {
			// 解锁不能挂在请求 ctx 上:请求早已取消 ⇒ 解锁必失败 ⇒ 锁要等 TTL。
			if uerr := m.locker.Unlock(context.WithoutCancel(ctx), key, id); uerr != nil {
				// 已过期易主或 redis 抖动：仅告警，TTL 会兜底
				slog.Debug("mirror: 释放跨实例锁", "channel", chID, "error", uerr)
			}
		}, true, nil
	case errors.Is(lerr, distlock.ErrLockNotAcquired):
		return func() {}, false, fmt.Errorf("通道 %d 正在其它实例执行,请稍后重试", chID)
	default:
		// redis 抖动：降级为进程内互斥，不阻断镜像任务
		slog.Warn("mirror: 跨实例锁获取失败,降级为进程内互斥", "channel", chID, "error", lerr)
		return func() {}, true, nil
	}
}

// lockChannel 获取通道级互斥：先取进程内互斥（阻塞，保持既有串行语义），
// 再抢跨实例锁（配 redis 时；被其它实例占用 → 忙碌错误，不排队等待）。
// 返回幂等 release；通道被另一实例占用时返回忙碌错误。
func (m *MirrorService) lockChannel(ctx context.Context, chID uint) (release func(), err error) {
	releaseLocal, err := m.lockChannelLocal(ctx, chID)
	if err != nil {
		return nil, err
	}
	releaseRemote, acquired, rerr := m.tryLockRemote(ctx, chID)
	if !acquired {
		releaseLocal()
		return func() {}, rerr
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseLocal()
			releaseRemote()
		})
	}, nil
}

// reserveChannel 同步占位"通道执行权"(进程内,try-acquire 语义)。
//
// 为什么不用 KeyedGate:它是**排队**语义 —— HTTP 请求会一直阻塞到上一轮
// 发布结束(可能几分钟)⇒ 接口假死。这里要的是"满额即拒":占不上立刻回忙碌错误。
//
// 为什么必须有它:`HasRunningByChannel` 是 **check-then-act** ——
// 两个并发请求可以同时查到"没有在跑的 run",然后各建一条 run 各跑一遍。
// 占位必须**同步**发生在创建 run 记录之前,窗口才为零。
func (m *MirrorService) reserveChannel(chID uint) (release func(), ok bool) {
	if _, loaded := m.reservations.LoadOrStore(chID, struct{}{}); loaded {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { m.reservations.Delete(chID) }) }, true
}

// ---------- 请求/响应结构 ----------

type MirrorTargetInput struct {
	Remote       string `json:"remote"`
	RepoURL      string `json:"repoUrl"`
	TargetModule string `json:"targetModule"`
	CredType     string `json:"credType"`
	// Credential 明文仅在创建/更新时提交,格式随 CredType:
	// token→token 值;ssh_key→私钥 PEM。存储前整体加密。
	Credential string `json:"credential"`
	Username   string `json:"username"`
	// InsecureSkipTLS 跳过 TLS 校验(SSH 下等价于跳过主机密钥校验)。
	InsecureSkipTLS bool `json:"insecureSkipTLS"`
	// HostKeyFingerprint 期望的 SSH 主机公钥指纹("SHA256:xxx" 或裸 base64)。
	// 设置后连接时校验公钥指纹必须一致,否则拒绝(防 MITM)。
	HostKeyFingerprint string `json:"hostKeyFingerprint"`
	// KnownHostsPath 指定 known_hosts 文件(空 = 用 ~/.ssh/known_hosts)。
	KnownHostsPath string `json:"knownHostsPath"`
}

// MirrorScrubRuleInput 是一条痕迹清理规则的入参。
//
// 形状与历史同步侧一致(见 service.ScrubRuleDTO)。
type MirrorScrubRuleInput struct {
	// Name 规则名(出现在命中报告里)。
	Name string `json:"name"`
	// Category 类别分组(如 "internal-host" / "secret"),仅用于展示。
	Category string `json:"category"`
	// Pattern 正则源码。
	Pattern string `json:"pattern"`
	// Severity 处理强度:auto / mandatory,默认 auto。
	//
	// mandatory(密钥类)命中即**中止发布** —— 不做自动替换,
	// 也不允许它进公开 module。
	Severity string `json:"severity"`
	// Replacement 命中后的替换文本;auto 且为空表示删除。
	Replacement string `json:"replacement"`
}

type CreateMirrorChannelInput struct {
	Name    string              `json:"name"`
	Mode    string              `json:"mode"`
	RepoKey string              `json:"repoKey"`
	Targets []MirrorTargetInput `json:"targets"`
	// ScrubRules 痕迹清理规则:发布前把内容里的内网痕迹清掉。
	//
	// 身份改写只管 module path;内网 registry / 部署代号 / 令牌
	// 原样进公开 module 就是泄漏。
	ScrubRules []MirrorScrubRuleInput `json:"scrubRules"`
}

// compileScrubRules 把痕迹规则入参编译成可执行的规则。
//
// 正则编不过就报错 —— 错误规则留在配置里,只会在发布那一刻炸。
func compileScrubRules(in []MirrorScrubRuleInput) ([]mirror.ScrubRule, error) {
	out := make([]mirror.ScrubRule, 0, len(in))
	for i, r := range in {
		pattern, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, errors.Wrapf(err, "第 %d 条痕迹规则的 pattern 非法", i+1)
		}
		severity := mirror.ScrubAuto
		if r.Severity == "mandatory" {
			severity = mirror.ScrubMandatory
		}
		out = append(out, mirror.ScrubRule{
			Name: r.Name, Category: r.Category, Pattern: pattern,
			Severity: severity, Replacement: r.Replacement,
		})
	}
	return out, nil
}

// channelScrubRules 取通道的痕迹规则(已编译)。
func channelScrubRules(ch *model.MirrorChannel) ([]mirror.ScrubRule, error) {
	if ch == nil || ch.ScrubRules == "" {
		return nil, nil
	}
	var in []MirrorScrubRuleInput
	if err := json.Unmarshal([]byte(ch.ScrubRules), &in); err != nil {
		return nil, errors.Wrap(err, "通道的痕迹规则配置损坏")
	}
	return compileScrubRules(in)
}

// ---------- 通道 CRUD ----------

// CreateMirrorChannel 创建通道。publish 模式会先拉取源仓库并读取 go.mod,
// 提前暴露仓库不可达 / module 缺失 / 目标身份相同等问题。
func (m *MirrorService) CreateMirrorChannel(ctx context.Context, in CreateMirrorChannelInput) (*model.MirrorChannel, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, errors.New("通道名称不能为空")
	}
	if in.Mode == "" {
		in.Mode = model.MirrorModePublish
	}
	if in.Mode != model.MirrorModePublish && in.Mode != model.MirrorModeBackup {
		return nil, fmt.Errorf("不支持的模式: %s", in.Mode)
	}
	if len(in.Targets) == 0 {
		return nil, errors.New("至少需要一个目标")
	}

	repo, err := m.svc.GetRepoByKey(in.RepoKey)
	if err != nil || repo == nil {
		return nil, fmt.Errorf("源仓库不存在: %s", in.RepoKey)
	}

	ch := &model.MirrorChannel{
		Name:    in.Name,
		Mode:    in.Mode,
		RepoKey: in.RepoKey,
	}
	if len(in.ScrubRules) > 0 {
		// 规则在**创建时**就要校验(正则编不过就别落库),
		// 否则错误会推迟到发布时才暴露。
		if _, err := compileScrubRules(in.ScrubRules); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(in.ScrubRules)
		if err != nil {
			return nil, errors.Wrap(err, "marshal scrub rules failed")
		}
		ch.ScrubRules = string(raw)
	}
	if in.Mode == model.MirrorModePublish {
		dir, cleanup, err := m.ensureRepo(ctx, ch)
		if err != nil {
			return nil, fmt.Errorf("拉取源仓库失败: %w", err)
		}
		defer cleanup()
		ch.Module, err = mirror.ReadModulePath(dir)
		if err != nil {
			return nil, fmt.Errorf("读取源仓库 go.mod 失败: %w", err)
		}
	}

	if err := m.channels.Create(ch); err != nil {
		return nil, errors.Wrap(err, "create mirror channel failed")
	}
	for i := range in.Targets {
		if _, err := m.createTarget(ch, &in.Targets[i]); err != nil {
			_ = m.channels.Delete(ch.ID)
			return nil, err
		}
	}
	return m.GetMirrorChannel(ch.ID)
}

func (m *MirrorService) createTarget(ch *model.MirrorChannel, in *MirrorTargetInput) (*model.MirrorTarget, error) {
	if err := validateTargetInput(in, true); err != nil {
		return nil, err
	}
	if ch.Mode == model.MirrorModePublish {
		if in.TargetModule == "" {
			return nil, errors.New("目标 module 不能为空")
		}
		if in.TargetModule == ch.Module {
			return nil, fmt.Errorf("目标 module 与源相同(%s),无改写意义", ch.Module)
		}
	}
	t := &model.MirrorTarget{
		ChannelID:    ch.ID,
		Remote:       in.Remote,
		RepoURL:      in.RepoURL,
		TargetModule: in.TargetModule,
		CredType:     in.CredType,
		Username:     in.Username,
		// SSH 主机密钥钉扎:配了就必须**存下来** ——
		// 钉扎信息丢了等于没钉(重启后悄悄退化成不校验)。
		InsecureSkipTLS:    in.InsecureSkipTLS,
		HostKeyFingerprint: in.HostKeyFingerprint,
		KnownHostsPath:     in.KnownHostsPath,
	}
	if in.Credential != "" {
		enc, err := m.cm.Encrypt(in.Credential)
		if err != nil {
			return nil, errors.Wrap(err, "encrypt credential failed")
		}
		t.Credential = enc
	}
	if err := m.channels.CreateTarget(t); err != nil {
		return nil, errors.Wrap(err, "create mirror target failed")
	}
	t.HasCredential = t.Credential != ""
	return t, nil
}

// validateTargetInput 校验目标入参。
// requireCredential=false 用于更新路径：凭据留空表示保持原凭据（见 UpdateMirrorTarget）。
func validateTargetInput(in *MirrorTargetInput, requireCredential bool) error {
	if strings.TrimSpace(in.Remote) == "" || strings.HasPrefix(in.Remote, "-") {
		return fmt.Errorf("远端名非法: %q", in.Remote)
	}
	if strings.TrimSpace(in.RepoURL) == "" {
		return errors.New("目标仓库 URL 不能为空")
	}
	switch in.CredType {
	case "", model.MirrorCredNone:
		in.CredType = model.MirrorCredNone
	case model.MirrorCredToken, model.MirrorCredSSHKey:
		if requireCredential && in.Credential == "" {
			return fmt.Errorf("凭据类型 %s 需要提供凭据内容", in.CredType)
		}
	default:
		return fmt.Errorf("不支持的凭据类型: %s", in.CredType)
	}
	return nil
}

// UpdateMirrorTarget 更新目标(凭据为空串表示保持不变)。
func (m *MirrorService) UpdateMirrorTarget(ctx context.Context, targetID uint, in MirrorTargetInput) (*model.MirrorTarget, error) {
	t, err := m.channels.FindTargetByID(targetID)
	if err != nil {
		return nil, fmt.Errorf("目标不存在: %d", targetID)
	}
	// 凭据空串 = 保持原凭据（不要求重传）
	if err := validateTargetInput(&in, false); err != nil {
		return nil, err
	}
	t.Remote = in.Remote
	t.RepoURL = in.RepoURL
	t.TargetModule = in.TargetModule
	t.CredType = in.CredType
	t.Username = in.Username
	t.InsecureSkipTLS = in.InsecureSkipTLS
	t.HostKeyFingerprint = in.HostKeyFingerprint
	t.KnownHostsPath = in.KnownHostsPath
	if in.Credential != "" {
		enc, err := m.cm.Encrypt(in.Credential)
		if err != nil {
			return nil, errors.Wrap(err, "encrypt credential failed")
		}
		t.Credential = enc
	}
	if err := m.channels.UpdateTarget(t); err != nil {
		return nil, err
	}
	t.HasCredential = t.Credential != ""
	return t, nil
}

func (m *MirrorService) DeleteMirrorTarget(targetID uint) error {
	return m.channels.DeleteTarget(targetID)
}

func (m *MirrorService) ListMirrorChannels(page dao.Pagination) ([]*model.MirrorChannel, int64, error) {
	return m.channels.FindAll(page)
}

func (m *MirrorService) GetMirrorChannel(id uint) (*model.MirrorChannel, error) {
	ch, err := m.channels.FindByID(id)
	if err != nil {
		return nil, fmt.Errorf("通道不存在: %d", id)
	}
	targets, err := m.channels.FindTargets(id)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		t.HasCredential = t.Credential != ""
	}
	ch.Targets = targets
	return ch, nil
}

func (m *MirrorService) DeleteMirrorChannel(confirm string, id uint) error {
	ch, err := m.channels.FindByID(id)
	if err != nil {
		return fmt.Errorf("通道不存在: %d", id)
	}
	if confirm != ch.Name {
		return fmt.Errorf("确认名不匹配,请输入通道名 %q 以确认删除", ch.Name)
	}
	return m.channels.Delete(id)
}

// ---------- 目标连接测试 ----------

func (m *MirrorService) TestMirrorTarget(ctx context.Context, targetID uint) error {
	t, err := m.channels.FindTargetByID(targetID)
	if err != nil {
		return fmt.Errorf("目标不存在: %d", targetID)
	}
	auth, err := m.targetAuth(t)
	if err != nil {
		return err
	}
	return m.backend.TestConnection(ctx, t.RepoURL, auth)
}

// ---------- 源仓库本地克隆管理 ----------

// ensureRepo 返回源仓库的本地工作克隆(无则克隆,有则增量 fetch 全部 tag)。
// cleanup 在失败时清理目录;成功时保留供后续增量操作。
//
// 本函数**自己加锁**（进程内闸门 + 跨实例锁）。
// 已经持锁的调用方（如整段持锁的发布 run）必须改用 ensureRepoLocked ——
// KeyedGate 不是可重入的,嵌套 Acquire 会死锁。
func (m *MirrorService) ensureRepo(ctx context.Context, ch *model.MirrorChannel) (dir string, cleanup func(), err error) {
	unlock, lerr := m.lockChannel(ctx, ch.ID)
	if lerr != nil {
		return "", func() {}, lerr
	}
	defer unlock()
	return m.ensureRepoLocked(ctx, ch)
}

// ensureRepoLocked 是 ensureRepo 的**无锁**版本:调用方必须已持有通道锁。
func (m *MirrorService) ensureRepoLocked(ctx context.Context, ch *model.MirrorChannel) (dir string, cleanup func(), err error) {
	cleanup = func() {}
	repo, err := m.svc.GetRepoByKey(ch.RepoKey)
	if err != nil || repo == nil {
		return "", cleanup, fmt.Errorf("源仓库不存在: %s", ch.RepoKey)
	}
	auth := m.repoAuth(repo)

	dir = filepath.Join(m.svc.GetConfig().Git.TempDir, "mirror", fmt.Sprintf("channel-%d", ch.ID), "repo")
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(statErr) {
		if err = os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return "", cleanup, err
		}
		if err = m.backend.Clone(ctx, gitbackend.CloneOptions{URL: repo.CloneURL, Path: dir, Auth: auth}); err != nil {
			return "", cleanup, fmt.Errorf("clone %s 失败: %w", repo.CloneURL, err)
		}
		slog.Info("mirror: 已克隆源仓库", "channel", ch.ID, "dir", dir)
		return dir, cleanup, nil
	}

	// 增量:fetch 全部 tag(远端固定 origin;失败则尝试重建)
	if _, err = m.backend.Fetch(ctx, gitbackend.FetchOptions{RepoPath: dir, Remote: "origin", Tags: true, Auth: auth}); err != nil {
		slog.Warn("mirror: 增量 fetch 失败,重建克隆", "channel", ch.ID, "error", err)
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return "", cleanup, rmErr
		}
		if err = m.backend.Clone(ctx, gitbackend.CloneOptions{URL: repo.CloneURL, Path: dir, Auth: auth}); err != nil {
			return "", cleanup, fmt.Errorf("re-clone 失败: %w", err)
		}
		return dir, cleanup, nil
	}
	// fetch 只更新 refs/remotes/origin/*,**工作区仍是旧内容**;
	// 而发布是从工作区取材(go.mod、工作树文件)—— 不同步就等于每次都发旧快照
	// (实测:源仓已提交 go.mod,发布仍因"工作区没有 go.mod"失败)。
	if err = alignWorktreeToSource(ctx, dir); err != nil {
		return "", cleanup, err
	}
	return dir, cleanup, nil
}

// alignWorktreeToSource 把镜像工作区对齐到源端默认分支的最新提交。
//
// 源端是唯一真相(镜像语义):工作区必须等于 origin/<默认分支>。
// origin/HEAD 由 clone 建立;万一没有就退回 origin/main / origin/master。
func alignWorktreeToSource(ctx context.Context, dir string) error {
	for _, ref := range []string{"origin/HEAD", "origin/main", "origin/master"} {
		out, err := runGitQuiet(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		if _, err := runGitQuiet(ctx, dir, "reset", "--hard", ref); err != nil {
			return fmt.Errorf("对齐工作区到 %s 失败: %w", ref, err)
		}
		return nil
	}
	// 源端还没有任何分支:保持原样(后续发布会按缺 tag 处理)。
	return nil
}

// ensureTargetRemote 把目标仓库配置为克隆里的远端(存在则核对 URL)。
func (m *MirrorService) ensureTargetRemote(ctx context.Context, dir string, t *model.MirrorTarget, auth gitbackend.AuthConfig) error {
	url, err := m.backend.GetRemoteURL(ctx, dir, t.Remote)
	if err != nil {
		return m.backend.AddRemote(ctx, dir, t.Remote, t.RepoURL)
	}
	if url != t.RepoURL {
		if err := m.backend.RemoveRemote(ctx, dir, t.Remote); err != nil {
			return err
		}
		return m.backend.AddRemote(ctx, dir, t.Remote, t.RepoURL)
	}
	return nil
}

// repoAuth 源仓库读取凭据。规则统一收敛到 executor.BuildRepoAuth
// (repo token 优先,回退平台 token;skipTLS 与 SSH host key 指纹/knownHosts
// 一并回填,git HTTPS 只认 Basic 不认 Bearer)。
func (m *MirrorService) repoAuth(repo *model.Repo) gitbackend.AuthConfig {
	var p *model.Platform
	if repo != nil && repo.PlatformID > 0 {
		if got, err := m.svc.GetPlatformByID(context.Background(), repo.PlatformID); err == nil && got != nil {
			p = got
		}
	}
	return executor.BuildRepoAuth(repo, p)
}

// applyTargetPlatformTLS 把目标仓所属平台的 TLS 设置(自签证书/过期证书)补到 auth 上。
//
// 目标远端通常是**另一个**平台上的仓(源在 GitHub、目标在自建 Gitea 很常见),
// 而它的 TLS 策略登记在"平台"上,目标记录本身没有这个字段。
// 缺了这一步,自建实例的"远端一致性检查"会因为证书问题失败
// (实测:*.enjoye.top 证书过期 ⇒ 镜像发布在一致性检查这一步直接中止)。
func (m *MirrorService) applyTargetPlatformTLS(ctx context.Context, t *model.MirrorTarget, auth gitbackend.AuthConfig) gitbackend.AuthConfig {
	if auth.InsecureSkipTLS || t == nil {
		return auth
	}
	host := hostOfRepoURL(t.RepoURL)
	if host == "" {
		return auth
	}
	platforms, err := m.svc.ListPlatforms(ctx)
	if err != nil {
		return auth
	}
	for _, p := range platforms {
		if p == nil || !p.SkipTLSVerify {
			continue
		}
		if strings.EqualFold(hostOfRepoURL(p.InstanceURL), host) {
			auth.InsecureSkipTLS = true
			return auth
		}
	}
	return auth
}

// hostOfRepoURL 取仓库地址的 host(兼容 https:// 与 scp 式 git@host:path)。
// 平台的 instance_url 常常是**裸主机名**(如 git.enjoye.top),也要认出来。
func hostOfRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Hostname()
	}
	// scp 式:git@host:owner/repo.git
	if i := strings.Index(raw, "@"); i >= 0 {
		rest := raw[i+1:]
		if j := strings.IndexAny(rest, ":/"); j > 0 {
			return rest[:j]
		}
	}
	// 裸主机名:git.enjoye.top(无 scheme、无路径、无 @)
	if !strings.ContainsAny(raw, ":/@") {
		return raw
	}
	return ""
}

// targetAuth 目标仓库凭据(解密 Credential)。
func (m *MirrorService) targetAuth(t *model.MirrorTarget) (gitbackend.AuthConfig, error) {
	switch t.CredType {
	case "", model.MirrorCredNone:
		return gitbackend.AuthConfig{Type: gitbackend.AuthNone}, nil
	case model.MirrorCredToken:
		plain, err := m.cm.Decrypt(t.Credential)
		if err != nil {
			return gitbackend.AuthConfig{}, errors.Wrap(err, "decrypt credential failed")
		}
		user := t.Username
		if user == "" {
			user = "oauth2"
		}
		return gitbackend.NewHTTPBasicAuth(user, plain), nil
	case model.MirrorCredSSHKey:
		plain, err := m.cm.Decrypt(t.Credential)
		if err != nil {
			return gitbackend.AuthConfig{}, errors.Wrap(err, "decrypt credential failed")
		}
		if plain == "" {
			// 构造器把空 key 折叠成 AuthNone;这里保持原语义(空 key 是坏配置,
			// 走 SSH 认证失败而不是悄悄降级成匿名)。
			return gitbackend.AuthConfig{Type: gitbackend.AuthSSH}, nil
		}
		auth := gitbackend.NewSSHKeyContentAuth(plain, "")
		// **SSH 主机密钥钉扎**:目标侧之前没法钉 ——
		// 于是同一次发布里「检查路径」fail-closed、「推送路径」TOFU。
		// 现在两个路径都从这一份 AuthConfig 走,配了就都校验。
		auth.HostKeyFingerprint = t.HostKeyFingerprint
		auth.KnownHostsPath = t.KnownHostsPath
		auth.InsecureSkipTLS = t.InsecureSkipTLS
		return auth, nil
	default:
		return gitbackend.AuthConfig{}, fmt.Errorf("不支持的凭据类型: %s", t.CredType)
	}
}

// ---------- 预检 ----------

// PreviewMirrorRun 对单个 tag 做预检(DryRun,不推送)。
func (m *MirrorService) PreviewMirrorRun(ctx context.Context, channelID, targetID uint, tag string) (*mirror.TagReport, error) {
	ch, t, err := m.loadChannelAndTarget(channelID, targetID)
	if err != nil {
		return nil, err
	}
	if ch.Mode != model.MirrorModePublish {
		return nil, errors.New("预检仅支持 publish 模式")
	}
	// 预检整段持通道闸门:与发布同一把 ⇒ 预检不会读到"发布改到一半"的工作区。
	unlock, lerr := m.lockChannelLocal(ctx, channelID)
	if lerr != nil {
		return nil, lerr
	}
	defer unlock()
	dir, cleanup, err := m.ensureRepoLocked(ctx, ch)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rep, err := m.publishOne(ctx, ch, t, dir, tag, false, true)
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// ---------- 执行 ----------

type ExecuteMirrorRunInput struct {
	TargetID       uint     `json:"targetId"`
	Tags           []string `json:"tags"`
	AllowOverwrite bool     `json:"allowOverwrite"`
	// Kind 省略时为 publish。
	Kind string `json:"kind"`
	// Module/Version 供 verify 类别使用。
	Version string `json:"-"`
}

// ExecuteMirrorRun 创建并异步执行一条镜像 run。同一通道同时只允许一个
// 进行中的执行(含预检外的写操作),避免克隆目录竞争。
func (m *MirrorService) ExecuteMirrorRun(ctx context.Context, channelID uint, in ExecuteMirrorRunInput) (*model.MirrorRun, error) {
	ch, t, err := m.loadChannelAndTarget(channelID, in.TargetID)
	if err != nil {
		return nil, err
	}
	kind := in.Kind
	if kind == "" {
		kind = model.MirrorKindPublish
	}
	if kind != model.MirrorKindPublish && kind != model.MirrorKindVerify {
		return nil, fmt.Errorf("不支持的执行类别: %s", kind)
	}
	if len(in.Tags) == 0 {
		return nil, errors.New("Tags 至少需要一个")
	}
	// --- 同通道执行权:**同步占位** ---
	//
	// 原来的保护只有 `HasRunningByChannel`(查 DB 有没有在跑的 run),那是
	// **check-then-act**:两个并发请求可以同时查到"没有",然后各建一条 run
	// 各跑一遍 —— 克隆目录竞争 + 推 main 互相回退。
	// 占位必须发生在**创建 run 记录之前**,竞争窗口才为零。
	releaseReserve, reserved := m.reserveChannel(channelID)
	if !reserved {
		return nil, errors.New("该通道有执行中的任务,请稍后再试")
	}
	// 跨实例:配 redis 时抢分布式锁,被别的实例占用即忙碌(不排队)。
	releaseRemote, acquired, lerr := m.tryLockRemote(ctx, channelID)
	if !acquired {
		releaseReserve()
		if lerr != nil {
			return nil, lerr
		}
		return nil, errors.New("该通道正在其它实例执行,请稍后再试")
	}

	// DB 里的"有在跑的 run"仍然查一遍:覆盖**进程重启后遗留的僵尸 run**
	// 与多实例部署里本进程看不到的那部分(占位只在进程内有效)。
	running, err := m.runs.HasRunningByChannel(channelID)
	if err != nil {
		releaseRemote()
		releaseReserve()
		return nil, err
	}
	if running {
		releaseRemote()
		releaseReserve()
		return nil, errors.New("该通道有执行中的任务,请稍后再试")
	}

	tagsJSON, _ := json.Marshal(in.Tags)
	run := &model.MirrorRun{
		ChannelID:      channelID,
		TargetID:       in.TargetID,
		Kind:           kind,
		Tags:           strings.Trim(string(tagsJSON), "[]"),
		Status:         model.MirrorRunPending,
		AllowOverwrite: in.AllowOverwrite,
		TagStatuses:    "{}",
		Steps:          "[]",
	}
	if err := m.runs.Create(run); err != nil {
		releaseRemote()
		releaseReserve()
		return nil, errors.Wrap(err, "create mirror run failed")
	}

	// 异步执行:挂到 Service 的后台上下文,随服务关停而取消
	runCtx, cancel := context.WithCancel(m.svc.bgCtx)
	m.svc.wg.Add(1)
	async.GoSafe(func() {
		defer m.svc.wg.Done()
		defer cancel()
		// 执行权在**整段 run 结束**后才释放 —— 不是"创建完 run 记录"就释放。
		defer releaseRemote()
		defer releaseReserve()
		m.execute(runCtx, run.ID, ch, t, in)
	})
	return run, nil
}

func (m *MirrorService) execute(ctx context.Context, runID uint, ch *model.MirrorChannel, t *model.MirrorTarget, in ExecuteMirrorRunInput) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mirror run panic", "run", runID, "panic", r)
			m.failRun(runID, fmt.Sprintf("内部错误: %v", r))
		}
	}()

	// 整段 run 持通道闸门(进程内串行)。
	//
	// 必须覆盖到**推送结束**:ensureRepo 的增量 fetch 会 `git reset --hard`
	// 改工作区,而发布正是从工作区取材 ⇒ fetch 与发布交错会把发布内容改花。
	//
	// 注意这里只取**本地闸门**:跨实例锁已在 ExecuteMirrorRun 持有,
	// 再抢一次会因"自己已持有"而被判为占用 ⇒ 反而死锁。
	unlock, lerr := m.lockChannelLocal(ctx, ch.ID)
	if lerr != nil {
		slog.Error("mirror run 等待通道闸门失败", "run", runID, "channel", ch.ID, "error", lerr)
		m.failRun(runID, fmt.Sprintf("等待通道锁失败: %v", lerr))
		return
	}
	defer unlock()

	run, err := m.runs.FindByID(runID)
	if err != nil {
		slog.Error("mirror run load failed", "run", runID, "error", err)
		return
	}
	now := time.Now()
	run.Status = model.MirrorRunRunning
	run.StartedAt = &now
	run.Steps = marshalSteps([]model.MirrorStep{{Name: "准备源仓库", Status: "running"}})
	_ = m.runs.Update(run)

	var runErr error
	switch ch.Mode {
	case model.MirrorModePublish:
		runErr = m.executePublish(ctx, run, ch, t, in)
	case model.MirrorModeBackup:
		runErr = errors.New("backup 模式将在 P1 提供")
	}

	fin := time.Now()
	run.FinishedAt = &fin
	run.DurationMs = fin.Sub(now).Milliseconds()
	if runErr != nil {
		if run.Status == model.MirrorRunRunning {
			run.Status = model.MirrorRunFailed
		}
		run.Error = runErr.Error()
	} else if run.Status == model.MirrorRunRunning {
		run.Status = model.MirrorRunSuccess
	}
	_ = m.runs.Update(run)
	slog.Info("mirror run finished", "run", runID, "status", run.Status)
}

// executePublish 逐 tag 调用 core Publish,行级结果互不阻塞。
//
// 调用方(execute)已持有通道闸门 ⇒ 这里用 ensureRepoLocked(不再嵌套加锁)。
func (m *MirrorService) executePublish(ctx context.Context, run *model.MirrorRun, ch *model.MirrorChannel, t *model.MirrorTarget, in ExecuteMirrorRunInput) error {
	dir, cleanup, err := m.ensureRepoLocked(ctx, ch)
	if err != nil {
		return err
	}
	_ = cleanup

	steps := []model.MirrorStep{{Name: "准备源仓库", Status: "success"}}
	statuses := map[string]*model.TagStatus{}

	if err := m.ensureTargetRemote(ctx, dir, t, gitbackend.AuthConfig{Type: gitbackend.AuthNone}); err != nil {
		return fmt.Errorf("配置目标远端失败: %w", err)
	}

	var divergent, failed int
	var lastReport *mirror.TagReport
	for _, tag := range splitTags(run.Tags) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		step := model.MirrorStep{Name: "发布 " + tag, Status: "running"}
		steps = append(steps, step)
		run.Steps = marshalSteps(steps)
		_ = m.runs.Update(run)

		rep, tagErr := m.publishOne(ctx, ch, t, dir, tag, in.AllowOverwrite, false)
		idx := len(steps) - 1
		if tagErr != nil {
			if isDivergentError(tagErr) {
				divergent++
				steps[idx].Status = "failed"
				steps[idx].Detail = "远端内容不一致,已拒绝覆盖"
				statuses[tag] = &model.TagStatus{Tag: tag, Status: model.MirrorRunDivergent, Detail: tagErr.Error()}
				run.Status = model.MirrorRunDivergent
			} else {
				failed++
				steps[idx].Status = "failed"
				steps[idx].Detail = strutil.Truncate(tagErr.Error(), 512)
				statuses[tag] = &model.TagStatus{Tag: tag, Status: model.MirrorRunFailed, Detail: strutil.Truncate(tagErr.Error(), 512)}
			}
		} else {
			steps[idx].Status = "success"
			steps[idx].Detail = "commit=" + rep.Commit
			statuses[tag] = &model.TagStatus{Tag: tag, Status: model.MirrorRunSuccess, Commit: rep.Commit, Tree: rep.Tree}
			lastReport = rep
		}
		run.Steps = marshalSteps(steps)
		run.TagStatuses = marshalTagStatuses(statuses)
		if lastReport != nil {
			run.Report = marshalJSON(lastReport)
		}
		_ = m.runs.Update(run)
	}
	if failed > 0 || divergent > 0 {
		return fmt.Errorf("部分 tag 执行失败(失败 %d,分歧 %d)", failed, divergent)
	}
	return nil
}

// publishOne 组装 core Options 执行单个 tag。
//
// dir 是**已准备好**的源仓库工作目录(调用方负责 prepare 与加锁):
// 之前这里自己调 ensureRepo ⇒ 每个 tag 都会重新 fetch 一次(同一轮 run 里 N+1 次),
// 而且会和调用方持有的通道闸门嵌套(死锁)。
func (m *MirrorService) publishOne(ctx context.Context, ch *model.MirrorChannel, t *model.MirrorTarget, dir, tag string, allowOverwrite, dryRun bool) (*mirror.TagReport, error) {
	// 预检/发布都要先把目标仓库接为本地克隆的远端
	if err := m.ensureTargetRemote(ctx, dir, t, gitbackend.AuthConfig{Type: gitbackend.AuthNone}); err != nil {
		return nil, fmt.Errorf("配置目标远端失败: %w", err)
	}
	auth, err := m.targetAuth(t)
	if err != nil {
		return nil, err
	}
	// 目标远端的 TLS 策略在**它所属的平台**上(自建实例常见自签/过期证书),
	// 目标记录本身没有这个字段 ⇒ 必须在这里按 host 找回平台并补上,
	// 否则"远端一致性检查"会因证书问题直接中止发布。
	auth = m.applyTargetPlatformTLS(ctx, t, auth)
	scrubRules, err := channelScrubRules(ch)
	if err != nil {
		return nil, err
	}
	rep2, err := mirror.Publish(ctx, mirror.Options{
		RepoDir: dir,
		// 与全系统其余部分同一个后端(生产配置 git.backend=gogit)
		BackendType:    m.svc.backendType(),
		Mapping:        mirror.Mapping{Source: ch.Module, Target: t.TargetModule},
		Tags:           []string{tag},
		Remote:         t.Remote,
		Auth:           auth,
		AllowOverwrite: allowOverwrite,
		DryRun:         dryRun,
		// 痕迹清理:身份改写只管 module path;
		// 内网 registry / 部署代号 / 令牌得靠规则清。
		ScrubRules: scrubRules,
	})
	if err != nil {
		return nil, err
	}
	if len(rep2.Tags) == 0 {
		return nil, errors.New("空报告")
	}
	return &rep2.Tags[0], nil
}

// VerifyMirrorTargetVersion 验证目标 module@tag 可被 go 工具链消费。
func (m *MirrorService) VerifyMirrorTargetVersion(ctx context.Context, channelID, targetID uint, tag string) (*model.MirrorRun, error) {
	_, t, err := m.loadChannelAndTarget(channelID, targetID)
	if err != nil {
		return nil, err
	}
	if t.TargetModule == "" {
		return nil, errors.New("该目标无 module 身份(backup 模式用 hash 比对验证,P1)")
	}
	if err := mirror.VerifyModule(ctx, t.TargetModule, tag); err != nil {
		return nil, err
	}
	run := &model.MirrorRun{
		ChannelID: channelID,
		TargetID:  targetID,
		Kind:      model.MirrorKindVerify,
		Tags:      `"` + tag + `"`,
		Status:    model.MirrorRunSuccess,
		Report:    marshalJSON(map[string]string{"module": t.TargetModule, "version": tag}),
	}
	now := time.Now()
	run.StartedAt, run.FinishedAt = &now, &now
	if err := m.runs.Create(run); err != nil {
		return nil, err
	}
	return run, nil
}

// ---------- 版本矩阵 ----------

// ListMirrorRuns 分页列出通道执行记录。
func (m *MirrorService) ListMirrorRuns(channelID uint, page dao.Pagination) ([]*model.MirrorRun, int64, error) {
	return m.runs.FindByChannel(channelID, page)
}

// GetMirrorRun 单条执行记录详情。
func (m *MirrorService) GetMirrorRun(id uint) (*model.MirrorRun, error) {
	run, err := m.runs.FindByID(id)
	if err != nil {
		return nil, fmt.Errorf("执行记录不存在: %d", id)
	}
	return run, nil
}

type MirrorVersionTarget struct {
	TargetID   uint   `json:"targetId"`
	Target     string `json:"target"`
	State      string `json:"state"` // unpublished|published|divergent|failed
	ExecutedAt string `json:"executedAt,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Tree       string `json:"tree,omitempty"`
	Verify     string `json:"verify"` // unverified|passed|failed
}

type MirrorVersion struct {
	Tag     string                `json:"tag"`
	Commit  string                `json:"commit,omitempty"`
	Targets []MirrorVersionTarget `json:"targets"`
}

type MirrorVersionsResult struct {
	Mode     string          `json:"mode"`
	Module   string          `json:"module"`
	Versions []MirrorVersion `json:"versions"`
}

// GetMirrorVersions 源仓库 tag 列表 ∪ 执行历史,合并出版本矩阵。
// ---------- 辅助 ----------

func (m *MirrorService) loadChannelAndTarget(channelID, targetID uint) (*model.MirrorChannel, *model.MirrorTarget, error) {
	ch, err := m.channels.FindByID(channelID)
	if err != nil {
		return nil, nil, fmt.Errorf("通道不存在: %d", channelID)
	}
	t, err := m.channels.FindTargetByID(targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("目标不存在: %d", targetID)
	}
	if t.ChannelID != channelID {
		return nil, nil, errors.New("目标不属于该通道")
	}
	return ch, t, nil
}

func (m *MirrorService) failRun(runID uint, msg string) {
	run, err := m.runs.FindByID(runID)
	if err != nil {
		return
	}
	fin := time.Now()
	run.Status = model.MirrorRunFailed
	run.Error = msg
	run.FinishedAt = &fin
	_ = m.runs.Update(run)
}

func isDivergentError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "拒绝覆盖")
}

func splitTags(csv string) []string {
	var tags []string
	for _, t := range strings.Split(strings.Trim(csv, `"`), ",") {
		t = strings.Trim(strings.TrimSpace(t), `"`)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func marshalSteps(steps []model.MirrorStep) string {
	b, _ := json.Marshal(steps)
	return string(b)
}

func marshalTagStatuses(m map[string]*model.TagStatus) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func marshalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}
