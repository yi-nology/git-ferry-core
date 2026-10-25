package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
	sdkprov "github.com/yi-nology/go-git-platform/provider"

	_ "github.com/yi-nology/go-git-platform/backends/all"
)

// githubPushBody 一条最小的 GitHub push 载荷。
const githubPushBody = `{"ref":"refs/heads/main","before":"1111111111111111111111111111111111111111",` +
	`"after":"2222222222222222222222222222222222222222",` +
	`"repository":{"id":1,"full_name":"octo/demo"},` +
	`"sender":{"id":2,"login":"tester"},"commits":[]}`

// githubSign GitHub 的 X-Hub-Signature-256 头格式:sha256=<hex(hmac-sha256(body, secret))>。
func githubSign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestReceiveWebhook_MultipleDeliveries_AllRecorded 连续两次投递,两条事件都必须入库。
//
// 回归背景(P0,入站同步从第 2 条起完全失效):
// GitHub / GitCode 的 webhook envelope **没有 IDPrefix** ⇒ SDK 的 NewEvent 不写
// 事件 ID ⇒ `event.ID == ""`。而 core 按 event_id 做**唯一索引 + 去重**:
//
//	第 1 条事件以 event_id="" 入库;
//	第 2 条起 → FindByEventID("") 命中第 1 条 ⇒ 直接 return(不建事件、不匹配
//	规则、不触发同步);并发时还会撞唯一索引 → isDuplicateKeyErr → 同样静默返回。
//
// ⇒ GitHub 上配了入站同步,第一条之后所有推送都静默消失。
//
// 这条用例是**反向验证**:把 SDK 的 NewEvent 改回"前缀为空就不写 ID",
// 或把 core 改回用空 ID 查重,本用例会变红(事件数只有 1)。
func TestReceiveWebhook_MultipleDeliveries_AllRecorded(t *testing.T) {
	const secret = "wh-secret"
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Platform{}, &model.Repo{}, &model.WebhookRule{},
		&model.WebhookRuleTask{}, &model.WebhookEvent{},
	))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	// 平台 API 不会真被调用(验签与解析都在本地),给个可达地址即可
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()

	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	ruleDAO := dao.NewWebhookRuleDAO(db)
	eventDAO := dao.NewWebhookEventDAO(db)

	platform := &model.Platform{
		Key: "gh", Name: "GitHub", Type: string(sdkprov.PlatformGitHub), APIURL: srv.URL + "/",
	}
	require.NoError(t, platformDAO.Create(platform))
	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "octo/demo", Name: "demo", PlatformID: platform.ID, Platform: platform.Type,
		PlatformOwner: "octo", PlatformRepo: "demo",
		CloneURL: "https://github.com/octo/demo.git",
		Status:   model.RepoStatusActive,
		// 验签用的密钥(加密存储,DAO 负责)
		WebhookSecret: secret,
	}))

	svc := &Service{
		repos:    NewRepoService(repoDAO, platformDAO, sdkprov.NewManager(0)),
		webhooks: NewWebhookService(ruleDAO, eventDAO, repoDAO),
		bgCtx:    context.Background(),
	}

	deliver := func(deliveryID, after string) {
		body := `{"ref":"refs/heads/main","before":"1111111111111111111111111111111111111111",` +
			`"after":"` + after + `",` +
			`"repository":{"id":1,"full_name":"octo/demo"},` +
			`"sender":{"id":2,"login":"tester"},"commits":[]}`
		err := svc.ReceiveWebhook(context.Background(), "octo/demo", &WebhookPayload{
			Method: http.MethodPost,
			Path:   "/",
			Header: map[string][]string{
				"X-GitHub-Event":      {"push"},
				"X-GitHub-Delivery":   {deliveryID},
				"X-Hub-Signature-256": {githubSign(secret, body)},
				"Content-Type":        {"application/json"},
			},
			Body: []byte(body),
		})
		require.NoError(t, err, "ReceiveWebhook 应成功")
	}

	deliver("delivery-0001", "2222222222222222222222222222222222222222")
	deliver("delivery-0002", "3333333333333333333333333333333333333333")

	// 两条投递 ⇒ 两条事件(旧实现只有 1 条:第 2 条被"空 ID 查重"挡掉)
	var count int64
	require.NoError(t, db.Model(&model.WebhookEvent{}).Where("repo_key = ?", "octo/demo").Count(&count).Error)
	assert.Equal(t, int64(2), count, "两次投递必须落两条事件(第 2 条曾被静默丢弃)")

	// 两条事件的事件 ID 必须都非空(空 ID 会撞唯一索引)
	var emptyIDs int64
	require.NoError(t, db.Model(&model.WebhookEvent{}).
		Where("repo_key = ? AND (event_id IS NULL OR event_id = '')", "octo/demo").Count(&emptyIDs).Error)
	assert.Equal(t, int64(0), emptyIDs, "事件 ID 绝不能为空")

	// 同一投递重投 ⇒ 幂等(不产生新事件)
	deliver("delivery-0001", "2222222222222222222222222222222222222222")
	require.NoError(t, db.Model(&model.WebhookEvent{}).Where("repo_key = ?", "octo/demo").Count(&count).Error)
	assert.Equal(t, int64(2), count, "同一投递重投必须幂等(平台投递 ID 是真幂等键)")

	// 等异步的 applyRules 收尾,避免 goroutine 泄漏到下一个用例
	time.Sleep(50 * time.Millisecond)
}

// TestWebhookEventID 事件 ID 决策:非空才去重,空则合成唯一 ID。
//
// 回归背景(P0):空 ID 被拿去查重 ⇒ 把别的仓库的事件挡掉;空 ID 入库 ⇒ 撞
// 唯一索引 ⇒ 第 2 条起的事件被静默丢弃。
//
// 这条用例是**反向验证**:把 needDedup 对空 ID 也返回 true,本用例变红。
func TestWebhookEventID(t *testing.T) {
	id, dedup := webhookEventID("github-delivery-1")
	assert.Equal(t, "github-delivery-1", id)
	assert.True(t, dedup, "非空 ID 必须去重(平台投递 ID 是真幂等键)")

	id2, dedup2 := webhookEventID("  github-delivery-2  ")
	assert.Equal(t, "github-delivery-2", id2, "首尾空白应被去掉")
	assert.True(t, dedup2)

	id3, dedup3 := webhookEventID("")
	assert.NotEmpty(t, id3, "空 ID 必须合成一个非空 ID")
	assert.False(t, dedup3, "空 ID 绝不能拿去查重(会把别的仓库的事件挡掉)")

	id4, _ := webhookEventID("   ")
	assert.NotEmpty(t, id4)
	assert.NotEqual(t, id3, id4, "合成 ID 必须唯一")
}
