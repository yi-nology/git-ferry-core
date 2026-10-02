package service

import (
	executor "github.com/yi-nology/git-ferry-core/executor"
	sdkprov "github.com/yi-nology/go-git-platform/provider"
	"gorm.io/gorm"
)

// Option 装配期依赖注入。全部在 NewService 之前生效，
// 取代此前"先 SetProviderHooks 再 NewService"的时序约定。
type Option func(*serviceOptions)

type serviceOptions struct {
	db                *gorm.DB
	providerHooks     *sdkprov.Hooks
	forcePushApprover executor.ForcePushApprover
}

// WithDB 注入外部 *gorm.DB（调用方持有连接生命周期，core 不再自行 initDB/Close）。
func WithDB(db *gorm.DB) Option {
	return func(o *serviceOptions) { o.db = db }
}

// WithProviderHooks 注入 provider 请求/响应生命周期钩子（壳层限流指标等）。
// 每次 provider 构造都会带上，无需再在 NewService 之前调用全局 SetProviderHooks。
func WithProviderHooks(h *sdkprov.Hooks) Option {
	return func(o *serviceOptions) { o.providerHooks = h }
}

// WithForcePushApprover 注入分歧保护的审批实现（壳层 HTTP 审批流）。
func WithForcePushApprover(a executor.ForcePushApprover) Option {
	return func(o *serviceOptions) { o.forcePushApprover = a }
}
