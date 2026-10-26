package model

// Run status constants
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// Webhook event status constants
const (
	StatusReceived   = "received"
	StatusProcessing = "processing"
	StatusProcessed  = "processed"
)

// Repository status constants
const (
	RepoStatusActive = "active"
)

// Trigger source constants
const (
	TriggerManual  = "manual"
	TriggerCron    = "cron"
	TriggerWebhook = "webhook"
	// TriggerRebuild 全量重建:执行前**清空任务工作目录**,强制从源重新克隆。
	TriggerRebuild = "rebuild"
	// TriggerAIRebuild AI 侧发起的全量重建,语义与 TriggerRebuild 相同。
	//
	// 它单独列一个常量而不是复用 TriggerRebuild,是为了让 run 记录里
	// 保留"是谁发起的"这一维(运维要能区分人工重建和 AI 重建)。
	TriggerAIRebuild = "ai_rebuild"
)

// IsRebuildTrigger 判断该触发器语义是否为"全量重建"(清 workdir + 重克隆)。
//
// **这是 rebuild 语义的唯一事实源**:清工作目录这件事必须由 core 在
// 拿到执行权之后做,不能由调用方各自去删 ——
// 调用方在拿到执行权之前删,会和正在跑的同步抢同一个目录
// (workdir 是按 taskKey 共享的),把跑着的同步的工作树删掉。
func IsRebuildTrigger(trigger string) bool {
	return trigger == TriggerRebuild || trigger == TriggerAIRebuild
}

// Webhook rule action constants
const (
	ActionSync = "sync"
)

// Database driver constants
const (
	DriverMySQL  = "mysql"
	DriverSQLite = "sqlite"
)

// Default configuration values
const (
	DefaultHost          = "0.0.0.0"
	DefaultPort          = 8890
	DefaultMaxIdleConns  = 10
	DefaultMaxOpenConns  = 100
	DefaultMaxConcurrent = 5
	DefaultTimeout       = 300
	DefaultRetryCount    = 3
	DefaultTempDir       = "/tmp/git-sync"

	DefaultConnMaxLifeSec    = 300 // 5 分钟
	DefaultConnMaxIdleSec    = 120 // 2 分钟
	DefaultWebhookRateLimit  = 10
	DefaultMaxBodySize       = 10 << 20 // 10MB
	DefaultRedisDialTimeout  = 5
	DefaultRedisReadTimeout  = 3
	DefaultRedisWriteTimeout = 3

	// 上限值
	MaxConcurrent       = 100
	MaxDBOpenConns      = 200
	MaxWebhookBodySize  = 100 << 20 // 100MB
	MaxWebhookRateLimit = 10000

	// Push retry 退避因子
	RetryBackoffMs = 500
)

// Step name constants
const (
	StepClone        = "clone"
	StepFetch        = "fetch"
	StepCheckout     = "checkout"
	StepEnsureRemote = "ensure_remote"
	StepPush         = "push"
	StepLFS          = "lfs"
	StepWiki         = "wiki"
	StepDivergeCheck = "diverge_check"
)

// Error type constants
const (
	ErrorAuth      = "auth"
	ErrorDivergent = "divergent"
	ErrorConflict  = "conflict"
	ErrorNetwork   = "network"
	ErrorConfig    = "config"
	ErrorGit       = "git"
	ErrorUnknown   = "unknown"
)

// Table name constants
const (
	TableRepos            = "repos"
	TableSyncTasks        = "sync_tasks"
	TableSyncRuns         = "sync_runs"
	TableSyncRunSteps     = "sync_run_steps"
	TableWebhookRules     = "webhook_rules"
	TableWebhookRuleTasks = "webhook_rule_tasks"
	TableWebhookEvents    = "webhook_events"
	TableOperationLogs    = "operation_logs"
)

// Model default value constants
const (
	DefaultBranch    = "main"
	DefaultPlatform  = "unknown"
	DefaultSyncMode  = "single"
	DefaultEventType = "push"
	DefaultAction    = "sync"
)

// RepoDirName 任务工作区里 git 仓库所在子目录名。
// 单源:executor 的工作区路径与 service 的 GetRepoDir 都必须用这一个名字,
// 两处各写一份字面量必然漂移(实测:备份推送曾把父目录当仓库用,报
// "repository does not exist")。
const RepoDirName = "repo"
