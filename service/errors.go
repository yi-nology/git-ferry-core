package service

import "errors"

var (
	// ErrRepoNotFound is returned when a repository is not found.
	ErrRepoNotFound = errors.New("repo not found")

	// ErrTaskNotFound is returned when a sync task is not found.
	ErrTaskNotFound = errors.New("task not found")

	// ErrTaskDisabled is returned when attempting to run a disabled task.
	ErrTaskDisabled = errors.New("task is disabled")

	// ErrTaskRunning is returned when a task is already executing (concurrent run skipped).
	ErrTaskRunning = errors.New("task is already running")

	// ErrTooManyConcurrent is returned when the global concurrency limit is reached.
	ErrTooManyConcurrent = errors.New("too many concurrent sync tasks")

	// ErrRuleNotFound is returned when a webhook rule is not found.
	ErrRuleNotFound = errors.New("rule not found")

	// ErrEventNotFound is returned when a webhook event is not found.
	ErrEventNotFound = errors.New("event not found")

	// errBackupDisabled 冷备目录未配置。
	errBackupDisabled = errors.New("backup dir not configured (sync.backup_dir)")

	// errInvalidName 文件名非法(含路径分隔符)。
	errInvalidName = errors.New("invalid file name")

	// errLegalHold 合规冻结期间禁止清理冷备。
	errLegalHold = errors.New("legal hold is active: cleanup refused")
)
