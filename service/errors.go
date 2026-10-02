package service

import (
	"errors"

	sdkprov "github.com/yi-nology/go-git-platform/provider"
)

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

	// ErrPlatformNotFound 平台记录缺失：查询"任意错误或空记录"统一归一化为此哨兵，
	// 文案是对外契约（壳层 404 回传），不可改动。
	ErrPlatformNotFound = errors.New("platform not found")

	// ErrTargetPlatformNotFound 元数据回灌目标平台缺失（target_platform 查无记录）。
	ErrTargetPlatformNotFound = errors.New("target platform not found")

	// ErrTemplateNotFound 策略模板缺失（壳层回 404，文案是对外契约）。
	ErrTemplateNotFound = errors.New("template not found")

	// ErrTemplateCycle 模板继承链成环（壳层回 400，完整文案带链路）。
	ErrTemplateCycle = errors.New("template extends cycle")

	// ErrStarredUnsupported 平台不支持 starred 列举（壳层回 400，文案是对外契约）。
	ErrStarredUnsupported = errors.New("starred import currently supports github only")

	// ErrMetadataValidation 元数据请求/快照读取类错误的分类哨兵（壳层据此回 400）。
	// 具体文案由 metadataError.Error() 给出（如 "no snapshot for <key>"、
	// "sync.backup_dir not configured"），保持逐字契约。
	ErrMetadataValidation = errors.New("metadata validation")

	// errBackupDisabled 冷备目录未配置。
	errBackupDisabled = errors.New("backup dir not configured (sync.backup_dir)")

	// errInvalidName 文件名非法(含路径分隔符)。
	errInvalidName = errors.New("invalid file name")

	// errLegalHold 合规冻结期间禁止清理冷备。
	errLegalHold = errors.New("legal hold is active: cleanup refused")
)

// ErrorClass 错误到传输层（HTTP）的分类契约：壳层据此选状态码，
// 不必逐个 errors.Is 哨兵。core 只给类别，不绑任何 Web 框架。
type ErrorClass string

const (
	ErrorClassNotFound    ErrorClass = "not_found"
	ErrorClassConflict    ErrorClass = "conflict"
	ErrorClassValidation  ErrorClass = "validation"
	ErrorClassAuth        ErrorClass = "auth"
	ErrorClassRateLimited ErrorClass = "rate_limited"
	ErrorClassUnavailable ErrorClass = "unavailable"
	ErrorClassInternal    ErrorClass = "internal"
)

// Classify 归类错误；err 为 nil 返回空串。
// 优先哨兵错误，其次 SDK provider 结构化错误，最后回落 internal。
func Classify(err error) ErrorClass {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrRepoNotFound),
		errors.Is(err, ErrTaskNotFound),
		errors.Is(err, ErrRuleNotFound),
		errors.Is(err, ErrEventNotFound),
		errors.Is(err, ErrPlatformNotFound),
		errors.Is(err, ErrTargetPlatformNotFound),
		errors.Is(err, ErrTemplateNotFound):
		return ErrorClassNotFound
	case errors.Is(err, ErrTaskRunning), errors.Is(err, ErrTooManyConcurrent):
		return ErrorClassConflict
	case errors.Is(err, ErrTaskDisabled):
		return ErrorClassValidation
	case errors.Is(err, errBackupDisabled), errors.Is(err, errInvalidName),
		errors.Is(err, ErrMetadataValidation),
		errors.Is(err, ErrTemplateCycle):
		return ErrorClassValidation
	case errors.Is(err, ErrStarredUnsupported):
		return ErrorClassValidation
	case errors.Is(err, errLegalHold):
		return ErrorClassConflict
	}

	var perr *sdkprov.ProviderError
	if errors.As(err, &perr) {
		switch {
		case sdkprov.IsRateLimited(err):
			return ErrorClassRateLimited
		case sdkprov.IsAuthentication(err):
			return ErrorClassAuth
		case sdkprov.IsNotFound(err):
			return ErrorClassNotFound
		case perr.IsServerError():
			return ErrorClassUnavailable
		case perr.IsClientError():
			return ErrorClassValidation
		}
	}
	return ErrorClassInternal
}

// metadataError 带自定义文案的校验错误：Error() 返回调用方约定的对外文案
// （前端/CLI/文档引用，逐字不可变），Is() 恒匹配 ErrMetadataValidation 供 Classify
// 归入 validation（壳层回 400）；cause 保留底层错误供 errors.Is/As 溯源。
type metadataError struct {
	msg   string
	cause error
}

// newMetadataError 构造元数据校验错误；cause 可为 nil。
func newMetadataError(msg string, cause error) error {
	return &metadataError{msg: msg, cause: cause}
}

func (e *metadataError) Error() string { return e.msg }

func (e *metadataError) Is(target error) bool { return target == ErrMetadataValidation }

func (e *metadataError) Unwrap() error { return e.cause }
