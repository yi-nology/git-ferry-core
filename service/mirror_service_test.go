package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/yi-nology/git-ferry-core/dao"
	"github.com/yi-nology/git-ferry-core/model"
)

// setupMirrorServiceTest 建仅 DB 依赖的 MirrorService（backup 模式不触发 git）。
func setupMirrorServiceTest(t *testing.T) (*MirrorService, *dao.RepoDAO, *gorm.DB) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")

	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "mirror.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.Platform{}, &model.Repo{},
		&model.MirrorChannel{}, &model.MirrorTarget{}, &model.MirrorRun{},
	))

	cfg := &model.Config{
		Git:  model.GitConfig{TempDir: t.TempDir()},
		Sync: model.SyncConfig{BackupDir: t.TempDir()},
	}
	repoDAO, err := dao.NewRepoDAO(db)
	require.NoError(t, err)
	platformDAO, err := dao.NewPlatformDAO(db)
	require.NoError(t, err)
	svc := &Service{config: cfg, repos: NewRepoService(repoDAO, platformDAO, nil)}

	m, err := NewMirrorService(svc, db)
	require.NoError(t, err)
	return m, repoDAO, db
}

// TestMirrorChannelCRUD_BackupMode backup 模式全程不碰 git：建/列/改/删 + 二次确认。
func TestMirrorChannelCRUD_BackupMode(t *testing.T) {
	m, repoDAO, db := setupMirrorServiceTest(t)
	ctx := context.Background()

	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "github/acme/app", Name: "app", Platform: "github",
		PlatformOwner: "acme", PlatformRepo: "app",
		CloneURL: "https://github.com/acme/app.git",
	}))

	ch, err := m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name:    "acme-backup",
		Mode:    model.MirrorModeBackup,
		RepoKey: "github/acme/app",
		Targets: []MirrorTargetInput{{
			Remote: "github", RepoURL: "https://github.com/acme/app-backup.git",
			CredType: model.MirrorCredToken, Credential: "s3cr3t-token",
		}},
	})
	require.NoError(t, err)
	require.NotZero(t, ch.ID)
	require.Len(t, ch.Targets, 1)

	// 凭据必须落库加密，不得明文
	var raw model.MirrorTarget
	require.NoError(t, db.Where("channel_id = ?", ch.ID).First(&raw).Error)
	require.NotEmpty(t, raw.Credential)
	assert.NotContains(t, raw.Credential, "s3cr3t-token", "凭据不能明文入库")

	list, total, err := m.ListMirrorChannels(dao.DefaultPagination(0, 20))
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, list, 1)
	assert.Equal(t, "acme-backup", list[0].Name)

	got, err := m.GetMirrorChannel(ch.ID)
	require.NoError(t, err)
	assert.Equal(t, "github/acme/app", got.RepoKey)

	// 更新目标：凭据空串 = 保持原凭据（不要求重传）
	tID := got.Targets[0].ID
	upd, err := m.UpdateMirrorTarget(ctx, tID, MirrorTargetInput{
		Remote: "github", RepoURL: "https://github.com/acme/renamed.git",
		CredType: model.MirrorCredToken, Credential: "",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/acme/renamed.git", upd.RepoURL)
	assert.True(t, upd.HasCredential, "空凭据应保留原凭据")
	var raw2 model.MirrorTarget
	require.NoError(t, db.First(&raw2, tID).Error)
	assert.Equal(t, raw.Credential, raw2.Credential, "未重传凭据时密文不变")
	assert.NotContains(t, raw2.Credential, "s3cr3t-token")

	// 删除目标
	require.NoError(t, m.DeleteMirrorTarget(tID))
	got, err = m.GetMirrorChannel(ch.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Targets)

	// 删除通道：确认名必须匹配
	require.Error(t, m.DeleteMirrorChannel("wrong", ch.ID))
	require.NoError(t, m.DeleteMirrorChannel("acme-backup", ch.ID))
	_, total, err = m.ListMirrorChannels(dao.DefaultPagination(0, 20))
	require.NoError(t, err)
	assert.EqualValues(t, 0, total)
}

// TestMirrorChannelCreateValidation 入参校验（不落库）。
func TestMirrorChannelCreateValidation(t *testing.T) {
	m, repoDAO, _ := setupMirrorServiceTest(t)
	ctx := context.Background()
	require.NoError(t, repoDAO.Create(&model.Repo{Key: "k/n/r", Name: "n", Platform: "github"}))

	targets := []MirrorTargetInput{{Remote: "origin", RepoURL: "https://x/y.git"}}

	_, err := m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "", RepoKey: "k/n/r", Targets: targets,
	})
	require.ErrorContains(t, err, "通道名称不能为空")

	_, err = m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "x", Mode: "weird", RepoKey: "k/n/r", Targets: targets,
	})
	require.ErrorContains(t, err, "不支持的模式")

	_, err = m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "x", Mode: model.MirrorModeBackup, RepoKey: "k/n/r",
	})
	require.ErrorContains(t, err, "至少需要一个目标")

	_, err = m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "x", Mode: model.MirrorModeBackup, RepoKey: "no/such/repo", Targets: targets,
	})
	require.ErrorContains(t, err, "源仓库不存在")

	// 远端名非法 / 缺凭据
	_, err = m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "x", Mode: model.MirrorModeBackup, RepoKey: "k/n/r",
		Targets: []MirrorTargetInput{{Remote: "--upload-pack", RepoURL: "https://x/y.git"}},
	})
	require.ErrorContains(t, err, "远端名非法")

	_, err = m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "x", Mode: model.MirrorModeBackup, RepoKey: "k/n/r",
		Targets: []MirrorTargetInput{{Remote: "origin", RepoURL: "https://x/y.git", CredType: model.MirrorCredToken}},
	})
	require.ErrorContains(t, err, "需要提供凭据内容")

	_, err = m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "x", Mode: model.MirrorModeBackup, RepoKey: "k/n/r",
		Targets: []MirrorTargetInput{{Remote: "origin", RepoURL: "https://x/y.git", CredType: "kerberos"}},
	})
	require.ErrorContains(t, err, "不支持的凭据类型")
}

// TestMirrorTargetSSHPinningRoundTrip 回归:SSH 主机密钥钉扎必须**落库并生效**。
//
// 背景:目标侧之前**没法钉**主机密钥 —— 于是同一次发布里
// 「检查路径」无 known_hosts 就硬错(fail-closed)、「推送路径」却用 accept-new(TOFU),
// **两套策略相反**。现在钉扎字段从入参落库,并进 AuthConfig。
//
// 还要盯住「钉扎信息不能丢」:它一旦丢失(没落库 / 标记成不持久),
// 就等于重启后悄悄退化成不校验 —— 比没有这个功能更危险。
func TestMirrorTargetSSHPinningRoundTrip(t *testing.T) {
	m, repoDAO, db := setupMirrorServiceTest(t)
	ctx := context.Background()
	require.NoError(t, repoDAO.Create(&model.Repo{
		Key: "github/acme/app", Name: "app", Platform: "github",
		PlatformOwner: "acme", PlatformRepo: "app",
		CloneURL: "https://github.com/acme/app.git",
	}))

	ch, err := m.CreateMirrorChannel(ctx, CreateMirrorChannelInput{
		Name: "pin", Mode: model.MirrorModeBackup, RepoKey: "github/acme/app",
		Targets: []MirrorTargetInput{{
			Remote: "github", RepoURL: "https://github.com/acme/app-backup.git",
			CredType: model.MirrorCredSSHKey, Credential: "-----BEGIN KEY-----\nfake\n-----END KEY-----\n",
			HostKeyFingerprint: "SHA256:abcdefghijklmnop",
			KnownHostsPath:     "/etc/ssh/known_hosts",
		}},
	})
	require.NoError(t, err)
	require.Len(t, ch.Targets, 1)

	// ① 落库:钉扎信息不能只在内存里。
	var stored model.MirrorTarget
	require.NoError(t, db.Where("id = ?", ch.Targets[0].ID).First(&stored).Error)
	assert.Equal(t, "SHA256:abcdefghijklmnop", stored.HostKeyFingerprint,
		"主机密钥指纹必须落库(丢了等于没钉)")
	assert.Equal(t, "/etc/ssh/known_hosts", stored.KnownHostsPath)

	// ② 生效:targetAuth 把它带进 AuthConfig(检查路径与推送路径同一份)。
	auth, err := m.targetAuth(&stored)
	require.NoError(t, err)
	assert.Equal(t, "SHA256:abcdefghijklmnop", auth.HostKeyFingerprint)
	assert.Equal(t, "/etc/ssh/known_hosts", auth.KnownHostsPath)
}

// TestChannelScrubRulesCompile 回归:通道配置的痕迹规则要能编译成可执行的规则。
//
// 镜像发布侧的关键防线:身份改写只管 module path,内容里的内网 registry /
// 部署代号 / 令牌靠痕迹规则清。规则存在通道上(JSON),发布时编译。
//
// 两条必须成立:
//   - 规则**编不过**要在发布前就报错(否则错误留到发布那一刻才炸);
//   - mandatory(密钥类)要正确映射(它会中止发布)。
func TestChannelScrubRulesCompile(t *testing.T) {
	// ① 规则编不过 → 报错,且指到第几条。
	_, err := compileScrubRules([]MirrorScrubRuleInput{
		{Name: "ok", Pattern: `^ok$`},
		{Name: "bad", Pattern: `([`},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "第 2 条")

	// ② 通道里存的 JSON → 编译出的规则。
	ch := &model.MirrorChannel{ScrubRules: `[{"name":"reg","category":"host","pattern":"registry\\.internal","severity":"mandatory"}]`}
	rules, err := channelScrubRules(ch)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "reg", rules[0].Name)
	assert.Equal(t, "registry\\.internal", rules[0].Pattern.String())
	assert.True(t, rules[0].Mandatory(), "severity=mandatory 必须映射成强制人工处理")

	// ③ 空配置 / 坏 JSON。
	rules, err = channelScrubRules(&model.MirrorChannel{})
	require.NoError(t, err)
	assert.Empty(t, rules)
	_, err = channelScrubRules(&model.MirrorChannel{ScrubRules: "{not json"})
	require.Error(t, err)
}
