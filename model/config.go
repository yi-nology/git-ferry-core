package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sethvargo/go-envconfig"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Redis     RedisConfig     `yaml:"redis"`
	Git       GitConfig       `yaml:"git"`
	Sync      SyncConfig      `yaml:"sync"`
	Webhook   WebhookConfig   `yaml:"webhook"`
	Log       LogConfig       `yaml:"log"`
	Templates TemplatesConfig `yaml:"templates"`
}

// TemplatesConfig 同步策略模板库（JSON 文件存储，壳层原本直接持有；下沉到 core 统一管理）。
type TemplatesConfig struct {
	// Path 模板库文件路径，空 = data/templates.json（与历史壳层默认一致）。
	Path string `yaml:"path" env:"GIT_SYNC_TEMPLATES_PATH"`
}

// ServerConfig 壳层 HTTP 监听相关配置。
// 用户登录 / API Key / SSO 等鉴权不属于本库；由 git-ferry / git-ferry-intranet 自行持有。
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
	// HistoryWorkDir 历史同步的移植产物目录(apply 的工作副本)。
	//
	// 空 = 系统临时目录下的 git-ferry-history 子目录。
	// **建议指到持久目录**:产物要活到「apply 之后的那次 push」,
	// 放临时目录的话,系统清理临时文件会把待推送的产物删掉。
	HistoryWorkDir string `yaml:"history_work_dir" env:"GIT_SYNC_HISTORY_WORK_DIR"`
	// BackupKeep 每任务保留最近 N 份 bundle(0=不轮转,全部保留)
	BackupKeep int `yaml:"backup_keep" env:"GIT_SYNC_BACKUP_KEEP"`
	// BackupDestinations 多目的地扇出(s3/webdav/azure/local);
	// 逐目的地独立成功/失败。
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
	// HistoryRetentionDays 执行历史（webhook 事件 / 同步运行 / 运行步骤）保留天数。
	// 0=不按天清理（默认）。非 0 时由后台周期任务按天清理过期数据。
	//
	// **为什么需要它**：这三张表此前**没有任何清理入口** ⇒ 随运行次数无限增长
	// （现网两个月已有 92 次运行 / 355 条步骤）。
	HistoryRetentionDays int `yaml:"history_retention_days" env:"GIT_SYNC_HISTORY_RETENTION_DAYS"`
	// DurableQueue 持久化执行队列(execution_jobs,P2 分布式地基):
	// true=触发先入队,由派发器按 lease 认领执行——实例崩溃/K8s 驱逐后
	// 任务可被其它实例接管,重复触发自动合并不重复发布;
	// false(默认)=直接执行(旧行为,零变化)。
	DurableQueue bool `yaml:"durable_queue" env:"GIT_SYNC_DURABLE_QUEUE"`
	// ReconcileIntervalHours 周期对账间隔(小时):>0 启用——按周期对全部
	// 启用任务做跨平台一致性对账(ReconcileTask),drift/分支缺失落
	// reconcile_results 并写 outbox 事件(reconcile.drift);0=关闭(默认)。
	ReconcileIntervalHours int `yaml:"reconcile_interval_hours" env:"GIT_SYNC_RECONCILE_INTERVAL_HOURS"`
	// MaxWorkdirMB 单任务工作目录(克隆+LFS 对象)磁盘配额,0=不限制。
	// 超限即中止本次执行并清理工作区(reclaim 磁盘),下次执行全量重克隆。
	// 大仓库/多任务共享 temp 目录时的宿主机保护(P2 资源限制)。
	MaxWorkdirMB int `yaml:"max_workdir_mb" env:"GIT_SYNC_MAX_WORKDIR_MB"`
	// CloneCacheDir 同源共享克隆缓存目录(P2 Clone Cache):空=关闭(默认,零行为变化)。
	// 配置后,指向同一源仓库的首次克隆会先种一棵共享仓(no-checkout 全量),
	// 之后每个 workdir 经 objects/info/alternates 借用其对象库——同源 N 任务
	// 网络从 N×全量降到 1×全量+N×增量,磁盘从 N×仓库体降到 1×共享+N×元数据;
	// 每次克隆前对共享仓做一次增量 fetch 保鲜。**仅 native 后端**(gogit 不支持
	// reference clone,配置了会在 NewExecutor fail-fast)。
	// 生命周期:共享仓是 per-instance 本地目录(勿多实例共享同一路径);手动删除
	// 共享仓不会打死存量 workdir——下轮执行检出 alternates 悬空即清工作区全量
	// 重克隆(并重建缓存)。不参与 max_workdir_mb 配额(配额只量单任务工作区)。
	CloneCacheDir string `yaml:"clone_cache_dir" env:"GIT_SYNC_CLONE_CACHE_DIR"`
	// CloneCacheMaxMB 共享缓存总量上限(MiB),<=0=不限制(默认)。
	// 超限即按 mtime 淘汰最旧条目(LRU 近似;刚种入/刚复用的永不删,
	// 单条超限保留并告警防抖)。被淘汰条目被存量 workdir 引用时,
	// 下轮执行由 alternates 悬空自愈重建(与手动删缓存同轨)。
	CloneCacheMaxMB int `yaml:"clone_cache_max_mb" env:"GIT_SYNC_CLONE_CACHE_MAX_MB"`
	// ExecuteJobs 本实例是否执行队列任务:nil/true=入队+派发执行(API/Worker 合一);
	// false=只入队不执行(纯 API/调度实例,配合独立 Worker 进程)。
	// 仅 DurableQueue=true 时有意义。
	ExecuteJobs *bool `yaml:"execute_jobs" env:"GIT_SYNC_EXECUTE_JOBS"`
	// MirrorLockFailClosed 镜像跨实例锁的 Redis 异常策略:
	// false(默认,单实例友好)= 获取锁遇非竞争类错误(Redis 抖动)时降级为
	// 仅进程内互斥,不阻断镜像任务(可用性优先,接受双实例短暂并发窗口);
	// true(多实例推荐)= 直接拒绝执行 —— 无法证明互斥成立时**不许进入
	// 互斥区**(fail-closed),宁可镜像任务失败重试,不可两个实例同时发布。
	MirrorLockFailClosed bool `yaml:"mirror_lock_fail_closed" env:"GIT_SYNC_MIRROR_LOCK_FAIL_CLOSED"`
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
	// clone_cache_dir 必须绝对路径:相对路径随进程 cwd 漂移(重启后缓存
	// "凭空消失"重新种一遍),~/ 波浪号不被展开(实测会字面量建出 ~ 目录)。
	// IsAbs 同时挡住这两种形态;空=关闭放行。
	if c.Sync.CloneCacheDir != "" && !filepath.IsAbs(c.Sync.CloneCacheDir) {
		return fmt.Errorf("sync.clone_cache_dir must be an absolute path (got %q; ~ is not expanded)", c.Sync.CloneCacheDir)
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
