package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/yi-nology/git-ferry-core/pkg/strutil"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	errors "github.com/cockroachdb/errors"
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
	locks sync.Map
	// redisLock 配 redis 时提供跨实例互斥：抢不到 = 另一实例正在执行同一通道，
	// 直接返回忙碌错误而不是两边同时克隆（TTL 见 mirrorLockTTL，覆盖整次操作）。
	redisLock *lock.RedisLock
	lockTTL   time.Duration
}

// mirrorLockTTL 通道锁 TTL：覆盖一次克隆+推送的最长耗时。
// 远大于 lock.defaultLockTTL(30s)，避免操作中途锁过期导致并发进入。
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
	// 配 redis 时追加跨实例互斥；未配或构造失败只降级为进程内互斥（有告警）。
	var rl *lock.RedisLock
	if cfg := svc.GetConfig(); cfg != nil && cfg.Redis.Addr != "" {
		rl = lock.NewRedisLock(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	}
	return &MirrorService{
		svc:       svc,
		channels:  dao.NewMirrorChannelDAO(db),
		runs:      dao.NewMirrorRunDAO(db),
		backend:   backend,
		cm:        cm,
		redisLock: rl,
		lockTTL:   mirrorLockTTL,
	}, nil
}

// lockChannel 获取通道级互斥：先取进程内互斥（阻塞，保持既有串行语义），
// 再抢跨实例锁（配 redis 时；被其它实例占用 → 忙碌错误，不排队等待）。
// 返回幂等 release；通道被另一实例占用时返回忙碌错误。
func (m *MirrorService) lockChannel(ctx context.Context, chID uint) (release func(), err error) {
	mu, _ := m.locks.LoadOrStore(fmt.Sprintf("repo-%d", chID), &sync.Mutex{})
	gate := mu.(*sync.Mutex)
	gate.Lock()

	var once sync.Once
	unlock := func() {
		once.Do(func() {
			gate.Unlock()
			if m.redisLock != nil {
				if uerr := m.redisLock.Unlock(ctx, fmt.Sprintf("mirror-channel-%d", chID)); uerr != nil {
					// 未持有(降级路径)或已过期：仅告警，TTL 会兜底
					slog.Debug("mirror: 释放跨实例锁", "channel", chID, "error", uerr)
				}
			}
		})
	}

	if m.redisLock != nil {
		key := fmt.Sprintf("mirror-channel-%d", chID)
		ok, lerr := m.redisLock.TryLockWithTTL(ctx, key, m.lockTTL)
		if lerr != nil {
			// redis 抖动：降级为进程内互斥，不阻断镜像任务
			slog.Warn("mirror: 跨实例锁获取失败,降级为进程内互斥", "channel", chID, "error", lerr)
		} else if !ok {
			gate.Unlock()
			return func() {}, fmt.Errorf("通道 %d 正在其它实例执行,请稍后重试", chID)
		}
	}
	return unlock, nil
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
}

type CreateMirrorChannelInput struct {
	Name    string              `json:"name"`
	Mode    string              `json:"mode"`
	RepoKey string              `json:"repoKey"`
	Targets []MirrorTargetInput `json:"targets"`
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
func (m *MirrorService) ensureRepo(ctx context.Context, ch *model.MirrorChannel) (dir string, cleanup func(), err error) {
	unlock, lerr := m.lockChannel(ctx, ch.ID)
	if lerr != nil {
		return "", func() {}, lerr
	}
	defer unlock()

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
	}
	return dir, cleanup, nil
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
		return gitbackend.NewSSHKeyContentAuth(plain, ""), nil
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
	rep, err := m.publishOne(ctx, ch, t, tag, false, true)
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
	running, err := m.runs.HasRunningByChannel(channelID)
	if err != nil {
		return nil, err
	}
	if running {
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
		return nil, errors.Wrap(err, "create mirror run failed")
	}

	// 异步执行:挂到 Service 的后台上下文,随服务关停而取消
	runCtx, cancel := context.WithCancel(m.svc.bgCtx)
	m.svc.wg.Add(1)
	go func() {
		defer m.svc.wg.Done()
		defer cancel()
		m.execute(runCtx, run.ID, ch, t, in)
	}()
	return run, nil
}

func (m *MirrorService) execute(ctx context.Context, runID uint, ch *model.MirrorChannel, t *model.MirrorTarget, in ExecuteMirrorRunInput) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mirror run panic", "run", runID, "panic", r)
			m.failRun(runID, fmt.Sprintf("内部错误: %v", r))
		}
	}()

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
func (m *MirrorService) executePublish(ctx context.Context, run *model.MirrorRun, ch *model.MirrorChannel, t *model.MirrorTarget, in ExecuteMirrorRunInput) error {
	dir, cleanup, err := m.ensureRepo(ctx, ch)
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

		rep, tagErr := m.publishOne(ctx, ch, t, tag, in.AllowOverwrite, false)
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
func (m *MirrorService) publishOne(ctx context.Context, ch *model.MirrorChannel, t *model.MirrorTarget, tag string, allowOverwrite, dryRun bool) (*mirror.TagReport, error) {
	dir, cleanup, err := m.ensureRepo(ctx, ch)
	if err != nil {
		return nil, err
	}
	_ = cleanup
	// 预检/发布都要先把目标仓库接为本地克隆的远端
	if err := m.ensureTargetRemote(ctx, dir, t, gitbackend.AuthConfig{Type: gitbackend.AuthNone}); err != nil {
		return nil, fmt.Errorf("配置目标远端失败: %w", err)
	}
	auth, err := m.targetAuth(t)
	if err != nil {
		return nil, err
	}
	rep2, err := mirror.Publish(ctx, mirror.Options{
		RepoDir:        dir,
		Mapping:        mirror.Mapping{Source: ch.Module, Target: t.TargetModule},
		Tags:           []string{tag},
		Remote:         t.Remote,
		Auth:           auth,
		AllowOverwrite: allowOverwrite,
		DryRun:         dryRun,
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
