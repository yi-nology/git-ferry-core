package model

import (
	"context"
	"fmt"
	"os"

	"github.com/sethvargo/go-envconfig"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Redis    RedisConfig    `yaml:"redis"`
	Git      GitConfig      `yaml:"git"`
	Sync     SyncConfig     `yaml:"sync"`
	Webhook  WebhookConfig  `yaml:"webhook"`
	Log      LogConfig      `yaml:"log"`
}

// ServerConfig 壳层 HTTP 监听相关配置。
// 用户登录 / API Key / SSO 等鉴权不属于本库；由 git-sync-service / git-sync-intranet 自行持有。
// core 库本身不监听端口、不做用户鉴权。
type ServerConfig struct {
	Host string `yaml:"host" env:"GIT_SYNC_SERVER_HOST"`
	Port int    `yaml:"port" env:"GIT_SYNC_SERVER_PORT"`
	Mode string `yaml:"mode" env:"GIT_SYNC_SERVER_MODE"`
}

type DatabaseConfig struct {
	Driver         string `yaml:"driver" env:"GIT_SYNC_DB_DRIVER"`
	DSN            string `yaml:"dsn" env:"GIT_SYNC_DB_DSN"`
	MaxIdleConns   int    `yaml:"max_idle_conns" env:"GIT_SYNC_DB_MAX_IDLE_CONNS"`
	MaxOpenConns   int    `yaml:"max_open_conns" env:"GIT_SYNC_DB_MAX_OPEN_CONNS"`
	ConnMaxLifeSec int    `yaml:"conn_max_life_sec" env:"GIT_SYNC_DB_CONN_MAX_LIFE_SEC"` // 连接最大存活秒数,0 用默认 5 分钟
	ConnMaxIdleSec int    `yaml:"conn_max_idle_sec" env:"GIT_SYNC_DB_CONN_MAX_IDLE_SEC"` // 空闲连接最大存活秒数,0 用默认 2 分钟
}

type RedisConfig struct {
	Addr            string `yaml:"addr" env:"GIT_SYNC_REDIS_ADDR"`
	Password        string `yaml:"password" env:"GIT_SYNC_REDIS_PASSWORD"`
	DB              int    `yaml:"db" env:"GIT_SYNC_REDIS_DB"`
	PoolSize        int    `yaml:"pool_size" env:"GIT_SYNC_REDIS_POOL_SIZE"`                 // 连接池大小,0 用 go-redis 默认(10*GOMAXPROCS)
	MinIdleConns    int    `yaml:"min_idle_conns" env:"GIT_SYNC_REDIS_MIN_IDLE_CONNS"`       // 最小空闲连接数,0 不预热
	DialTimeoutSec  int    `yaml:"dial_timeout_sec" env:"GIT_SYNC_REDIS_DIAL_TIMEOUT_SEC"`   // 建连超时秒数,0 不设超时
	ReadTimeoutSec  int    `yaml:"read_timeout_sec" env:"GIT_SYNC_REDIS_READ_TIMEOUT_SEC"`   // 读超时秒数,0 不设超时
	WriteTimeoutSec int    `yaml:"write_timeout_sec" env:"GIT_SYNC_REDIS_WRITE_TIMEOUT_SEC"` // 写超时秒数,0 不设超时
}

type GitConfig struct {
	Backend string `yaml:"backend" env:"GIT_SYNC_GIT_BACKEND"`
	TempDir string `yaml:"temp_dir" env:"GIT_SYNC_GIT_TEMP_DIR"`
}

// BackupDestinationConfig 冷备多目的地之一(s3/webdav/azure/local)。
type BackupDestinationConfig struct {
	Type string `yaml:"type" json:"type"` // s3 | webdav | azure | local
	Name string `yaml:"name" json:"name"`
	// S3/OSS/MinIO
	Endpoint  string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Region    string `yaml:"region,omitempty" json:"region,omitempty"`
	Bucket    string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Prefix    string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	AccessKey string `yaml:"access_key,omitempty" json:"access_key,omitempty"`
	SecretKey string `yaml:"secret_key,omitempty" json:"secret_key,omitempty"`
	PathStyle bool   `yaml:"path_style,omitempty" json:"path_style,omitempty"`
	// WebDAV / local dir(用 url 字段)
	URL      string `yaml:"url,omitempty" json:"url,omitempty"`
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
	// Azure Blob
	AccountName string `yaml:"account_name,omitempty" json:"account_name,omitempty"`
	AccountKey  string `yaml:"account_key,omitempty" json:"account_key,omitempty"`
	Container   string `yaml:"container,omitempty" json:"container,omitempty"`
	Enabled     *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

type SyncConfig struct {
	// BackupDir git bundle 冷备输出目录(空=禁用)
	BackupDir string `yaml:"backup_dir" env:"GIT_SYNC_BACKUP_DIR"`
	// BackupKeep 每任务保留最近 N 份 bundle(0=不轮转,全部保留)
	BackupKeep int `yaml:"backup_keep" env:"GIT_SYNC_BACKUP_KEEP"`
	// BackupS3 bundle 异地冷备(S3 兼容);空配置=只存本地
	BackupS3 struct {
		Endpoint  string `yaml:"endpoint"`
		Region    string `yaml:"region"`
		Bucket    string `yaml:"bucket"`
		Prefix    string `yaml:"prefix"`
		AccessKey string `yaml:"access_key"`
		SecretKey string `yaml:"secret_key"`
		PathStyle bool   `yaml:"path_style"`
	} `yaml:"backup_s3"`
	// BackupDestinations 多目的地扇出(s3/webdav/azure/local),与 BackupS3 并存;
	// 两者都配置时全部尝试,逐目的地独立成功/失败。
	BackupDestinations []BackupDestinationConfig `yaml:"backup_destinations"`
	// BackupEncryptKey 冷备加密密钥(base64 32 字节 AES-256-GCM);空=不加密。
	// 加密文件后缀 .bundle.enc,恢复时需同一密钥。
	BackupEncryptKey string `yaml:"backup_encrypt_key" env:"GIT_SYNC_BACKUP_ENCRYPT_KEY"`
	// BackupRetentionDays 冷备保留天数(0=不按天清理);与 BackupKeep 取更严者。
	BackupRetentionDays int `yaml:"backup_retention_days" env:"GIT_SYNC_BACKUP_RETENTION_DAYS"`
	// LegalHold 冻结清理(合规保留),true 时禁止自动轮转/过期删除。
	LegalHold bool `yaml:"legal_hold" env:"GIT_SYNC_LEGAL_HOLD"`
	// AutoDiscoverIntervalMinutes 平台自动发现周期(分钟);0=关闭。
	AutoDiscoverIntervalMinutes int `yaml:"auto_discover_interval_minutes" env:"GIT_SYNC_AUTO_DISCOVER_INTERVAL"`
	// AutoDiscoverImport 发现到新仓库时是否自动导入(否则仅报告)。
	AutoDiscoverImport bool `yaml:"auto_discover_import" env:"GIT_SYNC_AUTO_DISCOVER_IMPORT"`
	// PartialClone 部分克隆 filter(blob:none / tree:0),空=全量
	PartialClone   string `yaml:"partial_clone" env:"GIT_SYNC_PARTIAL_CLONE"`
	MaxConcurrent  int    `yaml:"max_concurrent" env:"GIT_SYNC_MAX_CONCURRENT"`
	DefaultTimeout int    `yaml:"default_timeout" env:"GIT_SYNC_DEFAULT_TIMEOUT"`
	RetryCount     int    `yaml:"retry_count" env:"GIT_SYNC_RETRY_COUNT"`
	// PostExecScript 同步结束（成功或失败）后执行的脚本；
	// 注入 GITFERRY_TASK / GITFERRY_RESULT / GITFERRY_RUN_ID / GITFERRY_TRIGGER。
	PostExecScript string `yaml:"post_exec_script" env:"GIT_SYNC_POST_EXEC_SCRIPT"`
	// BackupFormat 冷备形态:bundle(默认,git 语义) | zip(人工取件友好)
	BackupFormat string `yaml:"backup_format" env:"GIT_SYNC_BACKUP_FORMAT"`
	// BackupRemotes 额外备份远端（GitHub/GitLab 等），同步成功后 push 镜像副本。
	// 与「镜像中心开源发布」区分：此处不改写 module 身份，纯备份。
	BackupRemotes []BackupRemoteConfig `yaml:"backup_remotes"`
}

// BackupRemoteConfig 备份远端（git push）。
type BackupRemoteConfig struct {
	// Name 远端名（日志用）
	Name string `yaml:"name" json:"name"`
	// URL git 远端地址（https/ssh）；支持 {owner}/{repo} 占位由任务推导
	URL string `yaml:"url" json:"url"`
	// Token 注入用；也可走 GIT_SYNC_TOKEN_<NAME>
	Token string `yaml:"token,omitempty" json:"token,omitempty"`
	// Force 是否允许 force（仍受任务 force_push_policy 约束）
	Force bool `yaml:"force" json:"force"`
	// Enabled 默认 true
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

type WebhookConfig struct {
	RateLimit   int `yaml:"rate_limit" env:"GIT_SYNC_WEBHOOK_RATE_LIMIT"`
	MaxBodySize int `yaml:"max_body_size" env:"GIT_SYNC_WEBHOOK_MAX_BODY_SIZE"`
}

type LogConfig struct {
	Level  string `yaml:"level" env:"GIT_SYNC_LOG_LEVEL"`
	Format string `yaml:"format" env:"GIT_SYNC_LOG_FORMAT"`
}

// LoadConfig 从指定路径加载配置文件。
// path 参数由调用方控制（通常是启动参数或环境变量），不存在用户注入风险。
// 环境变量会覆盖 YAML 文件中的同名配置（env 优先级更高）。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // 配置文件路径由程序启动参数决定，非用户可控输入
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// 环境变量覆盖 YAML 配置值（env 优先级高于 YAML）。
	// 使用 ProcessWith 配合 DefaultOverwrite=true 确保已由 YAML 设定的字段也能被环境变量覆盖。
	ctx := context.Background()
	if err := envconfig.ProcessWith(ctx, &envconfig.Config{
		Target:           &cfg,
		Lookuper:         envconfig.OsLookuper(),
		DefaultOverwrite: true,
	}); err != nil {
		return nil, fmt.Errorf("processing environment variables: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate 校验配置并填充默认值。
// 注意：环境变量覆盖发生在 Validate 之前，因此此处的默认值逻辑仅在
// YAML 和环境变量均未提供对应字段时才生效。
func (c *Config) Validate() error {
	if c.Server.Host == "" {
		c.Server.Host = DefaultHost
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		c.Server.Port = DefaultPort
	}
	if c.Database.Driver == "" {
		return fmt.Errorf("database driver is required")
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database dsn is required")
	}
	if c.Database.Driver != DriverMySQL && c.Database.Driver != DriverSQLite {
		return fmt.Errorf("unsupported database driver: %s", c.Database.Driver)
	}
	if c.Database.MaxIdleConns <= 0 {
		c.Database.MaxIdleConns = DefaultMaxIdleConns
	}
	if c.Database.MaxOpenConns <= 0 {
		c.Database.MaxOpenConns = DefaultMaxOpenConns
	}
	if c.Database.ConnMaxLifeSec <= 0 {
		c.Database.ConnMaxLifeSec = DefaultConnMaxLifeSec
	}
	if c.Database.ConnMaxIdleSec <= 0 {
		c.Database.ConnMaxIdleSec = DefaultConnMaxIdleSec
	}
	if c.Git.TempDir == "" {
		c.Git.TempDir = DefaultTempDir
	}
	if c.Sync.MaxConcurrent <= 0 {
		c.Sync.MaxConcurrent = DefaultMaxConcurrent
	}
	if c.Sync.DefaultTimeout <= 0 {
		c.Sync.DefaultTimeout = DefaultTimeout
	}
	if c.Sync.RetryCount <= 0 {
		c.Sync.RetryCount = DefaultRetryCount
	}
	if c.Webhook.RateLimit <= 0 {
		c.Webhook.RateLimit = DefaultWebhookRateLimit
	}
	if c.Webhook.MaxBodySize <= 0 {
		c.Webhook.MaxBodySize = DefaultMaxBodySize
	}
	// Redis 超时默认值:0 意味着无限阻塞,生产环境必须有超时
	if c.Redis.Addr != "" {
		if c.Redis.DialTimeoutSec <= 0 {
			c.Redis.DialTimeoutSec = DefaultRedisDialTimeout
		}
		if c.Redis.ReadTimeoutSec <= 0 {
			c.Redis.ReadTimeoutSec = DefaultRedisReadTimeout
		}
		if c.Redis.WriteTimeoutSec <= 0 {
			c.Redis.WriteTimeoutSec = DefaultRedisWriteTimeout
		}
	}
	// 数值上限:防止配置错误导致资源耗尽
	if c.Sync.MaxConcurrent > MaxConcurrent {
		c.Sync.MaxConcurrent = MaxConcurrent
	}
	if c.Database.MaxOpenConns > MaxDBOpenConns {
		c.Database.MaxOpenConns = MaxDBOpenConns
	}
	if c.Webhook.MaxBodySize > MaxWebhookBodySize {
		c.Webhook.MaxBodySize = MaxWebhookBodySize
	}
	if c.Webhook.RateLimit > MaxWebhookRateLimit {
		c.Webhook.RateLimit = MaxWebhookRateLimit
	}
	return nil
}
