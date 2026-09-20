// Package sync 是 git-ferry-core 的库入口：配置加载与 Service 构造。
//
// 本库**不提供任何 HTTP/Web 服务端**，也不依赖 hertz/gin 等 Web 框架。
// 不包含用户登录 / API Key / SSO 等鉴权；入站 Webhook 由壳层收包后
// 转成 service.WebhookPayload 再调用 Service.ReceiveWebhook。
// Config.Server 仅含 Host/Port/Mode，core 自身不会监听端口。
//
// 详细子域见 model / service / executor / dao / lock。
package sync

import (
	"github.com/yi-nology/git-ferry-core/model"
	"github.com/yi-nology/git-ferry-core/service"
)

type Service = service.Service

type Config = model.Config

// WebhookPayload 协议无关的 Webhook 入站载荷（由壳层构造）。
type WebhookPayload = service.WebhookPayload

func NewService(cfg *Config) (*Service, error) {
	return service.NewService(cfg)
}

func LoadConfig(path string) (*Config, error) {
	return model.LoadConfig(path)
}
