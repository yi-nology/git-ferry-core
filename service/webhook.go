package service

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/oekit/concurrency/async"
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/go-git-platform/pkg/branchfilter"
)

// webhookEventID 决定入库用的事件 ID,以及是否需要按 ID 去重。
//
//   - 非空 ID ⇒ 原样使用并**需要去重**(平台投递 ID 是真幂等键:重投时值不变)。
//   - 空 ID   ⇒ 合成一个唯一 ID 且**不去重**:拿空 ID 查重会把别的仓库的事件
//     挡掉,拿空 ID 入库会撞唯一索引 ⇒ 第 2 条起的事件被静默丢弃。
//
// SDK 侧已保证事件 ID 非空,这里是数据完整性兜底(不依赖上游不变量)。
func webhookEventID(raw string) (id string, needDedup bool) {
	if s := strings.TrimSpace(raw); s != "" {
		return s, true
	}
	return syntheticEventID(), false
}

// syntheticEventID 给"没有事件 ID"的平台合成一个唯一 ID。
//
// 只在没有可用去重键时使用(宁可重复处理,也不能丢事件);前缀 syn- 便于
// 在库里一眼认出"这是合成 ID,不是平台投递 ID"。
func syntheticEventID() string {
	return "syn-" + uuid.NewString()
}

// isDuplicateKeyErr 判断错误是否为 DB 唯一约束冲突(MySQL 1062 / SQLite UNIQUE)。
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || // MySQL
		strings.Contains(msg, "UNIQUE constraint failed") || // SQLite
		strings.Contains(msg, "duplicate key") // PostgreSQL
}

// WebhookPayload 是协议无关的入站 Webhook 载荷。
// core 不提供 HTTP 服务端；壳层（hz/gin/…）负责收包后转成该结构再调用。
// 内部会拼一个临时 *http.Request 交给 git-platform-sdk 做验签/解析（SDK 契约如此），
// 库对外 API 不暴露 Web 服务器语义。
type WebhookPayload struct {
	Method     string
	Path       string
	Header     map[string][]string
	Body       []byte
	RemoteAddr string
}

// toHTTPRequest 仅用于对接 SDK 的 WebhookManager，勿在业务路径外使用。
func (p *WebhookPayload) toHTTPRequest() *http.Request {
	method := p.Method
	if method == "" {
		method = http.MethodPost
	}
	path := p.Path
	if path == "" {
		path = "/"
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(p.Body))
	if err != nil {
		// NewRequest 对 method/path 的合法组合几乎不失败；兜底用 POST /
		req, _ = http.NewRequest(http.MethodPost, "/", bytes.NewReader(p.Body))
	}
	for k, vals := range p.Header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.RemoteAddr = p.RemoteAddr
	req.ContentLength = int64(len(p.Body))
	return req
}

func (s *Service) ReceiveWebhook(ctx context.Context, repoKey string, payload *WebhookPayload) error {
	if payload == nil {
		return fmt.Errorf("webhook payload is nil")
	}

	repo, prov, err := s.repos.GetRepoWithProvider(repoKey)
	if err != nil {
		return err
	}

	req := payload.toHTTPRequest()
	if err := prov.ValidateWebhookSignature(req, repo.WebhookSecret); err != nil {
		return fmt.Errorf("invalid webhook signature: %w", err)
	}

	event, err := prov.ParseWebhookEvent(req, repo.WebhookSecret)
	if err != nil {
		return fmt.Errorf("parse webhook event failed: %w", err)
	}

	// 事件 ID 的处理:**绝不能用空 ID**。
	//
	// ① 去重键是 event_id(唯一索引)⇒ 空 ID 会与"第一条空 ID 事件"永久相撞
	//    ⇒ 第 2 条起的事件被静默丢弃(不建事件、不匹配规则、不触发同步);
	// ② 拿空 ID 去查重更糟:会把**别的仓库**的事件当成重复,直接挡掉。
	//
	// SDK 侧已保证事件 ID 非空(platform.WebhookEnvelope.NewEvent 用平台名兜底
	// 前缀 + 优先用平台投递 ID),这里再兜一层:万一某个后端仍给出空 ID,
	// 宁可重复处理也不丢事件。
	eventID, needDedup := webhookEventID(event.ID)
	if needDedup {
		existing, err := s.webhooks.FindEventByEventID(eventID)
		if err != nil {
			return err
		}
		if existing != nil {
			return nil
		}
	}

	actorName := ""
	if event.Actor != nil {
		actorName = event.Actor.Name
	}

	whEvent := &model.WebhookEvent{
		EventID:   eventID,
		RepoKey:   repoKey,
		EventType: event.Type,
		Source:    string(event.Source),
		ActorName: actorName,
		Branch:    event.Branch,
		CommitSHA: event.CommitSHA,
		Payload:   event.RawPayload,
		Status:    model.StatusReceived,
	}

	if err := s.webhooks.CreateWebhookEvent(whEvent); err != nil {
		// 并发去重:event_id 有唯一索引,插入冲突说明另一个请求已处理该事件,直接幂等返回。
		// 依赖 DB 唯一约束保证正确性,不再做二次查询(消除竞态窗口+减少一次 DB 往返)。
		if isDuplicateKeyErr(err) {
			return nil
		}
		return err
	}

	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()
		s.applyRules(s.bgCtx, repoKey, whEvent)
	})

	return nil
}

func (s *Service) applyRules(ctx context.Context, repoKey string, event *model.WebhookEvent) {
	eventID := event.ID

	// 正常入站路径必须闭环状态机:received → processing → processed。
	// 此前只在 RetryEvent 里标记 processed,首次接收的事件会永远停在
	// received,历史列表无法区分「待处理」和「已处理完」。
	if _, err := s.webhooks.MarkEventProcessing(ctx, eventID); err != nil {
		slog.Warn("mark event processing failed", "eventID", eventID, "error", err)
	}
	// **触发失败 ⇒ 事件落 failed 终态**(而不是 processed):
	// 否则事件列表里与成功事件无法区分,运维看不出"这次没同步",
	// 也没有可重试的对象。
	if err := s.webhooks.ApplyRules(ctx, repoKey, event, &s.lastTriggerTime, func(ctx context.Context, taskKey, trigger string, webhookEventID *uint) error {
		return s.RunTaskWithTrigger(ctx, taskKey, trigger, webhookEventID)
	}, &eventID); err != nil {
		slog.Error("webhook 触发失败,事件标记为 failed", "eventID", eventID, "error", err)
		if mErr := s.webhooks.MarkEventFailed(event, err.Error()); mErr != nil {
			slog.Error("mark event failed failed", "eventID", eventID, "error", mErr)
		}
		return
	}

	if err := s.webhooks.MarkEventProcessed(event); err != nil {
		slog.Error("mark event processed failed", "eventID", eventID, "error", err)
	}
}

func (s *Service) RetryEvent(ctx context.Context, eventID uint) error {
	event, err := s.webhooks.MarkEventProcessing(ctx, eventID)
	if err != nil {
		return err
	}
	// 纳入 WaitGroup + 用 bgCtx,优雅关停时能被等待/取消,不再泄露 goroutine。
	// applyRules 内部会闭环 processing → processed,这里不再重复标记。
	s.wg.Add(1)
	async.GoSafe(func() {
		defer s.wg.Done()
		s.applyRules(s.bgCtx, event.RepoKey, event)
	})
	return nil
}

// ListRules returns webhook rules for a repository.
func (s *Service) ListRules(ctx context.Context, repoKey string) ([]*model.WebhookRule, error) {
	return s.webhooks.ListRules(ctx, repoKey)
}

// GetRule returns a webhook rule by ID.
func (s *Service) GetRule(ctx context.Context, id uint) (*model.WebhookRule, error) {
	return s.webhooks.GetRule(ctx, id)
}

// CreateRule creates a new webhook rule.
func (s *Service) CreateRule(ctx context.Context, req *model.CreateRuleRequest) (*model.WebhookRule, error) {
	return s.webhooks.CreateRule(ctx, req)
}

// UpdateRule updates an existing webhook rule.
func (s *Service) UpdateRule(ctx context.Context, req *model.UpdateRuleRequest) (*model.WebhookRule, error) {
	return s.webhooks.UpdateRule(ctx, req)
}

// DeleteRule deletes a webhook rule by ID.
func (s *Service) DeleteRule(ctx context.Context, id uint) error {
	return s.webhooks.DeleteRule(ctx, id)
}

// ListEvents returns webhook events for a repository.
func (s *Service) ListEvents(ctx context.Context, repoKey string, offset, limit int) ([]*model.WebhookEvent, int64, error) {
	return s.webhooks.ListEvents(ctx, repoKey, offset, limit)
}

// matchEventType 事件类型匹配,复用 SDK branchfilter(逗号分隔 glob,
// 空/ "*" 匹配全部;顺带支持 "push*" 等模式)。
func matchEventType(pattern, actual string) bool {
	f, err := branchfilter.New(pattern)
	if err != nil {
		// 非法模式按全匹配兜底,避免 webhook 整条链路因配置笔误失效
		return true
	}
	return f.Match(actual)
}
