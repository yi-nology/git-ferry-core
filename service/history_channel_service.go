package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	errors "github.com/cockroachdb/errors"
	"gorm.io/gorm"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/executor"
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
}

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
	return &HistoryService{
		svc:       svc,
		channels:  dao.NewHistoryChannelDAO(db),
		targets:   dao.NewHistoryTargetDAO(db),
		runs:      dao.NewHistoryRunDAO(db),
		overrides: dao.NewHistoryOverrideDAO(db),
		backend:   backend,
		cm:        cm,
	}, nil
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
	if in.ModuleSource == "" || in.ModuleTarget == "" {
		return nil, fmt.Errorf("module_source 与 module_target 都不能为空(身份改写是历史同步的前提)")
	}
	if in.SourceRepoKey == "" && in.SourceDir == "" {
		return nil, fmt.Errorf("source_repo_key 与 source_dir 至少给一个")
	}
	if in.TargetRepoKey == "" && in.TargetDir == "" {
		return nil, fmt.Errorf("target_repo_key 与 target_dir 至少给一个")
	}
	if existing, err := h.channels.FindByName(in.Name); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("通道名已存在: %s", in.Name)
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
		SourceRepoKey: in.SourceRepoKey,
		TargetRepoKey: in.TargetRepoKey,
		SourceDir:     in.SourceDir,
		TargetDir:     in.TargetDir,
		ModuleSource:  in.ModuleSource,
		ModuleTarget:  in.ModuleTarget,
		HostSource:    in.HostSource,
		HostTarget:    in.HostTarget,
		PathRules:     string(pathRules),
		ScrubRules:    string(scrubRules),
		ConflictRules: string(conflictRules),
		SourceBranch:  in.SourceBranch,
		TargetBranch:  in.TargetBranch,
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
func (h *HistoryService) UpdateHistoryChannel(_ context.Context, id uint, in HistoryChannelInput) (*HistoryChannelDTO, error) {
	ch, err := h.channels.FindByID(id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		ch.Name = strings.TrimSpace(in.Name)
	}
	ch.SourceRepoKey = in.SourceRepoKey
	ch.TargetRepoKey = in.TargetRepoKey
	ch.SourceDir = in.SourceDir
	ch.TargetDir = in.TargetDir
	ch.ModuleSource = in.ModuleSource
	ch.ModuleTarget = in.ModuleTarget
	ch.HostSource = in.HostSource
	ch.HostTarget = in.HostTarget
	ch.SourceBranch = in.SourceBranch
	ch.TargetBranch = in.TargetBranch
	if in.Enabled != nil {
		// 未指定时保持原值,避免「不传 enabled」把已禁用的通道重新启用。
		ch.Enabled = *in.Enabled
	}

	if in.PathRules != nil {
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
func (h *HistoryService) DeleteHistoryChannel(_ context.Context, id uint) error {
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
	credType, credential, username, err := h.encryptCredential(in)
	if err != nil {
		return nil, err
	}
	t := &model.HistoryTarget{
		ChannelID:  channelID,
		Name:       strings.TrimSpace(in.Name),
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
		t.RepoURL = strings.TrimSpace(in.RepoURL)
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
	ch, opts, err := h.channelAndOptions(ctx, channelID)
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

	res, err := history.Apply(ctx, history.ApplyOptions{
		SourceDir:  opts.SourceDir,
		TargetDir:  opts.TargetDir,
		TargetRef:  opts.TargetRef,
		Rules:      opts.Rules,
		PathRules:  opts.PathRules,
		ScrubRules: opts.ScrubRules,
		Plan:       plan,
	})
	if err != nil {
		return h.finishRun(run, model.HistoryRunFailed, err.Error())
	}

	run.BaseHead = res.BaseHead
	run.Head = res.Head
	run.WorkRepoPath = res.RepoPath
	run.PlanSummary = jsonString(plan.Summary)
	run.Commits = jsonString(res.Commits)
	run.Files = jsonString(res.AppliedFileList())
	run.Skipped = jsonString(res.Skipped)
	run.ScrubReport = jsonString(res.MandatoryHits)
	run.GateReport = jsonString(map[string]any{
		"gofmt":  "见物化目录的 gofmt 校验(M2 门禁)",
		"files":  res.Files,
		"commit": len(res.Commits),
	})
	if err := h.runs.Update(run); err != nil {
		return nil, err
	}
	return h.finishRun(run, model.HistoryRunSuccess, "")
}

// finishRun 收尾执行记录(状态 + 耗时 + **入链**)。
//
// 哈希只在终态计算:执行中的记录字段还在变,提前算出的哈希必然失配。
// 续链取该通道最近一条**已入链**的记录 —— 执行中的记录没有哈希,必须跳过。
func (h *HistoryService) finishRun(run *model.HistoryRun, status, errMsg string) (*HistoryRunDTO, error) {
	now := time.Now()
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
		if expect := model.ComputeHistoryRunHash(r.PrevHash, r); r.EntryHash != expect {
			res.OK = false
			res.BrokenAt = r.ID
			res.Message = "entry_hash mismatch (tampered?)"
			return res, nil
		}
		prev = r.EntryHash
		res.Checked++
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
	// 找最近一次 **apply** 记录:apply 记录才有产物路径。
	// (最新的记录可能是上一条 push 记录, 它自己不带产物。)
	run, err := h.runs.FindLatestByChannelKind(channelID, model.HistoryRunKindApply)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("通道 %d 还没有 apply 记录,先 apply", channelID)
	}
	return h.pushRun(ctx, run)
}

// HistoryPushRun 推送指定执行记录的产物。
func (h *HistoryService) HistoryPushRun(ctx context.Context, runID uint) (*HistoryPushDTO, error) {
	run, err := h.runs.FindByID(runID)
	if err != nil {
		return nil, err
	}
	return h.pushRun(ctx, run)
}

// pushRun 执行推送:按通道目标的顺序依次推送,任一失败立即中止。
func (h *HistoryService) pushRun(ctx context.Context, run *model.HistoryRun) (*HistoryPushDTO, error) {
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

	auths := make(map[string]gitbackend.AuthConfig, len(targets))
	for _, t := range targets {
		auth, err := h.targetAuth(t)
		if err != nil {
			return nil, err
		}
		auths[t.Name] = auth
	}

	// 逐个目标推送:用各自凭据。任一失败即中止,不推其余。
	report := &HistoryPushDTO{RunID: run.ID, Heads: map[string]string{}}
	branch := "main"
	if ch, err := h.channels.FindByID(run.ChannelID); err == nil && ch.TargetBranch != "" {
		branch = ch.TargetBranch
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
			Targets:  []history.PushTarget{{Name: t.Name, URL: t.RepoURL}},
			Branch:   branch,
			Auth:     auths[t.Name],
			Force:    true,
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
		return nil, fmt.Errorf("打开产物工作仓库失败: %w", err)
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
	rollbackHash, err := resolveRollbackRef(run.WorkRepoPath, "refs/remotes/"+backup.Name+"/"+ref)
	if err != nil {
		return nil, fmt.Errorf("留底 %s 里没有 ref %s: %w", backup.Name, ref, err)
	}

	// 把该提交强推到发布目标。
	publishRemote, err := ensureRemote(workRepo, publish.Name, publish.RepoURL)
	if err != nil {
		return nil, err
	}
	_ = publishRemote
	branch := "main"
	if ch, err := h.channels.FindByID(run.ChannelID); err == nil && ch.TargetBranch != "" {
		branch = ch.TargetBranch
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
	}); err != nil {
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
func resolveRollbackRef(repoPath, remoteRefName string) (plumbing.Hash, error) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	// remoteRefName 形如 refs/remotes/backup/main;取末段作为候选 ref 名。
	parts := strings.Split(remoteRefName, "/")
	candidate := parts[len(parts)-1]
	if h, err := repo.ResolveRevision(plumbing.Revision(candidate)); err == nil {
		if commit, err := repo.CommitObject(*h); err == nil {
			return commit.Hash, nil
		}
	}
	ref, err := repo.Reference(plumbing.ReferenceName(remoteRefName), true)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// ensureRemote 确保仓库里有该远端且 URL 一致,返回远端名。
func ensureRemote(repo *git.Repository, name, url string) (string, error) {
	remotes, err := repo.Remotes()
	if err != nil {
		return "", fmt.Errorf("读取远端列表失败: %w", err)
	}
	for _, r := range remotes {
		if r.Config().Name != name {
			continue
		}
		if len(r.Config().URLs) > 0 && r.Config().URLs[0] == url {
			return name, nil
		}
		if err := repo.DeleteRemote(name); err != nil {
			return "", fmt.Errorf("删除远端 %s 失败: %w", name, err)
		}
		break
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: name, URLs: []string{url},
	}); err != nil {
		return "", fmt.Errorf("创建远端 %s 失败: %w", name, err)
	}
	return name, nil
}

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
func (h *HistoryService) SetHistoryOverride(_ context.Context, channelID uint, in HistoryOverrideInput) (*HistoryOverrideDTO, error) {
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
