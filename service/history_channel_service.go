package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	errors "github.com/cockroachdb/errors"
	"github.com/oekit/concurrency/syncx"
	"github.com/oekit/redisx/distlock"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/executor"
	"github.com/yi-nology/git-ferry-core/lock"
	"github.com/yi-nology/git-ferry-core/mirror"
	"github.com/yi-nology/git-ferry-core/mirror/history"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/gitbackend"
	"github.com/yi-nology/go-git-platform/pkg/credential"
)

// HistoryService 历史同步服务。
//
// **接口是通道优先的**:
//   - 配置只存在通道里(通道 + 推送目标两张表),调用方不重传规则;
//   - 执行按 channel id 进行:plan / resolve / apply / push;
//   - apply 产出**执行记录(run)**,服务端自己记住产物路径,
//     调用方不必回传服务端的临时路径;
//   - run 同时是审计留痕与回滚锚点。
//
// 与 MirrorService(身份镜像)并存,语义不同:
//   - MirrorService:同一仓库的多个身份,每 tag 一个孤儿快照;
//   - HistoryService:两个有完整历史的仓库,按内容差异叠加,
//     目标树的既有提交原样保留。
type HistoryService struct {
	svc       *Service
	channels  *dao.HistoryChannelDAO
	targets   *dao.HistoryTargetDAO
	runs      *dao.HistoryRunDAO
	overrides *dao.HistoryOverrideDAO
	backend   gitbackend.GitBackend
	cm        *credential.CryptoManager
	// locks 保证同一通道的进程内互斥(移植/推送/回滚不可并发)。
	// 用 oekit concurrency/syncx.KeyedGate(cap=1 → 每个 key 一把锁)。
	locks     *syncx.KeyedGate
	locksOnce sync.Once
	// locker 配 redis 时提供跨实例互斥:抢不到 = 另一实例正在执行同一通道。
	locker  *distlock.RedisLocker
	lockTTL time.Duration
	// lockClient 是 locker 底层的 redis 连接,由 Service.Stop 统一关闭。
	lockClient *redis.Client
	// materializeRuns 统计物化次数(可观测性:一次同步跑了多少遍流水线)。
	//
	// **一键同步应当是 1**:plan/resolve/物化/门禁都只跑一遍。
	// 旧实现把流水线跑了两遍(预检一遍、执行一遍)—— 实仓上
	// plan+resolve≈65s、物化≈50s、类型门禁≈60s,等于把时间翻倍;
	// 而且两遍算出的 head 可能不同,预检报告说的就不是要推的那棵树。
	materializeRuns atomic.Int64
}

// MaterializeRuns 返回本进程累计的物化次数(可观测性指标)。
func (h *HistoryService) MaterializeRuns() int64 { return h.materializeRuns.Load() }

// historyLockTTL 通道锁 TTL:覆盖一次移植+推送的最长耗时。
// 远大于任务锁的 30s,避免操作中途锁过期导致并发进入。
const historyLockTTL = 30 * time.Minute

// NewHistoryService 构造历史同步服务。
func NewHistoryService(svc *Service, db *gorm.DB) (*HistoryService, error) {
	backend, err := gitbackend.NewGitBackend(gitbackend.Options{})
	if err != nil {
		return nil, errors.Wrap(err, "init git backend failed")
	}
	cm, err := credential.NewCryptoManager()
	if err != nil {
		return nil, errors.Wrap(err, "init credential manager failed")
	}
	// 配 redis 时追加跨实例互斥;未配或 redis 抖动只降级为进程内互斥(有告警)。
	var locker *distlock.RedisLocker
	var lockClient *redis.Client
	if cfg := svc.GetConfig(); cfg != nil && cfg.Redis.Addr != "" {
		lockClient = lock.NewRedisClient(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
		locker = lock.NewLocker(lockClient)
	}
	h := &HistoryService{
		svc:        svc,
		channels:   dao.NewHistoryChannelDAO(db),
		targets:    dao.NewHistoryTargetDAO(db),
		runs:       dao.NewHistoryRunDAO(db),
		overrides:  dao.NewHistoryOverrideDAO(db),
		backend:    backend,
		cm:         cm,
		locks:      syncx.NewKeyedGate(1),
		locker:     locker,
		lockTTL:    historyLockTTL,
		lockClient: lockClient,
	}
	// 启动时清一次过期产物:清理的触发点只有「apply / push」,
	// 服务长期不用时产物会一直留着(它是临时目录,不该攒着)。
	h.cleanupStaleProducts()
	return h, nil
}

// channelGate 返回本实例的进程内通道闸门(懒初始化)。
// HistoryService 允许直接字面量构造(测试路径),不能假设 locks 已注入;
// KeyedGate 的零值 cap=0 不构成互斥,必须显式 NewKeyedGate(1)。
func (h *HistoryService) channelGate() *syncx.KeyedGate {
	h.locksOnce.Do(func() {
		if h.locks == nil {
			h.locks = syncx.NewKeyedGate(1)
		}
	})
	return h.locks
}

// lockChannel 取通道级互斥:先取进程内互斥(阻塞,保持串行语义),
// 再抢跨实例锁(配 redis 时;被其它实例占用 → 忙碌错误,不排队等待)。
//
// **同一通道的写操作必须串行**,原因有三:
//   - 执行记录是一条哈希链,续接靠「取上一条已入链记录的哈希」——
//     两个并发操作会拿到同一个 prev,链分叉,校验时报「prev_hash mismatch」;
//   - 两次并发推送会互相覆盖远端分支,先完成的那次可能被后完成的那次顶掉;
//   - 移植产物路径(run.WorkRepoPath)会被后一次执行改写。
func (h *HistoryService) lockChannel(ctx context.Context, chID uint) (func(), error) {
	releaseLocal, err := h.channelGate().Acquire(ctx, fmt.Sprintf("history-channel-%d", chID))
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("history-channel-%d", chID)
	var ownerID string
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			// **先放跨实例锁,再放进程内闸门**。
			// 反过来的话,两者之间另一个 goroutine 拿到本地闸门,
			// 却又因为 redis 锁还在而被误报成「正在其它实例执行」。
			if h.locker != nil && ownerID != "" {
				// 用**不受请求取消影响**的 ctx:请求 ctx 可能已取消,
				// 那时解锁会直接失败 → redis 锁残留到 TTL(最长 30 分钟),
				// 期间该通道对其它实例一律报忙。
				uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if uerr := h.locker.Unlock(uctx, key, ownerID); uerr != nil {
					slog.Debug("history: 释放跨实例锁", "channel", chID, "error", uerr)
				}
			}
			releaseLocal()
		})
	}

	if h.locker != nil {
		id, lerr := h.locker.TryLock(ctx, key, h.lockTTL)
		switch {
		case lerr == nil:
			ownerID = id
		case errors.Is(lerr, distlock.ErrLockNotAcquired):
			releaseLocal()
			return func() {}, fmt.Errorf("通道 %d 正在其它实例执行,请稍后重试", chID)
		default:
			// redis 抖动:降级为进程内互斥,不阻断操作。
			slog.Warn("history: 抢跨实例锁失败,降级为进程内互斥", "channel", chID, "error", lerr)
		}
	}
	return unlock, nil
}

// History 返回历史同步服务。
func (s *Service) History() *HistoryService {
	return s.history
}

// ===== 通道 CRUD =====

// CreateHistoryChannel 新建历史同步通道。
func (h *HistoryService) CreateHistoryChannel(_ context.Context, in HistoryChannelInput) (*HistoryChannelDTO, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("通道名不能为空")
	}
	if strings.TrimSpace(derefStr(in.ModuleSource)) == "" ||
		strings.TrimSpace(derefStr(in.ModuleTarget)) == "" {
		return nil, fmt.Errorf("module_source 与 module_target 都不能为空(身份改写是历史同步的前提)")
	}
	if derefStr(in.SourceRepoKey) == "" && derefStr(in.SourceDir) == "" {
		return nil, fmt.Errorf("source_repo_key 与 source_dir 至少给一个")
	}
	if derefStr(in.TargetRepoKey) == "" && derefStr(in.TargetDir) == "" {
		return nil, fmt.Errorf("target_repo_key 与 target_dir 至少给一个")
	}
	// 本地路径直连必须先过路径闸门(见 validateHistoryRepoDir)。
	if err := validateHistoryRepoDir(derefStr(in.SourceDir), "source_dir"); err != nil {
		return nil, err
	}
	if err := validateHistoryRepoDir(derefStr(in.TargetDir), "target_dir"); err != nil {
		return nil, err
	}
	if existing, err := h.channels.FindByName(in.Name); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("通道名已存在: %s", in.Name)
	}

	if err := validatePathRules(in.PathRules); err != nil {
		return nil, err
	}
	pathRules, err := json.Marshal(in.PathRules)
	if err != nil {
		return nil, err
	}
	scrubRules, err := json.Marshal(in.ScrubRules)
	if err != nil {
		return nil, err
	}
	conflictRules, err := json.Marshal(in.ConflictRules)
	if err != nil {
		return nil, err
	}

	ch := &model.HistoryChannel{
		Name:          strings.TrimSpace(in.Name),
		SourceRepoKey: derefStr(in.SourceRepoKey),
		TargetRepoKey: derefStr(in.TargetRepoKey),
		SourceDir:     derefStr(in.SourceDir),
		TargetDir:     derefStr(in.TargetDir),
		ModuleSource:  derefStr(in.ModuleSource),
		ModuleTarget:  derefStr(in.ModuleTarget),
		HostSource:    derefStr(in.HostSource),
		HostTarget:    derefStr(in.HostTarget),
		AuthorName:    derefStr(in.AuthorName),
		AuthorEmail:   derefStr(in.AuthorEmail),
		PathRules:     string(pathRules),
		ScrubRules:    string(scrubRules),
		ConflictRules: string(conflictRules),
		SourceBranch:  derefStr(in.SourceBranch),
		TargetBranch:  derefStr(in.TargetBranch),
		Enabled:       true, // 默认启用;显式 false 由下面的 in.Enabled 覆盖
	}
	if in.Enabled != nil {
		ch.Enabled = *in.Enabled
	}
	if err := h.channels.Create(ch); err != nil {
		return nil, err
	}
	return toDTO(ch), nil
}

// UpdateHistoryChannel 更新通道。
func (h *HistoryService) UpdateHistoryChannel(ctx context.Context, id uint, in HistoryChannelInput) (*HistoryChannelDTO, error) {
	// 改配置也要与执行互斥:否则一次执行可能读到改了一半的配置
	// (目录换了、身份映射没换 → 内网 module path 原样进公开仓)。
	release, err := h.lockChannel(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.updateHistoryChannel(id, in)
}

// updateHistoryChannel 是 UpdateHistoryChannel 的内部实现(**调用方须已持有通道锁**)。
func (h *HistoryService) updateHistoryChannel(id uint, in HistoryChannelInput) (*HistoryChannelDTO, error) {
	ch, err := h.channels.FindByID(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		ch.Name = strings.TrimSpace(in.Name)
	}
	// **部分更新**:未指定(nil)的字段保持原值,只有显式给了才覆盖。
	// 这条是安全底线:身份映射一旦被抹掉,执行时就没有任何身份规则,
	// 内网 module path 会原样进公开仓。
	setIfGiven(&ch.SourceRepoKey, in.SourceRepoKey)
	setIfGiven(&ch.TargetRepoKey, in.TargetRepoKey)
	setIfGiven(&ch.SourceDir, in.SourceDir)
	setIfGiven(&ch.TargetDir, in.TargetDir)
	setIfGiven(&ch.ModuleSource, in.ModuleSource)
	setIfGiven(&ch.ModuleTarget, in.ModuleTarget)
	setIfGiven(&ch.HostSource, in.HostSource)
	setIfGiven(&ch.HostTarget, in.HostTarget)
	setIfGiven(&ch.AuthorName, in.AuthorName)
	setIfGiven(&ch.AuthorEmail, in.AuthorEmail)
	setIfGiven(&ch.SourceBranch, in.SourceBranch)
	setIfGiven(&ch.TargetBranch, in.TargetBranch)
	if in.Enabled != nil {
		// 未指定时保持原值,避免「不传 enabled」把已禁用的通道重新启用。
		ch.Enabled = *in.Enabled
	}
	// 身份映射不能被更新请求抹掉(抹掉就没有任何改写规则了)。
	if strings.TrimSpace(ch.ModuleSource) == "" || strings.TrimSpace(ch.ModuleTarget) == "" {
		return nil, fmt.Errorf("module_source 与 module_target 不能为空(身份改写是历史同步的前提)")
	}
	// 本地路径直连必须先过路径闸门。
	if err := validateHistoryRepoDir(ch.SourceDir, "source_dir"); err != nil {
		return nil, err
	}
	if err := validateHistoryRepoDir(ch.TargetDir, "target_dir"); err != nil {
		return nil, err
	}

	if in.PathRules != nil {
		if err := validatePathRules(in.PathRules); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(in.PathRules)
		if err != nil {
			return nil, err
		}
		ch.PathRules = string(raw)
	}
	if in.ScrubRules != nil {
		raw, err := json.Marshal(in.ScrubRules)
		if err != nil {
			return nil, err
		}
		ch.ScrubRules = string(raw)
	}
	if in.ConflictRules != nil {
		raw, err := json.Marshal(in.ConflictRules)
		if err != nil {
			return nil, err
		}
		ch.ConflictRules = string(raw)
	}
	if err := h.channels.Update(ch); err != nil {
		return nil, err
	}
	return toDTO(ch), nil
}

// ListHistoryChannels 列出通道。
func (h *HistoryService) ListHistoryChannels(_ context.Context, offset, limit int) ([]*HistoryChannelDTO, int64, error) {
	channels, total, err := h.channels.FindAll(dao.Pagination{Offset: offset, Limit: limit})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*HistoryChannelDTO, 0, len(channels))
	for _, ch := range channels {
		dto := toDTO(ch)
		if targets, err := h.targets.FindByChannel(ch.ID); err == nil {
			for _, t := range targets {
				dto.Targets = append(dto.Targets, toTargetDTO(t))
			}
		}
		out = append(out, dto)
	}
	return out, total, nil
}

// GetHistoryChannel 取通道详情。
func (h *HistoryService) GetHistoryChannel(_ context.Context, id uint) (*HistoryChannelDTO, error) {
	ch, err := h.channels.FindByID(id)
	if err != nil {
		return nil, err
	}
	dto := toDTO(ch)
	targets, err := h.targets.FindByChannel(id)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		dto.Targets = append(dto.Targets, toTargetDTO(t))
	}
	return dto, nil
}

// DeleteHistoryChannel 删除通道(连同其推送目标)。
func (h *HistoryService) DeleteHistoryChannel(ctx context.Context, id uint) error {
	release, err := h.lockChannel(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	return h.deleteHistoryChannel(id)
}

// deleteHistoryChannel 是 DeleteHistoryChannel 的内部实现(**调用方须已持有通道锁**)。
func (h *HistoryService) deleteHistoryChannel(id uint) error {
	targets, err := h.targets.FindByChannel(id)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := h.targets.Delete(t.ID); err != nil {
			return err
		}
	}
	return h.channels.Delete(id)
}

// ===== 推送目标 CRUD =====

// CreateHistoryTarget 新建推送目标。
//
// 目标顺序即推送优先级:**第一个是留底,其后才是发布**。
func (h *HistoryService) CreateHistoryTarget(_ context.Context, channelID uint, in HistoryTargetInput) (*HistoryTargetDTO, error) {
	if _, err := h.channels.FindByID(channelID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("目标名不能为空")
	}
	if strings.TrimSpace(in.RepoURL) == "" {
		return nil, fmt.Errorf("目标地址不能为空")
	}
	// 目标名在通道内必须唯一:推送时按名字定位目标/凭据,同名会让"推给谁"
	// 变得不确定(历史上确实出现过凭据串号)。
	name := strings.TrimSpace(in.Name)
	existing, err := h.targets.FindByChannel(channelID)
	if err != nil {
		return nil, err
	}
	for _, t := range existing {
		if t.Name == name {
			return nil, fmt.Errorf("通道 %d 已存在同名目标 %q:目标名必须唯一", channelID, name)
		}
	}
	credType, credential, username, err := h.encryptCredential(in)
	if err != nil {
		return nil, err
	}
	t := &model.HistoryTarget{
		ChannelID:  channelID,
		Name:       name,
		RepoURL:    strings.TrimSpace(in.RepoURL),
		CredType:   credType,
		Credential: credential,
		Username:   username,
	}
	if err := h.targets.Create(t); err != nil {
		return nil, err
	}
	return toTargetDTO(t), nil
}

// UpdateHistoryTarget 更新推送目标。
func (h *HistoryService) UpdateHistoryTarget(_ context.Context, id uint, in HistoryTargetInput) (*HistoryTargetDTO, error) {
	t, err := h.targets.FindByID(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		t.Name = strings.TrimSpace(in.Name)
	}
	if in.RepoURL != "" {
		newURL := strings.TrimSpace(in.RepoURL)
		// 客户端回传的是**脱敏后**的地址(响应里就是脱敏的)⇒ 视为"没改地址",
		// 保留库里带凭据的原值;否则一次无关的更新就把凭据抹掉了。
		if newURL != history.RedactRepoURL(t.RepoURL) {
			t.RepoURL = newURL
		}
	}
	if in.CredType != "" || in.Credential != "" {
		credType, credential, username, err := h.encryptCredential(in)
		if err != nil {
			return nil, err
		}
		t.CredType = credType
		t.Credential = credential
		t.Username = username
	}
	if err := h.targets.Update(t); err != nil {
		return nil, err
	}
	return toTargetDTO(t), nil
}

// DeleteHistoryTarget 删除推送目标。
func (h *HistoryService) DeleteHistoryTarget(_ context.Context, id uint) error {
	return h.targets.Delete(id)
}

// ListHistoryTargets 列出通道的推送目标(按推送优先级)。
func (h *HistoryService) ListHistoryTargets(_ context.Context, channelID uint) ([]*HistoryTargetDTO, error) {
	targets, err := h.targets.FindByChannel(channelID)
	if err != nil {
		return nil, err
	}
	out := make([]*HistoryTargetDTO, 0, len(targets))
	for _, t := range targets {
		out = append(out, toTargetDTO(t))
	}
	return out, nil
}

// encryptCredential 规范化凭据并加密落库。
func (h *HistoryService) encryptCredential(in HistoryTargetInput) (credType, encrypted, username string, err error) {
	credType = in.CredType
	if credType == "" {
		credType = model.HistoryCredNone
	}
	username = in.Username
	switch credType {
	case model.HistoryCredNone:
		return credType, "", "", nil
	case model.HistoryCredToken:
		if in.Credential == "" {
			return "", "", "", fmt.Errorf("token 类凭据必须提供 credential")
		}
		enc, err := h.cm.Encrypt(in.Credential)
		if err != nil {
			return "", "", "", errors.Wrap(err, "encrypt credential failed")
		}
		if username == "" {
			username = "oauth2"
		}
		return credType, enc, username, nil
	case model.HistoryCredSSHKey:
		enc, err := h.cm.Encrypt(in.Credential)
		if err != nil {
			return "", "", "", errors.Wrap(err, "encrypt credential failed")
		}
		return credType, enc, username, nil
	default:
		return "", "", "", fmt.Errorf("不支持的凭据类型: %s", credType)
	}
}

// targetAuth 解出目标的认证配置。
func (h *HistoryService) targetAuth(t *model.HistoryTarget) (gitbackend.AuthConfig, error) {
	switch t.CredType {
	case "", model.HistoryCredNone:
		return gitbackend.AuthConfig{Type: gitbackend.AuthNone}, nil
	case model.HistoryCredToken:
		plain, err := h.cm.Decrypt(t.Credential)
		if err != nil {
			return gitbackend.AuthConfig{}, errors.Wrap(err, "decrypt credential failed")
		}
		user := t.Username
		if user == "" {
			user = "oauth2"
		}
		return gitbackend.NewHTTPBasicAuth(user, plain), nil
	case model.HistoryCredSSHKey:
		plain, err := h.cm.Decrypt(t.Credential)
		if err != nil {
			return gitbackend.AuthConfig{}, errors.Wrap(err, "decrypt credential failed")
		}
		if plain == "" {
			return gitbackend.AuthConfig{Type: gitbackend.AuthSSH}, nil
		}
		return gitbackend.NewSSHKeyContentAuth(plain, ""), nil
	default:
		return gitbackend.AuthConfig{}, fmt.Errorf("不支持的凭据类型: %s", t.CredType)
	}
}

// ===== 执行(通道优先) =====

// HistoryPlan 按通道生成同步计划(纯读)。
func (h *HistoryService) HistoryPlan(ctx context.Context, channelID uint) (*HistoryPlanDTO, error) {
	opts, err := h.channelOptions(ctx, channelID)
	if err != nil {
		return nil, err
	}
	plan, err := history.BuildPlan(ctx, *opts)
	if err != nil {
		return nil, err
	}
	return planDTO(channelID, plan, false), nil
}

// HistoryResolve 按通道生成计划并计算 hold 的自动解法。
func (h *HistoryService) HistoryResolve(ctx context.Context, channelID uint) (*HistoryPlanDTO, error) {
	opts, err := h.channelOptions(ctx, channelID)
	if err != nil {
		return nil, err
	}
	plan, err := history.BuildPlan(ctx, *opts)
	if err != nil {
		return nil, err
	}
	if err := history.Resolve(ctx, *opts, plan); err != nil {
		return nil, err
	}
	// 通道里的裁决覆盖确定性解法(人工/AI 已经拍过板的,不再重复判)。
	if err := h.applyOverrides(ctx, channelID, plan); err != nil {
		return nil, err
	}
	return planDTO(channelID, plan, true), nil
}

// applyOverrides 把通道里的裁决写进计划的解法。
func (h *HistoryService) applyOverrides(ctx context.Context, channelID uint, plan *history.Plan) error {
	overrides, err := h.overrides.FindByChannel(channelID)
	if err != nil {
		return err
	}
	if len(overrides) == 0 {
		return nil
	}
	for _, ov := range overrides {
		res, ok := plan.Resolutions[ov.Path]
		if !ok {
			continue // 不是 hold 文件(或本次没有该文件),裁决暂不生效。
		}
		switch ov.Verdict {
		case model.HistoryVerdictKeepTarget:
			res.Verdict = history.ResolutionTakeOurs
			res.Content = ""
			res.Reason = "人工裁决:保留目标侧 — " + ov.Reason
		case model.HistoryVerdictTakeSource:
			res.Verdict = history.ResolutionTakeTheirs
			// **必须带上源侧内容**:冲突的 Resolution.Content 本来是空的,
			// 不赋值的话下游会把空串当内容写进树(发布一个空文件)。
			res.Content = res.Theirs
			res.Reason = "人工裁决:取源侧 — " + ov.Reason
		case model.HistoryVerdictMerged:
			res.Verdict = history.ResolutionMergeClean
			res.Content = ov.Content
			res.Reason = "人工裁决:采用裁决内容 — " + ov.Reason
		default:
			continue
		}
		res.Reason = res.Reason + "（裁决者:" + ov.DecidedBy + "）"
		plan.Resolutions[ov.Path] = res
		plan.Summary.Resolved++
	}
	return nil
}

// HistoryApply 按通道把计划落到目标树,产出执行记录。
//
// 纯本地操作:不推送、不改原始目标仓。
// 产物路径记在执行记录里,推送时服务端自己取。
func (h *HistoryService) HistoryApply(ctx context.Context, channelID uint) (*HistoryRunDTO, error) {
	release, err := h.lockChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.applyRun(ctx, channelID)
}

// applyRun 是 HistoryApply 的内部实现(**调用方须已持有通道锁**)。
//
// 拆开是为了让 HistorySync 能「一次锁住、连做移植+推送」——
// KeyedGate 不可重入,同一把锁再取一次会死等。
func (h *HistoryService) applyRun(ctx context.Context, channelID uint) (*HistoryRunDTO, error) {
	ch, opts, plan, err := h.planAndResolve(ctx, channelID)
	if err != nil {
		return nil, err
	}
	out, err := h.applyWithPlan(ctx, ch, opts, plan)
	if err != nil {
		return nil, err
	}
	return out.Run, nil
}

// applyOutcome 是一次物化 + 门禁的完整结果。
type applyOutcome struct {
	// Run 执行记录(已入链)。
	Run *HistoryRunDTO
	// Res 物化结果;物化失败时为 nil。
	Res *history.ApplyResult
	// Gate 门禁结论。
	Gate GateReport
}

// applyWithPlan 用**已算好的计划**物化并落执行记录。
//
// 与 applyRun 的区别只在于「计划从哪来」:一键同步已经算好了计划,
// 不该再算一遍 —— 两遍算出来的树可能不是同一棵
// (两次计算之间仓变了 ⇒ 预检报告说的不是真正要推的那棵树)。
//
// 计划里**必须已经应用人工裁决**(见 planAndResolve):
// 少了这一步,裁决对发布的树零影响 —— 预检说「已解决、这个文件会被发布」,
// 真正 apply 时该文件却进 skipped(或者发布的是旧内容),
// 而预检报告一切正常。整条「AI 建议 + 人工确认 → 裁决 → 生效」的闭环
// 断在最后一步。
func (h *HistoryService) applyWithPlan(ctx context.Context, ch *model.HistoryChannel,
	opts *history.Options, plan *history.Plan) (*applyOutcome, error) {

	now := time.Now()
	run := &model.HistoryRun{
		ChannelID: ch.ID,
		Kind:      model.HistoryRunKindApply,
		Status:    model.HistoryRunRunning,
		StartedAt: &now,
	}
	if err := h.runs.Create(run); err != nil {
		return nil, err
	}

	res, err := h.materialize(ctx, *opts, plan,
		// 产物放在**服务自己的数据目录**下,不用系统共享临时目录 ——
		// 否则清理逻辑会去动同机其它用户/进程的东西。
		h.productRoot(),
		// 「还没推送成功」的产物不许被这次 apply 的清理删掉。
		h.pendingProductDirs())
	if err != nil {
		dto, ferr := h.finishRun(run, model.HistoryRunFailed, err.Error())
		if ferr != nil {
			return nil, ferr
		}
		return &applyOutcome{Run: dto}, nil
	}

	run.BaseHead = res.BaseHead
	run.Head = res.Head
	run.WorkRepoPath = res.RepoPath
	run.PlanSummary = jsonString(plan.Summary)
	run.Commits = jsonString(res.Commits)
	run.Files = jsonString(res.AppliedFileList())
	run.Skipped = jsonString(res.Skipped)
	// 痕迹报告必须区分「已自动清理」与「强制人工」:
	// 前者内容已发布,后者文件没被发布。混在一起会让人以为密钥已处理。
	run.ScrubReport = jsonString(map[string]any{
		"hits":      res.ScrubHits,
		"mandatory": res.MandatoryHits,
	})
	// **门禁跑在产物上** —— 不是源树,也不是预检时的临时目录:
	// 推出去的就是这个目录里的那棵树,门禁必须回答「推出去能不能编译」。
	//
	// 结论写进执行记录(gate_report),推送前会**重新核对**它与产物 HEAD 是否一致
	// (见 verifyPushableProduct)。
	gate := h.gateCheck(ctx, res.RepoPath, internalIdentifiers(ch)...)
	run.GateReport = jsonString(historyGateResult{
		OK:              gate.OK,
		Head:            res.Head,
		GofmtViolations: gate.GofmtViolations,
		TypeCheckErr:    gate.TypeCheckErr,
		Note:            gate.Note,
		Files:           res.Files,
		Commits:         len(res.Commits),
	})
	if err := h.runs.Update(run); err != nil {
		return nil, err
	}
	if !gate.OK {
		// 门禁不通过的产物**不算移植成功**:记录状态必须如实写明
		// 「这棵树推出去编译不过」,推送侧也会独立核对结论并拒绝。
		dto, ferr := h.finishRun(run, model.HistoryRunFailed,
			"门禁未通过:"+gate.Note+gateFailureDetail(gate))
		if ferr != nil {
			return nil, ferr
		}
		return &applyOutcome{Run: dto, Res: res, Gate: gate}, nil
	}
	dto, ferr := h.finishRun(run, model.HistoryRunSuccess, "")
	if ferr != nil {
		return nil, ferr
	}
	return &applyOutcome{Run: dto, Res: res, Gate: gate}, nil
}

// historyGateResult 是写进执行记录的门禁结论(结构化)。
//
// 形状是**约定**,不是随手起的:推送前要重新核对这份结论
// (见 verifyPushableProduct),所以字段名与语义都不能随意改。
//
//   - ok:门禁是否通过;
//   - head:门禁验的那棵树的 commit(必须等于产物仓分支的 HEAD);
//   - gofmt_violations / type_check_err:失败细节;
//   - files / commits:这次移植的规模(便于对着审计材料核)。
type historyGateResult struct {
	OK              bool     `json:"ok"`
	Head            string   `json:"head"`
	GofmtViolations []string `json:"gofmt_violations,omitempty"`
	TypeCheckErr    string   `json:"type_check_err,omitempty"`
	Note            string   `json:"note"`
	Files           int      `json:"files"`
	Commits         int      `json:"commits"`
}

// gateFailureDetail 把门禁失败细节拼成人能读的说明(进执行记录的 error 字段)。
func gateFailureDetail(g GateReport) string {
	var b strings.Builder
	if g.TypeCheckErr != "" {
		b.WriteString("\n类型检查:" + g.TypeCheckErr)
	}
	if len(g.GofmtViolations) > 0 {
		b.WriteString(fmt.Sprintf("\ngofmt 违规 %d 个文件:%s",
			len(g.GofmtViolations), strings.Join(g.GofmtViolations, ", ")))
	}
	return b.String()
}

// verifyPushableProduct 核对「执行记录的门禁结论 + 产物路径 + 产物 HEAD」三者一致。
//
// 三者一致 ⇒ **门禁验过的树就是即将推出去的那棵树**,推送前不必再跑一次
// 完整类型检查(大仓上要几十秒,而 HEAD 一致已经证明树没变)。
// 任一不一致都中止,不产生任何推送:
//
//   - 没有可解析的门禁结论 → 这棵树从没通过编译门禁
//     (老记录里的占位 gate_report 也落在这里:宁可拒绝,也不盲推);
//   - 结论不通过 → 编译不过的树不许推;
//   - 产物 HEAD 与执行记录不符 → 产物在 apply 之后被改动过;
//   - 结论里的 head 与产物 HEAD 不符 → 验的不是要推的那棵树。
func (h *HistoryService) verifyPushableProduct(run *model.HistoryRun, branch string) error {
	if run.WorkRepoPath == "" {
		return fmt.Errorf("执行记录 %d 没有产物路径(未 apply?)", run.ID)
	}
	if _, err := os.Stat(run.WorkRepoPath); err != nil {
		// 只报目录名:绝对路径会一路流进模型上下文与日志。
		return fmt.Errorf("产物目录不存在或不可读(%s):%w", filepath.Base(run.WorkRepoPath), err)
	}
	var gate historyGateResult
	if err := json.Unmarshal([]byte(run.GateReport), &gate); err != nil {
		return fmt.Errorf("执行记录 %d 没有可用的门禁结论(记录格式无法解析),推送已中止:请重新 apply 后再推送", run.ID)
	}
	if !gate.OK {
		return fmt.Errorf("执行记录 %d 的门禁未通过,推送已中止:%s", run.ID, gate.Note)
	}
	repo, err := git.PlainOpen(run.WorkRepoPath)
	if err != nil {
		return fmt.Errorf("打开产物仓库失败: %w", err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return fmt.Errorf("解析产物分支 %s 失败: %w", branch, err)
	}
	head := ref.Hash().String()
	if head != run.Head {
		return fmt.Errorf("产物 HEAD(%s)与执行记录(%s)不一致,推送已中止:产物在 apply 之后被改动过,请重新 apply",
			shortHash(head), shortHash(run.Head))
	}
	if gate.Head != "" && gate.Head != head {
		return fmt.Errorf("门禁结论对应的树(%s)与即将推送的树(%s)不一致,推送已中止:请重新 apply",
			shortHash(gate.Head), shortHash(head))
	}
	return nil
}

// finishRun 收尾执行记录(状态 + 耗时 + **入链**)。
//
// 哈希只在终态计算:执行中的记录字段还在变,提前算出的哈希必然失配。
// 续链取该通道最近一条**已入链**的记录 —— 执行中的记录没有哈希,必须跳过。
func (h *HistoryService) finishRun(run *model.HistoryRun, status, errMsg string) (*HistoryRunDTO, error) {
	// 时间先截到毫秒再算哈希:哈希输入与入库值必须是同一个值。
	// (MySQL 的 datetime(3) 存不下纳秒,入库时被四舍五入 → 整条链误报被篡改。)
	now := model.TruncateDBTime(time.Now())
	run.Status = status
	run.Error = errMsg
	run.FinishedAt = &now
	if run.StartedAt != nil {
		run.DurationMs = now.Sub(*run.StartedAt).Milliseconds()
	}
	prev, err := h.runs.FindLastHashedByChannel(run.ChannelID)
	if err != nil {
		return nil, err
	}
	if prev != nil {
		run.PrevHash = prev.EntryHash
	}
	run.EntryHash = model.ComputeHistoryRunHash(run.PrevHash, run)
	if err := h.runs.Update(run); err != nil {
		return nil, err
	}
	// 链尾锚点:记下最后一条已入链记录的哈希。
	// 有了它,「链尾被截断」也能检出(链本身只能证明前后接得上)。
	if err := h.channels.UpdateLastEntryHash(run.ChannelID, run.EntryHash); err != nil {
		return nil, err
	}
	return toRunDTO(run), nil
}

// HistoryChainResult 是通道内执行记录链的校验结果。
type HistoryChainResult struct {
	// OK 链完整。
	OK bool `json:"ok"`
	// Checked 已校验的记录数。
	Checked int `json:"checked"`
	// BrokenAt 链断裂处的记录 ID。
	BrokenAt uint `json:"broken_at,omitempty"`
	// Message 结论说明。
	Message string `json:"message"`
}

// VerifyHistoryChannelChain 校验通道内执行记录的哈希链。
//
// 与既有审计链(OperationLog)同构,但**链在通道内**:
// 不同通道的同步记录混在一条链里没有意义,而且链会随通道数无限增长。
//
// 执行中的记录(没有哈希)被跳过,但会记在结论里 —— 它们不是链的一部分。
func (h *HistoryService) VerifyHistoryChannelChain(_ context.Context, channelID uint) (*HistoryChainResult, error) {
	runs, err := h.runs.ListByChannelAscending(channelID)
	if err != nil {
		return nil, err
	}
	res := &HistoryChainResult{OK: true}
	prev := ""
	inFlight := 0
	for _, r := range runs {
		if r.EntryHash == "" {
			// 执行中:尚未入链,跳过但不打断链。
			inFlight++
			continue
		}
		if r.PrevHash != prev {
			res.OK = false
			res.BrokenAt = r.ID
			res.Message = "prev_hash mismatch"
			return res, nil
		}
		if !model.VerifyHistoryRunHash(r.PrevHash, r, r.EntryHash) {
			res.OK = false
			res.BrokenAt = r.ID
			res.Message = "entry_hash mismatch (tampered?)"
			return res, nil
		}
		prev = r.EntryHash
		res.Checked++
	}
	// 链尾锚点核对:**必须在「Checked == 0」早退之前**。
	//
	// 锚点就是为检出「链尾被截断」而设的。若先早退,「把一个通道的
	// 执行记录全删光」反而会报「尚无已入链的执行记录」+ OK=true ——
	// 证据被删光,审计链却显示完好,比不校验更糟。
	if ch, err := h.channels.FindByID(channelID); err == nil && ch.LastEntryHash != "" {
		if prev != ch.LastEntryHash {
			res.OK = false
			res.Message = "链尾锚点不匹配(链尾被截断或记录被删光):期望 " +
				ch.LastEntryHash[:min(8, len(ch.LastEntryHash))] +
				",实际 " + prev[:min(8, len(prev))]
			return res, nil
		}
	}
	if res.Checked == 0 {
		res.Message = "通道尚无已入链的执行记录"
		return res, nil
	}
	if inFlight > 0 {
		res.Message = fmt.Sprintf("audit chain intact (%d 条已入链,%d 条执行中未入链)", res.Checked, inFlight)
		return res, nil
	}
	res.Message = "audit chain intact"
	return res, nil
}

// HistoryPush 按通道推送**最近一次**执行记录的产物。
func (h *HistoryService) HistoryPush(ctx context.Context, channelID uint) (*HistoryPushDTO, error) {
	return h.HistoryPushWithOptions(ctx, channelID, HistoryPushOptions{})
}

// HistoryPushWithOptions 按通道推送**最近一次**执行记录的产物。
//
// Force=true 时跳过**分叉检查**:远端有我们不知道的提交也照样覆盖。
// 默认跳过 —— 强推会把远端的提交静默丢掉(实测:远端别人的提交
// 被丢弃后只剩 0 条),这种事必须由人明确点头。
func (h *HistoryService) HistoryPushWithOptions(ctx context.Context, channelID uint,
	opts HistoryPushOptions) (*HistoryPushDTO, error) {

	release, err := h.lockChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	defer release()
	// **强制推送的闸门放在 core**:没有批准者就不许强推。
	if opts.Force && opts.ForceApprovedBy == "" {
		return nil, fmt.Errorf("强制推送需要批准者(admin 角色):没有批准者就不能覆盖远端已有的提交")
	}
	return h.pushChannelLatest(ctx, channelID, opts.Force)
}

// pushChannelLatest 推送通道最近一次 apply 的产物(**调用方须已持有通道锁**)。
func (h *HistoryService) pushChannelLatest(ctx context.Context, channelID uint, force bool) (*HistoryPushDTO, error) {
	// 找最近一次 **apply** 记录:apply 记录才有产物路径。
	// (最新的记录可能是上一条 push 记录, 它自己不带产物。)
	run, err := h.runs.FindLatestByChannelKind(channelID, model.HistoryRunKindApply)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("通道 %d 还没有 apply 记录,先 apply", channelID)
	}
	return h.pushRun(ctx, run, force)
}

// HistoryPushRun 推送指定执行记录的产物。
func (h *HistoryService) HistoryPushRun(ctx context.Context, runID uint) (*HistoryPushDTO, error) {
	run, err := h.runs.FindByID(runID)
	if err != nil {
		return nil, err
	}
	release, err := h.lockChannel(ctx, run.ChannelID)
	if err != nil {
		return nil, err
	}
	defer release()
	// 拿锁后重读:等锁期间这条记录可能已被改写(新的 apply 产物)。
	run, err = h.runs.FindByID(runID)
	if err != nil {
		return nil, err
	}
	return h.pushRun(ctx, run, false)
}

// pushRun 执行推送:按通道目标的顺序依次推送,任一失败立即中止。
//
// force=true 时跳过**分叉检查**(远端有我们不知道的提交时仍然覆盖)。
// 默认不跳过:强推会把远端的提交**静默丢掉**
// (实测:远端别人的提交被丢弃后只剩 0 条)。
func (h *HistoryService) pushRun(ctx context.Context, run *model.HistoryRun, force bool) (*HistoryPushDTO, error) {
	if run.WorkRepoPath == "" {
		return nil, fmt.Errorf("执行记录 %d 没有产物路径(未 apply?)", run.ID)
	}
	targets, err := h.targets.FindByChannel(run.ChannelID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("通道 %d 没有配置推送目标", run.ChannelID)
	}

	// 凭据**按目标 ID 索引**,不按名字:同名目标(名字没有唯一约束)会让
	// map 只留最后一个的凭据 ⇒ 先推的那个用错凭据 ⇒ 认证失败,而报错指向
	// 凭据、看不出是"配置重复"。
	auths := make(map[uint]gitbackend.AuthConfig, len(targets))
	for _, t := range targets {
		auth, err := h.targetAuth(t)
		if err != nil {
			return nil, err
		}
		auths[t.ID] = auth
	}

	// 逐个目标推送:用各自凭据。任一失败即中止,不推其余。
	report := &HistoryPushDTO{RunID: run.ID, Heads: map[string]string{}}
	branch := "main"
	if ch, err := h.channels.FindByID(run.ChannelID); err == nil && ch.TargetBranch != "" {
		branch = ch.TargetBranch
	}

	// **推送前的门禁核对**:执行记录的门禁结论、产物路径、产物 HEAD
	// 三者必须一致 —— 一致才说明「门禁验过的就是即将推出去的那棵树」。
	//
	// 这一步是**推送路径上唯一不可绕过的闸门**:Force(忽略阻塞项)、
	// ForcePush(覆盖远端)都不越过它。编译不过的树永远推不出去。
	if err := h.verifyPushableProduct(run, branch); err != nil {
		return nil, err
	}

	// 顺手清一次过期产物:产物只在「apply 之后的那次 push」与
	// 「之后的回滚」里用得上,但不该一直占着磁盘。
	h.cleanupStaleProducts()

	// **分叉检查**:远端有没有我们不知道的提交。
	// 没有这一步,强推就是一个静默丢数据的开关。
	if !force {
		for _, t := range targets {
			diverged, note, err := h.checkPushDivergence(ctx, run, t, branch)
			if err != nil {
				return nil, fmt.Errorf("推送前检查远端 %s 失败: %w", t.Name, err)
			}
			if diverged {
				return nil, fmt.Errorf("推送已中止:远端 %s 与本次移植的基线已分叉(%s);"+
					"强推会丢掉远端的提交 —— 请先更新目标仓再重新 apply,或显式指定强制推送", t.Name, note)
			}
		}
	}

	// 推送另起一条记录:**不修改已入链的 apply 记录**。
	// 已入链记录的哈希在 apply 结束时算好,改动它的任何字段都会让链报「被篡改」。
	now := time.Now()
	pushRun := &model.HistoryRun{
		ChannelID:   run.ChannelID,
		Kind:        model.HistoryRunKindPush,
		PushedRunID: run.ID,
		Status:      model.HistoryRunRunning,
		BaseHead:    run.BaseHead,
		Head:        run.Head,
		StartedAt:   &now,
		RefsBefore:  jsonString(map[string]string{"target": run.BaseHead}),
	}
	if err := h.runs.Create(pushRun); err != nil {
		return nil, err
	}

	for _, t := range targets {
		res, err := history.Push(ctx, history.PushOptions{
			RepoPath: run.WorkRepoPath,
			// 与全系统其余部分同一个后端(生产配置 git.backend=gogit)
			BackendType: h.svc.backendType(),
			Targets:     []history.PushTarget{{Name: t.Name, URL: t.RepoURL}},
			Branch:      branch,
			Auth:        auths[t.ID],
			Force:       true, // 分叉检查已在上面做过;通过后强推是安全的(幂等)。
		})
		if err != nil {
			_, _ = h.finishRun(pushRun, model.HistoryRunFailed, err.Error())
			return nil, err
		}
		report.Pushed = append(report.Pushed, res.Pushed...)
		for k, v := range res.Heads {
			report.Heads[k] = v
		}
	}
	pushRun.RefsAfter = jsonString(report.Heads)
	if _, err := h.finishRun(pushRun, model.HistoryRunSuccess, ""); err != nil {
		return nil, err
	}
	return report, nil
}

// checkPushDivergence 判断远端分支与本次移植的基线是否已分叉。
//
// 判据与预检一致(见 checkTargetImpact):
//   - 远端没有该分支(空仓库/首次发布)→ 不分叉;
//   - 远端 HEAD == 本次移植的基线 → 快进,不分叉;
//   - 远端 HEAD == 本次移植的结果 → 已经推过了(重复推送),不分叉;
//   - 其它 → **分叉**:远端有我们不知道的提交,强推会把它们丢掉。
//
// 返回 (分叉?, 说明, 错误)。
func (h *HistoryService) checkPushDivergence(ctx context.Context, run *model.HistoryRun,
	t *model.HistoryTarget, branch string) (bool, string, error) {

	auth, err := h.targetAuth(t)
	if err != nil {
		return false, "", err
	}
	repo, err := git.PlainOpen(run.WorkRepoPath)
	if err != nil {
		return false, "", fmt.Errorf("打开工作仓库失败: %w", err)
	}
	if err := history.EnsureRemote(repo, t.Name, t.RepoURL); err != nil {
		return false, "", err
	}
	if _, err := h.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: run.WorkRepoPath,
		Remote:   t.Name,
		Branches: []string{branch},
		Auth:     auth,
	}); err != nil {
		if isEmptyRemoteErr(err) {
			// 空仓库:推送会创建该分支,没有东西会被覆盖。
			return false, "远端是空仓库(首次发布)", nil
		}
		return false, "", fmt.Errorf("拉取远端状态失败: %w", err)
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName(t.Name, branch), true)
	if err != nil {
		// 远端没有该分支 → 同空仓库。
		return false, "远端没有分支 " + branch, nil
	}
	remoteHead := ref.Hash().String()
	// 判定与预检共用同一份(classifyRemote):口径相反会让预检把"远端落后"
	// "重复发布"报成"已分叉需强推",并把一次本可安全完成的同步卡住。
	rel, note := classifyRemote(repo, remoteHead, run.BaseHead, run.Head)
	return needForceForRelation(rel), note, nil
}

// ===== 执行记录与回滚 =====

// ListHistoryRuns 列出通道的执行记录。
func (h *HistoryService) ListHistoryRuns(_ context.Context, channelID uint, offset, limit int) ([]*HistoryRunDTO, int64, error) {
	runs, total, err := h.runs.FindByChannel(channelID, dao.Pagination{Offset: offset, Limit: limit})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*HistoryRunDTO, 0, len(runs))
	for _, r := range runs {
		out = append(out, toRunDTO(r))
	}
	return out, total, nil
}

// GetHistoryRun 取执行记录详情。
func (h *HistoryService) GetHistoryRun(_ context.Context, id uint) (*HistoryRunDTO, error) {
	run, err := h.runs.FindByID(id)
	if err != nil {
		return nil, err
	}
	return toRunDTO(run), nil
}

// HistoryRollback 把目标树回滚到指定 ref。
//
// 保障来自通道的**留底目标**(第一个):从留底取回该 ref 的对象,
// 再强推到发布目标。危险操作:调用方须显式确认。
func (h *HistoryService) HistoryRollback(ctx context.Context, runID uint, ref string) (*HistoryRollbackDTO, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("回滚目标 ref 不能为空")
	}
	run, err := h.runs.FindByID(runID)
	if err != nil {
		return nil, err
	}
	release, err := h.lockChannel(ctx, run.ChannelID)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.rollbackRun(ctx, runID, ref)
}

// rollbackRun 是 HistoryRollback 的内部实现(**调用方须已持有通道锁**)。
func (h *HistoryService) rollbackRun(ctx context.Context, runID uint, ref string) (*HistoryRollbackDTO, error) {
	run, err := h.runs.FindByID(runID)
	if err != nil {
		return nil, err
	}
	targets, err := h.targets.FindByChannel(run.ChannelID)
	if err != nil {
		return nil, err
	}
	if len(targets) < 2 {
		return nil, fmt.Errorf("回滚需要至少两个目标(留底 + 发布),当前只有 %d 个", len(targets))
	}
	backup, publish := targets[0], targets[1]

	workRepo, err := openRepo(run.WorkRepoPath)
	if err != nil {
		return nil, fmt.Errorf("打开产物工作仓库失败(%s): %w —— 产物目录有保留期,"+
			"过期会被回收;需要回滚请重新 apply 再回滚", run.WorkRepoPath, err)
	}
	backupAuth, err := h.targetAuth(backup)
	if err != nil {
		return nil, err
	}
	publishAuth, err := h.targetAuth(publish)
	if err != nil {
		return nil, err
	}

	// 从留底取回目标 ref 的对象。
	if _, err := h.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: run.WorkRepoPath,
		Remote:   backup.Name,
		Tags:     true,
		Auth:     backupAuth,
	}); err != nil {
		return nil, fmt.Errorf("从留底 %s 取回对象失败: %w", backup.Name, err)
	}

	// 找到回滚目标:优先按 commit hash 解析(常见用法是回滚到 run 的 base_head),
	// 再退回到留底远端的同名分支。
	// **ref 与被推的分支必须对得上**。
	// ref 是留底上的分支名、而推送目标恒为目标分支(main)时,
	// 把留底的另一个分支强推到 main,公开主干就被换成了不相干的内容。
	// 传 commit hash 的用法不受影响(hash 不属于任何分支的语义)。
	branch := "main"
	if ch, err := h.channels.FindByID(run.ChannelID); err == nil && ch.TargetBranch != "" {
		branch = ch.TargetBranch
	}
	if !isCommitHash(ref) && ref != branch {
		return nil, fmt.Errorf("回滚目标 %s 与目标分支 %s 不一致:回滚只接受目标分支的同名 ref,或一个 commit hash",
			ref, branch)
	}
	rollbackHash, err := resolveRollbackRef(run.WorkRepoPath,
		"refs/remotes/"+backup.Name+"/"+ref, ref)
	if err != nil {
		return nil, fmt.Errorf("留底 %s 里没有 ref %s: %w", backup.Name, ref, err)
	}
	// 回滚到当前 HEAD 等于什么都不做 —— 要说清楚,不能「看起来成功」。
	if run.Head != "" && rollbackHash.String() == run.Head {
		return nil, fmt.Errorf("回滚目标 %s 就是当前的 HEAD(%s),回滚不会改变任何内容",
			ref, shortHash(run.Head))
	}

	// 把该提交强推到发布目标。
	if err := history.EnsureRemote(workRepo, publish.Name, publish.RepoURL); err != nil {
		return nil, err
	}
	// 回滚同样另起一条记录(理由同 push:不改已入链的记录)。
	now := time.Now()
	rbRun := &model.HistoryRun{
		ChannelID:   run.ChannelID,
		Kind:        model.HistoryRunKindRollback,
		PushedRunID: run.ID,
		Status:      model.HistoryRunRunning,
		BaseHead:    run.Head,
		Head:        rollbackHash.String(),
		StartedAt:   &now,
		RefsBefore:  jsonString(map[string]string{"target": run.Head}),
	}
	if err := h.runs.Create(rbRun); err != nil {
		return nil, err
	}

	if _, err := h.backend.Push(ctx, gitbackend.PushOptions{
		RepoPath: run.WorkRepoPath,
		Remote:   publish.Name,
		RefSpecs: []string{
			fmt.Sprintf("%s:refs/heads/%s", rollbackHash.String(), branch),
		},
		Force: true,
		Auth:  publishAuth,
	}); err != nil {
		_, _ = h.finishRun(rbRun, model.HistoryRunFailed, err.Error())
		return nil, fmt.Errorf("回滚强推到 %s 失败: %w", publish.Name, err)
	}
	rbRun.RefsAfter = jsonString(map[string]string{publish.Name: rollbackHash.String()})
	if _, err := h.finishRun(rbRun, model.HistoryRunSuccess, ""); err != nil {
		return nil, err
	}

	return &HistoryRollbackDTO{
		RunID:  runID,
		Ref:    ref,
		Pushed: []string{publish.Name},
		Heads:  map[string]string{publish.Name: rollbackHash.String()},
	}, nil
}

// productRoot 是移植产物的根目录。
//
// 优先用配置里指定的目录(sync.history_work_dir):产物要活到
// 「apply 之后的那次 push」,放临时目录的话系统清理会把待推送的产物删掉。
//
// 没配置时用系统临时目录下的**专属子目录** ——
// 不能直接用临时目录根:清理逻辑会扫到同机其它进程的东西。
//
// 拿不到配置时(单测里 Service 是字面量构造的)也不能 panic。
func (h *HistoryService) productRoot() string {
	root := ""
	if h.svc != nil {
		if cfg := h.svc.GetConfig(); cfg != nil {
			root = cfg.Sync.HistoryWorkDir
		}
	}
	if root == "" {
		// 每用户独占的目录:系统临时目录是多用户共享的,
		// 放在那里会出现「目录归属第一个创建者、第二个实例写不进去」。
		if cache, err := os.UserCacheDir(); err == nil && cache != "" {
			root = filepath.Join(cache, "git-ferry-history")
		} else {
			root = filepath.Join(os.TempDir(), "git-ferry-history")
		}
	}
	return filepath.Join(root, "history-work")
}

// pendingProductDirs 返回「还没推送成功」的产物目录(清理时必须保留)。
func (h *HistoryService) pendingProductDirs() []string {
	dirs, err := h.runs.ListPendingProductDirs()
	if err != nil {
		slog.Debug("history: 取待推送产物失败", "error", err)
		return nil
	}
	return dirs
}

// cleanupStaleProducts 清理过期的移植产物。
//
// **「还没推送成功」的产物一律保留** —— 只看「目录多旧」就删,
// 会把待推送的产物删掉:推送必然失败,而且产物不可恢复
// (重做 apply 也会因为基线变了而得到不同的 head)。
func (h *HistoryService) cleanupStaleProducts() {
	history.CleanupStaleProducts(h.productRoot(), h.pendingProductDirs()...)
}

// ===== 内部工具 =====

// channelAndOptions 取通道并解析成执行参数。
func (h *HistoryService) channelAndOptions(ctx context.Context, id uint) (*model.HistoryChannel, *history.Options, error) {
	ch, err := h.channels.FindByID(id)
	if err != nil {
		return nil, nil, err
	}
	if !ch.Enabled {
		return nil, nil, fmt.Errorf("通道已禁用: %s", ch.Name)
	}
	opts, err := h.channelOptions(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return ch, opts, nil
}

// channelOptions 把通道配置解析成可执行的选项(含仓库克隆)。
func (h *HistoryService) channelOptions(ctx context.Context, id uint) (*history.Options, error) {
	ch, err := h.channels.FindByID(id)
	if err != nil {
		return nil, err
	}
	if !ch.Enabled {
		return nil, fmt.Errorf("通道已禁用: %s", ch.Name)
	}

	// **身份映射是执行的前提**:没有 module 映射就没有任何改写规则,
	// 内网 module path 会原样进公开仓。宁可拒绝执行,也不发一个错的树。
	if strings.TrimSpace(ch.ModuleSource) == "" || strings.TrimSpace(ch.ModuleTarget) == "" {
		return nil, fmt.Errorf("通道 %s 缺少身份映射(module_source/module_target),拒绝执行:"+
			"没有改写规则会把内网 module path 原样发布到公开仓", ch.Name)
	}
	// host 映射必须成对:只给 source 不给 target,等于没配 ——
	// 内网主机名会原样进公开仓(与 module 映射同级的风险)。
	if (strings.TrimSpace(ch.HostSource) == "") != (strings.TrimSpace(ch.HostTarget) == "") {
		return nil, fmt.Errorf("通道 %s 的 host 映射不成对(host_source 与 host_target 必须同时给):"+
			"只配一半等于没配,内网主机名会原样发布到公开仓", ch.Name)
	}

	sourceDir := ch.SourceDir
	if sourceDir == "" {
		sourceDir, err = h.resolveRepoDir(ctx, ch, "source", ch.SourceRepoKey)
		if err != nil {
			return nil, err
		}
	}
	targetDir := ch.TargetDir
	if targetDir == "" {
		targetDir, err = h.resolveRepoDir(ctx, ch, "target", ch.TargetRepoKey)
		if err != nil {
			return nil, err
		}
	}
	// 本地路径直连:执行前再过一次闸门(配置写入后路径可能被换掉)。
	if err := validateHistoryRepoDir(sourceDir, "source_dir"); err != nil {
		return nil, err
	}
	if err := validateHistoryRepoDir(targetDir, "target_dir"); err != nil {
		return nil, err
	}

	opts := &history.Options{
		SourceDir: sourceDir,
		TargetDir: targetDir,
		SourceRef: ch.SourceBranch,
		TargetRef: ch.TargetBranch,
	}
	if ch.ModuleSource != "" && ch.ModuleTarget != "" {
		opts.Rules = append(opts.Rules, mirror.IdentityRule{
			Source: ch.ModuleSource, Target: ch.ModuleTarget, Kind: mirror.KindModule,
		})
	}
	if ch.HostSource != "" && ch.HostTarget != "" {
		opts.Rules = append(opts.Rules, mirror.IdentityRule{
			Source: ch.HostSource, Target: ch.HostTarget, Kind: mirror.KindHost,
		})
	}
	opts.AuthorName = ch.AuthorName
	opts.AuthorEmail = ch.AuthorEmail
	if ch.PathRules != "" {
		var pathRules []PathRuleDTO
		if err := json.Unmarshal([]byte(ch.PathRules), &pathRules); err != nil {
			return nil, fmt.Errorf("通道的路径规则配置损坏: %w", err)
		}
		for _, r := range pathRules {
			opts.PathRules = append(opts.PathRules, history.PathRule{From: r.From, To: r.To})
		}
	}
	if ch.ScrubRules != "" {
		var scrubRules []ScrubRuleDTO
		if err := json.Unmarshal([]byte(ch.ScrubRules), &scrubRules); err != nil {
			return nil, fmt.Errorf("通道的痕迹规则配置损坏: %w", err)
		}
		for i, r := range scrubRules {
			pattern, err := regexp.Compile(r.Pattern)
			if err != nil {
				return nil, fmt.Errorf("通道第 %d 条痕迹规则的 pattern 非法: %w", i+1, err)
			}
			severity := history.ScrubAuto
			if r.Severity == "mandatory" {
				severity = history.ScrubMandatory
			}
			opts.ScrubRules = append(opts.ScrubRules, history.ScrubRule{
				Name: r.Name, Category: r.Category, Pattern: pattern,
				Severity: severity, Replacement: r.Replacement,
			})
		}
	}
	if ch.ConflictRules != "" {
		var rules []history.ConflictRuleConfig
		if err := json.Unmarshal([]byte(ch.ConflictRules), &rules); err != nil {
			return nil, fmt.Errorf("通道的冲突判据配置损坏: %w", err)
		}
		opts.ConflictRules = rules
	}
	return opts, nil
}

// resolveRepoDir 把仓库 key 解析成本地克隆目录(必要时克隆)。
func (h *HistoryService) resolveRepoDir(ctx context.Context, ch *model.HistoryChannel,
	role, repoKey string) (string, error) {

	if repoKey == "" {
		return "", fmt.Errorf("通道 %s 未配置 %s 仓库", ch.Name, role)
	}
	repo, err := h.svc.GetRepoByKey(repoKey)
	if err != nil || repo == nil {
		return "", fmt.Errorf("%s 仓库不存在: %s", role, repoKey)
	}
	dir := filepath.Join(h.svc.GetConfig().Git.TempDir, "history",
		fmt.Sprintf("channel-%d", ch.ID), role)
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(statErr) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return "", err
		}
		auth := h.repoAuth(repo)
		if err := h.backend.Clone(ctx, gitbackend.CloneOptions{
			URL: repo.CloneURL, Path: dir, Auth: auth,
		}); err != nil {
			return "", fmt.Errorf("clone %s 失败: %w", repo.CloneURL, err)
		}
		return dir, nil
	}
	// 增量:拉取最新(远端固定 origin;失败则重建克隆)。
	auth := h.repoAuth(repo)
	if _, err := h.backend.Fetch(ctx, gitbackend.FetchOptions{
		RepoPath: dir, Remote: "origin", Tags: true, Auth: auth,
	}); err == nil {
		// **拉取之后必须把本地分支快进到远端**:fetch 只更新
		// refs/remotes/origin/*,不动本地 refs/heads/* ⇒ 不快进的话本地分支
		// 永远停在首次克隆时 ⇒ 差异计算与基线判定都用陈旧基线 ⇒ 第二次起的
		// 发布一律被判"远端已分叉"拦下(只能强推,强推又会丢掉远端的提交)。
		if ffErr := fastForwardMirrorBranches(dir, "origin"); ffErr != nil {
			// 快进失败不致命:推送闸门会按实际远端状态判定(分叉时报错而不是放行)。
			slog.Warn("history: 快进镜像分支失败(基线可能陈旧)", "dir", dir, "error", ffErr)
		}
	} else {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return "", rmErr
		}
		if err := h.backend.Clone(ctx, gitbackend.CloneOptions{
			URL: repo.CloneURL, Path: dir, Auth: auth,
		}); err != nil {
			return "", fmt.Errorf("re-clone %s 失败: %w", repo.CloneURL, err)
		}
	}
	return dir, nil
}

// repoAuth 读仓库凭据。规则与镜像中心同一套:
// executor.BuildRepoAuth(repo token 优先,回退平台 token;
// skipTLS 与 SSH host key 指纹一并回填)。
func (h *HistoryService) repoAuth(repo *model.Repo) gitbackend.AuthConfig {
	var p *model.Platform
	if repo != nil && repo.PlatformID > 0 {
		if got, err := h.svc.GetPlatformByID(context.Background(), repo.PlatformID); err == nil && got != nil {
			p = got
		}
	}
	return executor.BuildRepoAuth(repo, p)
}

// openRepo 打开一个 git 仓库目录。
func openRepo(dir string) (*git.Repository, error) {
	return git.PlainOpen(dir)
}

// resolveRollbackRef 解析回滚目标。
//
// 优先按 **commit hash** 解析:回滚的典型用法是退回到某次同步前的 HEAD
// (即执行记录里的 base_head),那是一个 hash 而不是分支名。
// 找不到时再退回到留底远端的同名分支。
func resolveRollbackRef(repoPath, remoteRefName, ref string) (plumbing.Hash, error) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	// 顺序很重要:
	//  1. **留底远端的同名分支**(refs/remotes/<backup>/<ref>)——
	//     回滚的语义就是「退回留底上的那个状态」;
	//  2. **revision**(commit hash、短 hash、HEAD~2 之类)。
	//
	// 原来把 revision 放在前面:传分支名("main")时解析到的是**本地**
	// main —— 而本地 main 就是本次移植的结果,于是回滚成了空操作:
	// 看上去成功,实际什么也没回滚。
	if h, err := repo.Reference(plumbing.ReferenceName(remoteRefName), true); err == nil {
		if commit, err := repo.CommitObject(h.Hash()); err == nil {
			return commit.Hash, nil
		}
	}
	if h, err := repo.ResolveRevision(plumbing.Revision(ref)); err == nil {
		if commit, err := repo.CommitObject(*h); err == nil {
			return commit.Hash, nil
		}
	}
	return plumbing.ZeroHash, fmt.Errorf(
		"解析回滚目标 %s 失败:既不是留底 %s 上的分支,也不是仓库里的提交", ref, remoteRefName)
}

// ensureRemote 确保仓库里有该远端且 URL 一致,返回远端名。

// ===== 裁决覆盖(人工/AI 拍板) =====

// HistoryOverrideInput 是裁决的入参。
type HistoryOverrideInput struct {
	// Path 目标侧路径(裁决定位键)。
	Path string `json:"path"`
	// SourcePath 源侧路径(可选,回溯用)。
	SourcePath string `json:"source_path"`
	// Verdict 裁决:keep_target / take_source / merged。
	Verdict string `json:"verdict"`
	// Content 裁决后的内容(仅 merged 时必填)。
	Content string `json:"content"`
	// Reason 裁决理由(审计用)。
	Reason string `json:"reason"`
	// DecidedBy 裁决者(人工或 AI 标识)。
	DecidedBy string `json:"decided_by"`
}

// HistoryOverrideDTO 是裁决的响应视图。
type HistoryOverrideDTO struct {
	ID         uint   `json:"id"`
	ChannelID  uint   `json:"channel_id"`
	Path       string `json:"path"`
	SourcePath string `json:"source_path"`
	Verdict    string `json:"verdict"`
	Content    string `json:"content,omitempty"`
	Reason     string `json:"reason"`
	DecidedBy  string `json:"decided_by"`
	CreatedAt  string `json:"created_at"`
}

// SetHistoryOverride 写入(或覆盖)一条裁决。
//
// 裁决只对 hold 生效;写入后重新 resolve 即按裁决出解,apply 时用裁决内容。
func (h *HistoryService) SetHistoryOverride(ctx context.Context, channelID uint, in HistoryOverrideInput) (*HistoryOverrideDTO, error) {
	// 裁决与执行互斥:裁决写进库、执行读裁决解冲突,不能交叉。
	release, err := h.lockChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.setHistoryOverride(channelID, in)
}

// setHistoryOverride 是 SetHistoryOverride 的内部实现(**调用方须已持有通道锁**)。
func (h *HistoryService) setHistoryOverride(channelID uint, in HistoryOverrideInput) (*HistoryOverrideDTO, error) {
	if _, err := h.channels.FindByID(channelID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return nil, fmt.Errorf("裁决必须指明路径")
	}
	switch in.Verdict {
	case model.HistoryVerdictKeepTarget, model.HistoryVerdictTakeSource:
	case model.HistoryVerdictMerged:
		if in.Content == "" {
			return nil, fmt.Errorf("merged 裁决必须提供 content")
		}
	default:
		return nil, fmt.Errorf("不支持的裁决类型: %s", in.Verdict)
	}
	ov := &model.HistoryOverride{
		ChannelID:  channelID,
		Path:       strings.TrimSpace(in.Path),
		SourcePath: in.SourcePath,
		Verdict:    in.Verdict,
		Content:    in.Content,
		Reason:     in.Reason,
		DecidedBy:  in.DecidedBy,
	}
	if err := h.overrides.Upsert(ov); err != nil {
		return nil, err
	}
	return toOverrideDTO(ov), nil
}

// ListHistoryOverrides 列出通道的裁决。
func (h *HistoryService) ListHistoryOverrides(_ context.Context, channelID uint) ([]*HistoryOverrideDTO, error) {
	list, err := h.overrides.FindByChannel(channelID)
	if err != nil {
		return nil, err
	}
	out := make([]*HistoryOverrideDTO, 0, len(list))
	for _, ov := range list {
		out = append(out, toOverrideDTO(ov))
	}
	return out, nil
}

// DeleteHistoryOverride 删除一条裁决(回到自动判定)。
func (h *HistoryService) DeleteHistoryOverride(_ context.Context, id uint) error {
	return h.overrides.Delete(id)
}

// toOverrideDTO 把裁决转成响应视图。
func toOverrideDTO(ov *model.HistoryOverride) *HistoryOverrideDTO {
	return &HistoryOverrideDTO{
		ID:         ov.ID,
		ChannelID:  ov.ChannelID,
		Path:       ov.Path,
		SourcePath: ov.SourcePath,
		Verdict:    ov.Verdict,
		Content:    ov.Content,
		Reason:     ov.Reason,
		DecidedBy:  ov.DecidedBy,
		CreatedAt:  ov.CreatedAt.Format("2006-01-02 15:04:05"),
	}
}

// ===== 凭据轮换 =====

// HistoryCredentialInput 是轮换凭据的入参。
//
// 轮换**只换密钥**:URL、目标名都不动 —— 换密钥是常规运维动作,
// 不该有机会顺手改掉推送到哪里。
type HistoryCredentialInput struct {
	// CredType 凭据类型:none / token / ssh_key。
	CredType string `json:"cred_type"`
	// Credential 凭据明文(明文只在传输中出现,落库即加密)。
	Credential string `json:"credential"`
	// Username HTTPS Basic 用户名(token 类用)。
	Username string `json:"username"`
	// ExpiresAt 凭据到期时间(可选)。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// RotateHistoryTargetCredential 轮换一个推送目标的凭据。
//
// 与 UpdateHistoryTarget 的区别:**只改凭据**,不碰 URL / 目标名 ——
// 换密钥是常规运维动作,不该有机会顺手改掉推送到哪里。
func (h *HistoryService) RotateHistoryTargetCredential(_ context.Context, id uint, in HistoryCredentialInput) (*HistoryTargetDTO, error) {
	t, err := h.targets.FindByID(id)
	if err != nil {
		return nil, err
	}
	credType, encrypted, username, err := h.encryptCredential(HistoryTargetInput{
		CredType:   in.CredType,
		Credential: in.Credential,
		Username:   in.Username,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	t.CredType = credType
	t.Credential = encrypted
	t.Username = username
	t.CredentialUpdatedAt = &now
	t.CredentialExpiresAt = in.ExpiresAt
	if err := h.targets.Update(t); err != nil {
		return nil, err
	}
	return toTargetDTO(t), nil
}

// CredentialStatus 是一个推送目标的凭据状态。
type CredentialStatus struct {
	// TargetID 目标 ID。
	TargetID uint `json:"target_id"`
	// Name 目标名。
	Name string `json:"name"`
	// HasCredential 是否有凭据。
	HasCredential bool `json:"has_credential"`
	// UpdatedAt 凭据最后更新时间。
	UpdatedAt string `json:"updated_at,omitempty"`
	// ExpiresAt 凭据到期时间。
	ExpiresAt string `json:"expires_at,omitempty"`
	// Expired 已过期。
	Expired bool `json:"expired"`
	// ExpiringSoon 临期(7 天内)。
	ExpiringSoon bool `json:"expiring_soon"`
	// Note 说明。
	Note string `json:"note"`
}

// credentialStatus 汇总凭据状态。
func (h *HistoryService) credentialStatus(targets []*model.HistoryTarget) []CredentialStatus {
	now := time.Now()
	out := make([]CredentialStatus, 0, len(targets))
	for _, t := range targets {
		st := CredentialStatus{
			TargetID:      t.ID,
			Name:          t.Name,
			HasCredential: t.Credential != "",
		}
		if t.CredentialUpdatedAt != nil {
			st.UpdatedAt = t.CredentialUpdatedAt.Format("2006-01-02 15:04:05")
		}
		if t.CredentialExpiresAt != nil {
			st.ExpiresAt = t.CredentialExpiresAt.Format("2006-01-02 15:04:05")
			switch {
			case now.After(*t.CredentialExpiresAt):
				st.Expired = true
				st.Note = "凭据已过期,推送会认证失败"
			case t.CredentialExpiresAt.Sub(now) < 7*24*time.Hour:
				st.ExpiringSoon = true
				st.Note = "凭据即将过期,请安排轮换"
			}
		}
		if st.Note == "" && !st.HasCredential {
			st.Note = "无凭据(公开仓库或本地路径)"
		}
		out = append(out, st)
	}
	return out
}

// ===== 通道入参辅助 =====

// derefStr 取指针值(nil → 空串)。
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// setIfGiven 在「调用方给了值」时覆盖目标(指针语义:nil = 未指定)。
func setIfGiven(dst *string, given *string) {
	if given == nil {
		return
	}
	*dst = *given
}

// sensitiveLocalRoots 是**不允许**作为历史同步本地目录的系统位置。
//
// 这些位置装的是凭据与系统配置,不是「待发布的代码」。
// 把它们当源仓,一次执行就能把内容推到远端 ——
// 配合可配置的推送目标,等于把本机密钥发出去。
var sensitiveLocalRoots = []string{
	"/etc", "/proc", "/sys", "/dev", "/boot", "/root", "/run",
	"/var/lib", "/var/run", "/var/spool",
	// macOS 的 /etc /var 是 /private 下的软链,解析后要走这条。
	"/private/etc", "/private/var/lib", "/private/var/run", "/private/var/spool",
}

// sensitiveHomeNames 是家目录下**不允许**直连的子目录(凭据与配置)。
var sensitiveHomeNames = []string{
	".ssh", ".aws", ".gnupg", ".kube", ".docker", ".config",
	".password-store", ".git-credentials", ".netrc", ".npmrc", ".pypirc",
}

// validateHistoryRepoDir 校验「本地路径直连」的目录能不能当历史同步的仓。
//
// 规则(每条都有实际风险支撑):
//  1. 必须是绝对路径 —— 相对路径的基准不确定,不该当配置项;
//  2. 必须存在且是目录;
//  3. 必须是**这个路径本身**就是一个 git 仓(工作树或裸仓库):
//     不是的话,git 会向上层目录找仓,实际发布的范围会超出配置,
//     指到 /etc、~/.ssh 这类地方时更是直接把本机内容发出去;
//  4. 不能落在系统敏感目录或家目录的凭据子目录里。
//
// 空目录 = 未指定(走 repo key 路径)→ 放行。
func validateHistoryRepoDir(dir, field string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s 必须是绝对路径: %s", field, dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%s 不可用: %w", field, err)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s 必须是已存在的目录: %s", field, dir)
	}
	if !isGitRepoDir(real) {
		return fmt.Errorf("%s 必须指向一个 git 仓库(工作树或裸仓库): %s", field, dir)
	}
	return checkSensitivePath(real, field)
}

// isGitRepoDir 判断目录**本身**是不是一个 git 仓。
//
// 不用 git 的探测:它会向上层目录找仓,那样「指到哪就发布哪」就不成立了。
func isGitRepoDir(dir string) bool {
	// 工作树:.git 是目录;worktree/submodule 下 .git 是文件(指向真实 gitdir)。
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	// 裸仓库:HEAD + objects + refs。
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || head.IsDir() {
		return false
	}
	obj, err := os.Stat(filepath.Join(dir, "objects"))
	if err != nil || !obj.IsDir() {
		return false
	}
	refs, err := os.Stat(filepath.Join(dir, "refs"))
	return err == nil && refs.IsDir()
}

// checkSensitivePath 拒绝系统敏感位置与家目录的凭据子目录。
func checkSensitivePath(real, field string) error {
	clean := filepath.Clean(real)
	for _, root := range sensitiveLocalRoots {
		if clean == root || strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			return fmt.Errorf("%s 不能指向系统敏感目录: %s", field, clean)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	home = filepath.Clean(home)
	if clean == home {
		return fmt.Errorf("%s 不能指向家目录本身: %s", field, clean)
	}
	rel, err := filepath.Rel(home, clean)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	first := strings.Split(filepath.ToSlash(rel), "/")[0]
	for _, name := range sensitiveHomeNames {
		if first == name {
			return fmt.Errorf("%s 不能指向家目录下的凭据目录: %s", field, clean)
		}
	}
	return nil
}

// isCommitHash 判断字符串是否像一个 commit hash(全十六进制、7~40 位)。
func isCommitHash(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// validatePathRules 校验路径规则:目标前缀不能越出仓库。
//
// `To` 会拼成目标仓里的路径,带 `..` 就能写到工作目录之外
// (实测能写到 out/../escaped/x.go)。规则是调用方给的,必须在入口挡住。
func validatePathRules(rules []PathRuleDTO) error {
	for _, r := range rules {
		for _, part := range []string{r.From, r.To} {
			if part == "" {
				continue
			}
			slashed := filepath.ToSlash(part)
			if strings.HasPrefix(slashed, "/") {
				return fmt.Errorf("路径规则的前缀必须是仓库内相对路径(不能以 / 开头): %q", part)
			}
			for _, seg := range strings.Split(slashed, "/") {
				if seg == ".." {
					return fmt.Errorf("路径规则的前缀不能含 ..(会写到仓库外): %q", part)
				}
			}
		}
	}
	return nil
}
